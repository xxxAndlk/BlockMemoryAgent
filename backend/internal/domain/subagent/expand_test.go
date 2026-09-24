package subagent

// expand_test.go 展开式召回（TODO #22④）：摘要链注入、答案硬顶 2K、
// 结构性禁递归（展开器工具面只读、无 call_sub_agent）。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// TestBuildExpandTask 验证展开器任务拼装：摘要链 + 查询 + 只读纪律 + 答案硬顶。
func TestBuildExpandTask(t *testing.T) {
	task := buildExpandTask("render.js 的 drawTower 签名", "【摘要链】\n包1: 早期讨论\n")
	if !strings.Contains(task, "【摘要链】") || !strings.Contains(task, "render.js 的 drawTower 签名") {
		t.Fatalf("task must carry chain + query, got:\n%s", task)
	}
	if !strings.Contains(task, "2000") || !strings.Contains(task, "ReadFile") {
		t.Fatalf("task must state answer cap + read tools, got:\n%s", task)
	}
}

// TestExpandMemoryTool_AnswerCapped 验证答案硬顶 2K runes（截答不靠提示词自觉）。
func TestExpandMemoryTool_AnswerCapped(t *testing.T) {
	long := strings.Repeat("细", expandAnswerMaxRune+500)
	got := truncateRunes(long, expandAnswerMaxRune)
	if n := len([]rune(got)); n > expandAnswerMaxRune+20 { // +省略标记余量
		t.Fatalf("answer must be capped at %d runes, got %d", expandAnswerMaxRune, n)
	}
}

// TestExpandMemoryTool_NoRecursion 验证结构性禁递归：展开器工具面只含只读工具，
// call_sub_agent/send_message 不在其中（派不出下级）。
func TestExpandMemoryTool_NoRecursion(t *testing.T) {
	for _, name := range []string{"ReadFile", "SearchInFiles", "ListDir"} {
		if !strings.Contains(expandReadTools, name) {
			t.Fatalf("expandReadTools must include %s, got %s", name, expandReadTools)
		}
	}
	for _, banned := range []string{"call_sub_agent", "send_message", "WriteFile"} {
		if strings.Contains(expandReadTools, banned) {
			t.Fatalf("expandReadTools must NOT include %s (no recursion / read-only)", banned)
		}
	}
}

// TestExpandMemoryTool_EndToEnd 验证端到端：expand_memory 经注册表派发，
// 摘要链回调注入、答案截顶、无 query 拒绝。
func TestExpandMemoryTool_EndToEnd(t *testing.T) {
	d, _, _, _, _, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: strings.Repeat("答", expandAnswerMaxRune+300)})
	d.RegisterExpandTool(toolsReg)
	d.WithExpandChain(func(string) []string { return []string{"包1: 早期约定 drawTower 签名"} })

	res, err := toolsReg.Dispatch(dispatchCtx(), "expand_memory", map[string]any{"query": "drawTower 签名"})
	if err != nil || !res.Success {
		t.Fatalf("expand_memory failed: err=%v res=%+v", err, res)
	}
	if n := len([]rune(res.Output)); n > expandAnswerMaxRune+40 {
		t.Fatalf("answer must be capped (~%d runes), got %d", expandAnswerMaxRune, n)
	}

	// 无 query → 校验拒绝。
	bad, badErr := toolsReg.Dispatch(dispatchCtx(), "expand_memory", map[string]any{})
	if badErr == nil && (bad == nil || bad.Error == "") {
		t.Fatal("empty query must be rejected")
	}
	_ = agent.NopMemoryPipeline{}
}
