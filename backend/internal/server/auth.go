package server

// 本文件实现简单的 Bearer Token 鉴权中间件（Gin 版）。
// 设计目标：为 HTTP API 提供最小可运维的访问控制，防止任意客户端直接操作会话与工具。
// 如需更复杂的鉴权（OAuth2、RBAC），应在此基础上扩展。

import (
	"net/http" // HTTP 状态码
	"strings"  // 字符串前缀匹配

	"github.com/gin-gonic/gin" // Gin Web 框架
)

// GinAuthMiddleware 返回一个 Gin 中间件，校验请求头中的 Authorization Bearer Token。
//
// 参数：
//   - token：合法的 API Token；空字符串时中间件直接放行（兼容未启用鉴权的部署）。
//   - publicPaths：不需要鉴权的路径前缀列表（如 /api/health 供 k8s 探针使用）。
//     生产路由中公开端点直接注册在无鉴权组，此参数主要为测试与白名单场景保留。
//
// 返回：gin.HandlerFunc 中间件；未通过校验时返回 401 并终止后续处理。
//
// 规则：
//   - token 为空视为未启用鉴权，直接放行。
//   - 请求路径以 publicPaths 中任一前缀开头，直接放行。
//   - Authorization 头为 "Bearer <token>" 且 token 匹配，放行。
//   - 其他情况返回 401 Unauthorized。
func GinAuthMiddleware(token string, publicPaths []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// token 为空视为未启用鉴权，直接放行
		if token == "" {
			c.Next()
			return
		}

		// 公开路径白名单（前缀匹配）
		path := c.Request.URL.Path
		for _, prefix := range publicPaths {
			if prefix != "" && strings.HasPrefix(path, prefix) {
				c.Next()
				return
			}
		}

		// 校验 Bearer Token
		auth := c.Request.Header.Get("Authorization")
		const prefix = "Bearer "
		if strings.HasPrefix(auth, prefix) && strings.TrimSpace(auth[len(prefix):]) == token {
			c.Next()
			return
		}

		c.Header("WWW-Authenticate", `Bearer realm="BlockMemoryAgent"`)
		c.String(http.StatusUnauthorized, "Unauthorized")
		c.Abort()
	}
}
