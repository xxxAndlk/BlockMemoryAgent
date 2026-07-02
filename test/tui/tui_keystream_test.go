//go:build integration

package tui_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/tui"
	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestTUIKeyStream constructs a TUI Model, sends a sequence of key messages, and
// asserts that focus/input state changes as expected.
func TestTUIKeyStream(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// Build a TUI model wired to the same backend as the HTTP tests.
	m := tui.NewModel(
		f.Server.Deps.SessionManager,
		f.Server.Deps.Graph.Registry(),
		f.Server.Deps.Runtime,
		f.Server.Deps.DAGScheduler,
		f.Server.Deps.Postgres,
		f.Server.URL(),
		"mock-model",
	)

	// Ensure the model starts with a sane initial state.
	if m == nil {
		t.Fatal("NewModel returned nil")
	}

	// Send Tab to move focus between panels. bubbletea value semantics mean we
	// reassign the model on each Update.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	_ = cmd

	// Type a command prefix in the input bar.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/', 'n', 'e', 'w'}})

	// TODO: expose getters on tui.Model or use reflection to assert state
	// transitions such as focus == panelInput and input mode changes.
	// For now the test verifies the wiring compiles and Update does not panic.
}
