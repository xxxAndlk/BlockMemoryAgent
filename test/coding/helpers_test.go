//go:build integration

package coding_test

// 共享辅助：创建会话 + 轮询至终态。供 coding 端到端场景测试复用。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// createSession 用给定 goal 创建会话，返回会话 ID。
func createSession(t testing.TB, f *fixtures.IntegrationFixture, goal string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"goal": goal})
	resp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session unexpected status: %d", resp.StatusCode)
	}
	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	id, _ := sess["id"].(string)
	if id == "" {
		t.Fatalf("missing session id: %+v", sess)
	}
	return id
}

// waitForTerminal 轮询会话直到离开 running/awaiting_clarify 态或超时。
// 返回终态 status 字符串（超时返回 "timeout"）。
func waitForTerminal(t testing.TB, f *fixtures.IntegrationFixture, id string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := sessionStatus(t, f, id)
		if status != "" && status != "running" && status != "awaiting_clarify" {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "timeout"
}

// sessionStatus 取会话当前 status 字段。
func sessionStatus(t testing.TB, f *fixtures.IntegrationFixture, id string) string {
	t.Helper()
	resp, err := http.Get(f.Server.URL() + "/api/sessions/" + id)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var sess map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	s, _ := sess["status"].(string)
	return s
}

// promptContains 报告 LLM 是否收到过包含 substr 的 prompt（大小写不敏感）。
func promptContains(f *fixtures.IntegrationFixture, substr string) bool {
	needle := strings.ToLower(substr)
	for _, p := range f.LLM.RequestPrompts() {
		if strings.Contains(strings.ToLower(p), needle) {
			return true
		}
	}
	return false
}

// toolCall 构造一个 OpenAI 格式的 mock 工具调用。
func toolCall(id, name string, args map[string]any) fixtures.MockToolCall {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	tc := fixtures.MockToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = string(raw)
	return tc
}

// writeFileCall 构造写入会话临时目录的 WriteFile 工具调用（temporary=true）。
func writeFileCall(id, path, content string) fixtures.MockToolCall {
	return toolCall(id, "WriteFile", map[string]any{
		"path":      path,
		"content":   content,
		"temporary": true,
	})
}

// readFileCall 构造一个 ReadFile 工具调用。
func readFileCall(id, path string) fixtures.MockToolCall {
	return toolCall(id, "ReadFile", map[string]any{"path": path})
}
