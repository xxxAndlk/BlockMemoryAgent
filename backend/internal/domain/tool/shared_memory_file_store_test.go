package tool

// shared_memory_file_store_test.go 验证 FileSharedMemoryStore 的 Set/Get/Delete/Keys
// 与 sanitizeKey/desanitizeKey 的可逆性。

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
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

// TestFileSharedMemoryStore_MultiColonKeyRoundtrip 验证多冒号 key（多 key spec：
// "<agentID>:spec:<key>"，slot 段含 ":"）Set/Get/Keys 往返。
// 回归（2026-08-25 水果忍者）：旧实现 slot 原样落文件名，Windows 上 ":" 被当
// NTFS ADS 流分隔符——内容写进 ADS、主文件 0 字节无 .md 后缀，Keys() 枚举不到，
// dispatcher 唯一候选回退失效，call_sub_agent 连续 spec missing。
func TestFileSharedMemoryStore_MultiColonKeyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	key := "session-1-abc-1:spec:fruit-ninja"
	if err := s.Set(ctx, key, "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil || got != "v" {
		t.Fatalf("Get mismatch: got=%q err=%v", got, err)
	}

	keys := s.Keys(ctx)
	if !slices.Contains(keys, key) {
		t.Fatalf("expected key %q in Keys(), got %v", key, keys)
	}

	// 落盘文件名不得含 ":"（Windows ADS 陷阱），且必须有 .md 后缀（Keys 枚举前提）。
	entries, err := os.ReadDir(filepath.Join(dir, ".bma", "shared"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	if strings.Contains(name, ":") {
		t.Fatalf("filename must not contain ':' (ADS hazard), got %q", name)
	}
	if !strings.HasSuffix(name, ".md") {
		t.Fatalf("expected .md suffix, got %q", name)
	}
}

// TestFileSharedMemoryStore_LegacyFilenameCompat 验证旧格式文件（"<hex>__<slot>.md"，
// 升级前落盘）仍可被 Get/Keys 读取——desanitize 整 hex 解码失败时回退旧格式。
func TestFileSharedMemoryStore_LegacyFilenameCompat(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	root := filepath.Join(dir, ".bma", "shared")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 手工落旧格式文件：hex(agentID)__spec.md
	agentID := "session-1-abc-1"
	oldName := hex.EncodeToString([]byte(agentID)) + "__spec.md"
	if err := os.WriteFile(filepath.Join(root, oldName), []byte("legacy-value"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Get(context.Background(), agentID+":spec")
	if err != nil || got != "legacy-value" {
		t.Fatalf("legacy file should be readable, got=%q err=%v", got, err)
	}
	if !slices.Contains(s.Keys(context.Background()), agentID+":spec") {
		t.Fatalf("expected legacy key %q in Keys(), got %v", agentID+":spec", s.Keys(context.Background()))
	}
}

// TestFileSharedMemoryStore_UnsafeSlotCharsRoundtrip 验证 slot 含其他 Windows 非法
// 字符（/、?、* 等）时整键 hex 编码仍往返一致。
func TestFileSharedMemoryStore_UnsafeSlotCharsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	for _, key := range []string{
		"session-1:spec:a/b",
		"session-1:spec:美术资源",
		"session-1:spec:a*b?c",
	} {
		if err := s.Set(ctx, key, "v-"+key); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
		got, err := s.Get(ctx, key)
		if err != nil || got != "v-"+key {
			t.Fatalf("Get %s mismatch: got=%q err=%v", key, got, err)
		}
	}
	// 文件名全部无 ":" 且有 .md 后缀。
	entries, err := os.ReadDir(filepath.Join(dir, ".bma", "shared"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 files, got %d", len(entries))
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") || !strings.HasSuffix(e.Name(), ".md") {
			t.Fatalf("bad filename %q", e.Name())
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

	if !slices.Contains(s.Keys(ctx), key) {
		t.Fatalf("expected key %q in Keys() got %v", key, s.Keys(ctx))
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

// TestFileSharedMemoryStore_Clear 验证 Clear 删全部 MD 文件，供 session 启动清理旧残留。
func TestFileSharedMemoryStore_Clear(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)

	ctx := context.Background()
	for _, k := range []string{"session-1:spec", "session-1:file_tree", "session-1:shared"} {
		if err := s.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	if len(s.Keys(ctx)) != 3 {
		t.Fatalf("expected 3 keys before Clear, got %d", len(s.Keys(ctx)))
	}

	if err := s.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if keys := s.Keys(ctx); len(keys) != 0 {
		t.Fatalf("expected 0 keys after Clear, got %d: %v", len(keys), keys)
	}

	// Clear 空目录幂等。
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("Clear empty should be idempotent, got: %v", err)
	}
}
