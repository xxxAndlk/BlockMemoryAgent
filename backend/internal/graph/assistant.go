package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// AssistantNode Layer 3: 助手角色 / 具体任务执行者。
//
// 职责：
//   - 接收上层（Domain/SubDomain）派发的单一任务并执行
//   - 通过 ModelFactory 调用真实 LLM 完成任务（无 API key 时退回 mock）
//   - 把执行结果回写到对应 SessionBlock 的 TaskResults
//   - 通过 PopCallStack 返回控制权给调用者
//
// 并发安全：节点本身无共享可变状态；实例状态由 registry 内部锁保护。
type AssistantNode struct {
	name         string             // 节点名（固定 "Assistant"），实现 ThreeLayerNode.Name
	instID       string             // 本实例ID，对应 registry 中的 RoleInstance.ID
	registry     *RoleRegistry      // 角色注册表，查询实例/角色定义、更新状态
	workspace    WorkspaceWriter    // 工作区写入器，持久化 AgentOutput（可能为 nil）
	modelFactory *model.ModelFactory // LLM 模型工厂，nil 时退回 mock
	rt           *runtime.Runtime   // 运行时聚合体，读取 AgentCfg 超时配置
	progress     ProgressCallback   // 进度回调，推送 prompt / token_usage 事件
}

// NewAssistantNode 创建助手节点。
//
// 参数：
//   - instID：实例ID（由上层 factory.CreateInstance 生成）
//   - registry：角色注册表
//   - workspace：工作区写入器（可为 nil，仅退化跳过持久化）
//
// 返回：装配好的 *AssistantNode，待图调度循环拉起。
func NewAssistantNode(instID string, registry *RoleRegistry, workspace WorkspaceWriter) *AssistantNode {
	return &AssistantNode{
		name:      "Assistant",
		instID:    instID,
		registry:  registry,
		workspace: workspace,
	}
}

// Name 返回节点名称。
func (n *AssistantNode) Name() string { return n.name }

// InstanceID 返回实例ID。
func (n *AssistantNode) InstanceID() string { return n.instID }

// —— 依赖注入接收者接口实现 ——

// SetModelFactory 实现 ModelFactoryReceiver。
func (n *AssistantNode) SetModelFactory(mf *model.ModelFactory) { n.modelFactory = mf }

// SetRuntime 实现 RuntimeReceiver。
func (n *AssistantNode) SetRuntime(rt *runtime.Runtime) { n.rt = rt }

// SetProgressCallback 实现 ProgressCallbackReceiver。
func (n *AssistantNode) SetProgressCallback(cb ProgressCallback) { n.progress = cb }

// Invoke 执行本节点。
//
// 流程：
//  1. 注册/更新本实例状态为 active
//  2. 从调用栈顶取 CallRequest（含 task 文本与上下文）
//  3. 取角色定义（含名称 / 技能 / 描述）
//  4. 调用 LLM 执行任务（或退回 mock）
//  5. 结果回写到 SessionBlock
//  6. 持久化 AgentOutput
//  7. 标记完成，弹出调用栈
//  8. 路由：有调用者切回调用者，否则 Continue
func (n *AssistantNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 取本实例信息
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return nil, fmt.Errorf("assistant instance %s not found", n.instID)
	}

	// 2. 取角色定义
	roleDef := n.registry.GetRoleDef(inst.RoleDefID)
	if roleDef == nil {
		return nil, fmt.Errorf("role def %s not found", inst.RoleDefID)
	}

	// 3. 标记为活跃
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// 4. 从调用栈顶 peek 调用请求
	var callReq *types.CallRequest
	if len(state.CallStack) > 0 {
		callReq = state.CallStack[len(state.CallStack)-1]
	}

	// 5. 确定任务文本
	task := roleDef.Description
	if callReq != nil {
		task = callReq.Task
	}

	// 6. 执行任务 — 优先走 LLM，不可用时退回 mock
	result := n.executeTask(ctx, roleDef, task, callReq)

	// 7. 回写结果到 SessionBlock
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

	// 8. 持久化 AgentOutput
	output := &types.AgentOutput{
		AgentID:   n.instID,
		Version:   1,
		Summary:   result,
		Timestamp: time.Now(),
		Validated: true,
	}
	if n.workspace != nil {
		_ = n.workspace.SaveAgentOutput(ctx, state.SessionID, output)
	}

	// 9. 标记为完成
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	// 10. 弹出调用栈
	state.PopCallStack()

	// 11. 路由
	if callReq != nil && callReq.CallerID != "" {
		n.registry.UpdateInstanceStatus(callReq.CallerID, types.RoleStatusActive)
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = callReq.CallerID
	} else {
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// executeTask 调用 LLM 执行任务（不可用时退回 mock）。
func (n *AssistantNode) executeTask(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) string {
	// 有模型工厂时调用真实 LLM
	if n.modelFactory != nil {
		result, err := n.callLLM(ctx, roleDef, task, callReq)
		if err == nil {
			return result
		}
		// LLM 调用失败时退回 mock，保证主流程不中断
		if n.progress != nil {
			n.progress(ctx, ProgressEvent{
				SessionID: SessionIDFromContext(ctx),
				Kind:      "error",
				Agent:     n.instID,
				Message:   fmt.Sprintf("LLM call failed, fallback to mock: %v", err),
			})
		}
	}

	// LLM 不可用时退回模拟结果
	return n.mockResult(roleDef, task, callReq)
}

// callLLM 构建 prompt 并调用 LLM。
func (n *AssistantNode) callLLM(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) (string, error) {
	llm, err := n.modelFactory.GetModel(ctx, roleDef.ID)
	if err != nil {
		return "", fmt.Errorf("get model: %w", err)
	}

	// 构建 prompt：角色定义 + 任务
	domain := ""
	if callReq != nil && callReq.Context != nil {
		if d, ok := callReq.Context["domain"]; ok {
			domain = fmt.Sprintf("%v", d)
		}
	}
	prompt := fmt.Sprintf("你是一个 %s，专长：%s。\n领域：%s\n请完成以下任务：%s",
		roleDef.Name, roleDef.Description, domain, task)

	// 推送 prompt 事件
	if n.progress != nil {
		n.progress(ctx, ProgressEvent{
			SessionID: SessionIDFromContext(ctx),
			Kind:      "prompt",
			Agent:     n.instID,
			Message:   fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", roleDef.Name, model.EstimateTokens(prompt)),
			Detail:    model.SummarizePrompt(prompt, 500),
		})
	}

	// 自适应超时
	hardTimeout := 90 * time.Second
	if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.LLMHardTimeoutSec > 0 {
		hardTimeout = time.Duration(n.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
	}

	// 带超时的 LLM 调用
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := llm.Generate(ctx, prompt)
		ch <- result{text, err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(hardTimeout):
		return "", fmt.Errorf("LLM 硬超时 %v", hardTimeout)
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		// 推送 token_usage 事件
		if n.progress != nil {
			n.progress(ctx, ProgressEvent{
				SessionID: SessionIDFromContext(ctx),
				Kind:      "token_usage",
				Agent:     n.instID,
				Message:   fmt.Sprintf("[%s] LLM 响应 (%d 字符)", roleDef.Name, len(r.text)),
			})
		}
		return r.text, nil
	}
}

// mockResult 生成模拟结果（无 LLM key 时的占位）。
func (n *AssistantNode) mockResult(roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) string {
	contextInfo := ""
	if callReq != nil && callReq.Context != nil {
		if domain, ok := callReq.Context["domain"]; ok {
			contextInfo = fmt.Sprintf("[领域: %v] ", domain)
		}
	}
	return fmt.Sprintf("%s助手[%s]完成任务: %s\n(模拟结果，未接入 LLM)",
		contextInfo, roleDef.Name, task)
}

// CanHandle 检查是否能处理某类任务。
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
