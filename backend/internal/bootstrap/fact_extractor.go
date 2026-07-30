package bootstrap

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
)

// llmFactExtractor 用轻量模型从子 Agent 输出中提取关键事实。
// 实现 subagent.FactExtractor 接口，由 bootstrap 注入到 Dispatcher。
type llmFactExtractor struct {
	factory *model.ModelFactory
}

// Extract 调用轻量模型提取 1-5 条关键事实，返回 JSON 数组解析后的字符串切片。
// LLM 返回非 JSON 或解析失败时返回 error，调用方回退到原始文本保存。
func (e *llmFactExtractor) Extract(ctx context.Context, text, goal, roleID string) ([]string, error) {
	prompt := buildFactExtractionPrompt(text, goal, roleID)
	resp, err := e.factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return subagent.ParseFactsJSON(resp)
}

// buildFactExtractionPrompt 构造事实提取提示词。
// 要求 LLM 输出纯 JSON 字符串数组，便于机械解析。
func buildFactExtractionPrompt(text, goal, roleID string) string {
	return fmt.Sprintf(`从以下 Agent 输出中提取 1-5 条关键事实、决策或结论。

要求:
- 只提取可独立成立的事实,不要过程描述或工具调用细节
- 每条事实简洁完整,不超过 80 字
- 优先提取可复用的结论、决策、用户偏好、技术约束
- 输出 JSON 字符串数组,如 ["fact1", "fact2"]
- 不要输出任何其他文本、不要 markdown 围栏

任务目标: %s
Agent 角色: %s
Agent 输出:
%s`, goal, roleID, truncateForPrompt(text, 4000))
}

// truncateForPrompt 截断输入避免超长 prompt。
func truncateForPrompt(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n...(已截断)"
}
