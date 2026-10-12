package tool

// block_memory_search_test.go 验证 search_block_memory 工具的 tool 层语义：
// query 必填校验、hook 未接线返错、正常路径 query 透传与 hook 返回值原样回传。

import (
	"context"
	"strings"
	"testing"
)

// TestSearchBlockMemory_EmptyQuery 空/全空白 query 拒收且不触达 hook。
func TestSearchBlockMemory_EmptyQuery(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	called := false
	r.SetBlockMemorySearchHook(func(ctx context.Context, query string) (string, error) {
		called = true
		return "ok", nil
	})
	for _, args := range []map[string]any{
		{},              // 缺 query
		{"query": ""},   // 空串
		{"query": "  "}, // 全空白
	} {
		res, _ := r.Dispatch(context.Background(), "search_block_memory", args)
		if res.Success {
			t.Fatalf("args %v should be rejected", args)
		}
	}
	if called {
		t.Fatal("hook must not be called for empty query")
	}
}

// TestSearchBlockMemory_Unwired hook 未注入时返回未接线错误。
func TestSearchBlockMemory_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(context.Background(), "search_block_memory", map[string]any{"query": "渲染引擎"})
	if res.Success {
		t.Fatalf("unwired search_block_memory should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got: %s", res.Error)
	}
}

// TestSearchBlockMemory_PassThrough query 原样透传 hook，hook 返回值原样作为 Output 回传。
func TestSearchBlockMemory_PassThrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var gotQuery string
	r.SetBlockMemorySearchHook(func(ctx context.Context, query string) (string, error) {
		gotQuery = query
		return "块记忆命中 1 条：\n--- [1] 塔防项目 ---\ndrawTower 签名 (ctx, x, y)", nil
	})
	res, _ := r.Dispatch(context.Background(), "search_block_memory", map[string]any{"query": " 塔防 drawTower 签名 "})
	if !res.Success {
		t.Fatalf("search should succeed, got: %+v", res)
	}
	if gotQuery != "塔防 drawTower 签名" {
		t.Fatalf("query should pass through trimmed, got: %q", gotQuery)
	}
	if res.Output != "块记忆命中 1 条：\n--- [1] 塔防项目 ---\ndrawTower 签名 (ctx, x, y)" {
		t.Fatalf("hook output should pass back verbatim, got: %s", res.Output)
	}
}

// TestSearchBlockMemory_HookError hook 返回错误转工具级错误。
func TestSearchBlockMemory_HookError(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetBlockMemorySearchHook(func(ctx context.Context, query string) (string, error) {
		return "", context.Canceled
	})
	res, _ := r.Dispatch(context.Background(), "search_block_memory", map[string]any{"query": "x"})
	if res.Success || !strings.Contains(res.Error, "检索失败") {
		t.Fatalf("hook error should surface, got: %+v", res)
	}
}
