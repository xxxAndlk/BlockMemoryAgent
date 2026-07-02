//go:build integration

package coding_test

import (
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestCSSRefactor asserts that a CSS refactoring task routes through
// RouteDirectAssistant when possible.
//
// TODO: register a CSS refactor prompt and inspect the session events to confirm
// the direct-assistant path was chosen.
func TestCSSRefactor(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "CSS refactored.",
	})

	// Seed an existing CSS file for the agent to refactor.
	f.WS.WriteFile("styles.css", []byte(".old { padding: 1px; }"))

	// TODO: create session "refactor styles.css to use tailwind" and assert
	// RouteDirectAssistant is recorded in session events.
	_ = f
}
