package store

import (
	"context"
	"testing"
)

// TestAgentMsgRedisStore_NilNoOp 验证 nil store 全部方法 no-op 不 panic（测试/未接线场景）。
func TestAgentMsgRedisStore_NilNoOp(t *testing.T) {
	var s *AgentMsgRedisStore
	if err := s.AppendMsg(context.Background(), "session-1/domain-1", AgentMsgEntry{Seq: 0}); err != nil {
		t.Fatalf("nil AppendMsg 应 no-op, got %v", err)
	}
	if got, err := s.TailMsg(context.Background(), "session-1/domain-1", 10); err != nil || got != nil {
		t.Fatalf("nil TailMsg 应 nil,nil, got %v,%v", got, err)
	}
	if got, err := s.BeforeMsg(context.Background(), "session-1/domain-1", 5, 10); err != nil || got != nil {
		t.Fatalf("nil BeforeMsg 应 nil,nil, got %v,%v", got, err)
	}
	if got, err := s.AfterMsg(context.Background(), "session-1/domain-1", 5, 10); err != nil || got != nil {
		t.Fatalf("nil AfterMsg 应 nil,nil, got %v,%v", got, err)
	}
}

// TestAgentMsgKey 验证 key 派生：MetaAgent==sessionID；子 Agent 取 "/" 前段为会话段。
func TestAgentMsgKey(t *testing.T) {
	cases := map[string]string{
		"session-1":                            "sess:session-1:agent:session-1:msgs",
		"session-1/domain-2":                   "sess:session-1:agent:session-1/domain-2:msgs",
		"session-42/domain-1/code_assistant-3": "sess:session-42:agent:session-42/domain-1/code_assistant-3:msgs",
	}
	for in, want := range cases {
		if got := agentMsgKey(in); got != want {
			t.Errorf("agentMsgKey(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAgentMsgWindowSlicing 验证 Before/After 窗口切片纯逻辑（经内存切片，无需真实 Redis）。
func TestAgentMsgWindowSlicing(t *testing.T) {
	all := make([]AgentMsgEntry, 0, 10)
	for i := 0; i < 10; i++ {
		all = append(all, AgentMsgEntry{Seq: i})
	}
	if got := beforeWindow(all, 7, 2); len(got) != 2 || got[0].Seq != 5 || got[1].Seq != 6 {
		t.Fatalf("beforeWindow(7,2) 应得 seq[5,6], got %+v", got)
	}
	if got := afterWindow(all, 7, 2); len(got) != 2 || got[0].Seq != 8 || got[1].Seq != 9 {
		t.Fatalf("afterWindow(7,2) 应得 seq[8,9], got %+v", got)
	}
	if got := afterWindow(all, 9, 5); len(got) != 0 {
		t.Fatalf("afterWindow(9,5) 应空, got %+v", got)
	}
}
