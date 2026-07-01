package memory

import (
	"context"
	"fmt"
	"sort"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// WorkspaceReader 工作区读取接口：抽象跨 Agent 共享状态的读取能力。
// 实现方通常基于 Redis / Postgres 提供 Topic 元数据、约束、上游输出与依赖图。
type WorkspaceReader interface {
	// GetTopicMeta 读取话题元数据（目标、状态、时间）。
	GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error)
	// GetTopicConstraints 读取话题级约束键值对，供 System 段注入。
	GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error)
	// GetLatestAgentOutput 读取某 Agent 在某 Topic 下的最新公开输出，用于 SharedState 段。
	GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error)
	// GetDepsGraph 读取话题内 Agent 间的依赖邻接表，保留以便后续扩展按依赖裁剪。
	GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error)
}

// GlobalRetriever 全局知识检索接口：从全局知识库召回与查询相关的 KnowledgeRecord。
// 实现方通常对接 Postgres + pgvector 的语义检索。
type GlobalRetriever interface {
	// Retrieve 按查询语义召回 topK 条全局知识记录。
	Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error)
}

// ContextAssembler 上下文构建器：将多源信息组装为 LLM 可消费的 4 段式上下文。
// 段划分：System（角色+约束）/ TopicGlobal（话题目标）/ SharedState（上游输出+全局知识）/
// PrivateMemory（私有记忆+快照）。各段受 TokenBudget 限制。
type ContextAssembler struct {
	workspace WorkspaceReader     // 工作区读取句柄，提供共享状态
	globalKB  GlobalRetriever     // 全局知识检索句柄
	store     PrivateStore        // 私有记忆存储句柄，提供 Episode 读取
	budget    *types.TokenBudget  // 4 段 Token 预算配置
	scorer    *SearchScorer       // 多信号评分器（可选），nil 时退回重要性排序
}

// NewContextAssembler 创建上下文构建器并初始化默认 Token 预算。
// 参数：workspace 工作区读取；globalKB 全局知识检索；store 私有记忆存储。
// 返回：装配好的 *ContextAssembler，budget 字段使用默认段位配置。
// 副作用：无。
func NewContextAssembler(workspace WorkspaceReader, globalKB GlobalRetriever, store PrivateStore) *ContextAssembler {
	// 组装依赖并配置默认 Token 预算（各段位数值依据经验设定，可后续从配置覆盖）。
	return &ContextAssembler{
		workspace: workspace,
		globalKB:  globalKB,
		store:     store,
		budget: &types.TokenBudget{
			SystemRole:    2048, // 系统角色段：soul.md + 角色定义
			TopicGlobal:   4096, // 话题全局段：目标与状态
			SharedState:   8192, // 共享状态段：上游 Agent 输出
			GlobalKB:      4096, // 全局知识段：KnowledgeRecord
			PrivateMemory: 8192, // 私有记忆段：本 Agent 的 Episode
			TaskQuery:     2048, // 任务查询段：当前任务描述
			Reserve:       4096, // 预留缓冲，防止超出模型上下文窗口
		},
	}
}

// SetScorer 注入多信号评分器，启用语义级记忆排序。
func (a *ContextAssembler) SetScorer(scorer *SearchScorer) { a.scorer = scorer }

// BuildContext 构建一次 LLM 调用所需的完整上下文。
// 流程：取话题元数据 → 取约束 → 取上游输出 → 取私有记忆 → 全局知识检索 →
// 按相关性裁剪私有记忆 → 组装 4 段消息 → 返回 ContextPack。
//
// 参数：
//   - ctx: 取消信号。
//   - req: 构建请求，包含 AgentID / TopicID / 依赖 / 当前任务 / 快照。
//
// 返回：组装好的 *graph.ContextPack（含消息列表与 Token 预算）；失败时返回错误。
// 副作用：可能触发 store / workspace / globalKB 的读取操作。
// 并发安全：实例本身无共享可变状态；底层依赖由实现方保证。
func (a *ContextAssembler) BuildContext(ctx context.Context, req *graph.BuildRequest) (*graph.ContextPack, error) {
	// 1. 获取话题元数据，用于 System 与 TopicGlobal 段。
	topicMeta, err := a.workspace.GetTopicMeta(ctx, req.TopicID)
	if err != nil {
		// 话题元数据缺失视为致命错误，直接返回。
		return nil, fmt.Errorf("get topic meta: %w", err)
	}

	// 获取话题约束（键值对），失败时降级为空 map 继续组装。
	constraints, err := a.workspace.GetTopicConstraints(ctx, req.TopicID)
	if err != nil {
		// 约束读取失败不阻断流程，使用空约束。
		constraints = make(map[string]string)
	}

	// 2. 获取依赖相关的上游输出，构成 SharedState 段的核心内容。
	var sharedOutputs []*types.AgentOutput
	for _, depAgentID := range req.DependsOn {
		// 逐个依赖 Agent 读取其最新公开输出。
		output, err := a.workspace.GetLatestAgentOutput(ctx, req.TopicID, depAgentID)
		if err != nil {
			// 单个依赖读取失败时跳过，避免整体流程中断。
			continue
		}
		if output != nil {
			// 仅在非空时纳入，避免 nil 污染后续格式化。
			sharedOutputs = append(sharedOutputs, output)
		}
	}

	// 3. 检索私有记忆：取本 Agent 最近 50 条 Episode 作为候选。
	episodes, err := a.store.GetEpisodes(ctx, req.AgentID, req.TopicID, 50)
	if err != nil {
		// 私有记忆读取失败视为致命错误，因为它是 Agent 上下文的核心。
		return nil, fmt.Errorf("get episodes: %w", err)
	}

	// 4. 全局知识检索：按当前任务查询召回 top 3 条 KnowledgeRecord。
	globalRecords, err := a.globalKB.Retrieve(ctx, req.TaskQuery, 3)
	if err != nil {
		// 全局知识检索为非致命错误，缺失时跳过该段。
		globalRecords = nil
	}

	// 5. 按相关性分配配额并裁剪私有记忆，控制在 PrivateMemory 预算内。
	selectedEpisodes := a.allocateByRelevance(ctx, episodes, req.TaskQuery, a.budget.PrivateMemory)

	// 6. 组装为消息列表，按 System / TopicGlobal / SharedState / GlobalKB / Private / Snapshot / Task 顺序拼接。
	messages := a.buildMessages(topicMeta, constraints, sharedOutputs, selectedEpisodes, globalRecords, req)

	// 返回 ContextPack，包含消息列表与本次使用的 Token 预算。
	return &graph.ContextPack{
		Messages:    messages,
		TokenBudget: a.budget,
	}, nil
}

// allocateByRelevance 按多信号相关性分配 Token 配额，裁剪私有记忆。
// 优先使用 scorer 做语义+实体+因果+时间多信号评分；scorer 为 nil 时退回重要性排序。
//
// 参数：
//   - ctx: 上下文，透传给 SearchAndScore/Embedder 以支持取消（M4 修复：原用 context.Background() 忽略上层取消）。
//   - episodes: 候选 Episode 列表。
//   - query: 当前任务查询文本，供多信号评分使用。
//   - budget: PrivateMemory 段的 Token 预算上限。
//
// 返回：裁剪后的 Episode 切片（按相关性降序）；输入为空时返回 nil。
// 副作用：scorer 非 nil 时可能调用 Embedder 计算语义相似度。
func (a *ContextAssembler) allocateByRelevance(ctx context.Context, episodes []*types.Episode, query string, budget int) []*types.Episode {
	if len(episodes) == 0 {
		return nil
	}

	sorted := make([]*types.Episode, len(episodes))
	copy(sorted, episodes)

	if a.scorer != nil {
		// 多信号评分：为每条 episode 调用 ScoreEpisode，综合语义/实体/时间/因果
		results, err := a.scorer.SearchAndScore(ctx, "", "", query, sorted)
		if err == nil {
			// 按 FinalScore 降序提取 episode
			sorted = make([]*types.Episode, 0, len(results))
			for _, r := range results {
				sorted = append(sorted, r.Episode)
			}
		}
	}

	// scorer 不可用时回退到重要性排序
	if a.scorer == nil {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Importance > sorted[j].Importance
		})
	}

	avgTokens := 200
	maxCount := budget / avgTokens
	if maxCount > len(sorted) {
		maxCount = len(sorted)
	}
	if maxCount <= 0 {
		maxCount = 1
	}

	return sorted[:maxCount]
}

// buildMessages 构建最终发送给 LLM 的消息列表。
// 按 4 段式（含 Snapshot 与 Task 扩展段）顺序拼接：
// System → TopicGlobal → SharedState → GlobalKB → Private → Snapshot → Task。
//
// 参数：
//   - topicMeta: 话题元数据，可空。
//   - constraints: 话题约束键值对，可为空 map。
//   - sharedOutputs: 上游 Agent 公开输出列表。
//   - selectedEpisodes: 经裁剪的本 Agent 私有记忆。
//   - globalRecords: 全局知识库召回记录。
//   - req: 原始构建请求，用于读取 Snapshot 与 TaskQuery。
//
// 返回：组装好的 graph.Message 切片。
// 副作用：无。
func (a *ContextAssembler) buildMessages(
	topicMeta *types.TopicMeta,
	constraints map[string]string,
	sharedOutputs []*types.AgentOutput,
	selectedEpisodes []*types.Episode,
	globalRecords []*types.KnowledgeRecord,
	req *graph.BuildRequest,
) []*graph.Message {
	// 预声明消息切片，按段位顺序 append。
	var messages []*graph.Message

	// [System] 角色定义 + 全局约束：固定前缀 + 话题目标 + 约束键值对。
	systemContent := "You are an AI agent working on a specific task."
	if topicMeta != nil {
		// 追加话题目标，帮助 LLM 对齐当前语境。
		systemContent += fmt.Sprintf(" Topic: %s.", topicMeta.Goal)
	}
	if len(constraints) > 0 {
		// 追加约束前缀，再逐项拼接 key=value。
		systemContent += " Constraints:"
		for k, v := range constraints {
			systemContent += fmt.Sprintf(" %s=%s;", k, v)
		}
	}
	// 以 system 角色注入约束段。
	messages = append(messages, &graph.Message{
		Role:    enums.ChatRoleSystem,
		Content: systemContent,
	})

	// [TopicGlobal] 话题目标 + 当前状态：以 user 角色强化目标。
	if topicMeta != nil {
		messages = append(messages, &graph.Message{
			Role:    enums.ChatRoleUser,
			Content: fmt.Sprintf("Topic Goal: %s", topicMeta.Goal),
		})
	}

	// [SharedState] 相关上游输出：列举依赖 Agent 的最新输出摘要。
	if len(sharedOutputs) > 0 {
		// 固定头部说明本段语义。
		content := "Related outputs from other agents:\n"
		for _, output := range sharedOutputs {
			// 每条输出格式：- AgentID (v版本): 摘要。
			content += fmt.Sprintf("- %s (v%d): %s\n", output.AgentID, output.Version, output.Summary)
		}
		messages = append(messages, &graph.Message{
			Role:    enums.ChatRoleUser,
			Content: content,
		})
	}

	// [GlobalKB] 全局知识：列举检索召回的 KnowledgeRecord。
	if len(globalRecords) > 0 {
		// 固定头部说明本段为相关知识。
		content := "Relevant knowledge:\n"
		for _, rec := range globalRecords {
			// 每条知识格式：- [类型] 正文。
			content += fmt.Sprintf("- [%s] %s\n", rec.KnowledgeType, rec.Content)
		}
		messages = append(messages, &graph.Message{
			Role:    enums.ChatRoleUser,
			Content: content,
		})
	}

	// [Private] 私有记忆：列举本 Agent 的工作历史，附重要性以便 LLM 权衡。
	if len(selectedEpisodes) > 0 {
		// 固定头部说明本段为自身历史。
		content := "Your previous work:\n"
		for _, ep := range selectedEpisodes {
			// 每条历史格式：- [StepID] 摘要 (importance: 分值)。
			content += fmt.Sprintf("- [%s] %s (importance: %.2f)\n", ep.StepID, ep.ObservationSummary, ep.Importance)
		}
		messages = append(messages, &graph.Message{
			Role:    enums.ChatRoleUser,
			Content: content,
		})
	}

	// [Snapshot] 快照信息：注入未解决问题，便于 LLM 延续处理。
	if req.Snapshot != nil && len(req.Snapshot.OpenIssues) > 0 {
		// 固定头部说明本段为未决问题。
		content := "Open issues:\n"
		for _, issue := range req.Snapshot.OpenIssues {
			// 每条问题格式：- 问题ID: 描述。
			content += fmt.Sprintf("- %s: %s\n", issue.ID, issue.Description)
		}
		messages = append(messages, &graph.Message{
			Role:    enums.ChatRoleUser,
			Content: content,
		})
	}

	// [Task] 当前任务：最后一条消息，明确本次调用需要 LLM 解决的问题。
	messages = append(messages, &graph.Message{
		Role:    enums.ChatRoleUser,
		Content: fmt.Sprintf("Current task: %s", req.TaskQuery),
	})

	// 返回组装好的消息列表，供 LLM 调用方直接使用。
	return messages
}
