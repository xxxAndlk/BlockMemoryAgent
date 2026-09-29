package subagent

// dispatcher_wallclock_test.go 验证派发级墙钟（wall_clock_min）与引擎辅助 LLM 超时/保活。
import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// TestWallClockArg 验证 wall_clock_min 入参解析：数字转 Duration，缺失/非法/<=0 回 0。
func TestWallClockArg(t *testing.T) {
	d := &Dispatcher{}
	cases := []struct {
		args map[string]any
		want time.Duration
	}{
		{map[string]any{}, 0},
		{map[string]any{"wall_clock_min": float64(15)}, 15 * time.Minute},
		{map[string]any{"wall_clock_min": float64(0.5)}, 30 * time.Second},
		{map[string]any{"wall_clock_min": float64(-1)}, 0},
		{map[string]any{"wall_clock_min": "15"}, 0}, // 非数字忽略
	}
	for i, c := range cases {
		if got := d.wallClockArg(c.args); got != c.want {
			t.Fatalf("case %d: wallClockArg=%v want %v", i, got, c.want)
		}
	}
}

// ctxAwareHangingProvider 模拟尊重 ctx 的挂起 LLM：ctx 取消/超时即返回错误
//（真实 SDK 网络调用语义；区别于忽略 ctx 的 hangingProvider）。
type ctxAwareHangingProvider struct {
	release chan struct{}
}

func (p *ctxAwareHangingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	select {
	case <-p.release:
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (p *ctxAwareHangingProvider) Name() string { return "ctx-hanging" }

// TestDispatchOne_WallClockEnforced 验证 wall_clock_min 代码级强制：叶子超派发级墙钟被终止
//（ctx deadline -> 失败回执 -> PendingChildren 归 0），不烧到全局 sub_agent_timeout。
func TestDispatchOne_WallClockEnforced(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 释放悬挂 goroutine，防泄漏

	// 全局超时给足 10min，派发级给 300ms（0.005min）：证明收口来自 wall_clock_min 而非全局值。
	d := NewDispatcher(reg, &mockModelFactory{provider: &ctxAwareHangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithTimeout(10 * time.Minute)
	d.RegisterCallTool(toolsReg)
	t.Cleanup(d.ClosePatrol)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":        "code_assistant",
		"task":           "hang",
		"wall_clock_min": 0.005, // 300ms
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.PendingChildren("meta") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.PendingChildren("meta") != 0 {
		t.Fatalf("wall_clock deadline should terminate sub-agent, pending=%d", d.PendingChildren("meta"))
	}
	// 失败文案应报派发级上限（300ms）而非全局 10m。
	msgs := mb.Drain("meta")
	if len(msgs) == 0 {
		t.Fatal("expected timeout failure notify in mailbox")
	}
	if !strings.Contains(msgs[0].Body, "300ms") {
		t.Fatalf("failure message should report dispatch-level wall clock, got: %s", msgs[0].Body)
	}
}

// TestDispatchOne_DomainReconClockEnforced 验证 domain 侦察墙钟：无显式 wall_clock_min 的
// domain 派发吃 domainReconClock（而非全局 timeout），超时被终止且失败文案报侦察上限。
// 显式 wall_clock_min 仍优先生效（不被 reconClock 放大）。
func TestDispatchOne_DomainReconClockEnforced(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles:  []types.RoleDefinition{},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	// 全局 10min、侦察墙钟 300ms：domain 无 wall_clock_min 应被 reconClock 收口。
	d := NewDispatcher(reg, &mockModelFactory{provider: &ctxAwareHangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithTimeout(10 * time.Minute).
		WithDomainReconClock(300 * time.Millisecond)
	d.RegisterCallTool(toolsReg)
	t.Cleanup(d.ClosePatrol)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "测试",
		"responsibility": "测试领域",
		"task":           "hang",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.PendingChildren("meta") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.PendingChildren("meta") != 0 {
		t.Fatalf("recon clock should terminate domain agent, pending=%d", d.PendingChildren("meta"))
	}
	msgs := mb.Drain("meta")
	if len(msgs) == 0 {
		t.Fatal("expected timeout failure notify in mailbox")
	}
	if !strings.Contains(msgs[0].Body, "300ms") {
		t.Fatalf("failure message should report recon clock limit, got: %s", msgs[0].Body)
	}
}

// TestWrapEngineLLM_TimeoutAndKeepalive 验证引擎辅助 LLM 包装：
// 1) 整次调用（含内部挂起）被 engineLLMTimeout 快速掐断（不再烧到墙钟）；
// 2) 证据化（TODO 第10项②）：调用期间 llmInFlight=true（巡检豁免，替代旧 keepalive
//    ticker 盲报），结束后复位 + lastTS 收口为 llm_end。
func TestWrapEngineLLM_TimeoutAndKeepalive(t *testing.T) {
	d := &Dispatcher{engineLLMTimeout: 60 * time.Millisecond}
	e := &activityEvidence{}
	d.activity.Store("s1/code_assistant-1", e)

	var inFlightDuring bool
	start := time.Now()
	_, err := d.wrapEngineLLM("s1/code_assistant-1", func(ctx context.Context, prompt string) (string, error) {
		inFlightDuring = e.llmInFlight.Load() // 调用中采样：应处于在飞态
		<-ctx.Done()                          // 模拟 SDK 挂起直至超时
		return "", ctx.Err()
	})(context.Background(), "judge prompt")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("engine llm timeout should fail fast, elapsed=%v", elapsed)
	}
	if !inFlightDuring {
		t.Fatal("llmInFlight should be set while engine LLM is in flight")
	}
	if e.llmInFlight.Load() {
		t.Fatal("llmInFlight should be cleared after engine LLM returns")
	}
}

// TestRecordFileWrite 验证 WriteFile/EditFile 实时事件识别、去重与窗口过滤。
func TestRecordFileWrite(t *testing.T) {
	d := &Dispatcher{}
	id := "s1/code_assistant-1"
	d.recordFileWrite(id, agent.LiveEvent{Kind: agent.LiveEventToolCall, Tool: "ReadFile", Input: `{"path":"a.js"}`})
	if v, ok := d.lastWrites.Load(id); ok && len(v.(*fileWriteState).recs) != 0 {
		t.Fatalf("non-write tool should not record, got: %v", v)
	}
	d.recordFileWrite(id, agent.LiveEvent{Kind: agent.LiveEventToolCall, Tool: "WriteFile", Input: `{"path":"engine.js","content":"x"}`})
	d.recordFileWrite(id, agent.LiveEvent{Kind: agent.LiveEventToolCall, Tool: "EditFile", Input: `{"path":"engine.js"}`})
	d.recordFileWrite(id, agent.LiveEvent{Kind: agent.LiveEventToolCall, Tool: "WriteFile", Input: `{"path":"ui.js"}`})
	paths := d.recentWrittenFiles(id, time.Minute)
	if len(paths) != 2 || paths[0] != "engine.js" || paths[1] != "ui.js" {
		t.Fatalf("expected deduped [engine.js ui.js], got: %v", paths)
	}
	if got := d.recentWrittenFiles(id, 0); len(got) != 0 {
		t.Fatalf("zero window should return empty, got: %v", got)
	}
}

// TestHotDomain_WallClockWrapUpNotifiesParent 验证热驻 domain 墙钟到期走失败收口而非静默销毁：
// 父邮箱收到超时回执（报真实预算）+ PendingChildren 归 0 + 树 Failed + 槽销毁。
// 回归 2026-08-28 实证：墙钟 cancel（AfterFunc+cancelTask）与外部硬取消同为 context.Canceled，
// 共用静默销毁路径后不减 PendingChildren、邮箱无消息，MetaAgent 终结保护 wait loop
// 永久空等（对话栏卡死"等待 domain-3 回传"）。既有墙钟测试只覆盖叶子路径
//（context.WithTimeout → DeadlineExceeded → 通用失败分支），热驻路径零覆盖。
func TestHotDomain_WallClockWrapUpNotifiesParent(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d, mb, tr, toolsReg := newIdleTestEnv(t, &ctxAwareHangingProvider{release: release}, time.Hour)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "hang",
		"domain":         "金融",
		"responsibility": "负责金融模块",
		"wall_clock_min": 0.005, // 300ms
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)

	// 槽销毁（墙钟收口后任务 ctx 已死，不可续）。
	waitForCond(t, "slot destroyed after wall clock", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	// 父未决计数归 0（终结保护可退出）。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Fatalf("pending after wall clock wrap-up = %d, want 0", got)
	}
	// 树终态 Failed（而非 destroySlot 的 TTL-Done 覆盖——Finish 幂等首个生效）。
	n, ok := tr.Get(subID)
	if !ok || n.Status != orchestrator.StatusFailed {
		t.Fatalf("tree status = %v ok=%v, want failed", n.Status, ok)
	}
	// 父邮箱收到墙钟超时回执，且报派发级预算（300ms）而非全局值。
	msgs := mb.Drain("s1")
	if len(msgs) == 0 {
		t.Fatal("expected wall clock failure notify in parent mailbox")
	}
	if !strings.Contains(msgs[0].Body, "墙钟预算耗尽") {
		t.Fatalf("failure message should be wall-clock wrap-up, got: %s", msgs[0].Body)
	}
	if !strings.Contains(msgs[0].Body, "300ms") {
		t.Fatalf("failure message should report dispatch-level budget, got: %s", msgs[0].Body)
	}
}

// TestHotDomain_DoneStopsWallTimer 验证任务完结统一停表：DONE 转 Idle 后残留 wallTimer
// 必须已停（回归 2026-08-28 实证：01:20 DONE 的 domain-2 在 02:38 被残留 timer 空放触发）。
func TestHotDomain_DoneStopsWallTimer(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "快速完成",
		"domain":         "金融",
		"responsibility": "负责金融模块",
		"wall_clock_min": 0.005, // 300ms，任务远早于预算完成
	})
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot destroyed after done; want hot-resident")
	}
	s.mu.Lock()
	timerNil := s.wallTimer == nil
	fired := s.wallFired
	s.mu.Unlock()
	if !timerNil {
		t.Error("wallTimer not stopped after task done; stale timer will misfire later")
	}
	if fired {
		t.Error("wallFired should be false after successful done")
	}
}

// TestHotDomain_HardCancelBackstopDecrementsParent 验证 destroySlot 兜底：执行中任务被
// 外部硬取消（wallFired=false 的 Canceled 路径）静默销毁时，兜底递减父未决计数 + 回告父，
// 父终结保护不永久空等。
func TestHotDomain_HardCancelBackstopDecrementsParent(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d, mb, tr, toolsReg := newIdleTestEnv(t, &ctxAwareHangingProvider{release: release}, time.Hour)

	res, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "hang",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	subID := subAgentIDOf(res)
	waitForCond(t, "running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})

	// 模拟外部硬取消（巡检 kill / Tree.Cancel 的 cancelTask 语义；非墙钟、非软停止）。
	// 先等 runDomainTask 入口绑定 cancelTask（dispatch 返回早于 supervisor 绑定，直接读会拿 nil）。
	waitForCond(t, "cancelTask bound", func() bool {
		s := d.pool.slot("s1", subID)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cancelTask != nil
	})
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing")
	}
	s.mu.Lock()
	cancelTask := s.cancelTask
	s.mu.Unlock()
	cancelTask()

	waitForCond(t, "slot destroyed after hard cancel", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	if got := d.PendingChildren("s1"); got != 0 {
		t.Fatalf("pending after hard-cancel backstop = %d, want 0", got)
	}
	msgs := mb.Drain("s1")
	if len(msgs) == 0 {
		t.Fatal("expected backstop notify in parent mailbox")
	}
	if !strings.Contains(msgs[0].Body, "兜底递减") {
		t.Fatalf("backstop notify should explain compensation, got: %s", msgs[0].Body)
	}
}

// TestWallClockWarnLadder_DeliversEscalating 验证递进预警阶梯：50%/75%/90% 三档按序投递，
// 文案逐级升压（一半 -> 立即停止 -> 最终预警）。阈值临时缩小到毫秒级以便快速验证。
func TestWallClockWarnLadder_DeliversEscalating(t *testing.T) {
	oldOffset, oldRemain := wallClockWarnMinOffset, wallClockWarnMinRemain
	wallClockWarnMinOffset, wallClockWarnMinRemain = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { wallClockWarnMinOffset, wallClockWarnMinRemain = oldOffset, oldRemain })

	mb := mailbox.New()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	wallClockWarnLadder(mb, "sub-1", 150*time.Millisecond, done)

	// 三档触发点 75ms/112.5ms/135ms，留足余量后一次性 Drain（Drain 取出即删，不可轮询消费）。
	time.Sleep(600 * time.Millisecond)
	msgs := mb.Drain("sub-1")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 escalating warnings, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Body, "一半") {
		t.Fatalf("first warning should be 50%% planning notice, got %q", msgs[0].Body)
	}
	if !strings.Contains(msgs[1].Body, "立即停止") {
		t.Fatalf("second warning should be 75%% wrap-up notice, got %q", msgs[1].Body)
	}
	if !strings.Contains(msgs[2].Body, "最终预警") {
		t.Fatalf("third warning should be 90%% final ultimatum, got %q", msgs[2].Body)
	}
}

// TestWallClockWarnLadder_ShortClockSkipsAll 短墙钟（各档位距派发/到期 <30s）全部跳过，
// 与旧行为一致退化为仅到期硬杀（零行为变化）。
func TestWallClockWarnLadder_ShortClockSkipsAll(t *testing.T) {
	mb := mailbox.New()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	wallClockWarnLadder(mb, "sub-1", 300*time.Millisecond, done) // 默认 30s 阈值下三档全跳过
	time.Sleep(500 * time.Millisecond)
	if msgs := mb.Drain("sub-1"); len(msgs) != 0 {
		t.Fatalf("short wall clock should skip all warning steps, got %d", len(msgs))
	}
}

// TestWallClockWarnLadder_DoneStopsDelivery 任务结束（done 关闭）后档位不再投递，
// goroutine 随之退出（零泄漏、不污染热驻槽的下一任务）。
func TestWallClockWarnLadder_DoneStopsDelivery(t *testing.T) {
	oldOffset, oldRemain := wallClockWarnMinOffset, wallClockWarnMinRemain
	wallClockWarnMinOffset, wallClockWarnMinRemain = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { wallClockWarnMinOffset, wallClockWarnMinRemain = oldOffset, oldRemain })

	mb := mailbox.New()
	done := make(chan struct{})
	wallClockWarnLadder(mb, "sub-1", 10*time.Second, done)
	close(done) // 任务立即完成
	time.Sleep(300 * time.Millisecond)
	if msgs := mb.Drain("sub-1"); len(msgs) != 0 {
		t.Fatalf("no warnings should be delivered after done, got %d", len(msgs))
	}
}

// mapWallClockTestEnv 构造 map 墙钟测试环境：spec_exempt 侦察角色 + 挂起 provider。
// 全局超时 10min（远大于各项预算，证明收口来自派发级墙钟）。
func mapWallClockTestEnv(t *testing.T) (*Dispatcher, *mailbox.Mailbox, chan struct{}) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "scout", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "scout", SpecExempt: true},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	d := NewDispatcher(reg, &mockModelFactory{provider: &ctxAwareHangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithTimeout(10 * time.Minute)
	d.RegisterCallTool(toolsReg)
	t.Cleanup(d.ClosePatrol)
	return d, mb, release
}

// TestMapSubAgents_SpecExemptDefaultWallClock 验证 map 批量路径继承 spec_exempt 缺省墙钟：
// 角色 SpecExempt=true 且未给 wall_clock_min 时，每项经 dispatchOne 注入 5 分钟兜底预算
// （2026-09-20 关闭 V5 遗留"map 批量实际生效路径未验证"——与单派同一条注入链）。
func TestMapSubAgents_SpecExemptDefaultWallClock(t *testing.T) {
	d, _, _ := mapWallClockTestEnv(t)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := d.tools.Dispatch(ctx, "map_sub_agents", map[string]any{
		"role_id":       "scout",
		"task_template": "侦察 {{item}}",
		"items":         []any{"alpha"},
	}); err != nil {
		t.Fatalf("map dispatch: %v", err)
	}

	// dispatchOne 同步注册 subMeta：逐项读有效墙钟，应为缺省 5 分钟（非全局 10 分钟、非 0）。
	found := false
	d.subMeta.Range(func(k, v any) bool {
		found = true
		if got := v.(*subAgentMeta).wallClock; got != 5*time.Minute {
			t.Fatalf("spec_exempt default wall clock via map = %v, want 5m (sub=%s)", got, k.(string))
		}
		return true
	})
	if !found {
		t.Fatal("no sub-agent registered after map dispatch")
	}
}

// TestMapSubAgents_ExplicitWallClockEnforced 验证 map 批量 + 显式 wall_clock_min 的
// 端到端强制收口（V5 遗留核心疑问）：到点硬杀、父未决计数归 0、聚合消息把被杀项
// 标"失败"（而非旧实现的恒"完成"）且文案报显式预算 300ms。
func TestMapSubAgents_ExplicitWallClockEnforced(t *testing.T) {
	d, mb, _ := mapWallClockTestEnv(t)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := d.tools.Dispatch(ctx, "map_sub_agents", map[string]any{
		"role_id":        "scout",
		"task_template":  "侦察 {{item}}",
		"items":          []any{"alpha", "beta"},
		"wall_clock_min": 0.005, // 300ms/项
		"aggregate":      true,
	}); err != nil {
		t.Fatalf("map dispatch: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.PendingChildren("meta") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.PendingChildren("meta") != 0 {
		t.Fatalf("explicit wall clock should terminate map items, pending=%d", d.PendingChildren("meta"))
	}

	// 聚合单条回传：两项均标失败、计数 0/2、失败项首行带 timeout 机读标记
	//（单项摘要按首行展示，"上限 300ms"全文在台账/回传路径可查——firstLine 截断为设计口径）。
	msgs := mb.Drain("meta")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one aggregate message, got %d", len(msgs))
	}
	body := msgs[0].Body
	if !strings.Contains(body, "【map_sub_agents 聚合回传】") {
		t.Fatalf("aggregate message marker missing, got: %s", body)
	}
	if !strings.Contains(body, "完成 0 / 失败 2") {
		t.Fatalf("killed items should be counted as failed, got: %s", body)
	}
	if strings.Count(body, "[failure kind=timeout") != 2 {
		t.Fatalf("both items should carry timeout failure marker, got: %s", body)
	}
}


// TestSlotWallClock_QueueDoesNotBurnBudget 验证热驻槽"排队不计墙钟"（P1"出队才计墙钟"
// 在热驻第5条路径的补齐）：任务入场仅 resetSlotWallClock（timer 不挂载），并发池排队
// 等待再久也不触发；过闸（出队）后 armWallClock 挂载，任务以全额预算起跑——到期点 =
// 挂载点+预算，而非入场点+预算。
// 旧行为回归：runDomainTask 入口 armWallClock 会把排队时长烧进预算；排队超预算时 timer
// mid-queue 触发 cancelTask -> Acquire 出局 -> wallFired 分支，给从未执行一轮的任务
// 误报墙钟失败。本测试的"reset 后排队超预算不触发"在旧语义下必然失败（timer 会 firing）。
func TestSlotWallClock_QueueDoesNotBurnBudget(t *testing.T) {
	d := &Dispatcher{timeout: time.Hour} // mailbox=nil：wallClockWarnLadder 直通返回
	s := &domainSlot{id: "s1/domain-1", domain: "金融"}
	taskCtx, cancelTask := context.WithCancel(context.Background())
	t.Cleanup(cancelTask)

	const budget = 150 * time.Millisecond

	// 模拟残留/旧式入场挂载，随后走新入场 bookkeeping：reset 必须解除已挂载的 timer。
	d.armWallClock(s, taskCtx, cancelTask, budget)
	resetSlotWallClock(s)
	s.mu.Lock()
	if s.wallTimer != nil || s.wallFired {
		s.mu.Unlock()
		t.Fatal("entry reset should leave no armed timer and wallFired=false")
	}
	s.mu.Unlock()

	// 排队等待 2 倍预算：原 timer 不得再 firing，任务 ctx 不得被取消（排队不烧预算）。
	time.Sleep(2 * budget)
	s.mu.Lock()
	firedDuringQueue := s.wallFired
	s.mu.Unlock()
	if firedDuringQueue || taskCtx.Err() != nil {
		t.Fatalf("queue wait must not fire wall clock: fired=%v ctxErr=%v", firedDuringQueue, taskCtx.Err())
	}

	// 过闸（出队）挂载：起足全额预算——半预算点不触发，满预算才触发并 cancel 任务 ctx。
	armAt := time.Now()
	d.armWallClock(s, taskCtx, cancelTask, budget)
	time.Sleep(budget / 2)
	s.mu.Lock()
	firedEarly := s.wallFired
	s.mu.Unlock()
	if firedEarly {
		t.Fatal("post-gate arm should grant a fresh full budget; fired at half budget")
	}
	waitForCond(t, "wall clock fires full budget after dequeue", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.wallFired
	})
	if elapsed := time.Since(armAt); elapsed < budget {
		t.Fatalf("wall clock fired %v after post-gate arm, before full budget %v", elapsed, budget)
	}
	if taskCtx.Err() == nil {
		t.Fatal("wall clock fire should cancel task ctx")
	}
}

// TestSlotWallClock_GateAbortLeavesNoTimer 验证 gate-abort（排队期被取消、未过闸即出局）
// 路径的 timer 状态：入场 reset 后不再 arm，wallTimer 保持 nil、wallFired=false——
// runDomainTask 的 Canceled 分支据此走外部取消分类（软停止/手动暂停/硬取消），不会把
// 排队期取消误判成墙钟失败；任务终结的统一停表 stopWallClock 对无 timer 槽为无害 no-op。
func TestSlotWallClock_GateAbortLeavesNoTimer(t *testing.T) {
	s := &domainSlot{id: "s1/domain-2", domain: "金融"}
	resetSlotWallClock(s) // 入场 bookkeeping；gateErr 路径不再 arm
	stopWallClock(s)      // runDomainTask 任务终结统一停表：无 timer 时 no-op
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wallTimer != nil {
		t.Fatal("gate-abort path should leave no wall timer")
	}
	if s.wallFired {
		t.Fatal("gate-abort path must keep wallFired=false for external-cancel classification")
	}
}
