package subagent

// idle_pool_test.go 验证 DomainAgent 热驻留核心路径（仅 hotCfg.Enabled 开启时）：
//   - domain 任务完成 → tree.Idle + 父 notify + pending 归零 + 槽 idle 热存（无 TTL）。
//   - ArmIdleTTLs 武装 TTL；到期投 opDestroy 销毁槽 + tree Finish Done。
//   - 复用派发（reuse_agent_id）：idle 槽唤醒 + reuseCount++ + 新任务执行。
//   - 忙碌槽入队：当前任务完成后自动出队执行。
//   - 失败销毁：err 路径 slot destroyed + treeFinish Failed。
//   - PendingChildren 对称性：队列任务在销毁时补偿递减。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// failProvider 恒定失败，驱动任务失败销毁路径。
type failProvider struct{}

func (m *failProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return nil, errors.New("mock failure")
}
func (m *failProvider) Name() string { return "fail-mock" }

// scriptProvider 按序返回预设文本；耗尽后返回空 assistant（任务自然结束）。
type scriptProvider struct {
	mu    sync.Mutex
	calls int
	lines []string
}

func (m *scriptProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	text := ""
	if m.calls < len(m.lines) {
		text = m.lines[m.calls]
	}
	m.calls++
	return &blades.ModelResponse{Message: blades.AssistantMessage(text)}, nil
}
func (m *scriptProvider) Name() string { return "script-mock" }

// newIdleTestEnv 构造开启热驻的测试环境。
func newIdleTestEnv(t *testing.T, provider agent.ModelProvider, ttl time.Duration) (*Dispatcher, *mailbox.Mailbox, *orchestrator.Tree, *tool.Registry) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "通用领域纪律"},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: provider}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{MaxIterations: 50} })
	tr := orchestrator.NewTree("s1", nil)
	d.WithTree(func(string) *orchestrator.Tree { return tr })
	d.WithDomainHotResident(domainHotConfig{
		Enabled:        true,
		BaseTTL:        ttl,
		ExtendPerReuse: ttl,
		MaxTTL:         ttl,
		MaxPerSession:  4,
		TaskQueueLen:   4,
	})
	d.RegisterCallTool(toolsReg)
	return d, mb, tr, toolsReg
}

// TestHotDomain_TaskDoneEntersIdle 验证热驻 domain 任务完成：
// tree.Idle + 父 notify + pending 归零 + 槽状态 idle（无 TTL timer——完成后一直热存）。
func TestHotDomain_TaskDoneEntersIdle(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result"}}
	d, mb, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := res.Output

	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 父 pending 归零（Idle=完成语义）。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after done = %d, want 0", got)
	}
	// 父 mailbox 收到完成通知。
	drains := mb.Drain("s1")
	if len(drains) != 1 {
		t.Errorf("parent mailbox: want 1 notify, got %d", len(drains))
	}
	// 槽仍热存（未销毁）。
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot destroyed after task done; want hot-resident")
	}
	s.mu.Lock()
	state, ttlArmed := s.state, s.ttlArmed
	s.mu.Unlock()
	if state != slotIdle {
		t.Errorf("slot state = %d, want slotIdle", state)
	}
	if ttlArmed {
		t.Error("TTL should NOT be armed on enterIdle (armed only after next user message)")
	}
}

// TestHotDomain_ArmAndExpiry 验证 TTL 生命周期：ArmIdleTTLs 武装；到期销毁槽 + tree Done。
func TestHotDomain_ArmAndExpiry(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, 80*time.Millisecond)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := res.Output
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 武装 TTL。
	d.ArmIdleTTLs("s1")
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing")
	}
	s.mu.Lock()
	armed := s.ttlArmed
	s.mu.Unlock()
	if !armed {
		t.Fatal("TTL not armed after ArmIdleTTLs")
	}

	// 到期：槽销毁 + tree Done。
	waitForCond(t, "slot destroyed after TTL", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	waitForCond(t, "tree done after TTL", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusDone
	})
}

// TestHotDomain_ReuseWake 验证复用派发：idle 槽唤醒 + reuseCount++ + 新任务完成再 idle。
func TestHotDomain_ReuseWake(t *testing.T) {
	provider := &scriptProvider{lines: []string{"first result", "second result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := res.Output
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 复用派发（reuse_agent_id）。
	res2, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"reuse_agent_id": subID,
		"task":           "第二任务",
	})
	if err != nil || !res2.Success {
		t.Fatalf("reuse dispatch failed: err=%v res=%+v", err, res2)
	}

	// reuseCount 递增。
	s := d.pool.slot("s1", subID)
	s.mu.Lock()
	rc := s.reuseCount
	s.mu.Unlock()
	if rc != 1 {
		t.Errorf("reuseCount = %d, want 1", rc)
	}

	// 第二任务完成回 idle。
	waitForCond(t, "tree idle again", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
	// pending 对称：两次派发两减。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after reuse = %d, want 0", got)
	}
}

// TestHotDomain_QueueWhenBusy 验证忙碌槽入队：第二任务缓冲，第一任务完成后自动执行。
func TestHotDomain_QueueWhenBusy(t *testing.T) {
	provider := &scriptProvider{lines: []string{"first result", "second result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := res.Output

	// 第一任务仍在跑（同 goroutine 未完成），立即再派发应入队。
	// 由于 scriptProvider 立即返回，忙碌窗口极窄；轮询 tree Running 态时再派。
	waitForCond(t, "running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})
	res2, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"reuse_agent_id": subID,
		"task":           "第二任务",
	})
	if err != nil || !res2.Success {
		t.Fatalf("queue dispatch failed: err=%v res=%+v", err, res2)
	}

	// 两任务全部完成后回 idle 且 pending 归零。
	waitForCond(t, "all tasks done", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle && d.PendingChildren("s1") == 0
	})
	// 第二任务确实执行（provider 调用 >= 2 次）。
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls < 2 {
		t.Errorf("provider calls = %d, want >= 2 (queued task executed)", calls)
	}
}

// TestHotDomain_ReuseUnknownSlotRejected 验证复用不存在的槽报错。
func TestHotDomain_ReuseUnknownSlotRejected(t *testing.T) {
	provider := &scriptProvider{lines: []string{}}
	_, _, _, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"reuse_agent_id": "s1/domain-999",
		"task":           "任务",
	})
	if err != nil || res.Success {
		t.Fatalf("expected rejection for unknown slot, got err=%v res=%+v", err, res)
	}
	if res.Category != tool.ResultCategoryValidationRejected {
		t.Errorf("category = %s, want validation_rejected", res.Category)
	}
}

// TestHotDomain_FailDestroysSlot 验证失败销毁：任务 err 后槽 destroyed + tree Failed + pending 归零。
func TestHotDomain_FailDestroysSlot(t *testing.T) {
	provider := &failProvider{}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "会失败的任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := res.Output

	waitForCond(t, "tree failed", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusFailed
	})
	// 槽销毁（失败不热驻）。
	waitForCond(t, "slot destroyed", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after fail = %d, want 0", got)
	}
}

// TestSuspendGate_ParkAndWake 验证 SuspendGate：挂起期间 Park 阻塞，resume 广播唤醒。
func TestSuspendGate_ParkAndWake(t *testing.T) {
	d, _, _, _ := newIdleTestEnv(t, &scriptProvider{}, time.Hour)
	st := d.suspendState("s1")
	st.suspend()

	gate := &slotSuspendGate{d: d, sid: "s1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	parked := make(chan error, 1)
	go func() {
		parked <- gate.Park(ctx)
	}()
	select {
	case err := <-parked:
		t.Fatalf("Park returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
		// 仍阻塞，符合预期。
	}
	st.resume()
	select {
	case err := <-parked:
		if err != nil {
			t.Fatalf("Park returned error on wake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Park not woken after resume")
	}
}

// TestSuspendGate_NotSuspendedNoBlock 验证未挂起时 Park 立即返回。
func TestSuspendGate_NotSuspendedNoBlock(t *testing.T) {
	d, _, _, _ := newIdleTestEnv(t, &scriptProvider{}, time.Hour)
	gate := &slotSuspendGate{d: d, sid: "s1"}
	if err := gate.Park(context.Background()); err != nil {
		t.Fatalf("Park returned error when not suspended: %v", err)
	}
}

// TestIdleRoster_Render 验证清单渲染（agent 包 renderIdleRoster 的 subagent 侧数据源）。
func TestIdleRoster_Render(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := res.Output
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	roster := d.IdleRoster("s1")
	if len(roster) != 1 {
		t.Fatalf("roster len = %d, want 1", len(roster))
	}
	if roster[0].AgentID != subID || roster[0].Domain != "金融" {
		t.Errorf("roster entry = %+v", roster[0])
	}
	if roster[0].Busy {
		t.Error("idle slot reported busy")
	}
}
