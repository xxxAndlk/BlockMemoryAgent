package agent

// list_agents_hot_test.go 验证 ListAgents 的 Hot 字段（Domain 热驻存活标记）：
// meta 根节点恒 true；SlotAlive 命中（热驻）的节点 true；冷驻（树 Idle 但无槽）/
// 终态无槽节点 false；provider 未注入时子节点恒 false。

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// fakeRosterProvider 固定 SlotAlive 结果的 IdleRosterProvider 测试桩。
type fakeRosterProvider struct{ alive map[string]bool }

func (f *fakeRosterProvider) IdleRoster(sessionID string) []IdleDomainInfo { return nil }
func (f *fakeRosterProvider) SlotAlive(sessionID, agentID string) bool {
	return f.alive[agentID]
}

func TestListAgents_HotField(t *testing.T) {
	svc := NewReactService(nil, nil, nil, mailbox.New(), NopMemoryPipeline{}, nil)
	sess := svc.store.createSession("hot 字段测试", "")
	tr := svc.TreeFor(sess.ID)
	hotID := sess.ID + "/domain-1"
	coldID := sess.ID + "/domain-2"
	doneID := sess.ID + "/domain-3"
	tr.Register(orchestrator.Node{ID: hotID, ParentID: sess.ID, Role: "domain", Domain: "金融", Status: orchestrator.StatusRunning})
	tr.Register(orchestrator.Node{ID: coldID, ParentID: sess.ID, Role: "domain", Domain: "物流", Status: orchestrator.StatusRunning})
	tr.Idle(coldID, "done", nil) // 冷驻：树 Idle 但无热驻槽
	tr.Register(orchestrator.Node{ID: doneID, ParentID: sess.ID, Role: "domain", Domain: "旧域", Status: orchestrator.StatusRunning})
	tr.Finish(doneID, "done", nil)

	svc.SetIdleRosterProvider(&fakeRosterProvider{alive: map[string]bool{hotID: true}})

	insts, err := svc.ListAgents(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("ListAgents failed: %v", err)
	}
	byID := map[string]AgentInstance{}
	for _, in := range insts {
		byID[in.ModuleID] = in
	}
	if !byID["meta"].Hot {
		t.Error("meta root hot should always be true")
	}
	if !byID[hotID].Hot {
		t.Error("hot-resident slot should be hot=true")
	}
	if byID[coldID].Hot {
		t.Error("cold-resident node (no slot) should be hot=false")
	}
	if byID[doneID].Hot {
		t.Error("done node without slot should be hot=false")
	}

	// provider 未注入：子节点恒 false，meta 恒 true。
	svc2 := NewReactService(nil, nil, nil, mailbox.New(), NopMemoryPipeline{}, nil)
	sess2 := svc2.store.createSession("无 provider", "")
	tr2 := svc2.TreeFor(sess2.ID)
	tr2.Register(orchestrator.Node{ID: sess2.ID + "/domain-1", ParentID: sess2.ID, Role: "domain", Domain: "金融", Status: orchestrator.StatusRunning})
	insts2, err := svc2.ListAgents(context.Background(), sess2.ID)
	if err != nil {
		t.Fatalf("ListAgents failed: %v", err)
	}
	for _, in := range insts2 {
		if in.ModuleID == "meta" && !in.Hot {
			t.Error("meta hot should always be true (no provider)")
		}
		if in.ModuleID != "meta" && in.Hot {
			t.Errorf("node %s hot should be false without provider", in.ModuleID)
		}
	}
}
