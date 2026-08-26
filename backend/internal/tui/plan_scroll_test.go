package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
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

// TestPlanFooterShowsCurrentTaskAndUptime 验证计划面板底部计时行：
// "预计剩余"已替换为当前任务已执行时间 + TUI 开启至今的总计时。
func TestPlanFooterShowsCurrentTaskAndUptime(t *testing.T) {
	tuiStart := time.Now().Add(-2 * time.Minute)
	m := &Model{styles: NewStyles(), startedAt: tuiStart}
	snap := board.Snapshot{Goal: "测试", Tasks: []board.SubTask{
		{ID: "t1", Title: "已完成", Status: board.TaskDone, CreatedAt: tuiStart, UpdatedAt: tuiStart.Add(time.Minute)},
		{ID: "t2", Title: "进行中", Status: board.TaskInProgress, CreatedAt: time.Now().Add(-30 * time.Second), UpdatedAt: time.Now()},
		{ID: "t3", Title: "待处理", Status: board.TaskPending, CreatedAt: time.Now()},
	}}

	lines := m.planFooterLines(60, snap, 1, 3)
	if len(lines) != 3 {
		t.Fatalf("footer 应为 3 行（空行+进度条+计时行），实际 %d", len(lines))
	}
	timing := stripANSI(lines[2])
	if !strings.Contains(timing, "当前任务: ") || !strings.Contains(timing, "总计时: ") {
		t.Errorf("计时行应包含 当前任务/总计时: %q", timing)
	}
	if strings.Contains(timing, "预计剩余") {
		t.Errorf("预计剩余 应已被移除: %q", timing)
	}
	// 当前任务已执行约 30 秒；总计时约 2 分钟。
	if !strings.Contains(timing, "当前任务: 00:00:3") {
		t.Errorf("当前任务已执行应约 30s: %q", timing)
	}
	if !strings.Contains(timing, "总计时: 00:02:") {
		t.Errorf("总计时应约 2min: %q", timing)
	}

	// 无进行中任务时当前任务列显示占位符。
	snap.Tasks[1].Status = board.TaskDone
	timing = stripANSI(m.planFooterLines(60, snap, 2, 3)[2])
	if !strings.Contains(timing, "当前任务: --:--:--") {
		t.Errorf("无进行中任务时应显示占位符: %q", timing)
	}

	// 未记录启动时间（测试字面量构造）时总计时显示占位符而非异常大值。
	m2 := &Model{styles: NewStyles()}
	timing = stripANSI(m2.planFooterLines(60, snap, 2, 3)[2])
	if !strings.Contains(timing, "总计时: --:--:--") {
		t.Errorf("无启动时间时总计时应显示占位符: %q", timing)
	}
}

// TestAgentsWheelScroll 验证滚轮悬停在右侧 Agent 编排面板区域时滚动编排树而非对话区：
// agentScroll 随滚轮增减并钳制在 [0, agentMaxScroll]，planScroll 不受影响。
func TestAgentsWheelScroll(t *testing.T) {
	// 12 个运行中的领域分支，编排树内容必然超出可视高度。
	var nodes []orchestrator.Node
	for i := 0; i < 12; i++ {
		nodes = append(nodes, orchestrator.Node{
			ID:       fmt.Sprintf("session-1/domain-%d", i),
			ParentID: "session-1",
			Role:     "domain",
			Domain:   fmt.Sprintf("领域%d", i),
			Task:     "做任务",
			Status:   orchestrator.StatusRunning,
			Started:  time.Now(),
		})
	}
	ag := &mockAgentForPlan{sessionID: "session-1", treeNodes: nodes}
	m := NewModel(ag, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = modelPtr(nm)
	m.sessions = []*server.Session{{ID: "session-1", Status: enums.SessionStatusRunning, StartedAt: time.Now()}}
	m.sessionsCursor = 0
	m.rebuildAgents()

	if !m.rightPanelVisible() {
		t.Fatal("宽度 120 且有会话，右侧面板应可见")
	}
	if max := m.agentMaxScroll(); max <= 0 {
		t.Fatalf("12 个领域分支应可滚动，agentMaxScroll=%d", max)
	}

	// 编排面板区域内（右栏下段）滚轮下滚 → agentScroll 增大，planScroll 不动。
	topH, _ := rightPanelHeights(m.mainContentHeight())
	x := m.chatAreaWidth() + 2
	y := 1 + topH + 2
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.agentScroll != 3 {
		t.Fatalf("下滚一次 agentScroll 应为 3，实际 %d", m.agentScroll)
	}
	if m.planScroll != 0 {
		t.Fatalf("编排面板滚轮不应改变 planScroll，实际 %d", m.planScroll)
	}
	// 上滚回顶并钳制不低于 0。
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.agentScroll != 0 {
		t.Fatalf("连续上滚应钳制在 0，实际 %d", m.agentScroll)
	}
	// 连续下滚钳制在 agentMaxScroll。
	for i := 0; i < 20; i++ {
		nm, _ = m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = modelPtr(nm)
	}
	if m.agentScroll != m.agentMaxScroll() {
		t.Fatalf("连续下滚应钳制在 agentMaxScroll=%d，实际 %d", m.agentMaxScroll(), m.agentScroll)
	}

	// 对话区内的滚轮不受影响（仍滚动对话 viewport）。
	before := m.agentScroll
	nm, _ = m.Update(tea.MouseMsg{X: 5, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = modelPtr(nm)
	if m.agentScroll != before {
		t.Fatalf("对话区滚轮不应改变 agentScroll，原=%d 现=%d", before, m.agentScroll)
	}
}
