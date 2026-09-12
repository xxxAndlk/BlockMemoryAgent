package store

import (
	"context" // 上下文,贯穿所有 Redis 调用以支持超时与取消
	"fmt"     // 格式化错误
	"sync"    // 保护 eventCursors map 的并发访问
	"time"    // Ping 超时控制

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// RedisStore 是 Redis 存储层复合入口。
// 职责: 按领域拆分为子 Store,自身保留共享的 Redis 客户端与 eventCursors 状态,
// 并通过委托包装保留原有公开方法签名,保证接口实现不中断。
// 并发安全: go-redis 客户端内部维护连接池,可在多 goroutine 间共享。
type RedisStore struct {
	client *redis.Client // 共享 Redis 客户端

	Topic    *TopicRedisStore    // 话题维度元数据/约束/依赖图/归档
	Output   *OutputRedisStore   // Agent 输出 Sorted Set
	Event    *EventRedisStore    // 事件流 Stream 与决策日志 List
	Snapshot *SnapshotRedisStore // Agent 快照热缓存
	TTL      *TTLRedisStore      // 话题 TTL 与整话题删除
	AgentMsg *AgentMsgRedisStore // Agent 消息热层（编排页对话视图）

	// eventCursors 维护每个 topic events stream 的已读游标（last message ID）。
	// C4 修复：原 PollEvents 用 XRead "0" 起始，每次返回全量消息，
	// 随 stream 增长从 O(1) 退化为 O(n)。维护游标后只读增量。
	eventCursorsMu sync.Mutex
	eventCursors   map[string]string
}

// NewRedisStore 创建 Redis 存储实例。
// 参数:
//   - ctx:      用于 Ping 超时控制的上下文
//   - addr:     Redis 地址 (host:port)
//   - password: 认证密码,空表示无密码
//   - db:       Redis 数据库编号
//
// 返回:
//   - *RedisStore: 已通过 Ping 校验的存储实例
//   - error: Ping 失败时返回包装错误
func NewRedisStore(ctx context.Context, addr, password string, db int) (*RedisStore, error) {
	// 初始化 go-redis 客户端，尚未真正建连
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	// 主动 Ping 一次,提前暴露网络/凭证类错误；使用超时 context 避免启动挂死
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	// 装配各子 Store，注入共享 client
	return &RedisStore{
		client:       client,
		eventCursors: make(map[string]string),
		Topic:        &TopicRedisStore{client: client},
		Output:       &OutputRedisStore{client: client},
		Event:        &EventRedisStore{client: client},
		Snapshot:     &SnapshotRedisStore{client: client},
		TTL:          &TTLRedisStore{client: client},
		AgentMsg:     &AgentMsgRedisStore{client: client},
	}, nil
}

// Close 关闭 Redis 客户端连接。
// 调用后该 store 不可再用。
// 返回: 关闭错误。
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// Ping 检查 Redis 连通性,用于健康检查。
// 参数:
//   - ctx: 超时控制
//
// 返回: Ping 错误。
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// SaveTopicMeta 保存话题元数据到 Redis Hash (委托给 Topic 子 Store)。
func (s *RedisStore) SaveTopicMeta(ctx context.Context, topic *types.TopicMeta) error {
	return s.Topic.SaveTopicMeta(ctx, topic)
}

// GetTopicMeta 获取话题元数据 (委托给 Topic 子 Store)。
func (s *RedisStore) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	return s.Topic.GetTopicMeta(ctx, topicID)
}

// SetTopicConstraints 设置话题的全局约束 (委托给 Topic 子 Store)。
func (s *RedisStore) SetTopicConstraints(ctx context.Context, topicID string, constraints map[string]string) error {
	return s.Topic.SetTopicConstraints(ctx, topicID, constraints)
}

// GetTopicConstraints 获取话题的全局约束 (委托给 Topic 子 Store)。
func (s *RedisStore) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return s.Topic.GetTopicConstraints(ctx, topicID)
}

// SetDepsGraph 设置话题的依赖关系图 (委托给 Topic 子 Store)。
func (s *RedisStore) SetDepsGraph(ctx context.Context, topicID string, deps map[string][]string) error {
	return s.Topic.SetDepsGraph(ctx, topicID, deps)
}

// GetDepsGraph 获取话题的依赖关系图 (委托给 Topic 子 Store)。
func (s *RedisStore) GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error) {
	return s.Topic.GetDepsGraph(ctx, topicID)
}

// SaveAgentOutput 保存 Agent 输出 (委托给 Output 子 Store)。
func (s *RedisStore) SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error {
	return s.Output.SaveAgentOutput(ctx, topicID, output)
}

// GetAgentOutputs 获取 Agent 在某话题下的全部输出 (委托给 Output 子 Store)。
func (s *RedisStore) GetAgentOutputs(ctx context.Context, topicID, agentID string) ([]*types.AgentOutput, error) {
	return s.Output.GetAgentOutputs(ctx, topicID, agentID)
}

// GetLatestAgentOutput 获取 Agent 最新版本输出 (委托给 Output 子 Store)。
func (s *RedisStore) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	return s.Output.GetLatestAgentOutput(ctx, topicID, agentID)
}

// PushEvent 推送 Event 到话题的 Redis Stream (委托给 Event 子 Store)。
func (s *RedisStore) PushEvent(ctx context.Context, topicID string, event *types.Event) error {
	return s.Event.PushEvent(ctx, topicID, event)
}

// PollEvents 拉取待处理 Event。
// 实现: 维护 per-topic 的 stream 游标（last message ID），从游标位置 XRead 增量拉取，
// 避免 stream 增长后每次全量读取导致 O(n) 退化（C4 修复）。游标在内存中，
// 进程重启会回退到 "0" 重新读取——可接受，因为 Pending 事件幂等处理。
// 参数:
//   - ctx:    请求上下文。
//   - topicID:话题 ID。
//   - count:  本次最多拉取条数，<=0 时默认 10。
//
// 返回: Pending 事件切片与错误。
func (s *RedisStore) PollEvents(ctx context.Context, topicID string, count int64) ([]*types.Event, error) {
	if count <= 0 {
		count = 10
	}
	// 生成 topic events stream 的统一 key
	key := topicKey(topicID, "events")
	// 加锁读取当前游标；不长时间持有锁，避免阻塞其他 topic 操作
	s.eventCursorsMu.Lock()
	cursor, ok := s.eventCursors[key]
	s.eventCursorsMu.Unlock()
	if !ok {
		// 首次拉取或进程重启，从 stream 起始处读取
		cursor = "0"
	}

	// 委托 Event 子 Store 执行实际 XRead，返回事件与最后一条消息 ID
	events, lastID, err := s.Event.pollEvents(ctx, topicID, count, cursor)
	if err != nil {
		return nil, err
	}
	// 如果有读到消息，原子更新游标，下次从增量位置开始
	if lastID != "" {
		s.eventCursorsMu.Lock()
		s.eventCursors[key] = lastID
		s.eventCursorsMu.Unlock()
	}
	return events, nil
}

// AppendDecision 追加一条决策日志 (委托给 Event 子 Store)。
func (s *RedisStore) AppendDecision(ctx context.Context, topicID, decision string) error {
	return s.Event.AppendDecision(ctx, topicID, decision)
}

// GetDecisions 获取决策日志 (委托给 Event 子 Store)。
func (s *RedisStore) GetDecisions(ctx context.Context, topicID string, count int64) ([]string, error) {
	return s.Event.GetDecisions(ctx, topicID, count)
}

// SaveSnapshot 保存快照到 Redis (委托给 Snapshot 子 Store)。
func (s *RedisStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
	return s.Snapshot.SaveSnapshot(ctx, snapshot, ttl)
}

// GetSnapshot 从 Redis 获取快照 (委托给 Snapshot 子 Store)。
func (s *RedisStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	return s.Snapshot.GetSnapshot(ctx, agentID, topicID)
}

// SaveTopicArchive 保存话题归档摘要 (委托给 Topic 子 Store)。
func (s *RedisStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error {
	return s.Topic.SaveTopicArchive(ctx, topicID, summary, outputs, decisions, embedding)
}

// SetTopicTTL 设置话题所有 Key 的 TTL (int 秒版本,委托给 TTL 子 Store)。
func (s *RedisStore) SetTopicTTL(ctx context.Context, topicID string, ttlSeconds int) error {
	return s.TTL.SetTopicTTL(ctx, topicID, ttlSeconds)
}

// SetTopicTTLDuration 设置话题所有 Key 的 TTL (time.Duration 版本,委托给 TTL 子 Store)。
func (s *RedisStore) SetTopicTTLDuration(ctx context.Context, topicID string, ttl time.Duration) error {
	return s.TTL.SetTopicTTLDuration(ctx, topicID, ttl)
}

// DeleteTopic 删除话题的所有数据 (委托给 TTL 子 Store)。
func (s *RedisStore) DeleteTopic(ctx context.Context, topicID string) error {
	return s.TTL.DeleteTopic(ctx, topicID)
}
