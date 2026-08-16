package server

import (
	"encoding/json" // 会话快照与事件的 JSON 序列化
	"fmt"           // SSE 帧格式化输出
	"net/http"      // HTTP 处理器与状态码
	"time"          // 轮询 ticker 与时间戳

	"github.com/blockmemory/agent/backend/pkg/enums" // 会话状态枚举
	"github.com/blockmemory/agent/backend/pkg/types" // ClarifyOption（awaiting_clarify 帧结构化选项）
)

// HandleSessionStream 处理 GET /api/sessions/{id}/stream。
// 职责：建立 Server-Sent Events 长连接，周期性推送会话最新快照与新增事件，
// 直到会话结束或客户端断开。
func (m *SessionManager) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	// 先获取一次会话，确认存在；同时用于计算初始事件偏移。
	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	// 设置 SSE 响应头。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// SSE 是长连接：清除 server 级 WriteTimeout 对本连接设置的写截止时间，
	// 避免长任务下连接在 WriteTimeout（默认 30s）后被强制断开。
	// 客户端断开仍由 r.Context().Done() 感知，不受影响。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// 断言 http.Flusher，不支持流式则返回 500。
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式输出", http.StatusInternalServerError)
		return
	}

	// 推送初始完整快照。
	snapshot := ToServerSession(session)
	data, _ := json.Marshal(snapshot)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// 500ms 轮询一次会话状态。
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastEventCount := len(snapshot.Events)

	for {
		select {
		case <-ticker.C:
			// 重新拉取会话。
			session, err := m.agent.Get(r.Context(), id)
			if err != nil {
				return
			}

			snapshot = ToServerSession(session)
			currentEvents := snapshot.Events

			// 防止事件切片被重置导致下标越界。
			if lastEventCount > len(currentEvents) {
				lastEventCount = len(currentEvents)
			}

			// 推送新增事件。
			if len(currentEvents) > lastEventCount {
				for _, ev := range currentEvents[lastEventCount:] {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				lastEventCount = len(currentEvents)
				flusher.Flush()
			}

			// 若会话等待用户澄清，推送 awaiting_clarify 事件（TODO #53：帧携带结构化选项）。
			if snapshot.Status == enums.SessionStatusAwaitingClarify {
				pending := ""
				qid := ""
				var opts []types.ClarifyOption
				multi := false
				if snapshot.State != nil && snapshot.State.PendingClarify != nil {
					pc := snapshot.State.PendingClarify
					pending = pc.Question
					qid = pc.ID
					opts = pc.Options
					multi = pc.MultiSelect
				}
				frame := map[string]any{
					"type":        "awaiting_clarify",
					"status":      string(snapshot.Status),
					"question":    pending,
					"question_id": qid,
					"multi_select": multi,
				}
				if len(opts) > 0 {
					frame["options"] = opts
				}
				data, _ := json.Marshal(frame)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			// 会话结束且非等待澄清，推送 done 事件并关闭连接。
			if snapshot.Status != enums.SessionStatusRunning && snapshot.Status != enums.SessionStatusAwaitingClarify {
				data, _ := json.Marshal(map[string]string{"type": "done", "status": string(snapshot.Status)})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-r.Context().Done():
			// 客户端断开或请求被取消。
			return
		}
	}
}
