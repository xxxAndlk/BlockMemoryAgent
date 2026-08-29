package agent

// messages_store_test.go 验证 PostgresMessagesStore 的非 DB 路径与 sessionID 派生。
// 真实 INSERT/SELECT 往返依赖 PostgreSQL，属 e2e 范畴（docker compose up 后覆盖）。

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestSanitizeMessageFields 验证入库字段清洗：含 0x00 的 content/reasoning/tool_calls
// 在 INSERT 前被剥离 NUL、非法 UTF-8 字节替换为 ""——否则 PG 拒绝
// （invalid byte sequence for encoding "UTF8": 0x00, 22021）。合法输入原样保留。
func TestSanitizeMessageFields(t *testing.T) {
	m := ReactMessage{
		Role:             "user",
		Content:          "hello\x00world",
		ReasoningContent: "think\x00ing",
		ToolCalls:        []ToolCall{{ID: "c1", Name: "shell", Input: map[string]any{"cmd": "ls-la"}}},
	}
	content, reasoning, calls := sanitizeMessageFields(m)
	if content != "helloworld" {
		t.Errorf("content 应剥离 NUL, got %q", content)
	}
	if reasoning != "thinking" {
		t.Errorf("reasoning 应剥离 NUL, got %q", reasoning)
	}
	// json.Marshal 本身会把控制字符转义为 \u0000 形式，sanitizeUTF8 双保险：
	// 序列化结果不应含字面 NUL 字节且必须是合法 UTF-8（PG jsonb 前置条件）。
	if strings.Contains(calls, "\x00") {
		t.Errorf("tool_calls JSON 不应含字面 NUL 字节, got %q", calls)
	}
	if !utf8.ValidString(calls) {
		t.Errorf("tool_calls JSON 应为合法 UTF-8, got %q", calls)
	}
	if !strings.Contains(calls, "ls-la") {
		t.Errorf("tool_calls JSON 应保留原有内容, got %q", calls)
	}

	// 非法 UTF-8 字节（非 NUL）替换为 Unicode 替换字符。
	bad := ReactMessage{Content: string([]byte{'a', 0xff, 'b'})}
	got, _, _ := sanitizeMessageFields(bad)
	if got != "a\ufffdb" {
		t.Errorf("非法 UTF-8 应替换为 , got %q", got)
	}

	// 合法输入原样返回（含多行与制表符），零拷贝快速路径不改动内容。
	clean := "func main() {\n\tprintln(\"ok\")\n}"
	got, _, _ = sanitizeMessageFields(ReactMessage{Content: clean})
	if got != clean {
		t.Errorf("合法内容应原样保留, got %q", got)
	}
}
