package server

import (
	"context"
	"sync"

	"encoding/json" // 响应体解析
	"github.com/blockmemory/agent/backend/internal/agent"
	"net/http" // HTTP 方法与状态码
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

// captureControlAgent 记录 Control 调用（mockAgentForServer.Control 是空实现，
// 校验入参需要捕获）。
type captureControlAgent struct {
	mockAgentForServer
	mu    sync.Mutex
	ops   []agent.ControlCommand
	errAt int // 第 N 次调用返回 controlErr（1-based；0=不返回）
	err   error
}

func (c *captureControlAgent) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, cmd)
	if c.errAt != 0 && len(c.ops) == c.errAt {
		return c.err
	}
	return nil
}

// TestHandleSessionWorkDir 验证 POST /api/sessions/:id/workdir：
// 成功 200 回显规范化后的绝对路径；非法/缺失字段 400；会话不存在 404（ErrSessionNotFound 映射）。
func TestHandleSessionWorkDir(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cap := &captureControlAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions/:id/workdir", mgr.HandleSessionWorkDir)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/workdir", strings.NewReader(body))
		r.ServeHTTP(rec, req)
		return rec
	}
	// Windows 路径含反斜杠，手工拼 JSON 易错——统一走 json.Marshal 构造请求体。
	mkBody := func(dir string) string {
		b, _ := json.Marshal(map[string]string{"work_dir": dir})
		return string(b)
	}

	// 1) 成功：相对路径也转绝对，回显服务端权威值。
	rec := do(mkBody(dir))
	if rec.Code != http.StatusOK {
		t.Fatalf("成功用例 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
		WorkDir   string `json:"work_dir"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.SessionID != "s1" || resp.WorkDir != dir {
		t.Fatalf("回显不符: %+v (want dir=%q)", resp, dir)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 || cap.ops[0].Op != agent.ControlOpWorkDir || cap.ops[0].Args["work_dir"] != dir {
		t.Fatalf("Control 入参不符: %+v", cap.ops)
	}
	cap.mu.Unlock()

	// 2) 显式空串 = 清除为进程默认（允许）。
	if rec := do(mkBody("")); rec.Code != http.StatusOK {
		t.Fatalf("清空用例 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 3) 未传字段 = 参数错（不能与"清空"混为一谈）。
	if rec := do(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺字段应 400, got %d", rec.Code)
	}

	// 4) 路径不存在 = 400（与创建会话同一份校验）。
	if rec := do(mkBody(filepath.Join(dir, "nope"))); rec.Code != http.StatusBadRequest {
		t.Fatalf("不存在路径应 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 5) 会话不存在 = 404（agent 层返回 ErrSessionNotFound）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = agent.ErrSessionNotFound
	cap.mu.Unlock()
	if rec := do(mkBody(dir)); rec.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
