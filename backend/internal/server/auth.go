package server

// 本文件实现简单的 Bearer Token 鉴权中间件。
// 设计目标：为 HTTP API 提供最小可运维的访问控制，防止任意客户端直接操作会话与工具。
// 如需更复杂的鉴权（OAuth2、RBAC），应在此基础上扩展。

import (
	"net/http" // HTTP 中间件与状态码
	"strings"  // 字符串前缀匹配
)

// AuthMiddleware 返回一个 HTTP 中间件，校验请求头中的 Authorization Bearer Token。
//
// 参数：
//   - token：合法的 API Token；空字符串时中间件直接放行（兼容未启用鉴权的部署）。
//   - publicPaths：不需要鉴权的路径前缀列表（如 /api/health 供 k8s 探针使用）。
//   - next：下游处理器。
//
// 返回：包装后的 http.HandlerFunc。
//
// 规则：
//   - 请求路径以 publicPaths 中任一前缀开头，直接放行。
//   - Authorization 头为 "Bearer <token>" 且 token 匹配，放行。
//   - 其他情况返回 401 Unauthorized。
func AuthMiddleware(token string, publicPaths []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// token 为空视为未启用鉴权，直接放行
		if token == "" {
			next(w, r)
			return
		}

		// 公开路径白名单
		for _, prefix := range publicPaths {
			if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
				next(w, r)
				return
			}
		}

		// 校验 Bearer Token
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if strings.HasPrefix(auth, prefix) && strings.TrimSpace(auth[len(prefix):]) == token {
			next(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="BlockMemoryAgent"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}
