package store

import (
	"context"       // 上下文，控制 Redis 调用生命周期
	"encoding/json" // 结构体序列化
	"fmt"           // key 拼接

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// OutputRedisStore 负责 Agent 输出 (Sorted Set) 的 Redis 读写。
type OutputRedisStore struct {
	client *redis.Client // 共享 Redis 客户端
}

// SaveAgentOutput 保存 Agent 输出到 Sorted Set (按版本号排序)。
// 参数:
//   - ctx:    请求上下文。
//   - topicID:话题 ID。
//   - output: Agent 输出指针，含版本号。
//
// 返回: 序列化或 ZAdd 错误。
func (s *OutputRedisStore) SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error {
	// 每个 agent 在一个 topic 下有自己的 outputs sorted set
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", output.AgentID))
	// 序列化输出为 JSON 字符串作为 member
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}
	// 使用版本号作为 score，保证按版本有序
	return s.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(output.Version),
		Member: string(data),
	}).Err()
}

// GetAgentOutputs 获取 Agent 在某话题下的全部输出 (按版本升序)。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - agentID: Agent ID。
//
// 返回: Agent 输出切片与 Redis 错误。
func (s *OutputRedisStore) GetAgentOutputs(ctx context.Context, topicID, agentID string) ([]*types.AgentOutput, error) {
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	// ZRange 0 到 -1 表示全部成员，按 score（版本）升序
	results, err := s.client.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var outputs []*types.AgentOutput
	for _, r := range results {
		var o types.AgentOutput
		// 单条反序列化失败跳过，避免脏数据影响整体读取
		if err := json.Unmarshal([]byte(r), &o); err != nil {
			continue
		}
		outputs = append(outputs, &o)
	}
	return outputs, nil
}

// GetLatestAgentOutput 获取 Agent 最新版本输出。
// 参数:
//   - ctx:     请求上下文。
//   - topicID: 话题 ID。
//   - agentID: Agent ID。
//
// 返回: 最新版本输出指针；无记录返回 (nil, nil)；反序列化失败返回错误。
func (s *OutputRedisStore) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
	// ZRevRange 0 到 0 取 score 最大的一个成员
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
