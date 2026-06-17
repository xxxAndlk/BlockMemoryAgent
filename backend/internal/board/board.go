// Package board 实现 v3 §7.1 设计的任务看板。
//
// 一个 TaskBoard 对应一个用户级"话题"（topic），由 MetaAgent 在
// 会话启动时创建，所有 DomainAgent / SubDomainAgent 通过看板观察
// 全局状态（目标、子任务、约束、进度），但任何状态变更都必须经过
// 看板 API 以保证原子性，从而避免在多 Agent 并发下污染上下文。
package board

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TaskStatus 子任务状态
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskBlocked    TaskStatus = "blocked"
	TaskDone       TaskStatus = "done"
	TaskFailed     TaskStatus = "failed"
)

// SubTask 看板上的一个子任务
type SubTask struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Assignee   string     `json:"assignee,omitempty"` // domainAgent / assistant 实例 ID
	Status     TaskStatus `json:"status"`
	Result     string     `json:"result,omitempty"`
	DependsOn  []string   `json:"depends_on,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// TaskBoard 任务看板
type TaskBoard struct {
	mu sync.RWMutex

	TopicID     string            // 话题 ID（=会话 ID）
	Goal        string            // 全局目标
	Status      string            // NEW / IN_PROGRESS / DONE / FAILED
	Constraints map[string]string // 全局约束（如"兼容旧版 API"）
	Tasks       map[string]*SubTask
	Order       []string // 子任务展示顺序（创建序）
	UpdatedAt   time.Time
}

// NewTaskBoard 新建空看板
func NewTaskBoard(topicID, goal string) *TaskBoard {
	return &TaskBoard{
		TopicID:     topicID,
		Goal:        goal,
		Status:      "NEW",
		Constraints: make(map[string]string),
		Tasks:       make(map[string]*SubTask),
		UpdatedAt:   time.Now(),
	}
}

// AddSubTask 追加一个子任务，title 重复时返回已有 ID
func (b *TaskBoard) AddSubTask(title string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil && t.Title == title {
			return id
		}
	}
	id := fmt.Sprintf("%s_t%d", b.TopicID, len(b.Order)+1)
	now := time.Now()
	b.Tasks[id] = &SubTask{
		ID:        id,
		Title:     title,
		Status:    TaskPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	b.Order = append(b.Order, id)
	b.Status = "IN_PROGRESS"
	b.UpdatedAt = now
	return id
}

// SetConstraint 设置全局约束
func (b *TaskBoard) SetConstraint(key, value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Constraints[key] = value
	b.UpdatedAt = time.Now()
}

// Assign 把某个子任务分配给某 Agent
func (b *TaskBoard) Assign(taskID, assignee string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.Tasks[taskID]
	if !ok {
		return errors.New("task not found")
	}
	t.Assignee = assignee
	t.Status = TaskInProgress
	t.UpdatedAt = time.Now()
	b.UpdatedAt = t.UpdatedAt
	return nil
}

// MarkDone 标记任务完成（带结果）
func (b *TaskBoard) MarkDone(taskID, result string) error {
	return b.transition(taskID, TaskDone, result)
}

// MarkFailed 标记任务失败
func (b *TaskBoard) MarkFailed(taskID, reason string) error {
	return b.transition(taskID, TaskFailed, reason)
}

// MarkBlocked 标记任务被阻塞
func (b *TaskBoard) MarkBlocked(taskID, reason string) error {
	return b.transition(taskID, TaskBlocked, reason)
}

func (b *TaskBoard) transition(taskID string, status TaskStatus, result string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.Tasks[taskID]
	if !ok {
		return errors.New("task not found")
	}
	t.Status = status
	t.Result = result
	t.UpdatedAt = time.Now()
	b.UpdatedAt = t.UpdatedAt
	b.recomputeStatusLocked()
	return nil
}

// recomputeStatusLocked 重新计算看板整体状态（mu 已加锁时调用）
func (b *TaskBoard) recomputeStatusLocked() {
	if len(b.Tasks) == 0 {
		b.Status = "NEW"
		return
	}
	allTerminal, anyFailed := true, false
	for _, t := range b.Tasks {
		switch t.Status {
		case TaskFailed:
			anyFailed = true
		case TaskDone:
			// ok
		default:
			allTerminal = false
		}
	}
	switch {
	case allTerminal && anyFailed:
		b.Status = "FAILED"
	case allTerminal:
		b.Status = "DONE"
	default:
		b.Status = "IN_PROGRESS"
	}
}

// Snapshot 返回看板的只读视图（拷贝），供注入 Agent 上下文使用
//
// 注入时必须只用 Snapshot()，禁止把 *TaskBoard 直接序列化，
// 防止外部代码绕过锁修改内部状态。
type Snapshot struct {
	TopicID     string            `json:"topic_id"`
	Goal        string            `json:"goal"`
	Status      string            `json:"status"`
	Constraints map[string]string `json:"constraints"`
	Tasks       []SubTask         `json:"tasks"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Snapshot 返回看板视图
func (b *TaskBoard) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cs := make(map[string]string, len(b.Constraints))
	for k, v := range b.Constraints {
		cs[k] = v
	}
	tasks := make([]SubTask, 0, len(b.Order))
	for _, id := range b.Order {
		if t := b.Tasks[id]; t != nil {
			tasks = append(tasks, *t)
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
func (b *TaskBoard) Brief(maxTasks int) string {
	if maxTasks <= 0 {
		maxTasks = 6
	}
	snap := b.Snapshot()
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
	count := 0
	for _, t := range snap.Tasks {
		if count >= maxTasks {
			out += fmt.Sprintf("...(%d more)\n", len(snap.Tasks)-count)
			break
		}
		assign := "-"
		if t.Assignee != "" {
			assign = t.Assignee
		}
		out += fmt.Sprintf("  [%s] %s -> %s (%s)\n", t.Status, t.Title, assign, t.ID)
		count++
	}
	return out
}

// Manager 多看板管理器（一个会话一个 TaskBoard）
type Manager struct {
	mu     sync.RWMutex
	boards map[string]*TaskBoard
}

// NewManager 创建管理器
func NewManager() *Manager {
	return &Manager{boards: make(map[string]*TaskBoard)}
}

// Get 获取看板，没有则返回 nil
func (m *Manager) Get(topicID string) *TaskBoard {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.boards[topicID]
}

// GetOrCreate 取或新建看板
func (m *Manager) GetOrCreate(topicID, goal string) *TaskBoard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boards[topicID]; ok {
		return b
	}
	b := NewTaskBoard(topicID, goal)
	m.boards[topicID] = b
	return b
}

// Remove 移除看板
func (m *Manager) Remove(topicID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.boards, topicID)
}
