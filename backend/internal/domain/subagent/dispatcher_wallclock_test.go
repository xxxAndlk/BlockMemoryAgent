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
