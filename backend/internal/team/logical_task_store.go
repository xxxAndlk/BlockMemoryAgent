package team

// logical_task_store.go 逻辑任务存储接口 + 进程内实现（WP1）。
//
// 事务与幂等要求（计划 §3.5）：
//   - 投影表 logical_tasks 以 (session_id, logical_task_id) 为键，只保存当前状态；
//   - 事件表 logical_task_events append-only，idempotency_key 唯一（重试重放不重复写入）；
//   - 同一会话只允许一条 active 投影（新任务/新 revision 落库时旧投影转 superseded）；
//   - 失败必须显式返回错误：不允许"写失败但投影看起来成功"。
//
// PG 实现见 internal/store/logical_task_store.go；本文件的内存实现供单测与
// 无 PG 场景（Cmd/TUI 离线）使用，语义与 PG 版一致。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogicalTaskStore 逻辑任务持久化接口。
type LogicalTaskStore interface {
	// CreateTask 写入初始任务（created 事件 + 投影）。
	CreateTask(ctx context.Context, c *LogicalTaskContract, ev LogicalTaskEvent) error
	// AppendEvent 追加事件并同步更新投影（幂等：相同 idempotency_key 不重复写入）。
	AppendEvent(ctx context.Context, c *LogicalTaskContract, ev LogicalTaskEvent) error
	// GetActive 返回会话当前 active 投影（无则返回 nil, ErrLogicalTaskNotFound）。
	GetActive(ctx context.Context, sessionID string) (*LogicalTaskContract, error)
	// GetByID 返回指定任务投影（含非 active；回收站/审计用）。
	GetByID(ctx context.Context, sessionID, taskID string) (*LogicalTaskContract, error)
	// ListEvents 返回任务事件（按 event_id 升序；taskID 空表示会话全部）。
	ListEvents(ctx context.Context, sessionID, taskID string) ([]LogicalTaskEvent, error)
	// ListTasks 返回会话全部任务投影（按 revision 升序）。
	ListTasks(ctx context.Context, sessionID string) ([]*LogicalTaskContract, error)
	// FindAcceptable 返回会话中**当前可接受**的任务（交付事实：状态为 verified 或
	// delivered-unverified，取最新创建/交付的那一条）。
	//
	// 为什么必须由存储层提供（2026-11-19 再次验收 P1-2）：`ListTasks` 按 revision 排序，
	// 已被多次 refine 的旧 superseded 任务 revision 更大，取“数组末尾”会选错任务——
	// 要么拒掉真正待接受的结果，要么把一个废弃任务改成 accepted。接受语义必须由
	// 交付事实（状态 + 创建/更新时间）决定，不得用排序数组末尾推断。
	// 无可接受任务时返回 ErrLogicalTaskNotFound。
	FindAcceptable(ctx context.Context, sessionID string) (*LogicalTaskContract, error)
}

// ErrLogicalTaskNotFound 投影不存在（legacy 会话走 fallback）。
var ErrLogicalTaskNotFound = fmt.Errorf("logical task not found")

// MemoryLogicalTaskStore 进程内实现（测试/无 PG）。
type MemoryLogicalTaskStore struct {
	mu     sync.Mutex
	tasks  map[string]map[string]*LogicalTaskContract // sessionID -> taskID -> 投影
	order  map[string][]string                        // sessionID -> taskID 插入顺序（稳定读取）
	events map[string][]LogicalTaskEvent              // sessionID -> 事件
	idem   map[string]bool                            // sessionID + idempotency_key
	seq    int64
}

// NewMemoryLogicalTaskStore 构造内存逻辑任务存储。
func NewMemoryLogicalTaskStore() *MemoryLogicalTaskStore {
	return &MemoryLogicalTaskStore{
		tasks:  map[string]map[string]*LogicalTaskContract{},
		order:  map[string][]string{},
		events: map[string][]LogicalTaskEvent{},
		idem:   map[string]bool{},
	}
}

func idemKey(sessionID, key string) string { return sessionID + "\x00" + key }

// CreateTask 实现 LogicalTaskStore。
func (m *MemoryLogicalTaskStore) CreateTask(_ context.Context, c *LogicalTaskContract, ev LogicalTaskEvent) error {
	if c == nil {
		return fmt.Errorf("logical task: contract required")
	}
	if err := NormalizeContract(c); err != nil {
		return err
	}
	if err := ValidateContract(c); err != nil {
		return err
	}
	if strings.TrimSpace(ev.IdempotencyKey) == "" {
		return fmt.Errorf("logical task: idempotency_key required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idem[idemKey(c.SessionID, ev.IdempotencyKey)] {
		return nil // 幂等重放
	}
	if _, exists := m.tasks[c.SessionID][c.LogicalTaskID]; exists && m.tasks[c.SessionID] != nil {
		return fmt.Errorf("logical task %s already exists", c.LogicalTaskID)
	}
	// 单 active 约束：新任务落库时旧 active 转 superseded（历史保留）。
	m.supersedeActiveLocked(c.SessionID, c.LogicalTaskID, c.Revision)
	if m.tasks[c.SessionID] == nil {
		m.tasks[c.SessionID] = map[string]*LogicalTaskContract{}
	}
	c.CreatedAt = time.Now()
	clone := *c
	m.tasks[c.SessionID][c.LogicalTaskID] = &clone
	m.order[c.SessionID] = append(m.order[c.SessionID], c.LogicalTaskID)
	ev.SessionID = c.SessionID
	ev.LogicalTaskID = c.LogicalTaskID
	if ev.Revision <= 0 {
		ev.Revision = c.Revision
	}
	m.seq++
	ev.EventID = m.seq
	ev.CreatedAt = time.Now()
	m.events[c.SessionID] = append(m.events[c.SessionID], ev)
	m.idem[idemKey(c.SessionID, ev.IdempotencyKey)] = true
	return nil
}

// AppendEvent 实现 LogicalTaskStore：事件先入账后更新投影（append-only 纪律）。
func (m *MemoryLogicalTaskStore) AppendEvent(_ context.Context, c *LogicalTaskContract, ev LogicalTaskEvent) error {
	if c == nil {
		return fmt.Errorf("logical task: contract required")
	}
	if err := NormalizeContract(c); err != nil {
		return err
	}
	if err := ValidateContract(c); err != nil {
		return err
	}
	if strings.TrimSpace(ev.IdempotencyKey) == "" {
		return fmt.Errorf("logical task: idempotency_key required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idem[idemKey(c.SessionID, ev.IdempotencyKey)] {
		return nil
	}
	if m.tasks[c.SessionID] == nil || m.tasks[c.SessionID][c.LogicalTaskID] == nil {
		return fmt.Errorf("logical task %s not found in %s", c.LogicalTaskID, c.SessionID)
	}
	if c.Status == TaskStatusActive || c.Status == TaskStatusDraft {
		m.supersedeActiveLocked(c.SessionID, c.LogicalTaskID, c.Revision)
	}
	clone := *c
	if exists := m.tasks[c.SessionID][c.LogicalTaskID]; exists != nil {
		clone.CreatedAt = exists.CreatedAt
	}
	m.tasks[c.SessionID][c.LogicalTaskID] = &clone
	ev.SessionID = c.SessionID
	ev.LogicalTaskID = c.LogicalTaskID
	if ev.Revision <= 0 {
		ev.Revision = c.Revision
	}
	m.seq++
	ev.EventID = m.seq
	ev.CreatedAt = time.Now()
	m.events[c.SessionID] = append(m.events[c.SessionID], ev)
	m.idem[idemKey(c.SessionID, ev.IdempotencyKey)] = true
	return nil
}

// supersedeActiveLocked 把同会话其它 active 投影标记为 superseded（只改状态，不删行）。
func (m *MemoryLogicalTaskStore) supersedeActiveLocked(sessionID, keepTaskID string, keepRevision int64) {
	for id, proj := range m.tasks[sessionID] {
		if proj == nil || id == keepTaskID {
			continue
		}
		if proj.Status == TaskStatusActive || proj.Status == TaskStatusDraft {
			proj.Status = TaskStatusSuperseded
		}
	}
	// 同任务的新 revision：旧 revision 已在同一投影上覆盖，事件底账保留。
	_ = keepRevision
}

// GetActive 实现 LogicalTaskStore。
func (m *MemoryLogicalTaskStore) GetActive(_ context.Context, sessionID string) (*LogicalTaskContract, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order[sessionID] {
		proj := m.tasks[sessionID][id]
		if proj != nil && (proj.Status == TaskStatusActive || proj.Status == TaskStatusDraft) {
			clone := *proj
			return &clone, nil
		}
	}
	return nil, ErrLogicalTaskNotFound
}

// GetByID 实现 LogicalTaskStore。
func (m *MemoryLogicalTaskStore) GetByID(_ context.Context, sessionID, taskID string) (*LogicalTaskContract, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proj := m.tasks[sessionID][taskID]
	if proj == nil {
		return nil, ErrLogicalTaskNotFound
	}
	clone := *proj
	return &clone, nil
}

// ListEvents 实现 LogicalTaskStore。
func (m *MemoryLogicalTaskStore) ListEvents(_ context.Context, sessionID, taskID string) ([]LogicalTaskEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LogicalTaskEvent, 0, len(m.events[sessionID]))
	for _, ev := range m.events[sessionID] {
		if taskID != "" && ev.LogicalTaskID != taskID {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// ListTasks 实现 LogicalTaskStore。
func (m *MemoryLogicalTaskStore) ListTasks(_ context.Context, sessionID string) ([]*LogicalTaskContract, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*LogicalTaskContract, 0, len(m.order[sessionID]))
	for _, id := range m.order[sessionID] {
		if proj := m.tasks[sessionID][id]; proj != nil {
			clone := *proj
			out = append(out, &clone)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Revision != out[j].Revision {
			return out[i].Revision < out[j].Revision
		}
		return out[i].LogicalTaskID < out[j].LogicalTaskID
	})
	return out, nil
}

// FindAcceptable 实现 LogicalTaskStore：按**创建/交付顺序倒序**取最新可接受任务。
//
// 倒序遍历插入顺序（m.order 就是创建顺序），而不是按 revision 排序——revision 只在
// 同一任务内递增，旧任务被 refine 多次后 revision 会比新任务大，用 revision 会选错。
func (m *MemoryLogicalTaskStore) FindAcceptable(_ context.Context, sessionID string) (*LogicalTaskContract, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.order[sessionID]
	for i := len(ids) - 1; i >= 0; i-- {
		proj := m.tasks[sessionID][ids[i]]
		if proj == nil || !TaskAcceptsUserDecision(proj.Status) {
			continue
		}
		clone := *proj
		return &clone, nil
	}
	return nil, ErrLogicalTaskNotFound
}
