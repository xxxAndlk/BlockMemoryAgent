package graph

import (
	"context"

	"github.com/blockmemory/agent/pkg/types"
)

// Archiver 归档接口
type Archiver interface {
	SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions []byte, embedding []float32) error
	SetTopicTTL(ctx context.Context, topicID string, ttl int) error
}

// SinkerNode 归档节点
type SinkerNode struct {
	name     string
	archiver Archiver
}

// NewSinkerNode 创建归档节点
func NewSinkerNode(archiver Archiver) *SinkerNode {
	return &SinkerNode{
		name:     "Sinker",
		archiver: archiver,
	}
}

// Name 返回节点名称
func (n *SinkerNode) Name() string {
	return n.name
}

// Invoke 执行归档
func (n *SinkerNode) Invoke(ctx context.Context, state *State) (*State, error) {
	// 1. 生成话题级摘要
	summary := n.generateTopicSummary(state)

	// 2. 归档到全局知识库
	// TODO: 生成嵌入向量
	_ = n.archiver.SaveTopicArchive(ctx, state.TopicID, summary, nil, nil, nil)

	// 3. 设置 Redis TTL (7 天后清理)
	_ = n.archiver.SetTopicTTL(ctx, state.TopicID, 7*24*3600)

	// 标记完成
	state.NextAction = types.ActionFinish

	return state, nil
}

// generateTopicSummary 生成话题摘要
func (n *SinkerNode) generateTopicSummary(state *State) string {
	summary := "Topic: " + state.TopicGoal + "\n"
	summary += "Agents involved: "
	for id := range state.AgentOutputs {
		summary += id + " "
	}
	summary += "\n"
	return summary
}
