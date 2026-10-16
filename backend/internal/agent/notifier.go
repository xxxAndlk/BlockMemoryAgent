package agent

// notifier.go 会话终态 Webhook 通知（TODO #18-5 T32）：
// 会话到达终态（completed/error 等，按 notify.webhook_events 过滤）时向
// notify.webhook_url best-effort POST 一条 JSON。3s 超时、失败仅记日志、
// 异步发送不阻塞会话收尾。webhook_url 为空时整体关闭（默认）。

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Notifier 会话终态 Webhook 通知器（零依赖，nil 安全）。
type Notifier struct {
	url     string          // 目标 URL；空=关闭
	events  map[string]bool // 关心的事件（会话终态值：completed/error/...）
	client  *http.Client    // 3s 超时独立客户端，不占用默认客户端连接池
	lastFir map[string]time.Time // sessionID -> 上次通知时间（同会话同状态 60s 去重防抖）
}

// NotifyWebhookPayload 终态通知报文（POST body）。
type NotifyWebhookPayload struct {
	Type       string `json:"type"`        // 固定 "session_final"
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`      // 终态值：completed / error / ...
	Goal       string `json:"goal"`        // 会话目标（截断防超大报文）
	OccurredAt string `json:"occurred_at"` // RFC3339
}

// NewNotifier 构造通知器。
// 参数 url：webhook 地址，空=整体关闭；events：关心的事件列表，空切片=关闭
//（显式与默认解耦——默认值由 config 层填充，这里不做隐式默认）。
func NewNotifier(url string, events []string) *Notifier {
	if url == "" || len(events) == 0 {
		return nil
	}
	set := make(map[string]bool, len(events))
	for _, e := range events {
		if e = trimNotifyEvent(e); e != "" {
			set[e] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return &Notifier{
		url:    url,
		events: set,
		client: &http.Client{Timeout: 3 * time.Second},
		lastFir: make(map[string]time.Time),
	}
}

// trimNotifyEvent 事件名去空白（配置容忍尾随空格）。
func trimNotifyEvent(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// NotifySessionFinal 会话终态通知入口（finalizeSession 调用；异步 best-effort）。
// 未接线 / 不关心该状态 / 60s 内重复通知时静默跳过。
func (n *Notifier) NotifySessionFinal(sessionID, status, goal string) {
	if n == nil || !n.events[status] {
		return
	}
	// 去重防抖：同会话**同状态** 60s 内重复终态（恢复重跑/竞态收尾）只发一次；
	// 不同状态（completed 后又 error 的异常路径）各自独立通知。
	key := sessionID + "\x00" + status
	if t, ok := n.lastFir[key]; ok && time.Since(t) < 60*time.Second {
		return
	}
	n.lastFir[key] = time.Now()
	if len(n.lastFir) > 256 { // 粗粒度容量回收，防长进程缓慢增长
		for k, t := range n.lastFir {
			if time.Since(t) >= 60*time.Second {
				delete(n.lastFir, k)
			}
		}
	}
	go n.post(NotifyWebhookPayload{
		Type:       "session_final",
		SessionID:  sessionID,
		Status:     status,
		Goal:       truncateNotifyRunes(goal, 200),
		OccurredAt: time.Now().Format(time.RFC3339),
	})
}

// NotifyRunFinalPayload 是工作流量产 run 的终态通知报文（POST body）。
type NotifyRunFinalPayload struct {
	Type       string `json:"type"` // 固定 "workflow_run_final"
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"` // done / failed / aborted
	RecipeID   string `json:"recipe_id"`
	Note       string `json:"note,omitempty"` // 补充说明（pause_reason 等）
	OccurredAt string `json:"occurred_at"` // RFC3339
}

// NotifyRunFinal 工作流量产 run 终态通知入口（workflow 引擎终态回调；异步 best-effort）。
// 过滤规则：webhook_events 命中 run 原始状态（done/failed/aborted）**或其会话语义别名**
//（done↔completed、failed↔error）即发——默认配置 [completed, error] 无需改动即可收到
// run 终态；60s 同 run 同状态去重。
func (n *Notifier) NotifyRunFinal(runID, sessionID, status, recipeID, note string) {
	if n == nil || !n.runFinalWanted(status) {
		return
	}
	key := "wf\x00" + runID + "\x00" + status
	if t, ok := n.lastFir[key]; ok && time.Since(t) < 60*time.Second {
		return
	}
	n.lastFir[key] = time.Now()
	if len(n.lastFir) > 256 {
		for k, t := range n.lastFir {
			if time.Since(t) >= 60*time.Second {
				delete(n.lastFir, k)
			}
		}
	}
	go n.postAny(NotifyRunFinalPayload{
		Type:       "workflow_run_final",
		RunID:      runID,
		SessionID:  sessionID,
		Status:     status,
		RecipeID:   recipeID,
		Note:       truncateNotifyRunes(note, 200),
		OccurredAt: time.Now().Format(time.RFC3339),
	})
}

// runFinalWanted run 终态是否在配置的关注集合内（原始状态或会话语义别名）。
func (n *Notifier) runFinalWanted(status string) bool {
	if n.events[status] {
		return true
	}
	switch status {
	case "done":
		return n.events["completed"]
	case "failed":
		return n.events["error"]
	}
	return false
}

// postAny 发送任意报文（post 的泛化版本，工作流通知复用同一 3s 客户端与日志口径）。
func (n *Notifier) postAny(p any) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	resp, err := n.client.Post(n.url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify] [WARN] webhook 发送失败: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify] [WARN] webhook 返回 %d", resp.StatusCode)
	}
}

// post 发送 HTTP POST；失败仅记日志（通知属 best-effort，不影响会话主流程）。
func (n *Notifier) post(p NotifyWebhookPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	resp, err := n.client.Post(n.url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify] [WARN] webhook 发送失败: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify] [WARN] webhook 返回 %d", resp.StatusCode)
	}
}

// truncateNotifyRunes 按 rune 截断（目标含中文）。
func truncateNotifyRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
