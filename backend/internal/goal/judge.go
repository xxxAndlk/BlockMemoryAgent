package goal

// judge.go 指令完成度评估器：复用 ModelFactory.CallLightweightWithRetry（轻量模型）
// + workflow/judge.go 的 ```json 围栏解析套路。两个评估点：
//   - goal：每轮终答判定目标是否达成（achieved）；
//   - loop_until：判定循环停止条件是否满足（met）。
// 评委不可用（调用失败/解析失败）按"未达成"处理——goal 多跑一轮无害，
// loop 不会被打折误判停。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// evalResult 是两类判定共用的解析结果。
type evalResult struct {
	Hit    bool   `json:"-"`
	Reason string `json:"-"`
}

// buildGoalEvalPrompt 组装 goal 完成度判定提示词。
func buildGoalEvalPrompt(d *Directive, finalText string) string {
	criterion := strings.TrimSpace(d.CompletionCriterion)
	if criterion == "" {
		criterion = "（未显式给出，按目标本身语义判定）"
	}
	return fmt.Sprintf(`你是目标完成度评估器。一个 Agent 会话正在自主推进目标，本轮刚结束。

【目标】%s
【完成条件】%s
【已推进轮数】%d
【本轮 Agent 终答】
%s

判定目标是否已经达成：完成条件已被满足，或 Agent 明确交付了可验证的最终成果（如"已提交并收到受理号"这类等待外部审核的中间态**不算**达成）。
只输出 JSON，不要其他文字：
{"achieved": true或false, "reason": "一句话依据（≤60字）"}`,
		d.Objective, criterion, d.TurnCount, truncateForJudge(finalText))
}

// buildLoopUntilPrompt 组装 loop 停止条件判定提示词。
func buildLoopUntilPrompt(d *Directive, finalText string) string {
	return fmt.Sprintf(`你是循环执行判定器。一个会话在循环执行指令。

【循环指令】%s
【停止条件】%s
【已执行轮数】%d
【本轮 Agent 终答】
%s

判定停止条件是否已经满足。
只输出 JSON，不要其他文字：
{"met": true或false, "reason": "一句话依据（≤60字）"}`,
		d.Objective, d.LoopUntil, d.TurnCount, truncateForJudge(finalText))
}

// parseEvalOutput 解析判定响应（剥 ```json 围栏 / 截取首尾大括号，workflow/judge 套路）。
// hitKeys 按序尝试（如 ["achieved"] / ["met"]）。
func parseEvalOutput(resp string, hitKeys ...string) (evalResult, error) {
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return evalResult{}, fmt.Errorf("评估响应无可解析 JSON")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return evalResult{}, fmt.Errorf("评估 JSON 解析失败: %w", err)
	}
	res := evalResult{Reason: stringify(raw["reason"])}
	for _, k := range hitKeys {
		if v, ok := raw[k]; ok {
			res.Hit = truthy(v)
			return res, nil
		}
	}
	return evalResult{}, fmt.Errorf("评估 JSON 缺少字段 %v", hitKeys)
}

// truthy 宽容解析 bool / "true" / "yes" / "1"。
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "yes" || s == "1"
	case float64:
		return t != 0
	default:
		return false
	}
}

// stringify 把评估 reason 收敛成单行短文本。
func stringify(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// truncateForJudge 终答过长时截断（评委只需结论段，上限 4000 字符）。
func truncateForJudge(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…（截断）"
}
