package tool

// ask_user_test.go 验证 TODO #24 ask_user 工具的 tool 层语义：
// 未接线返错、hook 答复透传、超时转"自行决策"。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAskUserTool_Unwired 未注入 hook 时返回未配置错误。
func TestAskUserTool_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"question": "配色？"})
	if res.Success {
		t.Fatalf("unwired ask_user should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got: %s", res.Error)
	}
}

// TestAskUserTool_AnswerPassThrough hook 返回的原始答复透传为工具结果。
func TestAskUserTool_AnswerPassThrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string) (string, error) {
		if !strings.Contains(question, "配色") {
			t.Fatalf("question should pass through, got: %s", question)
		}
		return "用深色", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"question": "配色？"})
	if !res.Success {
		t.Fatalf("ask_user should succeed, got: %+v", res)
	}
	if !strings.Contains(res.Output, "用户答复: 用深色") {
		t.Fatalf("expected answer in output, got: %s", res.Output)
	}
}

// TestAskUserTool_TimeoutSelfDecision hook 超时未答复 -> "用户未答复，自行决策"。
func TestAskUserTool_TimeoutSelfDecision(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question":    "配色？",
		"timeout_sec": float64(1),
	})
	if !res.Success {
		t.Fatalf("timeout should be a successful self-decision result, got: %+v", res)
	}
	if !strings.Contains(res.Output, "用户未答复，自行决策") {
		t.Fatalf("expected self-decision text, got: %s", res.Output)
	}
}

// TestAskUserTool_HookError 会话取消等 hook 错误 -> 工具失败（ReAct 随 ctx 退出）。
func TestAskUserTool_HookError(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string) (string, error) {
		return "", errors.New("session gone")
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"question": "q"})
	if res.Success {
		t.Fatalf("hook error should fail the tool, got: %+v", res)
	}
	if !strings.Contains(res.Error, "提问中断") {
		t.Fatalf("expected 提问中断 error, got: %s", res.Error)
	}
}

// TestAskUserTool_DefaultTimeout 注册表默认超时生效（timeout_sec 缺省时）。
func TestAskUserTool_DefaultTimeout(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	r.SetAskUserTimeoutDefault(1)
	start := time.Now()
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"question": "q"})
	if !res.Success {
		t.Fatalf("default timeout should produce self-decision result, got: %+v", res)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("default timeout should bound the wait, elapsed=%v", elapsed)
	}
}

// ---- TODO #28 remember_preference ----

// TestRememberPreferenceTool 显式写入画像：hook 收到文本，工具返回"已记住"。
func TestRememberPreferenceTool(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var got string
	r.SetUserProfileHook(func(ctx context.Context, text string) error {
		got = text
		return nil
	})
	res, _ := r.Dispatch(context.Background(), "remember_preference", map[string]any{"text": "直接改别问"})
	if !res.Success || !strings.Contains(res.Output, "已记住") {
		t.Fatalf("expected success with 已记住, got: %+v", res)
	}
	if got != "直接改别问" {
		t.Fatalf("hook should receive text, got %q", got)
	}
}

// TestRememberPreferenceTool_Unwired 未接线返回未配置错误。
func TestRememberPreferenceTool_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(context.Background(), "remember_preference", map[string]any{"text": "x"})
	if res.Success || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("unwired should fail, got: %+v", res)
	}
}

// ---- TODO #27 search_knowledge ----

// TestSearchKnowledgeTool hook 命中格式化透传。
func TestSearchKnowledgeTool(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetKnowledgeSearchHook(func(ctx context.Context, query string, topK int) (string, error) {
		return "外部知识库命中 1 条：\n--- [1] external:wiki.md ---\nCanvas 2D 渲染", nil
	})
	res, _ := r.Dispatch(context.Background(), "search_knowledge", map[string]any{"query": "渲染引擎", "top_k": float64(3)})
	if !res.Success || !strings.Contains(res.Output, "Canvas 2D") {
		t.Fatalf("expected hit output, got: %+v", res)
	}
}

// TestSearchKnowledgeTool_Unwired 未接线返回未配置错误。
func TestSearchKnowledgeTool_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(context.Background(), "search_knowledge", map[string]any{"query": "x"})
	if res.Success || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("unwired should fail, got: %+v", res)
	}
}
