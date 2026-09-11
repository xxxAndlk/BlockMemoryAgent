package server

import (
	"net/http" // HTTP 状态码

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/agent" // Agent 门面与控制命令
)

// HandleSessionClarify 处理 POST /api/sessions/{id}/clarify。
// 职责：接收用户对澄清问题的回答，转发给 Agent 继续会话。
func (m *SessionManager) HandleSessionClarify(c *gin.Context) {
	id := c.Param("id")

	req, err := DecodeBody[struct {
		Answer     string   `json:"answer"`
		Answers    []string `json:"answers"` // 批量逐题答复（任务 140，下标与题目对齐）
		QuestionID string   `json:"question_id"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Answer == "" && len(req.Answers) == 0 {
		c.String(http.StatusBadRequest, "答复内容不能为空")
		return
	}

	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
		Op: agent.ControlOpClarify,
		Args: map[string]any{
			"answer":      req.Answer,
			"answers":     req.Answers,
			"question_id": req.QuestionID,
		},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"status":     "running",
	})
}

// HandleSessionInterrupt 处理 POST /api/sessions/{id}/interrupt。
// 职责：向会话发送中断内容，打断当前 Agent 执行。
func (m *SessionManager) HandleSessionInterrupt(c *gin.Context) {
	id := c.Param("id")
	req, err := DecodeBody[struct {
		Content string `json:"content"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Content == "" {
		c.String(http.StatusBadRequest, "内容不能为空")
		return
	}

	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpInterrupt,
		Args: map[string]any{"content": req.Content},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionEnqueue 处理 POST /api/sessions/{id}/enqueue。
// 职责：将用户内容入队，供会话后续处理。
func (m *SessionManager) HandleSessionEnqueue(c *gin.Context) {
	id := c.Param("id")
	req, err := DecodeBody[struct {
		Content string `json:"content"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Content == "" {
		c.String(http.StatusBadRequest, "内容不能为空")
		return
	}

	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpEnqueue,
		Args: map[string]any{"content": req.Content},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionStop 处理 POST /api/sessions/{id}/stop（TODO #37 软停止）。
// 与 /cancel 的区别：不销毁——停止当前会话全部子任务（domain 落 Paused 可续跑、
// 叶子部分回灌），会话转入 PausedOnChild；倒计时内任意消息续跑，到期未续跑硬销毁。
func (m *SessionManager) HandleSessionStop(c *gin.Context) {
	id := c.Param("id")

	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{Op: agent.ControlOpStop}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "stopping"})
}

// HandleSessionCancel 处理 POST /api/sessions/{id}/cancel。
// 职责：取消会话当前任务。
func (m *SessionManager) HandleSessionCancel(c *gin.Context) {
	id := c.Param("id")

	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{Op: agent.ControlOpCancel}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}

	c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "error"})
}
