package server

// routes.go 提供会话路由的统一注册入口（Gin 版）。
// 生产路由（bootstrap/router.go）、TUI 本地服务（cmd/tui）与测试复用同一注册逻辑，
// 避免三处手工维护重复的路由清单。

import "github.com/gin-gonic/gin" // Gin Web 框架

// RegisterSessionRoutes 把全部 /sessions* 端点注册到指定路由器（组或引擎）。
// 路径使用相对形式（/sessions...），调用方决定是否拼接 /api 前缀与鉴权中间件。
// 参数 rg：Gin 路由器（*gin.Engine 或 *gin.RouterGroup）；m：会话管理器（handler 绑定接收者）。
func RegisterSessionRoutes(rg gin.IRouter, m *SessionManager) {
	// 集合端点：GET 列出会话 / POST 创建会话。
	rg.GET("/sessions", m.HandleListSessions)
	rg.POST("/sessions", m.HandleCreateSession)

	// 会话子资源：handler 内部通过 c.Param("id") 读取会话 ID。
	rg.GET("/sessions/:id/stream", m.HandleSessionStream)                   // SSE 流式消息推送
	rg.POST("/sessions/:id/message", m.HandleSessionMessage)                // 向会话发送消息
	rg.GET("/sessions/:id/board", m.HandleSessionBoard)                     // 任务看板
	rg.GET("/sessions/:id/agents", m.HandleSessionAgents)                   // 会话内 Agent 列表
	rg.GET("/sessions/:id/tree", m.HandleSessionTree)                       // 权威 Agent 树快照
	rg.POST("/sessions/:id/agents/:aid/cancel", m.HandleSessionAgentCancel) // 取消子 Agent 实例
	rg.GET("/sessions/:id/metrics", m.HandleSessionMetrics)                 // 会话级指标
	rg.GET("/sessions/:id/logs", m.HandleSessionLogs)                       // 会话日志查询
	rg.GET("/sessions/:id/token-metrics", m.HandleSessionTokenMetrics)      // Token 消耗指标
	rg.GET("/sessions/:id/watchdog", m.HandleSessionWatchdog)               // Watchdog 状态
	rg.GET("/sessions/:id/mailbox", m.HandleSessionMailbox)                 // 邮箱消息
	rg.POST("/sessions/:id/clarify", m.HandleSessionClarify)                // 澄清答复
	rg.POST("/sessions/:id/interrupt", m.HandleSessionInterrupt)            // 抢占中断
	rg.POST("/sessions/:id/enqueue", m.HandleSessionEnqueue)                // 任务入队
	rg.POST("/sessions/:id/cancel", m.HandleSessionCancel)                  // 取消会话
	rg.POST("/sessions/:id/stop", m.HandleSessionStop)                      // 软停止（可续跑，TODO #37）
	rg.POST("/sessions/:id/topic", m.HandleSessionTopic)                    // 话题管理
	rg.GET("/sessions/:id", m.HandleGetSession)                             // 会话详情
}
