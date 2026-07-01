package store

import (
	"context"      // 上下文,贯穿所有 Redis 调用以支持超时与取消
	"encoding/json" // 结构体与 Redis 字符串值之间的序列化
	"fmt"           // 格式化错误与 key 拼接
	"sync"          // 保护 eventCursors map 的并发访问
	"time"          // TTL 与时间戳解析

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型 (TopicMeta / AgentOutput / Event / AgentSnapshot 等)
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// RedisStore 是 Redis 存储层。
// 职责: workspace 热数据 (Hash/SortedSet/Stream/List/String) 的读写,
// 以及 Agent 快照的热加载 (带 TTL)。
// 并发安全: go-redis 客户端内部维护连接池,可在多 goroutine 间共享。
type RedisStore struct {
	client *redis.Client // 共享 Redis 客户端,所有方法通过该句柄执行命令

	// eventCursors 维护每个 topic events stream 的已读游标（last message ID）。
	// C4 修复：原 PollEvents 用 XRead "0" 起始，每次返回全量消息，
	// 随 stream 增长从 O(1) 退化为 O(n)。维护游标后只读增量。
	eventCursorsMu sync.Mutex
	eventCursors   map[string]string
}

// NewRedisStore 创建 Redis 存储实例。
// 参数:
//   - addr:     Redis 地址 (host:port)
//   - password: 认证密码,空表示无密码
//   - db:       Redis 数据库编号
// 返回:
//   - *RedisStore: 已通过 Ping 校验的存储实例
//   - error: Ping 失败时返回包装错误
func NewRedisStore(addr, password string, db int) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	// 主动 Ping 一次,提前暴露网络/凭证类错误
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisStore{client: client, eventCursors: make(map[string]string)}, nil
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
// 返回: Ping 错误。
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// topicKey 生成 topic 前缀 key,统一命名空间。
// 参数:
//   - topicID: 话题 ID
//   - suffix:  子类型 (meta/constraints/events/outputs:xxx 等)
// 返回: 形如 "topic:{topicID}:{suffix}" 的 key。
func (s *RedisStore) topicKey(topicID, suffix string) string {
	return fmt.Sprintf("topic:%s:%s", topicID, suffix)
}

// SaveTopicMeta 保存话题元数据到 Redis Hash。
// 参数:
//   - topic: 话题元数据
// 返回: HSet 错误。
// 副作用: CreatedAt 为零值时补为当前时间;ExpiresAt 非空时一并写入。
func (s *RedisStore) SaveTopicMeta(ctx context.Context, topic *types.TopicMeta) error {
	// 以 map 形式一次性写入多个 Hash 字段
	data := map[string]string{
		"id":     topic.ID,
		"goal":   topic.Goal,
		"status": string(topic.Status),
	}
	if topic.CreatedAt.IsZero() {
		// 零值时兜底为当前时间,避免写入空字符串
		topic.CreatedAt = time.Now()
	}
	data["created_at"] = topic.CreatedAt.Format(time.RFC3339)
	if topic.ExpiresAt != nil {
		// 过期时间可选,非空才写入
		data["expires_at"] = topic.ExpiresAt.Format(time.RFC3339)
	}
	return s.client.HSet(ctx, s.topicKey(topic.ID, "meta"), data).Err()
}

// GetTopicMeta 获取话题元数据。
// 参数:
//   - topicID: 话题 ID
// 返回: 命中返回 *TopicMeta;Hash 为空 (话题不存在) 返回 (nil, nil)。
// 注意: 时间字段按 RFC3339 解析,解析失败则对应字段保持零值。
func (s *RedisStore) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	data, err := s.client.HGetAll(ctx, s.topicKey(topicID, "meta")).Result()
	if err != nil {
		return nil, err
	}
	// 空 map 视为话题不存在
	if len(data) == 0 {
		return nil, nil
	}
	topic := &types.TopicMeta{
		ID:     data["id"],
		Goal:   data["goal"],
		Status: enums.TopicStatus(data["status"]),
	}
	// 解析创建时间,失败则保持零值
	if t, err := time.Parse(time.RFC3339, data["created_at"]); err == nil {
		topic.CreatedAt = t
	}
	// 解析过期时间,失败则保持 nil
	if t, err := time.Parse(time.RFC3339, data["expires_at"]); err == nil {
		topic.ExpiresAt = &t
	}
	return topic, nil
}

// SetTopicConstraints 设置话题的全局约束 (Hash)。
// 参数:
//   - topicID:     话题 ID
//   - constraints: 约束键值对
// 返回: HSet 错误。
func (s *RedisStore) SetTopicConstraints(ctx context.Context, topicID string, constraints map[string]string) error {
	return s.client.HSet(ctx, s.topicKey(topicID, "constraints"), constraints).Err()
}

// GetTopicConstraints 获取话题的全局约束。
// 参数:
//   - topicID: 话题 ID
// 返回: 约束键值对 map;话题无约束时返回空 map。
func (s *RedisStore) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return s.client.HGetAll(ctx, s.topicKey(topicID, "constraints")).Result()
}

// SaveAgentOutput 保存 Agent 输出到 Sorted Set (按版本号排序)。
// 参数:
//   - topicID: 话题 ID
//   - output:  Agent 输出 (含 AgentID 与 Version)
// 返回: 序列化或 ZAdd 错误。
// 设计意图: 同一 Agent 的多版本输出按 Version 分数排序,支持历史回溯。
func (s *RedisStore) SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error {
	// key 形如 topic:{id}:outputs:{agentID}
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", output.AgentID))
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}
	// 以 Version 作为 score,JSON 字符串作为 member
	return s.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(output.Version),
		Member: string(data),
	}).Err()
}

// GetAgentOutputs 获取 Agent 在某话题下的全部输出 (按版本升序)。
// 参数:
//   - topicID, agentID: 检索范围
// 返回: AgentOutput 切片;反序列化失败的成员被跳过。
func (s *RedisStore) GetAgentOutputs(ctx context.Context, topicID, agentID string) ([]*types.AgentOutput, error) {
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	// ZRange 0 -1 取全部成员 (升序)
	results, err := s.client.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var outputs []*types.AgentOutput
	for _, r := range results {
		var o types.AgentOutput
		// 反序列化失败跳过当前成员
		if err := json.Unmarshal([]byte(r), &o); err != nil {
			continue
		}
		outputs = append(outputs, &o)
	}
	return outputs, nil
}

// GetLatestAgentOutput 获取 Agent 最新版本输出。
// 参数:
//   - topicID, agentID: 检索范围
// 返回: 最新 AgentOutput;无数据时返回 (nil, nil)。
// 设计意图: 取 Sorted Set 中 score 最大的成员 (ZRevRange 0 0)。
func (s *RedisStore) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	// ZRevRange 0 0 取降序首条,即最高版本
	results, err := s.client.ZRevRange(ctx, key, 0, 0).Result()
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		// 无数据返回 nil
		return nil, nil
	}
	var o types.AgentOutput
	if err := json.Unmarshal([]byte(results[0]), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// PushEvent 推送 Event 到话题的 Redis Stream。
// 参数:
//   - topicID: 话题 ID
//   - event:   待推送事件
// 返回: 序列化或 XAdd 错误。
// 设计意图: Stream 提供有序、可消费的事件流,供跨 Agent 通信使用。
func (s *RedisStore) PushEvent(ctx context.Context, topicID string, event *types.Event) error {
	key := s.topicKey(topicID, "events")
	// 事件整体序列化为 JSON 后作为 Stream 字段值
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		Values: map[string]any{
			"data": string(data),
		},
	}).Err()
}

// PollEvents 拉取待处理 Event。
// 参数:
//   - topicID: 话题 ID
//   - count:   最多拉取条数,<=0 时默认 10
// 返回: 状态为 Pending 的事件切片。
// 实现: 维护 per-topic 的 stream 游标（last message ID），从游标位置 XRead 增量拉取，
// 避免 stream 增长后每次全量读取导致 O(n) 退化（C4 修复）。游标在内存中，
// 进程重启会回退到 "0" 重新读取——可接受，因为 Pending 事件幂等处理。
func (s *RedisStore) PollEvents(ctx context.Context, topicID string, count int64) ([]*types.Event, error) {
	if count <= 0 {
		// 兜底默认值
		count = 10
	}
	key := s.topicKey(topicID, "events")
	// 取当前游标；无游标从 "0" 开始（首次拉取）
	s.eventCursorsMu.Lock()
	cursor, ok := s.eventCursors[key]
	s.eventCursorsMu.Unlock()
	if !ok {
		cursor = "0"
	}
	// XRead 从游标位置读取增量消息
	streams, err := s.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{key, cursor},
		Count:   count,
	}).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}

	var events []*types.Event
	var lastID string
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			lastID = msg.ID // 记录最后一条消息 ID 作为下次游标
			// 提取 data 字段,类型不匹配则跳过
			dataStr, ok := msg.Values["data"].(string)
			if !ok {
				continue
			}
			var ev types.Event
			// 反序列化失败跳过
			if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
				continue
			}
			// 仅返回 Pending 状态的事件
			if ev.Status == types.EventPending {
				events = append(events, &ev)
			}
		}
	}
	// 推进游标；"$" 表示已读到末尾，下次仍从末尾继续
	if lastID != "" {
		s.eventCursorsMu.Lock()
		s.eventCursors[key] = lastID
		s.eventCursorsMu.Unlock()
	}
	return events, nil
}

// AppendDecision 追加一条决策日志到 List (LPush)。
// 参数:
//   - topicID:  话题 ID
//   - decision: 决策文本
// 返回: LPush 错误。
// 副作用: 新决策插入到列表头部,最新决策排在最前。
func (s *RedisStore) AppendDecision(ctx context.Context, topicID, decision string) error {
	key := s.topicKey(topicID, "decisions")
	return s.client.LPush(ctx, key, decision).Err()
}

// GetDecisions 获取决策日志。
// 参数:
//   - topicID: 话题 ID
//   - count:   最多返回条数,<=0 时默认 50
// 返回: 决策文本切片 (按 LPush 顺序,最新在前)。
func (s *RedisStore) GetDecisions(ctx context.Context, topicID string, count int64) ([]string, error) {
	if count <= 0 {
		// 兜底默认值
		count = 50
	}
	key := s.topicKey(topicID, "decisions")
	// LRange 0 count-1 取前 count 条
	return s.client.LRange(ctx, key, 0, count-1).Result()
}

// SetDepsGraph 设置话题的依赖关系图。
// 参数:
//   - topicID: 话题 ID
//   - deps:    依赖关系 map (agentID -> 依赖的 agentID 列表)
// 返回: 序列化或 Set 错误。
// 副作用: 以 String 类型存储,TTL 为 0 (永久)。
func (s *RedisStore) SetDepsGraph(ctx context.Context, topicID string, deps map[string][]string) error {
	// 整体序列化为 JSON 字符串
	data, err := json.Marshal(deps)
	if err != nil {
		return err
	}
	// TTL 0 表示不过期
	return s.client.Set(ctx, s.topicKey(topicID, "deps_graph"), data, 0).Err()
}

// GetDepsGraph 获取话题的依赖关系图。
// 参数:
//   - topicID: 话题 ID
// 返回: 依赖关系 map;key 不存在返回 (nil, nil)。
func (s *RedisStore) GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error) {
	data, err := s.client.Get(ctx, s.topicKey(topicID, "deps_graph")).Result()
	// redis.Nil 表示 key 不存在,视为正常情况
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var deps map[string][]string
	if err := json.Unmarshal([]byte(data), &deps); err != nil {
		return nil, err
	}
	return deps, nil
}

// SaveSnapshot 保存快照到 Redis (热加载缓存)。
// 参数:
//   - snapshot: 含 AgentID/TopicID 与完整上下文状态
//   - ttl:      过期时间;<=0 时默认 7 天
// 返回: 序列化或 Set 错误。
// 设计意图: 作为 PG 快照的前置缓存,减少回源延迟;TTL 控制缓存时效。
func (s *RedisStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
	// key 形如 snapshot:{agentID}:{topicID}
	key := fmt.Sprintf("snapshot:%s:%s", snapshot.AgentID, snapshot.TopicID)
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if ttl <= 0 {
		// 兜底默认 7 天,与 PG 快照保留期对齐
		ttl = 7 * 24 * time.Hour
	}
	return s.client.Set(ctx, key, data, ttl).Err()
}

// GetSnapshot 从 Redis 获取快照。
// 参数:
//   - agentID, topicID: 快照定位
// 返回: 命中返回 *AgentSnapshot;key 不存在返回 (nil, nil)。
// 设计意图: 热加载路径优先查 Redis,未命中再回源 PG。
func (s *RedisStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	key := fmt.Sprintf("snapshot:%s:%s", agentID, topicID)
	data, err := s.client.Get(ctx, key).Result()
	// key 不存在视为正常情况
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap types.AgentSnapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// SaveTopicArchive 保存话题归档摘要 (实现 graph.Archiver 接口)。
// 参数:
//   - topicID:   话题 ID
//   - summary:   归档摘要文本
//   - outputs:   产出 JSON 字节 (本实现未使用)
//   - decisions: 决策 JSON 字节 (本实现未使用)
//   - embedding: 摘要向量 (本实现未使用)
// 返回: Set 错误。
// 注意: Redis 侧仅缓存 summary 文本,完整归档由 PostgresStore.SaveTopicArchive 持久化。
func (s *RedisStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error {
	// 仅以 String 形式缓存 summary,TTL 0 表示永久
	return s.client.Set(ctx, s.topicKey(topicID, "archived"), summary, 0).Err()
}

// SetTopicTTL 设置话题所有 Key 的 TTL (int 秒版本,实现 graph.Archiver 接口)。
// 参数:
//   - topicID:    话题 ID
//   - ttlSeconds: TTL 秒数
// 返回: 底层 SetTopicTTLDuration 错误。
func (s *RedisStore) SetTopicTTL(ctx context.Context, topicID string, ttlSeconds int) error {
	return s.SetTopicTTLDuration(ctx, topicID, time.Duration(ttlSeconds)*time.Second)
}

// SetTopicTTLDuration 设置话题所有 Key 的 TTL (time.Duration 版本)。
// 参数:
//   - topicID: 话题 ID
//   - ttl:     过期时长
// 返回: Scan 迭代错误。
// 副作用: 通过 SCAN 遍历 topic:{id}:* 模式的 key,逐个设置 Expire。
// 注意: 不设置 ttl<=0 的兜底,由调用方保证。
func (s *RedisStore) SetTopicTTLDuration(ctx context.Context, topicID string, ttl time.Duration) error {
	// 匹配该话题下所有 key
	pattern := s.topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		// 逐个 key 设置过期时间
		s.client.Expire(ctx, iter.Val(), ttl)
	}
	return iter.Err()
}

// DeleteTopic 删除话题的所有数据。
// 参数:
//   - topicID: 话题 ID
// 返回: Del 错误或 Scan 迭代错误。
// 副作用: SCAN 收集所有 topic:{id}:* 的 key 后批量 DEL。
func (s *RedisStore) DeleteTopic(ctx context.Context, topicID string) error {
	// 匹配该话题下所有 key
	pattern := s.topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	var keys []string
	for iter.Next(ctx) {
		// 累积所有匹配的 key
		keys = append(keys, iter.Val())
	}
	if len(keys) > 0 {
		// 批量删除,减少往返
		return s.client.Del(ctx, keys...).Err()
	}
	return iter.Err()
}
