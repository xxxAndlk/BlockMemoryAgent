package tui

// clipboard_input_test.go 验证 Alt+V 剪贴板图片粘贴：
// 占位符插入、暂存对齐、限流拒绝、提交后清空与 Alt+V 拦截（不插入字符 v）。

import (
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
)

func newClipTestModel() *Model {
	return &Model{
		styles:    NewStyles(),
		width:     120,
		height:    40,
		focus:     panelInput,
		shared:    newSharedState(),
		inputBar:  NewInputBar(),
	}
}

// TestAltVDoesNotInsertRune 验证 Alt+V 被拦截而非作为普通字符 'v' 插入。
func TestAltVDoesNotInsertRune(t *testing.T) {
	m := newClipTestModel()
	nm, cmd := m.handleInputKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	mv := nm.(*Model)
	if string(mv.inputBar.runes) != "" {
		t.Fatalf("Alt+V 不应插入字符, got %q", string(mv.inputBar.runes))
	}
	if cmd == nil {
		t.Fatal("Alt+V 应返回读取剪贴板的 tea.Cmd")
	}
}

// TestClipImageMsgInsertsPlaceholder 验证粘贴结果：暂存图片 + 光标处插入
// 递增 [image:N] 占位符；第 5 张拒绝。
func TestClipImageMsgInsertsPlaceholder(t *testing.T) {
	m := newClipTestModel()
	png := []byte{0x89, 0x50, 0x4E, 0x47}

	for i := 1; i <= 4; i++ {
		m.applyClipImage(clipImageMsg{png: png})
	}
	if len(m.inputBar.pendingImages) != 4 {
		t.Fatalf("应暂存 4 张图, got %d", len(m.inputBar.pendingImages))
	}
	if got := string(m.inputBar.runes); got != "[image:1][image:2][image:3][image:4]" {
		t.Fatalf("占位符序列不符: %q", got)
	}
	if m.inputBar.cursor != len("[image:1][image:2][image:3][image:4]") {
		t.Fatalf("光标应在末尾, got %d", m.inputBar.cursor)
	}

	// 第 5 张：拒绝，不追加占位符。
	m.applyClipImage(clipImageMsg{png: png})
	if len(m.inputBar.pendingImages) != 4 {
		t.Fatalf("第 5 张应被拒绝, got %d", len(m.inputBar.pendingImages))
	}
	if strings.Contains(string(m.inputBar.runes), "[image:5]") {
		t.Fatalf("第 5 张不应插入占位符: %q", string(m.inputBar.runes))
	}

	// 错误消息：不改状态。
	m.applyClipImage(clipImageMsg{err: errTestClip})
	if len(m.inputBar.pendingImages) != 4 {
		t.Fatalf("错误结果不应改变暂存, got %d", len(m.inputBar.pendingImages))
	}
}

// TestClipImageMsgSizeLimit 验证单张超 4MiB 拒绝。
func TestClipImageMsgSizeLimit(t *testing.T) {
	m := newClipTestModel()
	big := make([]byte, agent.MaxMessageImageBytes+1)
	m.applyClipImage(clipImageMsg{png: big})
	if len(m.inputBar.pendingImages) != 0 {
		t.Fatalf("超限图片应被拒绝, got %d", len(m.inputBar.pendingImages))
	}
	if string(m.inputBar.runes) != "" {
		t.Fatalf("超限不应插入占位符: %q", string(m.inputBar.runes))
	}
}

// TestSubmitClearsPendingImages 验证提交（Enter）后暂存图片清空、编号重置。
func TestSubmitClearsPendingImages(t *testing.T) {
	m := newClipTestModel()
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	m.applyClipImage(clipImageMsg{png: png})
	m.inputBar.insertRunes([]rune(" 看图实现"))

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv := nm.(*Model)
	if len(mv.inputBar.pendingImages) != 0 {
		t.Fatalf("提交后应清空暂存图片, got %d", len(mv.inputBar.pendingImages))
	}
	if string(mv.inputBar.runes) != "" {
		t.Fatalf("提交后输入栏应清空, got %q", string(mv.inputBar.runes))
	}

	// 下一轮粘贴从 [image:1] 重新编号。
	mv.applyClipImage(clipImageMsg{png: png})
	if got := string(mv.inputBar.runes); got != "[image:1]" {
		t.Fatalf("编号应从 1 重置: %q", got)
	}
}

// TestClipImageDataIsBase64 回归：Data 链路内约定为 base64 ASCII（ToBladesMessages
// 按 base64 解码，raw 字节会被静默丢弃——实证 glm 首测只收到占位符）。
func TestClipImageDataIsBase64(t *testing.T) {
	m := newClipTestModel()
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	m.applyClipImage(clipImageMsg{png: png})
	if len(m.inputBar.pendingImages) != 1 {
		t.Fatalf("应暂存 1 张图")
	}
	raw, err := base64.StdEncoding.DecodeString(string(m.inputBar.pendingImages[0].Data))
	if err != nil {
		t.Fatalf("Data 应为合法 base64: %v", err)
	}
	if string(raw) != string(png) {
		t.Fatalf("base64 解码应还原原 PNG 字节, got %v", raw)
	}
}

// errTestClip 是错误路径测试用的哨兵错误。
var errTestClip = &clipErrorStub{}

type clipErrorStub struct{}

func (e *clipErrorStub) Error() string { return "stub clipboard error" }
