package bootstrap

// lifecycle_test.go 数据生命周期维护（TODO #18-2 T29）：
// tool_outputs 按龄清理（过期删/新鲜留/0 关/缺目录静默）+ 维护循环 stop 幂等。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
)

func TestCleanOldToolOutputs_RemovesExpiredKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, ".bma", "tool_outputs")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(outDir, "agent-1-readfile.md")
	fresh := filepath.Join(outDir, "agent-2-bash.md")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, []byte("output"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-30 * 24 * time.Hour) // 30 天前 > 14 天保留期
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	if n := cleanOldToolOutputs(dir, 14); n != 1 {
		t.Fatalf("应删 1 个过期文件，got %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("过期文件应已删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新鲜文件应保留: %v", err)
	}
}

func TestCleanOldToolOutputs_ZeroOrMissingNoop(t *testing.T) {
	dir := t.TempDir()
	if n := cleanOldToolOutputs(dir, 0); n != 0 {
		t.Fatalf("保留期 0 应 no-op，got %d", n)
	}
	if n := cleanOldToolOutputs(filepath.Join(dir, "missing"), 14); n != 0 {
		t.Fatalf("缺目录应 no-op，got %d", n)
	}
}

// fakeArchiver 记录 ArchiveStale 调用参数。
type fakeArchiver struct {
	days  []int
	calls int
}

func (f *fakeArchiver) ArchiveStale(_ context.Context, olderThanDays int) (int64, error) {
	f.calls++
	f.days = append(f.days, olderThanDays)
	return 0, nil
}

// TestStartDataMaintenance_RunOnceAndStop 验证启动即跑一遍且 stop 幂等不 panic。
func TestStartDataMaintenance_RunOnceAndStop(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	fake := &fakeArchiver{}
	stop := startDataMaintenance(cfg, dir, fake)
	stop()
	stop() // 幂等
	if fake.calls != 1 {
		t.Fatalf("启动应同步跑一遍，got %d 次", fake.calls)
	}
}
