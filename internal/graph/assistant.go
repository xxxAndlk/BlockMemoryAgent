package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/pkg/types"
)

// AssistantNode Layer 3: 助手角色 / 具体任务执行者
type AssistantNode struct {
	name      string
	instID    string            // 本实例ID
	registry  *RoleRegistry
	workspace WorkspaceWriter
}

// NewAssistantNode 创建助手节点
func NewAssistantNode(instID string, registry *RoleRegistry, workspace WorkspaceWriter) *AssistantNode {
	return &AssistantNode{
		name:      "Assistant",
		instID:    instID,
		registry:  registry,
		workspace: workspace,
	}
}

// Name 返回节点名称
func (n *AssistantNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID
func (n *AssistantNode) InstanceID() string {
	return n.instID
}

// Invoke 执行助手逻辑
func (n *AssistantNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return nil, fmt.Errorf("assistant instance %s not found", n.instID)
	}

	// 获取角色定义
	roleDef := n.registry.GetRoleDef(inst.RoleDefID)
	if roleDef == nil {
		return nil, fmt.Errorf("role def %s not found", inst.RoleDefID)
	}

	// 更新状态为活跃
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// 1. 获取当前调用请求（从调用栈顶部）
	var callReq *types.CallRequest
	if len(state.CallStack) > 0 {
		callReq = state.CallStack[len(state.CallStack)-1]
	}

	// 2. 构建上下文
	task := roleDef.Description
	if callReq != nil {
		task = callReq.Task
	}

	// 3. 执行任务（实际应调用ChatModel）
	result := n.executeTask(ctx, roleDef, task, callReq)

	// 4. 将结果写入对应会话块
	if callReq != nil {
		if blockID, ok := callReq.Context["block_id"].(string); ok {
			if block := state.ActiveBlocks[blockID]; block != nil {
				if block.TaskResults == nil {
					block.TaskResults = make(map[string]string)
				}
				block.TaskResults[callReq.Task] = result
			}
		}
	}

	// 5. 保存输出
	output := &types.AgentOutput{
		AgentID:   n.instID,
		Version:   1,
		Summary:   result,
		Timestamp: time.Now(),
		Validated: true, // 助手输出直接标记为有效
	}

	if n.workspace != nil {
		_ = n.workspace.SaveAgentOutput(ctx, state.SessionID, output)
	}

	// 5. 更新状态为完成
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	// 6. 弹出调用栈
	state.PopCallStack()

	// 7. 如果有调用者，返回给调用者
	if callReq != nil && callReq.CallerID != "" {
		// 更新调用者状态
		n.registry.UpdateInstanceStatus(callReq.CallerID, types.RoleStatusActive)
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = callReq.CallerID
	} else {
		// 没有调用者，返回MetaAgent
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// executeTask 执行具体任务
func (n *AssistantNode) executeTask(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) string {
	// TODO: 实际应调用配置中的ChatModel
	// 这里返回模拟结果

	contextInfo := ""
	if callReq != nil && callReq.Context != nil {
		if domain, ok := callReq.Context["domain"]; ok {
			contextInfo = fmt.Sprintf("[领域: %v] ", domain)
		}
	}

	return fmt.Sprintf("%s助手[%s]完成任务: %s\n结果: 已处理%s",
		contextInfo, roleDef.Name, task, n.instID)
}

// CanHandle 检查是否能处理某类任务
func (n *AssistantNode) CanHandle(taskType string) bool {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return false
	}

	roleDef := n.registry.GetRoleDef(inst.RoleDefID)
	if roleDef == nil {
		return false
	}

	for _, skill := range roleDef.Skills {
		if skill == taskType {
			return true
		}
	}
	return false
}
