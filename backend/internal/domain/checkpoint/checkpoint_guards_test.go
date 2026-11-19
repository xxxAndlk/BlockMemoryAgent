package checkpoint

// checkpoint_guards_test.go 对应 2026-11-19 验收报告的第一阶段返修项：
//   - P0-1：受保护目录（.bma/verification 等）不得因父目录被排除而从快照消失；
//   - P1-1：外部删除 / 文件类型变化 / 权限变化都必须识别为 drift（不得覆盖）；
//   - P1-2：RestoreReport.Complete() 是"是否真正回滚完成"的唯一口径。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestForceIncludeSeesProtectedDirsUnderExcludedParent P0-1：
// .bma 默认被排除，但声明的受保护路径必须仍然进入快照并能被 diff 发现。
func TestForceIncludeSeesProtectedDirsUnderExcludedParent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".bma/verification/guards.md", "original\n")
	writeFile(t, root, ".bma/notes.txt", "excluded by default\n")
	writeFile(t, root, "src/a.css", "a\n")

	s := NewStore(t.TempDir(), WithForceInclude(".bma/verification"))
	if s == nil {
		t.Fatal("store required")
	}
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Files[".bma/verification/guards.md"]; !ok {
		t.Fatalf("protected file must be observed despite excluded parent: %+v", keysOf(before.Files))
	}
	if _, ok := before.Files[".bma/notes.txt"]; ok {
		t.Fatal("non-protected file under excluded parent must stay excluded")
	}

	// 篡改受保护文件 → diff 必须能看到（旧实现这里"无变更"）。
	writeFile(t, root, ".bma/verification/guards.md", "tampered\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Diff(before, after)
	if len(m.Modified) != 1 || m.Modified[0] != ".bma/verification/guards.md" {
		t.Fatalf("protected-file tampering must appear in manifest, got %+v", m)
	}
}

// TestForceIncludeExcludesOwnStorage 检查点自身存储永远不进快照（防无限膨胀）。
func TestForceIncludeExcludesOwnStorage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".bma/verification/guards.md", "g\n")
	writeFile(t, root, ".bma/checkpoints/index/x.json", "{}\n")
	s := NewStore(filepath.Join(root, ".bma", "checkpoints"), WithForceInclude(".bma/verification", ".bma/checkpoints"))
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files[".bma/verification/guards.md"]; !ok {
		t.Fatal("protected file must be observed")
	}
	for path := range snap.Files {
		if path == ".bma/checkpoints" || len(path) > 16 && path[:16] == ".bma/checkpoints" {
			t.Fatalf("checkpoint own storage must never be snapshotted: %s", path)
		}
	}
}

// TestAddForceIncludeAfterConstruction 管线构造后再声明受保护目录同样生效。
func TestAddForceIncludeAfterConstruction(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".bma/verification/g.md", "g\n")
	s := NewStore(t.TempDir())
	s.AddForceInclude(".bma/verification")
	snap, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files[".bma/verification/g.md"]; !ok {
		t.Fatal("AddForceInclude must take effect (bootstrap declares protection after NewStore)")
	}
	if len(s.ForceInclude()) != 1 {
		t.Fatalf("ForceInclude() = %v", s.ForceInclude())
	}
	s.AddForceInclude(".bma/verification") // 幂等
	if len(s.ForceInclude()) != 1 {
		t.Fatalf("duplicate force-include must be deduped: %v", s.ForceInclude())
	}
}

// TestExternalDeletionIsDrift P1-1：after 之后被外部删除的文件不得被回滚重建。
func TestExternalDeletionIsDrift(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.css", "before\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/a.css", "action\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src", "a.css")); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Drifted) != 1 || rep.Drifted[0] != "src/a.css" {
		t.Fatalf("external deletion must be reported as drift: %+v", rep)
	}
	if _, statErr := os.Stat(filepath.Join(root, "src", "a.css")); !os.IsNotExist(statErr) {
		t.Fatal("externally deleted file must not be resurrected by rollback")
	}
	if rep.Complete() {
		t.Fatal("report with drift must not be Complete()")
	}
}

// TestExternalAppearanceIsDrift after 中没有、磁盘上出现的文件（外部新建）不得被删除。
func TestExternalAppearanceIsDrift(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.css", "v1\n")
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/a.css", "v2\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	// 外部新建一个本 Action 没碰过的文件（在 manifest 之外，因此不在回滚目标里）；
	// 这里验证的是"目标文件被外部再改一次"场景。
	writeFile(t, root, "src/a.css", "external\n")
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Drifted) != 1 {
		t.Fatalf("external rewrite must be drift: %+v", rep)
	}
	if got := readFile(t, root, "src/a.css"); got != "external\n" {
		t.Fatalf("external content must survive, got %q", got)
	}
}

// TestModeOnlyChangeIsDriftAfterRestore 仅权限位被外部改动也算漂移（不得静默覆盖）。
func TestModeOnlyChangeIsDriftAfterRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不保留 Unix 权限位语义")
	}
	root := t.TempDir()
	writeFile(t, root, "s.sh", "#!/bin/sh\n")
	path := filepath.Join(root, "s.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	before, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "s.sh", "#!/bin/sh\necho x\n")
	after, err := s.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	// 外部只改权限位（内容仍是 Action 后的内容）。
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Restore(root, before, after, RestoreOptions{DetectDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Drifted) != 1 {
		t.Fatalf("external mode change must be drift: %+v", rep)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("external mode must not be overwritten, got %o", info.Mode().Perm())
	}
}

// TestRestoreReportCompleteSemantics P1-2：Complete() 是唯一"完整回滚"判据。
func TestRestoreReportCompleteSemantics(t *testing.T) {
	if !(RestoreReport{Restored: []string{"a"}}).Complete() {
		t.Fatal("clean restore must be Complete()")
	}
	for _, rep := range []RestoreReport{
		{Restored: []string{"a"}, Failed: []string{"b"}},
		{Restored: []string{"a"}, Unavailable: []string{"b"}},
		{Restored: []string{"a"}, Drifted: []string{"b"}},
	} {
		if rep.Complete() {
			t.Fatalf("partial restore must not be Complete(): %+v", rep)
		}
		if rep.Summary() == "" {
			t.Fatalf("Summary must explain the partial result: %+v", rep)
		}
	}
}

func keysOf(m map[string]FileEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
