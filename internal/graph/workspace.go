package graph

import (
	"context"

	"github.com/blockmemory/agent/pkg/types"
)

// WorkspaceUpdaterNode 工作区更新节点
type WorkspaceUpdaterNode struct {
	name      string
	workspace WorkspaceUpdater
}

// WorkspaceUpdater 工作区更新接口
type WorkspaceUpdater interface {
	PushEvent(ctx context.Context, topicID string, event *types.Event) error
	AppendDecision(ctx context.Context, topicID, decision string) error
}

// NewWorkspaceUpdaterNode 创建工作区更新节点
func NewWorkspaceUpdaterNode(workspace WorkspaceUpdater) *WorkspaceUpdaterNode {
	return &WorkspaceUpdaterNode{
		name:      "WorkspaceUpdater",
		workspace: workspace,
	}
}

// Name 返回节点名称
func (n *WorkspaceUpdaterNode) Name() string {
	return n.name
}

// Invoke 执行工作区更新
func (n *WorkspaceUpdaterNode) Invoke(ctx context.Context, state *State) (*State, error) {
	// 1. 处理 Event 队列中的事件
	for _, ev := range state.EventQueue {
		if ev.Status == types.EventPending {
			ev.Status = types.EventProcessing
			if err := n.workspace.PushEvent(ctx, state.TopicID, ev); err != nil {
				return state, err
			}
		}
	}

	// 2. 记录决策
	if state.Reason != "" {
		decision := state.Reason
		if state.CurrentAgent != "" {
			decision = state.CurrentAgent + ": " + decision
		}
		_ = n.workspace.AppendDecision(ctx, state.TopicID, decision)
	}

	// 3. 清空已处理的事件队列
	state.EventQueue = make([]*types.Event, 0)
	state.Reason = ""

	return state, nil
}
