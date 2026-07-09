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
	client *redis.Client
}

// SaveTopicMeta 保存话题元数据到 Redis Hash。
func (s *TopicRedisStore) SaveTopicMeta(ctx context.Context, topic *types.TopicMeta) error {
	data := map[string]string{
		"id":     topic.ID,
		"goal":   topic.Goal,
		"status": string(topic.Status),
	}
	if topic.CreatedAt.IsZero() {
		topic.CreatedAt = time.Now()
	}
	data["created_at"] = topic.CreatedAt.Format(time.RFC3339)
	if topic.ExpiresAt != nil {
		data["expires_at"] = topic.ExpiresAt.Format(time.RFC3339)
	}
	return s.client.HSet(ctx, topicKey(topic.ID, "meta"), data).Err()
}

// GetTopicMeta 获取话题元数据。
func (s *TopicRedisStore) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	data, err := s.client.HGetAll(ctx, topicKey(topicID, "meta")).Result()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	topic := &types.TopicMeta{
		ID:     data["id"],
		Goal:   data["goal"],
		Status: enums.TopicStatus(data["status"]),
	}
	if t, err := time.Parse(time.RFC3339, data["created_at"]); err == nil {
		topic.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, data["expires_at"]); err == nil {
		topic.ExpiresAt = &t
	}
	return topic, nil
}

// SetTopicConstraints 设置话题的全局约束 (Hash)。
func (s *TopicRedisStore) SetTopicConstraints(ctx context.Context, topicID string, constraints map[string]string) error {
	return s.client.HSet(ctx, topicKey(topicID, "constraints"), constraints).Err()
}

// GetTopicConstraints 获取话题的全局约束。
func (s *TopicRedisStore) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return s.client.HGetAll(ctx, topicKey(topicID, "constraints")).Result()
}

// SetDepsGraph 设置话题的依赖关系图。
func (s *TopicRedisStore) SetDepsGraph(ctx context.Context, topicID string, deps map[string][]string) error {
	data, err := json.Marshal(deps)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, topicKey(topicID, "deps_graph"), data, 0).Err()
}

// GetDepsGraph 获取话题的依赖关系图。
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
func (s *TopicRedisStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error {
	return s.client.Set(ctx, topicKey(topicID, "archived"), summary, 0).Err()
}

// ensure TopicRedisStore implements graph.Archiver if consumed as such.
var _ interface {
	SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error
} = (*TopicRedisStore)(nil)

// topicKey 生成 topic 前缀 key,统一命名空间。
func topicKey(topicID, suffix string) string {
	return fmt.Sprintf("topic:%s:%s", topicID, suffix)
}
