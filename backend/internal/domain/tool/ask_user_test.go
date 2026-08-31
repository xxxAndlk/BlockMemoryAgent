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
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		if !strings.Contains(question, "配色") {
			t.Fatalf("question should pass through, got: %s", question)
		}
		if len(opts.Options) != 0 {
			t.Fatalf("no options passed, got: %+v", opts.Options)
		}
		return "用深色", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"question": "配色？"})
	if !res.Success {
		t.Fatalf("ask_user should succeed, got: %+v", res)
	}
	if !strings.Contains(res.Output, "答复: 用深色") {
		t.Fatalf("expected answer in output, got: %s", res.Output)
	}
}

// TestAskUserTool_TimeoutSelfDecision hook 超时未答复 -> "用户未答复，自行决策"。
func TestAskUserTool_TimeoutSelfDecision(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
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
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
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
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
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

// TestAskUserTool_OptionsPassThrough 结构化选项透传（TODO #53）：
// options/multi_select 入参解析后交 hook，多选标记透传。
func TestAskUserTool_OptionsPassThrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		if !opts.MultiSelect {
			t.Fatal("multi_select=true 应透传")
		}
		if len(opts.Options) != 3 {
			t.Fatalf("expected 3 options, got %d: %+v", len(opts.Options), opts.Options)
		}
		want := []AskUserOption{
			{ID: "dark", Label: "深色", Description: "护眼"},
			{ID: "light", Label: "浅色"},
			{ID: "auto", Label: "跟随系统"},
		}
		for i, w := range want {
			if opts.Options[i] != w {
				t.Fatalf("option %d mismatch: got %+v want %+v", i, opts.Options[i], w)
			}
		}
		return "深色", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question":     "配色？",
		"multi_select": true,
		"options": []map[string]any{
			{"id": "dark", "label": "深色", "description": "护眼"},
			{"id": "light", "label": "浅色"},
			{"id": "auto", "label": "跟随系统"},
		},
	})
	if !res.Success {
		t.Fatalf("ask_user with options should succeed, got: %+v", res)
	}
	if !strings.Contains(res.Output, "答复: 深色") {
		t.Fatalf("expected answer in output, got: %s", res.Output)
	}
}

// TestAskUserTool_OptionsGarbageIgnored 入参 options 格式损坏时降级为空选项（自由文本），不报错。
func TestAskUserTool_OptionsGarbageIgnored(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		if len(opts.Options) != 0 {
			t.Fatalf("garbage options should be dropped, got: %+v", opts.Options)
		}
		return "ok", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question": "q",
		"options":  []any{"not-a-map", map[string]any{"id": "", "label": "x"}},
	})
	if !res.Success {
		t.Fatalf("garbage options should not fail the tool, got: %+v", res)
	}
}

// TestAskUserTool_BatchQuestions 批量模式：questions 数组逐题调 hook，答案按题号汇总。
func TestAskUserTool_BatchQuestions(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var asked []string
	answers := map[string]string{"技术方案？": "单文件HTML", "几张地图？": "3张"}
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		asked = append(asked, question)
		return answers[question], nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"questions": []any{
			map[string]any{
				"question": "技术方案？",
				"options": []any{
					map[string]any{"id": "single", "label": "单文件HTML"},
					map[string]any{"id": "phaser", "label": "Phaser"},
				},
			},
			map[string]any{"question": "几张地图？"},
		},
	})
	if !res.Success {
		t.Fatalf("batch ask_user should succeed, got: %+v", res)
	}
	if len(asked) != 2 || asked[0] != "技术方案？" || asked[1] != "几张地图？" {
		t.Fatalf("questions should be asked in order, got: %v", asked)
	}
	if !strings.Contains(res.Output, "1. 答复: 单文件HTML") ||
		!strings.Contains(res.Output, "2. 答复: 3张") {
		t.Fatalf("expected numbered answers, got: %s", res.Output)
	}
}

// TestAskUserTool_BatchTimeoutContinues 批量中单题超时不中断：记"自行决策"继续下一题。
func TestAskUserTool_BatchTimeoutContinues(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var asked []string
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		asked = append(asked, question)
		if question == "卡住？" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "答案", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"timeout_sec": float64(1),
		"questions": []any{
			map[string]any{"question": "卡住？"},
			map[string]any{"question": "正常？"},
		},
	})
	if !res.Success {
		t.Fatalf("batch with one timeout should still succeed, got: %+v", res)
	}
	if len(asked) != 2 {
		t.Fatalf("second question should still be asked, asked: %v", asked)
	}
	if !strings.Contains(res.Output, "1. 答复: 用户未答复，自行决策。") ||
		!strings.Contains(res.Output, "2. 答复: 答案") {
		t.Fatalf("expected timeout note + continuing answer, got: %s", res.Output)
	}
}

// TestAskUserTool_QuestionsPreferredOverTopLevel questions 与顶层 question 并存时 questions 优先。
func TestAskUserTool_QuestionsPreferredOverTopLevel(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var asked []string
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		asked = append(asked, question)
		return "ok", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question":  "顶层问题？",
		"questions": []any{map[string]any{"question": "批量问题1？"}, map[string]any{"question": "批量问题2？"}},
	})
	if !res.Success {
		t.Fatalf("should succeed, got: %+v", res)
	}
	if len(asked) != 2 || asked[0] != "批量问题1？" {
		t.Fatalf("questions should take precedence, asked: %v", asked)
	}
}

// TestAskUserTool_BatchCap 超过 5 题截断到上限。
func TestAskUserTool_BatchCap(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	n := 0
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		n++
		return "ok", nil
	})
	qs := make([]any, 0, 8)
	for range 8 {
		qs = append(qs, map[string]any{"question": "q"})
	}
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{"questions": qs})
	if !res.Success || n != maxAskUserBatch {
		t.Fatalf("expected %d questions asked, got %d, res: %+v", maxAskUserBatch, n, res)
	}
}

// TestAskUserTool_DetailPassthrough detail 参数（计划全文等长上下文）透传 hook。
func TestAskUserTool_DetailPassthrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var gotDetail string
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		gotDetail = opts.Detail
		return "ok", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question": "确认？",
		"detail":   "计划全文……",
	})
	if !res.Success {
		t.Fatalf("should succeed, got: %+v", res)
	}
	if gotDetail != "计划全文……" {
		t.Fatalf("detail 应透传 hook, got %q", gotDetail)
	}
}

// TestAskUserTool_Schema 入参 schema 含 question/options/multi_select/timeout_sec（TODO #53）。
func TestAskUserTool_Schema(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	found := false
	for _, s := range r.Schema() {
		if s.Name() != "ask_user" {
			continue
		}
		found = true
	}
	if !found {
		t.Fatal("ask_user 应出现在 Registry.Schema()（TODO #53 实现 SchemaSource 后）")
	}
	toolInst, ok := r.toolByName("ask_user")
	if !ok {
		t.Fatal("ask_user 应已注册")
	}
	src, ok := toolInst.(SchemaSource)
	if !ok {
		t.Fatal("ask_user 应实现 SchemaSource")
	}
	props := src.InputSchema().Properties
	for _, want := range []string{"question", "options", "multi_select", "timeout_sec", "questions"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("ask_user schema missing parameter %q (have %v)", want, keysOf(props))
		}
	}
	if len(src.InputSchema().Required) != 1 || src.InputSchema().Required[0] != "question" {
		t.Fatalf("question 应为唯一必填项, got %v", src.InputSchema().Required)
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
