package tui

// agent_tree_panel_test.go 验证 Agent 编排面板的命名、轮次过滤与紧凑卡片渲染。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestAgentNodeName 验证树节点展示名：domain 角色按 Domain 字段拼"XX领域"，
// 固定助手映射中文角色名，未知角色回退 Role 原文。
func TestAgentNodeName(t *testing.T) {
	cases := []struct {
		node orchestrator.Node
		want string
	}{
		{orchestrator.Node{Role: "domain", Domain: "游戏渲染"}, "游戏渲染领域"},
		{orchestrator.Node{Role: "domain", Domain: "炮塔实体"}, "炮塔实体领域"},
		// LLM 已带"领域"后缀时不重复拼接。
		{orchestrator.Node{Role: "domain", Domain: "游戏渲染领域"}, "游戏渲染领域"},
		// Domain 缺失时兜底。
		{orchestrator.Node{Role: "domain"}, "领域Agent"},
		{orchestrator.Node{Role: "code_assistant"}, "代码助手"},
		{orchestrator.Node{Role: "ui_assistant"}, "UI助手"},
		{orchestrator.Node{Role: "test_assistant"}, "测试助手"},
		{orchestrator.Node{Role: "doc_assistant"}, "文档助手"},
		{orchestrator.Node{Role: "code_reviewer"}, "代码审查助手"},
		{orchestrator.Node{Role: "prompt_reviewer"}, "提示词审查助手"},
		{orchestrator.Node{Role: "mystery_role"}, "mystery_role"},
	}
	for i, c := range cases {
		if got := agentNodeName(c.node); got != c.want {
			t.Errorf("case %d: agentNodeName(%+v) = %q, want %q", i, c.node, got, c.want)
		}
	}
}

// TestFilterPrevRoundNodes 验证上一轮终态节点被过滤、当前轮节点与未完结节点保留。
func TestFilterPrevRoundNodes(t *testing.T) {
	t1 := time.Now().Add(-time.Hour) // 第一轮
	t2 := time.Now()                 // 第二轮（当前轮）
	nodes := []orchestrator.Node{
		{ID: "s/domain-1", Status: orchestrator.StatusDone, Started: t1.Add(time.Minute)},       // 上一轮完成 -> 过滤
		{ID: "s/domain-2", Status: orchestrator.StatusFailed, Started: t1.Add(2 * time.Minute)}, // 上一轮失败 -> 过滤
		{ID: "s/domain-3", Status: orchestrator.StatusCancelled, Started: t1.Add(3 * time.Minute)},
		{ID: "s/domain-4", Status: orchestrator.StatusRunning, Started: t1.Add(4 * time.Minute)}, // 上一轮但仍在跑 -> 保留
		{ID: "s/domain-5", Status: orchestrator.StatusPaused, Started: t1.Add(5 * time.Minute)},  // 暂停待恢复 -> 保留
		{ID: "s/domain-6", Status: orchestrator.StatusDone, Started: t2.Add(time.Minute)},        // 当前轮完成 -> 保留
		{ID: "s/domain-7", Status: orchestrator.StatusRunning, Started: t2.Add(2 * time.Minute)}, // 当前轮运行 -> 保留
	}
	out := filterPrevRoundNodes(nodes, t2)
	var ids []string
	for _, n := range out {
		ids = append(ids, n.ID)
	}
	got := strings.Join(ids, ",")
	want := "s/domain-4,s/domain-5,s/domain-6,s/domain-7"
	if got != want {
		t.Errorf("过滤结果 = %v, want %v", got, want)
	}
	// roundStart 为零值时不过滤（恢复的历史会话无消息时间戳）。
	if len(filterPrevRoundNodes(nodes, time.Time{})) != len(nodes) {
		t.Errorf("roundStart 为零值时不应过滤任何节点")
	}
}

// TestAgentTreePanelRebuildFiltersPreviousRound 回归测试：第二轮运行时，
// 上一轮已完成的 Agent 不再占据编排面板位置，新派发的 Agent 可见且命名为领域名。
func TestAgentTreePanelRebuildFiltersPreviousRound(t *testing.T) {
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()
	mock := &mockAgentForPlan{
		sessionID: "session-1",
		treeNodes: []orchestrator.Node{
			// 第一轮已完成/失败的领域 Agent（应在第二轮被过滤）。
			{ID: "session-1/domain-1", ParentID: "session-1", Role: "domain", Domain: "游戏渲染", Task: "实现渲染循环", Status: orchestrator.StatusDone, Started: t1.Add(time.Minute)},
			{ID: "session-1/domain-2", ParentID: "session-1", Role: "domain", Domain: "怪物寻路", Task: "实现寻路", Status: orchestrator.StatusFailed, Started: t1.Add(2 * time.Minute)},
			// 第二轮新派发的领域 Agent 与其叶子助手（应可见）。
			{ID: "session-1/domain-8", ParentID: "session-1", Role: "domain", Domain: "炮塔实体", Task: "实现炮塔", Status: orchestrator.StatusRunning, Started: t2.Add(time.Minute)},
			{ID: "session-1/domain-8/code_assistant-9", ParentID: "session-1/domain-8", Role: "code_assistant", Task: "写 tower.js", Status: orchestrator.StatusDone, Started: t2.Add(2 * time.Minute)},
		},
	}
	s := &server.Session{
		ID:        "session-1",
		Goal:      "塔防游戏",
		Status:    enums.SessionStatusRunning,
		StartedAt: t1,
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "做一个塔防游戏", Timestamp: t1},
			{Role: enums.ChatRoleAssistant, Content: "已完成初版", Timestamp: t1.Add(30 * time.Minute)},
			{Role: enums.ChatRoleUser, Content: "给炮塔加升级功能", Timestamp: t2},
		},
	}

	var at AgentTreePanel
	at.rebuild(mock, s)

	var names []string
	for _, n := range at.nodes {
		names = append(names, n.name)
	}
	joined := strings.Join(names, ",")
	// 第一轮的完成/失败 Agent 不再占位。
	if strings.Contains(joined, "游戏渲染领域") || strings.Contains(joined, "怪物寻路领域") {
		t.Errorf("上一轮终态 Agent 应被过滤, got nodes: %v", joined)
	}
	// 第二轮新 Agent 可见，且按领域顾名思义命名。
	if !strings.Contains(joined, "炮塔实体领域") {
		t.Errorf("新一轮领域 Agent 应可见且命名为 炮塔实体领域, got nodes: %v", joined)
	}
	if !strings.Contains(joined, "代码助手") {
		t.Errorf("新一轮叶子助手应可见且命名为 代码助手, got nodes: %v", joined)
	}
	// MetaAgent 恒在首位。
	if len(at.nodes) == 0 || at.nodes[0].name != "MetaAgent" {
		t.Fatalf("首个节点应为 MetaAgent, got: %v", joined)
	}
}

// TestBoardSnapshotFiltersPreviousRound 验证计划面板（任务进度）同样只统计当前轮任务，
// 上一轮的已完成任务不再稀释进度。
func TestBoardSnapshotFiltersPreviousRound(t *testing.T) {
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()
	mock := &mockAgentForPlan{
		sessionID: "session-1",
		treeNodes: []orchestrator.Node{
			{ID: "session-1/domain-1", ParentID: "session-1", Role: "domain", Domain: "游戏渲染", Task: "实现渲染循环", Status: orchestrator.StatusDone, Started: t1.Add(time.Minute)},
			{ID: "session-1/domain-8", ParentID: "session-1", Role: "domain", Domain: "炮塔实体", Task: "实现炮塔", Status: orchestrator.StatusRunning, Started: t2.Add(time.Minute)},
		},
	}
	s := &server.Session{
		ID:        "session-1",
		StartedAt: t1,
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "做一个塔防游戏", Timestamp: t1},
			{Role: enums.ChatRoleUser, Content: "给炮塔加升级功能", Timestamp: t2},
		},
	}
	m := &Model{agent: mock}
	snap := m.boardSnapshot(s)
	if len(snap.Tasks) != 1 {
		t.Fatalf("计划面板应只剩当前轮 1 条任务, got %d: %+v", len(snap.Tasks), snap.Tasks)
	}
	if !strings.Contains(snap.Tasks[0].Title, "炮塔实体领域") {
		t.Errorf("任务标题应使用领域展示名, got %q", snap.Tasks[0].Title)
	}
	if snap.Tasks[0].Status != "in_progress" {
		t.Errorf("当前轮任务状态应为 in_progress, got %q", snap.Tasks[0].Status)
	}
}

// TestAgentsPanelSecondRoundRefresh 端到端回归：经 Model.rebuildAgents 全链路渲染编排面板，
// 第二轮时上一轮已完成的 Agent 卡片不占位，新派发的领域 Agent 以领域名+状态展示。
func TestAgentsPanelSecondRoundRefresh(t *testing.T) {
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()
	mock := &mockAgentForPlan{
		sessionID: "session-1",
		startedAt: t1,
		messages: []agent.Message{
			{Role: string(enums.ChatRoleUser), Content: "做一个塔防游戏", Timestamp: t1},
			{Role: string(enums.ChatRoleUser), Content: "给炮塔加升级功能", Timestamp: t2},
		},
		treeNodes: []orchestrator.Node{
			{ID: "session-1/domain-1", ParentID: "session-1", Role: "domain", Domain: "游戏渲染", Task: "实现渲染循环", Status: orchestrator.StatusDone, Started: t1.Add(time.Minute)},
			{ID: "session-1/domain-8", ParentID: "session-1", Role: "domain", Domain: "炮塔实体", Task: "实现炮塔", Status: orchestrator.StatusRunning, Started: t2.Add(time.Minute)},
		},
	}
	s := &server.Session{ID: "session-1", Goal: "塔防游戏", Status: enums.SessionStatusRunning, StartedAt: t1}
	m := &Model{
		styles:   NewStyles(),
		agent:    mock,
		sessions: []*server.Session{s},
	}
	m.rebuildAgents()
	panel := m.renderAgentsPanel(56, 26)
	// MetaAgent 只写名称。
	if !strings.Contains(panel, "MetaAgent") {
		t.Errorf("面板应包含 MetaAgent, got:\n%s", panel)
	}
	// 新一轮领域 Agent 可见且顾名思义命名。
	if !strings.Contains(panel, "炮塔实体领域") {
		t.Errorf("面板应包含 炮塔实体领域, got:\n%s", panel)
	}
	// 上一轮已完成 Agent 不再占住位置。
	if strings.Contains(panel, "游戏渲染领域") {
		t.Errorf("上一轮完成 Agent 不应再占位, got:\n%s", panel)
	}
}
