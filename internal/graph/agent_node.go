package graph

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/pkg/types"
)

// ContextAssembler 上下文构建器接口
type ContextAssembler interface {
	BuildContext(ctx context.Context, req *BuildRequest) (*ContextPack, error)
}

// BuildRequest 上下文构建请求
type BuildRequest struct {
	AgentID   string
	TopicID   string
	TaskQuery string
	Snapshot  *types.AgentSnapshot
	DependsOn []string
}

// ContextPack 构建的上下文包
type ContextPack struct {
	Messages    []*Message
	TokenBudget *types.TokenBudget
}

// Message 简化消息结构
type Message struct {
	Role    string
	Content string
}

// SnapshotManager 快照管理器接口
type SnapshotManager interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	Save(ctx context.Context, snapshot *types.AgentSnapshot) error
}

// AgentExecutorNode Agent 执行节点
type AgentExecutorNode struct {
	name         string
	agentID      string
	assembler    ContextAssembler
	snapshotMgr  SnapshotManager
	workspace    WorkspaceWriter
}

// WorkspaceWriter 工作区写入接口
type WorkspaceWriter interface {
	SaveAgentOutput(ctx context.Context, topicID string, output *types.AgentOutput) error
}

// NewAgentExecutorNode 创建 Agent 执行节点
func NewAgentExecutorNode(agentID string, assembler ContextAssembler, snapshotMgr SnapshotManager, workspace WorkspaceWriter) *AgentExecutorNode {
	return &AgentExecutorNode{
		name:        agentID,
		agentID:     agentID,
		assembler:   assembler,
		snapshotMgr: snapshotMgr,
		workspace:   workspace,
	}
}

// Name 返回节点名称
func (n *AgentExecutorNode) Name() string {
	return n.name
}

// AgentID 返回 Agent ID
func (n *AgentExecutorNode) AgentID() string {
	return n.agentID
}

// Invoke 执行 Agent
func (n *AgentExecutorNode) Invoke(ctx context.Context, state *State) (*State, error) {
	// 1. 加载快照
	snapshot, err := n.snapshotMgr.Load(ctx, n.agentID, state.TopicID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// 2. 构建上下文
	ctxPack, err := n.assembler.BuildContext(ctx, &BuildRequest{
		AgentID:   n.agentID,
		TopicID:   state.TopicID,
		TaskQuery: state.TopicGoal,
		Snapshot:  snapshot,
		DependsOn: nil, // 从注册表获取
	})
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}

	// 3. 模拟执行 (实际应调用 Eino ChatModel)
	result := n.execute(ctx, ctxPack, state)

	// 4. 保存输出到工作区
	output := &types.AgentOutput{
		AgentID:   n.agentID,
		Version:   getNextVersion(state, n.agentID),
		Summary:   result,
		Validated: false,
	}
	if err := n.workspace.SaveAgentOutput(ctx, state.TopicID, output); err != nil {
		return nil, fmt.Errorf("save output: %w", err)
	}

	// 5. 更新状态
	state.CurrentAgent = n.agentID
	state.SetAgentOutput(n.agentID, output)

	return state, nil
}

// execute 模拟 Agent 执行
func (n *AgentExecutorNode) execute(ctx context.Context, pack *ContextPack, state *State) string {
	// TODO: 实际实现应调用 Eino ChatModel
	// 这里返回模拟结果
	return fmt.Sprintf("Agent %s executed with %d messages", n.agentID, len(pack.Messages))
}

// getNextVersion 获取下一个版本号
func getNextVersion(state *State, agentID string) int {
	output := state.GetAgentOutput(agentID)
	if output == nil {
		return 1
	}
	return output.Version + 1
}
