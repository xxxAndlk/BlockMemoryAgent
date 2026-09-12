package subagent

// dispatcher_heartbeat_test.go 验证 stall 巡检主动 kill 假死叶子子 Agent（步间静默形态）。
import (
	"context"
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
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// hangingProvider 模拟 LLM 调用挂起（慢思考/流式首块前停滞）：Generate 阻塞至 release
// 关闭，忽略 ctx 取消。证据化语义（TODO 第10项②）下该形态被 llmInFlight 豁免巡检，
// 兜底收敛归 react_llm_timeout（CallLLM 单呼超时）+ 流空闲卡口 + 会话墙钟。
type hangingProvider struct {
	release chan struct{}
}

func (h *hangingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-h.release
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}
func (h *hangingProvider) Name() string { return "hanging" }

// TestDispatcher_HeartbeatKillsStuckLeaf 验证叶子 Agent 假死（Generate 无返回）时，
// TestDispatcher_StuckLeaf_InFlightLLMExempt 验证证据化豁免（TODO 第10项②）：
// 端到端派发的叶子 Agent 在 LLM 调用挂起（llmInFlight=true）期间即使远超巡检阈值
// 也不被 patrol kill——task-115 类慢思考误杀根因修复。挂死 LLM 的兜底收敛归
// react_llm_timeout（CallLLM 单呼超时，默认 300s）+ 流空闲卡口 + 会话墙钟。
// 巡检 kill 路径（cancel+notify+兜底递减）的端到端覆盖见 TestDispatcher_KillStuckNilCancelNoPanic；
// 判定三态分流见 stall_evidence_test.go。
func TestDispatcher_StuckLeaf_InFlightLLMExempt(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 测试结束释放悬挂 goroutine，防泄漏

	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithHeartbeatTimeout(200 * time.Millisecond)
	d.RegisterCallTool(toolsReg)
	t.Cleanup(d.ClosePatrol)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "hang",
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected dispatch success, got: %s", res.Error)
	}
	subID := subAgentIDOf(res)

	// 前提守卫：reporter 已注入且 llm_start 置 llmInFlight=true（Generate 挂起中）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e := d.activityEvidenceFor(subID); e != nil && e.llmInFlight.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	e := d.activityEvidenceFor(subID)
	if e == nil || !e.llmInFlight.Load() {
		t.Fatal("premise: leaf evidence should have llmInFlight=true while Generate hangs")
	}

	// 等待 3× 阈值：在飞 LLM 期间巡检不得 kill。
	time.Sleep(600 * time.Millisecond)
	if d.PendingChildren("meta") != 1 {
		t.Fatalf("in-flight LLM leaf must NOT be patrol-killed, pending=%d", d.PendingChildren("meta"))
	}
	if msgs := mb.Drain("meta"); len(msgs) > 0 {
		t.Fatalf("no kill notify expected while LLM in flight, got: %s", msgs[0].Body)
	}
}

// TestDispatcher_HeartbeatNoPatrolWhenDisabled 验证 heartbeatTimeout<=0 时不启动巡检：
// 假死叶子 Agent 不会被 kill，父 PendingChildren 保持 >0（回归保护：默认关闭不误杀）。
func TestDispatcher_HeartbeatNoPatrolWhenDisabled(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles:  []types.RoleDefinition{{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	// heartbeatTimeout=0 -> 巡检关闭。
	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{"role_id": "code_assistant", "task": "hang"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// 等待足够巡检周期（若有 bug 启动巡检，此时应已 kill）。
	time.Sleep(300 * time.Millisecond)
	if d.PendingChildren("meta") == 0 {
		t.Fatal("expected PendingChildren>0 when heartbeat disabled (no patrol), got 0")
	}
}

// TestDispatcher_PingActivityPreventsKill 验证等待用户答复期间的保活探针（PingActivity）：
// 活动时间被持续刷新时巡检不判假死；停止 ping 后超阈值才 kill。
// 回归场景：子 Agent 阻塞在审批/提问等用户答复，期间无 LLM/工具活动（2026-08-18
// 三次误杀均卡在此状态），会话层周期性 PingActivity 保活。
func TestDispatcher_PingActivityPreventsKill(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles:  []types.RoleDefinition{{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()

	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: make(chan struct{})}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithHeartbeatTimeout(150 * time.Millisecond)
	d.ensurePatrol()
	t.Cleanup(d.ClosePatrol)

	id := "sess-1/code_assistant-1"
	killed := make(chan struct{})
	var cancelOnce sync.Once
	d.activity.Store(id, &activityEvidence{}) // lastTS=0：首次 ping 前即静默（PingActivity 刷新后豁免）
	d.subMeta.Store(id, &subAgentMeta{
		parentID:  "meta",
		sessionID: "sess-1",
		cancel:    func() { cancelOnce.Do(func() { close(killed) }) },
	})

	// 模拟保活：每 50ms ping 一次，持续 400ms（> 2× 阈值），期间不得被杀。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-killed:
			t.Fatal("保活期间不应被心跳 kill")
		default:
		}
		d.PingActivity(id)
		time.Sleep(50 * time.Millisecond)
	}

	// 停止保活：超阈值后巡检应 kill。
	select {
	case <-killed:
		// 预期：停止 ping 后被巡检判定假死。
	case <-time.After(3 * time.Second):
		t.Fatal("停止保活后应被心跳 kill")
	}
}

// TestDispatcher_KillStuckNilCancelNoPanic 回归（2026-08-26 实证进程级 panic）：
// 热驻 domain 槽首注册的 subMeta.cancel 为 nil（idle_pool dispatchHotDomain 在任务 ctx
// 创建前注册），挂起等用户续跑/换绑前被巡检命中时 killStuckSubAgent 的 meta.cancel()
// 空指针崩溃。修复后 kill 走 nil 守卫：不 panic + doneOnce 兜底递减 + 父邮箱收到假死通知。
func TestDispatcher_KillStuckNilCancelNoPanic(t *testing.T) {
	d, mb, _, _ := newIdleTestEnv(t, &scriptProvider{lines: []string{"x"}}, time.Hour)
	d.WithHeartbeatTimeout(150 * time.Millisecond)
	t.Cleanup(d.ClosePatrol)

	id := "s1/domain-9"
	d.activity.Store(id, &activityEvidence{}) // lastTS=0：立即判静默
	d.subMeta.Store(id, &subAgentMeta{parentID: "s1", sessionID: "s1"}) // cancel=nil（热驻首注册形态）
	d.ensurePatrol()

	// 巡检应完成 kill（LoadAndDelete subMeta）；期间不得 panic（panic 会直接终止测试进程）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.subMeta.Load(id); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := d.subMeta.Load(id); ok {
		t.Fatal("expected patrol to kill the nil-cancel stuck slot")
	}
	if got := d.PendingChildren("s1"); got != 0 {
		t.Fatalf("PendingChildren = %d, want 0 after doneOnce fallback decrement", got)
	}
	msgs := mb.Drain("s1")
	if len(msgs) == 0 {
		t.Fatal("expected stuck notify in mailbox, got none")
	}
	if !strings.Contains(msgs[0].Body, "假死") && !strings.Contains(msgs[0].Body, "无活动") {
		t.Fatalf("expected stuck notify body, got: %s", msgs[0].Body)
	}
}

// ctxCancelProvider 模拟可被取消的挂起 LLM 调用：Generate 阻塞至 ctx 取消后返回 ctx.Err()
//（区别于 hangingProvider 的无视 ctx，用于验证巡检 kill 的 cancel 真正传导到任务 ctx）。
type ctxCancelProvider struct{}

func (p *ctxCancelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *ctxCancelProvider) Name() string { return "ctx-cancel-mock" }

// TestHotDomain_HeartbeatKillCancelsRunningTask 验证热驻槽假死 kill 的换绑修复：
// runDomainTask 把每任务 cancelTask 换绑进 subMeta，巡检 kill 应真正取消执行中的
// 任务 ctx（ctx 取消 -> Canceled 分支 -> domainTaskDestroyed -> 槽销毁 + 树 Failed），
// 而非旧实现的 subMeta.cancel 恒 nil（要么空指针 panic，要么 kill 后槽继续跑到墙钟）。
func TestHotDomain_HeartbeatKillCancelsRunningTask(t *testing.T) {
	d, mb, tr, toolsReg := newIdleTestEnv(t, &ctxCancelProvider{}, time.Hour)
	d.WithHeartbeatTimeout(150 * time.Millisecond)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "挂起任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)

	// 前提：等引擎 LLM 进入在飞（wrapEngineLLM beginAuxLLM 置 llmInFlight=true）。
	// 换绑先于引擎执行，此时任务 cancelTask 已换绑进 subMeta。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e := d.activityEvidenceFor(subID); e != nil && e.llmInFlight.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 证据化语义（TODO 第10项②）：在飞 LLM 是巡检豁免形态，不再直接构成 kill 触发。
	// 注入"步间静默"证据条目（lastTS=0、无在飞 LLM/工具）触发巡检 kill，
	// 被测对象仍是换绑后 cancel 的传导链：cancel → ctx 取消 → Canceled 分支 → 槽销毁。
	d.activity.Store(subID, &activityEvidence{})

	// 巡检 kill：树节点应落 Failed（步间静默疑似卡死）。
	waitForCond(t, "tree node failed after heartbeat kill", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusFailed
	})
	// cancel 传导到任务 ctx：引擎经 Canceled 分支返回 domainTaskDestroyed，槽被销毁。
	waitForCond(t, "slot destroyed after cancel propagation", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	// 父计数归零（kill 的 doneOnce 兜底递减）。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Fatalf("PendingChildren = %d, want 0 after heartbeat kill", got)
	}
	// 父邮箱收到假死通知。
	found := false
	for _, m := range mb.Drain("s1") {
		if strings.Contains(m.Body, "假死") || strings.Contains(m.Body, "无活动") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected stuck notify in parent mailbox")
	}
}
