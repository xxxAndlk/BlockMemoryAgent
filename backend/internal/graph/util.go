package graph

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// WorkspaceWriter 工作区写入接口
type WorkspaceWriter interface {
	SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error
}

// getString 从 map 中获取字符串
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// BuildRequest 上下文构建请求
type BuildRequest struct {
	AgentID    string
	TopicID    string
	DependsOn  []string
	TaskQuery  string
	Snapshot   *types.AgentSnapshot
}

// ContextPack 上下文包
type ContextPack struct {
	Messages    []*Message
	TokenBudget *types.TokenBudget
}

// Message 消息
type Message struct {
	Role    string
	Content string
}
