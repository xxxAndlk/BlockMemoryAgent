package store

import (
	"context"       // 上下文,贯穿所有 Redis 调用以支持超时与取消
	"encoding/json" // 结构体与 Redis 字符串值之间的序列化
	"fmt"           // 格式化错误与 key 拼接
	"time"          // TTL 与时间戳解析

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// TopicRedisStore 负责话题维度元数据、约束、依赖图与归档摘要的 Redis 读写。
type TopicRedisStore struct {
	client *redis.Client // 共享 Redis 客户端
}

// SaveTopicMeta 保存话题元数据到 Redis Hash。
// 参数:
//   - ctx:   请求上下文。
//   - topic: 话题元数据指针。
//
// 返回: HSet 错误。
func (s *TopicRedisStore) SaveTopicMeta(ctx context.Context, topic *types.TopicMeta) error {
	// 基础字段写入 map
	data := map[string]string{
		"id":     topic.ID,
		"goal":   topic.Goal,
		"status": string(topic.Status),
	}
	// CreatedAt 为零值时补为当前时间，保证后续解析不失败
	if topic.CreatedAt.IsZero() {
		topic.CreatedAt = time.Now()
	}
	data["created_at"] = topic.CreatedAt.Format(time.RFC3339)
	// ExpiresAt 非空时写入过期时间
	if topic.ExpiresAt != nil {
		data["expires_at"] = topic.ExpiresAt.Format(time.RFC3339)
	}
	return s.client.HSet(ctx, topicKey(topic.ID, "meta"), data).Err()
}

// GetTopicMeta 获取话题元数据。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//
// 返回: 命中返回 *TopicMeta；不存在返回 (nil, nil)；解析错误返回错误。
func (s *TopicRedisStore) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	data, err := s.client.HGetAll(ctx, topicKey(topicID, "meta")).Result()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		// Hash 为空表示该 topic 在 Redis 中不存在
		return nil, nil
	}
	// 填充基础字段
	topic := &types.TopicMeta{
		ID:     data["id"],
		Goal:   data["goal"],
		Status: enums.TopicStatus(data["status"]),
	}
	// 解析创建时间；失败时保留零值
	if t, err := time.Parse(time.RFC3339, data["created_at"]); err == nil {
		topic.CreatedAt = t
	}
	// 解析过期时间；失败时不填充
	if t, err := time.Parse(time.RFC3339, data["expires_at"]); err == nil {
		topic.ExpiresAt = &t
	}
	return topic, nil
}

// SetTopicConstraints 设置话题的全局约束 (Hash)。
// 参数:
//   - ctx:         请求上下文。
//   - topicID:     话题 ID。
//   - constraints: 约束键值对。
//
// 返回: HSet 错误。
func (s *TopicRedisStore) SetTopicConstraints(ctx context.Context, topicID string, constraints map[string]string) error {
	return s.client.HSet(ctx, topicKey(topicID, "constraints"), constraints).Err()
}

// GetTopicConstraints 获取话题的全局约束。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//
// 返回: 约束键值对与 Redis 错误。
func (s *TopicRedisStore) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return s.client.HGetAll(ctx, topicKey(topicID, "constraints")).Result()
}

// SetDepsGraph 设置话题的依赖关系图。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - deps:    依赖图 map[节点][]依赖。
//
// 返回: 序列化或 Set 错误。
func (s *TopicRedisStore) SetDepsGraph(ctx context.Context, topicID string, deps map[string][]string) error {
	data, err := json.Marshal(deps)
	if err != nil {
		return err
	}
	// 0 表示无 TTL，持久化存储
	return s.client.Set(ctx, topicKey(topicID, "deps_graph"), data, 0).Err()
}

// GetDepsGraph 获取话题的依赖关系图。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//
// 返回: 依赖图；不存在返回 (nil, nil)；反序列化失败返回错误。
func (s *TopicRedisStore) GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error) {
	data, err := s.client.Get(ctx, topicKey(topicID, "deps_graph")).Result()
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

// SaveTopicArchive 保存话题归档摘要 (实现 graph.Archiver 接口)。
// 参数:
//   - ctx:      请求上下文。
//   - topicID:  话题 ID。
//   - summary:  归档摘要文本。
//   - outputs:  产出 JSON（当前未使用）。
//   - decisions:决策 JSON（当前未使用）。
//   - embedding:摘要向量（当前未使用）。
//
// 返回: Redis Set 错误。
func (s *TopicRedisStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error {
	// 当前 Redis 归档仅保存摘要文本，pgvector 等持久化由 PostgreSQL 侧处理
	return s.client.Set(ctx, topicKey(topicID, "archived"), summary, 0).Err()
}

// 以下编译期断言确保 TopicRedisStore 在作为 graph.Archiver 消费时接口兼容。
var _ interface {
	SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error
} = (*TopicRedisStore)(nil)

// topicKey 生成 topic 前缀 key,统一命名空间。
// 参数:
//   - topicID: 话题 ID。
//   - suffix:  key 后缀，如 "meta"/"events"。
//
// 返回: 形如 "topic:{topicID}:{suffix}" 的 Redis key。
func topicKey(topicID, suffix string) string {
	return fmt.Sprintf("topic:%s:%s", topicID, suffix)
}
