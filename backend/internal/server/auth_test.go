package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	handler := AuthMiddleware("secret", []string{"/api/health"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	cases := []struct {
		name       string
		path       string
		auth       string
		wantStatus int
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
