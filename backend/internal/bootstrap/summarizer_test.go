package bootstrap

// summarizer_test.go 覆盖压缩摘要"只保留执行状态"契约（2026-10-24 P1，计划 §5.2）：
// 摘要器不得再被要求生成【已定决策】——那会让模型从工具记录里反推出"用户要求 X"
// 这类从未存在的目标，写进摘要后每轮全文注入（会话 #65/#66 的污染来源之一）。

import (
	"strings"
	"testing"
)

// TestHistorySummarizerPromptHasNoDecisionAuthority 提示词不得要求生成决策，
// 必须显式禁止发明用户目标/决策/验收口径，并保留四节执行状态骨架。
func TestHistorySummarizerPromptHasNoDecisionAuthority(t *testing.T) {
	for _, merge := range []bool{false, true} {
		p := historySummarizerPrompt("工具输出：ReadMedia(...)", merge)
		if strings.Contains(p, "已定决策") {
			t.Fatalf("摘要提示词不应再含【已定决策】(merge=%v):\n%s", merge, p)
		}
		if !strings.Contains(p, "【执行状态】") || !strings.Contains(p, "【产物】") ||
			!strings.Contains(p, "【失败与未验证】") || !strings.Contains(p, "【文件要点】") {
			t.Fatalf("摘要提示词应保留四节执行状态骨架 (merge=%v):\n%s", merge, p)
		}
		if !strings.Contains(p, "不得发明用户目标") {
			t.Fatalf("摘要提示词应显式禁止发明用户决策 (merge=%v):\n%s", merge, p)
		}
	}
	// merge 模式必须提示"已否决方案不得复活"。
	if p := historySummarizerPrompt("旧压缩包", true); !strings.Contains(p, "已否决") {
		t.Fatalf("合并提示词应携带否决纪律:\n%s", p)
	}
}
