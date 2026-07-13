package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

func TestAgentTreePrefix(t *testing.T) {
	nodes := []agentTreeNode{
		{depth: 0, name: "Meta"},
		{depth: 1, name: "A"},
		{depth: 1, name: "B"},
		{depth: 2, name: "B1"},
		{depth: 2, name: "B2"},
		{depth: 1, name: "C"},
	}

	cases := []struct {
		idx    int
		want   string
	}{
		{0, ""},
		{1, "├─ "},
		{2, "├─ "},
		{3, "│  ├─ "},
		{4, "│  └─ "},
		{5, "└─ "},
	}

	for _, c := range cases {
		got := agentTreePrefix(nodes, c.idx)
		if got != c.want {
			t.Errorf("agentTreePrefix(nodes, %d) = %q, want %q", c.idx, got, c.want)
		}
	}
}

func TestAgentCardLineRendersGoalAndBadge(t *testing.T) {
	m := &Model{
		styles:         NewStyles(),
		taskBriefCache: NewTaskBriefCache(),
	}
	nodes := []agentTreeNode{
		{depth: 0, name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive, createdAt: time.Now()},
		{depth: 1, name: "UI", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone, goal: "设计后台前端页面", createdAt: time.Now()},
	}
	lines := m.agentCardLine(nodes[1], 1, 80)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines for agent with goal, got %d: %v", len(lines), lines)
	}
	first := lines[0]
	if !strings.Contains(first, "UI") {
		t.Errorf("first line missing agent name UI: %s", first)
	}
	if !strings.Contains(first, "完成") {
		t.Errorf("first line missing status badge 完成: %s", first)
	}
	second := lines[1]
	if !strings.Contains(second, "设计后台前端页面") {
		t.Errorf("second line missing goal: %s", second)
	}
}

// stubAgent returns titles unchanged so the brief cache falls back to truncation.
type stubAgent struct{}

func (stubAgent) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) { return nil, nil }
func (stubAgent) ResumeSession(ctx context.Context, sessionID string, req agent.ResumeRequest) (*agent.Session, error) {
	return nil, nil
}
func (stubAgent) Send(ctx context.Context, sessionID string, msg agent.Message) error { return nil }
func (stubAgent) Stream(ctx context.Context, sessionID string) (<-chan agent.Event, error) { return nil, nil }
func (stubAgent) Query(ctx context.Context, sessionID string, q agent.Query) (agent.Result, error) { return agent.Result{}, nil }
func (stubAgent) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error { return nil }
func (stubAgent) List(ctx context.Context, filter agent.Filter) ([]*agent.Session, error) { return nil, nil }
func (stubAgent) Get(ctx context.Context, sessionID string) (*agent.Session, error) { return nil, nil }
func (stubAgent) ListAgents(ctx context.Context, sessionID string) ([]agent.AgentInstance, error) { return nil, nil }
func (stubAgent) Shutdown(ctx context.Context) error { return nil }
func (stubAgent) SummarizeTaskTitle(ctx context.Context, title string) string { return title }

func TestSummarizeTaskTitleFallback(t *testing.T) {
	m := &Model{
		styles:         NewStyles(),
		taskBriefCache: NewTaskBriefCache(),
		agent:          stubAgent{},
	}
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
