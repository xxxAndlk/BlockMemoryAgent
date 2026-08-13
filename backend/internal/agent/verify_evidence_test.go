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
