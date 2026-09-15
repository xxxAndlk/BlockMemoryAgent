package bootstrap

import (
	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/server"
)

// NewDefaultRouter 从已装配的 App 构造生产环境 Gin 路由引擎。
// 它把鉴权中间件挂到 /api 分组上，并将所有 API 路由注册到 *gin.Engine。
//
// 参数：
//   - app: 经过 bootstrap.Build 装配完成的应用上下文，包含配置、session manager、runtime 等依赖。
//
// 返回：注册完全部路由的 *gin.Engine，可直接交给 http.Server 使用（gin.Engine 实现 http.Handler）。
func NewDefaultRouter(app *App) *gin.Engine {
	// 取出配置对象，供后续判断认证是否开启。
	cfg := app.Config

	// 生产模式：关闭 Gin 的调试日志与默认路由打印。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.HandleMethodNotAllowed = true // 未匹配方法返回 405（对齐原 mux 行为）
	// 子 Agent 实例 ID 形如 "session-1/domain-2"（含 "/"），前端经 encodeURIComponent 转义为
	// %2F 放进单段路径参数。gin 默认按解码后的 URL.Path 匹配，%2F 会被当成路径分隔符 →
	// /agents/:aid/* 全 404（编排页对子 Agent 不可用）。开启 RawPath 匹配 + 参数解码，
	// 使 :aid 收得回完整实例 ID（回归护栏：server/routes_agentparam_test.go）。
	router.UseRawPath = true
	router.UnescapePathValues = true

	// authToken 在认证关闭时为空字符串，开启时取自配置；由 GinAuthMiddleware 负责判断是否放行。
	authToken := ""
	if cfg.HTTP.AuthEnabled {
		authToken = cfg.HTTP.AuthToken
	}
	// 公开端点直接注册在无鉴权组（/api/health），因此鉴权中间件无需路径白名单。
	auth := server.GinAuthMiddleware(authToken, nil)

	// sessionMgr 复用 App 中的 SessionManager，负责所有 /api/sessions/* 端点。
	sessionMgr := app.Server

	// 构造通用 APIHandler 并注入业务依赖。
	apiHandler := apiHandlerOf(app)
	apiHandler.SetPluginManager(app.Plugins) // 插件管理器（热插拔插件管理 API）

	// /api 分组：全部业务端点统一经 Bearer Token 鉴权。
	api := router.Group("/api", auth)

	// 健康检查：公开访问（k8s 探针），注册在鉴权分组之外。
	router.GET("/api/health", apiHandler.HealthHandler)

	// 会话端点全集：生产、TUI 本地服务与测试共用同一注册函数。
	server.RegisterSessionRoutes(api, sessionMgr)
	// 会话导出（TODO #18-2 T29）：handler 在 APIHandler（需要 pgStore/工作目录解析），
	// 不进 RegisterSessionRoutes——TUI 本地服务无存储依赖，导出属 Web/备份诉求。
	api.GET("/sessions/:id/export", apiHandler.SessionExportHandler)

	// 注册全局/系统级端点。
	api.GET("/metrics", apiHandler.MetricsHandler)           // 指标聚合（Prometheus 格式）
	api.GET("/status", apiHandler.StatusHandler)             // 服务状态
	api.GET("/metrics/timeline", apiHandler.TimelineHandler) // 指标时间线
	api.GET("/activity", apiHandler.ActivityHandler)         // 活动日志
	api.GET("/capabilities", apiHandler.CapabilitiesHandler) // 装机能力自检（TODO #18-1 T28，settings 能力面板数据源）
	api.GET("/export/memory", apiHandler.MemoryExportHandler) // 记忆库导出 zip（TODO #18-2 T29：知识/画像/技能包）

	// DAG 相关端点：调度器未启用时各 handler 自行返回 503。
	app.DAGHandler.RegisterRoutes(api)

	// 其他业务端点。
	api.POST("/snapshot", apiHandler.SnapshotHandler)                         // 快照管理
	api.POST("/memory/search", apiHandler.MemorySearchHandler)                // 记忆检索
	api.GET("/memory/levels", apiHandler.MemoryLevelsHandler)                 // 记忆层级
	api.GET("/memory/eval", apiHandler.MemoryEvalHandler)                     // 记忆评估
	api.GET("/skills", apiHandler.SkillsHandler)                              // Skill 列表
	api.GET("/files", apiHandler.FilesHandler)                                // 文件列表
	api.GET("/files/content", apiHandler.FileContentHandler)                  // 文件内容读取
	api.GET("/fs/browse", apiHandler.BrowseFSHandler)                         // 目录浏览（前端工作目录选择器）
	api.POST("/fs/pick-dir", apiHandler.PickDirHandler)                       // 系统原生目录选择框（服务端主机）
	api.GET("/profile", apiHandler.ProfileHandler)                            // 用户画像查看（TODO #28）
	api.PUT("/profile", apiHandler.SaveProfileHandler)                        // 用户画像编辑
	api.GET("/project/preferences", apiHandler.ProjectPreferencesHandler)     // 项目偏好查看（2026-09-02 设计 §5）
	api.PUT("/project/preferences", apiHandler.SaveProjectPreferencesHandler) // 项目偏好编辑
	api.GET("/project/tester-config", apiHandler.TesterConfigHandler)         // 测试助手验收开关查看（工作目录级）
	api.PUT("/project/tester-config", apiHandler.SaveTesterConfigHandler)     // 测试助手验收开关编辑

	// 自进化技能库 + 进化日志（2026-09-02 设计 §8）。
	api.GET("/skills/learned", apiHandler.ListLearnedSkillsHandler)                  // 技能库列表（含禁用）
	api.GET("/skills/learned/:name", apiHandler.GetLearnedSkillHandler)              // 技能详情 + 文件全文
	api.PUT("/skills/learned/:name", apiHandler.SaveLearnedSkillHandler)             // 手动编辑（重写文件+向量）
	api.POST("/skills/learned/:name/enable", apiHandler.EnableLearnedSkillHandler)   // 启用
	api.POST("/skills/learned/:name/disable", apiHandler.DisableLearnedSkillHandler) // 禁用
	api.GET("/evolution/log", apiHandler.EvolutionLogHandler)                        // 进化审计流水

	// 插件管理 API（设计文档 §5）。
	api.GET("/plugins", apiHandler.ListPluginsHandler)                // 插件列表
	api.GET("/plugins/:id", apiHandler.GetPluginHandler)              // 插件详情
	api.POST("/plugins/:id/enable", apiHandler.EnablePluginHandler)   // 热启用
	api.POST("/plugins/:id/disable", apiHandler.DisablePluginHandler) // 热停用
	api.POST("/plugins/reload", apiHandler.ReloadPluginsHandler)      // 重读配置 + 重扫 plugins.d/

	// 模型动态切换 API（TUI 走进程内直调，不经此端点）。
	api.GET("/models", apiHandler.ListModelsHandler)          // 模型目录：注册表清单 + 各角色当前模型
	api.POST("/models/switch", apiHandler.SwitchModelHandler) // 切换：{role, model_id, thinking}（含连通性探测）
	api.POST("/models", apiHandler.AddModelHandler)           // 新增模型条目：{name?, provider, model, api_key, base_url}

	return router
}

// apiHandlerOf 构造注入了 bootstrap 依赖的 APIHandler。
// 每次调用返回新实例（字段注入为只读配置，无共享可变状态）。
// 参数 app：装配完成的应用上下文。
// 返回值：*server.APIHandler。
func apiHandlerOf(app *App) *server.APIHandler {
	h := server.NewAPIHandler(nil)
	h.SetSessionManager(app.Server)      // 会话管理能力
	h.SetRuntime(app.Runtime)            // 运行时上下文（人格、skill、watchdog 等）
	h.SetStores(app.Postgres, app.Redis) // 持久化与缓存存储
	h.SetRoleConfig(app.RoleConfig)      // 角色配置
	h.SetModelFactory(app.ModelFactory)  // 模型工厂
	h.SetDefaultWorkDir(app.WorkDir)     // 进程默认工作目录（work_dir 为空时的回落目录）
	if mgr, ok := app.Agent.(agent.ModelManager); ok {
		h.SetModelManager(mgr) // 模型动态切换（ReactService 实现；测试桩缺该能力时跳过）
	}
	if app.Postgres != nil {
		h.SetLearnedSkills(app.Postgres.LearnedSkills) // 自进化技能库存储
	}
	return h
}
