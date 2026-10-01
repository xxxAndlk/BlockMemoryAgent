package server

import (
	"context"            // 超时上下文
	"fmt"                // 格式化字符串
	"mime"               // 按扩展名探测 Content-Type
	"net/http"           // HTTP 状态码
	"net/url"            // Content-Disposition 文件名 URL 编码
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
	// skillConsolidation 技能库整理器（C 库存治理）：手动触发一轮合并/归档，返回摘要。
	skillConsolidation func(ctx context.Context) (string, error)
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
// 增强：附带 size / mime / truncated（文本超过 fileContentMaxBytes 截断），
// 前端据此渲染「文件过大」提示；content 字段保持旧契约不变。
func (h *APIHandler) FileContentHandler(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}
	data, err := os.ReadFile(path) // 读取文件
	if err != nil {
		if os.IsNotExist(err) {
			c.String(http.StatusNotFound, "file not found")
			return
		}
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	size := fileSizeOf(data)
	truncated := false
	if len(data) > fileContentMaxBytes {
		data = data[:fileContentMaxBytes] // 大文件截断，前端提示下载查看
		truncated = true
	}
	c.JSON(http.StatusOK, map[string]any{
		"path":      path,
		"content":   string(data),
		"size":      size,
		"mime":      detectFileContentType(filepath.Ext(path)),
		"truncated": truncated,
	})
}

// 文件预览相关上限（/api/files/raw 与 /api/files/content 共用）。
const (
	fileRawMaxBytes     = 20 << 20 // 20MB：raw 下载/内联渲染上限，超限 413
	fileContentMaxBytes = 300 << 10 // 300KB：content JSON 文本截断阈值
)

// fileRawMimeTypes 常见扩展名 → Content-Type 白名单（mime.TypeByExtension 在
// Windows 下读注册表、结果不可控，预览相关类型在这里钉死）。
var fileRawMimeTypes = map[string]string{
	// 图片（内联渲染主力）
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	// 文档/数据
	".pdf":       "application/pdf",
	".json":      "application/json",
	".md":        "text/markdown; charset=utf-8",
	".markdown":  "text/markdown; charset=utf-8",
	".txt":       "text/plain; charset=utf-8",
	".log":       "text/plain; charset=utf-8",
	".csv":       "text/csv; charset=utf-8",
	".html":      "text/html; charset=utf-8",
	".htm":       "text/html; charset=utf-8",
	".xml":       "text/xml; charset=utf-8",
	".yaml":      "text/yaml; charset=utf-8",
	".yml":       "text/yaml; charset=utf-8",
	// 代码（按纯文本返回，高亮由前端 highlight.js 负责）
	".go":   "text/plain; charset=utf-8",
	".py":   "text/plain; charset=utf-8",
	".js":   "text/plain; charset=utf-8",
	".jsx":  "text/plain; charset=utf-8",
	".ts":   "text/plain; charset=utf-8",
	".tsx":  "text/plain; charset=utf-8",
	".vue":  "text/plain; charset=utf-8",
	".java": "text/plain; charset=utf-8",
	".c":    "text/plain; charset=utf-8",
	".cc":   "text/plain; charset=utf-8",
	".cpp":  "text/plain; charset=utf-8",
	".h":    "text/plain; charset=utf-8",
	".hpp":  "text/plain; charset=utf-8",
	".cs":   "text/plain; charset=utf-8",
	".rs":   "text/plain; charset=utf-8",
	".rb":   "text/plain; charset=utf-8",
	".php":  "text/plain; charset=utf-8",
	".sh":   "text/plain; charset=utf-8",
	".bat":  "text/plain; charset=utf-8",
	".ps1":  "text/plain; charset=utf-8",
	".sql":  "text/plain; charset=utf-8",
	".css":  "text/plain; charset=utf-8",
	".scss": "text/plain; charset=utf-8",
	".less": "text/plain; charset=utf-8",
	".toml": "text/plain; charset=utf-8",
	".ini":  "text/plain; charset=utf-8",
}

// detectFileContentType 按扩展名检测响应 Content-Type：白名单优先，
// 其次 mime.TypeByExtension（仅放行 text/* 与 image/*，防注册表脏数据），
// 其余统一 application/octet-stream。
func detectFileContentType(ext string) string {
	if ct, ok := fileRawMimeTypes[strings.ToLower(ext)]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" &&
		(strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "image/")) {
		return ct
	}
	return "application/octet-stream"
}

// isKnownWriteFilePath 校验 path 是否为任一会话中成功 WriteFile 事件的产物路径。
// 这是 /api/files/raw 的安全边界（与 FilesHandler 同源）：不做任意路径读取，
// 只有 Agent 自己写出且记录在会话事件里的文件才允许被浏览器取走。
func (h *APIHandler) isKnownWriteFilePath(path string) bool {
	if h.sessionMgr == nil {
		return false
	}
	for _, s := range h.sessionMgr.ListSessions() {
		for _, ev := range s.Events {
			if ev.Type == eventkind.ToolExec && ev.Tool == "WriteFile" && ev.Success && ev.ToolPath == path {
				return true
			}
		}
	}
	return false
}

// FileRawHandler 处理 GET /api/files/raw?path=... [&download=1] — 按真实
// Content-Type 返回文件字节，供 <img> 内联渲染与浏览器直接下载。
// 安全边界：path 必须命中某会话的成功 WriteFile 产物（isKnownWriteFilePath），
// 否则一律 404（不区分"不存在"与"越界"，避免路径探测）；大小超 fileRawMaxBytes 返回 413。
func (h *APIHandler) FileRawHandler(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}
	if !h.isKnownWriteFilePath(path) {
		c.String(http.StatusNotFound, "file not found")
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		c.String(http.StatusNotFound, "file not found")
		return
	}
	if info.Size() > fileRawMaxBytes {
		c.String(http.StatusRequestEntityTooLarge, "file too large (max 20MB)")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		c.String(http.StatusNotFound, "file not found")
		return
	}
	ct := detectFileContentType(filepath.Ext(path))
	c.Header("Content-Type", ct)
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Query("download") == "1" {
		name := url.PathEscape(filepath.Base(path))
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", name))
	}
	c.Data(http.StatusOK, ct, data)
}

// fileSizeOf 返回字节切片长度（int64），避免与 os.FileInfo 版 fileSize 混淆。
func fileSizeOf(data []byte) int64 { return int64(len(data)) }

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
