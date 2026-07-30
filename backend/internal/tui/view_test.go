package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestAgentTreePrefix 验证 Agent 树前缀连接符的渲染结果。
func TestAgentTreePrefix(t *testing.T) {
	// 构造一个三层扁平节点列表，用于测试前缀生成。
	nodes := []agentTreeNode{
		{depth: 0, name: "Meta"},
		{depth: 1, name: "A"},
		{depth: 1, name: "B"},
		{depth: 2, name: "B1"},
		{depth: 2, name: "B2"},
		{depth: 1, name: "C"},
	}

	// 期望每个索引对应的前缀。
	cases := []struct {
		idx  int
		want string
	}{
		{0, ""},
		{1, "├─ "},
		{2, "├─ "},
		{3, "│  ├─ "},
		{4, "│  └─ "},
		{5, "└─ "},
	}

	// 逐个断言。
	for _, c := range cases {
		got := agentTreePrefix(nodes, c.idx)
		if got != c.want {
			t.Errorf("agentTreePrefix(nodes, %d) = %q, want %q", c.idx, got, c.want)
		}
	}
}

// TestBuildAgentCardRendersStatusAndGoal 验证 Agent 卡片能正确渲染名称、状态文本与目标。
func TestBuildAgentCardRendersStatusAndGoal(t *testing.T) {
	m := &Model{
		styles:         NewStyles(),
		taskBriefCache: NewTaskBriefCache(),
	}
	// 构造一个已完成、带目标的 UI 领域 Agent 节点。
	node := agentTreeNode{
		depth:     1,
		name:      "UI",
		roleType:  enums.RoleTypeDomain,
		status:    enums.RoleStatusDone,
		goal:      "设计后台前端页面",
		createdAt: time.Now(),
	}
	card := m.buildAgentCard(node, 40)
	if !strings.Contains(card, "UI") {
		t.Errorf("card missing agent name UI:\n%s", card)
	}
	if !strings.Contains(card, "Done") {
		t.Errorf("card missing status Done:\n%s", card)
	}
	if !strings.Contains(card, "设计后台前端页面") {
		t.Errorf("card missing goal:\n%s", card)
	}
}

// stubAgent 是一个最小化的 agent.Agent 实现，SummarizeTaskTitle 直接返回原标题，
// 使 taskBriefCache 在测试中回退到截断逻辑。
type stubAgent struct{}

// CreateSession 是 stubAgent 的空实现。
func (stubAgent) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) {
	return nil, nil
}

// ResumeSession 是 stubAgent 的空实现。
func (stubAgent) ResumeSession(ctx context.Context, sessionID string, req agent.ResumeRequest) (*agent.Session, error) {
	return nil, nil
}

// Send 是 stubAgent 的空实现。
func (stubAgent) Send(ctx context.Context, sessionID string, msg agent.Message) error { return nil }

// Stream 是 stubAgent 的空实现。
func (stubAgent) Stream(ctx context.Context, sessionID string) (<-chan agent.Event, error) {
	return nil, nil
}

// Query 是 stubAgent 的空实现。
func (stubAgent) Query(ctx context.Context, sessionID string, q agent.Query) (agent.Result, error) {
	return agent.Result{}, nil
}

// Control 是 stubAgent 的空实现。
func (stubAgent) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error {
	return nil
}

// List 是 stubAgent 的空实现。
func (stubAgent) List(ctx context.Context, filter agent.Filter) ([]*agent.Session, error) {
	return nil, nil
}

// Get 是 stubAgent 的空实现。
func (stubAgent) Get(ctx context.Context, sessionID string) (*agent.Session, error) { return nil, nil }

// ListAgents 是 stubAgent 的空实现。
func (stubAgent) ListAgents(ctx context.Context, sessionID string) ([]agent.AgentInstance, error) {
	return nil, nil
}

// Tree 是 stubAgent 的空实现。
func (stubAgent) Tree(ctx context.Context, sessionID string) ([]orchestrator.Node, error) {
	return nil, nil
}

// CancelAgent 是 stubAgent 的空实现。
func (stubAgent) CancelAgent(ctx context.Context, sessionID, instID string) error { return nil }

// Shutdown 是 stubAgent 的空实现。
func (stubAgent) Shutdown(ctx context.Context) error { return nil }

// SummarizeTaskTitle 是 stubAgent 的标题摘要实现，直接返回原标题。
func (stubAgent) SummarizeTaskTitle(ctx context.Context, title string) string { return title }

// TestSummarizeTaskTitleFallback 验证 summarizeTaskTitle 对长标题的截断兜底与短标题的保留行为。
func TestSummarizeTaskTitleFallback(t *testing.T) {
	m := &Model{
		styles:         NewStyles(),
		taskBriefCache: NewTaskBriefCache(),
		agent:          stubAgent{},
	}
	// 构造一个超长标题。
	long := strings.Repeat("设计并实现一个完整的后台管理系统前端页面包含商品管理和订单管理以及用户管理模块", 2)
	got := m.summarizeTaskTitle(long)
	if strings.Contains(got, long) {
		t.Errorf("summarizeTaskTitle should truncate long title, got %q", got)
	}
	if got == "" {
		t.Error("summarizeTaskTitle should not return empty")
	}
	// 短标题不处理
	short := "修复 bug"
	if m.summarizeTaskTitle(short) != short {
		t.Errorf("short title should remain unchanged")
	}
}
