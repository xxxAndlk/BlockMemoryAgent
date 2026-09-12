package subagent

// domain_name_test.go 覆盖"子 Agent 展示名必须用中文领域名"的校验（2026-09-12 用户实证）：
// domain 会作为子 Agent 对用户可见的展示名出现在对话栏子 Agent 列表、编排页树、面包屑
// 与事件流上，模型填 doc-rev-a 这类英文编号时用户完全读不懂谁在干什么。
//
// 契约是**软着陆**（放行 + 警告进工具结果）而非硬拒：与"一波 9 个领域批量派发"叠加时，
// 硬拒会把整轮派发打成拒绝循环（同日实证 WriteSpec 连续 6 次拒绝已触发 loop guard 强退）。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// TestValidateDispatchArgs_DomainChineseName domain 非中文 → 放行并附提示；中文 → 干净放行。
func TestValidateDispatchArgs_DomainChineseName(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, agent.NopMemoryPipeline{})

	// 英文编号：放行（不能拒，否则批量派发被整轮打回），但必须带中文领域名提示。
	msg, warning := d.validateDispatchArgs("domain", "任务", "职责", "", "", "doc-rev-a")
	if msg != "" {
		t.Fatalf("英文 domain 应软着陆放行, got msg=%q", msg)
	}
	if !strings.Contains(warning, "中文领域名") || !strings.Contains(warning, "doc-rev-a") {
		t.Fatalf("应附中文领域名提示并回显原值, got %q", warning)
	}

	// 中文领域名：无提示。
	if msg, warning := d.validateDispatchArgs("domain", "任务", "职责", "", "", "文档修订-第3章"); msg != "" || warning != "" {
		t.Fatalf("中文 domain 应干净放行, msg=%q warning=%q", msg, warning)
	}

	// 含汉字的混合名（如「第1章-UI」）算中文名。
	if _, warning := d.validateDispatchArgs("domain", "任务", "职责", "", "", "第1章-UI 渲染"); warning != "" {
		t.Fatalf("含汉字应放行, got %q", warning)
	}

	// 非 domain 角色不受此约束（叶子助手没有领域展示名）。
	if _, warning := d.validateDispatchArgs("code_assistant", "任务", "", "", "", "fix-bug"); warning != "" {
		t.Fatalf("固定助手不应触发领域名提示, got %q", warning)
	}
}

// TestValidateDispatchArgs_DomainWarningMergesWithTaskWarning 两条软警告同时命中时合并
// （domain 非中文 + task 轻微超限），不能互相覆盖——覆盖会让模型看不到其中一条。
func TestValidateDispatchArgs_DomainWarningMergesWithTaskWarning(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, agent.NopMemoryPipeline{})
	soft := strings.Repeat("字", 3100) // 超软限未达硬限 → task 软着陆
	msg, warning := d.validateDispatchArgs("domain", soft, "职责", "", "", "doc-rev-b")
	if msg != "" {
		t.Fatalf("应放行, got msg=%q", msg)
	}
	if !strings.Contains(warning, "中文领域名") || !strings.Contains(warning, "runes") {
		t.Fatalf("两条软警告应合并保留, got %q", warning)
	}
}
