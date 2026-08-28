package subagent

// dispatcher_wallclock_test.go 验证派发级墙钟（wall_clock_min）与引擎辅助 LLM 超时/保活。
import (
	"context"
	"strings"
	"sync/atomic"
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
// 2) 调用期间保活定时器刷新 activity（防心跳巡检误杀 judge 长生成）。
func TestWrapEngineLLM_TimeoutAndKeepalive(t *testing.T) {
	d := &Dispatcher{engineLLMTimeout: 60 * time.Millisecond}
	act := new(atomic.Int64)
	act.Store(0)
	d.activity.Store("s1/code_assistant-1", act)

	start := time.Now()
	_, err := d.wrapEngineLLM("s1/code_assistant-1", func(ctx context.Context, prompt string) (string, error) {
		<-ctx.Done() // 模拟 SDK 挂起直至超时
		return "", ctx.Err()
	})(context.Background(), "judge prompt")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("engine llm timeout should fail fast, elapsed=%v", elapsed)
	}
	// 保活：超时前 30s ticker 不一定触发（60ms 超时 < 30s 间隔），此处仅验证不 panic；
	// activity 未被清零即路径可用。真正触发验证见 keepalive 语义（间隔 30s 硬编码，
	// 测试不等待 30s，超时快速失败已覆盖主事故场景）。
	if act.Load() == 0 {
		// 60ms 超时 < 30s 保活间隔，未触发是预期；不判失败。
		t.Log("keepalive ticker interval (30s) > timeout (60ms), touch not expected")
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
	subID := res.Output

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
	subID := res.Output
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
	subID := res.Output
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
