package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/pkg/types"
)

type stepKeyType struct{}

var stepKey = stepKeyType{}

// EscalationHandlerNode 升级处理节点（3层架构兼容）
type EscalationHandlerNode struct {
	name string
}

// NewEscalationHandlerNode 创建升级处理节点
func NewEscalationHandlerNode() *EscalationHandlerNode {
	return &EscalationHandlerNode{name: "EscalationHandler"}
}

// Name 返回节点名称
func (n *EscalationHandlerNode) Name() string {
	return n.name
}

// Invoke 执行升级处理（ThreeLayerNode 接口）
func (n *EscalationHandlerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	fmt.Printf("[ESCALATION] Session: %s, Reason: %s\n", state.SessionID, state.Reason)

	// 生成仲裁摘要
	arbitration := fmt.Sprintf("Escalation: %s", state.Reason)

	// 添加事件到当前会话块
	if block := state.ActiveBlocks[state.CurrentBlockID]; block != nil {
		block.Events = append(block.Events, &types.Event{
			ID:          fmt.Sprintf("esc_%d", time.Now().UnixNano()),
			Type:        types.EventEscalation,
			SourceAgent: state.CurrentDomain,
			Payload:     map[string]any{"arbitration": arbitration},
			Priority:    10,
			Status:      types.EventPending,
		})
	}

	state.NextAction = types.ActionContinue
	state.Reason = ""

	return state, nil
}
