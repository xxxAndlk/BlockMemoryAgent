package tool

// agentctx.go 提供工具包侧的 agent ID 上下文存取。
// 与 sessionIDKey 同处一个包，供 WriteSharedMemory 等需要识别调用方 Agent
// 的工具使用；agent 包通过 thin wrapper 复用同一 key 类型，避免重复定义。

import "context"

// agentIDKey 是用于在 context 中携带当前代理 ID 的键类型。
// 工具处理器（例如 call_sub_agent、WriteSharedMemory）通过它识别父代理。
type agentIDKey struct{}

// WithAgentID 返回一个携带当前代理 ID 的 context。
// ctx 是基础上下文；agentID 是要携带的代理标识。
func WithAgentID(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, agentID)
}

// AgentIDFromContext 从 ctx 中取出存储的代理 ID。
// 不存在时返回空字符串。
func AgentIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(agentIDKey{}).(string); ok {
		return v
	}
	return ""
}
