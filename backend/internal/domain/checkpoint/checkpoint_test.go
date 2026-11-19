package checkpoint

// checkpoint_test.go 覆盖计划 §6.7 的检查点必测场景（可机器验证的部分）：
//   - dirty（未提交修改）与 untracked 文件都进快照；
//   - 二进制、CRLF、权限位按字节保真恢复；
//   - 新增/修改/删除/重命名都能从 MutationManifest 看到；
//   - 恢复只回退本 Action 的变化，外部漂移（快照后又被改）不得被覆盖；
//   - 文件数超上限时快照标 degraded + 明确 reason（不得静默声称完整快照）；
//   - 不触碰用户 .git（本实现根本不读 .git，这里断言排除清单生效）。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if s == nil {
		t.Fatal("store must be configured")
	}
	return s
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestCaptureIncludesDirtyAndUntracked 快照必须包含 dirty 与 untracked 文件
// （计划 I-EXEC-4 / R-10 / R-11）：本实现直接扫盘，不依赖 git 状态。
func TestCaptureIncludesDirtyAndUntracked(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/app.vue", "<template>dirty</template>\n")
	writeFile(t, root, "src/new-untracked.css", ".x{color:red}\n")
	s := newTestStore(t)
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"src/app.vue", "src/new-untracked.css"} {
		if _, ok := snap.Files[want]; !ok {
			t.Fatalf("snapshot must contain %s, got %+v", want, snap.Files)
		}
	}
	if snap.Degraded {
		t.Fatalf("clean capture must not be degraded: %s", snap.Reason)
	}
}

// TestCaptureExcludesBMADirs .bma 与 node_modules 必须排除，且排除清单可审计。
func TestCaptureExcludesBMADirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.css", "a{}\n")
	writeFile(t, root, ".bma/artifacts/x.html", "<html></html>")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports=1")
	s := newTestStore(t)
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files[".bma/artifacts/x.html"]; ok {
		t.Fatal(".bma must be excluded from workspace snapshots")
	}
	if _, ok := snap.Files["node_modules/pkg/index.js"]; ok {
		t.Fatal("node_modules must be excluded from workspace snapshots")
	}
	joined := strings.Join(snap.Excluded, ",")
	for _, want := range []string{".bma", "node_modules", ".git"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("excluded list must record %q, got %v", want, snap.Excluded)
		}
	}
}

// TestDiffDetectsAllMutationKinds 新增/修改/删除/重命名都必须在 manifest 里出现。
func TestDiffDetectsAllMutationKinds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.css", ".keep{}\n")
	writeFile(t, root, "modify.css", ".m{width:1px}\n")
	writeFile(t, root, "delete.css", ".d{}\n")
	writeFile(t, root, "old-name.css", ".r{}\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "modify.css", ".m{width:2px}\n")
	writeFile(t, root, "added.css", ".a{}\n")
	if err := os.Remove(filepath.Join(root, "delete.css")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "old-name.css"), filepath.Join(root, "new-name.css")); err != nil {
		t.Fatal(err)
	}
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Diff(before, after)
	if len(m.Added) != 2 { // added.css + new-name.css（重命名在文件级 = 删除+新增）
		t.Fatalf("added = %v, want 2 entries", m.Added)
	}
	if len(m.Modified) != 1 || m.Modified[0] != "modify.css" {
		t.Fatalf("modified = %v, want [modify.css]", m.Modified)
	}
	if len(m.Deleted) != 2 { // delete.css + old-name.css
		t.Fatalf("deleted = %v, want 2 entries", m.Deleted)
	}
	if m.Empty() {
		t.Fatal("manifest must not be empty when files changed")
	}
}

// TestRestoreRevertsOnlyActionChanges 回滚只回退本 Action 的变化，不动未变更文件。
func TestRestoreRevertsOnlyActionChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "target.css", ".t{width:200px}\n")
	writeFile(t, root, "untouched.css", ".u{color:blue}\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Action：改 target.css + 新建 extra.css
	writeFile(t, root, "target.css", ".t{width:999px}\n")
	writeFile(t, root, "extra.css", ".e{}\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("restore failures: %+v", rep.Failed)
	}
	if got := readFile(t, root, "target.css"); got != ".t{width:200px}\n" {
		t.Fatalf("target.css not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "extra.css")); !os.IsNotExist(err) {
		t.Fatal("action-added file must be removed on rollback")
	}
	if got := readFile(t, root, "untouched.css"); got != ".u{color:blue}\n" {
		t.Fatalf("untouched file must stay identical: %q", got)
	}
}

// TestRestoreSkipsExternalDrift 快照后又被外部修改的文件不得被覆盖（R-12）。
func TestRestoreSkipsExternalDrift(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.css", "v1\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "a.css", "v2-action\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	// 用户在 Action 之后又改了一次（外部漂移）。
	writeFile(t, root, "a.css", "v3-user\n")
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Drifted) != 1 || rep.Drifted[0] != "a.css" {
		t.Fatalf("external drift must be detected, got %+v", rep)
	}
	if got := readFile(t, root, "a.css"); got != "v3-user\n" {
		t.Fatalf("external edit must not be overwritten, got %q", got)
	}
}

// TestRestoreBinaryAndCRLF 二进制与 CRLF 按字节恢复（R-19）。
func TestRestoreBinaryAndCRLF(t *testing.T) {
	root := t.TempDir()
	binary := []byte{0x00, 0x01, 0xFF, 0xFE, 'a', 'b', '\r', '\n'}
	orig := []byte("line1\r\nline2\r\n")
	if err := os.WriteFile(filepath.Join(root, "img.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "crlf.txt"), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "img.bin"), []byte("clobbered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "crlf.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true}); err != nil {
		t.Fatal(err)
	}
	gotBin, err := os.ReadFile(filepath.Join(root, "img.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBin) != string(binary) {
		t.Fatalf("binary not restored byte-for-byte: %v", gotBin)
	}
	gotText, err := os.ReadFile(filepath.Join(root, "crlf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotText) != string(orig) {
		t.Fatalf("CRLF not preserved: %q", gotText)
	}
}

// TestRestoreModeChange 权限位按 before 恢复（Unix 上验证；Windows 跳过权限断言）。
func TestRestoreModeChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不保留 Unix 权限位语义")
	}
	root := t.TempDir()
	writeFile(t, root, "script.sh", "#!/bin/sh\n")
	path := filepath.Join(root, "script.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Diff(before, after)
	if len(m.ModeChanged) != 1 {
		t.Fatalf("mode change must be detected, got %+v", m.ModeChanged)
	}
	if _, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", info.Mode().Perm())
	}
}

// TestCaptureDegradedOnFileLimit 超上限必须 degraded + reason（不得静默声称完整快照）。
func TestCaptureDegradedOnFileLimit(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.css", "b.css", "c.css"} {
		writeFile(t, root, n, "x\n")
	}
	s := NewStore(t.TempDir(), WithMaxFiles(1))
	if s == nil {
		t.Fatal("store required")
	}
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Degraded {
		t.Fatal("file limit exceeded must mark snapshot degraded")
	}
	if !strings.Contains(snap.Reason, "file limit") {
		t.Fatalf("reason must explain the limit, got %q", snap.Reason)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("snapshot must stop at limit, got %d files", len(snap.Files))
	}
}

// TestRestoreDryRunDoesNotTouchDisk shadow 模式：只算不落盘。
func TestRestoreDryRunDoesNotTouchDisk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "x.css", "before\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "x.css", "after\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Restored) != 1 {
		t.Fatalf("dry run must still report what would be restored, got %+v", rep)
	}
	if got := readFile(t, root, "x.css"); got != "after\n" {
		t.Fatalf("dry run must not touch disk, got %q", got)
	}
}

// TestSnapshotLoadRoundTrip 快照索引可回读且 ID 稳定（幂等）。
func TestSnapshotLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.css", "a\n")
	s := newTestStore(t)
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != snap.ID {
		t.Fatalf("identical workspace must produce stable snapshot id: %s vs %s", again.ID, snap.ID)
	}
	loaded, err := s.Load(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != snap.ID || len(loaded.Files) != len(snap.Files) {
		t.Fatalf("load mismatch: %+v", loaded)
	}
}

// TestNilStoreIsSafe 未配置检查点时零行为（off 模式/无数据目录）。
func TestNilStoreIsSafe(t *testing.T) {
	s := NewStore("")
	if s != nil {
		t.Fatal("empty dir must yield nil store")
	}
	if _, err := s.Capture("x"); err == nil {
		t.Fatal("nil store must return explicit error, not silent success")
	}
}
