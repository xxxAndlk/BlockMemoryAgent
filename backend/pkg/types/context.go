package types

import "github.com/blockmemory/agent/backend/pkg/enums"

// Message 消息。
// 轻量级消息结构，与 Eino SDK 解耦，便于在节点间传递。
// 已从 internal/graph 下移到 pkg/types，供 memory / graph 共享，避免 memory→graph 依赖。
type Message struct {
	Role    enums.ChatRole // 角色：system / user / assistant
	Content string         // 消息内容
}

// ContextPack 上下文包。
// BuildRequest 的响应：组装好的消息列表 + Token 预算信息。
type ContextPack struct {
	Messages    []*Message    // 组装好的对话消息（System/User/Assistant）
	TokenBudget *TokenBudget  // Token 预算（分四段：System/TopicGlobal/SharedState/PrivateMemory）
}

// BuildRequest 上下文构建请求。
// 由节点向 memory 模块发起，请求组装一段上下文（消息列表 + Token 预算）。
type BuildRequest struct {
	AgentID   string          // 请求方实例 ID
	TopicID   string          // 主题 ID（记忆检索锚点）
	DependsOn []string        // 依赖的其他 Agent 输出 ID
	TaskQuery string          // 任务查询串（用于语义检索）
	Snapshot  *AgentSnapshot  // 快照（可选，用于恢复上下文）
}
