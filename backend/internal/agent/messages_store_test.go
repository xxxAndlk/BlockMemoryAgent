package agent

// messages_store_test.go 验证 PostgresMessagesStore 的非 DB 路径与 sessionID 派生。
// 真实 INSERT/SELECT 往返依赖 PostgreSQL，属 e2e 范畴（docker compose up 后覆盖）。

import (
	"context"
	"testing"
)

// TestPostgresMessagesStore_NilDBNoOp 验证 db 为 nil 时 SaveMessages/LoadMessages 无操作返回 nil，
// 不 panic（测试场景降级为无持久化）。
func TestPostgresMessagesStore_NilDBNoOp(t *testing.T) {
	s := NewPostgresMessagesStore(nil)
	if err := s.SaveMessages(context.Background(), "a", "s", []ReactMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("nil db SaveMessages 应返回 nil, got %v", err)
	}
	msgs, err := s.LoadMessages(context.Background(), "a")
	if err != nil {
		t.Fatalf("nil db LoadMessages 应返回 nil err, got %v", err)
	}
	if msgs != nil {
		t.Fatalf("nil db LoadMessages 应返回 nil, got %v", msgs)
	}
	// nil receiver 同样无操作。
	var nilStore *PostgresMessagesStore
	if err := nilStore.SaveMessages(context.Background(), "a", "s", nil); err != nil {
		t.Fatalf("nil receiver SaveMessages 应返回 nil, got %v", err)
	}
	if _, err := nilStore.LoadMessages(context.Background(), "a"); err != nil {
		t.Fatalf("nil receiver LoadMessages 应返回 nil, got %v", err)
	}
}

// TestMessagesStoreSessionID 验证从 agentID 派生 sessionID：
// MetaAgent agentID==sessionID；子 Agent "session-N/role-K" 取 "/" 前段。
func TestMessagesStoreSessionID(t *testing.T) {
	cases := map[string]string{
		"session-1":                       "session-1", // MetaAgent
		"session-1/domain-2":              "session-1", // DomainAgent
		"session-42/domain-1/code_assistant-3": "session-42", // 多段取首段
		"":                                "",          // 空
	}
	for agentID, want := range cases {
		if got := messagesStoreSessionID(agentID); got != want {
			t.Errorf("messagesStoreSessionID(%q) = %q, want %q", agentID, got, want)
		}
	}
}
