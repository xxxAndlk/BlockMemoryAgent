package server

import (
	"context"       // 请求上下文
	"encoding/json" // JSON 编解码
	"net/http"      // HTTP 处理器与状态码
	"time"          // 消息时间戳

	"github.com/blockmemory/agent/backend/internal/agent" // Agent 门面
)

// HandleCreateSession 处理 POST /api/sessions。
// 职责：解析目标文本，调用 Agent 创建会话，返回会话快照。
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	// 解析请求体，仅需要 goal 字段。
	req, err := DecodeBody[struct {
		Goal string `json:"goal"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Goal == "" {
		http.Error(w, "目标 (goal) 不能为空", http.StatusBadRequest)
		return
	}

	// 调用 Agent 创建会话。
	session, err := m.agent.CreateSession(r.Context(), agent.CreateRequest{Goal: req.Goal})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}

// HandleGetSession 处理 GET /api/sessions/{id}。
// 职责：按 ID 返回会话快照。
func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}

// HandleListSessions 处理 GET /api/sessions。
// 职责：列出所有会话。
func (m *SessionManager) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := m.agent.List(r.Context(), agent.Filter{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	all := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		all = append(all, ToServerSession(s))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(all)
}

// HandleSessionBoard 处理 GET /api/sessions/{id}/board。
// 职责：返回会话的看板（board）数据。
func (m *SessionManager) HandleSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindBoard})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"board":      res.Data,
	})
}

// agentNode 是 /api/sessions/{id}/agents 接口的线型结构。
type agentNode struct {
	InstID    string `json:"inst_id"`            // Agent 实例 ID
	RoleDefID string `json:"role_def_id"`        // 角色定义 ID
	Name      string `json:"name"`               // Agent 名称
	Type      string `json:"type"`               // 角色类型
	Domain    string `json:"domain"`             // 所属领域
	Status    string `json:"status"`             // 当前状态
	ParentID  string `json:"parent_id"`          // 父节点 ID（当前固定为空）
	Goal      string `json:"goal,omitempty"`     // 目标（可选）
	BlockID   string `json:"block_id,omitempty"` // 所属 Block ID（可选）
}

// HandleSessionAgents 处理 GET /api/sessions/{id}/agents。
// 职责：列出会话中所有 Agent 实例，并补充其所属 Block 的目标与 ID。
func (m *SessionManager) HandleSessionAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	// 建立 domain -> goal / blockID 映射，便于后续填充。
	blockGoalByDomain := make(map[string]string)
	blockIDByDomain := make(map[string]string)
	for _, b := range session.ActiveBlocks {
		blockGoalByDomain[b.Domain] = b.Goal
		blockIDByDomain[b.Domain] = b.ID
	}

	instances, err := m.agent.ListAgents(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nodes := make([]agentNode, 0, len(instances))
	for _, inst := range instances {
		goal := ""
		blockID := ""
		if inst.Domain != "" {
			goal = blockGoalByDomain[inst.Domain]
			blockID = blockIDByDomain[inst.Domain]
		}
		nodes = append(nodes, agentNode{
			InstID:    inst.ModuleID,
			RoleDefID: inst.RoleDefID,
			Name:      inst.Name,
			Type:      inst.Role,
			Domain:    inst.Domain,
			Status:    inst.Status,
			ParentID:  "",
			Goal:      goal,
			BlockID:   blockID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"agents":     nodes,
	})
}

// HandleSessionTopic 处理 POST /api/sessions/{id}/topic。
// 职责：切换/创建话题；非运行中会话会创建新会话来延续话题。
func (m *SessionManager) HandleSessionTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	req, err := DecodeBody[struct {
		Name string `json:"name"`
		Goal string `json:"goal,omitempty"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "话题名称 (name) 不能为空", http.StatusBadRequest)
		return
	}

	// 优先使用专用的 SwitchTopic 方法，这样非运行中会话能正确返回新创建的对象。
	type topicSwitcher interface {
		SwitchTopic(ctx context.Context, sessionID, name, goal string) (*agent.Session, error)
	}

	var switched *agent.Session
	var topicErr error
	if ts, ok := m.agent.(topicSwitcher); ok {
		switched, topicErr = ts.SwitchTopic(r.Context(), id, req.Name, req.Goal)
	} else {
		// 回退到通用 Control 命令。
		topicErr = m.agent.Control(r.Context(), id, agent.ControlCommand{
			Op: agent.ControlOpTopic,
			Args: map[string]any{
				"name": req.Name,
				"goal": req.Goal,
			},
		})
	}
	if topicErr != nil {
		msg, status := agentErrorStatus(topicErr)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if switched != nil && switched.ID != id {
		// 非运行中会话：已创建新会话来延续该话题。
		json.NewEncoder(w).Encode(ToServerSession(switched))
	} else {
		json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running", "topic": req.Name})
	}
}

// HandleSessionMessage 处理 POST /api/sessions/{id}/message。
// 职责：向会话发送用户消息，并返回最新会话快照。
func (m *SessionManager) HandleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	req, err := DecodeBody[struct {
		Content string `json:"content"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	if err := m.agent.Send(r.Context(), id, agent.Message{Content: req.Content, Timestamp: time.Now()}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}
