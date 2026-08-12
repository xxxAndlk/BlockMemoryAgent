package tool

// writefile_guard_test.go 验证 WriteFile 的截断防护：
// 内容硬上限拒收 + 重写已有文件时大小骤减输出警告（辅助模型发现截断写入）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFile_ContentTooLargeRejected 验证超上限内容被拒收并给出拆分指引。
func TestWriteFile_ContentTooLargeRejected(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	big := strings.Repeat("x", maxWriteFileContentRunes+1)
	res := e.writeFile(context.Background(), map[string]any{"path": "a.js", "content": big})
	if res.Error == "" {
		t.Fatal("want error for oversized content")
	}
	if !strings.Contains(res.Error, "content too large") {
		t.Fatalf("error = %q, want content too large", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.js")); err == nil {
		t.Fatal("oversized content must not be written")
	}
}

// TestWriteFile_ContentAtLimitOK 验证恰好等于上限的内容正常写入。
func TestWriteFile_ContentAtLimitOK(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	content := strings.Repeat("x", maxWriteFileContentRunes)
	res := e.writeFile(context.Background(), map[string]any{"path": "a.js", "content": content})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
}

// TestWriteFile_ShrinkWarning 验证重写已有大文件且新内容骤减时输出附警告；
// 正常重写与新文件写入无警告。
func TestWriteFile_ShrinkWarning(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.js")
	if err := os.WriteFile(target, []byte(strings.Repeat("y", 10000)), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)

	// 新内容 1000 字节 < 10000*0.3 → 附警告。
	res := e.writeFile(context.Background(), map[string]any{"path": "big.js", "content": strings.Repeat("z", 1000)})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
	if !strings.Contains(res.Output, "写入警告") {
		t.Errorf("output = %q, want shrink warning", res.Output)
	}

	// 重写为 5000 字节（> 30%）→ 无警告。
	if err := os.WriteFile(target, []byte(strings.Repeat("y", 10000)), 0644); err != nil {
		t.Fatal(err)
	}
	res = e.writeFile(context.Background(), map[string]any{"path": "big.js", "content": strings.Repeat("z", 5000)})
	if strings.Contains(res.Output, "写入警告") {
		t.Errorf("output = %q, want no warning for normal rewrite", res.Output)
	}

	// 新文件写入 → 无警告。
	res = e.writeFile(context.Background(), map[string]any{"path": "new.js", "content": strings.Repeat("z", 100)})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
	if strings.Contains(res.Output, "写入警告") {
		t.Errorf("output = %q, want no warning for new file", res.Output)
	}
}

// TestWriteFile_ExtremeShrinkRefused 验证极端缩小（新内容 < 原文件 10% 且原文件 >= 5KB）
// 被硬拒绝，原文件保持不变（防 WriteFile 整文件覆盖把原文件截成片段）。
func TestWriteFile_ExtremeShrinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.js")
	original := strings.Repeat("y", 10000)
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)

	// 新内容 500 字节 = 5% 原文件，未传 confirm_shrink -> 拒收。
	res := e.writeFile(context.Background(), map[string]any{"path": "big.js", "content": strings.Repeat("z", 500)})
	if res.Success {
		t.Fatal("extreme shrink must be refused")
	}
	if !strings.Contains(res.Error, "refused") {
		t.Fatalf("error = %q, want refused", res.Error)
	}
	// 原文件未被覆盖。
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("original file overwritten: got %d bytes, want %d", len(got), len(original))
	}
}

// TestWriteFile_ExtremeShrinkConfirmOverride 验证 confirm_shrink=true 绕过极端缩小硬拒绝
// （合法大幅精简场景）；仍附 shrink 警告（落入 30% 警告区间）。
func TestWriteFile_ExtremeShrinkConfirmOverride(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.js")
	if err := os.WriteFile(target, []byte(strings.Repeat("y", 10000)), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)

	res := e.writeFile(context.Background(), map[string]any{
		"path":           "big.js",
		"content":        strings.Repeat("z", 500),
		"confirm_shrink": true,
	})
	if !res.Success {
		t.Fatalf("confirm_shrink should override refusal: %v", res.Error)
	}
	if !strings.Contains(res.Output, "写入警告") {
		t.Errorf("output = %q, want shrink warning even with confirm_shrink", res.Output)
	}
}

// TestWriteFile_SmallOriginalNoRefuse 验证原文件 < 5KB 时不触极端缩小硬拒绝
// （仅触发 30% 警告），避免小文件的常规重写被误拒。
func TestWriteFile_SmallOriginalNoRefuse(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "small.js")
	// 原文件 2KB < 5KB 阈值，新内容 100 字节 = 5% -> 警告但不拒绝。
	if err := os.WriteFile(target, []byte(strings.Repeat("y", 2000)), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	res := e.writeFile(context.Background(), map[string]any{"path": "small.js", "content": strings.Repeat("z", 100)})
	if !res.Success {
		t.Fatalf("small original must not be refused: %v", res.Error)
	}
	if !strings.Contains(res.Output, "写入警告") {
		t.Errorf("output = %q, want shrink warning", res.Output)
	}
}

// TestWriteFile_SnapshotBeforeOverwrite 验证重写已存在文件时快照原文件到
// .bma/snapshots/<sessionID>/<relPath>.<timestamp>.bak，Output 携带快照路径。
func TestWriteFile_SnapshotBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	original := "ORIGINAL CONTENT v1"
	target := filepath.Join(dir, "sub", "a.js")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "test-sess-1")
	res := e.writeFile(ctx, map[string]any{"path": filepath.Join("sub", "a.js"), "content": "NEW CONTENT v2"})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
	if !strings.Contains(res.Output, "原文件已备份到") {
		t.Fatalf("output = %q, want snapshot path", res.Output)
	}
	// 快照文件存在且内容 == 原文件。
	snapDir := filepath.Join(dir, ".bma", "snapshots", "test-sess-1")
	entries, err := os.ReadDir(filepath.Join(snapDir, "sub"))
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(snapDir, "sub", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("snapshot content = %q, want %q", got, original)
	}
	// 目标文件已被新内容覆盖。
	newGot, _ := os.ReadFile(target)
	if string(newGot) != "NEW CONTENT v2" {
		t.Fatalf("target = %q, want NEW CONTENT v2", newGot)
	}
}

// TestWriteFile_SnapshotSkippedForNewFile 验证新文件写入不产生快照（无原文件可备份）。
func TestWriteFile_SnapshotSkippedForNewFile(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "test-sess-2")
	res := e.writeFile(ctx, map[string]any{"path": "new.js", "content": "hello"})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
	if strings.Contains(res.Output, "原文件已备份到") {
		t.Errorf("output = %q, new file must not snapshot", res.Output)
	}
	snapDir := filepath.Join(dir, ".bma", "snapshots", "test-sess-2")
	if _, err := os.Stat(snapDir); err == nil {
		t.Errorf("snapshot dir must not exist for new file")
	}
}

// TestWriteFile_SnapshotSkippedForTemporary 验证 temporary=true 跳过快照
// （临时文件不是用户数据，无需备份）。
func TestWriteFile_SnapshotSkippedForTemporary(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	ctx := WithSessionID(context.Background(), "test-sess-3")
	// 先写一个临时文件（首次写入，无原文件）。
	res := e.writeFile(ctx, map[string]any{"path": "tmp.js", "content": "v1", "temporary": true})
	if !res.Success {
		t.Fatalf("first write failed: %v", res.Error)
	}
	if strings.Contains(res.Output, "原文件已备份到") {
		t.Errorf("output = %q, temporary first write must not snapshot", res.Output)
	}
	// 再写一次（覆盖临时文件）。
	res = e.writeFile(ctx, map[string]any{"path": "tmp.js", "content": "v2", "temporary": true})
	if !res.Success {
		t.Fatalf("second write failed: %v", res.Error)
	}
	if strings.Contains(res.Output, "原文件已备份到") {
		t.Errorf("output = %q, temporary overwrite must not snapshot", res.Output)
	}
}

// TestWriteFile_SnapshotSkippedWithoutSession 验证无 session 上下文时跳过快照
// （顶层无会话场景，无快照目录归属）。
func TestWriteFile_SnapshotSkippedWithoutSession(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.js")
	if err := os.WriteFile(target, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	// context 无 sessionID。
	res := e.writeFile(context.Background(), map[string]any{"path": "a.js", "content": "v2"})
	if !res.Success {
		t.Fatalf("write failed: %v", res.Error)
	}
	if strings.Contains(res.Output, "原文件已备份到") {
		t.Errorf("output = %q, no-session must not snapshot", res.Output)
	}
}
