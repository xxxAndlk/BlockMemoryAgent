package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"time"
)

// createAssistantForTask 为指定任务创建助手实例。
//
// 职责：
//   - 优先用 matchFixedAssistant 匹配固定助手（按技能/关键词打分）
//   - 匹配失败则用 factory.CreateAssistant 动态创建
//   - 校验 CanCall 权限
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：父实例
//   - task：任务文本
//
// 返回：助手实例与角色定义；任一环节失败返回 (nil, nil)。
// createAssistantForTask 委托公共实现：为指定任务创建或匹配助手实例。
//
// 保留 emitDetail 调试事件推送（公共函数不感知 UI 事件），其余逻辑走 CommonCreateAssistantForTask。
func (n *DomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	assistantInst, assistantDef := CommonCreateAssistantForTask(ctx, n.registry, n.factory, n.instID, inst, state, task, n.sessionLogger(ctx))
	// 动态创建成功时推送 Agent 创建调试事件，便于 UI 观察动态角色生成
	if assistantInst != nil && assistantDef != nil && assistantDef.Type == enums.RoleTypeDynamic {
		n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 Assistant: %s (任务: %s)", assistantInst.ID, task),
			fmt.Sprintf("instID=%s roleDefID=%s parentID=%s", assistantInst.ID, assistantInst.RoleDefID, n.instID))
	}
	return assistantInst, assistantDef
}

// runAssistant 在当前goroutine中运行助手执行任务。
//
// 职责：标记活跃 → 带 3 次指数退避重试执行 → 标记完成。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：助手实例
//   - def：助手角色定义
//   - task：任务文本
//
// 返回：结构化 AgentResult；失败则返回带 Error 字段的结果（P0-1）。
func (n *DomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) *types.AgentResult {
	// 标记助手活跃
	n.registry.UpdateInstanceStatus(inst.ID, enums.RoleStatusActive)

	var result *types.AgentResult // 任务结果
	var err error                 // 执行错误

	// 重试次数与初始退避：默认 3 次 / 100ms，可被 AgentCfg 覆盖（特性2）
	retryCount := 3
	retryDelay := 100 * time.Millisecond
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.RetryCount > 0 {
			retryCount = n.rt.AgentCfg.RetryCount
		}
		if n.rt.AgentCfg.RetryBackoffMs > 0 {
			retryDelay = time.Duration(n.rt.AgentCfg.RetryBackoffMs) * time.Millisecond
		}
	}
	err = retryWithBackoff(retryCount, retryDelay, func() error {
		// 在闭包内执行助手任务
		result, err = n.executeAssistantTask(ctx, def, task, state)
		return err
	})

	// 标记助手完成
	n.registry.UpdateInstanceStatus(inst.ID, enums.RoleStatusDone)

	// 失败则返回带 Error 字段的结果
	if err != nil {
		return &types.AgentResult{
			SummaryForUser: fmt.Sprintf("[ERROR] 助手[%s]执行失败: %v", def.Name, err),
			MemoryForMeta:  fmt.Sprintf("助手[%s]执行失败: %v", def.Name, err),
			Error:          err.Error(),
		}
	}
	if result == nil {
		result = &types.AgentResult{}
	}
	return result // 返回成功结果
}

// executeAssistantTask 执行助手任务（LLM+工具循环 或 回退模拟）。
//
// 职责：
//   - 优先走 executeAssistantWithTools（blades.Agent + function-calling ReAct 循环）
//   - 工具循环无输出则退化到单次 llm.Generate
//   - 无 modelFactory 则回退到模拟结果
//   - 写文件类任务失败时返回 error 触发上层重试
//
// 参数：
//   - ctx：请求上下文
//   - def：助手角色定义
//   - task：任务文本
//   - state：图全局状态
//
// 返回：结构化 AgentResult 与 error（P0-1）。
//
// 副作用：可能调用工具/写文件/运行命令（由工具循环内部决定）。
func (n *DomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (*types.AgentResult, error) {
	// 取出本 DomainAgent 装配的 Skill 列表（v3 §5），作为 system prompt 的可见技能段
	var skillBrief string
	if n.rt != nil && n.rt.Skills != nil {
		// 取本实例已绑定的 SkillSet
		if set := n.rt.Skills.GetForAgent(n.instID); set != nil {
			// 转为 prompt 可用的技能简介文本
			skillBrief = set.PromptList()
		}
	}
	// 委托公共执行入口：DomainAgent 启用写文件完成门控
	return CommonExecuteAssistantTask(ctx, n.modelFactory, n.toolCallback, n.rt, def, task, state,
		skillBrief, n.progress, "助手["+def.Name+"]", 0, true, n.llmTracker)
}
