//go:build integration

package coding_test

import (
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestSnakeGame is the end-to-end programming scenario: ask the agent to write
// a snake game, run it, and assert that files exist, the executable exits 0,
// and DB records exist.
//
// TODO: wire deterministic mock LLM tool-call responses so the agent reliably
// writes snake_game/main.go and runs `go run`. Until then the test only verifies
// the fixture plumbing compiles and a session can be created.
func TestSnakeGame(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "I'll write a snake game for you.",
	})

	// TODO: drive the session to completion with tool-call mocks and assert:
	//   - snake_game/main.go exists
	//   - `go run snake_game/main.go` exits 0
	//   - session_history row exists
	_ = f
}
