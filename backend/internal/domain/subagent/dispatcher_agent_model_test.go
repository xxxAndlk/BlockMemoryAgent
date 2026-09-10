package subagent

// dispatcher_agent_model_test.go 覆盖实例级模型覆盖（set_agent_model）与叶子治理：
//   - 工具门：参数必填、未知节点、越级（caller != node.ParentID）、meta 拒绝、终态拒绝、成功回执；
//   - 端到端：叶子暂停（history 落 msgStore）-> set_agent_model -> resume 异步续跑 -> 终态回收覆盖；
//   - GC 站点：终态漏斗清覆盖、暂停路径不清（覆盖须跨 resume 存活）、cancel_agent 清覆盖。

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// setAgentModelCall 记录一次 SetAgentModel 调用参数。
type setAgentModelCall struct {
	agentID string
	roleID  string
	modelID string
}

// switchableModelFactory 在 mockModelFactory 基础上实现实例级覆盖接口
// （GetBladesProviderForAgent/SetAgentModel/ClearAgentModel），并记录调用供断言。
type switchableModelFactory struct {
	provider agent.ModelProvider
	mu       sync.Mutex
	sets     []setAgentModelCall
	cleared  []string
}

func (f *switchableModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	return f.provider, nil
}

func (f *switchableModelFactory) GetBladesProviderForAgent(ctx context.Context, roleID, agentID string) (agent.ModelProvider, error) {
	return f.provider, nil
}

func (f *switchableModelFactory) SetAgentModel(ctx context.Context, agentID, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, setAgentModelCall{agentID: agentID, roleID: roleID, modelID: modelID})
	return types.AgentModelConfig{Provider: "mock", Model: "model-" + modelID, Thinking: thinking}, nil
}

func (f *switchableModelFactory) ClearAgentModel(agentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, agentID)
}

func (f *switchableModelFactory) setCalls() []setAgentModelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setAgentModelCall(nil), f.sets...)
}

func (f *switchableModelFactory) clearedContains(agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.cleared, agentID)
}

// hangThenCompleteProvider 首次 Generate 悬挂直到 ctx 取消（模拟运行中的长任务），
// 后续调用直接返回终止文本（模拟续跑完成）。
type hangThenCompleteProvider struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (p *hangThenCompleteProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage(p.text)}, nil
}

func (p *hangThenCompleteProvider) Name() string { return "hang-then-complete" }

// TestSetAgentModelTool_Gates 覆盖 set_agent_model 的拒绝路径与成功回执。
func TestSetAgentModelTool_Gates(t *testing.T) {
	d, _, _, _, tr, _ := newPauseTestEnv(t, &scriptProvider{lines: []string{"x"}})
	sf := &switchableModelFactory{provider: &scriptProvider{lines: []string{"x"}}}
	d.models = sf
	impl := &setAgentModelTool{dispatcher: d}

	// 参数必填。
	if res := impl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-1"}); res.Success || !strings.Contains(res.Error, "model_id 必填") {
		t.Errorf("缺 model_id 应拒绝: %+v", res)
	}
	// 未知节点。
	if res := impl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-9", "model_id": "m"}); res.Success || !strings.Contains(res.Error, "不存在") {
		t.Errorf("未知节点应拒绝: %+v", res)
	}
	// 越级：节点由 s1/domain-1 派发，调用方是 s1。
	tr.Register(orchestrator.Node{ID: "s1/leaf-1", ParentID: "s1/domain-1", Role: "code_assistant", Status: orchestrator.StatusRunning, Started: time.Now()})
	if res := impl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-1", "model_id": "m"}); res.Success || !strings.Contains(res.Error, "越级") {
		t.Errorf("非父调用应拒绝: %+v", res)
	}
	// meta 拒绝（无 caller 语义的调用 ctx，跳过授权门）。
	tr.Register(orchestrator.Node{ID: "s1/meta-x", ParentID: "", Role: "meta", Status: orchestrator.StatusRunning, Started: time.Now()})
	if res := impl.Execute(tool.WithSessionID(context.Background(), "s1"), map[string]any{"agent_id": "s1/meta-x", "model_id": "m"}); res.Success || !strings.Contains(res.Error, "meta") {
		t.Errorf("meta 应拒绝: %+v", res)
	}
	// 终态拒绝。
	tr.Register(orchestrator.Node{ID: "s1/leaf-2", ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusDone, Started: time.Now()})
	if res := impl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-2", "model_id": "m"}); res.Success || !strings.Contains(res.Error, "仅运行中") {
		t.Errorf("终态节点应拒绝: %+v", res)
	}
	// 成功：Running 叶子 + 父调用。
	tr.Register(orchestrator.Node{ID: "s1/leaf-3", ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning, Started: time.Now()})
	res := impl.Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-3", "model_id": "glm-flash", "reason": "提速"})
	if !res.Success || !strings.Contains(res.Output, "仅本实例生效") || !strings.Contains(res.Output, "提速") {
		t.Fatalf("成功回执不符: %+v", res)
	}
	calls := sf.setCalls()
	if len(calls) != 1 || calls[0].agentID != "s1/leaf-3" || calls[0].modelID != "glm-flash" || calls[0].roleID != "code_assistant" {
		t.Fatalf("SetAgentModel calls = %+v", calls)
	}
}

// TestLeafPauseSetModelResume_EndToEnd 验证 domain 管叶子的完整链路：
// 派发叶子（悬挂）-> pause_agent（history 落 msgStore + 节点 Paused + 父收处置通知）-> set_agent_model ->
// resume_agent 异步续跑 -> 完成回灌 -> 终态漏斗回收实例覆盖。
func TestLeafPauseSetModelResume_EndToEnd(t *testing.T) {
	prov := &hangThenCompleteProvider{text: "叶子完成"}
	d, _, mb, msgStore, tr, toolsReg := newPauseTestEnv(t, prov)
	sf := &switchableModelFactory{provider: prov}
	d.models = sf
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{TokenBudget: 1 << 30, MaxIterations: 50} })

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch leaf failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	waitForCond(t, "leaf running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})

	// 暂停（domain 视角：调用方 = 叶子的父）。
	leafCtx := tool.WithSessionID(agent.WithAgentID(context.Background(), "s1"), "s1")
	pres := (&pauseAgentTool{dispatcher: d}).Execute(leafCtx, map[string]any{"agent_id": subID, "reason": "换档"})
	if !pres.Success {
		t.Fatalf("pause leaf failed: %+v", pres)
	}
	waitForCond(t, "leaf paused", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusPaused
	})
	if len(msgStore.saved) == 0 {
		t.Fatal("暂停应把 history 落 msgStore 供续跑")
	}
	if sf.clearedContains(subID) {
		t.Fatal("暂停路径不得回收覆盖（覆盖须跨 resume 存活）")
	}
	if drains := mb.Drain("s1"); len(drains) != 1 || !strings.Contains(drains[0].Subject, "已暂停") {
		t.Fatalf("暂停应通知父处置: %+v", drains)
	}

	// 换档（暂停态允许预置）。
	sres := (&setAgentModelTool{dispatcher: d}).Execute(leafCtx, map[string]any{"agent_id": subID, "model_id": "glm-flash"})
	if !sres.Success {
		t.Fatalf("set_agent_model failed: %+v", sres)
	}

	// 续跑：非热驻异步路径，完成后回灌父邮箱并回收覆盖。
	rres := (&resumeAgentTool{dispatcher: d}).Execute(leafCtx, map[string]any{"agent_id": subID})
	if !rres.Success || !strings.Contains(rres.Output, "异步") {
		t.Fatalf("resume leaf failed: %+v", rres)
	}
	waitForCond(t, "leaf done after resume", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusDone
	})
	waitForCond(t, "override cleared at terminal", func() bool { return sf.clearedContains(subID) })
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after resume done = %d, want 0", got)
	}
	drains := mb.Drain("s1")
	if len(drains) != 1 || !strings.Contains(drains[0].Body, "叶子完成") {
		t.Errorf("完成后应有一条结果回灌, got %+v", drains)
	}
}

// TestCancelAgentTool_ClearsOverride 验证 cancel_agent 收尾回收实例覆盖（GC 旁路站点）。
func TestCancelAgentTool_ClearsOverride(t *testing.T) {
	d, _, _, _, tr, _ := newPauseTestEnv(t, &scriptProvider{lines: []string{"x"}})
	sf := &switchableModelFactory{provider: &scriptProvider{lines: []string{"x"}}}
	d.models = sf
	tr.Register(orchestrator.Node{ID: "s1/leaf-1", ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning, Started: time.Now()})
	res := (&cancelAgentTool{dispatcher: d}).Execute(dispatchCtx(), map[string]any{"agent_id": "s1/leaf-1"})
	if !res.Success {
		t.Fatalf("cancel failed: %+v", res)
	}
	if !sf.clearedContains("s1/leaf-1") {
		t.Fatalf("cancel_agent 应回收实例覆盖, cleared=%+v", sf.cleared)
	}
}
