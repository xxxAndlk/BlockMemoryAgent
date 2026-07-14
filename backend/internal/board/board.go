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
	"sort"        // sort.Strings 稳定化约束键输出顺序
	"sync"        // sync.RWMutex 保护 TaskBoard / Manager 的并发访问
	"sync/atomic" // atomic.Int64 生成唯一任务 ID
	"time"        // time.Now / time.Time 记录时间戳
)

// TaskStatus 子任务状态枚举类型
type TaskStatus string

// 子任务状态枚举：覆盖从创建到终态的完整生命周期。
const (
	TaskPending    TaskStatus = "pending"     // 待处理：刚创建，尚未分配
	TaskInProgress TaskStatus = "in_progress" // 进行中：已被某 Agent 认领
	TaskBlocked    TaskStatus = "blocked"     // 阻塞：等待外部条件或依赖
	TaskDone       TaskStatus = "done"        // 完成：执行成功
	TaskFailed     TaskStatus = "failed"      // 失败：执行出错或被否决
)

// BoardStatus 看板整体状态枚举类型。
type BoardStatus string

// 看板整体状态枚举：覆盖从创建到终态的完整生命周期。
const (
	BoardStatusNew        BoardStatus = "NEW"
	BoardStatusInProgress BoardStatus = "IN_PROGRESS"
	BoardStatusDone       BoardStatus = "DONE"
	BoardStatusFailed     BoardStatus = "FAILED"
)

// SubTask 看板上的一个子任务
type SubTask struct {
	ID        string     `json:"id"`                   // 子任务唯一 ID，格式 <topicID>_t<n>
	Title     string     `json:"title"`                // 子任务标题（由 MetaAgent 推断）
	Assignee  string     `json:"assignee,omitempty"`   // 责任 Agent 实例 ID（domainAgent / assistant）
	Status    TaskStatus `json:"status"`               // 当前状态
	Result    string     `json:"result,omitempty"`     // 完成结果或失败/阻塞原因
	DependsOn []string   `json:"depends_on,omitempty"` // 依赖的其他子任务 ID 列表
	CreatedAt time.Time  `json:"created_at"`           // 创建时间
	UpdatedAt time.Time  `json:"updated_at"`           // 最近变更时间
}

// TaskBoard 任务看板：单个 topic 的全局状态容器
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
}

// NewTaskBoard 新建空看板
//
// 职责：创建一个处于 NEW 状态、不含任何子任务的看板实例。
//
// 参数：
//   - topicID：话题标识，通常等于会话 ID
//   - goal：全局目标文本，将注入到所有子 Agent 上下文
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

// AddSubTask 追加一个子任务，title 重复时返回已有 ID
//
// 职责：向看板追加新子任务；若同名任务已存在则幂等返回旧 ID，
// 避免重复创建导致看板膨胀。追加后看板状态自动切到 IN_PROGRESS。
//
// 参数：
//   - title：子任务标题
//
// 返回：新创建或已存在的子任务 ID。
//
// 副作用：可能新增 Tasks / Order 条目，并更新 Status / UpdatedAt。
//
// 并发安全：内部持写锁，可被多 goroutine 并发调用。
func (b *TaskBoard) AddSubTask(title string) string {
	b.mu.Lock()         // 加写锁，独占看板
	defer b.mu.Unlock() // 函数返回时释放
	// 遍历已有任务，若发现同名则幂等返回其 ID
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil && t.Title == title {
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
	return id
}

// SetConstraint 设置全局约束
//
// 职责：写入或覆盖一条全局约束（key-value）。
//
// 参数：
//   - key：约束键，如 "api_compat"
//   - value：约束值，如 "v1"
//
// 副作用：更新 Constraints 与 UpdatedAt。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) SetConstraint(key, value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Constraints[key] = value // 写入或覆盖约束
	b.UpdatedAt = time.Now()   // 刷新更新时间
}

// Assign 把某个子任务分配给某 Agent
//
// 职责：将子任务指派给指定 Agent，并把状态置为 in_progress。
//
// 参数：
//   - taskID：子任务 ID
//   - assignee：被分配 Agent 的实例 ID
//
// 返回：taskID 不存在时返回 errors.New("task not found")。
//
// 副作用：更新 Assignee / Status / UpdatedAt。
//
// 并发安全：内部持写锁。
func (b *TaskBoard) Assign(taskID, assignee string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.Tasks[taskID] // 查找任务
	if !ok {
		return errors.New("task not found") // 任务不存在，直接报错
	}
	t.Assignee = assignee     // 记录责任人
	t.Status = TaskInProgress // 分配后自动进入进行中
	t.UpdatedAt = time.Now()  // 刷新任务更新时间
	b.UpdatedAt = t.UpdatedAt // 同步看板更新时间
	return nil
}

// MarkDone 标记任务完成（带结果）
//
// 职责：将子任务置为 done，并记录结果文本。
//
// 参数：
//   - taskID：子任务 ID
//   - result：完成结果描述
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkDone(taskID, result string) error {
	return b.transition(taskID, TaskDone, result)
}

// MarkFailed 标记任务失败
//
// 职责：将子任务置为 failed，并记录失败原因。
//
// 参数：
//   - taskID：子任务 ID
//   - reason：失败原因
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算（任一失败则整体 FAILED）。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkFailed(taskID, reason string) error {
	return b.transition(taskID, TaskFailed, reason)
}

// MarkBlocked 标记任务被阻塞
//
// 职责：将子任务置为 blocked，并记录阻塞原因。
//
// 参数：
//   - taskID：子任务 ID
//   - reason：阻塞原因
//
// 返回：taskID 不存在时返回错误。
//
// 副作用：更新任务状态，并触发看板整体状态重算。
//
// 并发安全：内部持写锁（经 transition）。
func (b *TaskBoard) MarkBlocked(taskID, reason string) error {
	return b.transition(taskID, TaskBlocked, reason)
}

// transition 子任务状态转换的内部统一实现
//
// 职责：在持写锁的前提下，更新任务状态、结果与时间戳，
// 并联动重算看板整体状态。MarkDone/MarkFailed/MarkBlocked 共用此函数，
// 以保证状态转换路径的一致性。
//
// 参数：
//   - taskID：子任务 ID
//   - status：目标状态
//   - result：结果或原因文本
//
// 返回：taskID 不存在时返回 errors.New("task not found")。
//
// 副作用：更新任务与看板字段。
//
// 并发安全：内部持写锁；调用方无需再加锁。
func (b *TaskBoard) transition(taskID string, status TaskStatus, result string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.Tasks[taskID] // 查找任务
	if !ok {
		return errors.New("task not found") // 不存在则报错
	}
	t.Status = status         // 切换状态
	t.Result = result         // 记录结果/原因
	t.UpdatedAt = time.Now()  // 刷新任务时间
	b.UpdatedAt = t.UpdatedAt // 同步看板时间
	b.recomputeStatusLocked() // 联动重算看板整体状态
	return nil
}

// recomputeStatusLocked 重新计算看板整体状态（mu 已加锁时调用）
//
// 职责：遍历所有子任务，根据其终态推导看板 Status：
//   - 无任务 → NEW
//   - 全部终态且有失败 → FAILED
//   - 全部终态且无失败 → DONE
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
	allTerminal, anyFailed := true, false // 终态标记与失败标记
	for _, t := range b.Tasks {
		switch t.Status {
		case TaskFailed:
			anyFailed = true // 出现失败
		case TaskDone:
			// ok：终态成功，无需额外处理
		default:
			allTerminal = false // 仍有未完成任务
		}
	}
	switch {
	case allTerminal && anyFailed:
		b.Status = BoardStatusFailed // 全终态但含失败
	case allTerminal:
		b.Status = BoardStatusDone // 全部成功完成
	default:
		b.Status = BoardStatusInProgress // 仍有任务未结束
	}
}

// Snapshot 返回看板的只读视图（拷贝），供注入 Agent 上下文使用
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
}

// Snapshot 返回看板视图
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
	}
}

// Brief 紧凑文字表示，用于注入子 Agent 上下文（≈200 token）
//
// 严格遵循 v3 §4.4：主 Agent 上下文不超过 1000 token；
// 子 Agent 看到的是看板摘要，不是完整任务列表。
//
// 职责：基于 Snapshot 生成可读的多行文本，截断到 maxTasks 个任务。
//
// 参数：
//   - maxTasks：最多展示的任务数；<=0 时取默认 6
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
	for _, t := range snap.Tasks {
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
	return out
}

// Manager 多看板管理器（一个会话一个 TaskBoard）
type Manager struct {
	mu     sync.RWMutex          // 读写锁保护 boards map
	boards map[string]*TaskBoard // 按 topicID 索引的看板表
}

// NewManager 创建管理器
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

// Get 获取看板，没有则返回 nil
//
// 职责：按 topicID 查找看板。
//
// 参数：
//   - topicID：话题 ID
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

// GetOrCreate 取或新建看板
//
// 职责：若 topicID 已有看板则返回旧的；否则新建并登记。
// 用于会话首次访问时惰性创建看板。
//
// 参数：
//   - topicID：话题 ID
//   - goal：新建时使用的全局目标
//
// 返回：已存在或新建的 *TaskBoard。
//
// 副作用：可能向 boards 写入新条目。
//
// 并发安全：内部持写锁，保证同 topicID 只创建一次。
func (m *Manager) GetOrCreate(topicID, goal string) *TaskBoard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boards[topicID]; ok {
		return b // 命中已有看板
	}
	b := NewTaskBoard(topicID, goal) // 否则新建
	m.boards[topicID] = b            // 登记到管理器
	return b
}

// Remove 移除看板
//
// 职责：从管理器中删除指定 topicID 的看板，通常在会话结束时清理。
//
// 参数：
//   - topicID：话题 ID
//
// 副作用：删除 boards 中的条目（看板本身由 GC 回收）。
//
// 并发安全：内部持写锁。
func (m *Manager) Remove(topicID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.boards, topicID) // 幂等：缺键时无副作用
}
