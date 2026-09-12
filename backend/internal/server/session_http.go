package server

import (
	"context"       // 请求上下文
	"net/http"      // HTTP 状态码
	"os"            // work_dir 目录存在性校验
	"path/filepath" // work_dir 转绝对路径
	"strconv"       // 审计事件分页参数解析
	"strings"       // 合并门 action 归一化
	"time"          // 消息时间戳

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/agent" // Agent 门面
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// HandleCreateSession 处理 POST /api/sessions。
// 职责：解析目标文本，调用 Agent 创建会话，返回会话快照。
// workDirAbs 校验并规范化 work_dir 参数（创建会话 / 修改会话目录 / 项目偏好三个入口
// 共用同一规则，避免三份副本各自漂移）：空串返回 ("", true)，语义为"回落进程默认目录"；
// 非空转绝对路径，不存在或非目录时写 400 响应并返回 ok=false。
func workDirAbs(c *gin.Context, dir string) (string, bool) {
	if dir == "" {
		return "", true
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		c.String(http.StatusBadRequest, "work_dir 无效")
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		c.String(http.StatusBadRequest, "work_dir 不存在或不是目录")
		return "", false
	}
	return abs, true
}

func (m *SessionManager) HandleCreateSession(c *gin.Context) {
	// 解析请求体：goal 必填；images 可选（首条消息粘贴/上传的图片，与 /message 同规则限流）；
	// videos 可选（首条消息粘贴的视频文件路径，服务端抽帧后走图片链路）；
	// work_dir 可选（每会话工作目录，绝对/相对均转绝对）。
	req, err := DecodeBody[struct {
		Goal    string            `json:"goal"`
		Images  []agent.WireImage `json:"images,omitempty"`
		Videos  []agent.WireVideo `json:"videos,omitempty"`
		WorkDir string            `json:"work_dir,omitempty"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Goal == "" {
		c.String(http.StatusBadRequest, "目标 (goal) 不能为空")
		return
	}
	// work_dir 校验：转绝对路径，不存在或非目录直接 400（在到达 agent 前拦截）。
	abs, ok := workDirAbs(c, req.WorkDir)
	if !ok {
		return
	}
	req.WorkDir = abs
	// 用户图片限流（与 /message、TUI 粘贴侧同规则）：超限直接 400。
	images, err := agent.ParseWireImages(req.Images)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	// 用户视频限流：扩展名白名单/文件存在/大小上限，超限直接 400。
	videos, err := agent.ParseWireVideos(req.Videos, 0)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	// 调用 Agent 创建会话（images/videos 经 firstTurnImages 注入首轮 runCtx 后一次性消费）。
	session, err := m.agent.CreateSession(c.Request.Context(), agent.CreateRequest{Goal: req.Goal, Images: images, Videos: videos, WorkDir: req.WorkDir})
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

// HandleDeleteSession 处理 DELETE /api/sessions/{id}。
// 职责：硬删除单个会话（不可恢复）：运行中先终止执行，再物理删除持久化数据。
func (m *SessionManager) HandleDeleteSession(c *gin.Context) {
	id := c.Param("id")
	if err := m.agent.DeleteSession(c.Request.Context(), id); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"session_id": id, "status": "deleted"})
}

// maxBatchDeleteSessions 是批量删除单次请求的会话数上限（防误贴超大列表打爆存储）。
const maxBatchDeleteSessions = 200

// HandleDeleteSessions 处理 POST /api/sessions/delete。
// 职责：批量硬删除（body {"ids":[...]}）。逐条独立处理——单条失败不影响其余，
// 返回 {"deleted":[...], "errors":[{"id","error"}]}，前端据此提示部分成功。
func (m *SessionManager) HandleDeleteSessions(c *gin.Context) {
	req, err := DecodeBody[struct {
		IDs []string `json:"ids"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if len(req.IDs) == 0 {
		c.String(http.StatusBadRequest, "ids 不能为空")
		return
	}
	if len(req.IDs) > maxBatchDeleteSessions {
		c.String(http.StatusBadRequest, "单次最多删除 %d 个会话", maxBatchDeleteSessions)
		return
	}
	deleted := make([]string, 0, len(req.IDs))
	type deleteFailure struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	failures := make([]deleteFailure, 0)
	for _, id := range req.IDs {
		if id == "" {
			continue
		}
		if err := m.agent.DeleteSession(c.Request.Context(), id); err != nil {
			failures = append(failures, deleteFailure{ID: id, Error: err.Error()})
			continue
		}
		deleted = append(deleted, id)
	}
	c.JSON(http.StatusOK, map[string]any{"deleted": deleted, "errors": failures})
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
	ParentID  string `json:"parent_id"`          // 父节点 ID（空表示顶层）
	Goal      string `json:"goal,omitempty"`     // 目标（可选）
	BlockID   string `json:"block_id,omitempty"` // 所属 Block ID（可选）
	// 活动证据（TODO 第10项②展示面）：最近活动种类与距今时长，前端渲染
	// "in <tool> · active Xs ago" 小字；空串表示无监控条目/非运行态。
	ActivityKind    string `json:"activity_kind,omitempty"`
	LastActivityAgo string `json:"last_activity_ago,omitempty"`
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
		goal := inst.Goal
		blockID := ""
		if inst.Domain != "" {
			if g, ok := blockGoalByDomain[inst.Domain]; ok && g != "" {
				goal = g
			}
			blockID = blockIDByDomain[inst.Domain]
		}
		nodes = append(nodes, agentNode{
			InstID:          inst.ModuleID,
			RoleDefID:       inst.RoleDefID,
			Name:            inst.Name,
			Type:            inst.Role,
			Domain:          inst.Domain,
			Status:          inst.Status,
			ParentID:        inst.ParentID,
			Goal:            goal,
			BlockID:         blockID,
			ActivityKind:    inst.ActivityKind,
			LastActivityAgo: inst.LastActivityAgo,
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
		Videos  []agent.WireVideo `json:"videos,omitempty"`
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
	// 用户视频限流（与 TUI 粘贴侧同规则）：扩展名白名单/文件存在/大小上限。
	videos, err := agent.ParseWireVideos(req.Videos, 0)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	if err := m.agent.Send(c.Request.Context(), id, agent.Message{Content: req.Content, Images: images, Videos: videos, Timestamp: time.Now()}); err != nil {
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

// HandleSessionTrustMode 处理 POST /api/sessions/{id}/trust-mode（TODO 第10⑥）。
// 职责：切换会话信任模式（suggest|auto-edit|full-auto），经 Control 通道下发；
// atomic 即时生效——正在阻塞的 ReAct 循环下一次工具派发按新模式裁决。
// 非法枚举 400；会话不存在 404。
func (m *SessionManager) HandleSessionTrustMode(c *gin.Context) {
	id := c.Param("id")
	req, err := DecodeBody[struct {
		Mode string `json:"mode"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if !tool.ValidTrustMode(req.Mode) {
		c.String(http.StatusBadRequest, "mode 非法（want suggest|auto-edit|full-auto）")
		return
	}
	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpTrustMode,
		Args: map[string]any{"mode": req.Mode},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"trust_mode": req.Mode,
	})
}

// HandleSessionAgentPause 处理 POST /api/sessions/{id}/agents/{aid}/pause
// （TODO 第10项③ 审计面手动止血）。
// 职责：暂停指定 domain 支路（dispatcher Pause 收尾，热驻槽 parked 可 resume 续跑）。
// 仅 domain + Running 可暂停；叶子用 cancel 端点，全树用 stop 端点。
func (m *SessionManager) HandleSessionAgentPause(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	if err := m.agent.PauseAgent(c.Request.Context(), id, instID); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"agent_id":   instID,
		"status":     "paused",
	})
}

// HandleSessionEfficiency 处理 GET /api/sessions/{id}/efficiency
// （TODO 第9项⑥ 效率一等指标 + 第10项③ 支路成本表）。
// 返回五项效率指标 + 支路成本表 + 角色级 token 统计（纯聚合，无新采集管道）。
func (m *SessionManager) HandleSessionEfficiency(c *gin.Context) {
	id := c.Param("id")
	if _, err := m.agent.Get(c.Request.Context(), id); err != nil {
		c.String(http.StatusNotFound, "会话不存在")
		return
	}
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{Kind: agent.QueryKindEfficiency})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, res.Data)
}

// HandleSessionAgentEvents 处理 GET /api/sessions/{id}/agents/{aid}/events
// （TODO 第10项③ 子 Agent 审计下钻）。
// 返回该实例逐轮事件（tool_call/answer 回放数据源）；limit/offset 分页（query 参数）。
func (m *SessionManager) HandleSessionAgentEvents(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{
		Kind: agent.QueryKindAgentEvents,
		Args: map[string]any{"agent": instID, "limit": limit, "offset": offset},
	})
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, res.Data)
}

// HandleSessionAgentMessages 处理 GET /api/sessions/{id}/agents/{aid}/messages（编排页对话视图）。
// 返回该实例完整消息历史（热层+PG 合并分页）与 mailbox 留痕；aid=meta 映射为会话主 Agent。
func (m *SessionManager) HandleSessionAgentMessages(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	beforeSeq, _ := strconv.Atoi(c.Query("before_seq"))
	afterSeq, _ := strconv.Atoi(c.Query("after_seq"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{
		Kind: agent.QueryKindAgentMessages,
		Args: map[string]any{"agent": instID, "before_seq": beforeSeq, "after_seq": afterSeq, "limit": limit},
	})
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, res.Data)
}

// HandleSessionAgentMessage 处理 POST /api/sessions/{id}/agents/{aid}/message（编排页用户直连）。
// 状态机路由在 ReactService.MessageAgent：等子返回→注入唤醒；终态→复活重跑；
// 执行中→409（前端禁用发送）；Paused/Idle/meta→409。
func (m *SessionManager) HandleSessionAgentMessage(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	body, err := DecodeBody[struct {
		Content string `json:"content"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体解析失败")
		return
	}
	if err := m.agent.MessageAgent(c.Request.Context(), id, instID, body.Content); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// HandleSessionWorkDir 处理 POST /api/sessions/{id}/workdir（每会话工作目录修改）。
// body {"work_dir": "D:\\proj"}：显式传空串 = 清除本会话目录、回落进程默认目录；
// 未传字段（nil）= 参数错（避免"忘记传"被当成清空）。落库即时保存，下一回合生效
// （正在执行的工具调用已按旧目录解析，产物不迁移）。
func (m *SessionManager) HandleSessionWorkDir(c *gin.Context) {
	id := c.Param("id")
	req, err := DecodeBody[struct {
		WorkDir *string `json:"work_dir"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	if req.WorkDir == nil {
		c.String(http.StatusBadRequest, "缺少 work_dir 字段")
		return
	}
	abs, ok := workDirAbs(c, *req.WorkDir)
	if !ok {
		return
	}
	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpWorkDir,
		Args: map[string]any{"work_dir": abs},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"session_id": id, "work_dir": abs})
}

// HandleSessionWorktrees 处理 GET /api/sessions/{id}/worktrees
// （TODO 第9项⑤/#10项⑤ 合并门视图）。
// 返回会话全部 worktree 副本快照（路径/分支/base/patch 路径与 stat/合并状态）。
func (m *SessionManager) HandleSessionWorktrees(c *gin.Context) {
	id := c.Param("id")
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{Kind: agent.QueryKindWorktrees})
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, res.Data)
}

// HandleSessionWorktreeDiff 处理 GET /api/sessions/{id}/worktrees/{aid}/diff
// （TODO 第10项⑤ 合并门 review 数据源）：返回指定副本全量 diff（未收尾为实时 diff）。
func (m *SessionManager) HandleSessionWorktreeDiff(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{
		Kind: agent.QueryKindWorktreeDiff,
		Args: map[string]any{"agent": instID},
	})
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, res.Data)
}

// HandleSessionWorktreeAction 处理 POST /api/sessions/{id}/worktrees/{aid}/{action}
// （TODO 第9项⑤/#10项⑤ 合并门操作）：action ∈ merge|reject；reject body 可带 comments。
func (m *SessionManager) HandleSessionWorktreeAction(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	action := strings.ToLower(strings.TrimSpace(c.Param("action")))
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	switch action {
	case "merge", "reject":
	default:
		c.String(http.StatusBadRequest, "action 必须为 merge 或 reject")
		return
	}
	args := map[string]any{"action": action, "agent": instID}
	if action == "reject" {
		var req struct {
			Comments string `json:"comments"`
		}
		if err := c.ShouldBindJSON(&req); err == nil {
			args["comments"] = req.Comments
		}
	}
	if err := m.agent.Control(c.Request.Context(), id, agent.ControlCommand{Op: agent.ControlOpWorktree, Args: args}); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"session_id": id,
		"agent_id":   instID,
		"action":     action,
		"status":     "ok",
	})
}
