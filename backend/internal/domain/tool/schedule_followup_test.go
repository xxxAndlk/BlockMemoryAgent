package tool

// schedule_followup_test.go 覆盖 schedule_followup 工具：
// 未接线报错、ctx 会话注入校验、参数校验、hook 透传与摘要返回。

import (
	"context"
	"strings"
	"testing"
)

// TestScheduleFollowup_NotWired 未注入 hook 时返回未配置错误。
func TestScheduleFollowup_NotWired(t *testing.T) {
	tl := &scheduleFollowupTool{}
	res := tl.Execute(context.Background(), map[string]any{"delay_minutes": 60, "text": "x"})
	if res.Error == "" || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("expected 未接线 error, got %+v", res)
	}
}

// TestScheduleFollowup_NoSession  ctx 未注入会话 ID 时报错（不调 hook）。
func TestScheduleFollowup_NoSession(t *testing.T) {
	called := false
	tl := &scheduleFollowupTool{hook: func(ctx context.Context, sid string, delayMin int, text string) (string, error) {
		called = true
		return "ok", nil
	}}
	res := tl.Execute(context.Background(), map[string]any{"delay_minutes": 60, "text": "x"})
	if res.Error == "" || !strings.Contains(res.Error, "session") {
		t.Fatalf("expected session error, got %+v", res)
	}
	if called {
		t.Fatal("hook should not be called without session id")
	}
}

// TestScheduleFollowup_Validation 参数必填与取值校验（不调 hook）。
func TestScheduleFollowup_Validation(t *testing.T) {
	called := false
	tl := &scheduleFollowupTool{hook: func(ctx context.Context, sid string, delayMin int, text string) (string, error) {
		called = true
		return "ok", nil
	}}
	ctx := WithSessionID(context.Background(), "s1")
	for _, args := range []map[string]any{
		{"text": "x"},                              // 缺 delay_minutes
		{"delay_minutes": 0.5, "text": "x"},        // 间隔过小
		{"delay_minutes": 60},                      // 缺 text
		{"delay_minutes": 60, "text": "   "},       // 空白 text
	} {
		res := tl.Execute(ctx, args)
		if res.Error == "" {
			t.Fatalf("expected validation error for %v", args)
		}
	}
	if called {
		t.Fatal("hook should not be called on validation failure")
	}
}

// TestScheduleFollowup_Execute 正常路径：会话 ID 从 ctx 透传，delay 四舍五入，摘要返回。
func TestScheduleFollowup_Execute(t *testing.T) {
	var gotSID string
	var gotDelay int
	var gotText string
	tl := &scheduleFollowupTool{hook: func(ctx context.Context, sid string, delayMin int, text string) (string, error) {
		gotSID, gotDelay, gotText = sid, delayMin, text
		return "已安排 2026-10-19 09:00 回查：检查审核", nil
	}}
	ctx := WithSessionID(context.Background(), "sess-9")
	res := tl.Execute(ctx, map[string]any{"delay_minutes": 1440.4, "text": "检查审核"})
	if !res.Success || !strings.Contains(res.Output, "检查审核") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if gotSID != "sess-9" || gotDelay != 1440 || gotText != "检查审核" {
		t.Fatalf("hook args mismatch: sid=%s delay=%d text=%s", gotSID, gotDelay, gotText)
	}
}
