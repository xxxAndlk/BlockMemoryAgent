package server

import (
	"context"            // 超时上下文
	"fmt"                // 格式化字符串
	"net/http"           // HTTP 状态码
	"os"                 // 文件读取 / Stat
	"path/filepath"      // filepath.Base
	stdruntime "runtime" // 进程运行时指标
	"sort"               // 目录列表排序
	"strconv"            // Atoi 等
	"strings"            // 目录名大小写不敏感比较
	"sync"               // paused 互斥锁
	"time"               // 超时与时间戳

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/agent"            // ModelManager（模型动态切换）
	"github.com/blockmemory/agent/backend/internal/model"            // ModelFactory
	"github.com/blockmemory/agent/backend/internal/plugins"          // 插件管理器
	"github.com/blockmemory/agent/backend/internal/runtime"          // Runtime
	"github.com/blockmemory/agent/backend/internal/server/eventkind" // 事件类型常量
	"github.com/blockmemory/agent/backend/internal/store"            // Postgres / Redis
	"github.com/blockmemory/agent/backend/internal/domain/tool"      // WithWorkDir(work_dir 注入)
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"      // RoleConfigFile
	"github.com/blockmemory/agent/backend/pkg/types"                 // 共享类型
)

// APIHandler TUI / Web API 处理器。
// 职责：聚合 broadcaster / snapshotMgr / 各存储与配置，统一暴露 HTTP 接口给前端。
//
// 字段说明：
//   - broadcaster：SSE 广播器
//   - snapshotMgr：快照管理器（鸭子类型，避免循环依赖）
//   - paused：topic_id -> 是否暂停（用于 graphControl）
//   - pausedMu：保护 paused 的并发读写
//   - rt：聚合运行时（人格、skill、watchdog 等）
//   - sessionMgr：会话管理器
//   - pgStore：Postgres 存储
//   - redisStore：Redis 存储
//   - roleCfg：角色配置
//   - modelFactory：模型工厂
//   - statsService：会话统计聚合服务
type APIHandler struct {
	broadcaster *TUIBroadcaster // SSE 广播器
	snapshotMgr interface {     // 快照管理器（鸭子类型，避免循环依赖）
		Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	}
	// graphControl 用于暂停/恢复 Graph
	paused   map[string]bool // topic_id -> 是否暂停
	pausedMu sync.RWMutex    // 保护 paused 的并发读写

	rt           *runtime.Runtime          // 聚合运行时
	sessionMgr   *SessionManager           // 会话管理器
	pgStore      *store.PostgresStore      // Postgres 存储
	redisStore   *store.RedisStore         // Redis 存储
	roleCfg      *pkgconfig.RoleConfigFile // 角色配置
	modelFactory *model.ModelFactory       // 模型工厂
	modelMgr     agent.ModelManager        // 模型动态切换能力（ReactService 实现）
	statsService *StatsService             // 会话统计聚合服务
	pluginMgr    *plugins.Manager          // 插件管理器（热插拔插件管理 API）
	learnedSkills *store.LearnedSkillStore // 自进化技能库存储（2026-09-02 设计 §8）
	// defaultWorkDir 进程默认工作目录（bootstrap 注入）：work_dir 参数为空时
	// tester-config 等工作目录级配置的回落目录（与 ReactService.workDir 同源）。
	defaultWorkDir string
}

// NewAPIHandler 创建 API 处理器。
// 参数 broadcaster：SSE 广播器。
// 返回值：*APIHandler（其余字段需通过 Set* 方法注入）。
func NewAPIHandler(broadcaster *TUIBroadcaster) *APIHandler {
	return &APIHandler{
		broadcaster: broadcaster,
		paused:      make(map[string]bool), // 初始化暂停表
	}
}

// SetSnapshotManager 设置快照管理器。
// 参数 mgr：实现了 Load(ctx, agentID, topicID) 的对象。
func (h *APIHandler) SetSnapshotManager(mgr interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}) {
	h.snapshotMgr = mgr
}

// SetRuntime 注入 Runtime。
// 参数 rt：聚合运行时。
func (h *APIHandler) SetRuntime(rt *runtime.Runtime) {
	h.rt = rt
}

// SetSessionManager 注入会话管理器。
// 参数 mgr：会话管理器。
func (h *APIHandler) SetSessionManager(mgr *SessionManager) {
	h.sessionMgr = mgr
	if h.statsService == nil {
		h.statsService = NewStatsService(mgr)
	}
}

// SetStatsService 显式注入统计服务（测试用）。
// 参数 s：统计服务实例。
func (h *APIHandler) SetStatsService(s *StatsService) {
	h.statsService = s
}

// SetStores 注入存储层。
// 参数 pg：Postgres；redis：Redis。
func (h *APIHandler) SetStores(pg *store.PostgresStore, redis *store.RedisStore) {
	h.pgStore = pg
	h.redisStore = redis
}

// SetRoleConfig 注入角色配置。
// 参数 cfg：角色配置文件。
func (h *APIHandler) SetRoleConfig(cfg *pkgconfig.RoleConfigFile) {
	h.roleCfg = cfg
}

// SetModelFactory 注入模型工厂。
// 参数 mf：模型工厂。
func (h *APIHandler) SetModelFactory(mf *model.ModelFactory) {
	h.modelFactory = mf
}

// SetDefaultWorkDir 注入进程默认工作目录（work_dir 参数为空时的回落目录）。
func (h *APIHandler) SetDefaultWorkDir(dir string) {
	h.defaultWorkDir = dir
}

// RetrieveHandler 手动检索记忆。
// 职责：解析 POST body，广播 retrieve.request 事件给订阅者。
// 副作用：广播 UIEvent；不做实际检索，只触发前端展示。
func (h *APIHandler) RetrieveHandler(c *gin.Context) {
	// 解析请求体，提取 topic_id / agent_id / query。
	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	// 广播检索请求事件，通知订阅者展示检索意图。
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "retrieve.request",
		Payload: map[string]string{"agent_id": req.AgentID, "query": req.Query},
	})

	c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// EventResolveHandler 标记 Event 已处理。
// 职责：解析 POST body，广播 workspace.event 事件（status=Done），通知前端关闭事件。
func (h *APIHandler) EventResolveHandler(c *gin.Context) {
	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
		EventID string `json:"event_id"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type: "workspace.event",
		Payload: types.EventPayload{
			ID:     req.EventID,
			Status: "Done", // 标记为已解决
		},
	})

	c.JSON(http.StatusOK, map[string]string{"status": "resolved"})
}

// SnapshotInspectHandler 查看快照详情（已移除，保留端点兼容）。
func (h *APIHandler) SnapshotInspectHandler(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"agent_id": "",
		"snapshot": nil,
		"note":     "snapshot manager removed in ReAct refactor",
	})
}

// GraphPauseHandler 暂停 Graph。
// 职责：把 topic 标记为暂停，广播 graph.control(action=pause) 事件。
func (h *APIHandler) GraphPauseHandler(c *gin.Context) {
	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	h.pausedMu.Lock()
	h.paused[req.TopicID] = true // 标记暂停
	h.pausedMu.Unlock()

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "pause"},
	})

	c.JSON(http.StatusOK, map[string]string{"status": "paused"})
}

// GraphResumeHandler 恢复 Graph。
// 职责：从 paused 表删除 topic，广播 graph.control(action=resume) 事件。
func (h *APIHandler) GraphResumeHandler(c *gin.Context) {
	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
	}](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}

	h.pausedMu.Lock()
	delete(h.paused, req.TopicID) // 取消暂停
	h.pausedMu.Unlock()

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "resume"},
	})

	c.JSON(http.StatusOK, map[string]string{"status": "resumed"})
}

// IsPaused 检查话题是否已暂停。
// 参数 topicID：话题 ID。
// 返回值：bool - true 表示已暂停。
func (h *APIHandler) IsPaused(topicID string) bool {
	h.pausedMu.RLock()
	defer h.pausedMu.RUnlock()
	return h.paused[topicID]
}

// MetricsHandler 处理 GET /api/metrics — 返回 Prometheus 格式运行时指标。
// 指标：goroutine 数、内存分配、内存中会话数、LLM 调用/超时次数。
func (h *APIHandler) MetricsHandler(c *gin.Context) {
	var ms stdruntime.MemStats
	stdruntime.ReadMemStats(&ms)

	sessionCount := 0
	callCount, timeoutCount := 0, 0
	if h.sessionMgr != nil {
		sessionCount = h.sessionMgr.SessionCount()
		callCount, timeoutCount, _, _ = h.sessionMgr.LLMStats()
	}

	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(http.StatusOK, "# HELP go_goroutines Number of goroutines\n# TYPE go_goroutines gauge\ngo_goroutines %d\n\n", stdruntime.NumGoroutine())
	c.String(http.StatusOK, "# HELP go_memory_alloc_bytes Allocated memory in bytes\n# TYPE go_memory_alloc_bytes gauge\ngo_memory_alloc_bytes %d\n\n", ms.Alloc)
	c.String(http.StatusOK, "# HELP bma_sessions_total Total sessions in memory\n# TYPE bma_sessions_total gauge\nbma_sessions_total %d\n\n", sessionCount)
	c.String(http.StatusOK, "# HELP bma_llm_calls_total Total LLM calls\n# TYPE bma_llm_calls_total counter\nbma_llm_calls_total %d\n\n", callCount)
	c.String(http.StatusOK, "# HELP bma_llm_timeouts_total Total LLM timeouts\n# TYPE bma_llm_timeouts_total counter\nbma_llm_timeouts_total %d\n", timeoutCount)
}

// HealthHandler 处理 GET /api/health — 返回 Postgres / Redis / LLM 连接状态。
// 职责：分别 ping Postgres、Redis，统计 LLM 调用健康度，返回 JSON 报告。
func (h *APIHandler) HealthHandler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second) // 健康检查总超时 2 秒
	defer cancel()

	pgStatus := checkPostgres(ctx, h.pgStore)    // Postgres 探活
	redisStatus := checkRedis(ctx, h.redisStore) // Redis 探活
	llmStatus := checkLLM(h.sessionMgr)          // LLM 统计

	c.JSON(http.StatusOK, map[string]any{
		"postgres": pgStatus,
		"redis":    redisStatus,
		"llm":      llmStatus,
	})
}

// checkPostgres 检查 Postgres 连接。
// 参数 ctx：探活超时上下文；pg：Postgres 存储实例。
// 返回值：map - 含 name / online / detail / latency_ms。
func checkPostgres(ctx context.Context, pg *store.PostgresStore) map[string]any {
	status := map[string]any{"name": "Postgres", "online": false, "detail": "not configured"}
	if pg == nil {
		return status // 未配置直接返回
	}
	start := time.Now()
	if err := pg.DB().PingContext(ctx); err != nil {
		status["detail"] = err.Error() // ping 失败
		return status
	}
	status["online"] = true
	status["latency_ms"] = time.Since(start).Milliseconds() // 延迟毫秒
	status["detail"] = "connected"
	return status
}

// checkRedis 检查 Redis 连接。
// 参数 ctx：探活超时上下文；redis：Redis 存储实例。
// 返回值：map - 含 name / online / detail / latency_ms。
func checkRedis(ctx context.Context, redis *store.RedisStore) map[string]any {
	status := map[string]any{"name": "Redis", "online": false, "detail": "not configured"}
	if redis == nil {
		return status // 未配置直接返回
	}
	start := time.Now()
	if err := redis.Ping(ctx); err != nil {
		status["detail"] = err.Error()
		return status
	}
	status["online"] = true
	status["latency_ms"] = time.Since(start).Milliseconds()
	status["detail"] = "connected"
	return status
}

// checkLLM 通过 SessionManager 的 LLM 调用统计判断 LLM 健康。
// 健康判定：calls>0 且 timeouts<calls。
// 参数 mgr：会话管理器。
// 返回值：map - 含 name / online / detail。
func checkLLM(mgr *SessionManager) map[string]any {
	status := map[string]any{"name": "LLM API", "online": false, "detail": "not configured"}
	if mgr == nil {
		return status
	}
	calls, timeouts, avg, max := mgr.LLMStats() // 聚合统计
	online := calls > 0 && timeouts < calls     // 调用次数 > 0 且超时未占满
	status["online"] = online
	status["detail"] = fmt.Sprintf("calls=%d timeouts=%d avg=%v max=%v", calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond))
	if !online {
		status["detail"] = "no recent calls" // 无近期调用视为不健康
	}
	return status
}

// StatusHandler 处理 GET /api/status — 返回程序、模式、人格、LLM 配置概览。
// 职责：聚合 Soul 名称与角色配置中的 LLM provider / model，返回静态信息。
func (h *APIHandler) StatusHandler(c *gin.Context) {
	soulName := "default"
	if h.rt != nil && h.rt.Soul != nil {
		soulName = h.rt.Soul.Name() // 取人格名称
	}

	llmProvider := "mock"
	llmModel := "mock"
	if h.modelFactory != nil {
		// 从工厂读当前生效模型（跟随运行时动态切换的绑定），而非静态 roleCfg。
		if st, err := h.modelFactory.CurrentModelInfo("meta"); err == nil {
			llmProvider, llmModel = st.Provider, st.Model
		}
	} else if h.roleCfg != nil {
		llmProvider = h.roleCfg.MetaAgent.ModelConfig.Provider // 供应商
		llmModel = h.roleCfg.MetaAgent.ModelConfig.Model       // 模型名
	}

	c.JSON(http.StatusOK, map[string]any{
		"program":      "BlockMemoryAgent",
		"mode":         "multi-agent",
		"soul":         soulName,
		"llm_provider": llmProvider,
		"llm_model":    llmModel,
		"version":      "v3",
	})
}

// TimelineHandler 处理 GET /api/metrics/timeline — 返回最近会话 / LLM 时间线。
// 职责：按小时聚合所有会话的 token_usage 事件，返回 N 个时间点的 calls / tokens。
// 参数：?points=N - 时间点数（默认 12）。
func (h *APIHandler) TimelineHandler(c *gin.Context) {
	points := 12 // 默认 12 个点（12 小时）
	if q := c.Query("points"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			points = n
		}
	}

	data := h.statsService.Timeline(points)

	c.JSON(http.StatusOK, map[string]any{"points": data})
}

// ActivityHandler 处理 GET /api/activity — 返回最近活动流。
// 职责：聚合所有会话事件，倒序取前 N 条作为活动流。
// 参数：?limit=N - 返回条数（默认 10）。
func (h *APIHandler) ActivityHandler(c *gin.Context) {
	limit := 10 // 默认 10 条
	if q := c.Query("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}

	activities := h.statsService.Activity(limit)

	c.JSON(http.StatusOK, map[string]any{"activities": activities})
}

// SnapshotHandler 处理 POST /api/snapshot — 查看 Agent 快照（已移除，保留端点兼容）。
func (h *APIHandler) SnapshotHandler(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"agent_id": "",
		"snapshot": nil,
		"note":     "snapshot manager removed in ReAct refactor",
	})
}

// MemorySearchHandler 处理 POST /api/memory/search — 已移除（ReAct 重构）。
func (h *APIHandler) MemorySearchHandler(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"results": []any{},
		"note":    "memory search removed in ReAct refactor",
	})
}

// MemoryLevelsHandler 处理 GET /api/memory/levels — 已移除（ReAct 重构）。
func (h *APIHandler) MemoryLevelsHandler(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"total":  0,
		"levels": map[string]int{"raw": 0, "standard": 0},
		"note":   "memory levels removed in ReAct refactor",
	})
}

// MemoryEvalHandler 处理 GET /api/memory/eval — 已移除（ReAct 重构）。
func (h *APIHandler) MemoryEvalHandler(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]any{
		"total_episodes": 0,
		"raw":            0,
		"standard":       0,
		"pairs":          []any{},
		"note":           "memory eval removed in ReAct refactor",
	})
}

// SkillsHandler 处理 GET /api/skills — 返回 Skill 池全部技能（含来源标记 Source）。
// 职责：从 Runtime.Skills 取所有技能，返回 JSON 列表。
func (h *APIHandler) SkillsHandler(c *gin.Context) {
	var skills []*types.Skill
	if h.rt != nil && h.rt.Skills != nil {
		skills = h.rt.Skills.All() // 取所有技能
	}
	c.JSON(http.StatusOK, map[string]any{"skills": skills})
}

// FilesHandler 处理 GET /api/files — 返回某会话 WriteFile 输出文件列表。
// 职责：扫描会话事件中的 WriteFile 工具调用，去重后返回文件路径 / 大小 / 名称。
// 参数：?session=session-N。
func (h *APIHandler) FilesHandler(c *gin.Context) {
	sessionID := c.Query("session")
	if sessionID == "" {
		c.String(http.StatusBadRequest, "session required")
		return
	}

	var files []map[string]any
	if h.sessionMgr != nil {
		s := h.sessionMgr.GetSession(sessionID)
		if s != nil {
			seen := make(map[string]bool) // 文件路径去重
			for _, ev := range s.Events {
				if ev.Type != eventkind.ToolExec || ev.Tool != "WriteFile" || !ev.Success || ev.ToolPath == "" {
					continue // 仅成功的 WriteFile 事件
				}
				if seen[ev.ToolPath] {
					continue // 去重
				}
				seen[ev.ToolPath] = true
				info, _ := os.Stat(ev.ToolPath) // 查询文件信息
				files = append(files, map[string]any{
					"path": ev.ToolPath,
					"size": fileSize(info),
					"name": filepath.Base(ev.ToolPath), // 仅文件名
				})
			}
		}
	}

	c.JSON(http.StatusOK, map[string]any{
		"session_id": sessionID,
		"files":      files,
	})
}

// BrowseFSHandler 处理 GET /api/fs/browse?path=，只列目录（前端工作目录选择器）。
// path 为空：Windows 返回盘符列表，其他系统返回 /。
func (h *APIHandler) BrowseFSHandler(c *gin.Context) {
	type dirEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	p := c.Query("path")
	if p == "" {
		dirs := []dirEntry{}
		if stdruntime.GOOS == "windows" {
			for _, l := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
				d := string(l) + `:\`
				if _, err := os.Stat(d); err == nil {
					dirs = append(dirs, dirEntry{Name: d, Path: d})
				}
			}
		} else {
			dirs = append(dirs, dirEntry{Name: "/", Path: "/"})
		}
		c.JSON(http.StatusOK, gin.H{"path": "", "parent": "", "dirs": dirs})
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		c.String(http.StatusBadRequest, "路径不可读: %s", err.Error())
		return
	}
	dirs := []dirEntry{}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, dirEntry{Name: e.Name(), Path: filepath.Join(p, e.Name())})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	parent := filepath.Dir(p)
	// 盘符根/根目录（如 C:\）Dir 返回自身：无上级可回，parent 置空让前端禁用"上级"按钮。
	if parent == p {
		parent = ""
	}
	c.JSON(http.StatusOK, gin.H{"path": p, "parent": parent, "dirs": dirs})
}

// FileContentHandler 处理 GET /api/files/content — 读取文件内容。
// 职责：按 ?path=... 读取文件全文，返回 JSON（content 为字符串）。
func (h *APIHandler) FileContentHandler(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}
	data, err := os.ReadFile(path) // 读取文件
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"path":    path,
		"content": string(data),
	})
}

// fileSize 安全地获取文件大小，info 为 nil 时返回 0。
// 参数 info：os.FileInfo（可为 nil）。
// 返回值：int64 - 文件字节数。
func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

// ProfileHandler 处理 GET /api/profile — 返回用户画像全文（TODO #28 查看入口）。
func (h *APIHandler) ProfileHandler(c *gin.Context) {
	if h.sessionMgr == nil {
		c.JSON(http.StatusOK, map[string]any{"content": ""})
		return
	}
	p, err := h.sessionMgr.agent.Profile(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	content := ""
	if p != nil {
		content = p.Content
	}
	c.JSON(http.StatusOK, map[string]any{"path": p.Path, "content": content})
}

// SaveProfileHandler 处理 PUT /api/profile — 全量覆盖用户画像（TODO #28 可纠正，用户手动编辑）。
func (h *APIHandler) SaveProfileHandler(c *gin.Context) {
	if h.sessionMgr == nil {
		c.String(http.StatusInternalServerError, "session manager not wired")
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := h.sessionMgr.agent.SaveProfile(c.Request.Context(), req.Content); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// prefsWorkDirCtx 校验可选 work_dir 参数并注入 ctx（与 /api/sessions 的 work_dir 同规则：
// 非空时转绝对路径，不存在或非目录返回 400）；为空时原样返回请求 ctx（进程目录语义）。
func prefsWorkDirCtx(c *gin.Context, dir string) (context.Context, bool) {
	abs, ok := workDirAbs(c, dir)
	if !ok {
		return nil, false
	}
	if abs == "" {
		return c.Request.Context(), true
	}
	return tool.WithWorkDir(c.Request.Context(), abs), true
}

// ProjectPreferencesHandler 处理 GET /api/project/preferences — 返回当前 workDir 项目偏好全文
//（2026-09-02 设计 §5：.bma/project_preferences.md）。可选 query work_dir 按会话目录解析。
func (h *APIHandler) ProjectPreferencesHandler(c *gin.Context) {
	if h.sessionMgr == nil {
		c.JSON(http.StatusOK, map[string]any{"content": ""})
		return
	}
	ctx, ok := prefsWorkDirCtx(c, c.Query("work_dir"))
	if !ok {
		return
	}
	p, err := h.sessionMgr.agent.ProjectPreferences(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	content := ""
	if p != nil {
		content = p.Content
	}
	c.JSON(http.StatusOK, map[string]any{"path": p.Path, "content": content})
}

// SaveProjectPreferencesHandler 处理 PUT /api/project/preferences — 全量覆盖项目偏好。
// 可选 body 字段 work_dir 按会话目录解析（校验规则同 /api/sessions）。
func (h *APIHandler) SaveProjectPreferencesHandler(c *gin.Context) {
	if h.sessionMgr == nil {
		c.String(http.StatusInternalServerError, "session manager not wired")
		return
	}
	var req struct {
		Content string `json:"content"`
		WorkDir string `json:"work_dir,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	ctx, ok := prefsWorkDirCtx(c, req.WorkDir)
	if !ok {
		return
	}
	if err := h.sessionMgr.agent.SaveProjectPreferences(ctx, req.Content); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}
