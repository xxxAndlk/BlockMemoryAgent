package store

import (
	"context"       // 上下文
	"encoding/json" // 结构体序列化

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// EventRedisStore 负责话题事件流 (Stream) 与决策日志 (List) 的 Redis 读写。
type EventRedisStore struct {
	client *redis.Client
}

// PushEvent 推送 Event 到话题的 Redis Stream。
func (s *EventRedisStore) PushEvent(ctx context.Context, topicID string, event *types.Event) error {
	key := topicKey(topicID, "events")
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

// pollEvents 是 PollEvents 的核心实现,从指定游标位置读取增量事件。
// 返回解析出的 Pending 事件列表与最后一条消息 ID(用于推进游标)。
func (s *EventRedisStore) pollEvents(ctx context.Context, topicID string, count int64, cursor string) ([]*types.Event, string, error) {
	key := topicKey(topicID, "events")
	streams, err := s.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{key, cursor},
		Count:   count,
	}).Result()
	if err != nil && err != redis.Nil {
		return nil, "", err
	}

	var events []*types.Event
	var lastID string
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			lastID = msg.ID
			dataStr, ok := msg.Values["data"].(string)
			if !ok {
				continue
			}
			var ev types.Event
			if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
				continue
			}
			if ev.Status == enums.EventPending {
				events = append(events, &ev)
			}
		}
	}
	return events, lastID, nil
}

// AppendDecision 追加一条决策日志到 List (LPush)。
func (s *EventRedisStore) AppendDecision(ctx context.Context, topicID, decision string) error {
	key := topicKey(topicID, "decisions")
	return s.client.LPush(ctx, key, decision).Err()
}

// GetDecisions 获取决策日志。
func (s *EventRedisStore) GetDecisions(ctx context.Context, topicID string, count int64) ([]string, error) {
	if count <= 0 {
		count = 50
	}
	key := topicKey(topicID, "decisions")
	return s.client.LRange(ctx, key, 0, count-1).Result()
}
