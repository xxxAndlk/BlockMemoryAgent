package store

import (
	"context"       // 上下文
	"encoding/json" // 结构体序列化
	"fmt"           // key 拼接
	"time"          // TTL 控制

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// SnapshotRedisStore 负责 Agent 快照的 Redis 热缓存读写。
type SnapshotRedisStore struct {
	client *redis.Client
}

// SaveSnapshot 保存快照到 Redis (热加载缓存)。
func (s *SnapshotRedisStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
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

// GetSnapshot 从 Redis 获取快照。
func (s *SnapshotRedisStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
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
