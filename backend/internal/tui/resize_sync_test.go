package tui

// resize_sync_test.go 回归"Windows 下终端尺寸启动后不再更新"的修复：
// bubbletea v1 的 listenForResize 在 Windows 是空实现（无 SIGWINCH），
// 窗口最大化/还原/全屏切换后 WindowSizeMsg 不再上报，模型按启动尺寸渲染，
// 帧比物理屏幕高时整帧上滚、顶栏被顶出屏幕。修复 = 1s 轮询真实尺寸 +
// 变化时合成 WindowSizeMsg（模型与渲染器走同一 resize 路径）。

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestReconcileSize 验证尺寸对账：未变化返回 nil；变化时产出合成 WindowSizeMsg，
// 且该消息经 Update 后模型尺寸与 viewport 同步更新。
func TestReconcileSize(t *testing.T) {
	m := NewModel(nil, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mm := modelPtr(nm)

	if cmd := mm.reconcileSize(120, 40); cmd != nil {
		t.Fatalf("尺寸未变化应返回 nil，实际非 nil")
	}

	cmd := mm.reconcileSize(236, 57)
	if cmd == nil {
		t.Fatalf("尺寸变化应返回合成 WindowSizeMsg 的命令，实际 nil")
	}
	msg, ok := cmd().(tea.WindowSizeMsg)
	if !ok {
		t.Fatalf("命令应产出 tea.WindowSizeMsg，实际 %T", cmd())
	}
	if msg.Width != 236 || msg.Height != 57 {
		t.Fatalf("合成 WindowSizeMsg 尺寸错误: %+v", msg)
	}

	nm2, _ := mm.Update(msg)
	mm2 := modelPtr(nm2)
	if mm2.width != 236 || mm2.height != 57 {
		t.Fatalf("Update 后尺寸应为 236x57，实际 %dx%d", mm2.width, mm2.height)
	}
}

// TestSizePollMsgRearms 验证轮询消息处理后总是重新武装轮询命令：
// 尺寸未变化时只重新武装；变化时批量返回（轮询 + 合成 WindowSizeMsg）。
func TestSizePollMsgRearms(t *testing.T) {
	m := NewModel(nil, nil, "http://127.0.0.1:1", "test")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mm := modelPtr(nm)

	if _, cmd := mm.Update(sizePollMsg{width: 120, height: 40}); cmd == nil {
		t.Fatalf("尺寸未变化时也应返回重新武装的轮询命令")
	}
	if _, cmd := mm.Update(sizePollMsg{width: 200, height: 50}); cmd == nil {
		t.Fatalf("尺寸变化时应返回批量命令（轮询 + 合成 WindowSizeMsg）")
	}
}

// TestFitFrameLinesKeepsTopBar 超高帧裁剪：顶栏（第 0 行）与底部行保留，中间丢弃。
func TestFitFrameLinesKeepsTopBar(t *testing.T) {
	lines := []string{"top", "c1", "c2", "c3", "c4", "c5", "c6", "b1", "b2", "b3"}

	got := fitFrameLines(lines, 5)
	want := []string{"top", "c6", "b1", "b2", "b3"}
	if len(got) != len(want) {
		t.Fatalf("fitFrameLines 应为 %d 行，实际 %d 行: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fitFrameLines[%d] = %q, want %q（got %v）", i, got[i], want[i], got)
		}
	}

	if got := fitFrameLines(lines, len(lines)); len(got) != len(lines) {
		t.Fatalf("不超高原样返回，实际 %d 行", len(got))
	}
	if got := fitFrameLines(lines, 1); len(got) != 1 || got[0] != "top" {
		t.Fatalf("height=1 应仅保留顶栏，实际 %v", got)
	}
	if got := fitFrameLines(lines, 0); len(got) != len(lines) {
		t.Fatalf("height<=0 原样返回，实际 %d 行", len(got))
	}
}
