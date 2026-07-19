package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newOverflowModel 构造一个对话内容超出视口高度的 Model（无 agent facade，走本地降级路径），
// 用于验证对话区滚轮与滚动条行为。
func newOverflowModel(t *testing.T) *Model {
	t.Helper()
	m := NewModel(nil, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m = modelPtr(nm)

	// 填充 50 条消息使内容溢出（主内容区高 = 20-1-3-1 = 15）。
	sess := &server.Session{
		ID:        "session-1",
		Status:    enums.SessionStatusCompleted,
		StartedAt: time.Now(),
	}
	for i := 0; i < 50; i++ {
		sess.Messages = append(sess.Messages, types.ChatMessage{
			Role:      enums.ChatRoleAssistant,
			Content:   fmt.Sprintf("message %d", i),
			Timestamp: time.Now(),
		})
	}
	m.sessions = []*server.Session{sess}
	m.sessionsCursor = 0
	m.rebuildChatContent()
	m.chatPanel.vp.GotoBottom()
	return m
}

// TestChatWheelScroll 验证鼠标滚轮在对话区上下滚动的行为：
// 滚轮上滚减少 YOffset（取消跟随底部），滚轮下滚恢复。
func TestChatWheelScroll(t *testing.T) {
	m := newOverflowModel(t)
	if !m.chatPanel.vp.AtBottom() {
		t.Fatal("初始应处于底部")
	}
	bottomOffset := m.chatPanel.vp.YOffset
	if bottomOffset <= 0 {
		t.Fatalf("内容应可滚动，初始 YOffset=%d", bottomOffset)
	}

	// 滚轮上滚 3 次。
	for i := 0; i < 3; i++ {
		nm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		m = modelPtr(nm)
	}
	if m.chatPanel.vp.YOffset >= bottomOffset {
		t.Fatalf("滚轮上滚后 YOffset 应减小，原=%d 现=%d", bottomOffset, m.chatPanel.vp.YOffset)
	}
	if m.chatPanel.followBottom {
		t.Fatal("上滚后应取消跟随底部")
	}

	// 滚轮下滚到底。
	for i := 0; i < 10; i++ {
		nm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = modelPtr(nm)
	}
	if !m.chatPanel.vp.AtBottom() {
		t.Fatalf("连续下滚后应回到底部，YOffset=%d", m.chatPanel.vp.YOffset)
	}
	if !m.chatPanel.followBottom {
		t.Fatal("回到底部后应恢复跟随底部")
	}
}

// TestChatScrollbarClick 验证点击滚动条轨道可跳转视口位置。
func TestChatScrollbarClick(t *testing.T) {
	m := newOverflowModel(t)
	m.chatPanel.vp.GotoBottom()
	bottomOffset := m.chatPanel.vp.YOffset

	// 计算滚动条区域，点击轨道顶部（跳到内容顶部附近）。
	sx, sy, sw, sh := m.chatPanel.scrollbarArea(m.chatAreaWidth(), m.mainContentHeight())
	if sw <= 0 || sh <= 0 {
		t.Fatalf("滚动条区域无效: %+v %+v", sw, sh)
	}
	nm, _ := m.Update(tea.MouseMsg{
		X: sx, Y: sy,
		Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
	m = modelPtr(nm)
	if m.chatPanel.vp.YOffset >= bottomOffset {
		t.Fatalf("点击滚动条轨道顶部后 YOffset 应减小，原=%d 现=%d", bottomOffset, m.chatPanel.vp.YOffset)
	}
}
