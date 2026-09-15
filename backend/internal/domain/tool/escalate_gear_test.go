package tool

// escalate_gear_test.go 验证 TODO #14 T7 escalate_gear 工具的 tool 层语义：
// 未接线返错、参数必填校验、hook 入参透传（reason/task_brief/files）与结果回传。

import (
	"context"
	"strings"
	"testing"
)

// TestEscalateGearTool_Unwired 未注入 hook 时返回未配置错误。
func TestEscalateGearTool_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ := r.Dispatch(context.Background(), "escalate_gear", map[string]any{
		"reason": "需要改代码", "task_brief": "修 bug",
	})
	if res.Success {
		t.Fatalf("unwired escalate_gear should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got: %s", res.Error)
	}
}

// TestEscalateGearTool_RequiredArgs reason/task_brief 缺失拒收且不触达 hook。
func TestEscalateGearTool_RequiredArgs(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	called := false
	r.SetEscalateGearHook(func(ctx context.Context, req EscalateGearRequest) (string, error) {
		called = true
		return "ok", nil
	})
	for _, args := range []map[string]any{
		{"task_brief": "修 bug"},                       // 缺 reason
		{"reason": "要改文件"},                            // 缺 task_brief
		{"reason": "   ", "task_brief": "修 bug"},      // 空白 reason
		{"reason": "要改文件", "task_brief": ""},         // 空 brief
	} {
		res, _ := r.Dispatch(context.Background(), "escalate_gear", args)
		if res.Success {
			t.Fatalf("args %v should be rejected", args)
		}
	}
	if called {
		t.Fatal("hook must not be called for invalid args")
	}
}

// TestEscalateGearTool_PassThrough 入参透传 hook、结果回传模型；files 数组宽容解析。
func TestEscalateGearTool_PassThrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var got EscalateGearRequest
	r.SetEscalateGearHook(func(ctx context.Context, req EscalateGearRequest) (string, error) {
		got = req
		return "用户已确认升级集群档。", nil
	})
	res, _ := r.Dispatch(context.Background(), "escalate_gear", map[string]any{
		"reason":     "需要跨 3 个文件改接口",
		"task_brief": "给 orders 表加 status 索引并同步后端查询",
		"files":      []any{"backend/order.go", "migrations/010.sql", "  ", 42},
	})
	if !res.Success {
		t.Fatalf("escalate should succeed, got: %+v", res)
	}
	if got.Reason != "需要跨 3 个文件改接口" || got.TaskBrief == "" {
		t.Fatalf("hook args mismatch: %+v", got)
	}
	if len(got.Files) != 2 || got.Files[0] != "backend/order.go" {
		t.Fatalf("files should keep non-empty strings only, got: %v", got.Files)
	}
	if !strings.Contains(res.Output, "已确认升级集群档") {
		t.Fatalf("hook output should pass back, got: %s", res.Output)
	}
}

// TestEscalateGearTool_HookError hook 返回错误转工具级错误（不中止循环由 agent 层语义决定）。
func TestEscalateGearTool_HookError(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetEscalateGearHook(func(ctx context.Context, req EscalateGearRequest) (string, error) {
		return "", context.Canceled
	})
	res, _ := r.Dispatch(context.Background(), "escalate_gear", map[string]any{
		"reason": "x", "task_brief": "y",
	})
	if res.Success || !strings.Contains(res.Error, "中断") {
		t.Fatalf("hook error should surface, got: %+v", res)
	}
}
