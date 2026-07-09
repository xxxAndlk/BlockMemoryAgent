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

// MemoryCallbackHandler 记忆回调处理器接口。
// 在节点生命周期事件上驱动 Episode 写入、快照保存与状态广播。
// 实现方在 memory 包，graph 只依赖接口避免循环依赖。
type MemoryCallbackHandler interface {
	OnStart(ctx context.Context, agentID, topicID string)
	OnEnd(ctx context.Context, agentID, topicID, action, rawContent string, stepCount int)
	OnError(ctx context.Context, agentID, topicID string, err error)
}

// ContextAssembler 上下文组装器接口。
// 将系统角色、话题目标、共享状态、全局知识与私有记忆按 Token 预算组装成消息列表。
// 实现方在 memory 包，返回 graph 包定义的 ContextPack。
type ContextAssembler interface {
	BuildContext(ctx context.Context, req *BuildRequest) (*ContextPack, error)
}

// EpisodeCompressor Episode 压缩器接口。
// 按重要性 + 时间对私有记忆做分层压缩，降低长期记忆 Token 占用。
// 实现方在 memory 包。
type EpisodeCompressor interface {
	Compress(ctx context.Context, agentID, topicID string) (*types.CompressStats, error)
}

// AgentSnapshotManager Agent 快照管理器接口。
// 协调 Redis 热存与 Postgres 冷存的两级快照读写。
// 实现方在 memory 包。
type AgentSnapshotManager interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	SaveFromState(ctx context.Context, agentID, topicID string, output *types.AgentOutput, episodes []*types.Episode) error
}

// BlockMemoryFact 块记忆键值化事实（graph 包本地定义，避免反向依赖 memory 包产生循环导入）。
type BlockMemoryFact struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope"` // global | domain | task
}

// BlockMemoryStore 块记忆存储接口（特性3）。
// 抽象 domainAgent 执行后归档与按相似度检索的能力，避免 graph 反向依赖 store。
// 实现方在 store 包（PostgresStore + pgvector）。
type BlockMemoryStore interface {
	// SaveBlockMemory 归档一条 domainAgent 完成的块记忆，附带结构化 facts。
	SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string, facts []BlockMemoryFact) error
	// SearchBlockMemory 按 domain 过滤后检索 topK 条相似块记忆，返回可注入 prompt 的文本段。
	SearchBlockMemory(ctx context.Context, domain, query string, topK int) (string, error)
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

// BuildRequest、ContextPack、Message 已下移到 pkg/types，避免 memory 包反向依赖 graph。
// 这里保留类型别名，保证 graph 包内部调用点无需修改，同时让外部消费者逐步迁移到 pkg/types。
type BuildRequest = types.BuildRequest
type ContextPack = types.ContextPack
type Message = types.Message
