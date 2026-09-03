package server

import (
	"encoding/json" // 响应体解析
	"net/http"      // HTTP 方法与状态码
	"net/http/httptest"
	"net/url"       // query 参数转义
	"os"            // 临时目录/文件构造
	"path/filepath" // 路径拼接
	"runtime"       // 盘符根用例的 GOOS 判定
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

// TestProjectPreferences_WorkDirValidation 验证项目偏好端点（终审修复 I-2）的非法 work_dir
// 在到达 agent 前被拦为 400：GET 走 query 参数，PUT 走 body 字段。
func TestProjectPreferences_WorkDirValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &APIHandler{sessionMgr: &SessionManager{agent: newMockAgentForServer()}}
	r := gin.New()
	r.GET("/api/project/preferences", h.ProjectPreferencesHandler)
	r.PUT("/api/project/preferences", h.SaveProjectPreferencesHandler)

	bad := filepath.Join("Z:", "no-such-dir-xyz")
	// GET:query 参数 work_dir
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/project/preferences?work_dir="+url.QueryEscape(bad), nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET status %d, want 400", w.Code)
	}
	// PUT:body 字段 work_dir
	body := `{"content":"x","work_dir":"` + strings.ReplaceAll(bad, "\\", "\\\\") + `"}`
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPut, "/api/project/preferences", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("PUT status %d, want 400", w2.Code)
	}
	// 合法 work_dir（临时目录）正常放行 200
	good := t.TempDir()
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/project/preferences?work_dir="+url.QueryEscape(good), nil)
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("GET(valid) status %d, want 200", w3.Code)
	}
}

// TestBrowseFS_RootHasEmptyParent 验证终审修复 M-6:盘符根（如 C:\）的 parent 字段返回空
// （filepath.Dir 对根返回自身，前端据此禁用"上级"按钮）。
func TestBrowseFS_RootHasEmptyParent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("盘符根为 Windows 语义")
	}
	gin.SetMode(gin.TestMode)
	h := &APIHandler{} // BrowseFSHandler 不触任何注入依赖
	r := gin.New()
	r.GET("/api/fs/browse", h.BrowseFSHandler)

	root := filepath.VolumeName(t.TempDir()) + `\`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/fs/browse?path="+url.QueryEscape(root), nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Parent string `json:"parent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Parent != "" {
		t.Fatalf("parent got %q, want empty for drive root", resp.Parent)
	}
}
