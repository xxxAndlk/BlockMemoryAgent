package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRegistrySchema(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	schema := r.Schema()
	if len(schema) < 11 {
		t.Fatalf("expected at least 11 tools, got %d", len(schema))
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "hello.txt"})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	if !strings.Contains(res.Output, "world") {
		t.Fatalf("expected output to contain 'world', got: %s", res.Output)
	}
}

func TestWriteFileProtectedPath(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "backend/foo.go",
		"content": "package foo",
	})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if res.Success {
		t.Fatal("expected write to backend/ to fail")
	}
	if !strings.Contains(res.Error, "受保护") && !strings.Contains(res.Error, "protected") {
		t.Fatalf("expected protected path error, got: %s", res.Error)
	}
}

func TestRunCommandEcho(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "echo hello"
	} else {
		cmd = "echo hello"
	}
	res, err := r.Dispatch(context.Background(), "RunCommand", map[string]any{"command": cmd})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("expected output to contain 'hello', got: %s", res.Output)
	}
}

func TestUnknownTool(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	_, err := r.Dispatch(context.Background(), "NotATool", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got: %v", err)
	}
}
