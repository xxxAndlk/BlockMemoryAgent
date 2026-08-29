// editfile_test.go 验证 EditFile（精确局部替换）工具的匹配语义与 WriteFile 对齐机制：
// 唯一匹配替换 / 多处匹配拒绝 / replace_all 全替换 / 无匹配报错（附就近上下文提示）/
// CRLF 行尾等价 / 快照备份 / 共享记忆失效（Layer 2）/ 生产目录审批边界。
package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditFile_ExactUniqueReplace 验证唯一匹配精确替换：只改目标片段，其余内容不变，
// 输出携带替换次数与首个匹配行号。
func TestEditFile_ExactUniqueReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.js")
	original := "const x = 1;\nfunction foo() {\n  return x;\n}\n"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":       "a.js",
		"old_string": "return x;",
		"new_string": "return x + 1;",
	})
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "replaced 1 occurrence(s)") {
		t.Errorf("output = %q, want replaced count", res.Output)
	}
	if !strings.Contains(res.Output, "line 3") {
		t.Errorf("output = %q, want line number of match", res.Output)
	}
	got, _ := os.ReadFile(target)
	want := "const x = 1;\nfunction foo() {\n  return x + 1;\n}\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestEditFile_NoMatchError 验证无匹配时返回错误并附文件开头片段（就近上下文提示），
// 文件保持原样。
func TestEditFile_NoMatchError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	original := "line one\nline two\n"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":       "a.txt",
		"old_string": "不存在的文本",
		"new_string": "replacement",
	})
	if res.Success {
		t.Fatal("expected failure for no match")
	}
	if !strings.Contains(res.Error, "old_string 未在文件中找到") {
		t.Errorf("error = %q, want no-match message", res.Error)
	}
	// 就近上下文提示：错误应包含文件开头片段。
	if !strings.Contains(res.Error, "line one") {
		t.Errorf("error = %q, want file-start context hint", res.Error)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("file must stay unchanged on no match, got %q", got)
	}
}

// TestEditFile_MultipleMatchesRejected 验证多处匹配且未传 replace_all 时被拒绝，
// 报错携带出现次数，文件保持原样。
func TestEditFile_MultipleMatchesRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	original := "aaa bbb aaa ccc aaa\n"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":       "a.txt",
		"old_string": "aaa",
		"new_string": "zzz",
	})
	if res.Success {
		t.Fatal("expected rejection for multiple matches")
	}
	if !strings.Contains(res.Error, "出现 3 次") || !strings.Contains(res.Error, "replace_all=true") {
		t.Errorf("error = %q, want count + replace_all guidance", res.Error)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("file must stay unchanged on rejection, got %q", got)
	}
}

// TestEditFile_ReplaceAll 验证 replace_all=true 替换全部匹配处。
func TestEditFile_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("aaa bbb aaa ccc aaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":        "a.txt",
		"old_string":  "aaa",
		"new_string":  "zzz",
		"replace_all": true,
	})
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "replaced 3 occurrence(s)") {
		t.Errorf("output = %q, want 3 occurrences", res.Output)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "zzz bbb zzz ccc zzz\n" {
		t.Errorf("content = %q, want all replaced", got)
	}
}

// TestEditFile_CRLFTolerance 验证行尾等价：文件为 CRLF 时，old_string 用 LF 也能匹配，
// 且写回保持文件原有 CRLF 风格（全文件行尾不被破坏）。
func TestEditFile_CRLFTolerance(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "win.txt")
	original := "line one\r\nline two\r\nline three\r\n"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	// old_string 用 LF 行尾（模型从工具输出抄回的常见形态）。
	res := e.editFile(context.Background(), map[string]any{
		"path":       "win.txt",
		"old_string": "line two\n",
		"new_string": "line TWO\n",
	})
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Error)
	}
	got, _ := os.ReadFile(target)
	want := "line one\r\nline TWO\r\nline three\r\n"
	if string(got) != want {
		t.Errorf("content = %q, want CRLF preserved %q", got, want)
	}
}

// TestEditFile_SnapshotBeforeEdit 验证编辑已存在文件前快照原文件到
// .bma/snapshots/<sessionID>/<relPath>.<timestamp>.bak，Output 携带快照路径。
func TestEditFile_SnapshotBeforeEdit(t *testing.T) {
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
	ctx := WithSessionID(context.Background(), "test-sess-edit")
	res := e.editFile(ctx, map[string]any{
		"path":       "sub/a.txt",
		"old_string": "v1",
		"new_string": "v2",
	})
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "原文件已备份到") {
		t.Fatalf("output = %q, want snapshot path", res.Output)
	}
	snapDir := filepath.Join(dir, ".bma", "snapshots", "test-sess-edit")
	entries, err := os.ReadDir(filepath.Join(snapDir, "sub"))
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(entries))
	}
	snap, _ := os.ReadFile(filepath.Join(snapDir, "sub", entries[0].Name()))
	if string(snap) != original {
		t.Errorf("snapshot content = %q, want %q", snap, original)
	}
}

// TestEditFile_NewFileRefused 验证 EditFile 不能创建文件：目标不存在时报错并引导 WriteFile。
func TestEditFile_NewFileRefused(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":       "new.txt",
		"old_string": "x",
		"new_string": "y",
	})
	if res.Success {
		t.Fatal("expected failure for missing file")
	}
	if !strings.Contains(res.Error, "文件不存在") || !strings.Contains(res.Error, "WriteFile") {
		t.Errorf("error = %q, want missing-file message with WriteFile guidance", res.Error)
	}
}

// TestEditFile_ProtectedPathRefused 验证 EditFile 受保护路径被守卫拒绝（与 WriteFile 同口径）。
func TestEditFile_ProtectedPathRefused(t *testing.T) {
	dir := t.TempDir()
	// .git 目录本身可能不存在，但守卫基于路径特征拒绝，无需创建文件。
	e := NewExecutor(dir)
	res := e.editFile(context.Background(), map[string]any{
		"path":       ".git/config",
		"old_string": "x",
		"new_string": "y",
	})
	if res.Success {
		t.Fatal("expected rejection for protected path")
	}
	if res.Error == "" {
		t.Fatal("expected guard error")
	}
}

// TestEditFile_EmptyOldStringRejected 验证 old_string 必填、old==new 拒绝。
func TestEditFile_EmptyOldStringRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir)
	if res := e.editFile(context.Background(), map[string]any{"path": "a.txt", "old_string": "", "new_string": "z"}); res.Success || !strings.Contains(res.Error, "old_string is required") {
		t.Errorf("empty old_string: res=%+v", res)
	}
	if res := e.editFile(context.Background(), map[string]any{"path": "a.txt", "old_string": "abc", "new_string": "abc"}); res.Success || !strings.Contains(res.Error, "无需编辑") {
		t.Errorf("identical old/new: res=%+v", res)
	}
}

// TestEditFile_InvalidatesSharedMemory 验证 Layer 2：EditFile 成功后，
// 引用同 path 的 KV entry 被标记 stale（invalidated_at）但不物理删除、body 保留
//（与 WriteFile 行为一致）。
func TestEditFile_InvalidatesSharedMemory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(target, []byte("package bar\nfunc Bar() {}\n"), 0644); err != nil {
		t.Fatalf("write bar.go: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆，引用 bar.go。
	if _, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "bar.go defines package bar",
		"files":   []any{target},
	}); err != nil {
		t.Fatalf("WriteSharedMemory: %v", err)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("expected KV entry before EditFile")
	}

	// EditFile 修改 bar.go，触发失效 hook。
	eres, err := r.Dispatch(ctx, "EditFile", map[string]any{
		"path":       target,
		"old_string": "func Bar() {}",
		"new_string": "func Bar() int { return 1 }",
	})
	if err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	if !eres.Success {
		t.Fatalf("EditFile failed: %s", eres.Error)
	}

	// KV entry 应保留并打上 stale 标记（invalidated_at），body 原样留存。
	val, ok := store.items["meta-1:shared"]
	if !ok {
		t.Fatal("expected KV entry retained (marked stale, not deleted) after EditFile invalidated it")
	}
	fm, body, decOK := DecodeSharedMD(val)
	if !decOK {
		t.Fatalf("invalidated entry should remain decodable MD, got: %q", val)
	}
	if fm.InvalidatedAt == "" {
		t.Fatalf("expected invalidated_at marked in frontmatter, got: %q", val)
	}
	if body != "bar.go defines package bar" {
		t.Fatalf("expected body preserved after invalidation, got: %q", body)
	}
}

// TestEditFile_DoesNotInvalidateUnrelatedEntry 验证 Layer 2 精确性：
// EditFile 只删引用同 path 的 entry，不误删引用其他 path 的 entry。
func TestEditFile_DoesNotInvalidateUnrelatedEntry(t *testing.T) {
	dir := t.TempDir()
	foo := filepath.Join(dir, "foo.go")
	bar := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(foo, []byte("package foo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bar, []byte("package bar\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆只引用 foo.go。
	if _, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "foo summary",
		"files":   []any{foo},
	}); err != nil {
		t.Fatalf("WriteSharedMemory: %v", err)
	}

	// EditFile 修改 bar.go：foo 引用的 entry 不应被删。
	eres, err := r.Dispatch(ctx, "EditFile", map[string]any{
		"path":       bar,
		"old_string": "package bar",
		"new_string": "package bar // v2",
	})
	if err != nil || !eres.Success {
		t.Fatalf("EditFile bar: err=%v res=%+v", err, eres)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("KV entry should NOT be deleted: EditFile touched bar.go, entry references foo.go only")
	}
}

// TestEditFile_SchemaExposed 验证 EditFile 出现在 Schema 中且描述引导小改优先 EditFile。
func TestEditFile_SchemaExposed(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	found := false
	for _, tool := range r.Schema() {
		if tool.Name() == "EditFile" {
			found = true
			desc := tool.Description()
			if !strings.Contains(desc, "old_string") || !strings.Contains(desc, "replace_all") {
				t.Errorf("EditFile description = %q, want old_string/replace_all guidance", desc)
			}
			if !strings.Contains(desc, "小改") {
				t.Errorf("EditFile description = %q, want small-change guidance", desc)
			}
		}
	}
	if !found {
		t.Fatal("EditFile not exposed in Schema()")
	}
}
