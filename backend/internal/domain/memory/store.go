package memory

// 导入上下文与同步包：context 用于接口签名；sync 提供读写锁保证并发安全。
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

	// compressStates 以 agentID 为键保存层级压缩状态（压缩金字塔），模拟落库恢复。
	compressStates map[string]compressState

	// maxEventsPerAgent 控制每个代理在内存中最多保留的事件条数，超出时丢弃最旧事件。
	maxEventsPerAgent int
}

// NewInMemoryStore 创建并返回一个空的内存存储实例。
func NewInMemoryStore() *InMemoryStore {
	// 初始化 InMemoryStore，并分配空的事件字典，避免后续空指针访问。
	// maxEventsPerAgent 默认使用 DefaultMaxEventsPerAgent，防止事件流无限增长。
	return &InMemoryStore{
		events:           make(map[string][]agent.MemoryEvent),
		compressStates:   make(map[string]compressState),
		maxEventsPerAgent: DefaultMaxEventsPerAgent,
	}
}

// WithMaxEventsPerAgent 配置每个代理在内存中最多保留的事件条数。
// 参数 n 不大于 DefaultEventLimit 时回退到 DefaultMaxEventsPerAgent，
// 保证容量上限始终大于 Pipeline 的默认注入上限。
func (s *InMemoryStore) WithMaxEventsPerAgent(n int) *InMemoryStore {
	if n <= DefaultEventLimit {
		// n 不合法或过小时回退到默认值，维持容量上限大于 DefaultEventLimit 的不变式
		n = DefaultMaxEventsPerAgent
	}
	s.maxEventsPerAgent = n
	// 返回自身以支持链式调用，例如 NewInMemoryStore().WithMaxEventsPerAgent(500)
	return s
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

	// 超过每代理容量上限时丢弃最旧事件，仅保留最新 maxEventsPerAgent 条，防止内存无限增长。
	if s.maxEventsPerAgent > 0 && len(s.events[agentID]) > s.maxEventsPerAgent {
		s.events[agentID] = s.events[agentID][len(s.events[agentID])-s.maxEventsPerAgent:]
	}

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

// SaveCompressState 保存指定代理的层级压缩状态（深拷贝 Bundles，避免共享底层数组）。
func (s *InMemoryStore) SaveCompressState(_ context.Context, agentID string, state *compressState) error {
	if s == nil || state == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := compressState{TailStart: state.TailStart}
	cp.Bundles = append([]string(nil), state.Bundles...)
	s.compressStates[agentID] = cp
	return nil
}

// LoadCompressState 加载指定代理的层级压缩状态；无数据返回 (nil, nil)。
func (s *InMemoryStore) LoadCompressState(_ context.Context, agentID string) (*compressState, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.compressStates[agentID]
	if !ok {
		return nil, nil
	}
	cp := compressState{TailStart: st.TailStart}
	cp.Bundles = append([]string(nil), st.Bundles...)
	return &cp, nil
}
