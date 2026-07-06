package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestMultilinePasteCollapsesInput 验证粘贴多行内容后输入栏折叠为 [n行内容] 占位，
// 但底层仍保留完整内容用于发送。
func TestMultilinePasteCollapsesInput(t *testing.T) {
	m := &Model{
		styles:  NewStyles(),
		width:   120,
		height:  40,
		focus:   panelInput,
		flashMu: &sync.Mutex{},
	}
	m.chatVP.Width = 80
	m.chatVP.Height = 20
	m.chatVP.SetContent("")

	pasted := "line1\nline2\nline3"
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})
	mv := nm.(*Model)

	if string(mv.inputRunes) != pasted {
		t.Fatalf("底层应保留完整粘贴内容，got %q", string(mv.inputRunes))
	}
	if !mv.inputIsMultiline() {
		t.Fatal("inputIsMultiline 应为 true")
	}

	view := mv.View()
	if !strings.Contains(view, "[3行内容]") {
		t.Fatalf("输入栏应显示 [3行内容] 占位，got:\n%s", view)
	}
	// 视图不应直接展开三行原文
	if strings.Contains(view, "line1\nline2\nline3") {
		t.Fatalf("输入栏不应直接展开多行原文，got:\n%s", view)
	}
}

// TestMultilineInputBackspaceClearsAll 验证多行输入下 Backspace 一次性清空。
func TestMultilineInputBackspaceClearsAll(t *testing.T) {
	m := &Model{
		styles:      NewStyles(),
		width:       120,
		height:      40,
		focus:       panelInput,
		inputRunes:  []rune("a\nb\nc"),
		inputCursor: 5,
		flashMu:     &sync.Mutex{},
	}

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	mv := nm.(*Model)

	if len(mv.inputRunes) != 0 {
		t.Fatalf("Backspace 应清空多行输入，got %q", string(mv.inputRunes))
	}
}

// TestSingleLineInputStillShowsCursor 验证单行输入保持原有光标展示。
func TestSingleLineInputStillShowsCursor(t *testing.T) {
	m := &Model{
		styles:      NewStyles(),
		width:       120,
		height:      40,
		focus:       panelInput,
		inputRunes:  []rune("hello"),
		inputCursor: 2,
		flashMu:     &sync.Mutex{},
	}
	m.chatVP.Width = 80
	m.chatVP.Height = 20
	m.chatVP.SetContent("")

	view := m.View()
	if strings.Contains(view, "[1行内容]") {
		t.Fatalf("单行输入不应显示占位，got:\n%s", view)
	}
	if !strings.Contains(view, "he▌llo") {
		t.Fatalf("单行输入应显示光标，got:\n%s", view)
	}
}

// TestMultilineSubmitPreservesContent 验证折叠的多行内容在发送时完整提交，
// 且未选中会话时被完整保留到本地预展示。
func TestMultilineSubmitPreservesContent(t *testing.T) {
	m := &Model{
		styles:  NewStyles(),
		width:   120,
		height:  40,
		focus:   panelInput,
		flashMu: &sync.Mutex{},
	}
	m.chatVP.Width = 80
	m.chatVP.Height = 20
	m.chatVP.SetContent("")

	pasted := "func main() {\n\tfmt.Println(\"hello\")\n}"
	m.inputRunes = []rune(pasted)
	m.inputCursor = len(m.inputRunes)

	m.submitInput(string(m.inputRunes))

	if m.pendingFirstMessage != pasted {
		t.Fatalf("未选中会话时应完整保留粘贴内容用于创建会话，got %q", m.pendingFirstMessage)
	}
}

// TestPasteEnterInsertsNewline 验证粘贴内容中的换行不会触发提交，
// 而是作为文本换行插入，避免多行粘贴被拆成多次发送。
func TestPasteEnterInsertsNewline(t *testing.T) {
	m := &Model{
		styles:  NewStyles(),
		width:   120,
		height:  40,
		focus:   panelInput,
		flashMu: &sync.Mutex{},
	}
	m.chatVP.Width = 80
	m.chatVP.Height = 20
	m.chatVP.SetContent("")

	// 模拟粘贴第一行
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line1"), Paste: true})
	mv := nm.(*Model)
	// 粘贴中的换行应作为 \n 插入，而不是提交
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyEnter, Paste: true})
	mv = nm.(*Model)
	if string(mv.inputRunes) != "line1\n" {
		t.Fatalf("粘贴中的 Enter 应插入换行，got %q", string(mv.inputRunes))
	}
	if mv.pendingFirstMessage != "" {
		t.Fatalf("粘贴换行时不应提交，pendingFirstMessage=%q", mv.pendingFirstMessage)
	}

	// 继续粘贴第二行
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line2"), Paste: true})
	mv = nm.(*Model)
	// 模拟用户停顿后手动按 Enter，避免被判定为粘贴的一部分
	mv.lastKeyTime = time.Now().Add(-200 * time.Millisecond)
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv = nm.(*Model)
	if mv.pendingFirstMessage != "line1\nline2" {
		t.Fatalf("普通 Enter 应一次性提交完整多行内容，got %q", mv.pendingFirstMessage)
	}
}

// TestRapidPasteAccumulatesLines 验证终端把多行粘贴拆成无 Paste 标记的快速 Enter 时，
// 仍会把所有行合并成一条消息，而不是逐行提交。
func TestRapidPasteAccumulatesLines(t *testing.T) {
	m := &Model{
		styles:  NewStyles(),
		width:   120,
		height:  40,
		focus:   panelInput,
		flashMu: &sync.Mutex{},
	}
	m.chatVP.Width = 80
	m.chatVP.Height = 20
	m.chatVP.SetContent("")

	// 把 lastKeyTime 设为刚过去，模拟终端粘贴的高速连续按键
	m.lastKeyTime = time.Now().Add(-10 * time.Millisecond)

	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line1")})
	mv := nm.(*Model)
	mv.lastKeyTime = time.Now().Add(-10 * time.Millisecond)

	// 快速 Enter → 插入换行，不提交
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv = nm.(*Model)
	if string(mv.inputRunes) != "line1\n" {
		t.Fatalf("快速 Enter 应插入换行，got %q", string(mv.inputRunes))
	}
	if mv.pendingFirstMessage != "" {
		t.Fatalf("粘贴过程中不应提交，pendingFirstMessage=%q", mv.pendingFirstMessage)
	}
	mv.lastKeyTime = time.Now().Add(-10 * time.Millisecond)

	// 第二行文本
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line2")})
	mv = nm.(*Model)
	mv.lastKeyTime = time.Now().Add(-10 * time.Millisecond)

	// 再次快速 Enter → 继续累积
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv = nm.(*Model)
	if string(mv.inputRunes) != "line1\nline2\n" {
		t.Fatalf("应继续累积多行内容，got %q", string(mv.inputRunes))
	}

	// 稍等一会儿再按 Enter，视为手动提交
	mv.lastKeyTime = time.Now().Add(-200 * time.Millisecond)
	nm, _ = mv.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mv = nm.(*Model)
	if mv.pendingFirstMessage != "line1\nline2\n" {
		t.Fatalf("手动 Enter 应一次性提交完整内容，got %q", mv.pendingFirstMessage)
	}
}
