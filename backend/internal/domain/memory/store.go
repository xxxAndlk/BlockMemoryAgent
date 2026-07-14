package memory

import (
	"context"
	"sync"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// InMemoryStore 是一个线程安全的内存型 Store 实现。
// 适用于测试场景以及不需要持久化事件历史的部署环境。
type InMemoryStore struct {
	// mu 用于保护 events 字段的读写互斥锁，保证并发安全。
	mu sync.RWMutex

	// events 以 agentID 为键保存每个代理的事件切片。
	// 键为代理唯一标识，值为该代理按时间顺序追加的记忆事件列表。
	events map[string][]agent.MemoryEvent
}

// NewInMemoryStore 创建并返回一个空的内存存储实例。
func NewInMemoryStore() *InMemoryStore {
	// 初始化 InMemoryStore，并分配空的事件字典，避免后续空指针访问。
	return &InMemoryStore{events: make(map[string][]agent.MemoryEvent)}
}

// SaveEvent 将指定事件追加到对应代理的内存事件流中。
// 参数 _ 是 context.Context，当前实现未使用但保留接口签名。
// 参数 agentID 是目标代理的唯一标识。
// 参数 event 是要保存的记忆事件。
// 返回值 error 表示保存过程中是否发生错误。
func (s *InMemoryStore) SaveEvent(_ context.Context, agentID string, event agent.MemoryEvent) error {
	// 如果接收者为 nil，直接返回 nil 避免空指针解引用崩溃。
	if s == nil {
		return nil
	}

	// 加写锁，防止并发写入导致数据竞争或切片损坏。
	s.mu.Lock()

	// 函数返回前释放写锁，确保锁总是被释放。
	defer s.mu.Unlock()

	// 将事件追加到对应 agentID 的事件切片末尾。
	s.events[agentID] = append(s.events[agentID], event)

	// 保存成功，返回 nil 错误。
	return nil
}

// LoadEvents 返回指定代理最近最多 limit 条事件，按最旧到最新排序。
// 参数 _ 是 context.Context，当前实现未使用但保留接口签名。
// 参数 agentID 是目标代理的唯一标识。
// 参数 limit 是希望返回的最大事件数量，若小于等于 0 或超过总数则返回全部。
// 返回值 []agent.MemoryEvent 为查询到的事件切片，error 表示查询是否出错。
func (s *InMemoryStore) LoadEvents(_ context.Context, agentID string, limit int) ([]agent.MemoryEvent, error) {
	// 如果接收者为 nil，返回 nil 切片与 nil 错误，表示没有可用数据。
	if s == nil {
		return nil, nil
	}

	// 加读锁，允许多个读操作并发执行，但阻塞写操作。
	s.mu.RLock()

	// 函数返回前释放读锁，防止死锁。
	defer s.mu.RUnlock()

	// 取出该代理对应的事件源切片。
	src := s.events[agentID]

	// 校验 limit 参数：若 limit 无效或超过源切片长度，则调整为实际长度。
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}

	// 创建长度恰好为 limit 的结果切片。
	out := make([]agent.MemoryEvent, limit)

	// 从源切片尾部复制最近的 limit 条事件到结果切片。
	copy(out, src[len(src)-limit:])

	// 返回复制后的事件切片与 nil 错误。
	return out, nil
}
