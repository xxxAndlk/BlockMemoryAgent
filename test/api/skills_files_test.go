//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSkillsEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	resp, err := http.Get(f.Server.URL() + "/api/skills")
	if err != nil {
		t.Fatalf("GET /api/skills: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("skills unexpected status: %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode skills: %v", err)
	}
	if _, ok := body["skills"]; !ok {
		t.Errorf("skills response missing skills field")
	}
}

func TestFilesEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// Write a test file into the workspace.
	f.WS.WriteFile("test.txt", []byte("hello world"))

	// 列文件：GET /api/files?session=...（ReAct 重构后端点改 GET）。
	// 无 session 创建一个，handler 需要 session 参数。
	createBody, _ := json.Marshal(map[string]string{"goal": "list files"})
	createResp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	var sess map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	createResp.Body.Close()
	sessionID, _ := sess["id"].(string)

	resp, err := http.Get(f.Server.URL() + "/api/files?session=" + sessionID)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("files list unexpected status: %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode files: %v", err)
	}

	contentResp, err := http.Get(f.Server.URL() + "/api/files/content?path=" + f.WS.Path("test.txt"))
	if err != nil {
		t.Fatalf("GET /api/files/content: %v", err)
	}
	defer contentResp.Body.Close()
	if contentResp.StatusCode != http.StatusOK {
		t.Fatalf("file content unexpected status: %d", contentResp.StatusCode)
	}
}
