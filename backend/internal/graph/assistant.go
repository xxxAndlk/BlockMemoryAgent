package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/enums"
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
	name             string             // 节点名（固定 "Assistant"），实现 ThreeLayerNode.Name
	instID           string             // 本实例ID，对应 registry 中的 RoleInstance.ID
	registry         *RoleRegistry      // 角色注册表，查询实例/角色定义、更新状态
	workspace        WorkspaceWriter    // 工作区写入器，持久化 AgentOutput（可能为 nil）
	modelFactory     *model.ModelFactory // LLM 模型工厂，nil 时退回 mock
	rt               *runtime.Runtime   // 运行时聚合体，读取 AgentCfg 超时配置
	progress         ProgressCallback   // 进度回调，推送 prompt / token_usage 事件
	toolCallback     ToolCallback       // 工具执行结果回调（推 UI），启用工具循环
	contextAssembler ContextAssembler   // 上下文组装器，注入私有记忆与全局知识
	llmTracker       *model.LLMCallTracker // LLM 调用追踪器（统计超时/Token，复用 callLLMWithTimeout 模式避免 goroutine 泄漏）
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
		name:       "Assistant",
		instID:     instID,
		registry:   registry,
		workspace:  workspace,
		llmTracker: model.NewLLMCallTracker(), // 初始化追踪器，避免 nil 调用
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

// SetToolCallback 设置工具执行结果回调。
// 启用后 AssistantNode 的 executeTask 走 blades 工具循环（修复原 SubDomain→Assistant 路径无工具的问题）。
func (n *AssistantNode) SetToolCallback(cb ToolCallback) { n.toolCallback = cb }

// SetContextAssembler 注入上下文组装器。
// nil 时 Assistant 退化为原生的角色定义 + 任务 prompt。
func (n *AssistantNode) SetContextAssembler(a ContextAssembler) { n.contextAssembler = a }

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

	// 6. 执行任务 — 优先走 LLM+工具循环，不可用时退回 mock
	result := n.executeTask(ctx, roleDef, task, callReq, state)

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
// executeTask 调用 LLM+工具循环执行任务（不可用时退回 mock）。
//
// 走 CommonExecuteAssistantTask（blades ReAct 工具循环），与 DomainAgent 内联执行路径一致，
// 修复原 SubDomain→Assistant 路径无工具的问题。若注入了上下文组装器，把私有记忆/全局知识
// 作为前缀注入任务文本，避免丢失。
func (n *AssistantNode) executeTask(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest, state *types.ThreeLayerState) string {
	// 有模型工厂时走 LLM + 工具循环
	if n.modelFactory != nil {
		// 可选：注入上下文组装器的私有记忆/全局知识到任务文本前
		effectiveTask := task
		if n.contextAssembler != nil {
			req := &BuildRequest{
				AgentID:   n.instID,
				TopicID:   SessionIDFromContext(ctx),
				TaskQuery: task,
			}
			if callReq != nil {
				req.DependsOn = []string{callReq.CallerID}
			}
			if pack, err := n.contextAssembler.BuildContext(ctx, req); err == nil && pack != nil {
				if prefix := assembleContextPrefix(pack); prefix != "" {
					effectiveTask = prefix + "\n\n" + task
				}
			}
		}
		// 委托公共执行入口：AssistantNode 不启用写文件门控（与原 SubDomain 行为一致）
		result, err := CommonExecuteAssistantTask(ctx, n.modelFactory, n.toolCallback, n.rt,
			roleDef, effectiveTask, state, "", n.progress, roleDef.Name, 0, false)
		if err == nil && result != "" {
			return result
		}
		// 工具循环失败时退回 mock，保证主流程不中断
		if n.progress != nil {
			n.progress(ctx, ProgressEvent{
				SessionID: SessionIDFromContext(ctx),
				Kind:      "error",
				Agent:     n.instID,
				Message:   fmt.Sprintf("LLM+工具执行失败，退回 mock: %v", err),
			})
		}
	}

	// LLM 不可用时退回模拟结果
	return n.mockResult(roleDef, task, callReq)
}

// assembleContextPrefix 把 ContextPack 的消息列表拼成简短上下文前缀（供注入任务文本）。
// 返回空串表示无可注入内容。
func assembleContextPrefix(pack *ContextPack) string {
	if pack == nil || len(pack.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[相关上下文记忆]")
	for _, m := range pack.Messages {
		// 跳过空内容
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		switch m.Role {
		case enums.ChatRoleSystem:
			b.WriteString("\n[系统] " + m.Content)
		case enums.ChatRoleUser:
			b.WriteString("\n[用户] " + m.Content)
		case enums.ChatRoleAssistant:
			b.WriteString("\n[助手] " + m.Content)
		default:
			b.WriteString("\n[" + string(m.Role) + "] " + m.Content)
		}
	}
	return b.String()
}

// callLLM 构建 prompt 并调用 LLM。
// 使用 LLMCallTracker.CallWithTimeout 复用 MetaAgent/DomainAgent 的软/硬超时 + discard 机制，
// 避免 ctx 超时后 goroutine 持续运行导致泄漏（C1）。同时正确记录 token 用于 R1。
func (n *AssistantNode) callLLM(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) (string, error) {
	llm, err := n.modelFactory.GetModel(ctx, roleDef.ID)
	if err != nil {
		return "", fmt.Errorf("get model: %w", err)
	}

	// 构建 prompt：优先使用上下文组装器注入私有记忆 / 全局知识
	var prompt string
	if n.contextAssembler != nil {
		req := &BuildRequest{
			AgentID:   n.instID,
			TopicID:   SessionIDFromContext(ctx),
			TaskQuery: task,
		}
		if callReq != nil {
			req.DependsOn = []string{callReq.CallerID}
		}
		if pack, err := n.contextAssembler.BuildContext(ctx, req); err == nil && pack != nil {
			prompt = formatContextPack(pack, roleDef, task)
		}
	}
	if prompt == "" {
		// 回退到原生 prompt
		domain := ""
		if callReq != nil && callReq.Context != nil {
			if d, ok := callReq.Context["domain"]; ok {
				domain = fmt.Sprintf("%v", d)
			}
		}
		prompt = fmt.Sprintf("你是一个 %s，专长：%s。\n领域：%s\n请完成以下任务：%s",
			roleDef.Name, roleDef.Description, domain, task)
	}

	// 推送 prompt 事件
	caller := roleDef.Name
	if n.progress != nil {
		n.progress(ctx, ProgressEvent{
			SessionID: SessionIDFromContext(ctx),
			Kind:      "prompt",
			Agent:     n.instID,
			Message:   fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)),
			Detail:    model.SummarizePrompt(prompt, 500),
		})
	}

	// 解析软/硬超时：默认 30s/90s，可被 AgentCfg 覆盖
	softTimeout := 30 * time.Second
	hardTimeout := 90 * time.Second
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.LLMSoftTimeoutSec > 0 {
			softTimeout = time.Duration(n.rt.AgentCfg.LLMSoftTimeoutSec) * time.Second
		}
		if n.rt.AgentCfg.LLMHardTimeoutSec > 0 {
			hardTimeout = time.Duration(n.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
		}
	}

	// 通过 tracker 调用：内部用 timeoutCtx + discard goroutine 模式，超时后 goroutine 安全退出
	resp, callErr, timedOut := n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller, softTimeout, hardTimeout)

	// 推送 token_usage 事件（从 tracker 最新记录读取，格式与 MetaAgent/DomainAgent 一致以便 parseTokenUsage 解析）
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1]
		if n.progress != nil {
			n.progress(ctx, ProgressEvent{
				SessionID: SessionIDFromContext(ctx),
				Kind:      "token_usage",
				Agent:     n.instID,
				Message:   fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			})
		}
	}

	if callErr != nil {
		if timedOut {
			return "", fmt.Errorf("LLM 超时: %w", callErr)
		}
		return "", callErr
	}

	// 推送 LLM 响应摘要事件
	if n.progress != nil && resp != "" {
		n.progress(ctx, ProgressEvent{
			SessionID: SessionIDFromContext(ctx),
			Kind:      "llm_response",
			Agent:     n.instID,
			Message:   fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
			Detail:    model.SummarizePrompt(resp, 500),
		})
	}
	return resp, nil
}

// formatContextPack 把 ContextPack 中的消息列表与角色定义、任务拼接成单段 prompt。
// 当前 ChatModel 接口只接受字符串，因此把多段消息按角色顺序拼成文本。
func formatContextPack(pack *ContextPack, roleDef *types.RoleDefinition, task string) string {
	var b strings.Builder
	// 先写入角色定义，作为 system 段补充
	b.WriteString(fmt.Sprintf("你是 %s，专长：%s。\n", roleDef.Name, roleDef.Description))
	// 按消息角色顺序拼接
	for _, m := range pack.Messages {
		switch m.Role {
		case enums.ChatRoleSystem:
			b.WriteString(fmt.Sprintf("[系统] %s\n", m.Content))
		case enums.ChatRoleUser:
			b.WriteString(fmt.Sprintf("[用户] %s\n", m.Content))
		case enums.ChatRoleAssistant:
			b.WriteString(fmt.Sprintf("[助手] %s\n", m.Content))
		default:
			b.WriteString(fmt.Sprintf("[%s] %s\n", m.Role, m.Content))
		}
	}
	// 最后再次明确当前任务
	b.WriteString(fmt.Sprintf("\n请完成以下任务：%s", task))
	return b.String()
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
