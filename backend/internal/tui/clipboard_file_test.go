package tui

// clipboard_file_test.go 验证 Alt+V 剪贴板视频附件通道：
//   - parseFileDropOutput：EMPTY 哨兵 / 多路径含 \r / 空行；
//   - filterVideoPaths：扩展名白名单过滤；
//   - applyClipVideo：限流、[video:N] 占位插入与 pendingVideos 顺序对齐；
//   - clearPendingAttachments：图片视频同步清理（reset/backspace/delete/历史浏览）。

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestParseFileDropOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty sentinel", "EMPTY\r\n", nil},
		{"blank output", "", nil},
		{"single path", "C:\\Users\\a\\clip.mp4\r\n", []string{"C:\\Users\\a\\clip.mp4"}},
		{"multi path crlf", "C:\\a.mp4\r\nD:\\b.mov\r\n", []string{"C:\\a.mp4", "D:\\b.mov"}},
		{"blank lines skipped", "C:\\a.mp4\n\nD:\\b.webm\n", []string{"C:\\a.mp4", "D:\\b.webm"}},
	}
	for _, c := range cases {
		got := parseFileDropOutput(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func TestFilterVideoPaths(t *testing.T) {
	got := filterVideoPaths([]string{
		"C:\\a.mp4", "C:\\readme.txt", "D:\\v\\b.WEBM", "C:\\c.avi", "D:\\e.exe",
	})
	want := []string{"C:\\a.mp4", "D:\\v\\b.WEBM", "C:\\c.avi"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if v := filterVideoPaths([]string{"C:\\only.txt"}); v != nil {
		t.Fatalf("全非视频应返回 nil, got %v", v)
	}
}

func TestApplyClipVideo(t *testing.T) {
	m := newClipTestModel()
	a := filepath.Join("C:", "a.mp4")
	b := filepath.Join("D:", "b.mkv")

	m.applyClipVideo(clipVideoMsg{paths: []string{a, b}})
	if len(m.inputBar.pendingVideos) != 2 {
		t.Fatalf("应暂存 2 个视频, got %d", len(m.inputBar.pendingVideos))
	}
	if m.inputBar.pendingVideos[0].Path != a || m.inputBar.pendingVideos[0].MIMEType != "video/mp4" {
		t.Fatalf("第 1 个视频不符: %+v", m.inputBar.pendingVideos[0])
	}
	if m.inputBar.pendingVideos[1].MIMEType != "video/x-matroska" {
		t.Fatalf("第 2 个视频 mime 应按扩展名回填: %+v", m.inputBar.pendingVideos[1])
	}
	if got := string(m.inputBar.runes); got != "[video:1][video:2]" {
		t.Fatalf("占位符序列不符: %q", got)
	}

	// 超上限（2 个）：第 3 个拒绝，不追加占位符。
	m.applyClipVideo(clipVideoMsg{paths: []string{filepath.Join("E:", "c.mov")}})
	if len(m.inputBar.pendingVideos) != agent.MaxMessageVideos {
		t.Fatalf("超上限应拒绝, got %d", len(m.inputBar.pendingVideos))
	}
	if strings.Contains(string(m.inputBar.runes), "[video:3]") {
		t.Fatalf("超上限不应插入占位符: %q", string(m.inputBar.runes))
	}
}

func TestClipVideoPlaceholderRenumberAfterClear(t *testing.T) {
	m := newClipTestModel()
	m.applyClipVideo(clipVideoMsg{paths: []string{filepath.Join("C:", "a.mp4")}})
	// 提交（Enter）清空附件：编号下一轮从 1 重新计。
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv := nm.(*Model)
	if len(mv.inputBar.pendingVideos) != 0 || len(mv.inputBar.pendingImages) != 0 {
		t.Fatalf("提交后附件应清空, videos=%d images=%d", len(mv.inputBar.pendingVideos), len(mv.inputBar.pendingImages))
	}
	mv.applyClipVideo(clipVideoMsg{paths: []string{filepath.Join("C:", "b.mp4")}})
	if got := string(mv.inputBar.runes); got != "[video:1]" {
		t.Fatalf("编号应从 1 重置: %q", got)
	}
}

// TestClearPendingAttachmentsPaths 验证所有全量替换输入的路径同步清附件。
func TestClearPendingAttachmentsPaths(t *testing.T) {
	newFull := func() (*Model, string) {
		m := newClipTestModel()
		m.applyClipImage(clipImageMsg{png: []byte{0x89, 0x50, 0x4E, 0x47}})
		m.applyClipVideo(clipVideoMsg{paths: []string{filepath.Join("C:", "a.mp4")}})
		m.inputBar.insertRunes([]rune("第一行\n第二行")) // 多行
		return m, string(m.inputBar.runes)
	}
	check := func(t *testing.T, m *Model, scene string) {
		t.Helper()
		if len(m.inputBar.pendingVideos) != 0 || len(m.inputBar.pendingImages) != 0 {
			t.Fatalf("%s 后附件应清空, videos=%d images=%d", scene, len(m.inputBar.pendingVideos), len(m.inputBar.pendingImages))
		}
	}

	// reset。
	m, _ := newFull()
	m.inputBar.reset()
	check(t, m, "reset")

	// backspace 多行整清。
	m, _ = newFull()
	m.inputBar.backspace()
	check(t, m, "backspace")

	// delete 多行整清。
	m, _ = newFull()
	m.inputBar.delete()
	check(t, m, "delete")

	// 历史浏览（InputBar.historyUp/Down，input.go KeyUp/KeyDown 分支内联同款替换逻辑）。
	m, _ = newFull()
	m.inputBar.pushHistory("s1", "旧消息")
	m.inputBar.historyUp("s1")
	check(t, m, "historyUp")

	m, _ = newFull()
	m.inputBar.pushHistory("s1", "旧消息")
	m.inputBar.historyUp("s1")
	m.inputBar.historyDown("s1")
	check(t, m, "historyDown")

	// ESC 离开输入栏。
	m, _ = newFull()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	check(t, nm.(*Model), "esc")
}

// 编译期防回归：pendingVideos 与图片共用 agent.WireVideo / tool.ResultImage 类型。
var (
	_ = tool.ResultImage{}
	_ = agent.WireVideo{}
)
