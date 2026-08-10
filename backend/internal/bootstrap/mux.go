package bootstrap

import (
	"net/http"

	"github.com/blockmemory/agent/backend/internal/server"
)

// NewDefaultMux 从已装配的 App 构造生产环境 HTTP 路由复用器（mux）。
// 它会挂载配置好的 auth token 中间件，并将所有 API 路由注册到 *http.ServeMux。
//
// 参数：
//   - app: 经过 bootstrap.Build 装配完成的应用上下文，包含配置、session manager、runtime 等依赖。
//
// 返回：注册完全部路由的 *http.ServeMux，可直接交给 http.Server 使用。
func NewDefaultMux(app *App) *http.ServeMux {
	// 取出配置对象，供后续判断认证是否开启。
	cfg := app.Config
	// 创建标准库路由复用器；Go 1.22+ 支持 /api/sessions/{id} 形式的路径变量。
	mux := http.NewServeMux()

	// authToken 在认证关闭时为空字符串，开启时取自配置；由 AuthMiddleware 负责判断是否放行。
	authToken := ""
	// publicPaths 列出不需 token 即可访问的路径（目前仅健康检查）。
	publicPaths := []string{"/api/health"}
	// 若 HTTP 认证开启，则把配置的 token 交给中间件使用。
	if cfg.HTTP.AuthEnabled {
		authToken = cfg.HTTP.AuthToken
	}
	// wrap 是路由包装器：每个业务 handler 都会被 AuthMiddleware 包裹，统一鉴权。
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return server.AuthMiddleware(authToken, publicPaths, h)
	}

	// sessionMgr 复用 App 中的 SessionManager，负责所有 /api/sessions/* 端点。
	sessionMgr := app.Server

	// /api/sessions 集合端点：GET 列出会话，POST 创建会话，其他方法返回 405。
	mux.HandleFunc("/api/sessions", wrap(func(w http.ResponseWriter, r *http.Request) {
		// 按 HTTP 方法分发请求。
		switch r.Method {
		case http.MethodGet:
			// GET：返回会话列表。
			sessionMgr.HandleListSessions(w, r)
		case http.MethodPost:
			// POST：创建新会话。
			sessionMgr.HandleCreateSession(w, r)
		default:
			// 其他方法：直接拒绝并提示 Method Not Allowed。
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// 以下会话子资源均使用 Go 1.22 路径变量，handler 内部通过 r.PathValue("id") 读取会话 ID。
	mux.HandleFunc("/api/sessions/{id}/stream", wrap(sessionMgr.HandleSessionStream))              // SSE 流式消息推送
	mux.HandleFunc("/api/sessions/{id}/message", wrap(sessionMgr.HandleSessionMessage))            // 向会话发送消息
	mux.HandleFunc("/api/sessions/{id}/board", wrap(sessionMgr.HandleSessionBoard))                // 获取/更新任务看板
	mux.HandleFunc("/api/sessions/{id}/agents", wrap(sessionMgr.HandleSessionAgents))              // 获取会话内 Agent 列表
	mux.HandleFunc("/api/sessions/{id}/tree", wrap(sessionMgr.HandleSessionTree))                    // 获取权威 Agent 树快照
	mux.HandleFunc("/api/sessions/{id}/agents/{aid}/cancel", wrap(sessionMgr.HandleSessionAgentCancel)) // 取消子 Agent 实例
	mux.HandleFunc("/api/sessions/{id}/metrics", wrap(sessionMgr.HandleSessionMetrics))            // 会话级指标
	mux.HandleFunc("/api/sessions/{id}/logs", wrap(sessionMgr.HandleSessionLogs))                  // 会话日志查询
	mux.HandleFunc("/api/sessions/{id}/token-metrics", wrap(sessionMgr.HandleSessionTokenMetrics)) // Token 消耗指标
	mux.HandleFunc("/api/sessions/{id}/watchdog", wrap(sessionMgr.HandleSessionWatchdog))          // Watchdog 状态/重置
	mux.HandleFunc("/api/sessions/{id}/mailbox", wrap(sessionMgr.HandleSessionMailbox))            // 邮箱消息投递
	mux.HandleFunc("/api/sessions/{id}/clarify", wrap(sessionMgr.HandleSessionClarify))            // 请求澄清
	mux.HandleFunc("/api/sessions/{id}/interrupt", wrap(sessionMgr.HandleSessionInterrupt))        // 中断当前执行
	mux.HandleFunc("/api/sessions/{id}/enqueue", wrap(sessionMgr.HandleSessionEnqueue))            // 将任务入队
	mux.HandleFunc("/api/sessions/{id}/cancel", wrap(sessionMgr.HandleSessionCancel))              // 取消任务
	mux.HandleFunc("/api/sessions/{id}/topic", wrap(sessionMgr.HandleSessionTopic))                // 主题管理
	mux.HandleFunc("/api/sessions/{id}", wrap(sessionMgr.HandleGetSession))                        // 获取单个会话详情

	// 构造通用 APIHandler 并注入业务依赖。
	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(sessionMgr)      // 会话管理能力
	apiHandler.SetRuntime(app.Runtime)            // 运行时上下文（人格、skill、watchdog 等）
	apiHandler.SetStores(app.Postgres, app.Redis) // 持久化与缓存存储
	apiHandler.SetRoleConfig(app.RoleConfig)      // 角色配置
	apiHandler.SetModelFactory(app.ModelFactory)  // 模型工厂

	// 注册全局/系统级端点。
	mux.HandleFunc("/api/health", apiHandler.HealthHandler)                   // 健康检查：公开访问
	mux.HandleFunc("/api/metrics", wrap(apiHandler.MetricsHandler))           // 指标聚合：需鉴权
	mux.HandleFunc("/api/status", wrap(apiHandler.StatusHandler))             // 服务状态：需鉴权
	mux.HandleFunc("/api/metrics/timeline", wrap(apiHandler.TimelineHandler)) // 指标时间线
	mux.HandleFunc("/api/activity", wrap(apiHandler.ActivityHandler))         // 活动日志

	// DAG 相关的 handler 需要匹配 /api/dag 与 /api/dag/ 两个前缀，因此使用 http.Handle 模式。
	dagHandler := app.DAGHandler
	mux.Handle("/api/dag", wrap(func(w http.ResponseWriter, r *http.Request) {
		// 精确路径 /api/dag 的入口。
		dagHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/api/dag/", wrap(func(w http.ResponseWriter, r *http.Request) {
		// /api/dag/ 子路径的入口。
		dagHandler.ServeHTTP(w, r)
	}))

	// 注册其他业务端点。
	mux.HandleFunc("/api/snapshot", wrap(apiHandler.SnapshotHandler))              // 快照管理
	mux.HandleFunc("/api/memory/search", wrap(apiHandler.MemorySearchHandler))     // 记忆检索
	mux.HandleFunc("/api/memory/levels", wrap(apiHandler.MemoryLevelsHandler))     // 记忆层级
	mux.HandleFunc("/api/memory/eval", wrap(apiHandler.MemoryEvalHandler))         // 记忆评估
	mux.HandleFunc("/api/skills", wrap(apiHandler.SkillsHandler))                  // Skill 列表
	mux.HandleFunc("/api/agents/{id}/skills", wrap(apiHandler.AgentSkillsHandler)) // Agent Skill 管理
	mux.HandleFunc("/api/files", wrap(apiHandler.FilesHandler))                    // 文件列表
	mux.HandleFunc("/api/files/content", wrap(apiHandler.FileContentHandler))      // 文件内容读取
	mux.HandleFunc("/api/profile", wrap(apiHandler.ProfileHandler))     // 用户画像查看（TODO #28）
	mux.HandleFunc("PUT /api/profile", wrap(apiHandler.SaveProfileHandler)) // 用户画像编辑

	// 返回装配完成的路由复用器。
	return mux
}
