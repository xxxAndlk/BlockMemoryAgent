package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
)

// llmDigestMerger 用轻量模型把同波多个子 Agent 的回传汇成一条【整合纪要】。
// 实现 subagent.SummaryMerger 接口，由 bootstrap 注入到 Dispatcher（call_sub_agents
// 波聚合）。失败时调用方回退到逐领域拼接（fail-open），故此处错误直接上抛即可。
type llmDigestMerger struct {
	factory *model.ModelFactory
}

// digestEntryRunes 单领域摘要进 prompt 的截断（纪要输入防爆）。
const digestEntryRunes = 2000

// Merge 调用轻量模型按领域归并多条子 Agent 回传，输出一条 ≤1500 runes 的整合纪要。
func (m *llmDigestMerger) Merge(ctx context.Context, goal string, entries []subagent.DigestEntry) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("no entries")
	}
	resp, err := m.factory.CallLightweightWithRetry(ctx, buildDigestMergePrompt(goal, entries))
	if err != nil {
		return "", fmt.Errorf("call lightweight: %w", err)
	}
	out := strings.TrimSpace(resp)
	if out == "" {
		return "", fmt.Errorf("empty digest")
	}
	return out, nil
}

// buildDigestMergePrompt 构造整合纪要提示词：按领域归并事实、冲突点单列、
// 保留文件清单与验证状态、输出 ≤1500 runes 纯文本。
func buildDigestMergePrompt(goal string, entries []subagent.DigestEntry) string {
	var b strings.Builder
	for _, e := range entries {
		status := "完成"
		if !e.OK {
			status = "失败"
		}
		fmt.Fprintf(&b, "【%s】[%s]\n", e.Domain, status)
		if len(e.Files) > 0 {
			fmt.Fprintf(&b, "修改文件: %s\n", strings.Join(e.Files, ", "))
		}
		fmt.Fprintf(&b, "回传:\n%s\n\n", truncateForPrompt(e.Summary, digestEntryRunes))
	}
	return fmt.Sprintf(`把下列多个子 Agent 的回传整合成一条给上级 Agent 的【整合纪要】。

要求:
- 按领域归并事实与结论：同领域同主题合并，不同领域分段（标题「■ 领域名」）
- 冲突点单列：领域间结论/文件/接口冲突的，在末尾「■ 冲突与风险」段逐条列出，不擅自仲裁
- 保留关键信息：各领域修改文件清单、失败项及原因、未验证项——这些是上级决策的输入
- 丢弃过程叙述与客套，只留决策有用的事实
- 失败的领域保留失败原因与已改文件（返工定位用）
- 纯文本输出，总长 ≤1500 字，不要 markdown 围栏

总体目标: %s

各子 Agent 回传:
%s`, goal, strings.TrimRight(b.String(), "\n"))
}
