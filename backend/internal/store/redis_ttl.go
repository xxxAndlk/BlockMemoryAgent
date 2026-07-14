package store

import (
	"context" // 上下文，控制 Redis 调用生命周期
	"time"    // TTL 控制

	"github.com/redis/go-redis/v9" // Redis 客户端
)

// TTLRedisStore 负责话题 TTL 设置与整话题删除。
type TTLRedisStore struct {
	client *redis.Client // 共享 Redis 客户端
}

// SetTopicTTL 设置话题所有 Key 的 TTL (int 秒版本,实现 graph.Archiver 接口)。
// 参数:
//   - ctx:        请求上下文。
//   - topicID:    话题 ID。
//   - ttlSeconds: TTL 秒数。
//
// 返回: 设置过程中的迭代错误。
func (s *TTLRedisStore) SetTopicTTL(ctx context.Context, topicID string, ttlSeconds int) error {
	return s.SetTopicTTLDuration(ctx, topicID, time.Duration(ttlSeconds)*time.Second)
}

// SetTopicTTLDuration 设置话题所有 Key 的 TTL (time.Duration 版本)。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - ttl:     过期时间。
//
// 返回: 扫描或设置过程中的错误。
func (s *TTLRedisStore) SetTopicTTLDuration(ctx context.Context, topicID string, ttl time.Duration) error {
	// 扫描所有 topic:{topicID}:* 的 key
	pattern := topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		// 为每个匹配 key 设置过期时间；单个错误不影响其他 key
		s.client.Expire(ctx, iter.Val(), ttl)
	}
	return iter.Err()
}

// DeleteTopic 删除话题的所有数据。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//
// 返回: 删除或扫描错误。
func (s *TTLRedisStore) DeleteTopic(ctx context.Context, topicID string) error {
	pattern := topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	// 收集到批量 key 后再一次性删除，减少网络往返
	if len(keys) > 0 {
		return s.client.Del(ctx, keys...).Err()
	}
	return iter.Err()
}
