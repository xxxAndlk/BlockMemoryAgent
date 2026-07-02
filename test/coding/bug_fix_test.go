//go:build integration

package coding_test

import (
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestBugFix verifies that given a bug description, the agent locates and fixes
// code.
//
// TODO: seed a file with a known bug, mock ReadFile/WriteFile tool responses,
// and assert the bug is corrected.
func TestBugFix(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.WS.WriteFile("calc.go", []byte(`package calc

func Add(a, b int) int {
	return a - b // bug
}
`))

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "I see the bug: subtraction should be addition.",
	})

	// TODO: drive session and assert calc.go contains `return a + b`.
	_ = f
}
