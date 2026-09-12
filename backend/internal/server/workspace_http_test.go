package server

// workspace_http_test.go 钉死工作区文件服务的安全边界与流式语义：
// 正常文件按 Content-Type 返回、越界路径 400、目录/缺失 404、Range 请求 206。
// 这是对话栏媒体渲染（<img>/<video>/<iframe>）的读侧地基，越界防护失守等于把工作目录外
// 的任意文件暴露给浏览器。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// workspaceTestAgent 提供 SessionWorkDir 鸭子类型能力（其余走 mockAgentForServer）。
type workspaceTestAgent struct {
	*mockAgentForServer
	dir string
}

func (w *workspaceTestAgent) SessionWorkDir(string) string { return w.dir }

// newWorkspaceTestRouter 构造只挂工作区路由的测试路由器。
func newWorkspaceTestRouter(root string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	mgr := NewSessionManager(&workspaceTestAgent{mockAgentForServer: newMockAgentForServer(), dir: root})
	r := gin.New()
	r.GET("/api/sessions/:id/workspace/*path", mgr.HandleSessionWorkspace)
	return r
}

func TestHandleSessionWorkspace_ServesFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "assets")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// PNG 魔数 + 少量字节：只验证透传与 Content-Type，不要求是真图。
	payload := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("body")...)
	if err := os.WriteFile(filepath.Join(sub, "a.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	r := newWorkspaceTestRouter(root)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/workspace/assets/a.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q want image/png", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("应带 nosniff")
	}
	if got := rec.Body.Bytes(); string(got) != string(payload) {
		t.Fatalf("响应体与源文件不一致: %d bytes", len(got))
	}

	// 中文文件名/子目录（路径解码，HTML 相对引用常见形态）。
	zh := filepath.Join(root, "效果图")
	if err := os.MkdirAll(zh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zh, "首页.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/workspace/%E6%95%88%E6%9E%9C%E5%9B%BE/%E9%A6%96%E9%A1%B5.html", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("中文路径 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if ct := rec2.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("html Content-Type=%q", ct)
	}
}

func TestHandleSessionWorkspace_RangeRequest(t *testing.T) {
	root := t.TempDir()
	// 视频拖动进度条依赖 Range/206：这里用 mp4 扩展名验证服务端确实按 Range 切片。
	if err := os.WriteFile(filepath.Join(root, "v.mp4"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newWorkspaceTestRouter(root)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/workspace/v.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求应 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Fatalf("Range 切片 got %q want %q", got, "2345")
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Fatalf("Content-Range=%q", cr)
	}
}

func TestHandleSessionWorkspace_RejectsEscapes(t *testing.T) {
	root := t.TempDir()
	// 工作目录外放一个哨兵文件：任何越界写法都不得读到它。
	outside := filepath.Join(filepath.Dir(root), "outside-sentinel.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	r := newWorkspaceTestRouter(root)

	cases := []string{
		"/api/sessions/s1/workspace/..%2Foutside-sentinel.txt",
		"/api/sessions/s1/workspace/a/../../outside-sentinel.txt",
		"/api/sessions/s1/workspace/" + filepath.ToSlash(filepath.Join("..", "outside-sentinel.txt")),
	}
	for _, p := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应被拒（400/404），got %d body=%s", p, rec.Code, rec.Body.String())
		}
		if rec.Body.String() == "SECRET" {
			t.Fatalf("%s 读到了工作目录外的文件", p)
		}
	}

	// 目录与不存在的文件 → 404（只服务普通文件）。
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/sessions/s1/workspace/d", "/api/sessions/s1/workspace/nope.png"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404, got %d", p, rec.Code)
		}
	}
}

func TestHandleSessionWorkspace_UnknownSession(t *testing.T) {
	// SessionWorkDir 返回空串（会话不存在/无目录）→ 404，且不 panic。
	r := newWorkspaceTestRouter("")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/ghost/workspace/a.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知会话应 404, got %d", rec.Code)
	}
}
