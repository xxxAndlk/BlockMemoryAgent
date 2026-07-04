package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// 本文件实现 Plan-and-Execute 与 Self-Reflection（TODO #1）。
//
// Plan-and-Execute：DomainAgent 在派发助手前，为复杂多任务生成结构化计划（步骤列表），
// 按步骤顺序执行，支持断点续行。失败回退为原 tasks（零回归）。
//
// Self-Reflection：助手执行后用轻量模型评估结果是否达标，不达标则把反馈注入 prompt 重试一次。
// 仅重试一次，防死循环。Feature flag：agent.plan_enabled / agent.reflection_enabled。
//
// 计划类型（ExecutionPlan/PlanStep/PlanStepStatus）定义在 pkg/types，避免 graph ↔ types 循环依赖。

// generatePlan 调重量模型为多任务生成结构化计划。
//
// 解析失败/超时/无模型返回 nil（调用方回退为原 tasks，零回归）。
func (n *DomainAgentNode) generatePlan(ctx context.Context, state *types.ThreeLayerState, tasks []string) *types.ExecutionPlan {
	if n.modelFactory == nil || n.llmTracker.ShouldSkipLLM() {
		return nil
	}
	// 构造拆解 prompt：要求输出 JSON 步骤数组
	prompt := fmt.Sprintf(`你是任务规划专家。请把以下子任务整理成一个有序的执行计划，输出 JSON 数组。

领域: %s
领域目标: %s
子任务列表:
%s

要求:
- 输出严格的 JSON 数组，每个元素形如 {"goal":"步骤目标","tool_hint":"预期工具"}
- goal 必须是一个可直接用工具执行的动作（如"用 WriteFile 写 X"、"用 RunCommand 运行 Y"）
- 按依赖顺序排列（前置产物在前）
- 严禁纯思考类步骤（分析/设计/规划等应融入执行动作）
- 只输出 JSON，不要其他文字

JSON:`, state.CurrentDomain, state.DomainGoal, strings.Join(tasks, "\n"))
	resp, err, _ := n.callLLMAs(ctx, "DomainAgent/计划生成", prompt)
	if err != nil || resp == "" {
		return nil
	}
	// 抽取 JSON 片段
	jsonStr := extractJSON(resp)
	var rawSteps []struct {
		Goal     string `json:"goal"`
		ToolHint string `json:"tool_hint"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &rawSteps); err != nil || len(rawSteps) == 0 {
		return nil // 解析失败：回退
	}
	plan := &types.ExecutionPlan{}
	for _, rs := range rawSteps {
		if strings.TrimSpace(rs.Goal) == "" {
			continue
		}
		plan.Steps = append(plan.Steps, &types.PlanStep{
			Goal:     rs.Goal,
			ToolHint: rs.ToolHint,
			Status:   types.PlanStepPending,
		})
	}
	if len(plan.Steps) == 0 {
		return nil
	}
	n.emit(ctx, "think", fmt.Sprintf("生成计划：%d 步", len(plan.Steps)))
	return plan
}

// reflectOnResult 用轻量模型评估助手执行结果是否达标。
//
// 返回 (ok, feedback)：ok=false 时 feedback 为改进建议，供重试注入 prompt。
// 模型不可用/超时/解析失败时返回 (true, "")（不阻断，视为通过）。
func reflectOnResult(ctx context.Context, modelFactory *model.ModelFactory, task, result string) (bool, string) {
	if modelFactory == nil {
		return true, ""
	}
	prompt := fmt.Sprintf(`你是质量评审员。判断以下任务执行结果是否达标。

任务: %s
执行结果:
%s

判定标准:
- 结果是否完成了任务要求（写文件类是否落盘、查询类是否给出答案）
- 结果是否包含明显的失败标记（如 Error: missing WriteFile result、[ERROR]）
- 结果是否为空或仅是占位模拟

只输出 JSON：{"ok":true/false,"feedback":"若不达标，给出改进建议；达标则留空"}

JSON:`, task, truncateForPrompt(result, 800))
	// P0-1：轻量模型调用统一走 CallLightweightWithRetry（3 次重试）
	resp, err := modelFactory.CallLightweightWithRetry(ctx, prompt)
	if err != nil || resp == "" {
		return true, "" // 轻量模型不可用或调用失败：不阻断
	}
	jsonStr := extractJSON(resp)
	var verdict struct {
		OK       bool   `json:"ok"`
		Feedback string `json:"feedback"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &verdict); err != nil {
		return true, "" // 解析失败：不阻断
	}
	return verdict.OK, verdict.Feedback
}

// truncateForPrompt 截断文本到 maxChars，超长加省略号。
func truncateForPrompt(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...(truncated)"
}

// planEnabledFromRT 从运行时配置读取 Plan 开关（默认关闭，未配置时 false）。
func planEnabledFromRT(rt *runtime.Runtime) bool {
	return rt != nil && rt.AgentCfg != nil && rt.AgentCfg.PlanEnabled
}

// reflectionEnabledFromRT 从运行时配置读取 Reflection 开关。
func reflectionEnabledFromRT(rt *runtime.Runtime) bool {
	return rt != nil && rt.AgentCfg != nil && rt.AgentCfg.ReflectionEnabled
}
