package server

import (
	"encoding/json" // JSON 编解码
	"log"           // 日志输出
	"net/http"      // HTTP 处理器
	"strings"       // 路径处理
	"time"          // 时间戳

	"github.com/blockmemory/agent/backend/internal/cmdqueue" // 用户指令队列（特性6）
	"github.com/blockmemory/agent/backend/pkg/enums"         // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types"         // 共享类型
)

// HandleSessionClarify 处理 POST /api/sessions/{id}/clarify，提交人机对话答复。
// 特性5：人机对话 — 用户答复 Agent 提出的澄清问题。
// 职责：将答复追加到会话消息，清空 PendingClarify，异步恢复 graph 执行。
// 副作用：修改 session.Messages / Status / State；异步启动 resumeSession。
func (m *SessionManager) HandleSessionClarify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/clarify")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Answer     string `json:"answer"`
		QuestionID string `json:"question_id"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Answer == "" {
		http.Error(w, "答复内容不能为空", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	if session.Status != enums.SessionStatusAwaitingClarify {
		m.mu.Unlock()
		http.Error(w, "会话未处于等待澄清状态", http.StatusBadRequest)
		return
	}
	// 追加用户答复到对话历史：前缀 "[澄清答复]" 让 LLM 在后续上下文中识别这是对悬停问题的回答
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   "[澄清答复] " + req.Answer,
		Timestamp: time.Now(),
	})
	// 清空 PendingClarify，避免 resumeSession 时被再次判定为挂起状态
	if session.State != nil {
		session.State.PendingClarify = nil
	}
	// 切回 running 让其他端点（interrupt/enqueue）知道会话已恢复可被抢占
	session.Status = enums.SessionStatusRunning
	// 直接 append 事件，避免 addEvent 再次取锁自死锁（此处已持 m.mu）
	session.Events = append(session.Events, SessionEvent{
		Type:      "clarify",
		Agent:     "User",
		Message:   "用户答复: " + req.Answer,
		Success:   true,
		Timestamp: time.Now(),
	})
	m.mu.Unlock()

	// P3-3：澄清答复后清理上一轮运行时残留，重建计划与 Agent 拓扑
	m.resetSessionRuntime(session.ID)
	// 异步恢复：避免阻塞 HTTP 响应；graph 从最新 state 继续，可能再次 ActionWait
	go m.resumeSession(session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"status":     "running",
	})
}

// HandleSessionInterrupt 处理 POST /api/sessions/{id}/interrupt，抢占中断。
// 特性6：抢占中断 — 用户暂停当前任务并以新指令重启。
// 职责：把新指令作为 IntentInterrupt 推入会话队列；若会话已完成则直接以新指令恢复执行。
// 副作用：写入 CmdQueue；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/interrupt")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	rt := m.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		http.Error(w, "命令队列不可用", http.StatusServiceUnavailable)
		return
	}
	// 先入队再判断会话状态，避免运行中会话被漏掉：MetaAgent 下个 tick Drain 时会拿到这条指令
	if err := rt.CmdQueue.Enqueue(id, cmdqueue.Item{Content: req.Content, Intent: cmdqueue.IntentInterrupt}); err != nil {
		log.Printf("HandleSessionInterrupt: queue full for session %s: %v", id, err)
		http.Error(w, "命令队列已满", http.StatusServiceUnavailable)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	// 直接 append 事件，避免 addEvent 再次取锁自死锁
	session.Events = append(session.Events, SessionEvent{
		Type:      "interrupt",
		Agent:     "User",
		Message:   "抢占中断: " + req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束的会话需重新置 running 并清空 EndedAt，否则 resumeSession 会因状态不对跳过
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	// 已结束会话：异步恢复执行，由 drainCommandQueue 在首 tick 应用中断；
	// 运行中会话：不主动 resume，等 MetaAgent 下一个 tick 自然拉取队列
	if !wasRunning {
		// P3-3：中断后清理上一轮运行时残留，确保新的计划与 Agent 拓扑从当前指令重建
		m.resetSessionRuntime(session.ID)
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionEnqueue 处理 POST /api/sessions/{id}/enqueue，队列注入消息。
// 特性6：队列注入 — 用户在任务执行中追加指令，不中断当前流程。
// 职责：把新指令作为 IntentEnqueue 推入会话队列；若会话已结束则按 /message 行为恢复。
// 副作用：写入 CmdQueue；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/enqueue")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	rt := m.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		http.Error(w, "命令队列不可用", http.StatusServiceUnavailable)
		return
	}
	// 与 interrupt 同序：先入队，再判断是否需要 resume；enqueue 不重置上下文，仅追加消息
	if err := rt.CmdQueue.Enqueue(id, cmdqueue.Item{Content: req.Content, Intent: cmdqueue.IntentEnqueue}); err != nil {
		log.Printf("HandleSessionEnqueue: queue full for session %s: %v", id, err)
		http.Error(w, "命令队列已满", http.StatusServiceUnavailable)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	// 直接 append 事件，避免 addEvent 再次取锁自死锁
	session.Events = append(session.Events, SessionEvent{
		Type:      "enqueue",
		Agent:     "User",
		Message:   "队列注入: " + req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束会话：enqueue 退化为普通 message 恢复，重新置 running
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	// 运行中会话：等 MetaAgent 下个 tick Drain；已结束会话：异步恢复
	if !wasRunning {
		// P3-3：队列注入恢复前清理上一轮运行时残留
		m.resetSessionRuntime(session.ID)
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionCancel 处理 POST /api/sessions/{id}/cancel，终止运行中的会话。
// 调用 cancelFn 取消 graph 执行的 context，graph.Invoke 收到 ctx.Done() 后
// 应尽快退出。会话状态被设为 error，Result 记录取消原因。
func (m *SessionManager) HandleSessionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/cancel")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if session.Status != enums.SessionStatusRunning {
		m.mu.Unlock()
		http.Error(w, "session is not running", http.StatusBadRequest)
		return
	}
	cancelFn := session.cancelFn
	session.cancelFn = nil
	session.Status = enums.SessionStatusError
	session.Result = "cancelled by user"
	now := time.Now()
	session.EndedAt = &now
	session.Events = append(session.Events, SessionEvent{
		Type:      "system",
		Agent:     "System",
		Message:   "会话已被用户取消",
		Success:   true,
		Timestamp: now,
	})
	m.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "error"})
}
