package tui

import (
	"context"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/userprofile"
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

// TestBuildAgentCardRendersNameAndStatus 验证子 Agent 卡片简洁渲染：仅名称与运行状态，
// 不再展示目标/时间等额外信息（完整详情在 [A] 编排弹窗中查看）。
func TestBuildAgentCardRendersNameAndStatus(t *testing.T) {
	m := &Model{
		styles:         NewStyles(),
		taskBriefCache: NewTaskBriefCache(),
	}
	// 构造一个已完成、带目标的领域 Agent 节点。
	node := agentTreeNode{
		depth:     1,
		name:      "游戏渲染领域",
		roleType:  enums.RoleTypeDomain,
		status:    enums.RoleStatusDone,
		goal:      "设计后台前端页面",
		createdAt: time.Now(),
	}
	card := m.buildAgentCard(node, 40, "")
	if !strings.Contains(card, "游戏渲染领域") {
		t.Errorf("card missing agent name:\n%s", card)
	}
	if !strings.Contains(card, "Done") {
		t.Errorf("card missing status Done:\n%s", card)
	}
	// 简洁展示：已完成卡片不展示目标详情（完整详情在 [A] 编排弹窗中查看）。
	if strings.Contains(card, "设计后台前端页面") {
		t.Errorf("card 不应展示目标详情:\n%s", card)
	}
}

// TestBuildAgentCardWaitingAnnotation 验证运行中分支卡片带"等待 XX 完成"标注（TODO #48 子项 1）。
func TestBuildAgentCardWaitingAnnotation(t *testing.T) {
	m := &Model{styles: NewStyles()}
	node := agentTreeNode{
		depth:    1,
		name:     "游戏主控领域",
		roleType: enums.RoleTypeDomain,
		status:   enums.RoleStatusActive,
	}
	card := m.buildAgentCard(node, 40, "⏳ 等待 代码助手 完成")
	if !strings.Contains(card, "⏳ 等待 代码助手 完成") {
		t.Errorf("card missing waiting annotation:\n%s", card)
	}
}

// TestBuildAgentCardActiveShowsGoal 验证运行中无子节点的卡片展示当前任务摘要（进度感）。
func TestBuildAgentCardActiveShowsGoal(t *testing.T) {
	m := &Model{styles: NewStyles()}
	node := agentTreeNode{
		depth:    1,
		name:     "游戏主控领域",
		roleType: enums.RoleTypeDomain,
		status:   enums.RoleStatusActive,
		goal:     "实现游戏主循环与 6 步改造计划",
	}
	card := m.buildAgentCard(node, 40, "")
	if !strings.Contains(card, "实现游戏主循环") {
		t.Errorf("active card missing goal line:\n%s", card)
	}
}

// TestBuildMetaCardRendersNameOnly 验证 MetaAgent 卡片只写名称，不带任何额外信息。
func TestBuildMetaCardRendersNameOnly(t *testing.T) {
	m := &Model{styles: NewStyles()}
	node := agentTreeNode{
		depth:    0,
		name:     "MetaAgent",
		roleType: enums.RoleTypeMeta,
		role:     "Orchestrator",
		status:   enums.RoleStatusActive,
		goal:     "做一个塔防游戏",
	}
	card := m.buildMetaCard(node, "")
	if !strings.Contains(card, "MetaAgent") {
		t.Errorf("meta card missing name:\n%s", card)
	}
	if strings.Contains(card, "Orchestrator") || strings.Contains(card, "塔防") || strings.Contains(card, "Running") {
		t.Errorf("meta card 不应展示角色/目标/状态文本:\n%s", card)
	}
}

// TestBuildMetaCardWaitingAnnotation 验证 MetaAgent 等待子 Agent 时卡片带等待标注（TODO #48 子项 1）。
func TestBuildMetaCardWaitingAnnotation(t *testing.T) {
	m := &Model{styles: NewStyles()}
	node := agentTreeNode{
		depth:    0,
		name:     "MetaAgent",
		roleType: enums.RoleTypeMeta,
		status:   enums.RoleStatusActive,
	}
	card := m.buildMetaCard(node, "⏳ 等待 游戏主控领域 完成")
	if !strings.Contains(card, "⏳ 等待 游戏主控领域 完成") {
		t.Errorf("meta card missing waiting annotation:\n%s", card)
	}
}

// stubAgent 是一个最小化的 agent.Agent 实现，SummarizeTaskTitle 直接返回原标题，
// 使 taskBriefCache 在测试中回退到截断逻辑。
type stubAgent struct{}

// Board 返回看板快照，测试实现返回 nil（回退树合成）。
func (stubAgent) Board(ctx context.Context, sessionID string) (*board.Snapshot, error) {
	return nil, nil
}

// Profile 返回用户画像，测试实现返回空。
func (stubAgent) Profile(ctx context.Context) (*userprofile.Profile, error) {
	return &userprofile.Profile{}, nil
}

// SaveProfile 覆盖画像，测试实现为空操作。
func (stubAgent) SaveProfile(ctx context.Context, content string) error { return nil }

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
