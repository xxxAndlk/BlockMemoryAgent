package store

import (
	"context"       // 上下文
	"encoding/json" // 结构体序列化
	"fmt"           // key 拼接

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/redis/go-redis/v9"                   // Redis 客户端
)

// OutputRedisStore 负责 Agent 输出 (Sorted Set) 的 Redis 读写。
type OutputRedisStore struct {
	client *redis.Client
}

// SaveAgentOutput 保存 Agent 输出到 Sorted Set (按版本号排序)。
func (s *OutputRedisStore) SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error {
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", output.AgentID))
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return s.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(output.Version),
		Member: string(data),
	}).Err()
}

// GetAgentOutputs 获取 Agent 在某话题下的全部输出 (按版本升序)。
func (s *OutputRedisStore) GetAgentOutputs(ctx context.Context, topicID, agentID string) ([]*types.AgentOutput, error) {
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
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

// GetLatestAgentOutput 获取 Agent 最新版本输出。
func (s *OutputRedisStore) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	key := topicKey(topicID, fmt.Sprintf("outputs:%s", agentID))
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
