package agent

import (
	"fmt"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
)

func wireSeqs(items []map[string]any) []int {
	out := make([]int, 0, len(items))
	for _, it := range items {
		out = append(out, it["seq"].(int))
	}
	return out
}

// TestSliceMessagesWire 验证 PG 全量切片的三种窗口语义（tail/before/after），
// 与 Redis 热层 beforeWindow/afterWindow 口径一致。
func TestSliceMessagesWire(t *testing.T) {
	msgs := make([]ReactMessage, 0, 10)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, ReactMessage{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 0, 0, 3)); fmt.Sprint(got) != "[7 8 9]" {
		t.Fatalf("tail 3 应 [7 8 9], got %v", got)
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 7, 0, 2)); fmt.Sprint(got) != "[5 6]" {
		t.Fatalf("before 7 limit 2 应 [5 6], got %v", got)
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 0, 7, 5)); fmt.Sprint(got) != "[8 9]" {
		t.Fatalf("after 7 应 [8 9], got %v", got)
	}
	if got := sliceMessagesWire(msgs, 0, 9, 5); len(got) != 0 {
		t.Fatalf("after 9 应空, got %v", got)
	}
}

// TestEntriesToWire 验证热层条目转线型：tool_calls JSON 反序列化为数组，空串省略。
func TestEntriesToWire(t *testing.T) {
	in := []store.AgentMsgEntry{
		{Seq: 0, At: "2026-09-11T00:00:00Z", Role: "user", Content: "hi"},
		{Seq: 1, At: "2026-09-11T00:00:01Z", Role: "assistant", Content: "ok", ToolCalls: `[{"id":"c1","name":"ReadFile","input":{"path":"a.go"}}]`},
	}
	got := entriesToWire(in)
	if len(got) != 2 {
		t.Fatalf("应 2 条, got %d", len(got))
	}
	if _, has := got[0]["tool_calls"]; has {
		t.Fatal("空 tool_calls 不应出现在线型里")
	}
	calls, ok := got[1]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 应反序列化为数组, got %#v", got[1]["tool_calls"])
	}
}
