//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSessionTreeEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// 创建会话。
	createBody, _ := json.Marshal(map[string]string{"goal": "tree test"})
	resp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	resp.Body.Close()
	sessionID := sess["id"].(string)

	// GET /tree 应返回 200 + 空树（无子 Agent 派发）。
	treeResp, err := http.Get(f.Server.URL() + "/api/sessions/" + sessionID + "/tree")
	if err != nil {
		t.Fatalf("GET /tree: %v", err)
	}
	defer treeResp.Body.Close()
	if treeResp.StatusCode != http.StatusOK {
		t.Fatalf("tree unexpected status: %d", treeResp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(treeResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	if body["session_id"] != sessionID {
		t.Errorf("session_id = %v, want %s", body["session_id"], sessionID)
	}
	tree, ok := body["tree"].([]any)
	if !ok {
		t.Fatalf("tree field missing or wrong type: %T", body["tree"])
	}
	if len(tree) != 0 {
		t.Errorf("tree len = %d, want 0 (no sub-agents dispatched)", len(tree))
	}

	// POST /agents/{aid}/cancel 不存在的 instID 应返回 404。
	cancelResp, err := http.Post(
		f.Server.URL()+"/api/sessions/"+sessionID+"/agents/nonexistent/cancel",
		"application/json",
		nil,
	)
	if err != nil {
		t.Fatalf("POST /cancel: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel non-existent agent status = %d, want 404", cancelResp.StatusCode)
	}
}
