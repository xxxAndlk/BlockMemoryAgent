package server

import (
	"net/http"          // HTTP 方法与状态码
	"net/http/httptest" // 测试 HTTP 请求/响应
	"testing"           // 测试框架

	"github.com/gin-gonic/gin" // Gin Web 框架（测试路由）
)

// newAuthTestRouter 构造挂载 GinAuthMiddleware 的最小路由，供表驱动用例请求。
// 参数 token：合法 token；publicPaths：公开路径前缀。
// 返回值：*gin.Engine。
func newAuthTestRouter(token string, publicPaths []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinAuthMiddleware(token, publicPaths))
	ok := func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	}
	r.GET("/api/health", ok)   // 公开路径示例
	r.GET("/api/sessions", ok) // 受保护路径示例
	return r
}

// TestGinAuthMiddleware 测试 GinAuthMiddleware 的鉴权行为。
func TestGinAuthMiddleware(t *testing.T) {
	cases := []struct {
		name       string // 用例名称
		token      string // 中间件配置的合法 token
		path       string // 请求路径
		auth       string // Authorization 头内容
		wantStatus int    // 期望 HTTP 状态码
	}{
		{"public path without token", "secret", "/api/health", "", http.StatusOK},
		{"protected path with valid token", "secret", "/api/sessions", "Bearer secret", http.StatusOK},
		{"protected path without token", "secret", "/api/sessions", "", http.StatusUnauthorized},
		{"protected path with invalid token", "secret", "/api/sessions", "Bearer wrong", http.StatusUnauthorized},
		{"protected path with malformed header", "secret", "/api/sessions", "secret", http.StatusUnauthorized},
		{"empty token disables auth", "", "/api/sessions", "", http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 空 token 场景需要构造独立的中间件。
			token := c.token
			if c.name == "empty token disables auth" {
				token = ""
			}
			r := newAuthTestRouter(token, []string{"/api/health"})
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("%s: want %d, got %d", c.name, c.wantStatus, rec.Code)
			}
		})
	}
}
