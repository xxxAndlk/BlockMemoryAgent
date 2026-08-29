package tui

// control_char_input_test.go 验证输入栏 KeyRunes 的控制字符过滤：
// Windows 控制台 Ctrl+Space/Ctrl+@ 产生 0x00 的 KeyRunes，粘贴文本也可能夹带 NUL；
// 原样入库会被 PG 拒绝（invalid byte sequence 22021）。过滤只保留可打印字符，
// 但保留多行粘贴依赖的 \n / \t（见 multiline_input_test.go）。

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newControlCharTestModel() *Model {
	return &Model{
		styles:   NewStyles(),
		width:    120,
		height:   40,
		focus:    panelInput,
		shared:   newSharedState(),
		inputBar: NewInputBar(),
	}
}

// TestNULRuneNotInserted 验证纯 NUL 的 KeyRunes（Ctrl+Space/Ctrl+@）不插入任何字符。
func TestNULRuneNotInserted(t *testing.T) {
	m := newControlCharTestModel()
	nm, _ := m.handleInputKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0x00}})
	mv := nm.(*Model)
	if len(mv.inputBar.runes) != 0 {
		t.Fatalf("NUL 不应插入输入栏, got %q", string(mv.inputBar.runes))
	}
	if mv.inputBar.cursor != 0 {
		t.Fatalf("光标不应移动, got %d", mv.inputBar.cursor)
	}
}

// TestControlCharsFilteredFromInput 验证混合输入只保留可打印字符，光标按过滤后长度推进。
func TestControlCharsFilteredFromInput(t *testing.T) {
	m := newControlCharTestModel()
	nm, _ := m.handleInputKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', 0x00, 'b', 0x01, 0x1f}})
	mv := nm.(*Model)
	if got := string(mv.inputBar.runes); got != "ab" {
		t.Fatalf("控制字符应被过滤, got %q", got)
	}
	if mv.inputBar.cursor != 2 {
		t.Fatalf("光标应按过滤后长度推进到 2, got %d", mv.inputBar.cursor)
	}
}

// TestFilteredInsertRespectsCursor 验证光标在中间插入时，过滤与光标推进都基于过滤后内容。
func TestFilteredInsertRespectsCursor(t *testing.T) {
	m := newControlCharTestModel()
	m.inputBar.runes = []rune("xy")
	m.inputBar.cursor = 1
	nm, _ := m.handleInputKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0x00, 'Z'}})
	mv := nm.(*Model)
	if got := string(mv.inputBar.runes); got != "xZy" {
		t.Fatalf("应在光标处插入过滤后的字符, got %q", got)
	}
	if mv.inputBar.cursor != 2 {
		t.Fatalf("光标应前进 1（只插入了 Z）, got %d", mv.inputBar.cursor)
	}
}

// TestPasteKeepsNewlineTabStripsNUL 验证粘贴路径（单个 KeyRunes 携带多行内容）
// 保留 \n / \t，仅剥离 NUL 等控制字符，多行折叠判定不受影响。
func TestPasteKeepsNewlineTabStripsNUL(t *testing.T) {
	m := newControlCharTestModel()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\x00b\n\tc"), Paste: true})
	mv := nm.(*Model)
	if got := string(mv.inputBar.runes); got != "ab\n\tc" {
		t.Fatalf("粘贴应保留换行/制表符并剥离 NUL, got %q", got)
	}
	if !mv.inputIsMultiline() {
		t.Fatal("保留换行后仍应判定为多行输入")
	}
	if mv.inputBar.cursor != len([]rune("ab\n\tc")) {
		t.Fatalf("光标应在末尾, got %d", mv.inputBar.cursor)
	}
}
