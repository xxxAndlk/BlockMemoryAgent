package tool

// shared_memory_file_store_test.go 验证 FileSharedMemoryStore 的 Set/Get/Delete/Keys
// 与 sanitizeKey/desanitizeKey 的可逆性。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileSharedMemoryStore_SetGetDelete 验证基础 CRUD：Set 写文件，Get 读文件，Delete 删文件。
func TestFileSharedMemoryStore_SetGetDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	key := "session-1-abc-1:spec"
	val := EncodeSpecMD("session-1-abc-1", Spec{Goal: "g", Acceptance: []string{"a"}}, nil)

	if err := s.Set(ctx, key, val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != val {
		t.Fatalf("Get mismatch: got=%q want=%q", got, val)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got2, _ := s.Get(ctx, key)
	if got2 != "" {
		t.Fatalf("expected empty after Delete, got=%q", got2)
	}
}

// TestFileSharedMemoryStore_KeysEnumerates 验证 Keys 返回所有已写键（desanitize 还原）。
func TestFileSharedMemoryStore_KeysEnumerates(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	keys := []string{
		"session-1:spec",
		"session-1:shared",
		"session-1:file_tree",
	}
	for _, k := range keys {
		if err := s.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	got := s.Keys(ctx)
	if len(got) != len(keys) {
		t.Fatalf("expected %d keys, got %d: %v", len(keys), len(got), got)
	}
	gotSet := make(map[string]bool, len(got))
	for _, k := range got {
		gotSet[k] = true
	}
	for _, k := range keys {
		if !gotSet[k] {
			t.Fatalf("expected key %s in Keys(), got %v", k, got)
		}
	}
}

// TestFileSharedMemoryStore_SubAgentKeyReversible 验证含 "/" 的子 Agent ID 键名可逆。
// 子 Agent ID 形如 "session-1/domain-1"，roleID 含 "_"（如 code_assistant）。
func TestFileSharedMemoryStore_SubAgentKeyReversible(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	key := "session-1-abc-1/code_assistant-1:spec"
	if err := s.Set(ctx, key, "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, _ := s.Get(ctx, key)
	if got != "v" {
		t.Fatalf("Get mismatch: got=%q want=v", got)
	}

	keys := s.Keys(ctx)
	found := false
	for _, k := range keys {
		if k == key {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected key %q in Keys() got %v", key, keys)
	}
}

// TestFileSharedMemoryStore_PersistsToDisk 验证 Set 写入真实 MD 文件到 .bma/shared/。
func TestFileSharedMemoryStore_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	key := "session-1:spec"
	val := EncodeSpecMD("session-1", Spec{Goal: "g"}, nil)
	if err := s.Set(ctx, key, val); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// 文件应存在于 <dir>/.bma/shared/<sanitized>.md
	entries, err := os.ReadDir(filepath.Join(dir, ".bma", "shared"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".md") {
		t.Fatalf("expected .md suffix, got %q", name)
	}
	if !strings.Contains(name, "__spec") {
		t.Fatalf("expected filename to contain slot '__spec', got %q", name)
	}

	// 文件内容应为人读 MD（含 frontmatter 与 body）。
	data, err := os.ReadFile(filepath.Join(dir, ".bma", "shared", name))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasPrefix(string(data), "---\n") {
		t.Fatalf("expected MD frontmatter start, got: %q", string(data)[:50])
	}
	if !strings.Contains(string(data), "goal: g") {
		t.Fatalf("expected goal in frontmatter, got: %q", string(data))
	}
	if !strings.Contains(string(data), "# 任务规范") {
		t.Fatalf("expected body header, got: %q", string(data))
	}
}

// TestFileSharedMemoryStore_GetMissingReturnsEmpty 验证 Get 不存在的键返回 ("", nil)。
func TestFileSharedMemoryStore_GetMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	got, err := s.Get(context.Background(), "missing:key")
	if err != nil {
		t.Fatalf("Get missing should not error, got: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty for missing key, got=%q", got)
	}
}

// TestFileSharedMemoryStore_DeleteMissingIdempotent 验证 Delete 不存在的键幂等返回 nil。
func TestFileSharedMemoryStore_DeleteMissingIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	if err := s.Delete(context.Background(), "missing:key"); err != nil {
		t.Fatalf("Delete missing should be idempotent, got: %v", err)
	}
}
