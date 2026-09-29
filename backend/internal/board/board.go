// Package board 实现 v3 §7.1 设计的任务看板。
//
// 设计要点：
//   - 一个 TaskBoard 对应一个用户级"话题"（topic），由 MetaAgent 在会话启动时创建。
//   - 所有 DomainAgent / SubDomainAgent 通过看板观察全局状态（目标、子任务、约束、进度）。
//   - 任何状态变更都必须经过看板 API 以保证原子性，避免多 Agent 并发下污染上下文。
package board

import (
	"errors"      // errors.New 构造任务不存在等错误
	"fmt"         // fmt.Sprintf 生成任务 ID 与 Brief 文本
	"log"         // log.Printf 记录持久化/恢复失败（fail-open）
	"sort"        // sort.Strings 稳定化约束键输出顺序
	"strconv"     // strconv.ParseInt 解析任务 ID 的 _t<n> 后缀恢复 seq
	"strings"     // strings.TrimSpace 规范化计划任务 ID
	"sync"        // sync.RWMutex 保护 TaskBoard / Manager 的并发访问
	"sync/atomic" // atomic.Int64 生成唯一任务 ID
	"time"        // time.Now / time.Time 记录时间戳
)

// TaskStatus 子任务状态枚举类型。
type TaskStatus string

// 子任务状态枚举：覆盖从创建到终态的完整生命周期。
const (
	// TaskPending 表示待处理：刚创建，尚未分配。
	TaskPending TaskStatus = "pending"
	// TaskInProgress 表示进行中：已被某 Agent 认领。
	TaskInProgress TaskStatus = "in_progress"
	// TaskBlocked 表示阻塞：等待外部条件或依赖。
	TaskBlocked TaskStatus = "blocked"
	// TaskDone 表示完成：执行成功。
	TaskDone TaskStatus = "done"
	// TaskFailed 表示失败：执行出错或被否决。
	TaskFailed TaskStatus = "failed"
	// TaskUnverified 表示已交付但未验证（三态化）：
	// 产出已回传但缺机器可执行的验证证据（L0 证据缺/校验不可用），非失败语义。
	// 看板展示标黄不标红，由父 Agent 决定补验证或收口。
	TaskUnverified TaskStatus = "delivered-unverified"
)

// BoardStatus 看板整体状态枚举类型。
type BoardStatus string

// 看板整体状态枚举：覆盖从创建到终态的完整生命周期。
const (
	// BoardStatusNew 表示新建状态：看板尚无子任务。
	BoardStatusNew BoardStatus = "NEW"
	// BoardStatusInProgress 表示进行中：至少有一个子任务尚未到达终态。
	BoardStatusInProgress BoardStatus = "IN_PROGRESS"
	// BoardStatusDone 表示完成：所有子任务都已成功完成。
	BoardStatusDone BoardStatus = "DONE"
	// BoardStatusFailed 表示失败：所有子任务都已结束但至少有一个失败。
	BoardStatusFailed BoardStatus = "FAILED"
	// BoardStatusDelivered 表示已交付但存在未验证任务：
	// 所有子任务都已结束、无失败，但至少有一个是 delivered-unverified。
	// 与 DONE 区分：整品可用性未经全部验证；与 FAILED 区分：没有真失败。
	BoardStatusDelivered BoardStatus = "DELIVERED"
)

// SubTask 看板上的一个子任务。
type SubTask struct {
	ID        string     `json:"id"`                   // 子任务唯一 ID，格式 <topicID>_t<n>（write_plan 可显式指定）
	Title     string     `json:"title"`                // 子任务标题（由 MetaAgent 推断）
	Domain    string     `json:"domain,omitempty"`     // 所属领域（write_plan 指定，派发依赖门按它匹配）
	Assignee  string     `json:"assignee,omitempty"`   // 责任 Agent 实例 ID（domainAgent / assistant）
	Status    TaskStatus `json:"status"`               // 当前状态
	Result    string     `json:"result,omitempty"`     // 完成结果或失败/阻塞原因
	DependsOn []string   `json:"depends_on,omitempty"` // 依赖的其他子任务 ID 列表
	Acceptance []string  `json:"acceptance,omitempty"` // 验收标准（write_plan 指定，展示用）
	CreatedAt time.Time  `json:"created_at"`           // 创建时间
	UpdatedAt time.Time  `json:"updated_at"`           // 最近变更时间
}

// PlanTask 是 write_plan 工具入参的子任务项（TODO #22）。
type PlanTask struct {
	ID         string   `json:"id"`         // 子任务 ID（LLM 指定，供 depends_on 引用；非空且唯一）
	Title      string   `json:"title"`      // 标题
	Domain     string   `json:"domain"`     // 领域（派发依赖门按 domain 匹配）
	DependsOn  []string `json:"depends_on"` // 依赖的子任务 ID 列表
	Acceptance []string `json:"acceptance"` // 验收标准
}

// TaskBoard 任务看板：单个 topic 的全局状态容器。
//
// 并发说明：
//   - 所有会修改内部状态的公开方法都持有 mu 写锁或读锁。
//   - 外部禁止直接读写字段；应通过 AddSubTask / Assign / MarkDone 等方法访问。
//   - Snapshot() 返回的是值拷贝，可安全序列化。
type TaskBoard struct {
	mu sync.RWMutex // 读写锁：读多写少场景下允许并发 Snapshot

	TopicID     string              // 话题 ID（=会话 ID）
	Goal        string              // 全局目标
	Status      BoardStatus         // NEW / IN_PROGRESS / DONE / FAILED
	Constraints map[string]string   // 全局约束（如"兼容旧版 API"）
	Tasks       map[string]*SubTask // 子任务表，按 ID 索引
	Order       []string            // 子任务展示顺序（创建序）
	UpdatedAt   time.Time           // 看板最近变更时间
	seq         atomic.Int64        // 原子计数器，生成唯一任务序号
	persistFn   func(Snapshot)      // 持久化回调（Manager.WithStore 接线时注入）；nil=纯内存
}

// NewTaskBoard 新建空看板。
//
// 职责：创建一个处于 NEW 状态、不含任何子任务的看板实例。
//
// 参数：
//   - topicID：话题标识，通常等于会话 ID。
//   - goal：全局目标文本，将注入到所有子 Agent 上下文。
//
// 返回：初始化好的 *TaskBoard，Constraints / Tasks 内置 map 已预分配。
//
// 副作用：无（不会注册到 Manager）。
//
// 并发安全：构造期无共享，返回后可安全并发使用。
func NewTaskBoard(topicID, goal string) *TaskBoard {
	return &TaskBoard{
		TopicID:     topicID,                   // 绑定话题 ID
		Goal:        goal,                      // 记录全局目标
		Status:      BoardStatusNew,            // 初始状态为 NEW
		Constraints: make(map[string]string),   // 预分配约束 map，避免后续 nil 写入
		Tasks:       make(map[string]*SubTask), // 预分配任务 map
		UpdatedAt:   time.Now(),                // 记录创建时间
	}
}

// AddSubTask 追加一个子任务，title 重复时返回已有 ID。
//
// 职责：向看板追加新子任务；若同名任务已存在则幂等返回旧 ID，
// 避免重复创建导致看板膨胀。追加后看板状态自动切到 IN_PROGRESS。
//
// 参数：
//   - title：子任务标题。
//
// 返回：新创建或已存在的子任务 ID。
//
// 副作用：可能新增 Tasks / Order 条目，并更新 Status / UpdatedAt。
//
// 并发安全：内部持写锁，可被多 goroutine 并发调用。
func (b *TaskBoard) AddSubTask(title string) string {
	b.mu.Lock() // 加写锁，独占看板
	// 遍历已有任务，若发现同名则幂等返回其 ID（无状态变化，不调 persist）
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil && t.Title == title {
			b.mu.Unlock()
			return id
		}
	}
	// 生成新 ID：<topicID>_t<序号>，序号由原子计数器保证唯一。
	id := fmt.Sprintf("%s_t%d", b.TopicID, b.seq.Add(1))
	now := time.Now() // 统一时间戳，保证 CreatedAt == UpdatedAt
	b.Tasks[id] = &SubTask{
		ID:        id,          // 回填 ID，便于外部引用
		Title:     title,       // 标题
		Status:    TaskPending, // 新任务默认 pending
		CreatedAt: now,         // 创建时间
		UpdatedAt: now,         // 首次更新时间
	}
	b.Order = append(b.Order, id)    // 维持创建顺序，Brief 按此输出
	b.Status = BoardStatusInProgress // 只要有任务，看板即进入进行中
	b.UpdatedAt = now                // 刷新看板更新时间
	b.mu.Unlock()
	b.persist()
	return id
}

// SetConstraint 设置全局约束。
//
// 职责：写入或覆盖一条全局约束（key-value）。
//
// 参数：
//   - key：约束键，如 "api_compat"。
//   - value：约束值，如 "v1"。
//
// 副作用：更新 Constraints 与 UpdatedAt。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) SetConstraint(key, value string) {
	b.mu.Lock()
	b.Constraints[key] = value // 写入或覆盖约束
	b.UpdatedAt = time.Now()   // 刷新更新时间
	b.mu.Unlock()
	b.persist()
}

// SetPlan 全量覆盖看板计划（write_plan 工具用，TODO #22）。
//
// 校验（任一项失败返回错误，不落盘）：
//   - 任务 ID 非空且数组内唯一；
//   - 每个 depends_on 引用必须存在（新任务数组内或既有看板任务）；
//   - 新任务之间依赖无环（既有任务视为终态，不参与环判定）。
//
// 语义：已存在 ID 的任务保留状态与结果（重规划不改 Done 任务）；新增任务置 pending。
// 看板进入 IN_PROGRESS；goal 非空时更新看板目标。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) SetPlan(goal string, tasks []PlanTask) error {
	b.mu.Lock()

	if goal != "" {
		b.Goal = goal
	}
	// 校验新任务 ID 唯一。
	seen := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			b.mu.Unlock()
			return fmt.Errorf("plan task id must be non-empty")
		}
		if _, dup := seen[id]; dup {
			b.mu.Unlock()
			return fmt.Errorf("plan task id %q duplicated", id)
		}
		seen[id] = struct{}{}
	}
	// 校验依赖引用存在（新数组 ∪ 既有任务）。
	exists := func(id string) bool {
		if _, ok := seen[id]; ok {
			return true
		}
		_, ok := b.Tasks[id]
		return ok
	}
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if !exists(dep) {
				b.mu.Unlock()
				return fmt.Errorf("plan task %q depends on unknown task %q", t.ID, dep)
			}
		}
	}
	// 新任务依赖图环检测（DFS 三色标记，只在新任务子图上跑）。
	color := make(map[string]int, len(tasks)) // 0=未访问 1=在栈 2=完成
	var dfs func(id string) error
	depOf := func(id string) []string {
		for _, t := range tasks {
			if t.ID == id {
				return t.DependsOn
			}
		}
		return nil
	}
	dfs = func(id string) error {
		color[id] = 1
		for _, dep := range depOf(id) {
			if _, inNew := seen[dep]; !inNew {
				continue // 既有任务视为终态，无出边
			}
			switch color[dep] {
			case 1:
				return fmt.Errorf("plan dependency cycle detected at %q -> %q", id, dep)
			case 0:
				if err := dfs(dep); err != nil {
					return err
				}
			}
		}
		color[id] = 2
		return nil
	}
	for _, t := range tasks {
		if color[t.ID] == 0 {
			if err := dfs(t.ID); err != nil {
				b.mu.Unlock()
				return err
			}
		}
	}

	// 落盘：已存在保留，新增追加。
	now := time.Now()
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		if existing, ok := b.Tasks[id]; ok {
			existing.Domain = t.Domain
			existing.Acceptance = t.Acceptance
			existing.UpdatedAt = now
			continue
		}
		b.Tasks[id] = &SubTask{
			ID:         id,
			Title:      t.Title,
			Domain:     t.Domain,
			Status:     TaskPending,
			DependsOn:  append([]string{}, t.DependsOn...),
			Acceptance: append([]string{}, t.Acceptance...),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		b.Order = append(b.Order, id)
	}
	b.Status = BoardStatusInProgress
	b.UpdatedAt = now
	b.mu.Unlock()
	b.persist()
	return nil
}

// FindByDomain 返回指定 domain 的子任务 ID（首个匹配）；未匹配返回空串。
// 供派发依赖门（dispatchOne）按领域定位计划任务。
//
// 并发安全：内部持读锁。
func (b *TaskBoard) FindByDomain(domain string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil && t.Domain == domain {
			return id, true
		}
	}
	return "", false
}

// FindAllByDomain 返回指定 domain 的全部子任务 ID（按 Order 顺序）；未匹配返回 nil。
// MetaAgent 细粒度拆解时一个领域常对应多个子任务（如"怪物美术"7 步），
// 派发置进行中（boardAssign）与完成回写（boardUpdate）需联动整组而非仅首条。
//
// 并发安全：内部持读锁。
func (b *TaskBoard) FindAllByDomain(domain string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var ids []string
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil && t.Domain == domain {
			ids = append(ids, id)
		}
	}
	return ids
}

// TakeoverFrom 看板接力认领（TODO #73）：把旧 domain 的非 Done 子任务迁移到新 domain
// 名下并留痕"曾失败，由 <newDomain> 接力完成"。异名续建时旧 FAILED 条目不再永久红
//（实证 2026-08-25：domain-2 FAILED 永久红，异名"续建"完成后仍红，meta 只能散文解释；
// boardAssign/boardUpdate 按 domain 精确匹配，异名接管不命中旧条目）。
// 返回迁移条数；旧 domain 无非 Done 条目返回 0（幂等，同名续建天然覆盖）。
//
// 迁移语义：条目 Domain 改为新名、Title 追加留痕注记、状态翻回 pending
//（boardAssign 随派发置 in_progress、完成时整组 MarkDone 翻绿）；Done 条目不迁移
//（已完成的留原 domain 归档）。执行者标记清空（assignee 由新派发覆写）。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) TakeoverFrom(oldDomain, newDomain string) int {
	oldDomain = strings.TrimSpace(oldDomain)
	newDomain = strings.TrimSpace(newDomain)
	if oldDomain == "" || newDomain == "" || oldDomain == newDomain {
		return 0
	}
	b.mu.Lock()
	migrated := 0
	now := time.Now()
	for _, id := range b.Order {
		t := b.Tasks[id]
		if t == nil || t.Domain != oldDomain || t.Status == TaskDone {
			continue
		}
		t.Domain = newDomain
		if !strings.Contains(t.Title, "曾失败") {
			t.Title = t.Title + fmt.Sprintf("（曾失败，由 %s 接力完成）", newDomain)
		}
		t.Status = TaskPending
		t.Assignee = ""
		t.UpdatedAt = now
		migrated++
	}
	if migrated > 0 {
		b.UpdatedAt = now
	}
	b.mu.Unlock()
	if migrated > 0 {
		b.persist()
	}
	return migrated
}

// DependsDone 报告任务的所有依赖是否均已 done；无依赖或任务不存在返回 true。
// 供派发依赖门校验：依赖未全部完成时拒绝派发（TODO #22 Phase 1）。
//
// 并发安全：内部持读锁。
func (b *TaskBoard) DependsDone(taskID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.Tasks[taskID]
	if !ok {
		return true
	}
	for _, dep := range t.DependsOn {
		if d, ok := b.Tasks[dep]; !ok || d.Status != TaskDone {
			return false
		}
	}
	return true
}

// Assign 把某个子任务分配给某 Agent。
//
// 职责：将子任务指派给指定 Agent，并把状态置为 in_progress。
//
// 参数：
//   - taskID：子任务 ID。
//   - assignee：被分配 Agent 的实例 ID。
//
// 返回：taskID 不存在时返回 errors.New("task not found")。
//
// 副作用：更新 Assignee / Status / UpdatedAt。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) Assign(taskID, assignee string) error {
	b.mu.Lock()
	t, ok := b.Tasks[taskID] // 查找任务
	if !ok {
		b.mu.Unlock()
		return errors.New("task not found") // 任务不存在，直接报错
	}
	t.Assignee = assignee     // 记录责任人
	t.Status = TaskInProgress // 分配后自动进入进行中
	t.UpdatedAt = time.Now()  // 刷新任务更新时间
	b.UpdatedAt = t.UpdatedAt // 同步看板更新时间
	b.mu.Unlock()
	b.persist()
	return nil
}

// MarkDone 标记任务完成（带结果）。
//
// 职责：将子任务置为 done，并记录结果文本。
//
// 参数：
//   - taskID：子任务 ID。
//   - result：完成结果描述。
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkDone(taskID, result string) error {
	return b.transition(taskID, TaskDone, result)
}

// MarkFailed 标记任务失败。
//
// 职责：将子任务置为 failed，并记录失败原因。
//
// 参数：
//   - taskID：子任务 ID。
//   - reason：失败原因。
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算（任一失败则整体 FAILED）。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkFailed(taskID, reason string) error {
	return b.transition(taskID, TaskFailed, reason)
}

// MarkUnverified 标记任务"已交付但未验证"（三态化）。
//
// 职责：将子任务置为 delivered-unverified，并记录缺验证原因。
// 不触发看板 FAILED（非失败语义），整体状态走 DELIVERED。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkUnverified(taskID, reason string) error {
	return b.transition(taskID, TaskUnverified, reason)
}

// MarkBlocked 标记任务被阻塞。
//
// 职责：将子任务置为 blocked，并记录阻塞原因。
//
// 参数：
//   - taskID：子任务 ID。
//   - reason：阻塞原因。
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkBlocked(taskID, reason string) error {
	return b.transition(taskID, TaskBlocked, reason)
}

// transition 子任务状态转换的内部统一实现。
//
// 职责：在持写锁的前提下，更新任务状态、结果与时间戳，
// 并联动重算看板整体状态。MarkDone/MarkFailed/MarkBlocked 共用此函数，
// 以保证状态转换路径的一致性。
//
// 参数：
//   - taskID：子任务 ID。
//   - status：目标状态。
//   - result：结果或原因文本。
//
// 返回：taskID 不存在时返回 errors.New("task not found")。
//
// 副作用：更新任务与看板字段。
//
// 并发安全：内部持写锁；调用方无需再加锁。
func (b *TaskBoard) transition(taskID string, status TaskStatus, result string) error {
	b.mu.Lock()
	t, ok := b.Tasks[taskID] // 查找任务
	if !ok {
		b.mu.Unlock()
		return errors.New("task not found") // 不存在则报错
	}
	t.Status = status         // 切换状态
	t.Result = result         // 记录结果/原因
	t.UpdatedAt = time.Now()  // 刷新任务时间
	b.UpdatedAt = t.UpdatedAt // 同步看板时间
	b.recomputeStatusLocked() // 联动重算看板整体状态
	b.mu.Unlock()
	b.persist()
	return nil
}

// recomputeStatusLocked 重新计算看板整体状态（mu 已加锁时调用）。
//
// 职责：遍历所有子任务，根据其终态推导看板 Status：
//   - 无任务 → NEW
//   - 全部终态且有失败 → FAILED
//   - 全部终态、无失败但有 delivered-unverified → DELIVERED（三态化）
//   - 全部终态且无失败无未验证 → DONE
//   - 否则 → IN_PROGRESS
//
// 前置条件：调用方必须已持有 b.mu 写锁（"Locked" 后缀即此含义）。
//
// 副作用：修改 b.Status。
//
// 并发安全：非线程安全，仅供持锁路径调用。
func (b *TaskBoard) recomputeStatusLocked() {
	if len(b.Tasks) == 0 {
		b.Status = BoardStatusNew // 空看板归零
		return
	}
	allTerminal, anyFailed, anyUnverified := true, false, false
	for _, t := range b.Tasks {
		switch t.Status {
		case TaskFailed:
			anyFailed = true // 出现失败
		case TaskDone:
			// ok：终态成功，无需额外处理
		case TaskUnverified:
			anyUnverified = true // 终态但未验证（非失败语义）
		default:
			allTerminal = false // 仍有未完成任务
		}
	}
	switch {
	case allTerminal && anyFailed:
		b.Status = BoardStatusFailed // 全终态但含失败
	case allTerminal && anyUnverified:
		b.Status = BoardStatusDelivered // 全终态无失败但有未验证
	case allTerminal:
		b.Status = BoardStatusDone // 全部成功完成
	default:
		b.Status = BoardStatusInProgress // 仍有任务未结束
	}
}

// Snapshot 返回看板的只读视图（拷贝），供注入 Agent 上下文使用。
//
// 注入时必须只用 Snapshot()，禁止把 *TaskBoard 直接序列化，
// 防止外部代码绕过锁修改内部状态。
type Snapshot struct {
	TopicID     string            `json:"topic_id"`    // 话题 ID
	Goal        string            `json:"goal"`        // 全局目标
	Status      BoardStatus       `json:"status"`      // 看板状态
	Constraints map[string]string `json:"constraints"` // 全局约束快照
	Tasks       []SubTask         `json:"tasks"`       // 子任务列表（按创建序）
	UpdatedAt   time.Time         `json:"updated_at"`  // 快照时间
	Seq         int64             `json:"seq"`         // 自动编号计数器（AddSubTask 的 _t<n> 后缀水位）
}

// Snapshot 返回看板视图。
//
// 职责：在持读锁下深拷贝约束 map 与子任务列表，返回值可被外部
// 安全持有与序列化，不会受后续看板变更影响。
//
// 返回：Snapshot 值类型（非指针），内部切片为独立拷贝。
//
// 副作用：无（只读）。
//
// 并发安全：内部持读锁。
func (b *TaskBoard) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	// 深拷贝约束 map，避免外部修改污染内部状态
	cs := make(map[string]string, len(b.Constraints))
	for k, v := range b.Constraints {
		cs[k] = v
	}
	// 按 Order 顺序拷贝任务，保证 Brief 输出稳定
	tasks := make([]SubTask, 0, len(b.Order))
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil {
			tasks = append(tasks, *t) // 值拷贝 SubTask，含其切片字段
		}
	}
	return Snapshot{
		TopicID:     b.TopicID,
		Goal:        b.Goal,
		Status:      b.Status,
		Constraints: cs,
		Tasks:       tasks,
		UpdatedAt:   b.UpdatedAt,
		Seq:         b.seq.Load(),
	}
}

// BoardFromSnapshot 从持久化快照重建看板（2026-09-28 P1：重启后依赖门状态恢复）。
// seq 取快照水位与任务 ID 后缀（_t<n>）解析的最大值，防自动编号冲突
//（write_plan 自定义 ID 无 _t 后缀，解析失败跳过即可）。
func BoardFromSnapshot(s Snapshot) *TaskBoard {
	b := NewTaskBoard(s.TopicID, s.Goal)
	b.Status = s.Status
	for k, v := range s.Constraints {
		b.Constraints[k] = v
	}
	b.UpdatedAt = s.UpdatedAt
	maxSeq := s.Seq
	for _, t := range s.Tasks {
		tc := t // 值拷贝，防切片元素复用共享
		b.Tasks[t.ID] = &tc
		b.Order = append(b.Order, t.ID)
		if i := strings.LastIndex(t.ID, "_t"); i >= 0 {
			if n, err := strconv.ParseInt(t.ID[i+2:], 10, 64); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	b.seq.Store(maxSeq)
	return b
}

// Brief 紧凑文字表示，用于注入子 Agent 上下文（≈200 token）。
//
// 严格遵循 v3 §4.4：主 Agent 上下文不超过 1000 token；
// 子 Agent 看到的是看板摘要，不是完整任务列表。
//
// 职责：基于 Snapshot 生成可读的多行文本，截断到 maxTasks 个任务。
//
// 参数：
//   - maxTasks：最多展示的任务数；<=0 时取默认 6。
//
// 返回：拼接好的字符串，包含目标、约束与子任务列表。
//
// 副作用：无。
//
// 并发安全：依赖 Snapshot 的读锁，间接安全。
func (b *TaskBoard) Brief(maxTasks int) string {
	if maxTasks <= 0 {
		maxTasks = 6 // 默认展示前 6 个任务
	}
	snap := b.Snapshot() // 取只读快照，避免长字符串构造期持锁
	// 头部：看板 ID、状态、目标
	out := fmt.Sprintf("【任务看板 %s】 状态:%s 目标:%s\n", snap.TopicID, snap.Status, snap.Goal)
	if len(snap.Constraints) > 0 {
		out += "约束: "
		// 排序保证稳定输出（不依赖 map 顺序）
		keys := make([]string, 0, len(snap.Constraints))
		for k := range snap.Constraints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out += fmt.Sprintf("%s=%s; ", k, snap.Constraints[k])
		}
		out += "\n"
	}
	out += "子任务:\n"
	count := 0 // 已输出的任务计数
	failedDomains := map[string]bool{}
	for _, t := range snap.Tasks {
		if t.Status == TaskFailed {
			failedDomains[t.Domain] = true
		}
		if count >= maxTasks {
			// 超出上限时给出剩余数量提示后中止
			out += fmt.Sprintf("...(%d more)\n", len(snap.Tasks)-count)
			break
		}
		assign := "-" // 未分配时显示 "-"
		if t.Assignee != "" {
			assign = t.Assignee
		}
		// 每行：[状态] 标题 -> 责任人 (ID)
		out += fmt.Sprintf("  [%s] %s -> %s (%s)\n", t.Status, t.Title, assign, t.ID)
		count++
	}
	// 接管提示（TODO #73）：存在失败条目时引导续建沿用同名（自动翻绿）或显式 takeover
	//（异名接管），防旧 FAILED 条目永久红误导决策（实证 2026-08-25 水果忍者）。
	if len(failedDomains) > 0 {
		var ds []string
		for d := range failedDomains {
			if strings.TrimSpace(d) != "" {
				ds = append(ds, d)
			}
		}
		sort.Strings(ds)
		if len(ds) > 0 {
			out += fmt.Sprintf("【接管提示】存在失败条目（领域: %s）：续建同类工作时沿用原 domain 名（完成自动翻绿），或异名续建时在 call_sub_agent 填 takeover=旧域名 迁移留痕；忽略则旧条目永久红。\n", strings.Join(ds, "、"))
		}
	}
	return out
}

// persist 持久化切面（2026-09-28 P1 看板持久化）：每个 mutator 成功后调用。
// 快照经 RLock 拷贝后回调；回调失败由实现方 fail-open（log），不影响内存行为。
func (b *TaskBoard) persist() {
	if b.persistFn == nil {
		return
	}
	b.persistFn(b.Snapshot())
}

// Store 看板持久化接口（bootstrap 适配到 store.BoardStore；board 包不依赖 store——
// 分层纪律）。实现必须幂等（SaveBoard 为 UPSERT）。
type Store interface {
	LoadBoard(topicID string) (Snapshot, bool, error)
	SaveBoard(snap Snapshot) error
}

// Manager 多看板管理器（一个会话一个 TaskBoard）。
//
// 并发说明：boards map 受 mu 保护；读用 RLock，写用 Lock。
type Manager struct {
	mu     sync.RWMutex          // 读写锁保护 boards map
	boards map[string]*TaskBoard // 按 topicID 索引的看板表
	store  Store                 // 持久化（nil=纯内存，测试/未接线）
}

// NewManager 创建管理器。
//
// 职责：构造空的 Manager，预分配 boards map。
//
// 返回：*Manager，可直接用于 Get/GetOrCreate/Remove。
//
// 副作用：无。
//
// 并发安全：构造期无共享，返回后可并发使用。
func NewManager() *Manager {
	return &Manager{boards: make(map[string]*TaskBoard)}
}

// WithStore 注入持久化（bootstrap 接 store.BoardStore 适配器）。重复注入后者覆盖。
func (m *Manager) WithStore(s Store) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = s
	return m
}

// persist 是注入各 TaskBoard 的持久化回调（持 Manager 读锁取 store，fail-open）。
func (m *Manager) persist(snap Snapshot) {
	m.mu.RLock()
	s := m.store
	m.mu.RUnlock()
	if s == nil {
		return
	}
	if err := s.SaveBoard(snap); err != nil {
		log.Printf("[board] persist failed: topic=%s err=%v", snap.TopicID, err)
	}
}

// Get 获取看板，没有则返回 nil。
//
// 职责：按 topicID 查找看板。
//
// 参数：
//   - topicID：话题 ID。
//
// 返回：找到的 *TaskBoard，或 nil。
//
// 副作用：无。
//
// 并发安全：内部持读锁。
func (m *Manager) Get(topicID string) *TaskBoard {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.boards[topicID] // map 缺键时返回 nil
}

// restoreLocked 从持久层恢复看板到内存（mu 已持写锁时调用）。
// 命中则经 BoardFromSnapshot 重建、注入 persistFn 并登记到 boards 后返回；
// 未接 store / 无记录 / 读取出错返回 nil（出错仅记日志，fail-open 由调用方决定回落）。
func (m *Manager) restoreLocked(topicID string) *TaskBoard {
	if m.store == nil {
		return nil
	}
	snap, found, err := m.store.LoadBoard(topicID)
	if err != nil {
		log.Printf("[board] restore failed: topic=%s err=%v", topicID, err)
		return nil
	}
	if !found {
		return nil
	}
	b := BoardFromSnapshot(snap)
	b.persistFn = m.persist
	m.boards[topicID] = b
	return b
}

// GetRestored 取看板；内存 miss 且接了 store 时先从持久层恢复（不新建）。
// 与 GetOrCreate 的唯一差别是缺失时不新建——供派发依赖门/看板回写等只读消费方
// 在重启后、write_plan 重建之前找回依赖门状态（P1-5：依赖门状态随之恢复）。
//
// 返回：已存在或恢复的 *TaskBoard；未接 store / 无记录 / 恢复失败返回 nil。
//
// 并发安全：内部持写锁；恢复路径 PG 查询在锁内（惰性低频，与 GetOrCreate 同一权衡）。
func (m *Manager) GetRestored(topicID string) *TaskBoard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boards[topicID]; ok {
		return b // 命中已有看板
	}
	return m.restoreLocked(topicID)
}

// GetOrCreate 取或新建看板；内存 miss 且接了 store 时先从持久层恢复
//（重启后依赖门/任务状态随之找回），恢复失败/无记录才新建。
//
// 参数：
//   - topicID：话题 ID。
//   - goal：新建时使用的全局目标。
//
// 返回：已存在、恢复或新建的 *TaskBoard。
//
// 副作用：可能向 boards 写入新条目；可能触发一次 store 读。
//
// 并发安全：内部持写锁，保证同 topicID 只创建一次。
func (m *Manager) GetOrCreate(topicID, goal string) *TaskBoard {
	m.mu.Lock()
	defer m.mu.Unlock() // 恢复路径 PG 查询在锁内（惰性触发、低频，换取单创建语义）
	if b, ok := m.boards[topicID]; ok {
		return b // 命中已有看板
	}
	if b := m.restoreLocked(topicID); b != nil {
		return b // 从持久层恢复
	}
	b := NewTaskBoard(topicID, goal) // 否则新建
	b.persistFn = m.persist
	m.boards[topicID] = b // 登记到管理器
	return b
}

// Remove 移除看板。
//
// 职责：从管理器中删除指定 topicID 的看板，通常在会话结束时清理。
//
// 参数：
//   - topicID：话题 ID。
//
// 副作用：删除 boards 中的条目（看板本身由 GC 回收）。
//
// 并发安全：内部持写锁。
func (m *Manager) Remove(topicID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.boards, topicID) // 幂等：缺键时无副作用
}
