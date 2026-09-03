// restorefile_test.go 验证 RestoreFile（从 .bma/snapshots 快照恢复文件）工具：
// 最新快照恢复 / 指定时间戳恢复 / 误删文件重建 / 恢复前对当前内容再备份（可回退）/
// 无快照报错（附可用时间戳）/ 跨会话回退查找。
package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSnapshot 在 <dir>/.bma/snapshots/<sessionID>/<rel>.<stamp>.bak 直接构造一份快照文件。
func writeSnapshot(t *testing.T, dir, sessionID, rel, stamp, content string) string {
	t.Helper()
	p := filepath.Join(dir, ".bma", "snapshots", sessionID, rel+"."+stamp+".bak")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRestoreFile_EndToEnd 走 EditFile 真实链路：编辑生成快照后 RestoreFile 恢复原文。
func TestRestoreFile_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "a.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	original := "ORIGINAL v1"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "test-sess-restore")
	res := e.editFile(ctx, map[string]any{
		"path":       "sub/a.txt",
		"old_string": "v1",
		"new_string": "v2",
	})
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Error)
	}
	res = e.restoreFile(ctx, map[string]any{"path": "sub/a.txt"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "restored") {
		t.Errorf("output = %q, want restored", res.Output)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("content = %q, want %q", got, original)
	}
}

// TestRestoreFile_LatestSnapshotWins 验证缺省时恢复最新一份快照（按时间戳字典序）。
func TestRestoreFile_LatestSnapshotWins(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, dir, "s1", "a.txt", "20260901-100000", "v-old")
	writeSnapshot(t, dir, "s1", "a.txt", "20260902-100000", "v-new")
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "s1")
	res := e.restoreFile(ctx, map[string]any{"path": "a.txt"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "v-new" {
		t.Errorf("content = %q, want v-new (latest snapshot)", got)
	}
}

// TestRestoreFile_SpecificTimestamp 验证 timestamp 参数精确选择指定快照。
func TestRestoreFile_SpecificTimestamp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, dir, "s1", "a.txt", "20260901-100000", "v-old")
	writeSnapshot(t, dir, "s1", "a.txt", "20260902-100000", "v-new")
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "s1")
	res := e.restoreFile(ctx, map[string]any{"path": "a.txt", "timestamp": "20260901-100000"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "v-old" {
		t.Errorf("content = %q, want v-old (specified timestamp)", got)
	}
}

// TestRestoreFile_DeletedFileRecreated 验证误删场景：目标文件不存在时由快照重建。
func TestRestoreFile_DeletedFileRecreated(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, "s1", "sub/a.txt", "20260902-100000", "v-content")
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "s1")
	res := e.restoreFile(ctx, map[string]any{"path": "sub/a.txt"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "v-content" {
		t.Errorf("content = %q, want v-content", got)
	}
}

// TestRestoreFile_BacksUpCurrentBeforeOverwrite 验证恢复覆盖前当前内容再留一份快照，
// 恢复操作本身可回退。
func TestRestoreFile_BacksUpCurrentBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, dir, "s1", "a.txt", "20260901-100000", "v-old")
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "s1")
	res := e.restoreFile(ctx, map[string]any{"path": "a.txt"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "覆盖前内容已备份到") {
		t.Errorf("output = %q, want backup path note", res.Output)
	}
	// 恢复快照后再次 RestoreFile（不传时间戳，取最新）应回到 "current"——
	// 即恢复前对 current 做的备份成为最新快照。
	res = e.restoreFile(ctx, map[string]any{"path": "a.txt"})
	if !res.Success {
		t.Fatalf("second restore failed: %s", res.Error)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "current" {
		t.Errorf("content = %q, want current (reverted the restore)", got)
	}
}

// TestRestoreFile_NoSnapshot 验证无快照时报错；存在其他时间戳快照时错误附可用时间戳。
func TestRestoreFile_NoSnapshot(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "s1")
	res := e.restoreFile(ctx, map[string]any{"path": "a.txt"})
	if res.Success {
		t.Fatal("expected failure without snapshot")
	}
	if !strings.Contains(res.Error, "未找到") {
		t.Errorf("error = %q, want 未找到 message", res.Error)
	}

	// 指定不存在的时间戳：报错并列出可用时间戳。
	writeSnapshot(t, dir, "s1", "b.txt", "20260901-100000", "v")
	res = e.restoreFile(ctx, map[string]any{"path": "b.txt", "timestamp": "20000101-000000"})
	if res.Success {
		t.Fatal("expected failure for unknown timestamp")
	}
	if !strings.Contains(res.Error, "20260901-100000") {
		t.Errorf("error = %q, want available timestamp listed", res.Error)
	}
}

// TestRestoreFile_CrossSessionFallback 验证本会话目录无快照时回退扫描其他会话目录
// （会话重启后新 sessionID 也能找回旧会话的快照）。
func TestRestoreFile_CrossSessionFallback(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, "old-sess", "a.txt", "20260901-100000", "v-cross")
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "new-sess")
	res := e.restoreFile(ctx, map[string]any{"path": "a.txt"})
	if !res.Success {
		t.Fatalf("restore failed: %s", res.Error)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "v-cross" {
		t.Errorf("content = %q, want v-cross", got)
	}
}

// TestRestoreFile_PathRequired 验证 path 缺失时报错。
func TestRestoreFile_PathRequired(t *testing.T) {
	e := NewExecutor(t.TempDir())
	res := e.restoreFile(context.Background(), map[string]any{})
	if res.Success {
		t.Fatal("expected failure for empty path")
	}
	if !strings.Contains(res.Error, "path is required") {
		t.Errorf("error = %q, want path is required", res.Error)
	}
}
