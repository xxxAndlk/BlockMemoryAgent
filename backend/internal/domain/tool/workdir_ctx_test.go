package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkDirOf_PrefersContext(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	if got := e.workDirOf(context.Background()); got != dirA {
		t.Fatalf("no ctx: got %q want %q", got, dirA)
	}
	ctx := WithWorkDir(context.Background(), dirB)
	if got := e.workDirOf(ctx); got != dirB {
		t.Fatalf("ctx: got %q want %q", got, dirB)
	}
}

func TestResolvePathWithSandbox_PerSessionDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	// 相对路径落到 ctx 目录
	abs, err := e.resolvePathWithSandbox(ctx, "sub/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dirB, "sub", "f.txt"); abs != want {
		t.Fatalf("got %q want %q", abs, want)
	}
	// 默认目录(dirA)外的路径在 dirB 语境下同样受限;dirA 内的文件反而越界
	if _, err := e.resolvePathWithSandbox(ctx, filepath.Join(dirA, "x.txt")); err == nil {
		t.Fatal("want escape error for dirA path under dirB ctx")
	}
}

func TestSessionTempDir_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	got := e.sessionTempDir(ctx, "sess-1")
	if want := filepath.Join(dirB, ".bma", "tmp", "sess-1"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWriteFileLandsInCtxWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	r := NewBuiltinRegistry(dirA, nil, nil)
	ctx := WithWorkDir(context.Background(), dirB)
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "hello.txt", "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && !res.Success {
		t.Fatalf("WriteFile failed: %s", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dirB, "hello.txt")); err != nil {
		t.Fatalf("file not in dirB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "hello.txt")); err == nil {
		t.Fatal("file unexpectedly in dirA")
	}
}
