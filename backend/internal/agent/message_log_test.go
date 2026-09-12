package agent

import (
	"fmt"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeMsgLogger 记录 Log/Clear 调用。
type fakeMsgLogger struct {
	logs    []string
	cleared []string
}

func (f *fakeMsgLogger) Log(agentID string, seq int, msg ReactMessage) {
	f.logs = append(f.logs, fmt.Sprintf("%s#%d:%s", agentID, seq, msg.Role))
}

func (f *fakeMsgLogger) Clear(agentID string) { f.cleared = append(f.cleared, agentID) }

// TestAppendLoggedHotLog 验证 appendLogged：追加同时按 seq 热写；nil logger 零行为不 panic。
func TestAppendLoggedHotLog(t *testing.T) {
	fl := &fakeMsgLogger{}
	a := NewReActAgent("session-1/domain-1", types.RoleDefinition{ID: "domain"}, nil, nil).WithMessageLogger(fl)
	h := a.appendLogged(nil, ReactMessage{Role: "user", Content: "hi"})
	h = a.appendLogged(h, ReactMessage{Role: "assistant", Content: "ok"})
	if len(h) != 2 {
		t.Fatalf("history 长度应 2, got %d", len(h))
	}
	if len(fl.logs) != 2 || fl.logs[0] != "session-1/domain-1#0:user" || fl.logs[1] != "session-1/domain-1#1:assistant" {
		t.Fatalf("热写序列不符: %v", fl.logs)
	}

	a2 := NewReActAgent("x", types.RoleDefinition{ID: "domain"}, nil, nil)
	if got := a2.appendLogged(nil, ReactMessage{Role: "user"}); len(got) != 1 {
		t.Fatal("nil logger 时 append 应正常")
	}
}

// TestNewMessageLoggerNil 验证 nil Redis store → 返回 nil（调用方判空跳过）。
func TestNewMessageLoggerNil(t *testing.T) {
	if got := NewMessageLogger(nil); got != nil {
		t.Fatalf("nil store 应返回 nil logger, got %v", got)
	}
}
