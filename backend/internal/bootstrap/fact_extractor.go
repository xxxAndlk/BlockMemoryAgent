package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// llmFactExtractor 用轻量模型从子 Agent 输出中提取关键事实。
// 实现 subagent.FactExtractor 接口，由 bootstrap 注入到 Dispatcher。
type llmFactExtractor struct {
	factory  *model.ModelFactory
	maxFacts int // 单次提取条数上限（config agent.block_memory_facts_max，缺省 5）
}

// Extract 调用轻量模型提取 1-maxFacts 条关键事实，返回 JSON 数组解析后的字符串切片。
// LLM 返回非 JSON 或解析失败时返回 error，调用方回退到原始文本保存。
func (e *llmFactExtractor) Extract(ctx context.Context, text, goal, roleID string) ([]string, error) {
	prompt := buildFactExtractionPrompt(text, goal, roleID, e.factsMax())
	resp, err := e.factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	facts, err := subagent.ParseFactsJSON(resp)
	if err != nil {
		return nil, err
	}
	return filterJunkFacts(facts), nil
}

// filterJunkFacts 丢弃明显无召回价值的条目（失败标记原文等）。
// 2026-09-16 实证：存量块记忆混入 "[failure kind=killed retryable=false] ..." 一类
// 失败通知原文（打捞/回退路径），对后续任务召回纯噪声。
func filterJunkFacts(facts []string) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" || strings.HasPrefix(f, "[failure") || strings.Contains(f, "failure kind=") {
			continue
		}
		// 召回循环防护（TODO #22③）：被召回内容已围栏标记（WrapUntrusted），带围栏的
		// 事实不反向沉淀为新记忆——防"召回→转述→再沉淀"无限循环与 untrusted 入 curated 层。
		if agent.ContainsUntrustedFence(f) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// factsMax 提取条数上限（<=0 回落 5）。
func (e *llmFactExtractor) factsMax() int {
	if e.maxFacts <= 0 {
		return 5
	}
	return e.maxFacts
}

// factPromptHeadRunes / factPromptTailRunes 提取输入的首/尾保留量：
// 子 Agent 长输出常常"过程在头、结论在尾"，旧实现只裁尾部 4000 字会丢掉最终
// 结论/未验证项（2026-09-16）。改为头上+尾上拼接，中间省略。
const (
	factPromptHeadRunes = 4000
	factPromptTailRunes = 2000
)

// buildFactExtractionPrompt 构造事实提取提示词。
// 核心判据是**跨任务复用价值**（2026-09-16 收紧）：块记忆在派发时按语义召回注入
// 未来子 Agent，存量实证显示模型易把"本次任务收尾状态"当事实抽（测试通过/未修改
// X 文件/端口已关闭），这类快照换任务即失效，会稀释召回精度。故提示词同时给
// 收录侧与排除侧清单 + 反例，并要求"没有就输出空数组"（宁缺勿滥）。
// 输出纯 JSON 字符串数组，便于机械解析。
func buildFactExtractionPrompt(text, goal, roleID string, maxFacts int) string {
	return fmt.Sprintf(`从以下 Agent 输出中提取 0-%d 条**跨任务可复用**的事实。

判据（逐条自问：换个不同任务，这条还有用吗？没用就丢掉）:
- 收录: 项目结构/文件与模块职责；接口与数据格式契约；配置参数与取值；环境与外部约束；
  踩过的坑与绕过办法；用户偏好与约定；工具/命令的有效用法（含失败命令的正确替代）
- 排除: 本次任务的完成/验证状态（如"测试全部通过""构建成功"）；文件改动清单（如"修改了 X/Y"）；
  "未修改某文件"声明；进程/端口/临时文件清理声明；未验证项罗列；失败通知原文（"[failure ...]"）；过程叙述
- 反例: "本次修改的验证测试全部通过，含 33/33 单元测试" → 不提取（换任务即失效）
- 正例: "sprite 资产脚本需先生成 palette.json 再跑 extract_assets.py，否则贴图缺失" → 提取（可复用工艺）
- 只提取**本次改动或明确决策**产生的信息；文件清单、行数、用例计数等过程统计不提取
- 每条独立成立、简洁完整、不超过 80 字；不写"本次/本任务"等指代，写清对象名
- 没有可复用事实时输出空数组 []（纯验证/检查任务通常就是空）
- 输出 JSON 字符串数组，如 ["fact1", "fact2"]；不要输出任何其他文本、不要 markdown 围栏

任务目标: %s
Agent 角色: %s
Agent 输出:
%s`, maxFacts, goal, roleID, truncateHeadTail(text, factPromptHeadRunes, factPromptTailRunes))
}

// truncateHeadTail 超长时保留头部 head 个 rune + 尾部 tail 个 rune（textutil 单源）。
func truncateHeadTail(s string, head, tail int) string {
	return textutil.TruncateHeadTail(s, head, tail)
}

// truncateForPrompt 截断输入避免超长 prompt（textutil 单源，保省略后缀口径）。
func truncateForPrompt(s string, max int) string {
	return textutil.TruncateRunes(s, max, "\n...(已截断)")
}
