package tool

// 读取状态追踪（advisory 提示版）单元测试：
// 首读登记、mtime 未变重读附提示、mtime 变更附提示/写前警告、未读过写前警告、Agent 间隔离。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readStateUnchangedNote 是"mtime 未变重复读取"提示的关键片段。
const readStateUnchangedNote = "无需为重读而重读"

// readStateChangedNote 是"文件已被修改"提示的关键片段。
const readStateChangedNote = "以本次返回为准"

// readStateStaleWriteNote 是写前 stale 警告的关键片段。
const readStateStaleWriteNote = "old_string 可能基于过期内容"

// writeTestFile 写入测试文件并用 Chtimes 固定一个过去的 mtime，
// 避免文件系统 mtime 粒度导致"修改前后 mtime 相同"的偶发误判。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// TestReadState_FirstReadRegisters 首读登记：第一次成功 ReadFile 不附任何新鲜度提示，
// 但已把 path → mtime 登记到该 scope 的读取记录。
func TestReadState_FirstReadRegisters(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "v1")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}
	// 首读无历史记录可比，不附提示。
	if strings.Contains(res.Output, readStateUnchangedNote) || strings.Contains(res.Output, readStateChangedNote) {
		t.Fatalf("first read should carry no freshness note, got: %s", res.Output)
	}
	// 登记成功：记录的 mtime 与磁盘一致。
	mt, ok := r.lastFileMtime("agent-1", target)
	if !ok {
		t.Fatal("first read should register mtime record")
	}
	if disk, _ := statMtime(target); !mt.Equal(disk) {
		t.Fatalf("registered mtime %v should equal disk mtime %v", mt, disk)
	}
}

// TestReadState_RereadUnchanged 重复读取且 mtime 未变：结果尾部附
// "自你上次读取后未变更，请直接引用上文"提示（不同参数翻页，绕开连读守卫的同参数提醒）。
func TestReadState_RereadUnchanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "line1\nline2\nline3\n")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	if res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "limit": float64(2)}); err != nil || !res.Success {
		t.Fatalf("first read: err=%v success=%v", err, res.Success)
	}
	// 换 limit 重读（参数不同，不触发连读守卫提醒），mtime 未变应附"未变更"提示。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "limit": float64(3)})
	if err != nil || !res.Success {
		t.Fatalf("second read: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, readStateUnchangedNote) {
		t.Fatalf("unchanged reread should carry note %q, got: %s", readStateUnchangedNote, res.Output)
	}
	if strings.Contains(res.Output, readStateChangedNote) {
		t.Fatalf("unchanged reread should not carry changed note, got: %s", res.Output)
	}
}

// TestReadState_RereadChanged 重复读取时 mtime 已变（期间被外部修改）：
// 附"已被修改，以本次返回为准"提示。
func TestReadState_RereadChanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "line1\nline2\nline3\n")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	if res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "limit": float64(2)}); err != nil || !res.Success {
		t.Fatalf("first read: err=%v success=%v", err, res.Success)
	}
	// 模拟外部修改：内容变更并把 mtime 拨到当前（与登记值必然不同）。
	if err := os.WriteFile(target, []byte("line1\nline2 changed\nline3\n"), 0644); err != nil {
		t.Fatalf("external write: %v", err)
	}
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "limit": float64(3)})
	if err != nil || !res.Success {
		t.Fatalf("second read: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, readStateChangedNote) {
		t.Fatalf("changed reread should carry note %q, got: %s", readStateChangedNote, res.Output)
	}
}

// TestReadState_EditWithoutReadWarns 未读过就 EditFile：操作仍执行成功，
// 结果尾部附 stale 警告；读过且未变更后再 EditFile 则不附任何多余内容。
func TestReadState_EditWithoutReadWarns(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "hello world")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	// 从未读过直接编辑：成功但附警告。
	res, err := r.Dispatch(ctx, "EditFile", map[string]any{"path": "a.txt", "old_string": "world", "new_string": "there"})
	if err != nil || !res.Success {
		t.Fatalf("edit without read should still execute: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, readStateStaleWriteNote) {
		t.Fatalf("edit without read should carry stale warning, got: %s", res.Output)
	}

	// 编辑后该 Agent 已登记新 mtime，紧接着再次自编辑不应误报 stale。
	res2, err := r.Dispatch(ctx, "EditFile", map[string]any{"path": "a.txt", "old_string": "there", "new_string": "again"})
	if err != nil || !res2.Success {
		t.Fatalf("second edit: err=%v success=%v", err, res2.Success)
	}
	if strings.Contains(res2.Output, readStateStaleWriteNote) {
		t.Fatalf("self-edit after own write should not warn, got: %s", res2.Output)
	}
}

// TestReadState_EditAfterExternalChangeWarns 读过之后文件被外部修改（mtime 晚于上次读取），
// 再 EditFile：操作仍执行，结果尾部附"old_string 可能基于过期内容"警告。
func TestReadState_EditAfterExternalChangeWarns(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "hello world")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	if res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"}); err != nil || !res.Success {
		t.Fatalf("read: err=%v success=%v", err, res.Success)
	}
	// 读后外部修改（mtime 更新）。
	if err := os.WriteFile(target, []byte("hello world\nexternal line"), 0644); err != nil {
		t.Fatalf("external write: %v", err)
	}
	res, err := r.Dispatch(ctx, "EditFile", map[string]any{"path": "a.txt", "old_string": "hello world", "new_string": "hi world"})
	if err != nil || !res.Success {
		t.Fatalf("edit after external change should still execute: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, readStateStaleWriteNote) {
		t.Fatalf("edit after external change should carry stale warning, got: %s", res.Output)
	}
}

// TestReadState_PerAgentIsolation 读取状态按 Agent 隔离：
// agent-1 读过不影响 agent-2——agent-2 未读过直接编辑仍附警告；
// agent-1 的写入使 agent-2（读过旧版）的记录变 stale，而 agent-1 自己后续编辑不警告。
func TestReadState_PerAgentIsolation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "hello world")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx1 := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")
	ctx2 := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-2")

	// agent-1 读过；agent-2 未读过直接编辑 → agent-2 附警告。
	if res, err := r.Dispatch(ctx1, "ReadFile", map[string]any{"path": "a.txt"}); err != nil || !res.Success {
		t.Fatalf("agent-1 read: err=%v success=%v", err, res.Success)
	}
	res2, err := r.Dispatch(ctx2, "EditFile", map[string]any{"path": "a.txt", "old_string": "world", "new_string": "there"})
	if err != nil || !res2.Success {
		t.Fatalf("agent-2 edit: err=%v success=%v", err, res2.Success)
	}
	if !strings.Contains(res2.Output, readStateStaleWriteNote) {
		t.Fatalf("agent-2 (never read) edit should warn, got: %s", res2.Output)
	}
	// agent-2 的编辑不污染 agent-1 的记录：agent-1 的记录已变 stale（文件被 agent-2 改过），
	// agent-1 重读应收到"已被修改"提示而非"未变更"。
	res1, err := r.Dispatch(ctx1, "ReadFile", map[string]any{"path": "a.txt", "limit": float64(9)})
	if err != nil || !res1.Success {
		t.Fatalf("agent-1 reread: err=%v success=%v", err, res1.Success)
	}
	if !strings.Contains(res1.Output, readStateChangedNote) {
		t.Fatalf("agent-1 reread after agent-2 edit should report changed, got: %s", res1.Output)
	}
}

// TestReadState_ResetClears ResetReadHistory 同时清空读取状态追踪记录：
// 重置后再次编辑视为"从未读过"，恢复 stale 警告。
func TestReadState_ResetClears(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	writeTestFile(t, target, "hello world")
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "agent-1")

	if res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"}); err != nil || !res.Success {
		t.Fatalf("read: err=%v success=%v", err, res.Success)
	}
	r.ResetReadHistory("agent-1")
	if _, ok := r.lastFileMtime("agent-1", target); ok {
		t.Fatal("ResetReadHistory should clear fileReadMtime record")
	}
	res, err := r.Dispatch(ctx, "EditFile", map[string]any{"path": "a.txt", "old_string": "world", "new_string": "there"})
	if err != nil || !res.Success {
		t.Fatalf("edit after reset: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, readStateStaleWriteNote) {
		t.Fatalf("edit after reset (never-read state) should warn, got: %s", res.Output)
	}
}
