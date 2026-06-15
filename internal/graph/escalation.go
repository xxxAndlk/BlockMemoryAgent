package graph

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/pkg/types"
)

// EscalationHandlerNode 升级处理节点
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

// Invoke 执行升级处理
func (n *EscalationHandlerNode) Invoke(ctx context.Context, state *State) (*State, error) {
	// 1. 记录升级事件
	fmt.Printf("[ESCALATION] Topic: %s, Reason: %s\n", state.TopicID, state.Reason)

	// 2. 生成仲裁摘要
	arbitration := n.generateArbitration(state)

	// 3. 添加到 Event 队列
	state.AddEvent(&types.Event{
		ID:          fmt.Sprintf("esc_%d", ctx.Value("step")),
		Type:        types.EventEscalation,
		SourceAgent: state.CurrentAgent,
		Payload:     map[string]any{"arbitration": arbitration},
		Priority:    10,
		Status:      types.EventPending,
	})

	// 4. 重置为继续状态
	state.NextAction = types.ActionContinue
	state.Reason = ""

	return state, nil
}

// generateArbitration 生成仲裁摘要
func (n *EscalationHandlerNode) generateArbitration(state *State) string {
	return fmt.Sprintf("Agent %s escalation: %s", state.CurrentAgent, state.Reason)
}
