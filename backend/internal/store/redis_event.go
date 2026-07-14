package store

import (
	"context"       // 上下文，控制 Redis 调用生命周期
	"encoding/json" // 结构体序列化

	"github.com/blockmemory/agent/backend/pkg/enums" // 事件状态枚举
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// EventRedisStore 负责话题事件流 (Stream) 与决策日志 (List) 的 Redis 读写。
type EventRedisStore struct {
	client *redis.Client // 共享 Redis 客户端
}

// PushEvent 推送 Event 到话题的 Redis Stream。
// 参数:
//   - ctx:    请求上下文。
//   - topicID:话题 ID。
//   - event:  待推送事件指针。
//
// 返回: 序列化或 XAdd 错误。
func (s *EventRedisStore) PushEvent(ctx context.Context, topicID string, event *types.Event) error {
	// 生成 topic events stream 的统一 key
	key := topicKey(topicID, "events")
	// 将事件序列化为 JSON 字符串
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	// 使用 XAdd 追加到 stream；map 字段名固定为 data，便于消费者统一解析
	return s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		Values: map[string]any{
			"data": string(data),
		},
	}).Err()
}

// pollEvents 是 PollEvents 的核心实现,从指定游标位置读取增量事件。
// 参数:
//   - ctx:    请求上下文。
//   - topicID:话题 ID。
//   - count:  本次最多拉取条数。
//   - cursor: XRead 起始游标，"0" 表示从头读取。
//
// 返回: 解析出的 Pending 事件列表与最后一条消息 ID(用于推进游标)。
func (s *EventRedisStore) pollEvents(ctx context.Context, topicID string, count int64, cursor string) ([]*types.Event, string, error) {
	key := topicKey(topicID, "events")
	// XRead 阻塞等待 stream 增量消息；这里用非阻塞版本，由调用方控制重试节奏
	streams, err := s.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{key, cursor},
		Count:   count,
	}).Result()
	// redis.Nil 表示当前游标后无新消息，返回空结果
	if err != nil && err != redis.Nil {
		return nil, "", err
	}

	var events []*types.Event
	var lastID string
	// 遍历所有 stream（实际只有一个）
	for _, stream := range streams {
		// 遍历该 stream 返回的消息
		for _, msg := range stream.Messages {
			lastID = msg.ID
			// 取出 data 字段
			dataStr, ok := msg.Values["data"].(string)
			if !ok {
				continue
			}
			var ev types.Event
			// 反序列化失败则跳过该消息，避免单条脏数据阻塞消费
			if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
				continue
			}
			// 仅保留 Pending 状态的事件，已完成事件不返回
			if ev.Status == enums.EventPending {
				events = append(events, &ev)
			}
		}
	}
	return events, lastID, nil
}

// AppendDecision 追加一条决策日志到 List (LPush)。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - decision:决策文本。
//
// 返回: Redis 错误。
func (s *EventRedisStore) AppendDecision(ctx context.Context, topicID, decision string) error {
	key := topicKey(topicID, "decisions")
	// 使用 LPush 保证最新决策在列表头部，便于快速读取最近决策
	return s.client.LPush(ctx, key, decision).Err()
}

// GetDecisions 获取决策日志。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - count:   获取条数，<=0 时默认 50。
//
// 返回: 决策文本切片与 Redis 错误。
func (s *EventRedisStore) GetDecisions(ctx context.Context, topicID string, count int64) ([]string, error) {
	if count <= 0 {
		count = 50
	}
	key := topicKey(topicID, "decisions")
	// LRange 从头部开始取 count 条；索引 0 到 count-1
	return s.client.LRange(ctx, key, 0, count-1).Result()
}
