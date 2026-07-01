package memory

import (
	"context" // 上下文，用于取消与超时传递

	"github.com/blockmemory/agent/backend/pkg/enums" // TopicStatus 枚举
	"github.com/blockmemory/agent/backend/pkg/types" // 公共类型
)

// SimpleWorkspaceReader 是 WorkspaceReader 的最小实现。
// 当前不从外部存储读取，而是根据调用方传入的 topicID 构造空话题元数据，
// 保证 ContextAssembler 在缺少完整工作区服务时仍可运行。
type SimpleWorkspaceReader struct{}

// NewSimpleWorkspaceReader 创建最小工作区读取器。
func NewSimpleWorkspaceReader() *SimpleWorkspaceReader {
	return &SimpleWorkspaceReader{}
}

// GetTopicMeta 返回一个默认活跃的话题元数据。
// 当前实现不查库，仅把 topicID 作为 ID/Goal 返回，避免 ContextAssembler 因元数据缺失而失败。
func (r *SimpleWorkspaceReader) GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	return &types.TopicMeta{
		ID:     topicID,
		Goal:   topicID,
		Status: enums.TopicStatusActive,
	}, nil
}

// GetTopicConstraints 返回空约束。
func (r *SimpleWorkspaceReader) GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error) {
	return make(map[string]string), nil
}

// GetLatestAgentOutput 返回空输出（未实现跨 Agent 输出读取）。
func (r *SimpleWorkspaceReader) GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error) {
	return nil, nil
}

// GetDepsGraph 返回空依赖图。
func (r *SimpleWorkspaceReader) GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error) {
	return make(map[string][]string), nil
}

// Ensure SimpleWorkspaceReader 实现 WorkspaceReader 接口。
var _ WorkspaceReader = (*SimpleWorkspaceReader)(nil)

// 以下类型用于编译期校验，避免 interface 漂移。
type _ = types.TopicMeta
type _ = types.AgentOutput
