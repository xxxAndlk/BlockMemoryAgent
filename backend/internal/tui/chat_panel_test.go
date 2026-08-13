package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestCollectItemsCapsHistory 验证主对话区只加载最近 maxChatItems 条记录：
// 更早的记录不进对话区（避免长会话滚动陷入历史），顶部以一条省略提示代替，
// 并指引用户用 Ctrl+L 查看完整记录。
func TestCollectItemsCapsHistory(t *testing.T) {
	now := time.Now()
	s := &server.Session{Status: enums.SessionStatusCompleted}
	total := maxChatItems + 30
	for i := 0; i < total; i++ {
		s.Messages = append(s.Messages, types.ChatMessage{
			Role:      enums.ChatRoleUser,
			Content:   fmt.Sprintf("question-%03d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		})
	}

	cp := NewChatPanel()
	items := cp.collectItems(s, nil)

	// 超过上限：保留最近 maxChatItems 条 + 顶部 1 条省略提示。
	if len(items) != maxChatItems+1 {
		t.Fatalf("超过上限时应为 %d 条（含省略提示），got %d", maxChatItems+1, len(items))
	}
	if !items[0].isEvent || !strings.Contains(items[0].title, "已省略 30 条") || !strings.Contains(items[0].title, "Ctrl+L") {
		t.Fatalf("首条应为省略提示（含省略条数与 Ctrl+L 指引），got %q", items[0].title)
	}
	// 保留段从 question-030 开始，到最新一条结束，最新内容始终可见。
	if !strings.Contains(items[1].title, "question-030") {
		t.Fatalf("保留段应从 question-030 开始，got %q", items[1].title)
	}
	if !strings.Contains(items[len(items)-1].title, fmt.Sprintf("question-%03d", total-1)) {
		t.Fatalf("末条应为最新消息，got %q", items[len(items)-1].title)
	}
}

// TestCollectItemsBelowCapKeepsAll 验证未超上限时全部记录原样保留，不出现省略提示。
func TestCollectItemsBelowCapKeepsAll(t *testing.T) {
	now := time.Now()
	s := &server.Session{Status: enums.SessionStatusCompleted}
	for i := 0; i < 5; i++ {
		s.Messages = append(s.Messages, types.ChatMessage{
			Role:      enums.ChatRoleUser,
			Content:   fmt.Sprintf("q%d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		})
	}

	cp := NewChatPanel()
	items := cp.collectItems(s, nil)

	if len(items) != 5 {
		t.Fatalf("未超上限时应全部保留，got %d", len(items))
	}
	if strings.Contains(items[0].title, "已省略") {
		t.Fatal("未超上限时不应出现省略提示")
	}
}
