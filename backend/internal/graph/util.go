package graph

// 图相关的小工具集合：接口定义、map 取值、上下文构建请求/响应结构。
// 这些类型被多个节点文件共享，集中放在 util.go 避免循环依赖。

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
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
	Compress(ctx context.Context, agentID, topicID string) error
}

// AgentSnapshotManager Agent 快照管理器接口。
// 协调 Redis 热存与 Postgres 冷存的两级快照读写。
// 实现方在 memory 包。
type AgentSnapshotManager interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	SaveFromState(ctx context.Context, agentID, topicID string, output *types.AgentOutput, episodes []*types.Episode) error
}

// BlockMemoryStore 块记忆存储接口（特性3）。
// 抽象 domainAgent 执行后归档与按相似度检索的能力，避免 graph 反向依赖 store。
// 实现方在 store 包（PostgresStore + pgvector）。
type BlockMemoryStore interface {
	// SaveBlockMemory 归档一条 domainAgent 完成的块记忆。
	SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string) error
	// SearchBlockMemory 按 domain 过滤后检索 topK 条相似块记忆，返回可注入 prompt 的文本段。
	SearchBlockMemory(ctx context.Context, domain, query string, topK int) (string, error)
}

// DomainArchiveRecord domainAgent 归档记录（特性4）。
// 在 DomainAgent 完成后落库，跨会话可按领域相似度召回，复用其 Skill 子集与上下文摘要。
type DomainArchiveRecord struct {
	ArchiveID    string   // 归档唯一 ID（global_knowledge.id）
	SessionID    string   // 创建会话 ID
	Domain       string   // 领域名
	Goal         string   // 领域目标
	RoleDefID    string   // 角色定义 ID
	Skills       []string // 已装配 Skill ID 列表
	ContextSummary string // 上下文摘要（任务结果汇总）
	Weight       int      // 复用权重，每次被命中 +1
	ExpiresAt    time.Time // 过期时间（命中后延后）
	CreatedAt    time.Time // 创建时间
}

// DomainArchiveStore domainAgent 归档存储接口（特性4）。
type DomainArchiveStore interface {
	// SaveDomainArchive 归档或更新一条 domainAgent 记录。
	SaveDomainArchive(ctx context.Context, rec *DomainArchiveRecord) error
	// SearchDomainArchive 按领域/目标检索 topK 条相似归档，按权重降序。
	SearchDomainArchive(ctx context.Context, domain, goal string, topK int) ([]*DomainArchiveRecord, error)
	// BumpDomainArchiveWeight 命中复用时权重 +1 且延后过期时间。
	BumpDomainArchiveWeight(ctx context.Context, archiveID string, ttl time.Duration) error
	// CleanupExpiredDomainArchives 删除已过期归档，返回删除条数。
	CleanupExpiredDomainArchives(ctx context.Context) (int, error)
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
	Role    enums.ChatRole // 角色：system / user / assistant
	Content string         // 消息内容
}
