package tool

// evolution_prompt_test.go 验证 update_evolution_prompt 工具的 tool 层语义：
// 角色上下文归属（无 roleID 拒绝）、300 字硬上限（恰好 300 通过 / 301 拒绝且错误含字数）、
// 空内容拒收、hook 未接线返错、正常路径 hook 入参（roleID/content/sessionID）与结果回显。

import (
	"context"
	"strings"
	"testing"
)

// evolutionCtx 构造携带会话与角色 ID 的上下文（同包测试直接用 WithRoleID/WithSessionID）。
func evolutionCtx() context.Context {
	return WithRoleID(WithSessionID(context.Background(), "s1"), "meta")
}

// TestUpdateEvolutionPrompt_NoRole ctx 无角色 ID 时拒绝写入（无法定位准则归属），且不触达 hook。
func TestUpdateEvolutionPrompt_NoRole(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	called := false
	r.SetEvolutionPromptHook(func(ctx context.Context, roleID, content, sessionID string) error {
		called = true
		return nil
	})
	res, _ := r.Dispatch(context.Background(), "update_evolution_prompt", map[string]any{
		"content": "修改代码后必须跑构建验证再交付",
	})
	if res.Success {
		t.Fatalf("no-role call should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "缺少角色上下文") {
		t.Fatalf("expected 缺少角色上下文 error, got: %s", res.Error)
	}
	if called {
		t.Fatal("hook must not be called without role context")
	}
}

// TestUpdateEvolutionPrompt_EmptyContent 空/全空白 content 拒收且不触达 hook。
func TestUpdateEvolutionPrompt_EmptyContent(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	called := false
	r.SetEvolutionPromptHook(func(ctx context.Context, roleID, content, sessionID string) error {
		called = true
		return nil
	})
	for _, args := range []map[string]any{
		{},                   // 缺 content
		{"content": ""},      // 空串
		{"content": "   \n"}, // 全空白
	} {
		res, _ := r.Dispatch(evolutionCtx(), "update_evolution_prompt", args)
		if res.Success {
			t.Fatalf("args %v should be rejected", args)
		}
	}
	if called {
		t.Fatal("hook must not be called for empty content")
	}
}

// TestUpdateEvolutionPrompt_RuneLimit 300 字硬上限边界：恰好 300 字通过，301 字拒绝且错误含当前字数。
func TestUpdateEvolutionPrompt_RuneLimit(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var gotContent string
	calls := 0
	r.SetEvolutionPromptHook(func(ctx context.Context, roleID, content, sessionID string) error {
		calls++
		gotContent = content
		return nil
	})
	// 恰好 300 字：放行。
	ok := strings.Repeat("准", 300)
	res, _ := r.Dispatch(evolutionCtx(), "update_evolution_prompt", map[string]any{"content": ok})
	if !res.Success {
		t.Fatalf("300-rune content should pass, got: %+v", res)
	}
	if gotContent != ok {
		t.Fatalf("hook should receive full 300-rune content, got %d runes", len([]rune(gotContent)))
	}
	// 301 字：拒绝，错误含字数，且不再触达 hook。
	tooLong := strings.Repeat("准", 301)
	res, _ = r.Dispatch(evolutionCtx(), "update_evolution_prompt", map[string]any{"content": tooLong})
	if res.Success {
		t.Fatalf("301-rune content should be rejected, got: %+v", res)
	}
	if !strings.Contains(res.Error, "301") {
		t.Fatalf("rejection should carry current rune count, got: %s", res.Error)
	}
	if calls != 1 {
		t.Fatalf("over-limit content must not reach hook, calls=%d", calls)
	}
}

// TestUpdateEvolutionPrompt_Unwired hook 未注入时返回未接线错误。
func TestUpdateEvolutionPrompt_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(evolutionCtx(), "update_evolution_prompt", map[string]any{
		"content": "修改代码后必须跑构建验证再交付",
	})
	if res.Success {
		t.Fatalf("unwired update_evolution_prompt should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got: %s", res.Error)
	}
}

// TestUpdateEvolutionPrompt_Success 正常路径：hook 收到 roleID/content/sessionID，结果回显字数与全文。
func TestUpdateEvolutionPrompt_Success(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var gotRole, gotContent, gotSession string
	r.SetEvolutionPromptHook(func(ctx context.Context, roleID, content, sessionID string) error {
		gotRole, gotContent, gotSession = roleID, content, sessionID
		return nil
	})
	content := "修改代码后必须跑构建验证再交付" // 15 字
	res, _ := r.Dispatch(evolutionCtx(), "update_evolution_prompt", map[string]any{"content": content})
	if !res.Success {
		t.Fatalf("update should succeed, got: %+v", res)
	}
	if gotRole != "meta" || gotSession != "s1" || gotContent != content {
		t.Fatalf("hook args mismatch: role=%q session=%q content=%q", gotRole, gotSession, gotContent)
	}
	if !strings.Contains(res.Output, "15 字") || !strings.Contains(res.Output, content) {
		t.Fatalf("output should echo rune count and content, got: %s", res.Output)
	}
}

// TestUpdateEvolutionPrompt_HookError hook 返回错误转工具级错误。
func TestUpdateEvolutionPrompt_HookError(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetEvolutionPromptHook(func(ctx context.Context, roleID, content, sessionID string) error {
		return context.Canceled
	})
	res, _ := r.Dispatch(evolutionCtx(), "update_evolution_prompt", map[string]any{"content": "x"})
	if res.Success || !strings.Contains(res.Error, "写入失败") {
		t.Fatalf("hook error should surface, got: %+v", res)
	}
}
