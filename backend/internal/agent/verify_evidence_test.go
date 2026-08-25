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
