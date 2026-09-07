package subagent

// stall_evidence_test.go 验证证据化 stall 判定（TODO 第10项②，对标 Codex）三态分流：
//   - 在飞 LLM（llmInFlight）超阈值不杀——慢思考单呼可达 25min，流式卡口/调用超时/墙钟兜底；
//   - 工具在飞超阈值杀——真挂死工具（keepalive 盲报不再刷新 lastTS 续命）；
//   - 步间静默超阈值杀——见 heartbeat_kill_test.go / control_test.go 系列用例。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestScanStuck_InFlightLLMExempt 在飞 LLM 时即使步间静默远超阈值也不杀（豁免形态）。
func TestScanStuck_InFlightLLMExempt(t *testing.T) {
	d, _, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	d.WithHeartbeatTimeout(50 * time.Millisecond)

	id := "s1/code_assistant-1"
	tr.Register(orchestrator.Node{ID: id, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	e := newEvidence()
	e.lastTS.Store(time.Now().Add(-time.Hour).UnixNano()) // 步间静默早已超阈值
	e.lastKind.Store("llm_start")
	e.llmInFlight.Store(true)
	e.llmStartTS.Store(time.Now().Add(-time.Hour).UnixNano())
	d.activity.Store(id, e)
	d.subMeta.Store(id, &subAgentMeta{parentID: "s1", sessionID: "s1", cancel: func() {}})

	d.scanStuck()

	if _, ok := d.activity.Load(id); !ok {
		t.Fatal("in-flight LLM evidence must be exempt from stall kill")
	}
	if _, ok := d.subMeta.Load(id); !ok {
		t.Fatal("in-flight LLM agent must not be killed")
	}
}

// TestScanStuck_HungToolKilledAtThreshold 工具在飞超阈值杀，且父通知证据含工具名
//（lastTS 新鲜也不豁免——keepalive 盲报续命的挂死工具形态，行为变更核心）。
func TestScanStuck_HungToolKilledAtThreshold(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	d.WithHeartbeatTimeout(50 * time.Millisecond)

	id := "s1/code_assistant-2"
	tr.Register(orchestrator.Node{ID: id, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	e := newEvidence()
	e.lastTS.Store(time.Now().UnixNano()) // lastTS 新鲜（盲报续命形态）
	e.lastKind.Store("tool:RunCommand")
	e.toolStartTS.Store(time.Now().Add(-time.Hour).UnixNano())
	e.toolName.Store("RunCommand")
	d.activity.Store(id, e)
	d.subMeta.Store(id, &subAgentMeta{parentID: "s1", sessionID: "s1", cancel: func() {}})

	d.scanStuck()

	if _, ok := d.activity.Load(id); ok {
		t.Fatal("hung tool beyond threshold should be killed even with fresh lastTS")
	}
	var body string
	dl := time.Now().Add(2 * time.Second)
	for time.Now().Before(dl) && body == "" {
		for _, m := range mb.Drain("s1") {
			if strings.Contains(m.Body, "假死") {
				body = m.Body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("parent mailbox never got kill notification")
	}
	if !strings.Contains(body, "tool=RunCommand") {
		t.Fatalf("kill evidence should name the hung tool, got: %s", body)
	}
}
