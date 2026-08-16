package tui

// layout_overflow_test.go 复现并回归"右栏排版全乱、输入框被挤出屏幕"：
// 全帧每个物理行显示宽度不得超过终端宽度（超宽行在终端折行会把后续内容整体顶下去），
// 且总行数不得超过终端高度（超出部分在 alt-screen 下裁掉底部输入栏）。

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// buildCrowdedModel 构造截图场景：3 个领域分支（1 个运行中带等待标注与长任务）、
// 会话事件若干、右栏可见。经 NewModel + WindowSizeMsg 走真实尺寸分发路径。
func buildCrowdedModel(w, h int) *Model {
	m := NewModel(nil, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	mm := modelPtr(nm)
	mm.rightPanelForced = 1
	nodes := []agentTreeNode{
		{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusActive, goal: "重构塔防游戏入口流程"},
		{depth: 1, instID: "d1", parentID: "s", name: "页面流程UI领域", domain: "页面流程UI", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone},
		{depth: 1, instID: "d2", parentID: "s", name: "游戏流程进度领域", domain: "游戏流程进度", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone},
		{depth: 1, instID: "d3", parentID: "s", name: "整体验收领域", domain: "整体验收", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive,
			goal: "整品验收：语法检查与契约清单逐项核对"},
		{depth: 2, instID: "d3/c1", parentID: "d3", name: "代码审查助手", roleType: enums.RoleTypeFixed, status: enums.RoleStatusActive},
	}
	events := []server.SessionEvent{
		{Kind: "tool_call", Tool: "ReadFile", ToolPath: `D:\WebData\github\BlockMemoryAgent\workspace\js\game.js`, DetailJSON: `{"agent_id":"d1"}`},
		{Kind: "tool_call", Tool: "RunCommand", ToolPath: `D:\WebData\github\BlockMemoryAgent\workspace`, DetailJSON: `{"agent_id":"d2"}`},
		{Kind: "tool_call", Tool: "RunCommand", ToolPath: "$conn = Get-Content ...", DetailJSON: `{"agent_id":"d3"}`},
	}
	mm.sessions = []*server.Session{
		{ID: "s", Status: enums.SessionStatusRunning, Goal: "重构塔防游戏入口流程", StartedAt: time.Now(), Events: events},
	}
	mm.sessionsCursor = 0
	mm.agentTreePanel = AgentTreePanel{nodes: nodes}
	return mm
}

// TestViewNoPhysicalLineOverflow 拥挤场景（3 分支+长等待标注）下，
// View 的每个物理行宽度不得超过终端宽度、总行数不得超过终端高度。
func TestViewNoPhysicalLineOverflow(t *testing.T) {
	// 与 cmd/tui 保持一致：东亚字符按窄字符计算宽度。
	runewidth.DefaultCondition.EastAsianWidth = false
	for _, size := range [][2]int{{150, 45}, {120, 40}, {200, 55}, {100, 30}} {
		w, h := size[0], size[1]
		m := buildCrowdedModel(w, h)
		view := m.View()
		lines := strings.Split(view, "\n")
		if len(lines) > h {
			t.Errorf("w=%d h=%d: View 共 %d 行超出终端高度:\n%s", w, h, len(lines), view)
		}
		for i, l := range lines {
			if lw := runewidth.StringWidth(stripANSI(l)); lw > w {
				t.Errorf("w=%d h=%d: 第 %d 行宽 %d 超终端宽度 %d: %q", w, h, i, lw, w, stripANSI(l))
			}
		}
	}
}
