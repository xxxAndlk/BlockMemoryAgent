package subagent

import (
	"encoding/json"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// TestEmitContextAudit U-02/B-08：注入审计事件携带来源/作用域/字符数与隔离丢弃原因。
func TestEmitContextAudit(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	var gotSession string
	var got agent.LiveEvent
	d.WithLiveEvents(func(sid string, ev agent.LiveEvent) {
		gotSession = sid
		got = ev
	})

	d.emitContextAudit("s1", "s1/code-1", "s1", "code_assistant",
		map[string]int{"agents_md": 10, "block_recall": 20}, false, 5)
	if gotSession != "s1" || got.Kind != agent.LiveEventContextInjection || got.Detail == "" {
		t.Fatalf("audit event not emitted: sid=%q ev=%+v", gotSession, got)
	}
	var detail contextAuditDetail
	if err := json.Unmarshal([]byte(got.Detail), &detail); err != nil {
		t.Fatalf("detail JSON: %v", err)
	}
	if detail.Isolated || detail.TotalInjected != 35 || len(detail.Entries) != 3 {
		t.Fatalf("included audit mismatch: %+v", detail)
	}
	if detail.ManifestVersion == "" || len(detail.Required) != 1 || detail.Required[0] != "task" || len(detail.Optional) != 2 {
		t.Fatalf("manifest declaration mismatch: %+v", detail)
	}
	for _, e := range detail.Entries {
		if e.State != "included" || e.Chars <= 0 {
			t.Fatalf("entry should be included: %+v", e)
		}
	}

	// 隔离派发：前缀全部 dropped 并带原因，总数只计 task。
	d.emitContextAudit("s1", "s1/code-2", "s1", "code_assistant",
		map[string]int{"agents_md": 10}, true, 7)
	json.Unmarshal([]byte(got.Detail), &detail)
	if !detail.Isolated || detail.TotalInjected != 7 || len(detail.Dropped) != 1 || detail.Dropped[0] != "agents_md" {
		t.Fatalf("isolated audit mismatch: %+v", detail)
	}
	for _, e := range detail.Entries {
		if e.Source == "agents_md" && (e.State != "dropped" || e.Reason == "") {
			t.Fatalf("isolated prefix should be dropped with reason: %+v", e)
		}
	}
}
