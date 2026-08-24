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

// TestGroupAgentTree_BranchesAndChildren 验证真树形分组（TODO #48 子项 2）：
// meta 下按 depth=1 节点分叉，子节点沿 ParentID 链挂到所属分支。
func TestGroupAgentTree_BranchesAndChildren(t *testing.T) {
	nodes := []agentTreeNode{
		{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
		{depth: 1, instID: "s/domain-2", parentID: "s", name: "游戏主控领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		{depth: 1, instID: "s/domain-3", parentID: "s", name: "怪物领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone},
		{depth: 2, instID: "s/domain-2/code_assistant-1", parentID: "s/domain-2", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusActive},
		{depth: 2, instID: "s/domain-2/ui_assistant-1", parentID: "s/domain-2", name: "UI助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusActive},
		{depth: 2, instID: "s/domain-3/code_assistant-2", parentID: "s/domain-3", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusActive},
	}
	meta, branches, loose := groupAgentTree(nodes)
	if meta == nil || meta.instID != "MetaAgent" {
		t.Fatalf("meta = %+v, want MetaAgent", meta)
	}
	if len(branches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(branches))
	}
	if branches[0].node.name != "游戏主控领域" || branches[1].node.name != "怪物领域" {
		t.Fatalf("branch order wrong: %+v", branches)
	}
	// domain-2 的助手挂在 domain-2 分支下，domain-3 的助手挂在 domain-3 分支下。
	if len(branches[0].children) != 2 {
		t.Fatalf("domain-2 children = %d, want 2", len(branches[0].children))
	}
	if branches[0].children[0].instID != "s/domain-2/code_assistant-1" ||
		branches[0].children[1].instID != "s/domain-2/ui_assistant-1" {
		t.Errorf("domain-2 children mismatch: %+v", branches[0].children)
	}
	if len(branches[1].children) != 1 || branches[1].children[0].instID != "s/domain-3/code_assistant-2" {
		t.Errorf("domain-3 children mismatch: %+v", branches[1].children)
	}
	if len(loose) != 0 {
		t.Errorf("loose nodes = %+v, want none", loose)
	}
}

// TestGroupAgentTree_ClutterFiltered 验证拥挤过滤沿用旧口径：
// depth>=2 且无目标且已终结/空闲的节点不参与渲染。
func TestGroupAgentTree_ClutterFiltered(t *testing.T) {
	nodes := []agentTreeNode{
		{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta},
		{depth: 1, instID: "s/domain-2", parentID: "s", name: "游戏主控领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		// 无目标、已完成的助手：应被过滤。
		{depth: 2, instID: "s/domain-2/code_assistant-1", parentID: "s/domain-2", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusDone},
		// 有目标、已完成的助手：应保留。
		{depth: 2, instID: "s/domain-2/code_assistant-2", parentID: "s/domain-2", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusDone, goal: "写游戏循环"},
	}
	_, branches, _ := groupAgentTree(nodes)
	if len(branches) != 1 || len(branches[0].children) != 1 {
		t.Fatalf("want 1 branch with 1 child, got %+v", branches)
	}
	if branches[0].children[0].instID != "s/domain-2/code_assistant-2" {
		t.Errorf("kept wrong child: %+v", branches[0].children)
	}
}

// TestWaitingChildNames 验证等待标注文案（TODO #48 子项 1）：1/2/多 三种形态。
func TestWaitingChildNames(t *testing.T) {
	active := agentTreeNode{name: "代码助手", status: enums.RoleStatusActive}
	waiting := agentTreeNode{name: "UI助手", status: enums.RoleStatusWaiting}
	done := agentTreeNode{name: "测试助手", status: enums.RoleStatusDone}
	if got := waitingChildNames(nil); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	if got := waitingChildNames([]agentTreeNode{done}); got != "" {
		t.Errorf("all done = %q, want empty", got)
	}
	if got := waitingChildNames([]agentTreeNode{active}); got != "⏳ 等待 代码助手 完成" {
		t.Errorf("one = %q", got)
	}
	if got := waitingChildNames([]agentTreeNode{active, waiting}); got != "⏳ 等待 代码助手、UI助手 完成" {
		t.Errorf("two = %q", got)
	}
	if got := waitingChildNames([]agentTreeNode{active, waiting, active}); got != "⏳ 等待 3 个子 Agent 完成" {
		t.Errorf("many = %q", got)
	}
}

// TestRenderAgentsPanel_TreeLayout 验证真树形渲染（TODO #48 子项 2 验收 (c)）：
// 1/2/3 领域场景分叉形态正确（分支卡片都在、助手挂在所属领域下、无悬空连接线），
// 且 meta 等待标注可见（验收 (b)）。
func TestRenderAgentsPanel_TreeLayout(t *testing.T) {
	mkNode := func(inst, parent, name string, depth int, status enums.RoleStatus) agentTreeNode {
		return agentTreeNode{
			depth:    depth,
			instID:   inst,
			parentID: parent,
			name:     name,
			roleType: enums.RoleTypeDomain,
			status:   status,
		}
	}
	cases := []struct {
		name     string
		nodes    []agentTreeNode
		wantAll  []string // 必须全部出现
		wantNone []string // 必须全部不出现
	}{
		{
			name: "1 领域 1 助手",
			nodes: []agentTreeNode{
				{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
				mkNode("s/d1", "s", "游戏主控领域", 1, enums.RoleStatusActive),
				{depth: 2, instID: "s/d1/c1", parentID: "s/d1", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusActive},
			},
			wantAll:  []string{"MetaAgent", "游戏主控领域", "代码助手", "⏳ 等待 代码助手 完成"},
			wantNone: []string{"┌", "┐"},
		},
		{
			name: "2 领域并行",
			nodes: []agentTreeNode{
				{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
				mkNode("s/d1", "s", "游戏主控领域", 1, enums.RoleStatusActive),
				mkNode("s/d2", "s", "怪物领域", 1, enums.RoleStatusActive),
			},
			wantAll: []string{"MetaAgent", "游戏主控领域", "怪物领域", "⏳ 等待 游戏主控领域、怪物领域 完成"},
		},
		{
			name: "3 领域并行",
			nodes: []agentTreeNode{
				{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
				mkNode("s/d1", "s", "游戏主控领域", 1, enums.RoleStatusActive),
				mkNode("s/d2", "s", "怪物领域", 1, enums.RoleStatusDone),
				mkNode("s/d3", "s", "路径实体领域", 1, enums.RoleStatusDone),
			},
			wantAll: []string{"MetaAgent", "游戏主控领域", "怪物领域", "路径实体领域"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Model{styles: NewStyles()}
			m.agentTreePanel.nodes = c.nodes
			panel := m.renderAgentsPanel(80, 26)
			for _, want := range c.wantAll {
				if !strings.Contains(panel, want) {
					t.Errorf("panel missing %q:\n%s", want, panel)
				}
			}
			for _, no := range c.wantNone {
				if strings.Contains(panel, no) {
					t.Errorf("panel should not contain %q:\n%s", no, panel)
				}
			}
		})
	}
}

// TestBuildAgentCardWrapsName 验证窄列下卡片名称换行而非截断（Y 轴换 X 轴空间）：
// 名称完整保留、无省略号，且折为多行。
func TestBuildAgentCardWrapsName(t *testing.T) {
	m := &Model{styles: NewStyles()}
	node := agentTreeNode{
		depth:    1,
		name:     "游戏流程进度领域",
		roleType: enums.RoleTypeDomain,
		status:   enums.RoleStatusActive,
		goal:     "实现关卡进度持久化",
	}
	// cardW=14 → 文本区 10 列，名称（16 列宽）必须折成两行。
	card := stripANSI(m.buildAgentCard(node, 14, ""))
	if strings.Contains(card, "…") {
		t.Errorf("卡片名称不应截断为省略号:\n%s", card)
	}
	// 去掉换行、对齐空格与边框后，名称与任务摘要的所有字符应完整保留（折行点可在任意字符边界）。
	flat := strings.NewReplacer("\n", "", " ", "", "│", "").Replace(card)
	if !strings.Contains(flat, "游戏流程进度领域") {
		t.Errorf("卡片名称换行后应完整保留:\n%s", card)
	}
	if !strings.Contains(flat, "实现关卡进度持久化") {
		t.Errorf("卡片任务摘要应换行保留:\n%s", card)
	}
}

// TestRenderAgentsPanel_NarrowPanelThreeBranchesOneRow 验证窄面板（旧逻辑 innerW<66 只排 2 列）
// 下 3 个分支经卡片文字换行后仍排在同一行。
func TestRenderAgentsPanel_NarrowPanelThreeBranchesOneRow(t *testing.T) {
	mkNode := func(inst, name string) agentTreeNode {
		return agentTreeNode{
			depth:    1,
			instID:   inst,
			parentID: "s",
			name:     name,
			roleType: enums.RoleTypeDomain,
			status:   enums.RoleStatusActive,
		}
	}
	m := &Model{styles: NewStyles()}
	m.agentTreePanel.nodes = []agentTreeNode{
		{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
		mkNode("s/d1", "主控领域"),
		mkNode("s/d2", "怪物领域"),
		mkNode("s/d3", "路径领域"),
	}
	// w=52 → innerW=48：3 列各 14 宽（≥ minBranchColW），一行放下。
	panel := stripANSI(m.renderAgentsPanel(52, 30))
	sameRow := false
	for _, l := range strings.Split(panel, "\n") {
		if strings.Contains(l, "主控领域") && strings.Contains(l, "怪物领域") && strings.Contains(l, "路径领域") {
			sameRow = true
			break
		}
	}
	if !sameRow {
		t.Errorf("窄面板下 3 个分支应排在同一行:\n%s", panel)
	}
}

// TestRenderAgentsPanel_ChildCards 验证三级助手子分支的卡片式排列（与领域同一风格）：
// 子 Agent 以圆角边框卡片渲染在所属领域列内，列首有连接竖线，不再是 ├─ 纯文本行；
// 高度不足时按 agentScroll 滚动开窗（滚轮翻看），窗口首/末行提示未显示内容，不再省略截断。
func TestRenderAgentsPanel_ChildCards(t *testing.T) {
	mkChild := func(inst, parent, name string, status enums.RoleStatus) agentTreeNode {
		return agentTreeNode{
			depth:    2,
			instID:   inst,
			parentID: parent,
			name:     name,
			roleType: enums.RoleTypeFixed,
			status:   status,
		}
	}

	t.Run("两个助手各成卡片", func(t *testing.T) {
		m := &Model{styles: NewStyles()}
		m.agentTreePanel.nodes = []agentTreeNode{
			{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
			{depth: 1, instID: "s/d1", parentID: "s", name: "炮塔美术领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
			mkChild("s/d1/c1", "s/d1", "代码助手", enums.RoleStatusActive),
			mkChild("s/d1/c2", "s/d1", "UI助手", enums.RoleStatusWaiting),
		}
		panel := stripANSI(m.renderAgentsPanel(80, 40))
		for _, want := range []string{"代码助手", "UI助手"} {
			if !strings.Contains(panel, want) {
				t.Errorf("panel missing %q:\n%s", want, panel)
			}
		}
		// 圆角边框数 = 面板外框 1 + meta 1 + 领域 1 + 助手 2 = 5 个左上角。
		if got := strings.Count(panel, "╭"); got != 5 {
			t.Errorf("圆角卡片应有 5 张（含面板外框），实际 %d:\n%s", got, panel)
		}
		if strings.Contains(panel, "├─") {
			t.Errorf("子节点不应再用 ├─ 纯文本行:\n%s", panel)
		}
	})

	t.Run("高度不足滚动开窗并提示", func(t *testing.T) {
		m := &Model{styles: NewStyles()}
		nodes := []agentTreeNode{
			{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
			{depth: 1, instID: "s/d1", parentID: "s", name: "炮塔美术领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		}
		for i := 0; i < 4; i++ {
			nodes = append(nodes, mkChild("s/d1/c"+string(rune('1'+i)), "s/d1", "代码助手", enums.RoleStatusActive))
		}
		m.agentTreePanel.nodes = nodes
		// 高度放不下头部 + 4 个助手：首窗末行提示下方还有内容（滚轮翻看，不再省略截断），
		// 旧省略提示不再出现，底部状态图例固定可见。
		panel := stripANSI(m.renderAgentsPanel(80, 18))
		if !strings.Contains(panel, "↓ 下方还有") {
			t.Errorf("高度不足时首窗应提示下方还有内容:\n%s", panel)
		}
		if strings.Contains(panel, "… 还有") {
			t.Errorf("旧省略提示不应再出现（已改为滚动开窗）:\n%s", panel)
		}
		if !strings.Contains(panel, "◐ Waiting") {
			t.Errorf("底部状态图例应固定可见:\n%s", panel)
		}
		// 滚到底部（越界钳制到末尾窗口）：首行提示上方内容，下方提示消失。
		m.agentScroll = 99
		panel = stripANSI(m.renderAgentsPanel(80, 18))
		if !strings.Contains(panel, "↑ 上方还有") {
			t.Errorf("滚到底部时首行应提示上方还有内容:\n%s", panel)
		}
		if strings.Contains(panel, "↓ 下方还有") {
			t.Errorf("已到底部不应再有下方提示:\n%s", panel)
		}
	})
}
