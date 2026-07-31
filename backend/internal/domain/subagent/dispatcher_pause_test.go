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

// captureMessagesStore 记录 SaveMessages 调用，LoadMessages 返回 nil（resume 路径另有集成测试）。
type captureMessagesStore struct {
	saved []savedMsg
}

func (c *captureMessagesStore) SaveMessages(_ context.Context, agentID, sessionID string, msgs []agent.ReactMessage) error {
	c.saved = append(c.saved, savedMsg{agentID: agentID, sessionID: sessionID, msgs: msgs})
	return nil
}
func (c *captureMessagesStore) LoadMessages(context.Context, string) ([]agent.ReactMessage, error) {
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
		"role_id": "code_assistant",
		"task":    "write code",
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

// TestDispatcher_HasPausedChild 验证父 Agent 是否有 Paused 子 domain 节点检测。
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
