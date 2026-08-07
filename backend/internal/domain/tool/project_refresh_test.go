package tool

// project_refresh_test.go 验证去抖刷新器与删改类命令识别。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProjectRefresher_Debounce 验证连续 schedule 仅触发一次 refresh。
func TestProjectRefresher_Debounce(t *testing.T) {
	var calls int32
	var wg sync.WaitGroup
	refresh := func(ctx context.Context, workDir string) {
		atomic.AddInt32(&calls, 1)
		wg.Done()
	}
	p := newProjectRefresher(20*time.Millisecond, refresh)
	wg.Add(1)
	p.schedule("/wd")
	p.schedule("/wd") // 重置计时器，不应多触发
	p.schedule("/wd")
	wg.Wait()
	time.Sleep(40 * time.Millisecond) // 确保没有第二次 fire
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh call after debounce, got %d", got)
	}
}

// TestProjectRefresher_EmptyWorkDirNoop 验证空 workDir 不触发 refresh。
func TestProjectRefresher_EmptyWorkDirNoop(t *testing.T) {
	calls := 0
	refresh := func(ctx context.Context, workDir string) { calls++ }
	p := newProjectRefresher(5*time.Millisecond, refresh)
	p.schedule("")
	time.Sleep(20 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("expected 0 calls for empty workDir, got %d", calls)
	}
}

// TestProjectRefresher_LastWorkDirWins 验证连续 schedule 以最后一次 workDir 为准。
func TestProjectRefresher_LastWorkDirWins(t *testing.T) {
	var got string
	var mu sync.Mutex
	refresh := func(ctx context.Context, workDir string) {
		mu.Lock()
		got = workDir
		mu.Unlock()
	}
	p := newProjectRefresher(10*time.Millisecond, refresh)
	p.schedule("/a")
	p.schedule("/b")
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got != "/b" {
		t.Fatalf("expected last workDir /b, got %q", got)
	}
}

// TestCommandAffectsFiles 验证删改类命令识别（含 git 子命令、避免文件名误判）。
func TestCommandAffectsFiles(t *testing.T) {
	cases := map[string]bool{
		"rm foo":      true,
		"rm -rf x":    true,
		"mkdir bar":   true,
		"touch x":     true,
		"mv a b":      true,
		"cp a b":      true,
		"git rm x":    true,
		"git mv a b":  true,
		"git add x":   false,
		"ls -la":      false,
		"cat foo":     false,
		"go build":    false,
		"echo rm":     false, // rm 是 echo 的参数，非命令
		"warm up":     false,
		"":            false,
		"-rf":         false, // 只有选项
	}
	for cmd, want := range cases {
		if got := commandAffectsFiles(cmd); got != want {
			t.Errorf("commandAffectsFiles(%q) = %v, want %v", cmd, got, want)
		}
	}
}
