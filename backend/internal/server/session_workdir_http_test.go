package server

import (
	"encoding/json" // 响应体解析
	"net/http"      // HTTP 方法与状态码
	"net/http/httptest"
	"os"            // 临时目录/文件构造
	"path/filepath" // 路径拼接
	"strings"       // 请求体构造
	"testing"       // 测试框架

	"github.com/gin-gonic/gin" // Gin Web 框架（测试路由）
)

// TestBrowseFS_ReturnsDirsOnly 验证 GET /api/fs/browse 只返回目录（不返回文件）。
func TestBrowseFS_ReturnsDirsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	mkDir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkDir("a/b")
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644)

	h := &APIHandler{} // BrowseFSHandler 不触任何注入依赖
	r := gin.New()
	r.GET("/api/fs/browse", h.BrowseFSHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/fs/browse?path="+root, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
		Dirs []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"dirs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Dirs) != 1 || resp.Dirs[0].Name != "a" {
		t.Fatalf("dirs %+v, want only [a]", resp.Dirs)
	}
}

// TestCreateSession_WorkDirValidation 验证非法 work_dir（不存在）在到达 agent 前被拦为 400。
func TestCreateSession_WorkDirValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := &SessionManager{agent: newMockAgentForServer()}
	r := gin.New()
	r.POST("/api/sessions", m.HandleCreateSession)

	body := `{"goal":"g","work_dir":"` + strings.ReplaceAll(filepath.Join("Z:", "no-such-dir-xyz"), "\\", "\\\\") + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}
