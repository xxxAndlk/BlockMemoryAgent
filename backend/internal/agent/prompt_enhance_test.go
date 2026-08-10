package agent

// prompt_enhance_test.go 验证 TODO #36 Phase 0（纯规则版）：
//   - 意图规则表（续跑/控制/诊断/普通）；
//   - 消歧绑定（失败任务优先，未完成任务次之）；
//   - 只增不改（原文逐字保留在【用户原始指令】段）；
//   - 无状态不编造（仅意图标签）；未命中意图零行为变化。

import (
	"strings"
	"testing"
)

// TestClassifyIntent 意图规则表：续跑/控制/诊断/普通四类命中断言。
func TestClassifyIntent(t *testing.T) {
	cases := []struct {
		text string
		kind IntentKind
	}{
		{"继续", IntentResume},
		{"重新执行", IntentResume},
		{"接着做塔防", IntentResume},
		{"从头再来一次", IntentResume},
		{"重跑一遍", IntentResume},
		{"停止", IntentControl},
		{"取消当前任务", IntentControl},
		{"暂停一下", IntentControl},
		{"为什么失败了", IntentDiagnose},
		{"查一下日志找原因", IntentDiagnose},
		{"检查一下配置", IntentDiagnose},
		{"帮我把 config.js 宽度改为 1280", IntentNone},
		{"写个排序函数，验收 O(n log n)", IntentNone},
		{"你好", IntentNone},
	}
	for _, c := range cases {
		kind, _ := classifyIntent(c.text)
		if kind != c.kind {
			t.Errorf("classifyIntent(%q) = %v, want %v", c.text, kind, c.kind)
		}
	}
}

// TestEnhancePrompt_NoneIntentZeroChange 未命中意图：原样返回（零行为变化）。
func TestEnhancePrompt_NoneIntentZeroChange(t *testing.T) {
	orig := "帮我把 config.js 宽度改为 1280"
	got := EnhancePrompt(orig, EnhanceState{BoardGoal: "做塔防"})
	if got != orig {
		t.Fatalf("non-intent input must pass through verbatim, got: %q", got)
	}
}

// TestEnhancePrompt_BindsFailedTask 续跑意图 + 失败任务：输出两段式模板，
// 原文逐字保留、绑定段含失败任务标题与原因、建议指向续跑失败任务。
func TestEnhancePrompt_BindsFailedTask(t *testing.T) {
	orig := "重新执行"
	got := EnhancePrompt(orig, EnhanceState{
		BoardGoal: "自检任务",
		FailedTasks: []EnhanceTask{
			{Title: "自检", Domain: "自检", Status: "failed", Result: "loop guard 三连败终止"},
		},
	})
	if !strings.Contains(got, "【用户原始指令】\n"+orig) {
		t.Fatalf("original text must be preserved verbatim: %q", got)
	}
	if !strings.Contains(got, "【系统补全】") {
		t.Fatalf("missing 系统补全 section: %q", got)
	}
	if !strings.Contains(got, "意图: 续跑") {
		t.Fatalf("missing intent label: %q", got)
	}
	if !strings.Contains(got, "自检") || !strings.Contains(got, "loop guard 三连败终止") {
		t.Fatalf("failed task must be bound with reason: %q", got)
	}
	if !strings.Contains(got, "不要重跑已完成") {
		t.Fatalf("suggestion should warn against full rerun: %q", got)
	}
}

// TestEnhancePrompt_NoStateNoFabrication 命中意图但无绑定状态：
// 仅意图标签，无任务段（不编造绑定对象）。
func TestEnhancePrompt_NoStateNoFabrication(t *testing.T) {
	got := EnhancePrompt("继续", EnhanceState{})
	if !strings.Contains(got, "意图: 续跑") {
		t.Fatalf("intent label expected: %q", got)
	}
	if strings.Contains(got, "建议:") {
		t.Fatalf("no state should not fabricate suggestions: %q", got)
	}
	if strings.Contains(got, "失败") {
		t.Fatalf("no state should not mention failures: %q", got)
	}
}

// TestEnhancePrompt_PendingBinding 无失败任务但有未完成任务：绑定未完成清单。
func TestEnhancePrompt_PendingBinding(t *testing.T) {
	got := EnhancePrompt("继续做", EnhanceState{
		PendingTasks: []EnhanceTask{
			{Title: "渲染引擎", Domain: "渲染"},
			{Title: "游戏逻辑", Domain: "逻辑"},
		},
	})
	if !strings.Contains(got, "渲染引擎") || !strings.Contains(got, "游戏逻辑") {
		t.Fatalf("pending tasks should be listed: %q", got)
	}
	if !strings.Contains(got, "未完成任务") {
		t.Fatalf("missing pending section: %q", got)
	}
}
