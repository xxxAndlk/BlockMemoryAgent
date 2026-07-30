package tui

// tree_nodes_test.go 验证 orchestratorNodesToTreeNodes 的 depth 计算与状态映射。

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestOrchestratorNodesToTreeNodes_Depth 验证按 ParentID 链计算 depth:
// MetaAgent(sessionID)的直接子节点 depth=1, 孙节点 depth=2。
func TestOrchestratorNodesToTreeNodes_Depth(t *testing.T) {
	now := time.Now()
	root := "session-1"
	nodes := []orchestrator.Node{
		{ID: "session-1/domain-1", ParentID: root, Role: "domain", Domain: "认证", Task: "做认证", Status: orchestrator.StatusRunning, Started: now},
		{ID: "session-1/domain-1/code_assistant-2", ParentID: "session-1/domain-1", Role: "code_assistant", Task: "写登录", Status: orchestrator.StatusDone, Started: now},
		{ID: "session-1/ui_assistant-3", ParentID: root, Role: "ui_assistant", Task: "改 CSS", Status: orchestrator.StatusFailed, Started: now},
	}
	out := orchestratorNodesToTreeNodes(nodes, root)
	if len(out) != 3 {
		t.Fatalf("应映射 3 节点, got %d", len(out))
	}
	// domain-1 直接子节点 -> depth 1
	if out[0].depth != 1 || out[0].roleType != enums.RoleTypeDomain || out[0].status != enums.RoleStatusActive {
		t.Errorf("domain-1: depth=%d roleType=%v status=%v", out[0].depth, out[0].roleType, out[0].status)
	}
	// code_assistant-2 孙节点 -> depth 2
	if out[1].depth != 2 || out[1].roleType != enums.RoleTypeFixed || out[1].status != enums.RoleStatusDone {
		t.Errorf("code_assistant-2: depth=%d roleType=%v status=%v", out[1].depth, out[1].roleType, out[1].status)
	}
	// ui_assistant-3 直接子节点 -> depth 1, 失败 -> Error
	if out[2].depth != 1 || out[2].status != enums.RoleStatusError {
		t.Errorf("ui_assistant-3: depth=%d status=%v", out[2].depth, out[2].status)
	}
	// instID 与 goal 透传
	if out[1].instID != "session-1/domain-1/code_assistant-2" || out[1].goal != "写登录" {
		t.Errorf("instID/goal 透传错误: instID=%q goal=%q", out[1].instID, out[1].goal)
	}
}

// TestOrchestratorStatusToRole 验证四种树状态映射到 RoleStatus。
func TestOrchestratorStatusToRole(t *testing.T) {
	cases := map[orchestrator.Status]enums.RoleStatus{
		orchestrator.StatusRunning:   enums.RoleStatusActive,
		orchestrator.StatusDone:      enums.RoleStatusDone,
		orchestrator.StatusFailed:    enums.RoleStatusError,
		orchestrator.StatusCancelled: enums.RoleStatusDone, // 终态归 Done
	}
	for s, want := range cases {
		if got := orchestratorStatusToRole(s); got != want {
			t.Errorf("status %v -> %q, want %q", s, got, want)
		}
	}
}
