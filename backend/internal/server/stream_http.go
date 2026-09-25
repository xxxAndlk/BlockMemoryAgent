package server

import (
	"encoding/json" // 会话快照与事件的 JSON 序列化
	"fmt"           // SSE 帧格式化输出
	"net/http"      // HTTP 状态码与 Flusher
	"time"          // 轮询 ticker 与时间戳

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/pkg/enums" // 会话状态枚举
	"github.com/blockmemory/agent/backend/pkg/types" // ClarifyOption（awaiting_clarify 帧结构化选项）
)

// HandleSessionStream 处理 GET /api/sessions/{id}/stream。
// 职责：建立 Server-Sent Events 长连接，周期性推送会话最新快照与新增事件，
// 直到会话结束或客户端断开。
func (m *SessionManager) HandleSessionStream(c *gin.Context) {
	id := c.Param("id")

	// 先获取一次会话，确认存在；同时用于计算初始事件偏移。
	session, err := m.agent.Get(c.Request.Context(), id)
	if err != nil {
		c.String(http.StatusNotFound, "会话不存在")
		return
	}

	// 设置 SSE 响应头。
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// SSE 是长连接：清除 server 级 WriteTimeout 对本连接设置的写截止时间，
	// 避免长任务下连接在 WriteTimeout（默认 30s）后被强制断开。
	// gin 的 responseWriter 实现了 Unwrap()，NewResponseController 可穿透到底层
	// ResponseWriter；客户端断开仍由 r.Context().Done() 感知，不受影响。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// 断言 http.Flusher，不支持流式则返回 500。
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.String(http.StatusInternalServerError, "不支持流式输出")
		return
	}

	// 推送初始完整快照。
	snapshot := ToServerSession(session)
	data, _ := json.Marshal(snapshot)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// 模型实时文本（流式汇报/思考，同 TUI 轮询快照的 StreamingText/ThinkingText 源）上次推送值：变化才推 live 帧。
	lastStreamedText := snapshot.StreamingText
	lastThinkingText := snapshot.ThinkingText

	// 上次推送的会话状态：快照仅在连接建立时推一次，运行中状态翻转（running↔awaiting_clarify）
	// 前端无从感知——web 输入框答复路由依赖 status，状态滞后会把 ask_user 澄清答复误入
	// enqueue 通道（内容丢失 + 会话卡死）。状态变化即推 session_status 帧（2026-09-08）。
	lastPushedStatus := snapshot.Status

	// 150ms 轮询一次会话状态（2026-09-20：500ms 一档让流式输出最多滞后半秒一帧、
	// 跟手度差；内部 TUI 泵用 50ms，web 出口 150ms 兼顾实时性与 Get+序列化开销）。
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	lastEventCount := len(snapshot.Events)

	for {
		select {
		case <-ticker.C:
			// 重新拉取会话。
			session, err := m.agent.Get(c.Request.Context(), id)
			if err != nil {
				return
			}

			snapshot = ToServerSession(session)
			currentEvents := snapshot.Events

			// 事件切片被头部裁剪（addEvent 超 500 条时 trimDebugEvents 就地丢头部，
			// len 缩小）会让按索引的增量 diff 丢基线：直接把 lastEventCount 钳到 len
			// 会跳过裁剪同一窗口内追加的尾部事件（如最终 agent_done），前端永远
			// 收不到完成答复（2026-09-09 事故缺口 A）。此时推整帧快照，前端走既有
			// onSnapshot 原子替换路径，基线随之对齐，无事件丢失。
			if lastEventCount > len(currentEvents) {
				data, _ := json.Marshal(snapshot)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
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

			// 模型实时汇报/思考文本变化 → 推 live 帧（150ms tick 变化才推，零增量流量）。
			if snapshot.StreamingText != lastStreamedText || snapshot.ThinkingText != lastThinkingText {
				frame := map[string]string{
					"type":           "live",
					"streaming_text": snapshot.StreamingText,
					"thinking_text":  snapshot.ThinkingText,
				}
				data, _ := json.Marshal(frame)
				fmt.Fprintf(w, "data: %s\n\n", data)
				lastStreamedText = snapshot.StreamingText
				lastThinkingText = snapshot.ThinkingText
				flusher.Flush()
			}

			// 会话状态变化 → 推 session_status 帧（前端同步头部状态徽标与输入答复路由）。
			if snapshot.Status != lastPushedStatus {
				frame := map[string]string{
					"type":   "session_status",
					"status": string(snapshot.Status),
				}
				data, _ := json.Marshal(frame)
				fmt.Fprintf(w, "data: %s\n\n", data)
				lastPushedStatus = snapshot.Status
				flusher.Flush()
			}

			// 若会话等待用户澄清，推送 awaiting_clarify 事件（TODO #53：帧携带结构化选项；
			// 任务 140：批量模式增 questions 全量题目 + detail 长上下文，顶层 question/options
			// 继续镜像第一题保证旧客户端可渲染）。
			if snapshot.Status == enums.SessionStatusAwaitingClarify {
				pending := ""
				qid := ""
				detail := ""
				var opts []types.ClarifyOption
				multi := false
				var items []types.ClarifyQuestionItem
				var arts []types.ClarifyArtifact
				frameTimeoutSec := 0 // 提问答复剩余秒（deadline 现算，>0 才下发）
				if snapshot.State != nil && snapshot.State.PendingClarify != nil {
					pc := snapshot.State.PendingClarify
					pending = pc.Question
					qid = pc.ID
					opts = pc.Options
					multi = pc.MultiSelect
					detail = pc.Detail
					items = pc.Questions
					arts = pc.Artifacts
					if pc.Deadline != nil {
						// 剩余秒数服务端现算（前端时钟不可信）；到点工具侧兜底
						// "用户未答复，自行决策"，前端无需处理归零翻转。
						if sec := int(time.Until(*pc.Deadline).Seconds()); sec > 0 {
							frameTimeoutSec = sec
						}
					}
				}
				frame := map[string]any{
					"type":         "awaiting_clarify",
					"status":       string(snapshot.Status),
					"question":     pending,
					"question_id":  qid,
					"multi_select": multi,
				}
				if frameTimeoutSec > 0 {
					frame["timeout_sec"] = frameTimeoutSec
				}
				if len(opts) > 0 {
					frame["options"] = opts
				}
				if detail != "" {
					frame["detail"] = detail
				}
				if len(arts) > 0 {
					frame["artifacts"] = arts
				}
				if len(items) > 1 {
					qs := make([]map[string]any, 0, len(items))
					for _, q := range items {
						qf := map[string]any{
							"question":     q.Question,
							"multi_select": q.MultiSelect,
						}
						if len(q.Options) > 0 {
							qf["options"] = q.Options
						}
						qs = append(qs, qf)
					}
					frame["questions"] = qs
				}
				data, _ := json.Marshal(frame)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			// 真终态才推 done 并关闭连接（见 streamAlive）。
			if !streamAlive(snapshot.Status) {
				data, _ := json.Marshal(map[string]string{"type": "done", "status": string(snapshot.Status)})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-c.Request.Context().Done():
			// 客户端断开或请求被取消。
			return
		}
	}
}

// streamAlive 判断会话是否仍可能产生新事件（SSE 不该推 done 收口）。
//
// 只对**已知非终态**续流：running 继续跑、awaiting_clarify 等答复、awaiting_child 子完成
// 自动唤醒、paused_on_child 用户发消息续跑——后两者是"会话还没结束、随时会自己动起来"的
// 挂起态，其余（completed/error/未知状态）一律收口，保持旧契约。
//
// 教训（2026-09-25 用户实证）：旧条件只豁免 running/awaiting_clarify，Meta 一挂起等子就推
// done 关连接；前端收 done 视为终态（关 EventSource + 停面板定时器、不再重连），对话栏从
// 挂起那刻起永久收不到事件——头部徽标冻在「挂起等待子」、子完成唤醒后的最终答复永不渲染，
// 而侧栏列表走另一条刷新路却显示「完成」，两路分叉成自相矛盾的画面。挂起态必须保持推流。
func streamAlive(st enums.SessionStatus) bool {
	switch st {
	case enums.SessionStatusRunning,
		enums.SessionStatusAwaitingClarify,
		enums.SessionStatusAwaitingChild,
		enums.SessionStatusPausedOnChild:
		return true
	default:
		return false
	}
}
