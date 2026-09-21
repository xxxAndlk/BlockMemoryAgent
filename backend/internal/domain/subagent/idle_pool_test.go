package subagent

// idle_pool_test.go 验证 DomainAgent 热驻留核心路径（仅 hotCfg.Enabled 开启时）：
//   - domain 任务完成 → tree.Idle + 父 notify + pending 归零 + 槽 idle 热存（enterIdle 即武装 TTL）。
//   - TTL 到期投 opDestroy 销毁槽 + tree Finish Done；复用/直连唤醒停表，完成按新权重重新武装。
//   - 复用派发（reuse_agent_id）：idle 槽唤醒 + reuseCount++ + 新任务执行。
//   - 忙碌槽入队：当前任务完成后自动出队执行。
//   - 失败销毁：err 路径 slot destroyed + treeFinish Failed。
//   - PendingChildren 对称性：队列任务在销毁时补偿递减。

import (
	"context"
	"errors"
	"strings"
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
// tree.Idle + 父 notify + pending 归零 + 槽状态 idle（enterIdle 即武装 TTL——完成即开始销毁倒计时）。
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
	subID := subAgentIDOf(res)

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
	if !ttlArmed {
		t.Error("TTL should be armed on enterIdle (countdown starts at task completion)")
	}
}

// TestHotDomain_ArmAndExpiry 验证 TTL 生命周期：enterIdle 完成即武装（ArmIdleTTLs 幂等兼容）；
// 到期销毁槽 + tree Done。
func TestHotDomain_ArmAndExpiry(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, 500*time.Millisecond)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// enterIdle 已武装（完成即开始倒计时）；ArmIdleTTLs 为幂等兼容调用。
	d.ArmIdleTTLs("s1")
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing")
	}
	s.mu.Lock()
	armed := s.ttlArmed
	s.mu.Unlock()
	if !armed {
		t.Fatal("TTL not armed after enterIdle")
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
	subID := subAgentIDOf(res)
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
	subID := subAgentIDOf(res)

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
	subID := subAgentIDOf(res)

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
	subID := subAgentIDOf(res)
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
	if roster[0].Resp != "负责金融模块" {
		t.Errorf("roster responsibility = %q, want 负责金融模块", roster[0].Resp)
	}
	if roster[0].Busy {
		t.Error("idle slot reported busy")
	}
}

// TestIdleRoster_WrittenFiles 验证清单含近期写入文件（lastWrites 追踪 -> base 名）。
func TestIdleRoster_WrittenFiles(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "部署",
		"responsibility": "负责部署",
	})
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	d.lastWrites.Store(subID, &fileWriteState{recs: []fileWriteRecord{
		{path: "/srv/app.conf", at: time.Now()},
	}})
	roster := d.IdleRoster("s1")
	if len(roster) != 1 {
		t.Fatalf("roster len = %d, want 1", len(roster))
	}
	if len(roster[0].WrittenFiles) != 1 || roster[0].WrittenFiles[0] != "app.conf" {
		t.Errorf("roster written files = %v, want [app.conf]", roster[0].WrittenFiles)
	}
}

// gateProvider 第 blockOn 次（1 起计）LLM 调用阻塞至 release 关闭，
// 便于断言任务执行中的槽中态（running/reuseCount）；其余调用立即返回。
type gateProvider struct {
	mu      sync.Mutex
	calls   int
	blockOn int
	entered chan struct{}
	release chan struct{}
}

func (m *gateProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if n == m.blockOn {
		close(m.entered)
		<-m.release
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("result")}, nil
}
func (m *gateProvider) Name() string { return "gate-mock" }

// TestHotDomain_MailWakeIdle 验证兄弟邮件唤醒热驻 idle 槽（WakeIdleForMail，2026-09-21
// 问答面闭环修复）：idle 槽被 request 命中后唤醒续答——任务文本为【邮箱请求】处置提示
//（含提问方 id），槽回 running、父邮箱有唤醒 MsgInfo；非 idle 目标返回 busy/not-directable。
func TestHotDomain_MailWakeIdle(t *testing.T) {
	provider := &gateProvider{blockOn: 2, entered: make(chan struct{}), release: make(chan struct{})}
	d, mb, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
	mb.Drain("s1")

	// 兄弟 Agent 邮件唤醒 idle 槽。
	if err := d.WakeIdleForMail(subID, "s1/domain-9", "汇率 JSON 结构确认"); err != nil {
		t.Fatalf("WakeIdleForMail failed: %v", err)
	}

	// 等唤醒任务进入执行（第二次 LLM 调用被 gate 卡住）——任务文本应为【邮箱请求】。
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("邮件唤醒任务未开始执行")
	}
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing after mail wake")
	}
	s.mu.Lock()
	state, rc := s.state, s.reuseCount
	s.mu.Unlock()
	if state != slotRunning || rc != 1 {
		t.Errorf("after mail wake: state=%d rc=%d, want slotRunning/1", state, rc)
	}
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusRunning {
		t.Errorf("tree status = %v, want Running", n.Status)
	}
	// 父邮箱有唤醒 MsgInfo。
	found := false
	for _, m := range mb.Drain("s1") {
		if m.Type == mailbox.MsgInfo && m.From == "dispatcher" && strings.Contains(m.Body, "邮件提问唤醒") {
			found = true
		}
	}
	if !found {
		t.Error("parent should receive mail-wake notice")
	}

	// 非 idle 目标（running）返回 busy。
	if err := d.WakeIdleForMail(subID, "s1/domain-9", "x"); !errors.Is(err, agent.ErrAgentBusy) {
		t.Errorf("wake running slot should be busy, got %v", err)
	}
	// 不存在的热驻实例返回 not-directable。
	if err := d.WakeIdleForMail("s1/domain-99", "s1/domain-9", "x"); !errors.Is(err, agent.ErrAgentNotDirectable) {
		t.Errorf("wake unknown slot should be not-directable, got %v", err)
	}
	close(provider.release)
}

// TestHotDomain_UserWakeIdle 验证用户直连唤醒热驻 idle 槽（WakeIdleWithMessage）：
// 槽回 running + reuseCount+1 + 任务文本=用户消息原文（无派发前缀）；
// 完成回 idle 后 pending 对称、父邮箱有"用户直连唤醒"MsgInfo。
func TestHotDomain_UserWakeIdle(t *testing.T) {
	provider := &gateProvider{blockOn: 2, entered: make(chan struct{}), release: make(chan struct{})}
	d, mb, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
	mb.Drain("s1") // 清掉首任务完成通知，后续只数用户直连相关邮件。

	// 用户直连唤醒 idle 槽。
	if err := d.WakeIdleWithMessage(subID, "帮我再看看"); err != nil {
		t.Fatalf("WakeIdleWithMessage failed: %v", err)
	}

	// 等唤醒任务进入执行（第二次 LLM 调用被 gate 卡住）。
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("唤醒任务未开始执行")
	}

	// 执行中态：槽 running + reuseCount+1 + 树 Running（TTL 已停表，完成回 idle 重新武装）。
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing after wake")
	}
	s.mu.Lock()
	state, rc := s.state, s.reuseCount
	s.mu.Unlock()
	if state != slotRunning {
		t.Errorf("slot state = %d, want slotRunning", state)
	}
	if rc != 1 {
		t.Errorf("reuseCount = %d, want 1", rc)
	}
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusRunning {
		t.Errorf("tree status = %v, want Running", n.Status)
	}

	// 放行，唤醒任务完成回 idle。
	close(provider.release)
	waitForCond(t, "tree idle again", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 任务文本口径：history 含"【用户直连消息】"+用户原文，无 buildReuseTask 派发前缀。
	s.mu.Lock()
	found := false
	for _, m := range s.history {
		if m.Role == "user" && strings.Contains(m.Content, "【用户直连消息】") && strings.Contains(m.Content, "帮我再看看") {
			found = true
		}
	}
	s.mu.Unlock()
	if !found {
		t.Error("history should contain user-direct message task text")
	}

	// pending 对称：唤醒挂账与完成递减配对归零。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after wake task = %d, want 0", got)
	}
	// 父邮箱收到"用户直连唤醒"MsgInfo（From=dispatcher）。
	drains := mb.Drain("s1")
	wakeNotice := false
	for _, m := range drains {
		if m.Type == mailbox.MsgInfo && m.From == "dispatcher" && strings.Contains(m.Body, "用户直连唤醒") {
			wakeNotice = true
		}
	}
	if !wakeNotice {
		t.Error("parent should receive user-direct wake notice (MsgInfo from dispatcher)")
	}
}

// TestHotDomain_UserWakeRearmsTTL 验证对话刷新寿命：完成即武装 TTL（无需用户消息触发）；
// 用户直连唤醒停表执行，任务完成回 idle 后按新权重重新武装满额，到期销毁槽 + tree Done。
func TestHotDomain_UserWakeRearmsTTL(t *testing.T) {
	provider := &scriptProvider{lines: []string{"first result", "second result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, 500*time.Millisecond)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 普通完成即武装 TTL（倒计时自任务完成开始）。
	s := d.pool.slot("s1", subID)
	s.mu.Lock()
	armed := s.ttlArmed
	s.mu.Unlock()
	if !armed {
		t.Fatal("TTL should be armed on enterIdle (countdown starts at task completion)")
	}

	// 用户直连唤醒（TTL 停表）→ 任务完成回 idle 后按新权重重新武装满额。
	if err := d.WakeIdleWithMessage(subID, "继续聊聊"); err != nil {
		t.Fatalf("WakeIdleWithMessage failed: %v", err)
	}
	waitForCond(t, "ttl re-armed after wake task done", func() bool {
		s := d.pool.slot("s1", subID)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.state == slotIdle && s.ttlArmed && s.reuseCount == 1
	})

	// 到期：槽销毁 + tree Done。
	waitForCond(t, "slot destroyed after re-armed TTL", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	waitForCond(t, "tree done after TTL", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusDone
	})
}

// TestHotDomain_UserWakeRejected 验证错误路径：不存在槽 → ErrAgentNotDirectable；
// running 槽 → ErrAgentBusy。
func TestHotDomain_UserWakeRejected(t *testing.T) {
	provider := &gateProvider{blockOn: 1, entered: make(chan struct{}), release: make(chan struct{})}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	// 不存在的槽。
	if err := d.WakeIdleWithMessage("s1/domain-99", "喂"); !errors.Is(err, agent.ErrAgentNotDirectable) {
		t.Fatalf("unknown slot: want ErrAgentNotDirectable, got %v", err)
	}

	// 首任务 LLM 调用被 gate 卡住 → 槽稳定 running。
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("首任务未开始执行")
	}
	if err := d.WakeIdleWithMessage(subID, "喂"); !errors.Is(err, agent.ErrAgentBusy) {
		t.Fatalf("running slot: want ErrAgentBusy, got %v", err)
	}

	// 放行收尾（避免泄漏 goroutine 干扰其他用例）。
	close(provider.release)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
}
