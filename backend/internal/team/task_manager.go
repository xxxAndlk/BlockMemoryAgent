package team

// task_manager.go 逻辑任务管理器（WP1）：唯一允许写合同的地方。
//
// 职责仅限（计划 §5.2）：Create/Refine/Supersede/Revoke、ResolveActiveContract、
// InheritInvariants、Accept/RejectVerification、RebuildProjection、LegacyFallback。
// **不调用工具、不执行命令、不 import internal/agent 或 internal/server**——
// 资源解析与执行留 WP2 的 Action pipeline，本层只回答"当前任务合同是什么"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// LogicalTaskMode 逻辑任务灰度模式（agent.logical_task_mode）。
const (
	LogicalTaskModeOff     = "off"
	LogicalTaskModeShadow  = "shadow"
	LogicalTaskModeEnforce = "enforce"
)

// ValidLogicalTaskMode 校验模式枚举。
func ValidLogicalTaskMode(m string) bool {
	switch m {
	case LogicalTaskModeOff, LogicalTaskModeShadow, LogicalTaskModeEnforce:
		return true
	}
	return false
}

// NormalizeLogicalTaskMode 归一化：非法值回落最保守的 shadow（计划 §5.4）。
func NormalizeLogicalTaskMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case LogicalTaskModeOff:
		return LogicalTaskModeOff
	case LogicalTaskModeEnforce:
		return LogicalTaskModeEnforce
	case LogicalTaskModeShadow, "":
		return LogicalTaskModeShadow
	default:
		// 非法值由配置层告警（此处只做保守回落，不打日志避免重复）。
		return LogicalTaskModeShadow
	}
}

// Observation 一次任务管理决策的记录（shadow 模式下用于对拍，不改变行为）。
type Observation struct {
	Mode        string    `json:"mode"`
	Action      string    `json:"action"`
	SessionID   string    `json:"session_id"`
	TaskID      string    `json:"logical_task_id,omitempty"`
	Revision    int64     `json:"revision,omitempty"`
	ContentHash string    `json:"content_hash,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	At          time.Time `json:"at"`
}

// ObservationSink 观测落点（生产接 PG 事件/日志；测试可注入内存切片）。
type ObservationSink func(Observation)

// TaskManager 逻辑任务管理器。
type TaskManager struct {
	store LogicalTaskStore
	mode  string

	mu   sync.RWMutex
	sink ObservationSink
	// now 可注入时钟（测试确定性）。
	now func() time.Time
	// seq 用于生成幂等键的会话内序号（同毫秒多次调用不冲突）。
	seq uint64
}

// NewTaskManager 构造管理器。store 为 nil 时退化为内存实现（不影响启动）。
func NewTaskManager(store LogicalTaskStore, mode string) *TaskManager {
	if store == nil {
		store = NewMemoryLogicalTaskStore()
	}
	return &TaskManager{store: store, mode: NormalizeLogicalTaskMode(mode), now: time.Now}
}

// Mode 返回当前灰度模式。
func (m *TaskManager) Mode() string {
	if m == nil {
		return LogicalTaskModeOff
	}
	return m.mode
}

// SetMode 运行中切换模式（配置热更新/测试）。
func (m *TaskManager) SetMode(mode string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.mode = NormalizeLogicalTaskMode(mode)
	m.mu.Unlock()
}

// SetObservationSink 注入观测落点。
func (m *TaskManager) SetObservationSink(s ObservationSink) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sink = s
	m.mu.Unlock()
}

// Store 暴露存储（server 查询端点用）。
func (m *TaskManager) Store() LogicalTaskStore {
	if m == nil {
		return nil
	}
	return m.store
}

// enabled 判定是否执行写路径（off 完全零行为）。
func (m *TaskManager) enabled() bool {
	return m != nil && m.mode != LogicalTaskModeOff
}

func (m *TaskManager) observe(obs Observation) {
	if m == nil {
		return
	}
	m.mu.RLock()
	sink := m.sink
	mode := m.mode
	m.mu.RUnlock()
	obs.Mode = mode
	if obs.At.IsZero() {
		obs.At = m.now()
	}
	if sink != nil {
		sink(obs)
	}
}

// nextIdemKey 生成幂等键：sessionID/action/taskID/revision/序号。
func (m *TaskManager) nextIdemKey(sessionID, action, taskID string, rev int64) string {
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	return fmt.Sprintf("%s|%s|%s|%d|%d", sessionID, action, taskID, rev, seq)
}

// TaskInput 创建/细化任务的输入（统一首跑、续跑、运行中纠正三个入口）。
type TaskInput struct {
	SessionID      string
	Owner          string
	Goal           string
	OriginEventID  int64
	Targets        []SemanticTarget
	ReadSet        []ResourceRule
	WriteSet       []ResourceRule
	ExcludedSet    []ResourceRule
	Invariants     []Invariant
	Acceptance     []AcceptanceRule
	BaseCheckpoint string
	// Actor 谁触发（goal/user_message/mailbox/clarify，审计用）。
	Actor string
	// NewTask 显式新任务（续跑默认依据规则判定）。
	NewTask bool
}

// Create 创建根任务（新会话首条用户输入）。
func (m *TaskManager) Create(ctx context.Context, in TaskInput) (*LogicalTaskContract, error) {
	if !m.enabled() {
		return nil, nil
	}
	c := &LogicalTaskContract{
		SchemaVersion:  LogicalTaskSchemaVersion,
		LogicalTaskID:  m.nextTaskID(in.SessionID, m.now()),
		SessionID:      in.SessionID,
		Owner:          in.Owner,
		Revision:       1,
		Goal:           in.Goal,
		OriginEventID:  in.OriginEventID,
		Targets:        in.Targets,
		ReadSet:        in.ReadSet,
		WriteSet:       in.WriteSet,
		ExcludedSet:    in.ExcludedSet,
		Invariants:     in.Invariants,
		Acceptance:     in.Acceptance,
		BaseCheckpoint: in.BaseCheckpoint,
		Status:         TaskStatusActive,
	}
	if err := NormalizeContract(c); err != nil {
		return nil, err
	}
	if err := ValidateContract(c); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(c)
	ev := LogicalTaskEvent{
		SessionID:      c.SessionID,
		LogicalTaskID:  c.LogicalTaskID,
		Revision:       c.Revision,
		Kind:           LogicalEventCreated,
		Payload:        string(payload),
		OriginEventID:  in.OriginEventID,
		IdempotencyKey: m.nextIdemKey(c.SessionID, "create", c.LogicalTaskID, c.Revision),
		Owner:          c.Owner,
	}
	if err := m.store.CreateTask(ctx, c, ev); err != nil {
		return nil, err
	}
	m.observe(Observation{Action: "create", SessionID: c.SessionID, TaskID: c.LogicalTaskID,
		Revision: c.Revision, ContentHash: c.ContentHash, Actor: in.Actor, Reason: "new logical task"})
	return c, nil
}

// Refine 在当前任务上产生新 revision（运行中纠正/邮箱注入/ask_user 答复）。
// 旧 revision 通过事件底账保留（created/refined 全量快照）。
func (m *TaskManager) Refine(ctx context.Context, active *LogicalTaskContract, in TaskInput) (*LogicalTaskContract, error) {
	if !m.enabled() || active == nil {
		return active, nil
	}
	next := *active
	next.Revision = active.Revision + 1
	next.ParentRevision = active.Revision
	next.OriginEventID = in.OriginEventID
	next.Status = TaskStatusActive
	if g := strings.TrimSpace(in.Goal); g != "" {
		next.Goal = g
	}
	if len(in.Targets) > 0 {
		next.Targets = mergeTargets(active.Targets, in.Targets)
	}
	if len(in.ReadSet) > 0 {
		next.ReadSet = append(append([]ResourceRule{}, active.ReadSet...), in.ReadSet...)
	}
	if len(in.WriteSet) > 0 {
		next.WriteSet = append(append([]ResourceRule{}, active.WriteSet...), in.WriteSet...)
	}
	if len(in.ExcludedSet) > 0 {
		next.ExcludedSet = append(append([]ResourceRule{}, active.ExcludedSet...), in.ExcludedSet...)
	}
	if len(in.Invariants) > 0 {
		next.Invariants = mergeInvariants(active.Invariants, in.Invariants)
	}
	if len(in.Acceptance) > 0 {
		next.Acceptance = append(append([]AcceptanceRule{}, active.Acceptance...), in.Acceptance...)
	}
	if in.BaseCheckpoint != "" {
		next.BaseCheckpoint = in.BaseCheckpoint
	}
	if err := NormalizeContract(&next); err != nil {
		return active, err
	}
	if err := ValidateContract(&next); err != nil {
		return active, err
	}
	payload, _ := json.Marshal(&next)
	ev := LogicalTaskEvent{
		SessionID:      next.SessionID,
		LogicalTaskID:  next.LogicalTaskID,
		Revision:       next.Revision,
		Kind:           LogicalEventRefined,
		Payload:        string(payload),
		OriginEventID:  in.OriginEventID,
		IdempotencyKey: m.nextIdemKey(next.SessionID, "refine", next.LogicalTaskID, next.Revision),
		Owner:          next.Owner,
	}
	if err := m.store.AppendEvent(ctx, &next, ev); err != nil {
		return active, err
	}
	m.observe(Observation{Action: "refine", SessionID: next.SessionID, TaskID: next.LogicalTaskID,
		Revision: next.Revision, ContentHash: next.ContentHash, Actor: in.Actor, Reason: "user correction"})
	return &next, nil
}

// Revoke 显式撤销指定不变量（不删历史）。
func (m *TaskManager) Revoke(ctx context.Context, active *LogicalTaskContract, invariantIDs []string, actor string) (*LogicalTaskContract, error) {
	if !m.enabled() || active == nil || len(invariantIDs) == 0 {
		return active, nil
	}
	next := *active
	next.Revision = active.Revision + 1
	next.ParentRevision = active.Revision
	next.Status = TaskStatusActive
	next.Invariants = append([]Invariant{}, active.Invariants...)
	for _, id := range invariantIDs {
		RevokeInvariant(&next, id)
	}
	if err := NormalizeContract(&next); err != nil {
		return active, err
	}
	payload, _ := json.Marshal(map[string]any{"invariant_ids": invariantIDs})
	ev := LogicalTaskEvent{
		SessionID:      next.SessionID,
		LogicalTaskID:  next.LogicalTaskID,
		Revision:       next.Revision,
		Kind:           LogicalEventRevoked,
		Payload:        string(payload),
		IdempotencyKey: m.nextIdemKey(next.SessionID, "revoke", next.LogicalTaskID, next.Revision),
		Owner:          next.Owner,
	}
	if err := m.store.AppendEvent(ctx, &next, ev); err != nil {
		return active, err
	}
	m.observe(Observation{Action: "revoke", SessionID: next.SessionID, TaskID: next.LogicalTaskID,
		Revision: next.Revision, ContentHash: next.ContentHash, Actor: actor,
		Reason: "explicit revocation: " + strings.Join(invariantIDs, ",")})
	return &next, nil
}

// Supersede 新任务替代旧任务（旧任务历史仍保留）。
func (m *TaskManager) Supersede(ctx context.Context, active *LogicalTaskContract, actor, reason string) error {
	if !m.enabled() || active == nil {
		return nil
	}
	next := *active
	next.Status = TaskStatusSuperseded
	ev := LogicalTaskEvent{
		SessionID:      next.SessionID,
		LogicalTaskID:  next.LogicalTaskID,
		Revision:       next.Revision,
		Kind:           LogicalEventSuperseded,
		IdempotencyKey: m.nextIdemKey(next.SessionID, "supersede", next.LogicalTaskID, next.Revision),
		Owner:          next.Owner,
	}
	if err := m.store.AppendEvent(ctx, &next, ev); err != nil {
		return err
	}
	m.observe(Observation{Action: "supersede", SessionID: next.SessionID, TaskID: next.LogicalTaskID,
		Revision: next.Revision, Actor: actor, Reason: reason})
	return nil
}

// Transition 状态迁移（verified / delivered-unverified / failed / cancelled）。
func (m *TaskManager) Transition(ctx context.Context, active *LogicalTaskContract, nextStatus, kind string, actor, reason string) (*LogicalTaskContract, error) {
	if !m.enabled() || active == nil {
		return active, nil
	}
	next := *active
	if err := AdvanceStatus(&next, nextStatus); err != nil {
		return active, err
	}
	ev := LogicalTaskEvent{
		SessionID:      next.SessionID,
		LogicalTaskID:  next.LogicalTaskID,
		Revision:       next.Revision,
		Kind:           kind,
		IdempotencyKey: m.nextIdemKey(next.SessionID, kind, next.LogicalTaskID, next.Revision),
		Owner:          next.Owner,
	}
	if err := m.store.AppendEvent(ctx, &next, ev); err != nil {
		return active, err
	}
	m.observe(Observation{Action: kind, SessionID: next.SessionID, TaskID: next.LogicalTaskID,
		Revision: next.Revision, ContentHash: next.ContentHash, Actor: actor, Reason: reason})
	return &next, nil
}

// Accept 用户显式接受结果（verified/delivered-unverified → accepted）。
// 这是 Harness 唯一"用户已接受"事实来源；Evolver 只在 accepted 后运行（I-LEARN-1）。
func (m *TaskManager) Accept(ctx context.Context, active *LogicalTaskContract, actor string) (*LogicalTaskContract, error) {
	return m.Transition(ctx, active, TaskStatusAccepted, LogicalEventAccepted, actor, "user accepted")
}

// RejectVerification 用户否决（视为失败，不产生正面学习）。
func (m *TaskManager) RejectVerification(ctx context.Context, active *LogicalTaskContract, actor, reason string) (*LogicalTaskContract, error) {
	return m.Transition(ctx, active, TaskStatusFailed, LogicalEventRejected, actor, reason)
}

// ResolveActiveContract 返回会话当前 active 合同。
// legacy 会话（无合同）返回 (nil, ErrLogicalTaskNotFound)：调用方必须走 fallback，
// **不得伪造 accepted/verified**（计划 §3.5 末条）。
func (m *TaskManager) ResolveActiveContract(ctx context.Context, sessionID string) (*LogicalTaskContract, error) {
	if !m.enabled() {
		return nil, ErrLogicalTaskNotFound
	}
	return m.store.GetActive(ctx, sessionID)
}

// EnsureTask 首跑/续跑统一入口：有 active 则按规则 refine，否则创建新任务。
// 规则（计划 §3.3，确定性、保守、不额外调用 LLM）：
//   - 无 active：创建根任务，继承会话内最近终态任务的同类不变量（文件重叠）；
//   - 有 active 且未终态：运行中纠正 → refine；
//   - 有 active 但已终态（verified/delivered-unverified/failed/cancelled）：
//     默认创建新任务并记录 RelatedTaskIDs，继承不变量；
//   - in.NewTask 显式要求新任务：supersede 旧的再建（历史保留）。
func (m *TaskManager) EnsureTask(ctx context.Context, in TaskInput) (*LogicalTaskContract, error) {
	if !m.enabled() {
		return nil, nil
	}
	active, err := m.currentTask(ctx, in.SessionID)
	if err != nil && !errors.Is(err, ErrLogicalTaskNotFound) {
		return nil, err
	}
	if active != nil && !in.NewTask && !IsTerminalTaskStatus(active.Status) {
		return m.Refine(ctx, active, in)
	}
	// 继承：按规则挑最近一条终态任务计算不变量继承。
	if inherited, ok := m.inheritFromHistory(ctx, in); ok && len(in.Invariants) == 0 {
		in.Invariants = inherited.Inherited
		// 需确认项保留（needs_confirm），不得静默丢弃。
		in.Invariants = append(in.Invariants, inherited.NeedsConfirm...)
	}
	c := &LogicalTaskContract{
		SchemaVersion:  LogicalTaskSchemaVersion,
		LogicalTaskID:  m.nextTaskID(in.SessionID, m.now()),
		SessionID:      in.SessionID,
		Owner:          in.Owner,
		Revision:       1,
		Goal:           in.Goal,
		OriginEventID:  in.OriginEventID,
		Targets:        in.Targets,
		ReadSet:        in.ReadSet,
		WriteSet:       in.WriteSet,
		ExcludedSet:    in.ExcludedSet,
		Invariants:     in.Invariants,
		Acceptance:     in.Acceptance,
		BaseCheckpoint: in.BaseCheckpoint,
		Status:         TaskStatusActive,
	}
	if active != nil {
		c.RelatedTaskIDs = append(append([]string{}, active.RelatedTaskIDs...), active.LogicalTaskID)
		if err := m.Supersede(ctx, active, in.Actor, "new logical task"); err != nil {
			return nil, err
		}
	}
	if err := NormalizeContract(c); err != nil {
		return nil, err
	}
	if err := ValidateContract(c); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(c)
	ev := LogicalTaskEvent{
		SessionID:      c.SessionID,
		LogicalTaskID:  c.LogicalTaskID,
		Revision:       c.Revision,
		Kind:           LogicalEventCreated,
		Payload:        string(payload),
		OriginEventID:  in.OriginEventID,
		IdempotencyKey: m.nextIdemKey(c.SessionID, "create", c.LogicalTaskID, c.Revision),
		Owner:          c.Owner,
	}
	if err := m.store.CreateTask(ctx, c, ev); err != nil {
		return nil, err
	}
	m.observe(Observation{Action: "create", SessionID: c.SessionID, TaskID: c.LogicalTaskID,
		Revision: c.Revision, ContentHash: c.ContentHash, Actor: in.Actor, Reason: "ensure task"})
	return c, nil
}

// currentTask 返回会话中"当前"任务：优先 active/draft 投影；若会话已有终态任务但其投影
// 未被替代（verified/delivered-unverified/failed/cancelled），仍返回该终态任务——
// 这样 EnsureTask 才能把新输入建为新任务并记录 RelatedTaskIDs + supersede 旧的，
// 而不是把它误当成"会话没有任务"（旧缺陷：终态后 GetActive 返回 NotFound，新任务
// 丢失与旧任务的关系，旧投影一直停在 verified）。
func (m *TaskManager) currentTask(ctx context.Context, sessionID string) (*LogicalTaskContract, error) {
	if c, err := m.store.GetActive(ctx, sessionID); err == nil {
		return c, nil
	} else if !errors.Is(err, ErrLogicalTaskNotFound) {
		return nil, err
	}
	tasks, err := m.store.ListTasks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Status != TaskStatusSuperseded {
			return tasks[i], nil
		}
	}
	return nil, ErrLogicalTaskNotFound
}

// inheritFromHistory 从会话内最近一条终态任务计算不变量继承结果。
func (m *TaskManager) inheritFromHistory(ctx context.Context, in TaskInput) (InvariantInheritance, bool) {
	tasks, err := m.store.ListTasks(ctx, in.SessionID)
	if err != nil || len(tasks) == 0 {
		return InvariantInheritance{}, false
	}
	var prev *LogicalTaskContract
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Status != TaskStatusActive && tasks[i].Status != TaskStatusDraft {
			prev = tasks[i]
			break
		}
	}
	if prev == nil {
		return InvariantInheritance{}, false
	}
	rules := append(append([]ResourceRule{}, in.WriteSet...), in.ReadSet...)
	return InheritInvariants(prev, in.Targets, rules, in.Goal), true
}

// Rebuild 从事件重建投影（重启对账/恢复）。
func (m *TaskManager) Rebuild(ctx context.Context, sessionID, taskID string) (*LogicalTaskContract, error) {
	if m == nil || m.store == nil {
		return nil, ErrLogicalTaskNotFound
	}
	events, err := m.store.ListEvents(ctx, sessionID, taskID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrLogicalTaskNotFound
	}
	return RebuildProjection(sessionID, taskID, events)
}

// nextTaskID 生成稳定可读且会话内唯一的任务 ID。
// 用管理器内单调序号而非纯时间戳：同一纳秒内的连续调用（纠正立即接续/测试）也不会撞 ID。
func (m *TaskManager) nextTaskID(sessionID string, t time.Time) string {
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	prefix := sessionID
	if prefix == "" {
		prefix = "task"
	}
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	return fmt.Sprintf("lt-%s-%d-%d", prefix, t.UnixNano(), seq)
}

// mergeTargets 合并语义目标：同 ID 覆盖，新增追加。
func mergeTargets(old, add []SemanticTarget) []SemanticTarget {
	out := append([]SemanticTarget{}, old...)
	index := map[string]int{}
	for i, t := range out {
		index[t.ID] = i
	}
	for _, t := range add {
		if i, ok := index[t.ID]; ok {
			out[i] = t
			continue
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	return out
}

// mergeInvariants 合并不变量：同 ID 覆盖（保留旧状态），新增追加。
func mergeInvariants(old, add []Invariant) []Invariant {
	out := append([]Invariant{}, old...)
	index := map[string]int{}
	for i, inv := range out {
		index[inv.ID] = i
	}
	for _, inv := range add {
		if i, ok := index[inv.ID]; ok {
			if out[i].Status == "" {
				out[i].Status = InvariantActive
			}
			inv.Status = out[i].Status
			inv.SourceTaskID = firstNonEmpty(inv.SourceTaskID, out[i].SourceTaskID)
			out[i] = inv
			continue
		}
		if inv.Status == "" {
			inv.Status = InvariantActive
		}
		index[inv.ID] = len(out)
		out = append(out, inv)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
