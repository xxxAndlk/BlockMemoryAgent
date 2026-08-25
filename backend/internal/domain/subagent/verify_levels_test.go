package subagent

// verify_levels_test.go 测试 TODO #67/#68/#69 的 dispatcher 侧判定：
// visualEvidenceCheck 场景化分支、scoreAcceptance 逐项计分、runtime 重试文案。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// shotHist 构造一次成功截图历史（含可选的前置 navigate）。
func shotHist(id, output string, withNav bool) []agent.ReactMessage {
	var hist []agent.ReactMessage
	if withNav {
		hist = append(hist,
			agent.ReactMessage{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: id + "n", Name: "browser_navigate", Input: map[string]any{}}}},
			agent.ReactMessage{Role: "tool", ToolCallID: id + "n", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "browser_navigate", Success: true})},
		)
	}
	hist = append(hist,
		agent.ReactMessage{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: id, Name: "browser_take_screenshot", Input: map[string]any{}}}},
		agent.ReactMessage{Role: "tool", ToolCallID: id, Content: agent.ToolResultJSON(agent.ToolResult{Tool: "browser_take_screenshot", Success: true, Output: output})},
	)
	return hist
}

func TestVisualEvidenceCheck_ScenesBranch(t *testing.T) {
	// scenes 空 → 单截图判定（零行为变化）。
	ok, _ := visualEvidenceCheck(shotHist("s1", "a.png", false), nil)
	if !ok {
		t.Fatal("empty scenes should fall back to single-shot check")
	}
	ok, _ = visualEvidenceCheck(nil, nil)
	if ok {
		t.Fatal("no screenshot with empty scenes should fail")
	}

	// scenes 非空：同图连拍 3 张 < 2 场景要求。
	sameShots := append(shotHist("s1", "menu.png", true), shotHist("s2", "menu.png", true)...)
	ok, msg := visualEvidenceCheck(sameShots, []string{"主菜单", "游玩中"})
	if ok {
		t.Fatal("identical shots should fail scene coverage")
	}
	if !strings.Contains(msg, "同图连拍") {
		t.Fatalf("retry message should explain dedup rule, got %q", msg)
	}

	// 逐场景不同截图 + 邻接 → 通过。
	full := append(shotHist("s1", "menu.png", true), shotHist("s2", "game.png", true)...)
	ok, _ = visualEvidenceCheck(full, []string{"主菜单", "游玩中"})
	if !ok {
		t.Fatal("distinct shots per scene with adjacency should pass")
	}

	// 不同截图但无邻接 → 失败。
	noAdj := append(shotHist("s1", "menu.png", false), shotHist("s2", "game.png", false)...)
	ok, msg = visualEvidenceCheck(noAdj, []string{"主菜单", "游玩中"})
	if ok {
		t.Fatal("no adjacency should fail")
	}
	if !strings.Contains(msg, "邻接") {
		t.Fatalf("retry message should mention adjacency, got %q", msg)
	}
}

func TestRuntimeProbeRetryMessage(t *testing.T) {
	msg := runtimeProbeRetryMessage([]string{"打开主菜单", "断言实体生成"})
	for _, want := range []string{"browser_navigate", "browser_evaluate", "browser_console_messages", "打开主菜单", "断言实体生成"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q: %q", want, msg)
		}
	}
}

func TestScoreAcceptance(t *testing.T) {
	// 命令证据：验证类命令成功。
	cmdHist := []agent.ReactMessage{
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "RunCommand", Input: map[string]any{"command": "npm run lint"}}}},
		{Role: "tool", ToolCallID: "c1", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "RunCommand", Success: true})},
	}
	rec := &parentSpecRecord{}
	// 1 条 command（通过） + 1 条 probe（缺证据） + 1 条 manual（不计分） + 1 条无标记（不计分）。
	sc := scoreAcceptance(cmdHist, []string{
		"跑测试全过 [evidence:command]",
		"页面可打开 [evidence:probe]",
		"纸面标准 [evidence:manual]",
		"无标记存量条目",
	}, rec)
	if sc.total != 2 || sc.passed != 1 {
		t.Fatalf("score mismatch: total=%d passed=%d", sc.total, sc.passed)
	}
	if len(sc.unverified) != 1 || !strings.Contains(sc.unverified[0], "probe") {
		t.Fatalf("unverified list wrong: %v", sc.unverified)
	}
	report := renderAcceptanceScore(sc)
	if !strings.Contains(report, "1/2") {
		t.Fatalf("report should show 1/2: %q", report)
	}

	// quality 层缺证据 → qualityMissing 计数 + 报告标注。
	sc = scoreAcceptance(nil, []string{"特效对标原版 [evidence:screenshot layer:quality]"}, rec)
	if sc.qualityMissing != 1 || sc.qualityEvidence != 1 {
		t.Fatalf("quality missing count wrong: %+v", sc)
	}
	report = renderAcceptanceScore(sc)
	if !strings.Contains(report, "quality") {
		t.Fatalf("report should flag quality layer: %q", report)
	}

	// 纯 manual spec → total=0，报告为空（零行为变化）。
	sc = scoreAcceptance(nil, []string{"纸面一", "纸面二"}, rec)
	if sc.total != 0 || renderAcceptanceScore(sc) != "" {
		t.Fatalf("pure manual spec should not be scored: %+v", sc)
	}

	// file 证据：有写入历史即通过。
	fileHist := []agent.ReactMessage{
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "w1", Name: "WriteFile", Input: map[string]any{"path": "a.js"}}}},
		{Role: "tool", ToolCallID: "w1", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "WriteFile", Success: true})},
	}
	sc = scoreAcceptance(fileHist, []string{"产出文件 [evidence:file]"}, rec)
	if sc.total != 1 || sc.passed != 1 {
		t.Fatalf("file evidence should pass: %+v", sc)
	}
}
