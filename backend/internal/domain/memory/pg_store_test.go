package memory

// pg_store_test.go 验证 PostgresEventStore 的非 DB 路径与 sessionID 派生。
// 真实 INSERT/SELECT 轮次依赖 PostgreSQL, 属 e2e 范畴 (docker compose up 后覆盖)。

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// TestPostgresEventStore_NilDBNoOp 验证 db 为 nil 时 SaveEvent/LoadEvents 无操作返回 nil,
// 不 panic (测试场景降级为无持久化)。
func TestPostgresEventStore_NilDBNoOp(t *testing.T) {
	s := NewPostgresEventStore(nil)
	if err := s.SaveEvent(context.Background(), "a", agent.MemoryEvent{Type: "answer"}); err != nil {
		t.Fatalf("nil db SaveEvent 应返回 nil, got %v", err)
	}
	events, err := s.LoadEvents(context.Background(), "a", 10)
	if err != nil {
		t.Fatalf("nil db LoadEvents 应返回 nil err, got %v", err)
	}
	if events != nil {
		t.Fatalf("nil db LoadEvents 应返回 nil events, got %v", events)
	}
	// nil receiver 同样无操作。
	var nilStore *PostgresEventStore
	if err := nilStore.SaveEvent(context.Background(), "a", agent.MemoryEvent{}); err != nil {
		t.Fatalf("nil receiver SaveEvent 应返回 nil, got %v", err)
	}
}

// TestSessionIDFromAgentID 验证从 agentID 派生 sessionID:
// MetaAgent agentID==sessionID; 子 Agent "session-N/role-K" 取 "/" 前段。
func TestSessionIDFromAgentID(t *testing.T) {
	cases := map[string]string{
		"session-1":                       "session-1", // MetaAgent
		"session-1/code_assistant-5":      "session-1", // 子 Agent
		"session-42/domain-1/code_reviewer-2": "session-42", // 多段 ID 取首段
		"":                                "",          // 空
	}
	for agentID, want := range cases {
		if got := sessionIDFromAgentID(agentID); got != want {
			t.Errorf("sessionIDFromAgentID(%q) = %q, want %q", agentID, got, want)
		}
	}
}
