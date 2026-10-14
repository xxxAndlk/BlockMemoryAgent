package agent

// attachments_test.go 用户消息文件附件（2026-10-14）用例：
//   - AppendAttachmentRefs / MarshalAttachmentsJSON 拼装工具（格式、空短路、大小档位）；
//   - create 与 send 两条路径把结构化附件挂进 user_message 事件的 detail_json。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestAppendAttachmentRefs 引用行拼装：空列表原样返回；非空在尾部加块（含大小档位）。
func TestAppendAttachmentRefs(t *testing.T) {
	if got := AppendAttachmentRefs("原文", nil); got != "原文" {
		t.Fatalf("空附件应原样返回，got %q", got)
	}
	atts := []WireAttachment{
		{Name: "data.sql", Path: ".bma/uploads/data.sql", Size: 2 << 20},
		{Name: "表.xlsx", Path: ".bma/uploads/表.xlsx", Size: 512},
	}
	got := AppendAttachmentRefs("分析这些文件", atts)
	want := "分析这些文件\n\n[附件 1: data.sql | .bma/uploads/data.sql | 2.0 MB]\n" +
		"[附件 2: 表.xlsx | .bma/uploads/表.xlsx | 512 B]"
	if got != want {
		t.Fatalf("拼装不符:\ngot  %q\nwant %q", got, want)
	}
	// 空正文（只带文件不发文字）：只返回引用块；KB 档格式。
	got = AppendAttachmentRefs("", []WireAttachment{{Name: "a.sql", Path: ".bma/uploads/a.sql", Size: 1536}})
	if got != "[附件 1: a.sql | .bma/uploads/a.sql | 1.5 KB]" {
		t.Fatalf("空正文应只返回引用块（KB 档），got %q", got)
	}
}

// TestMarshalAttachmentsJSON detail_json 载荷：空列表返回空串（走无 detail 的
// addEvent 路径）；非空可解析且字段齐全。
func TestMarshalAttachmentsJSON(t *testing.T) {
	if got := MarshalAttachmentsJSON(nil); got != "" {
		t.Fatalf("空列表应返回空串，got %q", got)
	}
	raw := MarshalAttachmentsJSON([]WireAttachment{{Name: "a.sql", Path: ".bma/uploads/a.sql", Size: 7}})
	var payload struct {
		Attachments []WireAttachment `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("载荷应可解析: %v (%s)", err, raw)
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Path != ".bma/uploads/a.sql" || payload.Attachments[0].Size != 7 {
		t.Fatalf("载荷不符: %+v", payload)
	}
}

// TestCreateSessionAttachmentsIntoDetailJSON create 路径：首条 user_message 事件
// 的 detail_json 含结构化附件；firstTurnAttachments 一次性消费。
func TestCreateSessionAttachmentsIntoDetailJSON(t *testing.T) {
	svc := newAskUserTestService(t, &mockReactModelProvider{})
	ctx := context.Background()
	atts := []WireAttachment{{Name: "data.sql", Path: ".bma/uploads/data.sql", Size: 2048}}
	created, err := svc.CreateSession(ctx, CreateRequest{
		// Goal 由 server 层拼好（此处模拟含引用文本的持久形态）。
		Goal:        "分析文件\n\n[附件 1: data.sql | .bma/uploads/data.sql | 2.0 KB]",
		Attachments: atts,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.store.mu.RLock()
	sess := svc.store.sessions[created.ID]
	svc.store.mu.RUnlock()
	if sess == nil {
		t.Fatal("会话应已在 store 中")
	}
	ev := waitForSessionEvent(t, svc, sess, 5*time.Second, func(ev internalEvent) bool {
		return ev.Type == eventkind.UserMessage
	})
	if !strings.Contains(ev.DetailJSON, `"attachments"`) || !strings.Contains(ev.DetailJSON, ".bma/uploads/data.sql") {
		t.Fatalf("首条 user_message 事件 detail_json 应含附件: %q", ev.DetailJSON)
	}
	svc.store.mu.RLock()
	leftover := sess.firstTurnAttachments
	svc.store.mu.RUnlock()
	if len(leftover) != 0 {
		t.Fatalf("firstTurnAttachments 应一次性消费, got %+v", leftover)
	}
}

// TestSendMessageAttachmentsIntoDetailJSON send 路径：user_message 事件 detail_json
// 含结构化附件（主路径事件在 Send 返回前同步落盘，resume 才是异步）。
func TestSendMessageAttachmentsIntoDetailJSON(t *testing.T) {
	svc := newAskUserTestService(t, &mockReactModelProvider{})
	ctx := context.Background()
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "x"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等首轮跑完，避免并发事件干扰断言（同 video 接线用例模式）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := svc.Get(ctx, created.ID); s != nil && s.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	atts := []WireAttachment{{Name: "a.csv", Path: ".bma/uploads/a.csv", Size: 7}}
	if err := svc.Send(ctx, created.ID, Message{
		Content:     "看附件\n\n[附件 1: a.csv | .bma/uploads/a.csv | 7 B]",
		Attachments: atts,
		Timestamp:   time.Now(),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	svc.store.mu.RLock()
	sess := svc.store.sessions[created.ID]
	var found *internalEvent
	for i := range sess.Events {
		if sess.Events[i].Type == eventkind.UserMessage && strings.Contains(sess.Events[i].Message, "看附件") {
			found = &sess.Events[i]
			break
		}
	}
	svc.store.mu.RUnlock()
	if found == nil {
		t.Fatal("应落带附件的 user_message 事件")
	}
	if !strings.Contains(found.DetailJSON, `"path":".bma/uploads/a.csv"`) || !strings.Contains(found.DetailJSON, `"size":7`) {
		t.Fatalf("事件 detail_json 应含附件结构: %q", found.DetailJSON)
	}
}
