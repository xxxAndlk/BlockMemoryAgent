package memory

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/pkg/types"
)

// WorkspaceReader 工作区读取接口
type WorkspaceReader interface {
	GetTopicMeta(ctx context.Context, topicID string) (*types.TopicMeta, error)
	GetTopicConstraints(ctx context.Context, topicID string) (map[string]string, error)
	GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error)
	GetDepsGraph(ctx context.Context, topicID string) (map[string][]string, error)
}

// GlobalRetriever 全局知识检索接口
type GlobalRetriever interface {
	Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error)
}

// ContextAssembler 上下文构建器
type ContextAssembler struct {
	workspace   WorkspaceReader
	globalKB    GlobalRetriever
	store       PrivateStore
	budget      *types.TokenBudget
}

// NewContextAssembler 创建上下文构建器
func NewContextAssembler(workspace WorkspaceReader, globalKB GlobalRetriever, store PrivateStore) *ContextAssembler {
	return &ContextAssembler{
		workspace: workspace,
		globalKB:  globalKB,
		store:     store,
		budget: &types.TokenBudget{
			SystemRole:    2048,
			TopicGlobal:   4096,
			SharedState:   8192,
			GlobalKB:      4096,
			PrivateMemory: 8192,
			TaskQuery:     2048,
			Reserve:       4096,
		},
	}
}

// BuildContext 构建上下文
func (a *ContextAssembler) BuildContext(ctx context.Context, req *graph.BuildRequest) (*graph.ContextPack, error) {
	// 1. 获取话题信息
	topicMeta, err := a.workspace.GetTopicMeta(ctx, req.TopicID)
	if err != nil {
		return nil, fmt.Errorf("get topic meta: %w", err)
	}

	constraints, err := a.workspace.GetTopicConstraints(ctx, req.TopicID)
	if err != nil {
		constraints = make(map[string]string)
	}

	// 2. 获取依赖相关的上游输出
	var sharedOutputs []*types.AgentOutput
	for _, depAgentID := range req.DependsOn {
		output, err := a.workspace.GetLatestAgentOutput(ctx, req.TopicID, depAgentID)
		if err != nil {
			continue
		}
		if output != nil {
			sharedOutputs = append(sharedOutputs, output)
		}
	}

	// 3. 检索私有记忆
	episodes, err := a.store.GetEpisodes(ctx, req.AgentID, req.TopicID, 50)
	if err != nil {
		return nil, fmt.Errorf("get episodes: %w", err)
	}

	// 4. 全局知识检索
	globalRecords, err := a.globalKB.Retrieve(ctx, req.TaskQuery, 3)
	if err != nil {
		globalRecords = nil // 非致命错误
	}

	// 5. 按相关性分配配额并裁剪私有记忆
	selectedEpisodes := a.allocateByRelevance(episodes, a.budget.PrivateMemory)

	// 6. 组装消息
	messages := a.buildMessages(topicMeta, constraints, sharedOutputs, selectedEpisodes, globalRecords, req)

	return &graph.ContextPack{
		Messages:    messages,
		TokenBudget: a.budget,
	}, nil
}

// allocateByRelevance 按相关性分配配额
func (a *ContextAssembler) allocateByRelevance(episodes []*types.Episode, budget int) []*types.Episode {
	if len(episodes) == 0 {
		return nil
	}

	// 简化实现: 按重要性降序，取前 N 条
	// TODO: 实现完整的多信号评分和配额分配
	sorted := make([]*types.Episode, len(episodes))
	copy(sorted, episodes)

	// 按重要性降序排序 (简化冒泡排序)
	for i := 0; i < len(sorted)-1; i++ {
		for j := 0; j < len(sorted)-1-i; j++ {
			if sorted[j].Importance < sorted[j+1].Importance {
				sorted[j], sorted[j+1] = sorted[j+1], sorted[j]
			}
		}
	}

	// 计算可容纳的条数 (假设每条平均 200 token)
	avgTokens := 200
	maxCount := budget / avgTokens
	if maxCount > len(sorted) {
		maxCount = len(sorted)
	}

	return sorted[:maxCount]
}

// buildMessages 构建消息列表
func (a *ContextAssembler) buildMessages(
	topicMeta *types.TopicMeta,
	constraints map[string]string,
	sharedOutputs []*types.AgentOutput,
	selectedEpisodes []*types.Episode,
	globalRecords []*types.KnowledgeRecord,
	req *graph.BuildRequest,
) []*graph.Message {
	var messages []*graph.Message

	// [System] 角色定义 + 全局约束
	systemContent := "You are an AI agent working on a specific task."
	if topicMeta != nil {
		systemContent += fmt.Sprintf(" Topic: %s.", topicMeta.Goal)
	}
	if len(constraints) > 0 {
		systemContent += " Constraints:"
		for k, v := range constraints {
			systemContent += fmt.Sprintf(" %s=%s;", k, v)
		}
	}
	messages = append(messages, &graph.Message{
		Role:    "system",
		Content: systemContent,
	})

	// [TopicGlobal] 话题目标 + 当前状态
	if topicMeta != nil {
		messages = append(messages, &graph.Message{
			Role:    "user",
			Content: fmt.Sprintf("Topic Goal: %s", topicMeta.Goal),
		})
	}

	// [SharedState] 相关上游输出
	if len(sharedOutputs) > 0 {
		content := "Related outputs from other agents:\n"
		for _, output := range sharedOutputs {
			content += fmt.Sprintf("- %s (v%d): %s\n", output.AgentID, output.Version, output.Summary)
		}
		messages = append(messages, &graph.Message{
			Role:    "user",
			Content: content,
		})
	}

	// [GlobalKB] 全局知识
	if len(globalRecords) > 0 {
		content := "Relevant knowledge:\n"
		for _, rec := range globalRecords {
			content += fmt.Sprintf("- [%s] %s\n", rec.KnowledgeType, rec.Content)
		}
		messages = append(messages, &graph.Message{
			Role:    "user",
			Content: content,
		})
	}

	// [Private] 私有记忆
	if len(selectedEpisodes) > 0 {
		content := "Your previous work:\n"
		for _, ep := range selectedEpisodes {
			content += fmt.Sprintf("- [%s] %s (importance: %.2f)\n", ep.StepID, ep.ObservationSummary, ep.Importance)
		}
		messages = append(messages, &graph.Message{
			Role:    "user",
			Content: content,
		})
	}

	// [Snapshot] 快照信息
	if req.Snapshot != nil && len(req.Snapshot.OpenIssues) > 0 {
		content := "Open issues:\n"
		for _, issue := range req.Snapshot.OpenIssues {
			content += fmt.Sprintf("- %s: %s\n", issue.ID, issue.Description)
		}
		messages = append(messages, &graph.Message{
			Role:    "user",
			Content: content,
		})
	}

	// [Task] 当前任务
	messages = append(messages, &graph.Message{
		Role:    "user",
		Content: fmt.Sprintf("Current task: %s", req.TaskQuery),
	})

	return messages
}
