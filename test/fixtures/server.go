//go:build integration

package fixtures

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/blockmemory/agent/backend/internal/testserver"
)

// TestServer wraps an httptest.Server running the full backend handler plus the
// live dependencies returned by testserver.BuildHandler.
type TestServer struct {
	Server  *httptest.Server
	Deps    *testserver.Deps
	cleanup func()
}

// NewTestServer starts the backend in-process on an httptest.Server. It wires
// the same mux as backend/main.go by delegating to internal/testserver.
func NewTestServer(t testing.TB, cfg *TestConfigPaths) *TestServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mux, deps, cleanup, err := testserver.BuildHandler(
		ctx,
		cfg.ConfigPath,
		cfg.RolePath,
		cfg.EnvPath,
		cfg.SoulPath,
		cfg.SkillPath,
	)
	if err != nil {
		cancel()
		t.Fatalf("build test server: %v", err)
	}

	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		cleanup()
		cancel()
	})

	return &TestServer{
		Server: ts,
		Deps:   deps,
		cleanup: func() {
			ts.Close()
			cleanup()
			cancel()
		},
	}
}

// URL returns the base URL of the test server.
func (s *TestServer) URL() string {
	return s.Server.URL
}

// Close shuts down the test server and its dependencies.
func (s *TestServer) Close() {
	s.cleanup()
}
