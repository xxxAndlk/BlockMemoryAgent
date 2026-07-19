package tui

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestDeriveSubAgentNodes 验证从会话事件流派生子 Agent 节点：
// 派发创建运行中节点、tool_exec 回填 ID、sub_agent_done 标记完成。
func TestDeriveSubAgentNodes(t *testing.T) {
	now := time.Now()
	events := []server.SessionEvent{
		// 派发 code_assistant。
		{Type: "message", Kind: "sub_agent_dispatch", Tool: "code_assistant", Message: "写测试", Timestamp: now},
		// 派发 domain。
		{Type: "message", Kind: "sub_agent_dispatch", Tool: "domain", Message: "通用任务", Timestamp: now},
		// 两个 tool_exec 回填 ID（顺序对应）。
		{Type: "tool_exec", Tool: "call_sub_agent", ToolPath: "session-1/code_assistant-1", Success: true, Timestamp: now},
		{Type: "tool_exec", Tool: "call_sub_agent", ToolPath: "session-1/domain-2", Success: true, Timestamp: now},
		// code_assistant 完成。
		{Type: "message", Kind: "sub_agent_done", Message: "session-1/code_assistant-1", Timestamp: now},
	}

	nodes := deriveSubAgentNodes(events)
	if len(nodes) != 2 {
		t.Fatalf("应派生 2 个节点, got %d", len(nodes))
	}
	ca, dm := nodes[0], nodes[1]
	if ca.instID != "session-1/code_assistant-1" || ca.status != enums.RoleStatusDone {
		t.Fatalf("code_assistant 应为已完成且带 ID, got id=%q status=%v", ca.instID, ca.status)
	}
	if ca.roleType != enums.RoleTypeFixed {
		t.Fatalf("code_assistant 应为固定角色, got %v", ca.roleType)
	}
	if dm.instID != "session-1/domain-2" || dm.status != enums.RoleStatusActive {
		t.Fatalf("domain 应为运行中且带 ID, got id=%q status=%v", dm.instID, dm.status)
	}
	if dm.roleType != enums.RoleTypeDomain {
		t.Fatalf("domain 应为领域角色, got %v", dm.roleType)
	}
	if ca.goal != "写测试" {
		t.Fatalf("节点应携带任务摘要, got %q", ca.goal)
	}
}

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
