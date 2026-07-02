//go:build integration

package fixtures

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWorkspace creates a temporary workspace directory for a test. Files
// written here are isolated and cleaned up automatically.
type TestWorkspace struct {
	Root string
	t    testing.TB
}

// NewWorkspace creates a temporary directory under test/tmp. If the directory
// cannot be created, the test fails.
func NewWorkspace(t testing.TB) *TestWorkspace {
	t.Helper()
	root := RepositoryRoot()
	dir := filepath.Join(root, "test", "tmp", t.Name())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove old workspace: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return &TestWorkspace{Root: dir, t: t}
}

// Path joins the workspace root with the given segments.
func (w *TestWorkspace) Path(segments ...string) string {
	return filepath.Join(append([]string{w.Root}, segments...)...)
}

// WriteFile writes data to path under the workspace root.
func (w *TestWorkspace) WriteFile(path string, data []byte) {
	w.t.Helper()
	full := w.Path(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		w.t.Fatalf("write file: %v", err)
	}
}

// ReadFile reads a file from under the workspace root.
func (w *TestWorkspace) ReadFile(path string) []byte {
	w.t.Helper()
	data, err := os.ReadFile(w.Path(path))
	if err != nil {
		w.t.Fatalf("read file: %v", err)
	}
	return data
}
