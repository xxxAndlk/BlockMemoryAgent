package store

import (
	"context"       // 上下文，控制 Redis 调用生命周期
	"encoding/json" // 结构体序列化
	"fmt"           // key 拼接
	"time"          // TTL 控制

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// SnapshotRedisStore 负责 Agent 快照的 Redis 热缓存读写。
type SnapshotRedisStore struct {
	client *redis.Client // 共享 Redis 客户端
}

// SaveSnapshot 保存快照到 Redis (热加载缓存)。
// 参数:
//   - ctx:      请求上下文。
//   - snapshot: Agent 快照指针。
//   - ttl:      缓存过期时间，<=0 时默认 7 天。
//
// 返回: 序列化或 Set 错误。
func (s *SnapshotRedisStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
	// key 格式固定为 snapshot:{agent_id}:{topic_id}
	key := fmt.Sprintf("snapshot:%s:%s", snapshot.AgentID, snapshot.TopicID)
	// 序列化快照为 JSON 字符串
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if ttl <= 0 {
		// 默认缓存 7 天，避免冷数据长期占用 Redis 内存
		ttl = 7 * 24 * time.Hour
	}
	// 带 TTL 写入 Redis String
	return s.client.Set(ctx, key, data, ttl).Err()
}

// GetSnapshot 从 Redis 获取快照。
// 参数:
//   - ctx:     请求上下文。
//   - agentID: Agent ID。
//   - topicID: 话题 ID。
//
// 返回: 命中返回快照指针；不存在返回 (nil, nil)；反序列化失败返回错误。
func (s *SnapshotRedisStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	key := fmt.Sprintf("snapshot:%s:%s", agentID, topicID)
	data, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		// 缓存未命中是正常情况，返回 nil 让上层回源 PostgreSQL
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
