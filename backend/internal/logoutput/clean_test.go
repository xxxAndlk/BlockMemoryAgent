package logoutput

// clean_test.go 日志保留期清理（TODO #18-2 T29）：
// 过期文件删除 / 新文件保留 / 保留期 0 全 no-op / 缺目录静默。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanOldLogs_RemovesExpiredKeepsFresh(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "backend")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(sub, "2020-01-01.log")
	fresh := filepath.Join(sub, "today.log")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-60 * 24 * time.Hour) // 60 天前 > 30 天保留期
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	removed, err := CleanOldLogs(root, 30)
	if err != nil {
		t.Fatalf("CleanOldLogs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("应删 1 个过期文件，got %d", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("过期文件应已删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新文件应保留: %v", err)
	}

	// 子目录删空后应一并移除
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Logf("backend 子目录仍在（还有 today.log，属正常）")
	}
}

func TestCleanOldLogs_ZeroIsNoop(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "backend")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "2020-01-01.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{0, -1} {
		removed, err := CleanOldLogs(root, days)
		if err != nil || removed != 0 {
			t.Fatalf("retention=%d 应 no-op, got removed=%d err=%v", days, removed, err)
		}
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("保留期 0 不得删文件")
	}
}

func TestCleanOldLogs_MissingDirSilent(t *testing.T) {
	removed, err := CleanOldLogs(filepath.Join(t.TempDir(), "no-such-dir"), 30)
	if err != nil || removed != 0 {
		t.Fatalf("缺目录应静默 no-op, got removed=%d err=%v", removed, err)
	}
}
