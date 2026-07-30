// Package orchestrator 提供 Agent 树编排元数据层。
//
// Dispatcher 在派发子 Agent 时调用 Register/SetCancel/Finish 维护权威树，
// HTTP API 与 TUI 通过 Snapshot 读取，Cancel 端点通过存储的 cancel func 取消子 Agent。
// 不参与 ReAct 循环控制流，仅作可观测/可取消的元数据层。
package orchestrator

import (
	"context"
	"sync"
	"time"
)

// Status 表示 Agent 节点在树中的生命周期状态。
type Status int

const (
	StatusRunning Status = iota
	StatusDone
	StatusFailed
	StatusCancelled
)

// String 返回状态的可读名称，用于日志与 JSON 序列化。
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	}
	return "unknown"
}

// MarshalJSON 实现 json.Marshaler，使 Status 序列化为字符串而非整数。
func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// Node 是 Agent 树中的一个节点快照（值类型，外部不可变）。
type Node struct {
	ID       string    `json:"id"`
	ParentID string    `json:"parent_id,omitempty"`
	Role     string    `json:"role"`
	Domain   string    `json:"domain,omitempty"`
	Task     string    `json:"task,omitempty"`
	Status   Status    `json:"status"`
	Started  time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitzero"`
	Summary  string    `json:"summary,omitempty"`
	Err      string    `json:"err,omitempty"`
}

// Tree 维护单个会话的 Agent 树权威状态。
// nodes 存储节点元数据，cancels 存储对应 subAgentID 的 cancel func。
// 两个 map 同步生命周期：Register/Finish/Cancel 都先持锁再操作。
type Tree struct {
	mu      sync.RWMutex
	nodes   map[string]*Node
	cancels map[string]context.CancelFunc
}

// NewTree 构造空树。
func NewTree() *Tree {
	return &Tree{
		nodes:   make(map[string]*Node),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Register 写入新节点。若同 ID 已存在则覆盖（派发竞态保护）。
// 不设置 cancels 条目；SetCancel 单独调用以解耦 Register 与 ctx 创建时机。
func (t *Tree) Register(n Node) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n.Started.IsZero() {
		n.Started = time.Now()
	}
	if n.Status == 0 {
		n.Status = StatusRunning
	}
	node := n
	t.nodes[node.ID] = &node
}

// SetCancel 绑定 subAgentID 对应的 cancel func。
// 在 goroutine 启动前调用，保证 Cancel 端点不会因时序漏掉 cancel 句柄。
// 若节点不存在则忽略（Register 未调用的边缘场景）。
func (t *Tree) SetCancel(id string, cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[id]; !ok {
		return
	}
	t.cancels[id] = cancel
}

// Finish 标记节点结束。err 非 nil 则 StatusFailed，否则 StatusDone。
// summary 为最终文本或部分结果。删除 cancels[id] 防止 map 无界增长。
// 幂等：已 terminal 的节点重复调用 no-op。
func (t *Tree) Finish(id, summary string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	node, ok := t.nodes[id]
	if !ok {
		return
	}
	if node.Status == StatusDone || node.Status == StatusFailed || node.Status == StatusCancelled {
		return
	}
	node.Finished = time.Now()
	node.Summary = summary
	if err != nil {
		node.Status = StatusFailed
		node.Err = err.Error()
	} else {
		node.Status = StatusDone
	}
	delete(t.cancels, id)
}

// Cancel 调用已绑定的 cancel func 并将状态置为 StatusCancelled。
// 返回是否找到对应节点且处于可取消状态（Running）。
// context.CancelFunc 幂等（Go doc），与 goroutine defer cancel 重复调用安全。
// 已 terminal 的节点 no-op。
func (t *Tree) Cancel(id string) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok {
		return false
	}
	if node.Status != StatusRunning {
		t.mu.Unlock()
		return false
	}
	cancel, hasCancel := t.cancels[id]
	node.Status = StatusCancelled
	node.Finished = time.Now()
	delete(t.cancels, id)
	t.mu.Unlock()
	if hasCancel && cancel != nil {
		cancel()
	}
	return true
}

// Get 返回指定 ID 的节点拷贝。不存在返回 false。
func (t *Tree) Get(id string) (Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	node, ok := t.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *node, true
}

// Snapshot 返回所有节点的值拷贝切片，按 Started 升序。
// 外部修改不影响内部状态。
func (t *Tree) Snapshot() []Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Node, 0, len(t.nodes))
	for _, node := range t.nodes {
		out = append(out, *node)
	}
	// 简单插入排序：节点数通常 <100，无需 sort.Slice 引入依赖。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Started.Before(out[j-1].Started); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
