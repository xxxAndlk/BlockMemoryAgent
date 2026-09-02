package tool

// shared_memory_workdir_test.go 验证 .bma 状态存储按 ctx 会话工作目录解析（S2）：
// ctx 注入会话目录时共享记忆根落在 <会话目录>/.bma/shared，未注入时回落构造目录。

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSharedMemoryRoot_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	s := NewFileSharedMemoryStore(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	if got, want := s.root(ctx), filepath.Join(dirB, ".bma", "shared"); got != want {
		t.Fatalf("ctx root got %q want %q", got, want)
	}
	if got, want := s.root(context.Background()), filepath.Join(dirA, ".bma", "shared"); got != want {
		t.Fatalf("default root got %q want %q", got, want)
	}
	if got := s.WorkDirOf(ctx); got != dirB {
		t.Fatalf("WorkDirOf got %q want %q", got, dirB)
	}
	if got := s.WorkDirOf(context.Background()); got != dirA {
		t.Fatalf("WorkDirOf default got %q want %q", got, dirA)
	}
}

// TestSharedMemorySetGet_PerSession 验证读写按 ctx 会话目录隔离：
// 同一把 store，ctx 注入 dirB 时写入的文件落在 dirB 侧，默认 ctx 读不到。
func TestSharedMemorySetGet_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	s := NewFileSharedMemoryStore(dirA)
	ctxB := WithWorkDir(context.Background(), dirB)

	if err := s.Set(ctxB, "agent-1:shared", "hello B"); err != nil {
		t.Fatalf("Set ctxB: %v", err)
	}
	if got, err := s.Get(ctxB, "agent-1:shared"); err != nil || got != "hello B" {
		t.Fatalf("Get ctxB got %q err %v", got, err)
	}
	// 默认目录（dirA）侧读不到 dirB 的写入。
	if got, err := s.Get(context.Background(), "agent-1:shared"); err != nil || got != "" {
		t.Fatalf("Get default got %q err %v, want empty", got, err)
	}
	// 文件确实落在 dirB/.bma/shared 下。
	keys := s.Keys(ctxB)
	if len(keys) != 1 || keys[0] != "agent-1:shared" {
		t.Fatalf("Keys ctxB got %v", keys)
	}
	if keys := s.Keys(context.Background()); len(keys) != 0 {
		t.Fatalf("Keys default got %v, want empty", keys)
	}
}
