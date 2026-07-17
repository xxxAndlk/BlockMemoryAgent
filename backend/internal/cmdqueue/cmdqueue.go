// Package cmdqueue 提供按会话隔离的用户指令队列，支持特性6的
// "队列注入"与"抢占中断"两种交互模式。
//
// 设计意图：graph.Invoke 是同步循环，无法在运行中直接接收 HTTP 写入。
// SessionManager 在 HTTP 处理器中把用户指令写入队列，MetaAgent 在每个
// tick 顶部读取队列，从而实现"运行中追加指令"与"中断后重置上下文"。
package cmdqueue

import (
	"context" // 日志调用上下文
	"errors"  // 构造 ErrQueueFull 等哨兵错误
	"fmt"     // 格式化日志消息
	"log"     // 未注入 logger 时的回退输出
	"sync"    // 读写锁与互斥锁

	"github.com/blockmemory/agent/backend/internal/logger" // 结构化日志器
)

// Intent 表示用户指令的处置意图。
type Intent int

const (
	// IntentEnqueue 表示队列注入：将指令追加到当前上下文继续执行。
	IntentEnqueue Intent = iota
	// IntentInterrupt 表示抢占中断：清空当前上下文，以新指令重新启动。
	IntentInterrupt
)

// Item 是队列中的单个元素：一条用户指令 + 处置意图。
type Item struct {
	Content string // 用户输入的指令文本
	Intent  Intent // 处置意图：追加或中断
}

// defaultQueueCap 是默认单会话队列容量。
const defaultQueueCap = 100

// ErrQueueFull 当队列达到容量上限时返回。
var ErrQueueFull = errors.New("queue full")

// Queue 表示单个会话的指令队列。
//
// 并发说明：所有访问受 mu 保护，可被多 goroutine 安全调用。
type Queue struct {
	mu    sync.Mutex // 保护 items 与 cap 字段
	items []Item     // 队列中的指令列表，按入队时间升序
	cap   int        // 队列容量上限
}

// Manager 是进程级队列管理器，按 sessionID 隔离不同会话的队列。
//
// 并发说明：
//   - queues map 受 mu 保护。
//   - 读操作使用 RLock，写操作使用 Lock。
//   - 单个 Queue 的并发安全由 Queue.mu 自身保证。
type Manager struct {
	mu     sync.RWMutex      // 保护 queues map
	queues map[string]*Queue // 按 sessionID 索引的队列表
	log    *logger.Logger    // 结构化日志器，由 SetLogger 注入；nil 时回退标准库 log
}

// NewManager 创建空的队列管理器。
//
// 返回：已初始化 queues map 的 *Manager。
func NewManager() *Manager {
	return &Manager{queues: make(map[string]*Queue)}
}

// SetLogger 注入结构化日志器，使丢弃指令等错误以 [ERRO] 级别输出。
//
// 参数：l 为已初始化的 Logger 指针；未注入时回退标准库 log（级别固定 INFO）。
func (m *Manager) SetLogger(l *logger.Logger) {
	m.log = l
}

// getOrCreate 取或创建指定会话的队列。
//
// 实现细节：使用双重检查锁定（double-checked locking）减少写锁争用。
//
// 参数：sessionID 会话唯一标识。
//
// 返回：该会话对应的 *Queue（可能是新创建的）。
func (m *Manager) getOrCreate(sessionID string) *Queue {
	// 先尝试读锁，命中则直接返回
	m.mu.RLock()
	q, ok := m.queues[sessionID]
	m.mu.RUnlock()
	if ok {
		return q
	}
	// 未命中则加写锁创建
	m.mu.Lock()
	defer m.mu.Unlock()
	// 二次检查避免多个 goroutine 同时创建多个队列
	if q, ok = m.queues[sessionID]; ok {
		return q
	}
	// 新建队列并登记
	q = &Queue{cap: defaultQueueCap}
	m.queues[sessionID] = q
	return q
}

// Enqueue 追加一条指令到指定会话队列；队列满时返回 ErrQueueFull。
//
// 参数：
//   - sessionID：目标会话 ID。
//   - item：待追加的指令项。
//
// 返回：成功返回 nil；队列满返回 ErrQueueFull。
func (m *Manager) Enqueue(sessionID string, item Item) error {
	q := m.getOrCreate(sessionID)
	return q.Enqueue(item)
}

// Enqueue 追加一条指令到队列；队列满时返回 ErrQueueFull。
//
// 参数：item 待追加的指令项。
//
// 返回：成功返回 nil；队列满返回 ErrQueueFull。
func (q *Queue) Enqueue(item Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 容量检查，避免无界增长
	if len(q.items) >= q.cap {
		return ErrQueueFull
	}
	q.items = append(q.items, item)
	return nil
}

// Push 是 Enqueue 的兼容包装：队列满时记录错误并丢弃指令。
//
// 已废弃：新代码应直接使用 Enqueue 并处理 ErrQueueFull。
//
// 参数：
//   - sessionID：目标会话 ID。
//   - item：待追加的指令项。
func (m *Manager) Push(sessionID string, item Item) {
	// 委托给 Enqueue；失败只记录日志，不向上传播错误
	if err := m.Enqueue(sessionID, item); err != nil {
		if m.log != nil {
			m.log.Error(context.Background(), fmt.Sprintf("cmdqueue: Push dropped item for session %s", sessionID), err)
		} else {
			log.Printf("cmdqueue: Push dropped item for session %s: %v", sessionID, err)
		}
	}
}

// Drain 取出并清空指定会话队列。
//
// 返回：按入队时间升序排列的 Item 切片；若会话无队列返回 nil。
func (m *Manager) Drain(sessionID string) []Item {
	// 先读锁查找队列
	m.mu.RLock()
	q, ok := m.queues[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	// 加队列锁，交换出 items 并清空
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// HasPending 检查会话是否有待处理指令（不消费）。
//
// 参数：sessionID 会话 ID。
//
// 返回：有未消费指令返回 true；否则返回 false。
func (m *Manager) HasPending(sessionID string) bool {
	// 读锁查找队列
	m.mu.RLock()
	q, ok := m.queues[sessionID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	// 加队列锁检查长度
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) > 0
}

// Delete 清理会话队列（会话结束时调用）。
//
// 参数：sessionID 待清理的会话 ID。
//
// 说明：删除是幂等的，缺键时无副作用。
func (m *Manager) Delete(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.queues, sessionID)
}
