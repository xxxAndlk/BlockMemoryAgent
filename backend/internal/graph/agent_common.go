package graph

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// 本文件为 DomainAgent / SubDomainAgent / MetaAgent / AssistantNode 的公共执行原语。
//
// 设计意图（v3 重构）：消除 domain_agent.go 与 subdomain_agent.go 中逐行复制粘贴的
// createAssistantForTask / executeAssistantTask / matchFixedAssistant / summarizeResults，
// 建立单一执行入口，让 MetaAgent 也能"执行工具"与"调用助手"，让 AssistantNode 也能跑工具循环。
//
// 纯迁移为主：行为与原 DomainAgent 版本一致；差异点（skillBrief / agentName / 写文件门控）
// 通过参数显式传递，调用方按需取值。

// CommonMatchFixedAssistant 按技能/关键词打分匹配固定助手角色。
//
// 职责：遍历注册表中的助手角色定义，仅考虑固定角色，按技能命中(+10)/关键词命中(+5)
//
//	打分，取最高分；阈值 10（至少一个技能命中）才视为有效匹配。
//
// 参数：
//   - registry：角色注册表。
//   - task：任务文本（大小写不敏感匹配）。
//
// 返回：最佳匹配的 RoleDefinition；无有效匹配返回 nil。
//
// 并发安全：纯读 registry（registry 内部自带锁）。
func CommonMatchFixedAssistant(registry *RoleRegistry, task string) *types.RoleDefinition {
	if registry == nil {
		return nil
	}
	// 任务文本转小写，做大小写不敏感匹配
	taskLower := strings.ToLower(task)
	var bestMatch *types.RoleDefinition
	bestScore := 0

	// 遍历所有助手角色定义
	for _, def := range registry.GetAssistantRoleDefs() {
		// 只考虑固定角色（动态角色不参与匹配）
		if def.Type != enums.RoleTypeFixed {
			continue
		}
		score := 0
		// 技能命中：+10 分
		for _, skill := range def.Skills {
			// 任务文本包含技能关键词则加分
			if strings.Contains(taskLower, strings.ToLower(skill)) {
				score += 10
			}
		}
		// 关键词命中：+5 分
		for _, kw := range def.Keywords {
			// 任务文本包含角色关键词则加分
			if strings.Contains(taskLower, strings.ToLower(kw)) {
				score += 5
			}
		}
		// 更新最高分
		if score > bestScore {
			bestScore = score // 更新最高分
			bestMatch = def   // 记录最佳匹配
		}
	}

	// 阈值 10：至少一个技能命中才视为有效匹配
	if bestScore >= 10 {
		return bestMatch // 返回最佳匹配
	}
	return nil // 无有效匹配
}

// CommonCreateAssistantForTask 为指定任务创建助手实例。
//
// 职责：
//   - 优先用 CommonMatchFixedAssistant 匹配固定助手（按技能/关键词打分）
//   - 匹配失败则用 factory.CreateAssistant 动态创建
//   - 校验 CanCall 权限
//
// 参数：
//   - ctx：请求上下文。
//   - registry：角色注册表。
//   - factory：动态角色工厂（创建 Assistant）。
//   - callerInstID：调用者实例 ID（用于 CanCall 权限校验与父实例绑定）。
//   - parentInst：父实例（提供 Domain / RoleDefID）；可为 nil（MetaAgent 直接调用时无父领域）。
//   - state：图全局状态（取 SessionID）。
//   - task：任务文本。
//
// 返回：助手实例与角色定义；任一环节失败返回 (nil, nil)。
func CommonCreateAssistantForTask(
	ctx context.Context,
	registry *RoleRegistry,
	factory *RoleFactory,
	callerInstID string,
	parentInst *types.RoleInstance,
	state *types.ThreeLayerState,
	task string,
) (*types.RoleInstance, *types.RoleDefinition) {
	if registry == nil || factory == nil || state == nil {
		return nil, nil
	}
	// 父领域名：parentInst 可能为 nil（MetaAgent 直接派助手时），此时领域为空
	parentDomain := ""
	parentDefID := ""
	if parentInst != nil {
		parentDomain = parentInst.Domain
		parentDefID = parentInst.RoleDefID
	}

	// 1. 先尝试匹配固定助手
	assistantDef := CommonMatchFixedAssistant(registry, task)
	if assistantDef != nil {
		// 权限校验：本实例是否可调用该角色
		if !registry.CanCall(callerInstID, assistantDef.ID) {
			return nil, nil // 无权限
		}
		// 创建固定助手实例
		assistantInst, err := registry.CreateInstance(assistantDef.ID, state.SessionID, parentDomain, callerInstID)
		if err != nil {
			// 创建失败：打印日志
			log.Printf("[Common] create fixed assistant %s failed: %v", assistantDef.ID, err)
			return nil, nil
		}
		return assistantInst, assistantDef // 返回固定助手
	}

	// 2. 动态创建助手（由 LLM 推断角色定义）
	assistantInst, err := factory.CreateAssistant(ctx, state.SessionID, task, callerInstID, parentDefID)
	if err != nil {
		// 创建失败：打印日志
		log.Printf("[Common] create dynamic assistant for %q failed: %v", task, err)
		return nil, nil
	}

	// 3. 取出动态创建的角色定义
	assistantDef = registry.GetRoleDef(assistantInst.RoleDefID)
	if assistantDef == nil {
		return nil, nil // 角色定义缺失
	}
	// 权限校验
	if !registry.CanCall(callerInstID, assistantDef.ID) {
		return nil, nil // 无权限
	}
	return assistantInst, assistantDef
}

// CommonExecuteAssistantTask 助手任务执行的单一入口（LLM+工具循环 或 回退）。
//
// 职责：
//   - 优先走 executeAssistantWithTools（blades.Agent + function-calling ReAct 循环）
//   - 工具循环无输出则退化到单次 llm.Generate
//   - 无 modelFactory 则回退到模拟结果
//   - enforceWriteGate=true 时：写文件类任务无成功 WriteFile 记录返回 error 触发上层重试
//
// 参数：
//   - ctx：请求上下文。
//   - modelFactory：模型工厂；nil 时回退模拟。
//   - toolCallback：工具执行结果回调（推 UI）；可为 nil。
//   - rt：运行时聚合（取 AgentCfg 的 ToolCallMaxRounds）；可为 nil。
//   - def：助手角色定义（提供 SystemPrompt / ID）。
//   - task：任务文本。
//   - state：图全局状态（注入领域上下文与 sessionID）。
//   - skillBrief：已装配技能简介文本，注入 system prompt；可为空。
//   - progress：进度回调；可为 nil。
//   - agentName：当前 agent 名，用于事件归属（如 "助手[X]" / "MetaAgent"）。
//   - maxIters：ReAct 循环最大轮数（<=0 时使用默认 12）。
//   - enforceWriteGate：是否启用写文件完成门控。
//
// 返回：结构化 AgentResult 与 error（P0-1）。
func CommonExecuteAssistantTask(
	ctx context.Context,
	modelFactory *model.ModelFactory,
	toolCallback ToolCallback,
	rt *runtime.Runtime,
	def *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
	skillBrief string,
	progress ProgressCallback,
	agentName string,
	maxIters int,
	enforceWriteGate bool,
	llmTracker *model.LLMCallTracker,
) (*types.AgentResult, error) {
	// 1. 优先使用 LLM + 工具执行
	if modelFactory != nil {
		// 新建工具执行器并注入回调
		executor := NewToolExecutor("")
		if toolCallback != nil {
			executor.SetCallback(toolCallback)
		}

		// 解析 ReAct 循环最大轮数：rt 配置覆盖显式传入值（<=0 时取配置，仍 <=0 取默认 12）
		if rt != nil && rt.AgentCfg != nil && rt.AgentCfg.ToolCallMaxRounds > 0 {
			maxIters = rt.AgentCfg.ToolCallMaxRounds
		}

		// 调用 blades.Agent + 工具循环执行
		result, _ := executeAssistantWithTools(ctx, modelFactory, executor, def, task, state, skillBrief, progress, agentName, maxIters, llmTracker)
		if result != nil && result.Error == "" && result.SummaryForUser != "" {
			// Self-Reflection（TODO #1）：启用时评估结果，不达标则带反馈重试一次
			if reflectionEnabledFromRT(rt) {
				if ok, feedback := reflectOnResult(ctx, modelFactory, task, result.SummaryForUser); !ok && feedback != "" {
					if progress != nil {
						progress(ctx, ProgressEvent{
							SessionID: sessionIDFromState(state),
							Kind:      "reflect",
							Agent:     agentName,
							Message:   fmt.Sprintf("反思判定不达标，带反馈重试: %s", truncateForPrompt(feedback, 200)),
						})
					}
					// 把反馈注入任务文本前缀后重试一次（防死循环：仅一次）
					retryTask := fmt.Sprintf("[上次结果未达标，改进建议: %s]\n\n%s", feedback, task)
					if r2, _ := executeAssistantWithTools(ctx, modelFactory, executor, def, retryTask, state, skillBrief, progress, agentName, maxIters, llmTracker); r2 != nil && r2.Error == "" && r2.SummaryForUser != "" {
						result = r2
					}
				}
			}
			// 完成门控：若任务要求写文件但结果含失败标记，返回 error 触发上层重试/告警
			if enforceWriteGate && strings.HasPrefix(result.SummaryForUser, "[失败:") {
				return result, fmt.Errorf("助手未完成写文件任务: %s", task)
			}
			return result, nil // 成功返回
		}

		// 工具执行回退到普通 LLM（单次生成，无工具）
		llm, err := modelFactory.GetModel(ctx, def.ID)
		if err == nil {
			// 拼 prompt：角色 system prompt + 当前任务 + 领域目标
			domainGoal := ""
			if state != nil {
				domainGoal = state.DomainGoal
			}
			prompt := fmt.Sprintf("%s\n\n当前任务: %s\n领域目标: %s\n请执行任务并返回结果。",
				def.SystemPrompt, task, domainGoal)
			fbStart := time.Now()
			resp, err := llm.Generate(ctx, prompt)
			// P0-4：回退路径也记录一次调用并推送 token_usage 事件
			inTok := model.EstimateTokens(prompt)
			if inTok == 0 {
				inTok = 1
			}
			outTok := model.EstimateTokens(resp)
			if outTok == 0 {
				if err != nil {
					outTok = model.EstimateTokens(err.Error())
				}
				if outTok == 0 {
					outTok = 1
				}
			}
			fbDur := time.Since(fbStart)
			if llmTracker != nil {
				llmTracker.RecordCall(ctx, fbDur, err, agentName,
					model.SummarizePrompt(prompt, 500), prompt, resp, inTok, outTok, false)
			}
			if progress != nil {
				progress(ctx, ProgressEvent{
					SessionID: sessionIDFromState(state),
					Kind:      "token_usage",
					Agent:     agentName,
					Message:   fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, fbDur.Round(time.Millisecond)),
				})
			}
			if err == nil && resp != "" {
				return &types.AgentResult{
					SummaryForUser: resp,
					MemoryForMeta:  resp,
				}, nil // 成功返回
			}
		}
	}

	// 2. 回退到模拟结果（无 modelFactory 或上述路径都失败）
	contextInfo := ""
	if state != nil && state.CurrentDomain != "" {
		// 拼接领域前缀
		contextInfo = fmt.Sprintf("[领域: %s] ", state.CurrentDomain)
	}
	// 返回模拟结果
	mockText := fmt.Sprintf("%s助手[%s]完成任务: %s", contextInfo, def.Name, task)
	return &types.AgentResult{
		SummaryForUser: mockText,
		MemoryForMeta:  mockText,
	}, nil
}

// sessionIDFromState 从 state 取 SessionID（nil 安全）。
func sessionIDFromState(state *types.ThreeLayerState) string {
	if state == nil {
		return ""
	}
	return state.SessionID
}

// CommonCollectTaskSummaries 收集块内任务结果摘要（共享子过程）。
//
// 职责：遍历 block.TaskResults，每项截断到 100 字符，拼成 "任务: 结果" 格式。
// DomainAgent / SubDomainAgent 的 summarizeResults 共用此函数收集摘要，
// 各自再决定是否归档块记忆/领域归档。
//
// 参数：
//   - block：当前会话块；可为 nil。
//
// 返回：摘要字符串切片（无结果时为空切片）。
func CommonCollectTaskSummaries(block *types.SessionBlock) []string {
	var summaries []string
	if block == nil || block.TaskResults == nil {
		return summaries
	}
	for task, result := range block.TaskResults {
		// 结果过长则截断到 100 字符
		shortResult := result
		if len(shortResult) > 100 {
			// 截断并加省略号
			shortResult = shortResult[:100] + "..."
		}
		// 拼成 "任务: 结果" 格式
		summaries = append(summaries, fmt.Sprintf("%s: %s", task, shortResult))
	}
	return summaries
}
