package agent

// verify_evidence_test.go 测试 TODO #43 L0 可执行校验的证据扫描：
// HasExecutableVerification（验证类命令成功执行的客观证据）+ RecentVerificationOutputs。

import (
	"testing"
)

func runCommandCall(id, cmd string) ReactMessage {
	return ReactMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "RunCommand", Input: map[string]any{"command": cmd}}}}
}

func toolResultMsg(id string, r ToolResult) ReactMessage {
	return ReactMessage{Role: "tool", ToolCallID: id, Content: ToolResultJSON(r)}
}

func TestHasExecutableVerification_SuccessEvidence(t *testing.T) {
	history := []ReactMessage{
		runCommandCall("c1", "npm run lint"),
		toolResultMsg("c1", ToolResult{Tool: "RunCommand", Success: true, Output: "0 errors"}),
	}
	if !HasExecutableVerification(history) {
		t.Fatal("expected verification evidence found (lint succeeded)")
	}
}

// TestHasExecutableVerification_NodeCheckSyntax 回归 08-13 塔防事故：领域提示词规定
// `node -c <file>` 作 JS 语法检查，但 IsVerificationCommand 旧标记全不命中，
// 证据明明 success=true 却被判 verify_missing。
func TestHasExecutableVerification_NodeCheckSyntax(t *testing.T) {
	history := []ReactMessage{
		runCommandCall("c1", `cd D:\data\project\tower-defense; node -c js/monster.js; node -c js/config.js`),
		toolResultMsg("c1", ToolResult{Tool: "RunCommand", Success: true, Output: "PASS: monster.js\r\nPASS: config.js\r\n"}),
	}
	if !HasExecutableVerification(history) {
		t.Fatal("node -c 成功执行应为验证证据")
	}
}

func TestHasExecutableVerification_FailedCommand(t *testing.T) {
	history := []ReactMessage{
		runCommandCall("c1", "go test ./..."),
		toolResultMsg("c1", ToolResult{Tool: "RunCommand", Success: false, Error: "exit status 1"}),
	}
	if HasExecutableVerification(history) {
		t.Fatal("failed verification command must not count as evidence")
	}
}

func TestHasExecutableVerification_NoVerificationCommand(t *testing.T) {
	history := []ReactMessage{
		runCommandCall("c1", "node build.js"),
		toolResultMsg("c1", ToolResult{Tool: "RunCommand", Success: true, Output: "built"}),
	}
	if HasExecutableVerification(history) {
		t.Fatal("build (non-verification) command must not count as evidence")
	}
}

func TestHasExecutableVerification_NonRunCommandTool(t *testing.T) {
	history := []ReactMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "WriteFile", Input: map[string]any{"path": "a.js"}}}},
		toolResultMsg("c1", ToolResult{Tool: "WriteFile", Success: true}),
	}
	if HasExecutableVerification(history) {
		t.Fatal("WriteFile must not count as verification evidence")
	}
}

func TestHasExecutableVerification_EmptyHistory(t *testing.T) {
	if HasExecutableVerification(nil) {
		t.Fatal("empty history must have no evidence")
	}
}

func TestRecentVerificationOutputs_OrderAndCap(t *testing.T) {
	history := []ReactMessage{
		runCommandCall("c1", "npm run lint"),
		toolResultMsg("c1", ToolResult{Tool: "RunCommand", Success: true, Output: "lint ok"}),
		runCommandCall("c2", "node verify.js"),
		toolResultMsg("c2", ToolResult{Tool: "RunCommand", Success: false, Error: "boom"}),
	}
	outs := RecentVerificationOutputs(history, 3)
	if len(outs) != 2 {
		t.Fatalf("expected 2 outputs, got %d: %v", len(outs), outs)
	}
	if outs[0] == "" || outs[1] == "" {
		t.Fatalf("outputs should be non-empty: %v", outs)
	}
	if len(outs) == 2 && outs[0] == outs[1] {
		t.Fatalf("outputs should differ: %v", outs)
	}
}

func TestHasScreenshotEvidence(t *testing.T) {
	cases := []struct {
		name    string
		history []ReactMessage
		want    bool
	}{
		{
			name: "success screenshot counts",
			history: []ReactMessage{
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "browser_take_screenshot", Input: map[string]any{}}}},
				{Role: "tool", ToolCallID: "c1", Content: `{"success":true,"output":"screenshot saved"}`},
			},
			want: true,
		},
		{
			name: "failed screenshot does not count",
			history: []ReactMessage{
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "browser_take_screenshot", Input: map[string]any{}}}},
				{Role: "tool", ToolCallID: "c1", Content: `{"success":false,"error":"browser not running"}`},
			},
			want: false,
		},
		{
			name: "no screenshot call at all",
			history: []ReactMessage{
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "RunCommand", Input: map[string]any{"command": "node --check a.js"}}}},
				{Role: "tool", ToolCallID: "c1", Content: `{"success":true}`},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasScreenshotEvidence(tc.history); got != tc.want {
				t.Fatalf("HasScreenshotEvidence = %v, want %v", got, tc.want)
			}
		})
	}
}

// probeCall 构造一次浏览器探针工具调用消息（assistant 发起 + tool 结果）。
func probeCall(id, name string, success bool, output string) []ReactMessage {
	tr := ToolResult{Tool: name, Success: success, Output: output}
	return []ReactMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: name, Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: id, Content: ToolResultJSON(tr)},
	}
}

func TestHasRuntimeProbeEvidence(t *testing.T) {
	cases := []struct {
		name    string
		history []ReactMessage
		want    bool
	}{
		{
			name: "full probe sequence passes",
			history: concatHistories(
				probeCall("c1", "browser_navigate", true, "navigated"),
				probeCall("c2", "browser_evaluate", true, `{"fruits":3}`),
				probeCall("c3", "browser_console_messages", true, `[{"level":"log","text":"ready"}]`),
			),
			want: true,
		},
		{
			name: "prefixed plugin names pass (bare-name match)",
			history: concatHistories(
				probeCall("c1", "ui_preview__browser_navigate", true, "navigated"),
				probeCall("c2", "ui_preview__browser_evaluate", true, `{"fruits":3}`),
				probeCall("c3", "ui_preview__browser_console_messages", true, `[{"level":"log","text":"ready"}]`),
			),
			want: true,
		},
		{
			name: "missing navigate fails",
			history: concatHistories(
				probeCall("c2", "browser_evaluate", true, "ok"),
				probeCall("c3", "browser_console_messages", true, `[]`),
			),
			want: false,
		},
		{
			name: "missing evaluate fails",
			history: concatHistories(
				probeCall("c1", "browser_navigate", true, "navigated"),
				probeCall("c3", "browser_console_messages", true, `[]`),
			),
			want: false,
		},
		{
			name: "missing console readback fails",
			history: concatHistories(
				probeCall("c1", "browser_navigate", true, "navigated"),
				probeCall("c2", "browser_evaluate", true, "ok"),
			),
			want: false,
		},
		{
			name: "console with error fails",
			history: concatHistories(
				probeCall("c1", "browser_navigate", true, "navigated"),
				probeCall("c2", "browser_evaluate", true, "ok"),
				probeCall("c3", "browser_console_messages", true, `[{"level":"error","text":"Uncaught ReferenceError: playSwooshSound is not defined"}]`),
			),
			want: false,
		},
		{
			name: "failed navigate does not count",
			history: concatHistories(
				probeCall("c1", "browser_navigate", false, ""),
				probeCall("c2", "browser_evaluate", true, "ok"),
				probeCall("c3", "browser_console_messages", true, `[]`),
			),
			want: false,
		},
		{
			name:    "empty history fails",
			history: nil,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasRuntimeProbeEvidence(tc.history); got != tc.want {
				t.Fatalf("HasRuntimeProbeEvidence = %v, want %v", got, tc.want)
			}
		})
	}
}

func concatHistories(parts ...[]ReactMessage) []ReactMessage {
	var out []ReactMessage
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestHasSceneEvidence_DedupAndAdjacency(t *testing.T) {
	shot := func(id, output string) []ReactMessage {
		return []ReactMessage{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "browser_take_screenshot", Input: map[string]any{}}}},
			{Role: "tool", ToolCallID: id, Content: ToolResultJSON(ToolResult{Tool: "browser_take_screenshot", Success: true, Output: output})},
		}
	}
	// 同图连拍 7 张（字节相同菜单图）→ 去重后 1 张。
	sameShots := concatHistories(
		probeCall("n1", "browser_navigate", true, "ok"),
		shot("s1", "menu.png"), shot("s2", "menu.png"), shot("s3", "menu.png"),
		shot("s4", "menu.png"), shot("s5", "menu.png"), shot("s6", "menu.png"), shot("s7", "menu.png"),
	)
	rep := HasSceneEvidence(sameShots, 3)
	if rep.DistinctShots != 1 || rep.TotalShots != 7 {
		t.Fatalf("dedup failed: distinct=%d total=%d", rep.DistinctShots, rep.TotalShots)
	}
	if !rep.HasAdjacent {
		t.Fatal("navigate → screenshot adjacency should hold")
	}

	// 3 张不同截图 + 逐场景 navigate 邻接 → 通过。
	full := concatHistories(
		probeCall("n1", "browser_navigate", true, "menu"),
		shot("s1", "menu.png"),
		probeCall("n2", "browser_navigate", true, "game"),
		shot("s2", "game.png"),
		probeCall("e1", "browser_evaluate", true, "state=playing"),
		shot("s3", "slicing.png"),
	)
	rep = HasSceneEvidence(full, 3)
	if rep.DistinctShots != 3 || !rep.HasAdjacent {
		t.Fatalf("full coverage failed: %+v", rep)
	}

	// 截图数够但无邻接（纯截图无 navigate/evaluate）→ 邻接失败。
	noAdj := concatHistories(
		shot("s1", "a.png"), shot("s2", "b.png"), shot("s3", "c.png"),
	)
	rep = HasSceneEvidence(noAdj, 3)
	if rep.HasAdjacent {
		t.Fatal("no navigate/evaluate adjacency should fail")
	}

	// 失败截图不计入。
	failed := concatHistories(
		probeCall("n1", "browser_navigate", true, "ok"),
		shot("s1", "browser closed"),
	)
	// shot helper 硬编码 Success=true，这里手工构造失败截图。
	failed = concatHistories(failed[:1],
		[]ReactMessage{{Role: "assistant", ToolCalls: []ToolCall{{ID: "sf", Name: "browser_take_screenshot", Input: map[string]any{}}}}},
		[]ReactMessage{{Role: "tool", ToolCallID: "sf", Content: ToolResultJSON(ToolResult{Tool: "browser_take_screenshot", Success: false, Error: "browser closed"})}},
	)
	rep = HasSceneEvidence(failed, 1)
	if rep.DistinctShots != 0 || rep.TotalShots != 0 {
		t.Fatalf("failed shot should not count: %+v", rep)
	}
}

func TestFilesWrittenFromHistory(t *testing.T) {
	history := []ReactMessage{
		// 成功写入 a.js。
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w1", Name: "WriteFile", Input: map[string]any{"path": "a.js"}}}},
		{Role: "tool", ToolCallID: "w1", Content: ToolResultJSON(ToolResult{Tool: "WriteFile", Success: true})},
		// 失败写入 b.js（不计入）。
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w2", Name: "WriteFile", Input: map[string]any{"path": "b.js"}}}},
		{Role: "tool", ToolCallID: "w2", Content: ToolResultJSON(ToolResult{Tool: "WriteFile", Success: false, Error: "denied"})},
		// 成功编辑 a.js（去重）。
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w3", Name: "EditFile", Input: map[string]any{"path": "a.js"}}}},
		{Role: "tool", ToolCallID: "w3", Content: ToolResultJSON(ToolResult{Tool: "EditFile", Success: true})},
		// 成功写入 c.js。
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w4", Name: "WriteFile", Input: map[string]any{"path": "c.js"}}}},
		{Role: "tool", ToolCallID: "w4", Content: ToolResultJSON(ToolResult{Tool: "WriteFile", Success: true})},
	}
	got := FilesWrittenFromHistory(history)
	if len(got) != 2 || got[0] != "a.js" || got[1] != "c.js" {
		t.Fatalf("expected [a.js c.js], got %v", got)
	}
	// 对照：FilesModifiedFromHistory 不校验成功，应含 b.js。
	mod := FilesModifiedFromHistory(history)
	if len(mod) != 3 {
		t.Fatalf("FilesModifiedFromHistory should include failed write, got %v", mod)
	}
}
