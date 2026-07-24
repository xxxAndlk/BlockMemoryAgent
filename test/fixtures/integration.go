//go:build integration

package fixtures

import (
	"os"
	"testing"
)

// IntegrationFixture bundles the common infrastructure for a single integration
// test: ephemeral database, temporary workspace, mock LLM, generated config, and
// the in-process backend server.
type IntegrationFixture struct {
	T      testing.TB
	DB     *TestDatabase
	WS     *TestWorkspace
	LLM    *MockLLMServer
	Config *TestConfigPaths
	Server *TestServer
}

// NewIntegrationFixture creates a full integration-test fixture. It skips the
// test if Postgres/Redis are unavailable. The database is truncated before the
// test runs and again after it finishes.
func NewIntegrationFixture(t testing.TB) *IntegrationFixture {
	t.Helper()
	ws := NewWorkspace(t)
	db := NewTestDatabase(t)
	db.Truncate()
	t.Cleanup(db.Truncate)

	llm := NewMockLLMServer(t)
	cfg := WriteTestConfig(t, db, ws.Root, llm.BaseURL())
	server := NewTestServer(t, cfg)

	return &IntegrationFixture{
		T:      t,
		DB:     db,
		WS:     ws,
		LLM:    llm,
		Config: cfg,
		Server: server,
	}
}

// EnableAssistantSelfTest 在已创建的 fixture 上开启 assistant_self_test_enabled，
// 修改 config.yaml 后重建 TestServer。供验证闭环 e2e 测试使用。
// 必须在创建会话前调用。
func (f *IntegrationFixture) EnableAssistantSelfTest(t testing.TB) {
	t.Helper()
	// 在 config.yaml 中把 assistant_self_test_enabled: false 改为 true。
	data, err := os.ReadFile(f.Config.ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	patched := string(data)
	old := "assistant_self_test_enabled: false"
	new := "assistant_self_test_enabled: true"
	if !contains(patched, old) {
		t.Fatalf("config does not contain %q", old)
	}
	patched = replaceFirst(patched, old, new)
	if err := os.WriteFile(f.Config.ConfigPath, []byte(patched), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// 重建 server 使新配置生效。
	f.Server = NewTestServer(t, f.Config)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}
