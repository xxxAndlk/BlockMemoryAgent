package server

import (
	"context"       // 请求上下文与超时
	"encoding/json" // JSON 编解码
	"fmt"           // 格式化输出
	"log"           // 日志输出
	"net/http"      // HTTP 处理器
	"sort"          // 会话列表按时间排序
	"strings"       // 字符串处理
	"time"          // 时间戳

	"github.com/blockmemory/agent/backend/internal/cmdqueue" // 用户指令队列（特性6）
	"github.com/blockmemory/agent/backend/pkg/enums"         // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types"         // 共享类型
)

// HandleCreateSession 处理 POST /api/sessions，创建并启动新会话。
// 解析 goal 并创建会话，返回 Session 对象。
// 参数：w / r - HTTP 标准参数。
// 副作用：创建会话并异步启动 runSession。
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Goal string `json:"goal"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Goal == "" {
		http.Error(w, "目标 (goal) 不能为空", http.StatusBadRequest)
		return
	}

	session := m.CreateSession(context.Background(), req.Goal) // 创建并异步启动

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session) // 返回 Session JSON
}

// HandleGetSession 处理 GET /api/sessions/{id}，返回单个会话详情。
// 先查内存，未命中且配置了 Postgres 时回退到 session_history 表，返回最小记录。
func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sessions/"):] // 截取 id
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session := m.GetSession(id)
	if session == nil && m.pgStore != nil {
		// 内存已淘汰，从 DB 恢复最小记录（无 events 流）
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if rec, err := m.pgStore.GetSessionHistoryByID(ctx, id); err == nil && rec != nil {
			endedAt := rec.CreatedAt
			session = &Session{
				ID:        rec.SessionID,
				Goal:      rec.Goal,
				Status:    enums.SessionStatusCompleted,
				Result:    rec.Summary,
				StartedAt: rec.CreatedAt,
				EndedAt:   &endedAt,
				Events:    make([]SessionEvent, 0),
				Messages: []types.ChatMessage{
					{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
					{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
				},
			}
		}
	}
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// HandleListSessions 处理 GET /api/sessions，返回会话列表。
// 返回会话列表：内存中的运行中 + 最近完成会话，叠加 Postgres 中更早的历史会话。
// 内存未命中但 DB 有记录的会话以最小形态返回（goal/summary/result/time，无 events）。
func (m *SessionManager) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	memSessions := m.ListSessions() // 拷贝切片，已释放锁
	seen := make(map[string]bool, len(memSessions))
	for _, s := range memSessions {
		seen[s.ID] = true
	}

	var all []*Session
	all = append(all, memSessions...)

	if m.pgStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		recs, err := m.pgStore.RecentSessionHistories(ctx, 200)
		if err == nil {
			for _, rec := range recs {
				if seen[rec.SessionID] {
					continue // 内存已有，跳过 DB 版本
				}
				endedAt := rec.CreatedAt
				all = append(all, &Session{
					ID:        rec.SessionID,
					Goal:      rec.Goal,
					Status:    enums.SessionStatusCompleted,
					Result:    rec.Summary,
					StartedAt: rec.CreatedAt,
					EndedAt:   &endedAt,
					Events:    make([]SessionEvent, 0),
					Messages: []types.ChatMessage{
						{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
						{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
					},
				})
			}
		} else {
			log.Printf("列出会话失败(数据库回退): %v", err)
		}
	}

	// 按 StartedAt 倒序，最新在前
	sort.Slice(all, func(i, j int) bool {
		return all[i].StartedAt.After(all[j].StartedAt)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(all)
}

// HandleSessionBoard 处理 GET /api/sessions/{id}/board，返回任务看板。
// 返回该会话的 TaskBoard 快照（领域子任务、约束、状态）。若无则返回 null。
func (m *SessionManager) HandleSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/board") // 剥离 /board 后缀

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	var snap any
	if rt := m.graph.Runtime(); rt != nil && rt.Boards != nil {
		if b := rt.Boards.Get(id); b != nil {
			snap = b.Snapshot() // 取 TaskBoard 快照
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"board":      snap, // 没有则 nil
	})
}

// agentNode 是给前端用的扁平+树结构节点，含 RoleDefinition 名称与父子关系
type agentNode struct {
	InstID    string `json:"inst_id"`            // 实例 ID
	RoleDefID string `json:"role_def_id"`        // 角色定义 ID
	Name      string `json:"name"`               // 角色名称
	Type      string `json:"type"`               // 实例类型（meta / domain / subdomain / assistant）
	Domain    string `json:"domain"`             // 所属领域
	Status    string `json:"status"`             // 实例状态
	ParentID  string `json:"parent_id"`          // 父实例 ID（树结构）
	Goal      string `json:"goal,omitempty"`     // Block 目标
	BlockID   string `json:"block_id,omitempty"` // 所属 SessionBlock ID
}

// HandleSessionAgents 处理 GET /api/sessions/{id}/agents，返回会话内角色实例。
// 返回该会话所有 RoleInstance（带 RoleDefinition 名称），并附上对应 SessionBlock 的目标。
func (m *SessionManager) HandleSessionAgents(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/agents") // 剥离 /agents 后缀

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	// 收集 block 中 domain->goal 映射，便于按 domain 回填 goal
	blockGoalByDomain := make(map[string]string)
	blockIDByDomain := make(map[string]string)
	if session.State != nil {
		for _, b := range session.State.ActiveBlocks {
			blockGoalByDomain[b.Domain] = b.Goal // domain -> goal
			blockIDByDomain[b.Domain] = b.ID     // domain -> blockID
		}
	}

	instances := m.registry.GetInstancesBySession(id) // 查询所有实例
	nodes := make([]agentNode, 0, len(instances))
	for _, inst := range instances {
		name := "unknown"
		if def := m.registry.GetRoleDef(inst.RoleDefID); def != nil {
			name = def.Name // 取角色定义名
		}
		goal := ""
		blockID := ""
		if inst.Domain != "" {
			goal = blockGoalByDomain[inst.Domain]  // 按 domain 回填 goal
			blockID = blockIDByDomain[inst.Domain] // 按 domain 回填 blockID
		}
		nodes = append(nodes, agentNode{
			InstID:    inst.ID,
			RoleDefID: inst.RoleDefID,
			Name:      name,
			Type:      string(inst.Type),
			Domain:    inst.Domain,
			Status:    string(inst.Status),
			ParentID:  inst.ParentID,
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

// HandleSessionTopic 处理 POST /api/sessions/{id}/topic，切换或创建话题（SessionBlock）。
// 运行中会话：通过命令队列触发 MetaAgent 以新话题重新启动；非运行中会话：创建新会话。
func (m *SessionManager) HandleSessionTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/topic")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Name string `json:"name"`
		Goal string `json:"goal,omitempty"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "话题名称 (name) 不能为空", http.StatusBadRequest)
		return
	}
	goal := req.Goal
	if goal == "" {
		goal = req.Name
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	wasRunning := session.Status == enums.SessionStatusRunning
	oldDomain := ""
	if session.State != nil {
		oldDomain = session.State.CurrentDomain
	}
	// 记录话题切换事件
	session.Events = append(session.Events, SessionEvent{
		Type:      "progress",
		Agent:     "User",
		Message:   fmt.Sprintf("切换话题: 从 [%s] 到 [%s]", oldDomain, req.Name),
		Kind:      "topic_switch",
		Success:   true,
		Timestamp: time.Now(),
	})
	m.mu.Unlock()

	if wasRunning {
		rt := m.graph.Runtime()
		if rt != nil && rt.CmdQueue != nil {
			if err := rt.CmdQueue.Enqueue(id, cmdqueue.Item{Content: goal, Intent: cmdqueue.IntentInterrupt}); err != nil {
				log.Printf("HandleSessionTopicSwitch: queue full for session %s: %v", id, err)
				http.Error(w, "命令队列已满", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running", "topic": req.Name})
		return
	}

	// 非运行中：创建新会话继续该话题
	newSession := m.CreateSession(r.Context(), goal)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newSession)
}

// HandleSessionMessage 处理 POST /api/sessions/{id}/message，向会话追加用户消息。
// 向会话追加用户消息；若会话已结束则恢复执行。
// 副作用：修改 session.Messages / Status / EndedAt；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/api/sessions/"):]
	id = id[:len(id)-len("/message")] // 剥离 /message 后缀
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

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		// 会话不在内存中 — 从数据库恢复完整会话（含工具调用过程）。
		revived := m.reviveFromHistory(id)
		if revived == nil {
			http.Error(w, "会话不存在", http.StatusNotFound)
			return
		}
		m.mu.Lock()
		session = revived
	} else if session.Status == enums.SessionStatusCompleted && len(session.Events) == 0 && session.Result != "" {
		// 启动时 RestoreSessions 加载的最小历史会话（无 Events），
		// 续话前需替换为完整版本（含工具调用过程），否则 LLM 看不到历史工具结果会重复调用。
		m.mu.Unlock()
		revived := m.reviveFromHistory(id)
		m.mu.Lock()
		if revived != nil {
			session = revived
		}
	}

	// 追加用户消息与事件（直接 append，避免调用 addEvent 再次取锁自死锁）
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   req.Content,
		Timestamp: time.Now(),
	})
	session.Events = append(session.Events, SessionEvent{
		Type:      "user_message",
		Agent:     "User",
		Message:   req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})

	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束会话：重新激活并异步恢复
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	if !wasRunning {
		// P3-3：续话前清理上一轮运行时残留，确保右侧面板展示新的计划与 Agent 拓扑
		m.resetSessionRuntime(session.ID)
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}
