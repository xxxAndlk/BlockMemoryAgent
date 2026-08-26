package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestCollectItemsCapsHistory 验证主对话区只加载最近 maxChatItems 条记录：
// 更早的记录不进对话区（避免长会话滚动陷入历史），顶部以一条省略提示代替，
// 并指引用户用 Ctrl+L 查看完整记录。
func TestCollectItemsCapsHistory(t *testing.T) {
	now := time.Now()
	s := &server.Session{Status: enums.SessionStatusCompleted}
	total := maxChatItems + 30
	for i := 0; i < total; i++ {
		s.Messages = append(s.Messages, types.ChatMessage{
			Role:      enums.ChatRoleUser,
			Content:   fmt.Sprintf("question-%03d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		})
	}

	cp := NewChatPanel()
	items := cp.collectItems(s, nil)

	// 超过上限：保留最近 maxChatItems 条 + 顶部 1 条省略提示。
	if len(items) != maxChatItems+1 {
		t.Fatalf("超过上限时应为 %d 条（含省略提示），got %d", maxChatItems+1, len(items))
	}
	if !items[0].isEvent || !strings.Contains(items[0].title, "已省略 30 条") || !strings.Contains(items[0].title, "Ctrl+L") {
		t.Fatalf("首条应为省略提示（含省略条数与 Ctrl+L 指引），got %q", items[0].title)
	}
	// 保留段从 question-030 开始，到最新一条结束，最新内容始终可见。
	if !strings.Contains(items[1].title, "question-030") {
		t.Fatalf("保留段应从 question-030 开始，got %q", items[1].title)
	}
	if !strings.Contains(items[len(items)-1].title, fmt.Sprintf("question-%03d", total-1)) {
		t.Fatalf("末条应为最新消息，got %q", items[len(items)-1].title)
	}
}

// TestCollectItemsBelowCapKeepsAll 验证未超上限时全部记录原样保留，不出现省略提示。
func TestCollectItemsBelowCapKeepsAll(t *testing.T) {
	now := time.Now()
	s := &server.Session{Status: enums.SessionStatusCompleted}
	for i := 0; i < 5; i++ {
		s.Messages = append(s.Messages, types.ChatMessage{
			Role:      enums.ChatRoleUser,
			Content:   fmt.Sprintf("q%d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		})
	}

	cp := NewChatPanel()
	items := cp.collectItems(s, nil)

	if len(items) != 5 {
		t.Fatalf("未超上限时应全部保留，got %d", len(items))
	}
	if strings.Contains(items[0].title, "已省略") {
		t.Fatal("未超上限时不应出现省略提示")
	}
}

// TestGoalBarPinnedOnTop 验证对话区顶部固定目标栏（TODO #48 子项 3）：
// 主任务目标常驻展示在对话区顶部，等待/瞬时状态条目不会顶替它。
func TestGoalBarPinnedOnTop(t *testing.T) {
	now := time.Now()
	s := &server.Session{
		ID:        "session-1",
		Goal:      "做一个塔防游戏",
		Status:    enums.SessionStatusRunning,
		StartedAt: now,
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "做一个塔防游戏", Timestamp: now},
		},
	}
	m := &Model{
		styles:         NewStyles(),
		sessions:       []*server.Session{s},
		sessionsCursor: 0,
	}
	m.chatPanel.lastItems = 1 // 跳过欢迎页分支，直接渲染 viewport。
	out := m.renderChat(80, 20)
	if !strings.Contains(out, "🎯") || !strings.Contains(out, "做一个塔防游戏") {
		t.Fatalf("目标栏应固定展示主任务目标:\n%s", out)
	}
	// 目标栏置顶：出现在对话内容第一行（内容本体仍在，未被顶替）。
	if !strings.HasPrefix(out, "🎯") {
		t.Errorf("目标栏应位于对话区顶部:\n%s", out)
	}
}

// TestGoalBarAbsentWithoutGoal 验证无目标（无会话/无消息）时不渲染目标栏。
func TestGoalBarAbsentWithoutGoal(t *testing.T) {
	m := &Model{styles: NewStyles()}
	out := m.renderChat(80, 20)
	if strings.Contains(out, "🎯") {
		t.Fatalf("无会话时不应渲染目标栏:\n%s", out)
	}
	if m.chatPanel.goalBarH != 0 {
		t.Errorf("goalBarH = %d, want 0", m.chatPanel.goalBarH)
	}
}

// TestGoalBarSingleLineWithMultilineGoal 回归"多行目标把整帧撑超高"：
// 目标栏预算仅 1 行，而 truncate 原样保留 '\n'（换行符显示宽度为 0），
// 粘贴的多行需求文档作为目标时曾渲染出多行，整帧超高后 fitFrameLines
// 从主内容区顶部裁行，把右侧计划面板标题裁出屏幕。目标栏必须恒为 1 行。
func TestGoalBarSingleLineWithMultilineGoal(t *testing.T) {
	now := time.Now()
	goal := "玩家通过滑动屏幕斩切水果\n水果从浮岛下方的裂隙中抛射而出\n背景有三层视差滚动"
	s := &server.Session{
		ID:        "session-1",
		Goal:      goal,
		Status:    enums.SessionStatusRunning,
		StartedAt: now,
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: goal, Timestamp: now},
		},
	}
	m := &Model{
		styles:         NewStyles(),
		sessions:       []*server.Session{s},
		sessionsCursor: 0,
	}
	m.chatPanel.lastItems = 1 // 跳过欢迎页分支，直接渲染 viewport。

	const h = 20
	out := m.renderChat(80, h)
	if got := strings.Count(out, "\n") + 1; got != h {
		t.Fatalf("多行目标下对话区应为 %d 行，实际 %d 行:\n%s", h, got, out)
	}
	if !strings.Contains(out, "🎯") {
		t.Fatalf("目标栏应仍展示（折叠为单行）:\n%s", out)
	}
}

// TestViewportHeightSyncedWithGoalBar 回归"长答复最后一行渲染不到也滚动不到"：
// 目标栏常驻时 renderChat 按 bodyH=contentH-1 渲染，但持久 vp.Height 此前只在
// WindowSizeMsg 按 contentH 设置（renderChat 内的修正落在 View 值接收者的每帧副本上
// 被丢弃），GotoBottom/SetYOffset 的偏移上限因此差 1 行。Update 路径维护的持久
// vp.Height/goalBarH 必须与渲染口径一致。
func TestViewportHeightSyncedWithGoalBar(t *testing.T) {
	now := time.Now()
	s := &server.Session{
		ID:        "session-1",
		Goal:      "做一个塔防游戏",
		Status:    enums.SessionStatusRunning,
		StartedAt: now,
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "做一个塔防游戏", Timestamp: now},
		},
	}
	m := &Model{
		styles:         NewStyles(),
		sessions:       []*server.Session{s},
		sessionsCursor: 0,
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	um, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update 返回类型应为 Model，实际 %T", updated)
	}
	if um.chatPanel.goalBarH != 1 {
		t.Errorf("有目标时 goalBarH = %d, want 1", um.chatPanel.goalBarH)
	}
	if wantH := um.mainContentHeight() - 1; um.chatPanel.vp.Height != wantH {
		t.Errorf("有目标栏时 vp.Height = %d, want %d（渲染口径 contentH-1）", um.chatPanel.vp.Height, wantH)
	}

	// 无会话（无目标栏）：vp.Height 不扣减。
	m2 := &Model{styles: NewStyles()}
	updated2, _ := m2.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	um2 := updated2.(Model)
	if um2.chatPanel.goalBarH != 0 {
		t.Errorf("无会话时 goalBarH = %d, want 0", um2.chatPanel.goalBarH)
	}
	if wantH := um2.mainContentHeight(); um2.chatPanel.vp.Height != wantH {
		t.Errorf("无目标栏时 vp.Height = %d, want %d", um2.chatPanel.vp.Height, wantH)
	}
}
