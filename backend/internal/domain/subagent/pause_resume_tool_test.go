package subagent

// pause_resume_tool_test.go 验证 pause_agent / resume_agent 工具：
//   - 参数与前置校验（缺 agent_id、未知节点、越级调用、非 Running、tree 缺失）；
//   - 非热驻路径：暂停成功（标记先登记、收尾清标记、节点转 Paused、父 pending 不减）+ resume 异步续跑；
//   - 热驻路径：暂停落地（slotPaused）+ resume 唤醒（节点回 Running、suspended 复位）；
//   - 未暂停槽 resume 报错。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestPauseAgentTool_Validation 覆盖 pause_agent 的前置拒绝路径。
func TestPauseAgentTool_Validation(t *testing.T) {
	d, _, _, _, tr, _ := newPauseTestEnv(t, &scriptProvider{lines: []string{"x"}})
	toolImpl := &pauseAgentTool{dispatcher: d}

	if res := toolImpl.Execute(dispatchCtx(), map[string]any{}); res.Success || !strings.Contains(res.Error, "agent_id is required") {
		t.Errorf("缺 agent_id 应拒绝: %+v", res)
	}
	if res := toolImpl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/domain-9"}); res.Success || !strings.Contains(res.Error, "不存在") {
		t.Errorf("未知节点应拒绝: %+v", res)
	}
	// 越级调用：调用方（s1）不是节点派发者（s1/domain-1）时拒绝。
	tr.Register(orchestrator.Node{ID: "s1/code_assistant-1", ParentID: "s1/domain-1", Role: "code_assistant", Status: orchestrator.StatusRunning, Started: time.Now()})
	if res := toolImpl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/code_assistant-1"}); res.Success || !strings.Contains(res.Error, "越级") {
		t.Errorf("非父调用应拒绝: %+v", res)
	}
	// 非 Running 状态。
	tr.Register(orchestrator.Node{ID: "s1/domain-2", ParentID: "s1", Role: "domain", Status: orchestrator.StatusDone, Started: time.Now()})
	if res := toolImpl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/domain-2"}); res.Success || !strings.Contains(res.Error, "仅运行中可暂停") {
		t.Errorf("非 Running 应拒绝: %+v", res)
	}
	// tree 未接线。
	d2 := NewDispatcher(role.NewRegistry(&config.RoleConfigFile{}), &mockModelFactory{provider: &scriptProvider{}}, nil, nil, agent.NopMemoryPipeline{})
	if res := (&pauseAgentTool{dispatcher: d2}).Execute(context.Background(), map[string]any{"agent_id": "x"}); res.Success || !strings.Contains(res.Error, "agent tree not available") {
		t.Errorf("tree 缺失应拒绝: %+v", res)
	}
}

// TestPauseAgentTool_NonHotPauseAndAsyncResume 非热驻：暂停走 Pause 收尾（节点 Paused、
// 标记清理、父 pending 不减、父收处置通知），resume_agent 走异步续跑路径（节点回 Running）。
func TestPauseAgentTool_NonHotPauseAndAsyncResume(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d, _, mb, _, tr, toolsReg := newPauseTestEnv(t, &ctxAwareHangingProvider{release: release})
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{TokenBudget: 1 << 30, MaxIterations: 50} })
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "悬挂任务",
		"domain":         "测试",
		"responsibility": "负责测试",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "domain running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})

	// pause_agent 直接 Execute（本环境未注册控制工具）。
	pres := (&pauseAgentTool{dispatcher: d}).Execute(dispatchCtx(), map[string]any{"agent_id": subID, "reason": "换档"})
	if !pres.Success {
		t.Fatalf("pause failed: %+v", pres)
	}
	if !strings.Contains(pres.Output, "已暂停") || !strings.Contains(pres.Output, "换档") {
		t.Errorf("回执应含暂停说明与 reason: %s", pres.Output)
	}
	waitForCond(t, "paused", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusPaused
	})
	if d.isPauseRequested(subID) {
		t.Error("暂停标记应在收尾清除")
	}
	if got := d.PendingChildren("s1"); got != 1 {
		t.Errorf("pending after pause = %d, want 1", got)
	}
	if drains := mb.Drain("s1"); len(drains) != 1 || !strings.Contains(drains[0].Subject, "已暂停") {
		t.Errorf("pause should notify parent for handling, got %+v", drains)
	}

	// 非热驻 resume：树节点 Paused + history 已存 -> 异步续跑（节点回 Running）。
	rres := (&resumeAgentTool{dispatcher: d}).Execute(dispatchCtx(), map[string]any{"agent_id": subID})
	if !rres.Success || !strings.Contains(rres.Output, "异步") {
		t.Errorf("非热驻 resume 应发起异步续跑: %+v", rres)
	}
	waitForCond(t, "resumed running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})
}

// TestPauseResumeAgentTool_HotSlot 热驻：pause_agent 等槽落地（slotPaused）→ resume_agent
// 单槽 opResume 唤醒，节点回 Running、suspended 复位；未暂停槽 resume 报错。
func TestPauseResumeAgentTool_HotSlot(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d, _, tr, toolsReg := newIdleTestEnv(t, &ctxAwareHangingProvider{release: release}, time.Hour)
	d.RegisterControlTool(toolsReg)

	// 未暂停槽：resume 应拒绝。
	if res, _ := toolsReg.Dispatch(dispatchCtx(), "resume_agent", map[string]any{"agent_id": "s1/domain-9"}); res.Success || !strings.Contains(res.Error, "不存在") {
		t.Fatalf("未知槽 resume 应拒绝: %+v", res)
	}

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "长任务",
		"domain":         "测试",
		"responsibility": "负责测试",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "domain running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})

	// 运行中但未暂停：resume 应拒绝。
	if r, _ := toolsReg.Dispatch(dispatchCtx(), "resume_agent", map[string]any{"agent_id": subID}); r.Success || !strings.Contains(r.Error, "未处于暂停态") {
		t.Fatalf("未暂停槽 resume 应拒绝: %+v", r)
	}

	// pause_agent：工具内部等到槽落地（suspended）才返回。
	pres, _ := toolsReg.Dispatch(dispatchCtx(), "pause_agent", map[string]any{"agent_id": subID, "reason": "模型换档"})
	if !pres.Success {
		t.Fatalf("pause failed: %+v", pres)
	}
	if !d.slotPaused("s1", subID) {
		t.Fatal("pause_agent 返回时槽应已进入暂停落地态")
	}
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusPaused {
		t.Fatalf("tree 节点应为 Paused, got %s", n.Status)
	}
	if got := d.PendingChildren("s1"); got != 1 {
		t.Errorf("pending after pause = %d, want 1", got)
	}

	// resume_agent：单槽唤醒，节点回 Running、suspended 复位。
	rres, _ := toolsReg.Dispatch(dispatchCtx(), "resume_agent", map[string]any{"agent_id": subID})
	if !rres.Success {
		t.Fatalf("resume failed: %+v", rres)
	}
	waitForCond(t, "resumed running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})
	if s := d.pool.slot("s1", subID); s != nil {
		s.mu.Lock()
		suspended := s.suspended
		s.mu.Unlock()
		if suspended {
			t.Error("resume 后槽 suspended 应复位")
		}
	}
}

// 确保 types 包引用被使用（测试环境构造 RoleConfigFile 依赖）。
var _ = types.AgentModelConfig{}
