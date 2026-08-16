package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestEventChatItemClarifyQuestion 验证 clarify 提问事件（Agent 非 User）：
// 剥离后端 "Agent 提问: " 前缀后以 ❓ 前缀展示；审批场景无前缀的原始问题文本原样展示。
func TestEventChatItemClarifyQuestion(t *testing.T) {
	// ask_user 提问：Message 带 "Agent 提问: " 前缀。
	ev := server.SessionEvent{
		Type:      "clarify",
		Agent:     "System",
		Message:   "Agent 提问: 主题色选深色还是浅色？",
		Timestamp: time.Now(),
	}
	title, detail, rawDetail, ok := eventChatItem(ev, true)
	if !ok {
		t.Fatal("clarify 提问事件应展示")
	}
	if title != "❓ 主题色选深色还是浅色？" {
		t.Fatalf("应剥离 \"Agent 提问: \" 前缀并加 ❓，got %q", title)
	}
	if detail != "" {
		t.Fatalf("提问事件 detail 应为空，got %q", detail)
	}
	if rawDetail != title {
		t.Fatalf("rawDetail 应等于 title，got %q", rawDetail)
	}

	// 审批提问：Message 为原始问题文本（无前缀），仅加 ❓。
	ev2 := server.SessionEvent{
		Type:      "clarify",
		Agent:     "System",
		Message:   "执行 rm -rf /workspace 需确认",
		Timestamp: time.Now(),
	}
	title2, _, _, ok2 := eventChatItem(ev2, true)
	if !ok2 || title2 != "❓ 执行 rm -rf /workspace 需确认" {
		t.Fatalf("无前缀提问应仅加 ❓，got %q ok=%v", title2, ok2)
	}
}

// TestEventChatItemClarifyReply 验证 clarify 答复事件（Agent 为 User）：
// 剥离 "提问答复: " / "审批答复: " 前缀后以 ✅ 前缀展示。
func TestEventChatItemClarifyReply(t *testing.T) {
	cases := []struct {
		msg   string
		want  string
	}{
		{"提问答复: 深色", "✅ 答复: 深色"},
		{"审批答复: confirm", "✅ 答复: confirm"},
	}
	for _, tc := range cases {
		ev := server.SessionEvent{
			Type:      "clarify",
			Agent:     "User",
			Message:   tc.msg,
			Timestamp: time.Now(),
		}
		title, detail, rawDetail, ok := eventChatItem(ev, true)
		if !ok {
			t.Fatalf("答复事件 %q 应展示", tc.msg)
		}
		if title != tc.want {
			t.Fatalf("应剥离答复前缀并加 ✅，got %q want %q", title, tc.want)
		}
		if detail != "" {
			t.Fatalf("答复事件 detail 应为空，got %q", detail)
		}
		if rawDetail != title {
			t.Fatalf("rawDetail 应等于 title，got %q", rawDetail)
		}
	}
}

// TestEventChatItemClarifyEmpty 验证 clarify 事件消息为空时不展示（ok=false）。
func TestEventChatItemClarifyEmpty(t *testing.T) {
	for _, ev := range []server.SessionEvent{
		{Type: "clarify", Agent: "System", Message: "", Timestamp: time.Now()},
		{Type: "clarify", Agent: "User", Message: "  ", Timestamp: time.Now()},
	} {
		if _, _, _, ok := eventChatItem(ev, true); ok {
			t.Fatalf("空消息 clarify 事件不应展示: %+v", ev)
		}
	}
}

// TestChatItemsResolvedClarifyOptions 验证 clarify 提问条目在对话区追加结构化选项：
// 带 Options 时 detail 含 "N. <label>" 编号行与多选提示；无 Options 时 detail 为空。
func TestChatItemsResolvedClarifyOptions(t *testing.T) {
	now := time.Now()
	s := &server.Session{
		Messages: []types.ChatMessage{},
		Status:   enums.SessionStatusAwaitingClarify,
		State: &types.ThreeLayerState{
			PendingClarify: &types.ClarifyRequest{
				ID:          "ask-1",
				Question:    "主题色选深色还是浅色？",
				Kind:        "choice",
				MultiSelect: true,
				Options: []types.ClarifyOption{
					{ID: "dark", Label: "深色"},
					{ID: "light", Label: "浅色"},
				},
			},
		},
		Events: []server.SessionEvent{
			{Type: "clarify", Agent: "System", Message: "Agent 提问: 主题色选深色还是浅色？", Timestamp: now},
		},
	}
	items := chatItems(s, true)
	if len(items) != 1 {
		t.Fatalf("应有 1 条 clarify 条目，got %d: %+v", len(items), titlesOf(items))
	}
	it := items[0]
	if !strings.HasPrefix(it.title, "❓ ") {
		t.Fatalf("条目标题应以 ❓ 开头，got %q", it.title)
	}
	if !strings.Contains(it.detail, "1. 深色") || !strings.Contains(it.detail, "2. 浅色") {
		t.Fatalf("detail 应包含 1/2 编号选项行，got %q", it.detail)
	}
	if !strings.Contains(it.detail, "可多选") {
		t.Fatalf("多选时 detail 应含多选提示，got %q", it.detail)
	}

	// 无 Options 的会话：detail 保持为空。
	s2 := &server.Session{
		Messages: []types.ChatMessage{},
		State: &types.ThreeLayerState{
			PendingClarify: &types.ClarifyRequest{ID: "ask-2", Question: "请补充说明"},
		},
		Events: []server.SessionEvent{
			{Type: "clarify", Agent: "System", Message: "Agent 提问: 请补充说明", Timestamp: now},
		},
	}
	items2 := chatItems(s2, true)
	if len(items2) != 1 {
		t.Fatalf("应有 1 条 clarify 条目，got %d", len(items2))
	}
	if items2[0].detail != "" {
		t.Fatalf("无 Options 时 detail 应为空，got %q", items2[0].detail)
	}
}

// TestSyncInputMode 验证输入栏模式随会话待澄清状态翻转：
// awaiting_clarify + PendingClarify → inputClarify；恢复运行 → inputNormal 并清空 clarifySel。
func TestSyncInputMode(t *testing.T) {
	m := &Model{
		sessionsCursor: 0,
		sessions: []*server.Session{{
			ID:     "s1",
			Status: enums.SessionStatusAwaitingClarify,
			State: &types.ThreeLayerState{
				PendingClarify: &types.ClarifyRequest{ID: "ask-1", Question: "继续？"},
			},
		}},
	}
	m.syncInputMode()
	if m.inputBar.mode != inputClarify {
		t.Fatalf("awaiting_clarify 时应置 inputClarify，got %d", m.inputBar.mode)
	}
	// 已处于 inputClarify 时重复调用保持不重复置（仍为 inputClarify）。
	m.syncInputMode()
	if m.inputBar.mode != inputClarify {
		t.Fatalf("已处于 inputClarify 时不应被重置，got %d", m.inputBar.mode)
	}

	// 会话恢复运行：模式回退 inputNormal，多选选择集清空。
	m.clarifySel = []string{"dark", "light"}
	m.sessions[0].Status = enums.SessionStatusRunning
	m.syncInputMode()
	if m.inputBar.mode != inputNormal {
		t.Fatalf("非 awaiting_clarify 时应回退 inputNormal，got %d", m.inputBar.mode)
	}
	if m.clarifySel != nil {
		t.Fatalf("离开澄清态后 clarifySel 应清空，got %v", m.clarifySel)
	}

	// 无 PendingClarify 的 awaiting_clarify：不进入澄清模式。
	m2 := &Model{
		sessionsCursor: 0,
		sessions: []*server.Session{{
			ID:     "s2",
			Status: enums.SessionStatusAwaitingClarify,
		}},
	}
	m2.syncInputMode()
	if m2.inputBar.mode != inputNormal {
		t.Fatalf("无 PendingClarify 时不应进入 inputClarify，got %d", m2.inputBar.mode)
	}
}
