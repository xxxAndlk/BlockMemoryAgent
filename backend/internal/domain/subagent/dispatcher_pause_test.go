package subagent

// dispatcher_pause_test.go 验证 DomainAgent 触达 token 上限 -> Paused + 持久化 history
// 与叶子助手 -> 部分回灌两条路径，以及 HasPausedChild 检测。
//
// 关键断言：
//   - domain LimitReached：tree.Paused + msgStore.SaveMessages 调用 + 父 pending 不减 + 父 mailbox 不 notify。
//   - assistant LimitReached：tree.Finish Done + 父 notify + 父 pending 减。
//   - HasPausedChild：父有 Paused 子 domain 时返回 true。
//
// 经 call_sub_agent 工具派发（含 trackChildDone 包装器），覆盖 wrapper 的 paused 分支语义。

import (
	"context"
	"strings"
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

// tokenUsageProvider 返回带 TokenUsage 的 assistant 文本，使单次调用即超 budget 触发 LimitReached。
type tokenUsageProvider struct {
	text string
}

func (m *tokenUsageProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	msg := blades.AssistantMessage(m.text)
	msg.TokenUsage = blades.TokenUsage{InputTokens: 100, OutputTokens: 100, TotalTokens: 200}
	return &blades.ModelResponse{Message: msg}, nil
}
func (m *tokenUsageProvider) Name() string { return "token-usage-mock" }

// savedMsg 记录一次 SaveMessages 调用参数。
type savedMsg struct {
	agentID   string
	sessionID string
	msgs      []agent.ReactMessage
}

// captureMessagesStore 记录 SaveMessages 调用，LoadMessages 回放缓存的最近一份历史（供 resume 路径测试）。
type captureMessagesStore struct {
	saved []savedMsg
}

func (c *captureMessagesStore) SaveMessages(_ context.Context, agentID, sessionID string, msgs []agent.ReactMessage) error {
	c.saved = append(c.saved, savedMsg{agentID: agentID, sessionID: sessionID, msgs: msgs})
	return nil
}
func (c *captureMessagesStore) LoadMessages(_ context.Context, agentID string) ([]agent.ReactMessage, error) {
	for i := len(c.saved) - 1; i >= 0; i-- {
		if c.saved[i].agentID == agentID {
			return c.saved[i].msgs, nil
		}
	}
	return nil, nil
}

// newPauseTestEnv 构造一个带 tree + msgStore + 小 budget 的 Dispatcher 测试环境，
// 并注册 call_sub_agent 工具，使测试可经 Execute（含 trackChildDone 包装器）派发。
func newPauseTestEnv(t *testing.T, provider agent.ModelProvider) (*Dispatcher, *role.Registry, *mailbox.Mailbox, *captureMessagesStore, *orchestrator.Tree, *tool.Registry) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "通用领域纪律"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: provider}, toolsReg, mb, agent.NopMemoryPipeline{})
	// 小 budget：单次调用（200 token）即超限触发 LimitReached。
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{TokenBudget: 10, MaxIterations: 50} })
	msgStore := &captureMessagesStore{}
	d.WithMessagesStore(msgStore)
	tr := orchestrator.NewTree("s1", nil)
	d.WithTree(func(string) *orchestrator.Tree { return tr })
	d.RegisterCallTool(toolsReg)
	return d, reg, mb, msgStore, tr, toolsReg
}

// dispatchCtx 构造带 sessionID + agentID(=meta parentID) 的派发上下文。
func dispatchCtx() context.Context {
	return tool.WithSessionID(agent.WithAgentID(context.Background(), "s1"), "s1")
}

// TestRunSubAgent_DomainLimitReachedPauses 验证 DomainAgent 触达 token 上限（经 Execute 包装器）：
// tree.Paused + msgStore 存 history + 父 pending 不减（paused 不 trackChildDone）+ 父 mailbox 不 notify。
func TestRunSubAgent_DomainLimitReachedPauses(t *testing.T) {
	d, _, mb, msgStore, tr, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "domain work"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "实现 config.js",
		"domain":         "配置",
		"responsibility": "负责 config.js",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch domain failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	// 等 tree 节点进入 Paused。
	waitForCond(t, "domain paused", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusPaused
	})
	// 父 pending 不减（paused 路径不 trackChildDone）。
	if got := d.PendingChildren("s1"); got != 1 {
		t.Errorf("pending after = %d, want 1 (paused must not decrement)", got)
	}
	// msgStore 存 history。
	if len(msgStore.saved) != 1 || msgStore.saved[0].agentID != subID {
		t.Errorf("msgStore.SaveMessages not called with subID=%s, got %+v", subID, msgStore.saved)
	}
	// 父 mailbox 不 notify。
	if drains := mb.Drain("s1"); len(drains) != 0 {
		t.Errorf("parent mailbox should be empty on pause, got %d msgs", len(drains))
	}
}

// TestRunSubAgent_AssistantLimitReachedPartialReturn 验证叶子助手触达 token 上限（经 Execute 包装器）：
// tree.Finish Done + 父 mailbox notify + 父 pending 减（trackChildDone 照常）。
func TestRunSubAgent_AssistantLimitReachedPartialReturn(t *testing.T) {
	d, _, mb, _, tr, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "partial work"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "write code",
		"verify_kind": "none", // 本测试验证 token 上限部分返回路径，校验分层无关
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch assistant failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	// 等 tree 节点 Done（部分回灌标 Done）。
	waitForCond(t, "assistant done", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusDone
	})
	// 父 pending 减。
	if got := d.PendingChildren("s1"); got != 0 {
		t.Errorf("pending after = %d, want 0 (assistant must decrement)", got)
	}
	// 父 mailbox notify 部分产出。
	if drains := mb.Drain("s1"); len(drains) == 0 {
		t.Error("parent mailbox should have notify on partial return")
	}
}

// TestResumePaused_ConcludesAfterResumeCap 验证 Paused domain 的续跑次数上限（maxPausedResumes=1）：
// 首次 ResumePaused 正常续跑（mock 仍触限 -> re-pause，父 pending 不减、mailbox 不 notify）；
// 第二次 ResumePaused 触顶 -> 强制收口：tree.Finish Done + 父 mailbox notify（含"续跑上限"）+ 父 pending 减。
// 绑定点在续跑层：续跑重置 fresh budget，无上限则"触限-暂停-续跑"环路永不收敛（v10 实证研磨 30 分钟）。
func TestResumePaused_ConcludesAfterResumeCap(t *testing.T) {
	d, _, mb, msgStore, tr, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "domain work"})
	d.WithMaxPausedResumes(1)
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "实现 config.js",
		"domain":         "配置",
		"responsibility": "负责 config.js",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch domain failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	waitForCond(t, "domain paused", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusPaused
	})
	if len(msgStore.saved) != 1 {
		t.Fatalf("want 1 persisted history, got %d", len(msgStore.saved))
	}

	// 第一次续跑：正常路径，mock 继续触限 -> re-pause。
	r1, err := d.ResumePaused(dispatchCtx(), subID)
	if err != nil {
		t.Fatalf("first resume should not error, got %v", err)
	}
	if !r1.LimitReached {
		t.Fatalf("first resume should re-hit limit (re-pause), got %+v", r1)
	}
	if n, ok := tr.Get(subID); !ok || n.Status != orchestrator.StatusPaused {
		t.Fatalf("after first resume node should be Paused again, got %+v", n)
	}
	if got := d.PendingChildren("s1"); got != 1 {
		t.Fatalf("pending after re-pause = %d, want 1", got)
	}
	if drains := mb.Drain("s1"); len(drains) != 0 {
		t.Fatalf("mailbox should stay empty on re-pause, got %d msgs", len(drains))
	}

	// 第二次续跑：触顶 -> 强制收口部分返回。
	r2, err := d.ResumePaused(dispatchCtx(), subID)
	if err != nil {
		t.Fatalf("conclude should not error, got %v", err)
	}
	if !strings.Contains(r2.Text, "续跑上限") {
		t.Fatalf("conclude text should mention resume cap, got %q", r2.Text)
	}
	waitForCond(t, "node done after conclude", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusDone
	})
	if got := d.PendingChildren("s1"); got != 0 {
		t.Fatalf("pending after conclude = %d, want 0", got)
	}
	drains := mb.Drain("s1")
	if len(drains) != 1 {
		t.Fatalf("parent mailbox should have exactly 1 conclude notify, got %d", len(drains))
	}
	if !strings.Contains(drains[0].Body, "续跑上限") {
		t.Fatalf("conclude notify should mention resume cap, got %q", drains[0].Body)
	}
}
func TestDispatcher_HasPausedChild(t *testing.T) {
	d, _, _, _, tr, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "x"})
	parentID := "s1"
	// 无 Paused 子节点 -> false。
	if d.HasPausedChild(parentID) {
		t.Fatal("HasPausedChild should be false with no paused child")
	}
	// 注册一个 Paused domain 子节点。
	tr.Register(orchestrator.Node{ID: "s1/domain-1", ParentID: parentID, Role: "domain", Status: orchestrator.StatusRunning, Started: time.Now()})
	tr.Pause("s1/domain-1", "token budget exhausted")
	if !d.HasPausedChild(parentID) {
		t.Error("HasPausedChild should be true after pausing a domain child")
	}
	// Finish 后不再是 Paused -> false。
	tr.Finish("s1/domain-1", "done", nil)
	if d.HasPausedChild(parentID) {
		t.Error("HasPausedChild should be false after child finishes")
	}
}

// TestDispatcher_HasPausedChildNilTree 验证 treeFn 为 nil 时不 panic 且返回 false。
func TestDispatcher_HasPausedChildNilTree(t *testing.T) {
	d := NewDispatcher(role.NewRegistry(&config.RoleConfigFile{}), &mockModelFactory{provider: &mockProvider{text: "x"}}, nil, nil, agent.NopMemoryPipeline{})
	if d.HasPausedChild("any") {
		t.Error("HasPausedChild should be false when treeFn is nil")
	}
}

// TestRunSubAgent_ManualPauseRoutesToPausedTree 验证手动单支暂停（TODO 第9⑥/10③ 审计面）：
// MarkPauseNode + StopRunning 触发 domain ctx 取消，isPauseRequested 命中走 Pause 收尾——
// 树节点 Paused（非 Failed/Cancelled）、父 pending 不减、父 mailbox 不 notify、
// 节点暂停标记收尾即清（防残留误分流后续取消）。
func TestRunSubAgent_ManualPauseRoutesToPausedTree(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 释放悬挂 goroutine，防泄漏
	d, _, mb, _, tr, toolsReg := newPauseTestEnv(t, &ctxAwareHangingProvider{release: release})
	// 覆盖 env 默认的小 budget（10 token 会在悬挂前先触发 LimitReached pause，
	// 抢占手动暂停路径）——本测试只验证手动暂停分流。
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{TokenBudget: 1 << 30, MaxIterations: 50} })
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "悬挂的 domain 任务",
		"domain":         "测试",
		"responsibility": "负责测试",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch domain failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	// 前提：节点已 Running 且悬挂在 Generate 中。
	waitForCond(t, "domain running", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusRunning
	})

	// 手动暂停：登记标记后触发该支路 cancel（PauseAgent 服务层语义的两步）。
	d.MarkPauseNode(subID)
	if !tr.StopRunning(subID) {
		t.Fatal("StopRunning should succeed on a Running domain node")
	}
	waitForCond(t, "domain paused by manual pause", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusPaused
	})
	// 收尾清标记。
	if d.isPauseRequested(subID) {
		t.Error("pause marker should be cleared after pause finalize")
	}
	// 父 pending 不减（paused 不 trackChildDone）。
	if got := d.PendingChildren("s1"); got != 1 {
		t.Errorf("pending after manual pause = %d, want 1", got)
	}
	// 父 mailbox 不 notify。
	if drains := mb.Drain("s1"); len(drains) != 0 {
		t.Errorf("parent mailbox should be empty on manual pause, got %d msgs", len(drains))
	}
}

