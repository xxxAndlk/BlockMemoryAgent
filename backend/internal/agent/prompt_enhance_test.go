package agent

// prompt_enhance_test.go 验证 TODO #36 Phase 0（纯规则版）：
//   - 意图规则表（续跑/控制/诊断/普通）；
//   - 消歧绑定（失败任务优先，未完成任务次之）；
//   - 只增不改（原文逐字保留在【用户原始指令】段）；
//   - 无状态不编造（仅意图标签）；未命中意图零行为变化。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// TestEnhancePrompt_NoStateNoFabrication 命中意图但无绑定状态（TODO #39 L3 状态交叉验证）：
// 续跑意图无可绑定任务视为误判直通原文（宁漏判不误判）；控制/诊断保留标签不编造绑定。
func TestEnhancePrompt_NoStateNoFabrication(t *testing.T) {
	// 续跑无绑定对象 → 直通（TODO #39：无佐证降级为 IntentNone）。
	orig := "继续"
	if got := EnhancePrompt(orig, EnhanceState{}); got != orig {
		t.Fatalf("resume without binding candidates must pass through verbatim, got: %q", got)
	}
	// 控制无在跑任务 → 仅意图标签，无任务段（不编造绑定对象）。
	got := EnhancePrompt("停止", EnhanceState{})
	if !strings.Contains(got, "意图: 控制") {
		t.Fatalf("control label expected: %q", got)
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

// ---- TODO #39 四层管线测试（L0 闸门 / L1 分级 / L2 仲裁 / L3 防护）----

// TestEnhancePrompt_GateSkipsLongSpec 事故回归（2026-08-11）：数百字多行编号列表
// 任务描述（含"1.继续游戏"）→ 原文直通零附加。
func TestEnhancePrompt_GateSkipsLongSpec(t *testing.T) {
	orig := "给塔防游戏添加新功能：\n1. 粒子特效\n2. 怪物贴图\n3. 暂停菜单，按钮文案「1.继续游戏」「2.重新开始」\n4. 背景音乐\n详细规格见附档。"
	got, note := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{FailedTasks: []EnhanceTask{{Title: "旧任务", Status: "failed"}}}, EnhanceOptions{})
	if got != orig {
		t.Fatalf("long spec must pass through verbatim, got: %q", got)
	}
	if note != "gate_skip" {
		t.Fatalf("gate_skip note expected, got: %q", note)
	}
}

// TestEnhancePrompt_GateSkipsNumberedList 单行编号列表（短文本也拦）：任务描述特征。
func TestEnhancePrompt_GateSkipsNumberedList(t *testing.T) {
	orig := "把暂停菜单加上 1.继续游戏 2.开始游戏 按钮"
	got, _ := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{}, EnhanceOptions{})
	if got != orig {
		t.Fatalf("numbered list must pass through verbatim, got: %q", got)
	}
}

// TestEnhancePrompt_WeakFullMatch 弱词全句匹配高置信直出："继续"→续跑绑定失败任务。
func TestEnhancePrompt_WeakFullMatch(t *testing.T) {
	got, note := EnhancePromptWithOptions(context.Background(), "继续", EnhanceState{
		FailedTasks: []EnhanceTask{{Title: "自检", Status: "failed", Result: "loop guard 三连败"}},
	}, EnhanceOptions{})
	if !strings.Contains(got, "意图: 续跑") || !strings.Contains(got, "自检") {
		t.Fatalf("full-match weak word must bind: %q", got)
	}
	if note != "rule_strong" {
		t.Fatalf("rule_strong note expected, got: %q", note)
	}
}

// TestEnhancePrompt_WeakPrefixFallsBackToRules 弱词句首命中无仲裁器：降级规则结果
// （Phase 0 行为不回归）——"继续执行自检"仍绑定续跑。
func TestEnhancePrompt_WeakPrefixFallsBackToRules(t *testing.T) {
	got, note := EnhancePromptWithOptions(context.Background(), "继续执行自检", EnhanceState{
		FailedTasks: []EnhanceTask{{Title: "自检", Status: "failed", Result: "被杀"}},
	}, EnhanceOptions{})
	if !strings.Contains(got, "意图: 续跑") || !strings.Contains(got, "自检") {
		t.Fatalf("prefix weak word without arbiter must fall back to rules: %q", got)
	}
	if note != "rule_weak" {
		t.Fatalf("rule_weak note expected, got: %q", note)
	}
}

// TestEnhancePrompt_WeakPrefixArbiterOverrides 句首弱词 + 仲裁器：仲裁结果优先。
// "暂停菜单" 是名词短语，仲裁判 none → 直通（旧规则会误判控制）。
func TestEnhancePrompt_WeakPrefixArbiterOverrides(t *testing.T) {
	arb := func(_ context.Context, text string) (IntentKind, error) {
		return IntentNone, nil
	}
	orig := "暂停菜单"
	got, note := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{}, EnhanceOptions{Arbiter: arb})
	if got != orig {
		t.Fatalf("arbiter none must pass through, got: %q", got)
	}
	if note != "llm_miss" {
		t.Fatalf("llm_miss note expected, got: %q", note)
	}
}

// TestEnhancePrompt_ArbiterTimeout 仲裁超时：降级直通，不阻塞输入。
func TestEnhancePrompt_ArbiterTimeout(t *testing.T) {
	arb := func(ctx context.Context, _ string) (IntentKind, error) {
		<-ctx.Done()
		return IntentNone, ctx.Err()
	}
	orig := "继续弄一下"
	got, note := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{}, EnhanceOptions{
		Arbiter:        arb,
		ArbiterTimeout: 50 * time.Millisecond,
	})
	if got != orig {
		t.Fatalf("arbiter timeout must pass through, got: %q", got)
	}
	if note != "llm_timeout" {
		t.Fatalf("llm_timeout note expected, got: %q", note)
	}
}

// TestEnhancePrompt_ArbiterError 仲裁错误：降级直通（回退 L1 规则结果）。
func TestEnhancePrompt_ArbiterError(t *testing.T) {
	arb := func(_ context.Context, _ string) (IntentKind, error) {
		return IntentNone, errors.New("boom")
	}
	orig := "继续弄一下"
	got, note := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{}, EnhanceOptions{Arbiter: arb})
	if got != orig {
		t.Fatalf("arbiter error must pass through, got: %q", got)
	}
	if note != "llm_error" {
		t.Fatalf("llm_error note expected, got: %q", note)
	}
}

// TestEnhancePrompt_ArbiterHitNoStrongSuggestion L2 仲裁命中：给意图标签 + 状态绑定，
// 但不给"优先继续……"强引导建议（防低置信建议带偏 LLM）。
func TestEnhancePrompt_ArbiterHitNoStrongSuggestion(t *testing.T) {
	arb := func(_ context.Context, _ string) (IntentKind, error) {
		return IntentResume, nil
	}
	got, note := EnhancePromptWithOptions(context.Background(), "继续把刚才的活干完", EnhanceState{
		FailedTasks: []EnhanceTask{{Title: "自检", Status: "failed", Result: "被杀"}},
	}, EnhanceOptions{Arbiter: arb})
	if !strings.Contains(got, "意图: 续跑") || !strings.Contains(got, "自检") {
		t.Fatalf("arbiter hit must label and bind: %q", got)
	}
	if strings.Contains(got, "建议:") {
		t.Fatalf("arbiter hit must not emit strong suggestion: %q", got)
	}
	if note != "llm_hit" {
		t.Fatalf("llm_hit note expected, got: %q", note)
	}
}

// TestEnhancePrompt_GatePreservesShortNormalInput 短普通输入（无弱词）零变化直通。
func TestEnhancePrompt_GatePreservesShortNormalInput(t *testing.T) {
	orig := "写个排序函数"
	got, _ := EnhancePromptWithOptions(context.Background(), orig, EnhanceState{}, EnhanceOptions{})
	if got != orig {
		t.Fatalf("short normal input must pass through, got: %q", got)
	}
}
