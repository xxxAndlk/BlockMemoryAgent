package tool

// schedule_task_test.go 覆盖 schedule_task 工具：
// 未接线报错、参数校验、enabled 缺省 true、hook 透传与摘要返回。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestScheduleTask_NotWired 未注入 hook 时返回未配置错误。
func TestScheduleTask_NotWired(t *testing.T) {
	tl := &scheduleTaskTool{}
	res := tl.Execute(context.Background(), map[string]any{"name": "x", "goal": "y"})
	if res.Error == "" || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got %+v", res)
	}
}

// TestScheduleTask_Validation name/goal 必填校验（不调 hook）。
func TestScheduleTask_Validation(t *testing.T) {
	called := false
	tl := &scheduleTaskTool{hook: func(ctx context.Context, spec ScheduleTaskSpec) (string, error) {
		called = true
		return "ok", nil
	}}
	for _, args := range []map[string]any{
		{"goal": "y"},            // 缺 name
		{"name": "x"},            // 缺 goal
		{"name": "  ", "goal": "y"}, // 空白 name
	} {
		res := tl.Execute(context.Background(), args)
		if res.Error == "" {
			t.Fatalf("expected validation error for %v", args)
		}
	}
	if called {
		t.Fatal("hook should not be called on validation failure")
	}
}

// TestScheduleTask_Execute 正常路径：参数透传 hook，enabled 缺省 true，摘要返回。
func TestScheduleTask_Execute(t *testing.T) {
	var got ScheduleTaskSpec
	tl := &scheduleTaskTool{hook: func(ctx context.Context, spec ScheduleTaskSpec) (string, error) {
		got = spec
		return "已创建定时任务: id=dag-1", nil
	}}
	res := tl.Execute(context.Background(), map[string]any{
		"name": "每日 AI 新闻", "goal": "汇总 AI 新闻生成日报", "cron": "0 9 * * *",
	})
	if !res.Success || !strings.Contains(res.Output, "dag-1") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got.Name != "每日 AI 新闻" || got.Goal != "汇总 AI 新闻生成日报" || got.Cron != "0 9 * * *" {
		t.Fatalf("spec mismatch: %+v", got)
	}
	if !got.Enabled {
		t.Fatal("enabled should default to true")
	}
	if got.ID != "" {
		t.Fatalf("id should be empty for create, got %q", got.ID)
	}

	// 显式 enabled=false + 更新路径 id 透传。
	res = tl.Execute(context.Background(), map[string]any{
		"name": "n", "goal": "g", "id": "dag-9", "enabled": false,
	})
	if !res.Success || got.ID != "dag-9" || got.Enabled {
		t.Fatalf("update path mismatch: %+v spec=%+v", res, got)
	}
}

// TestScheduleTask_HookError hook 错误作为工具错误返回。
func TestScheduleTask_HookError(t *testing.T) {
	tl := &scheduleTaskTool{hook: func(ctx context.Context, spec ScheduleTaskSpec) (string, error) {
		return "", errors.New("调度规则非法: abc")
	}}
	res := tl.Execute(context.Background(), map[string]any{"name": "n", "goal": "g", "cron": "abc"})
	if res.Error != "调度规则非法: abc" {
		t.Fatalf("expected hook error passthrough, got %+v", res)
	}
}
