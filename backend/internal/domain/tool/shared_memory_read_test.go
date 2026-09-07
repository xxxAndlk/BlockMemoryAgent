package tool

import (
	"context"
	"strings"
	"testing"
)

// TestReadSharedMemory_ListAndResolveOwnSlot 验证：无 key 枚举槽位摘要；
// 仅 slot 入参自动用本 Agent 前缀解析（Meta 读自己写的槽位）。
func TestReadSharedMemory_ListAndResolveOwnSlot(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	if res, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"key":     "contract",
		"content": "root=D:/proj, api=Foo.Bar",
	}); err != nil || !res.Success {
		t.Fatalf("seed write failed: res=%v err=%v", res, err)
	}

	// 无 key：列出槽位名与字数。
	res, err := r.Dispatch(ctx, "ReadSharedMemory", map[string]any{})
	if err != nil || !res.Success {
		t.Fatalf("list failed: res=%v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "meta-1:contract") {
		t.Fatalf("list output missing slot key, got: %s", res.Output)
	}

	// 仅 slot：解析为本 Agent 前缀全键，正文为写入 content。
	res, err = r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "contract"})
	if err != nil || !res.Success {
		t.Fatalf("read own slot failed: res=%v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "api=Foo.Bar") || strings.Contains(res.Output, "---") {
		t.Fatalf("read output should be decoded body, got: %s", res.Output)
	}
}

// TestReadSharedMemory_UniqueCandidateFallback 验证：本 Agent 前缀未命中时，
// 全库唯一同名 slot 候选回退（跨 Agent 读沉淀），多候选时拒绝避免读错。
func TestReadSharedMemory_UniqueCandidateFallback(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	if err := store.Set(ctx, "session-1/domain-2:conclusion", "结论：根因是端口占用"); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	// Meta 读自己没有的 slot "conclusion"：唯一候选回退命中。
	res, err := r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "conclusion"})
	if err != nil || !res.Success || !strings.Contains(res.Output, "端口占用") {
		t.Fatalf("unique fallback failed: res=%v err=%v", res, err)
	}

	// meta-1 前缀优先：写入本人同名 slot 后，读回的是本人内容而非回退候选。
	if err := store.Set(ctx, "meta-1:conclusion", "meta自己的"); err != nil {
		t.Fatalf("seed set2: %v", err)
	}
	res, err = r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "conclusion"})
	if err != nil || !res.Success || !strings.Contains(res.Output, "meta自己的") {
		t.Fatalf("own-prefix priority failed: res=%v err=%v", res, err)
	}

	// 第三方 Agent（前缀未命中 + 全库两个同名候选）：拒绝歧义回退，报错列出槽位。
	third := WithAgentID(ctx, "session-9/other")
	res, err = r.Dispatch(third, "ReadSharedMemory", map[string]any{"key": "conclusion"})
	if err != nil {
		t.Fatalf("dispatch third-party: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "不存在") {
		t.Fatalf("expected ambiguous-candidate rejection, got res=%+v", res)
	}
}

// TestReadSharedMemory_MissingKeyAndTruncate 验证：key 不存在报错附槽位清单；
// 超长正文截断保护上下文。
func TestReadSharedMemory_MissingKeyAndTruncate(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "nope"})
	if err != nil || res.Success {
		t.Fatalf("expected error for missing key, got res=%v err=%v", res, err)
	}
	if !strings.Contains(res.Error, "不存在") {
		t.Fatalf("error should mention missing key, got: %s", res.Error)
	}

	big := strings.Repeat("字", readSharedMemoryTruncateLimit+500)
	if err := store.Set(ctx, "meta-1:big", encodeSharedMD("meta-1", "big", nil, big)); err != nil {
		t.Fatalf("seed big: %v", err)
	}
	res, err = r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "big"})
	if err != nil || !res.Success {
		t.Fatalf("read big failed: res=%v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "[截断") {
		t.Fatalf("expected truncation marker, got len=%d", len([]rune(res.Output)))
	}
}

// TestReadSharedMemory_NoStore 验证：store 未注入时报错而非 panic。
func TestReadSharedMemory_NoStore(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")
	res, err := r.Dispatch(ctx, "ReadSharedMemory", map[string]any{"key": "x"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "not configured") {
		t.Fatalf("expected not-configured error, got: %+v", res)
	}
}
