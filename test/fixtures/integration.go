//go:build integration

package fixtures

import (
	"testing"
)

// IntegrationFixture bundles the common infrastructure for a single integration
// test: ephemeral database, temporary workspace, mock LLM, generated config, and
// the in-process backend server.
type IntegrationFixture struct {
	T        testing.TB
	DB       *TestDatabase
	WS       *TestWorkspace
	LLM      *MockLLMServer
	Config   *TestConfigPaths
	Server   *TestServer
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
