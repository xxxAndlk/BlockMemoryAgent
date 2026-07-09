package store

import (
	"context" // 上下文
	"time"    // TTL 控制

	"github.com/redis/go-redis/v9" // Redis 客户端
)

// TTLRedisStore 负责话题 TTL 设置与整话题删除。
type TTLRedisStore struct {
	client *redis.Client
}

// SetTopicTTL 设置话题所有 Key 的 TTL (int 秒版本,实现 graph.Archiver 接口)。
func (s *TTLRedisStore) SetTopicTTL(ctx context.Context, topicID string, ttlSeconds int) error {
	return s.SetTopicTTLDuration(ctx, topicID, time.Duration(ttlSeconds)*time.Second)
}

// SetTopicTTLDuration 设置话题所有 Key 的 TTL (time.Duration 版本)。
func (s *TTLRedisStore) SetTopicTTLDuration(ctx context.Context, topicID string, ttl time.Duration) error {
	pattern := topicKey(topicID, "*")
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		s.client.Expire(ctx, iter.Val(), ttl)
	}
	return iter.Err()
}

// DeleteTopic 删除话题的所有数据。
func (s *TTLRedisStore) DeleteTopic(ctx context.Context, topicID string) error {
	pattern := topicKey(topicID, "*")
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
