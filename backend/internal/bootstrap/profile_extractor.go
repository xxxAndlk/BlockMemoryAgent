package bootstrap

// profile_extractor.go 实现 TODO #28 用户画像的会话完成自动提取：
// 轻量模型扫用户消息提取稳定偏好增量（与 llmFactExtractor 同模式：CallLightweightWithRetry + ParseFactsJSON）。

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
)

// extractProfilePreferences 从用户对话文本中提取 1-3 条稳定偏好。
// 提取失败/非 JSON 返回 error，调用方（ReactService.extractProfilePreferences）零副作用跳过。
func extractProfilePreferences(ctx context.Context, factory *model.ModelFactory, text string) ([]string, error) {
	prompt := fmt.Sprintf(`从以下用户消息中提取 1-3 条稳定的用户偏好（沟通风格/技术栈/确认频率/任务拆解粒度等长期偏好）。
只提取明确、可复用的陈述；一次性的具体任务指令不算偏好；不确定就不要提取。

要求:
- 每条简洁完整，不超过 60 字
- 输出 JSON 字符串数组,如 ["沟通风格: 直接给结论不要铺垫", "技术栈偏好: Go 与 VUE"]
- 不要输出任何其他文本、不要 markdown 围栏

用户消息:
%s`, truncateForPrompt(text, 4000))
	resp, err := factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return subagent.ParseFactsJSON(resp)
}
