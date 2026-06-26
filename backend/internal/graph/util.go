package graph

// 图相关的小工具集合：接口定义、map 取值、上下文构建请求/响应结构。
// 这些类型被多个节点文件共享，集中放在 util.go 避免循环依赖。

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// WorkspaceWriter 工作区写入接口。
// 抽象出"把 AgentOutput 持久化到工作区"的能力，便于节点解耦。
// 实现方在 store / runtime 包中，节点只依赖此接口。
type WorkspaceWriter interface {
	SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error
}

// BlockMemoryStore 块记忆存储接口（特性3）。
// 抽象 domainAgent 执行后归档与按相似度检索的能力，避免 graph 反向依赖 store。
// 实现方在 store 包（PostgresStore + pgvector）。
type BlockMemoryStore interface {
	// SaveBlockMemory 归档一条 domainAgent 完成的块记忆。
	SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string) error
	// SearchBlockMemory 按查询文本检索 topK 条相似块记忆，返回可注入 prompt 的文本段。
	SearchBlockMemory(ctx context.Context, query string, topK int) (string, error)
}

// getString 从 map 中获取字符串。
// 容错读取：map 为 nil 或 key 不存在或类型不符都返回空串。
// 用途：解析 LLM 返回的不确定结构 JSON 时安全取值。
func getString(m map[string]any, key string) string {
	if m == nil {
		return "" // 防 nil map
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s // 类型断言成功
		}
	}
	return "" // key 不存在或类型不符
}

// BuildRequest 上下文构建请求。
// 由节点向 memory 模块发起，请求组装一段上下文（消息列表 + Token 预算）。
type BuildRequest struct {
	AgentID    string                  // 请求方实例 ID
	TopicID    string                  // 主题 ID（记忆检索锚点）
	DependsOn  []string                // 依赖的其他 Agent 输出 ID
	TaskQuery  string                  // 任务查询串（用于语义检索）
	Snapshot   *types.AgentSnapshot    // 快照（可选，用于恢复上下文）
}

// ContextPack 上下文包。
// BuildRequest 的响应：组装好的消息列表 + Token 预算信息。
type ContextPack struct {
	Messages    []*Message          // 组装好的对话消息（System/User/Assistant）
	TokenBudget *types.TokenBudget  // Token 预算（分四段：System/TopicGlobal/SharedState/PrivateMemory）
}

// Message 消息。
// 轻量级消息结构，与 Eino SDK 解耦，便于在节点间传递。
type Message struct {
	Role    string // 角色：system / user / assistant
	Content string // 消息内容
}
