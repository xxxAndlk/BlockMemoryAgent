package server

import (
	"context"       // 请求上下文
	"net/http"      // HTTP 状态码
	"time"          // 消息时间戳

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/agent" // Agent 门面
)

// HandleCreateSession 处理 POST /api/sessions。
// 职责：解析目标文本，调用 Agent 创建会话，返回会话快照。
func (m *SessionManager) HandleCreateSession(c *gin.Context) {
	// 解析请求体：goal 必填；images 可选（首条消息粘贴/上传的图片，与 /message 同规则限流）。
	req, err := DecodeBody[struct {
		Goal   string            `json:"goal"`
		Images []agent.WireImage `json:"images,omitempty"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Goal == "" {
		c.String(http.StatusBadRequest, "目标 (goal) 不能为空")
		return
	}
	// 用户图片限流（与 /message、TUI 粘贴侧同规则）：超限直接 400。
	images, err := agent.ParseWireImages(req.Images)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	// 调用 Agent 创建会话（images 经 firstTurnImages 注入首轮 runCtx 后一次性消费）。
	session, err := m.agent.CreateSession(c.Request.Context(), agent.CreateRequest{Goal: req.Goal, Images: images})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}

	c.JSON(http.StatusOK, ToServerSession(session))
}

// HandleGetSession 处理 GET /api/sessions/{id}。
// 职责：按 ID 返回会话快照。
func (m *SessionManager) HandleGetSession(c *gin.Context) {
	id := c.Param("id")

	session, err := m.agent.Get(c.Request.Context(), id)
	if err != nil {
		c.String(http.StatusNotFound, "会话不存在")
		return
	}

	c.JSON(http.StatusOK, ToServerSession(session))
}

// HandleListSessions 处理 GET /api/sessions。
// 职责：列出所有会话。
func (m *SessionManager) HandleListSessions(c *gin.Context) {
	sessions, err := m.agent.List(c.Request.Context(), agent.Filter{})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}

	all := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		all = append(all, ToServerSession(s))
	}

	c.JSON(http.StatusOK, all)
}

// HandleSessionBoard 处理 GET /api/sessions/{id}/board。
// 职责：返回会话的看板（board）数据。
func (m *SessionManager) HandleSessionBoard(c *gin.Context) {
	id := c.Param("id")

	if _, err := m.agent.Get(c.Request.Context(), id); err != nil {
		c.String(http.StatusNotFound, "会话不存在")
		return
	}

	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{Kind: agent.QueryKindBoard})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}

	c.JSON(http.StatusOK, map[string]any{
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
func (m *SessionManager) HandleSessionAgents(c *gin.Context) {
	id := c.Param("id")

	session, err := m.agent.Get(c.Request.Context(), id)
	if err != nil {
		c.String(http.StatusNotFound, "会话不存在")
		return
	}

	// 建立 domain -> goal / blockID 映射，便于后续填充。
	blockGoalByDomain := make(map[string]string)
	blockIDByDomain := make(map[string]string)
	for _, b := range session.ActiveBlocks {
		blockGoalByDomain[b.Domain] = b.Goal
		blockIDByDomain[b.Domain] = b.ID
	}

	instances, err := m.agent.ListAgents(c.Request.Context(), id)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
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

	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"agents":     nodes,
	})
}

// HandleSessionTopic 处理 POST /api/sessions/{id}/topic。
// 职责：切换/创建话题；非运行中会话会创建新会话来延续话题。
func (m *SessionManager) HandleSessionTopic(c *gin.Context) {
	id := c.Param("id")

	req, err := DecodeBody[struct {
		Name string `json:"name"`
		Goal string `json:"goal,omitempty"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Name == "" {
		c.String(http.StatusBadRequest, "话题名称 (name) 不能为空")
		return
	}

	// 优先使用专用的 SwitchTopic 方法，这样非运行中会话能正确返回新创建的对象。
	type topicSwitcher interface {
		SwitchTopic(ctx context.Context, sessionID, name, goal string) (*agent.Session, error)
	}

	var switched *agent.Session
	var topicErr error
	if ts, ok := m.agent.(topicSwitcher); ok {
		switched, topicErr = ts.SwitchTopic(c.Request.Context(), id, req.Name, req.Goal)
	} else {
		// 回退到通用 Control 命令。
		topicErr = m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
			Op: agent.ControlOpTopic,
			Args: map[string]any{
				"name": req.Name,
				"goal": req.Goal,
			},
		})
	}
	if topicErr != nil {
		msg, status := agentErrorStatus(topicErr)
		c.String(status, "%s", msg)
		return
	}

	if switched != nil && switched.ID != id {
		// 非运行中会话：已创建新会话来延续该话题。
		c.JSON(http.StatusOK, ToServerSession(switched))
	} else {
		c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "running", "topic": req.Name})
	}
}

// HandleSessionMessage 处理 POST /api/sessions/{id}/message。
// 职责：向会话发送用户消息，并返回最新会话快照。
func (m *SessionManager) HandleSessionMessage(c *gin.Context) {
	id := c.Param("id")

	req, err := DecodeBody[struct {
		Content string            `json:"content"`
		Images  []agent.WireImage `json:"images,omitempty"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Content == "" {
		c.String(http.StatusBadRequest, "内容不能为空")
		return
	}
	// 用户图片限流（与 TUI 粘贴侧同规则）：超限直接 400，防御直连 API 的调用方。
	images, err := agent.ParseWireImages(req.Images)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	if err := m.agent.Send(c.Request.Context(), id, agent.Message{Content: req.Content, Images: images, Timestamp: time.Now()}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	session, err := m.agent.Get(c.Request.Context(), id)
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, ToServerSession(session))
}

// HandleSessionTree 处理 GET /api/sessions/{id}/tree。
// 职责：返回会话的权威 Agent 树快照，按启动时间升序。
func (m *SessionManager) HandleSessionTree(c *gin.Context) {
	id := c.Param("id")
	nodes, err := m.agent.Tree(c.Request.Context(), id)
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"tree":       nodes,
	})
}

// HandleSessionAgentCancel 处理 POST /api/sessions/{id}/agents/{aid}/cancel。
// 职责：取消指定子 Agent 实例，调用其绑定的 cancel func。
func (m *SessionManager) HandleSessionAgentCancel(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	if err := m.agent.CancelAgent(c.Request.Context(), id, instID); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"agent_id":   instID,
		"status":     "cancelled",
	})
}
