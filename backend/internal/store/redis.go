package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/redis/go-redis/v9"
)

// RedisStore Redis 存储层
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore 创建 Redis 存储
func NewRedisStore(addr, password string, db int) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisStore{client: client}, nil
}

// Close 关闭连接
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// Ping 检查 Redis 连接
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// topicKey 生成 topic 前缀 key
func (s *RedisStore) topicKey(topicID, suffix string) string {
	return fmt.Sprintf("topic:%s:%s", topicID, suffix)
}

// SaveTopicMeta 保存话题元数据
func (s *RedisStore) SaveTopicMeta(ctx context.Context, topic *types.TopicMeta) error {
	data := map[string]string{
		"id":     topic.ID,
		"goal":   topic.Goal,
		"status": topic.Status,
	}
	if topic.CreatedAt.IsZero() {
		topic.CreatedAt = time.Now()
	}
	data["created_at"] = topic.CreatedAt.Format(time.RFC3339)
	if topic.ExpiresAt != nil {
		data["expires_at"] = topic.ExpiresAt.Format(time.RFC3339)
	}
	return s.client.HSet(ctx, s.topicKey(topic.ID, "meta"), data).Err()
}

// GetTopicMeta 获取话题元数据
func (s *RedisStore) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	data, err := s.client.HGetAll(ctx, s.topicKey(topicID, "meta")).Result()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	topic := &types.TopicMeta{
		ID:     data["id"],
		Goal:   data["goal"],
		Status: data["status"],
	}
	if t, err := time.Parse(time.RFC3339, data["created_at"]); err == nil {
		topic.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, data["expires_at"]); err == nil {
		topic.ExpiresAt = &t
	}
	return topic, nil
}

// SetTopicConstraints 设置全局约束
func (s *RedisStore) SetTopicConstraints(ctx context.Context, topicID string, constraints map[string]string) error {
	return s.client.HSet(ctx, s.topicKey(topicID, "constraints"), constraints).Err()
}

// GetTopicConstraints 获取全局约束
func (s *RedisStore) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return s.client.HGetAll(ctx, s.topicKey(topicID, "constraints")).Result()
}

// SaveAgentOutput 保存 Agent 输出到 Sorted Set
func (s *RedisStore) SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error {
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", output.AgentID))
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return s.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(output.Version),
		Member: string(data),
	}).Err()
}

// GetAgentOutputs 获取 Agent 所有输出
func (s *RedisStore) GetAgentOutputs(ctx context.Context, topicID, agentID string) ([]*types.AgentOutput, error) {
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	results, err := s.client.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var outputs []*types.AgentOutput
	for _, r := range results {
		var o types.AgentOutput
		if err := json.Unmarshal([]byte(r), &o); err != nil {
			continue
		}
		outputs = append(outputs, &o)
	}
	return outputs, nil
}

// GetLatestAgentOutput 获取 Agent 最新输出
func (s *RedisStore) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	key := s.topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	results, err := s.client.ZRevRange(ctx, key, 0, 0).Result()
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	var o types.AgentOutput
	if err := json.Unmarshal([]byte(results[0]), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// PushEvent 推送 Event 到 Stream
func (s *RedisStore) PushEvent(ctx context.Context, topicID string, event *types.Event) error {
	key := s.topicKey(topicID, "events")
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

// PollEvents 拉取待处理 Event
func (s *RedisStore) PollEvents(ctx context.Context, topicID string, count int64) ([]*types.Event, error) {
	if count <= 0 {
		count = 10
	}
	key := s.topicKey(topicID, "events")
	// 读取所有未确认消息 (简单实现: 读取最近 count 条)
	streams, err := s.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{key, "0"},
		Count:   count,
	}).Result()
	if err != nil {
		return nil, err
	}

	var events []*types.Event
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			dataStr, ok := msg.Values["data"].(string)
			if !ok {
				continue
			}
			var ev types.Event
			if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
				continue
			}
			if ev.Status == types.EventPending {
				events = append(events, &ev)
			}
		}
	}
	return events, nil
}

// AppendDecision 追加决策日志
func (s *RedisStore) AppendDecision(ctx context.Context, topicID, decision string) error {
	key := s.topicKey(topicID, "decisions")
	return s.client.LPush(ctx, key, decision).Err()
}

// GetDecisions 获取决策日志
func (s *RedisStore) GetDecisions(ctx context.Context, topicID string, count int64) ([]string, error) {
	if count <= 0 {
		count = 50
	}
	key := s.topicKey(topicID, "decisions")
	return s.client.LRange(ctx, key, 0, count-1).Result()
}

// SetDepsGraph 设置依赖关系图
func (s *RedisStore) SetDepsGraph(ctx context.Context, topicID string, deps map[string][]string) error {
	data, err := json.Marshal(deps)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.topicKey(topicID, "deps_graph"), data, 0).Err()
}

// GetDepsGraph 获取依赖关系图
func (s *RedisStore) GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error) {
	data, err := s.client.Get(ctx, s.topicKey(topicID, "deps_graph")).Result()
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

// SaveSnapshot 保存快照到 Redis
func (s *RedisStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
	key := fmt.Sprintf("snapshot:%s:%s", snapshot.AgentID, snapshot.TopicID)
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return s.client.Set(ctx, key, data, ttl).Err()
}

// GetSnapshot 从 Redis 获取快照
func (s *RedisStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	key := fmt.Sprintf("snapshot:%s:%s", agentID, topicID)
	data, err := s.client.Get(ctx, key).Result()
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

// SaveTopicArchive 保存话题归档 (实现 graph.Archiver 接口)
func (s *RedisStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error {
	return s.client.Set(ctx, s.topicKey(topicID, "archived"), summary, 0).Err()
}

// SetTopicTTL 设置话题所有 Key 的 TTL (int 秒版本，实现 graph.Archiver 接口)
func (s *RedisStore) SetTopicTTL(ctx context.Context, topicID string, ttlSeconds int) error {
	return s.SetTopicTTLDuration(ctx, topicID, time.Duration(ttlSeconds)*time.Second)
}

// SetTopicTTLDuration 设置话题所有 Key 的 TTL (time.Duration 版本)
func (s *RedisStore) SetTopicTTLDuration(ctx context.Context, topicID string, ttl time.Duration) error {
	pattern := s.topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		s.client.Expire(ctx, iter.Val(), ttl)
	}
	return iter.Err()
}

// DeleteTopic 删除话题所有数据
func (s *RedisStore) DeleteTopic(ctx context.Context, topicID string) error {
	pattern := s.topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if len(keys) > 0 {
		return s.client.Del(ctx, keys...).Err()
	}
	return iter.Err()
}
