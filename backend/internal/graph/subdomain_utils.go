package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
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
// 保留 emitDetail 调试事件推送，其余逻辑走 CommonCreateAssistantForTask。
func (n *SubDomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	assistantInst, assistantDef := CommonCreateAssistantForTask(ctx, n.registry, n.factory, n.instID, inst, state, task)
	// 动态创建成功时推送 Agent 创建调试事件，便于 UI 观察动态角色生成
	if assistantInst != nil && assistantDef != nil && assistantDef.Type == enums.RoleTypeDynamic {
		n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 Assistant: %s (任务: %s)", assistantInst.ID, task),
			fmt.Sprintf("instID=%s roleDefID=%s parentID=%s", assistantInst.ID, assistantInst.RoleDefID, n.instID))
	}
	return assistantInst, assistantDef
}

// runAssistant 运行助手执行任务。
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
// 返回：结果文本；失败则返回 [ERROR] 前缀的描述。
func (n *SubDomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) string {
	// 标记助手活跃
	n.registry.UpdateInstanceStatus(inst.ID, enums.RoleStatusActive)

	var result string // 任务结果
	var err error     // 执行错误

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

	// 失败则返回错误信息
	if err != nil {
		return fmt.Sprintf("[ERROR] 助手[%s]执行失败: %v", def.Name, err)
	}
	return result // 返回成功结果
}

// executeAssistantTask 执行助手任务（LLM+工具循环 或 回退模拟）。
//
// 职责：
//   - 优先走 executeAssistantWithTools（blades.Agent + function-calling ReAct 循环）
//   - 工具循环无输出则退化到单次 llm.Generate
//   - 无 modelFactory 则回退到模拟结果
//
// 参数：
//   - ctx：请求上下文
//   - def：助手角色定义
//   - task：任务文本
//   - state：图全局状态
//
// 返回：结果文本与 error。
//
// 副作用：可能调用工具/写文件/运行命令（由工具循环内部决定）。
func (n *SubDomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (string, error) {
	// SubDomainAgent 当前与父 Domain 共享 Skill 子集（通过父 ID 查），
	// skillBrief 暂为空串，工具列表由 executeAssistantWithTools 内部默认值提供。
	// 委托公共执行入口：SubDomainAgent 不启用写文件完成门控（保持原行为）。
	return CommonExecuteAssistantTask(ctx, n.modelFactory, n.toolCallback, n.rt, def, task, state,
		"", n.progress, "SubDomainAgent["+def.Name+"]", 0, false, n.llmTracker)
}

// matchFixedAssistant 匹配固定助手。
//
// 职责：遍历所有固定助手角色定义，按技能/关键词命中打分，返回最高分者。
//
// 参数：
//   - task：任务文本
//
// 返回：匹配的角色定义；最高分 < 10 返回 nil（视为无匹配，转动态创建）。
// matchFixedAssistant 委托公共实现：按技能/关键词打分匹配固定助手。
func (n *SubDomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
	return CommonMatchFixedAssistant(n.registry, task)
}

// summarizeResults 汇总助手结果。
//
// 职责：把当前块的 TaskResults 拼成简短摘要，写入 state.Reason 供上层展示。
//
// 参数：
//   - state：图全局状态（原地修改 state.Reason）
//
// 副作用：修改 state.Reason；每个结果截断到 100 字符。
func (n *SubDomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	// 取本实例
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理，直接返回
		return
	}

	// 取当前块并收集结果摘要
	block := state.ActiveBlocks[state.CurrentBlockID]
	summaries := CommonCollectTaskSummaries(block)

	// 有摘要则拼成一句话写入 state.Reason
	if len(summaries) > 0 {
		// 用分号连接所有摘要
		state.Reason = fmt.Sprintf("子领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}
