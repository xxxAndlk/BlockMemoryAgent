package subagent

// failure_prune_test.go 失败分支整支剪枝（TODO #20④）：逻辑检查点落 agent_events、
// 剪枝重派种子剔除失败轨迹、分叉重派新支重跑旧支保留。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// captureMemory 记录 Write 的事件（checkpoint 落点断言）。
type captureMemory struct {
	agent.NopMemoryPipeline
	events []agent.MemoryEvent
}

func (m *captureMemory) Write(agentID string, ev agent.MemoryEvent) error {
	ev.AgentID = agentID
	m.events = append(m.events, ev)
	return nil
}

// TestCheckpointOnDispatchAndMilestone 验证逻辑检查点两处落点：
// 派发时（dispatchOne → RecordDispatch 后）+ 里程碑时（send_message 里程碑播报）。
// 事件形态：type=checkpoint、Input=任务账本快照。
func TestCheckpointOnDispatchAndMilestone(t *testing.T) {
	d, _, _, _, _, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "work"})
	d.RegisterMessagingTool(toolsReg)
	mem := &captureMemory{}
	d.memory = mem

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":    "code_assistant",
		"task":       "write code",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)

	ckpts := checkpointEvents(mem.events)
	if len(ckpts) != 1 {
		t.Fatalf("dispatch must write 1 checkpoint, got %d (%+v)", len(ckpts), mem.events)
	}
	if ckpts[0].AgentID != subID || ckpts[0].Content != checkpointPhaseDispatch {
		t.Fatalf("dispatch checkpoint mismatch: %+v", ckpts[0])
	}

	// 里程碑播报触发第二个检查点（发送方视角）。
	mile, mileErr := toolsReg.Dispatch(agent.WithAgentID(dispatchCtx(), subID), "send_message", map[string]any{
		"to_agent_id":  "s1",
		"subject":      "里程碑: 链路打通",
		"body":         "渲染链路已打通",
		"message_type": "info",
	})
	if mileErr != nil || mile.Error != "" {
		t.Fatalf("send milestone failed: err=%v res=%+v", mileErr, mile)
	}
	ckpts = checkpointEvents(mem.events)
	if len(ckpts) != 2 {
		t.Fatalf("milestone must write 2nd checkpoint, got %d", len(ckpts))
	}
	if ckpts[1].Content != checkpointPhaseMilestone {
		t.Fatalf("milestone checkpoint phase = %q, want %q", ckpts[1].Content, checkpointPhaseMilestone)
	}
}

// TestPruneSeedExcludesFailureTrail 验证剪枝重派种子（默认档）：
// Failed 节点重跑种子只带原任务+检查点账本+用户消息，上轮错误/失败摘要不进活跃上下文；
// Done 节点自动走同支续跑（现状种子保留上轮结果）。
func TestPruneSeedExcludesFailureTrail(t *testing.T) {
	d, _, _, _, tr, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "ok"})
	d.lastCheckpoints.Store("s1/domain-1", checkpointRec{brief: "【任务台账】#1 完成 A", phase: checkpointPhaseMilestone, at: time.Now()})

	failed := orchestrator.Node{
		ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "配置",
		Task: "实现 config.js", Summary: "写了一半炸了", Err: "exit status 1",
		Status: orchestrator.StatusFailed,
	}
	seed := d.buildReviveSeed(failed, "改成 yaml", resolveReviveMode(failed, ReviveAuto))
	if strings.Contains(seed, "exit status 1") || strings.Contains(seed, "写了一半炸了") {
		t.Fatalf("prune seed must exclude failure trail, got:\n%s", seed)
	}
	if !strings.Contains(seed, "实现 config.js") || !strings.Contains(seed, "【检查点账本（回滚点）】") ||
		!strings.Contains(seed, "#1 完成 A") || !strings.Contains(seed, "改成 yaml") {
		t.Fatalf("prune seed must carry task+checkpoint+userMsg, got:\n%s", seed)
	}

	done := orchestrator.Node{
		ID: "s1/domain-1", Task: "实现 config.js", Summary: "上一轮已写 config.js",
		Status: orchestrator.StatusDone,
	}
	seed2 := d.buildReviveSeed(done, "改成 yaml", resolveReviveMode(done, ReviveAuto))
	if !strings.Contains(seed2, "上一轮已写 config.js") {
		t.Fatalf("continue seed must keep last summary, got:\n%s", seed2)
	}
	_ = tr
}

// TestReviveFork_NewBranchOldKept 验证分叉重派：新支注册重跑（剪枝种子），
// 旧支节点原样保留（可 resume 考古），父收到分叉通知。
func TestReviveFork_NewBranchOldKept(t *testing.T) {
	d, _, mb, _, tr, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "forked"})
	oldID := "s1/domain-1"
	tr.Register(orchestrator.Node{
		ID: oldID, ParentID: "s1", Role: "domain", Domain: "配置",
		Task: "实现 config.js", Status: orchestrator.StatusRunning, Started: time.Now(),
	})
	tr.Finish(oldID, "半成品", nil)
	node, _ := tr.Get(oldID)

	if err := d.ReviveFork(dispatchCtx(), node, "重做"); err != nil {
		t.Fatalf("ReviveFork failed: %v", err)
	}

	// 旧支保留：仍 Done、原 ID 原 Summary。
	old, ok := tr.Get(oldID)
	if !ok || old.Status != orchestrator.StatusDone || old.Summary != "半成品" {
		t.Fatalf("old branch must stay intact for archaeology, got %+v ok=%t", old, ok)
	}
	// 新支：树中出现同 domain 的另一个 Running 节点。
	newCount := 0
	for _, n := range tr.Snapshot() {
		if n.Domain == "配置" && n.ID != oldID {
			newCount++
			if n.Status != orchestrator.StatusRunning {
				t.Fatalf("fork branch must be Running, got %s", n.Status)
			}
			if n.ParentID != "s1" {
				t.Fatalf("fork branch parent = %q, want s1", n.ParentID)
			}
		}
	}
	if newCount != 1 {
		t.Fatalf("expected exactly 1 fork branch, got %d", newCount)
	}
	// 父感知：分叉重派通知必达。
	waitForCond(t, "fork notice to parent", func() bool {
		for _, m := range mb.Peek("s1") {
			if strings.Contains(m.Subject, "分叉重派") {
				return true
			}
		}
		return false
	})
}

// checkpointEvents 过滤 checkpoint 类型事件。
func checkpointEvents(evs []agent.MemoryEvent) []agent.MemoryEvent {
	var out []agent.MemoryEvent
	for _, ev := range evs {
		if ev.Type == checkpointEventType {
			out = append(out, ev)
		}
	}
	return out
}
