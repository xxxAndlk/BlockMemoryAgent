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

	listBody, _ := json.Marshal(map[string]string{"path": f.WS.Root})
	resp, err := http.Post(f.Server.URL()+"/api/files", "application/json", bytes.NewReader(listBody))
	if err != nil {
		t.Fatalf("POST /api/files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("files list unexpected status: %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode files: %v", err)
	}

	contentBody, _ := json.Marshal(map[string]string{"path": f.WS.Path("test.txt")})
	resp, err = http.Post(f.Server.URL()+"/api/files/content", "application/json", bytes.NewReader(contentBody))
	if err != nil {
		t.Fatalf("POST /api/files/content: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("file content unexpected status: %d", resp.StatusCode)
	}
}
