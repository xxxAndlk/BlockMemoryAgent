// Package cmdqueue 提供按会话隔离的用户指令队列，支持特性6的
// "队列注入"与"抢占中断"两种交互模式。
//
// 设计意图：graph.Invoke 是同步循环，无法在运行中直接接收 HTTP 写入。
// SessionManager 在 HTTP 处理器中把用户指令写入队列，MetaAgent 在每个
// tick 顶部读取队列，从而实现"运行中追加指令"与"中断后重置上下文"。
package cmdqueue

import (
	"sync"
)

// Intent 指令意图，区分抢占中断与队列注入。
type Intent int

const (
	IntentEnqueue Intent = iota // 队列注入：追加到当前上下文继续执行
	IntentInterrupt             // 抢占中断：清空当前上下文，以新指令重新启动
)

// Item 队列项：一条用户指令 + 意图。
type Item struct {
	Content string // 用户输入的指令文本
	Intent  Intent // 处置意图
}

// Queue 单会话指令队列。并发安全。
type Queue struct {
	mu    sync.Mutex
	items []Item
}

// Manager 进程级队列管理器，按 sessionID 隔离。
type Manager struct {
	mu     sync.RWMutex
	queues map[string]*Queue
}

// NewManager 创建空管理器。
func NewManager() *Manager {
	return &Manager{queues: make(map[string]*Queue)}
}

// getOrCreate 取或创建会话队列。
func (m *Manager) getOrCreate(sessionID string) *Queue {
	m.mu.RLock()
	q, ok := m.queues[sessionID]
	m.mu.RUnlock()
	if ok {
		return q
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 二次检查避免竞态
	if q, ok = m.queues[sessionID]; ok {
		return q
	}
	q = &Queue{}
	m.queues[sessionID] = q
	return q
}

// Push 追加一条指令到会话队列。
func (m *Manager) Push(sessionID string, item Item) {
	q := m.getOrCreate(sessionID)
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
}

// Drain 取出并清空会话队列。返回切片顺序按入队时间升序。
func (m *Manager) Drain(sessionID string) []Item {
	m.mu.RLock()
	q, ok := m.queues[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// Delete 清理会话队列（会话结束时调用）。
func (m *Manager) Delete(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.queues, sessionID)
}
