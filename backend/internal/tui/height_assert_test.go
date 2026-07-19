package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestViewTotalLinesEqualsHeight 验证各窗口高度下 View 渲染总行数严格等于终端高度，
// 防止 alt-screen 因内容超出而把顶栏/首行顶出屏幕（输入栏 5 行预算回归测试）。
func TestViewTotalLinesEqualsHeight(t *testing.T) {
	for _, h := range []int{24, 40, 45, 55} {
		m := NewModel(nil, nil, "http://127.0.0.1:1", "test")
		nm, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: h})
		mv := modelPtr(nm)
		got := strings.Count(mv.View(), "\n") + 1
		if got != h {
			t.Fatalf("height=%d: View 应为 %d 行，实际 %d 行", h, h, got)
		}
	}
}
