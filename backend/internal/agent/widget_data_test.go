// Package agent 界面部件（Widget）瞬时数据通道的单元测试。
package agent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestPushWidgetData 钉死瞬时通道契约：写入最新值覆盖旧值、seq 递增、
// 快照携带 widget_id -> payload 全量 map（SSE 轮询与重连补档的数据源）、
// 会话不存在静默跳过、payload 超 64KB 截断。
func TestPushWidgetData(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)

	// 会话不存在：静默跳过，不 panic 不落任何状态。
	svc.PushWidgetData("ghost", "w", "p")

	svc.PushWidgetData(sess.ID, "wechat-qr", `{"n":1}`)
	svc.PushWidgetData(sess.ID, "wechat-qr", `{"n":2}`)
	svc.PushWidgetData(sess.ID, "news-card", `{"items":[]}`)

	// 内存态：最新值覆盖 + seq 递增。
	if e := sess.widgetData["wechat-qr"]; e.payload != `{"n":2}` || e.seq != 2 {
		t.Fatalf("最新值/seq 不符: %+v", e)
	}
	// DTO 快照：全量 map，只带 payload。
	dto := toReactAgentSession(svc.store.snapshotSession(sess))
	if len(dto.WidgetData) != 2 || dto.WidgetData["wechat-qr"] != `{"n":2}` || dto.WidgetData["news-card"] != `{"items":[]}` {
		t.Fatalf("快照 widget_data 不符: %+v", dto.WidgetData)
	}
	// 空 widgetID 忽略。
	svc.PushWidgetData(sess.ID, "", "x")
	if len(sess.widgetData) != 2 {
		t.Fatal("空 widgetID 不应写入")
	}

	// 超 64KB 截断。
	svc.PushWidgetData(sess.ID, "big", strings.Repeat("x", widgetPayloadMaxBytes+100))
	if got := sess.widgetData["big"].payload; len(got) != widgetPayloadMaxBytes {
		t.Fatalf("payload 应截断到 %d, got %d", widgetPayloadMaxBytes, len(got))
	}
}

// TestReportWidgetError 钉死错误回报契约：落 System 会话事件（用户可见）；
// 运行中会话额外注入邮箱（messenger 接线时）；终态不注入；会话不存在静默跳过。
func TestReportWidgetError(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)

	// 会话不存在：静默跳过。
	svc.ReportWidgetError("ghost", "w", "boom")

	svc.ReportWidgetError(sess.ID, "wechat-qr", "ReferenceError: x is not defined")

	var sysEv *internalEvent
	for i := range sess.Events {
		if sess.Events[i].Type == eventkind.System && strings.Contains(sess.Events[i].Message, "【部件报错】") {
			sysEv = &sess.Events[i]
		}
	}
	if sysEv == nil {
		t.Fatal("应落【部件报错】System 事件")
	}
	if !strings.Contains(sysEv.Message, "wechat-qr") || !strings.Contains(sysEv.Message, "ReferenceError") {
		t.Fatalf("事件应带部件 id 与错误文本: %q", sysEv.Message)
	}

	// messenger 未接线：不注入也不报错（降级只落 System 事件，上面已验证）。
	// 终态会话：即便 messenger 接线也不注入（无人消费）。
	sess.Status = enums.SessionStatusCompleted
	m := &fakeMessenger{}
	svc.SetAgentMessenger(m)
	svc.ReportWidgetError(sess.ID, "wechat-qr", "late error")
	if len(m.contents) != 0 {
		t.Fatalf("终态会话不应注入邮箱: %v", m.contents)
	}
	// 运行中会话：注入邮箱 FYI。
	sess.Status = enums.SessionStatusRunning
	svc.ReportWidgetError(sess.ID, "wechat-qr", "live error")
	if len(m.contents) != 1 || !strings.Contains(m.contents[0], "【部件报错】") {
		t.Fatalf("运行中应注入邮箱: %v", m.contents)
	}
}

// TestReportWidgetAction 钉死上行操作契约（仿 ReportWidgetError）：System 会话事件
// "【部件操作】" 用户可见；运行中会话注入邮箱；终态/不存在静默。
func TestReportWidgetAction(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)

	// 会话不存在：静默。
	svc.ReportWidgetAction("ghost", "w", "click", "{}")

	svc.ReportWidgetAction(sess.ID, "qr", "submit", `{"x":1}`)
	var sysEv *internalEvent
	for i := range sess.Events {
		if sess.Events[i].Type == eventkind.System && strings.Contains(sess.Events[i].Message, "【部件操作】") {
			sysEv = &sess.Events[i]
		}
	}
	if sysEv == nil {
		t.Fatal("应落【部件操作】System 事件")
	}
	if !strings.Contains(sysEv.Message, "qr") || !strings.Contains(sysEv.Message, "submit") {
		t.Fatalf("事件应带部件 id 与 action: %q", sysEv.Message)
	}

	// 运行中会话 + messenger 接线：注入邮箱；终态不注入。
	svc.SetAgentMessenger(&fakeMessenger{})
	m := &fakeMessenger{}
	svc.SetAgentMessenger(m)
	svc.ReportWidgetAction(sess.ID, "qr", "click", `{"ok":true}`)
	if len(m.contents) != 1 || !strings.Contains(m.contents[0], "【部件操作】") {
		t.Fatalf("运行中应注入邮箱: %v", m.contents)
	}
	sess.Status = enums.SessionStatusCompleted
	svc.ReportWidgetAction(sess.ID, "qr", "click", "{}")
	if len(m.contents) != 1 {
		t.Fatalf("终态会话不应注入邮箱: %v", m.contents)
	}
}
