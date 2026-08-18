package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// planSnapshotWith 构造含 n 个 pending 任务的看板快照。
func planSnapshotWith(n int) *board.Snapshot {
	snap := &board.Snapshot{Goal: "测试计划"}
	for i := 0; i < n; i++ {
		snap.Tasks = append(snap.Tasks, board.SubTask{
			ID:        fmt.Sprintf("t%d", i+1),
			Title:     fmt.Sprintf("任务 %d", i+1),
			Status:    board.TaskPending,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
	}
	return snap
}

// TestFormatPlanSnapshotScrollWindow 验证计划面板滚动开窗（替代旧"… 还有 N 项"截断）：
// scroll=0 时末行为"↓ 下方还有 N 项"提示；滚到底部时首行为"↑ 上方还有 N 项"提示；
// 两种情况下总行数恒等于 maxLines（底部统计区始终可见）。
func TestFormatPlanSnapshotScrollWindow(t *testing.T) {
	m := &Model{styles: NewStyles()}
	snap := *planSnapshotWith(10)
	const maxLines = 8 // budget = 8 - footer(3) = 5 条任务可见

	// scroll=0：顶部窗口，末行为下方提示。
	lines := m.formatPlanSnapshot(60, snap, maxLines, 0)
	if len(lines) != maxLines {
		t.Fatalf("总行数 = %d, want %d:\n%s", len(lines), maxLines, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "任务 1") {
		t.Errorf("scroll=0 首行应为任务 1: %q", lines[0])
	}
	if !strings.Contains(lines[maxLines-4], "下方还有 5 项") {
		t.Errorf("scroll=0 任务窗口末行应为下方提示: %q", lines[maxLines-4])
	}

	// scroll 到底（maxOff=5）：首行为上方提示，末条任务可见，无下方提示。
	lines = m.formatPlanSnapshot(60, snap, maxLines, 5)
	if len(lines) != maxLines {
		t.Fatalf("scroll=5 总行数 = %d, want %d", len(lines), maxLines)
	}
	if !strings.Contains(lines[0], "上方还有 5 项") {
		t.Errorf("scroll=5 首行应为上方提示: %q", lines[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "任务 10") {
		t.Errorf("scroll=5 应能看到任务 10:\n%s", joined)
	}
	if strings.Contains(joined, "下方还有") {
		t.Errorf("scroll=5 已到底部，不应有下方提示:\n%s", joined)
	}

	// scroll 越界钳制：超过 maxOff 回退到末尾窗口。
	lines = m.formatPlanSnapshot(60, snap, maxLines, 99)
	if !strings.Contains(strings.Join(lines, "\n"), "任务 10") {
		t.Errorf("scroll 越界应钳制到末尾窗口:\n%s", strings.Join(lines, "\n"))
	}
}

// TestPlanWheelScroll 验证滚轮悬停在右侧计划面板区域时滚动计划列表而非对话区：
// planScroll 随滚轮增减并钳制在 [0, planMaxScroll]，对话 viewport 不受影响。
func TestPlanWheelScroll(t *testing.T) {
	ag := &mockAgentForPlan{sessionID: "session-1", boardSnap: planSnapshotWith(30)}
	m := NewModel(ag, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = modelPtr(nm)
	m.sessions = []*server.Session{{ID: "session-1", Status: enums.SessionStatusRunning, StartedAt: time.Now()}}
	m.sessionsCursor = 0

	if !m.rightPanelVisible() {
		t.Fatal("宽度 120 且有会话，右侧面板应可见")
	}
	if max := m.planMaxScroll(); max <= 0 {
		t.Fatalf("30 个任务应可滚动，planMaxScroll=%d", max)
	}

	// 计划面板区域内（右栏上段）滚轮下滚 → planScroll 增大。
	x := m.chatAreaWidth() + 2
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: 2, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.planScroll != 3 {
		t.Fatalf("下滚一次 planScroll 应为 3，实际 %d", m.planScroll)
	}
	// 上滚回顶并钳制不低于 0。
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: 2, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: 2, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.planScroll != 0 {
		t.Fatalf("连续上滚应钳制在 0，实际 %d", m.planScroll)
	}
	// 连续下滚钳制在 planMaxScroll。
	for i := 0; i < 20; i++ {
		nm, _ = m.Update(tea.MouseMsg{X: x, Y: 2, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = modelPtr(nm)
	}
	if m.planScroll != m.planMaxScroll() {
		t.Fatalf("连续下滚应钳制在 planMaxScroll=%d，实际 %d", m.planMaxScroll(), m.planScroll)
	}

	// 对话区内的滚轮不受影响（仍滚动对话 viewport）。
	before := m.planScroll
	nm, _ = m.Update(tea.MouseMsg{X: 5, Y: 2, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.planScroll != before {
		t.Fatalf("对话区滚轮不应改变 planScroll，原=%d 现=%d", before, m.planScroll)
	}
}
