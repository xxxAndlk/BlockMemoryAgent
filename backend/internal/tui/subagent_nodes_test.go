package tui

import (
	"testing"
)

// TestSubAgentRoleLabelAndID 验证角色标签与 ID 还原工具函数。
func TestSubAgentRoleLabelAndID(t *testing.T) {
	if got := subAgentRoleFromID("session-1/code_assistant-1"); got != "code_assistant" {
		t.Fatalf("got %q", got)
	}
	if got := subAgentRoleFromID("session-12/domain-3"); got != "domain" {
		t.Fatalf("got %q", got)
	}
	if subAgentRoleLabel("domain") != "领域 Agent" {
		t.Fatal("domain 应标记为领域 Agent")
	}
	if subAgentRoleLabel("code_assistant") != "助手" {
		t.Fatal("code_assistant 应标记为助手")
	}
}
