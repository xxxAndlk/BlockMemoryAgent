package bootstrap

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
)

// llmSalvageExtractor 用轻量模型从失败子 Agent 输出中提取打捞摘要。
// 实现 subagent.SalvageExtractor 接口（TODO #20 第二层），由 bootstrap 注入到 Dispatcher；
// prompt 面向失败打捞（已读文件清单/已得结论/卡点），与事实提取（llmFactExtractor）不同。
type llmSalvageExtractor struct {
	factory *model.ModelFactory
}

// Extract 调用轻量模型提取 1-5 条打捞条目，返回 JSON 数组解析后的字符串切片。
// LLM 返回非 JSON 或解析失败时返回 error，调用方回退末条 assistant 文本截断。
func (e *llmSalvageExtractor) Extract(ctx context.Context, text, goal, roleID string) ([]string, error) {
	prompt := buildSalvagePrompt(text, goal, roleID)
	resp, err := e.factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return subagent.ParseFactsJSON(resp)
}

// buildSalvagePrompt 构造失败打捞提示词：产出"已读文件清单 + 已得结论 + 卡点"，
// 供父 Agent 与同域重派的新 Agent 基于前序探索成果继续，不重复探索。
func buildSalvagePrompt(text, goal, roleID string) string {
	return fmt.Sprintf(`以下是一个失败的 Agent 任务的输出。提取 1-5 条"打捞摘要"，供重派的新 Agent 接续工作，避免重复探索。

要求:
- 只提取可接续利用的信息，三类：已读文件/已确认的项目事实、已得出的结论与半成品、卡点与失败原因
- 每条简洁完整，不超过 80 字
- 输出 JSON 字符串数组,如 ["已读: config.js 配置项结构", "结论: 渲染引擎用 Canvas 2D 实现", "卡点: index.html 引用路径错误导致脚本未加载"]
- 不要输出任何其他文本、不要 markdown 围栏

Agent 角色: %s
Agent 输出:
%s`, roleID, truncateForPrompt(text, 4000))
}
