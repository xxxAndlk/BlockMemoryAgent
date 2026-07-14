package server

import (
	"net/http"          // HTTP 方法与状态码
	"net/http/httptest" // 测试 HTTP 请求/响应
	"testing"           // 测试框架
)

// TestAuthMiddleware 测试 AuthMiddleware 的鉴权行为。
func TestAuthMiddleware(t *testing.T) {
	handler := AuthMiddleware("secret", []string{"/api/health"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	cases := []struct {
		name       string // 用例名称
		path       string // 请求路径
		auth       string // Authorization 头内容
		wantStatus int    // 期望 HTTP 状态码
	}{
		{"public path without token", "/api/health", "", http.StatusOK},
		{"protected path with valid token", "/api/sessions", "Bearer secret", http.StatusOK},
		{"protected path without token", "/api/sessions", "", http.StatusUnauthorized},
		{"protected path with invalid token", "/api/sessions", "Bearer wrong", http.StatusUnauthorized},
		{"protected path with malformed header", "/api/sessions", "secret", http.StatusUnauthorized},
		{"empty token disables auth", "/api/sessions", "", http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := handler
			// 空 token 场景需要构造独立的中间件。
			if c.name == "empty token disables auth" {
				h = AuthMiddleware("", []string{}, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				})
			}
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("%s: want %d, got %d", c.name, c.wantStatus, rec.Code)
			}
		})
	}
}
