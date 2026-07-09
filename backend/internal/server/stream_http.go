package server

import (
	"encoding/json" // JSON 编解码
	"fmt"           // 格式化输出
	"net/http"      // HTTP 处理器
	"time"          // 轮询间隔

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
)

// HandleSessionStream 处理 GET /api/sessions/{id}/stream，SSE 实时事件流。
// SSE 长连接：先推送当前 session 全量快照，再轮询增量事件，直到会话结束或客户端断开。
// 副作用：阻塞当前 goroutine 直到 session 结束或客户端断开。
func (m *SessionManager) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream") // SSE 头
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*") // 跨域

	flusher, ok := w.(http.Flusher) // 断言 Flusher 接口
	if !ok {
		http.Error(w, "不支持流式输出", http.StatusInternalServerError)
		return
	}

	// 先发送当前状态（持锁快照 Events 避免与 addEventDebug 并发 append 产生 race，T4 修复）
	snapshot := m.snapshotSession(session)
	data, _ := json.Marshal(snapshot)    // 序列化当前 session 快照
	fmt.Fprintf(w, "data: %s\n\n", data) // 写入 SSE 帧
	flusher.Flush()

	// 轮询更新
	ticker := time.NewTicker(500 * time.Millisecond) // 500ms 轮询一次
	defer ticker.Stop()

	lastEventCount := len(snapshot.Events) // 记录上次推送的事件数

	for {
		select {
		case <-ticker.C:
			session = m.GetSession(id) // 重新查询（可能已被回收）
			if session == nil {
				return
			}

			// 持锁快照当前 Events（T4 修复：原无锁读 len + 切片，
			// 与 addEventDebug 的 append / trimDebugEvents 的前删产生竞态，
			// trim 后 lastEventCount 可能超过新 len 导致切片负长度 panic）
			snapshot = m.snapshotSession(session)
			currentEvents := snapshot.Events

			// trimDebugEvents 会从前端删除调试事件，导致 lastEventCount 超过新 len。
			// 此时应重置水位为当前长度（已删事件不可补推），而非切片 panic。
			if lastEventCount > len(currentEvents) {
				lastEventCount = len(currentEvents) // 重置到尾部，只推后续新事件
			}

			if len(currentEvents) > lastEventCount {
				// 推送增量事件（从快照拷贝中读，无锁竞争）
				for _, ev := range currentEvents[lastEventCount:] {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				lastEventCount = len(currentEvents) // 更新水位
				flusher.Flush()
			}

			// awaiting_clarify：会话挂起等待用户答复，推送 clarify 事件但保持流连接，
			// 让前端感知需要输入且能继续接收后续恢复后的事件（H8 修复：原实现统一发 done 退出，
			// 前端误认为会话终结，无法呈现澄清输入框）。
			if session.Status == enums.SessionStatusAwaitingClarify {
				pending := ""
				qid := ""
				if session.State != nil && session.State.PendingClarify != nil {
					pending = session.State.PendingClarify.Question
					qid = session.State.PendingClarify.ID
				}
				data, _ := json.Marshal(map[string]string{
					"type":        "awaiting_clarify",
					"status":      string(session.Status),
					"question":    pending,
					"question_id": qid,
				})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				// 不 return：保持 SSE 连接，等用户提交 clarify 后会话恢复 Running 继续推送
			}

			if session.Status != enums.SessionStatusRunning && session.Status != enums.SessionStatusAwaitingClarify {
				// 仅在终态（completed/error/cancelled）推送 done 并退出
				data, _ := json.Marshal(map[string]string{"type": "done", "status": string(session.Status)})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-r.Context().Done(): // 客户端断开
			return
		}
	}
}
