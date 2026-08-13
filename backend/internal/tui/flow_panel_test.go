package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// flowTestNodes 构造一个含 MetaAgent + 领域 + 固定助手的节点集。
func flowTestNodes() []agentTreeNode {
	return []agentTreeNode{
		{instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive},
		{instID: "s/domain-1", name: "游戏渲染领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		{instID: "s/domain-2", name: "UI领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone},
		{instID: "s/code_assistant-1", name: "代码助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusDone},
	}
}

// TestCollectFlowCards_DomainOnly 验证面板只收领域 Agent，过滤 Meta 与固定助手。
func TestCollectFlowCards_DomainOnly(t *testing.T) {
	cards := collectFlowCards(flowTestNodes(), nil)
	if len(cards) != 2 {
		t.Fatalf("应只有 2 张领域卡片，got %d", len(cards))
	}
	if cards[0].node.instID != "s/domain-1" || cards[1].node.instID != "s/domain-2" {
		t.Fatalf("卡片应保持节点顺序，got %q, %q", cards[0].node.instID, cards[1].node.instID)
	}
}

// TestFlowEventsFor_AgentIDMatch 验证按 DetailJSON agent_id 匹配事件，且只取尾部 3 条。
func TestFlowEventsFor_AgentIDMatch(t *testing.T) {
	node := flowTestNodes()[1]
	mk := func(ts time.Time, evType, kind, tool, path, msg string) server.SessionEvent {
		return server.SessionEvent{
			Type:       evType,
			Kind:       kind,
			Tool:       tool,
			ToolPath:   path,
			Message:    msg,
			Success:    true,
			Timestamp:  ts,
			DetailJSON: `{"agent_id":"s/domain-1"}`,
		}
	}
	base := time.Now()
	events := []server.SessionEvent{
		mk(base, "tool_exec", "", "ReadFile", "a.js", ""),
		mk(base.Add(time.Second), "tool_exec", "", "ReadFile", "b.js", ""),
		mk(base.Add(2*time.Second), "think", "think", "", "", "分析渲染管线"),
		mk(base.Add(3*time.Second), "tool_exec", "", "WriteFile", "game.js", ""),
		mk(base.Add(4*time.Second), "progress", "llm_result", "s/domain-1", "", "渲染完成"),
		// 其他实例的事件不应混入。
		{Type: "tool_exec", Tool: "ReadFile", ToolPath: "other.js", Timestamp: base, DetailJSON: `{"agent_id":"s/domain-2"}`},
	}

	got := flowEventsFor(node, events)
	if len(got) != maxFlowEvents {
		t.Fatalf("应只取尾部 %d 条，got %d: %+v", maxFlowEvents, len(got), got)
	}
	// 尾部 3 条 = b.js 之后的 think / WriteFile / llm_result。
	if got[0].text != "分析渲染管线" || got[0].icon != "💭" {
		t.Fatalf("第 1 条应为 think，got %+v", got[0])
	}
	if got[1].text != "WriteFile: game.js" {
		t.Fatalf("第 2 条应为 WriteFile，got %+v", got[1])
	}
	if got[2].text != "渲染完成" || got[2].icon != "◈" {
		t.Fatalf("第 3 条应为 llm_result，got %+v", got[2])
	}
}

// TestFlowEventsFor_FallbackNameMatch 验证无 DetailJSON 时回退展示名匹配。
func TestFlowEventsFor_FallbackNameMatch(t *testing.T) {
	node := flowTestNodes()[3] // 代码助手（固定助手走回退路径）
	events := []server.SessionEvent{
		{Type: "tool_exec", Tool: "ReadFile", ToolPath: "x.js", Agent: "代码助手", Success: true, Timestamp: time.Now()},
		{Type: "tool_exec", Tool: "ReadFile", ToolPath: "y.js", Agent: "其他Agent", Success: true, Timestamp: time.Now()},
	}
	got := flowEventsFor(node, events)
	if len(got) != 1 || got[0].text != "ReadFile: x.js" {
		t.Fatalf("回退匹配应只收展示名一致的事件，got %+v", got)
	}
}

// TestFormatFlowEvent_SubAgentDone 验证 sub_agent_done 生命周期事件映射。
func TestFormatFlowEvent_SubAgentDone(t *testing.T) {
	ev := server.SessionEvent{Type: "message", Kind: "sub_agent_done", Message: "s/domain-1"}
	fe, ok := formatFlowEvent("s/domain-1", ev)
	if !ok || fe.icon != "✓" || fe.text != "执行完成" {
		t.Fatalf("sub_agent_done 应映射为完成事件，got %+v ok=%v", fe, ok)
	}
	// 其他实例的 done 事件不应归属本卡片。
	if _, ok := formatFlowEvent("s/domain-2", ev); ok {
		t.Fatal("其他实例的 sub_agent_done 不应映射")
	}
}

// TestFormatFlowEvent_Error 验证失败事件映射为 ✗。
func TestFormatFlowEvent_Error(t *testing.T) {
	ev := server.SessionEvent{Type: "tool_exec", Tool: "WriteFile", Success: false}
	fe, ok := formatFlowEvent("s/domain-1", ev)
	if !ok || fe.icon != "✗" {
		t.Fatalf("失败工具事件应映射为 ✗，got %+v", fe)
	}
}

// TestFlowPanelCols 验证每行卡片列数计算。
func TestFlowPanelCols(t *testing.T) {
	if flowPanelCols(100) != 2 { // 100/34 = 2
		t.Fatalf("宽 100 应 2 列，got %d", flowPanelCols(100))
	}
	if flowPanelCols(20) != 1 {
		t.Fatalf("窄终端至少 1 列，got %d", flowPanelCols(20))
	}
}

// TestSubAgentFlowPanelHeight 验证面板高度计算：无领域节点为 0，有则为 1+6×行数。
func TestSubAgentFlowPanelHeight(t *testing.T) {
	sess := &server.Session{}
	m := &Model{
		sessions:       []*server.Session{sess},
		sessionsCursor: 0,
		width:          100,
	}
	if h := m.subAgentFlowPanelHeight(); h != 0 {
		t.Fatalf("无领域节点高度应为 0，got %d", h)
	}
	m.agentTreePanel.nodes = []agentTreeNode{
		{instID: "s/domain-1", name: "A领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		{instID: "s/domain-2", name: "B领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
	}
	if h := m.subAgentFlowPanelHeight(); h != 7 { // 2 卡 1 行
		t.Fatalf("2 领域单行高度应为 7，got %d", h)
	}
	m.width = 30 // 单列：2 卡 2 行
	if h := m.subAgentFlowPanelHeight(); h != 13 {
		t.Fatalf("2 领域两行高度应为 13，got %d", h)
	}
}

// TestRenderSubAgentFlowPanel 渲染冒烟：含标题、领域名与事件行，无领域时返回空串。
func TestRenderSubAgentFlowPanel(t *testing.T) {
	sess := &server.Session{
		Events: []server.SessionEvent{
			{Type: "tool_exec", Tool: "ReadFile", ToolPath: "a.js", Success: true, Timestamp: time.Now(), DetailJSON: `{"agent_id":"s/domain-1"}`},
		},
	}
	m := &Model{
		styles:         NewStyles(),
		sessions:       []*server.Session{sess},
		sessionsCursor: 0,
		width:          100,
		agentTreePanel: AgentTreePanel{nodes: []agentTreeNode{
			{instID: "s/domain-1", name: "游戏渲染领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
		}},
	}
	out := m.renderSubAgentFlowPanel(100)
	if out == "" {
		t.Fatal("有领域节点时面板不应为空")
	}
	if !strings.Contains(out, "领域 Agent 进度") || !strings.Contains(out, "游戏渲染领域") {
		t.Fatalf("面板应含标题与领域名，got:\n%s", out)
	}
	if !strings.Contains(out, "ReadFile: a.js") {
		t.Fatalf("面板应含事件行，got:\n%s", out)
	}
	if s := m.renderSubAgentFlowPanel(100); strings.Count(s, "╭") != 1 {
		t.Fatalf("单卡片应只有 1 个卡片边框，got:\n%s", s)
	}
	m.agentTreePanel.nodes = nil
	if out := m.renderSubAgentFlowPanel(100); out != "" {
		t.Fatalf("无领域节点面板应为空串，got %q", out)
	}
}
