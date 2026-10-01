package server

// files_http_test.go 钉死 /api/files/raw 与 /api/files/content 的行为与安全边界：
// raw 只服务会话 WriteFile 产物（越界 404）、按扩展名给 Content-Type、download=1
// 带 attachment、超 20MB 给 413；content 附带 size/mime/truncated 且大文件截断。
// raw 端点一旦被做成任意路径读取，等于把整盘文件暴露给浏览器，这里是必守底线。

import (
	"encoding/json"
	"net/http"
	"net/url"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// newFilesTestEnv 构造带一个"已产出 WriteFile 文件"会话的测试环境。
// 返回：挂好路由的 gin.Engine、产物路径（raw 应放行）、未登记文件路径（raw 应 404）。
func newFilesTestEnv(t *testing.T) (*gin.Engine, string, string) {
	t.Helper()
	root := t.TempDir()

	// 产物文件：登记为会话 WriteFile 事件。
	product := filepath.Join(root, "chart.png")
	payload := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("img-body")...)
	if err := os.WriteFile(product, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	// 未登记文件：存在于磁盘但不属于任何 WriteFile 事件。
	stray := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(stray, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := newMockAgentForServer()
	mock.sessions["session-1"] = &agent.Session{
		ID:     "session-1",
		Status: "completed",
		Events: []agent.Event{{
			Type:      eventkind.ToolExec,
			Tool:      "WriteFile",
			ToolPath:  product,
			Success:   true,
			Timestamp: time.Now(),
		}},
	}

	h := NewAPIHandler(nil)
	h.SetSessionManager(NewSessionManager(mock))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/files/raw", h.FileRawHandler)
	r.GET("/api/files/content", h.FileContentHandler)
	return r, product, stray
}

func rawGet(t *testing.T, r *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// TestFileRawHandler_ServesKnownWriteFile 验证产物文件按真实 Content-Type 透传。
func TestFileRawHandler_ServesKnownWriteFile(t *testing.T) {
	r, product, _ := newFilesTestEnv(t)
	rec := rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(product))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q want image/png", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("应带 nosniff")
	}
	if !strings.Contains(rec.Body.String(), "img-body") {
		t.Fatal("响应体应透传文件字节")
	}
}

// TestFileRawHandler_RejectsUnknownPath 验证未登记的磁盘文件一律 404（不做任意路径读取）。
func TestFileRawHandler_RejectsUnknownPath(t *testing.T) {
	r, _, stray := newFilesTestEnv(t)
	rec := rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(stray))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stray status=%d want 404", rec.Code)
	}
	// 已登记但磁盘上已被删除：同样 404。
	r2, product, _ := newFilesTestEnv(t)
	if err := os.Remove(product); err != nil {
		t.Fatal(err)
	}
	rec2 := rawGet(t, r2, "/api/files/raw?path="+url.QueryEscape(product))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("deleted status=%d want 404", rec2.Code)
	}
}

// TestFileRawHandler_DownloadDisposition 验证 download=1 时给 attachment 且文件名 URL 编码。
func TestFileRawHandler_DownloadDisposition(t *testing.T) {
	r, product, _ := newFilesTestEnv(t)
	rec := rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(product)+"&download=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "chart.png") {
		t.Fatalf("Content-Disposition=%q", cd)
	}
}

// TestFileRawHandler_TooLarge 验证超过 20MB 的产物返回 413。
func TestFileRawHandler_TooLarge(t *testing.T) {
	r, product, _ := newFilesTestEnv(t)
	big := make([]byte, fileRawMaxBytes+1)
	if err := os.WriteFile(product, big, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(product))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", rec.Code)
	}
}

// TestFileContentHandler_MetaAndTruncation 验证 content 端点返回 size/mime/truncated，
// 且超过 300KB 的文本被截断（truncated=true，content 长度为阈值）。
func TestFileContentHandler_MetaAndTruncation(t *testing.T) {
	r, product, _ := newFilesTestEnv(t)
	md := filepath.Join(filepath.Dir(product), "report.md")
	body := strings.Repeat("a", fileContentMaxBytes+100)
	if err := os.WriteFile(md, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := rawGet(t, r, "/api/files/content?path="+url.QueryEscape(md))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Size      int64  `json:"size"`
		Mime      string `json:"mime"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Size != int64(fileContentMaxBytes+100) {
		t.Fatalf("size=%d want %d", resp.Size, fileContentMaxBytes+100)
	}
	if !resp.Truncated {
		t.Fatal("truncated 应为 true")
	}
	if len(resp.Content) != fileContentMaxBytes {
		t.Fatalf("content len=%d want %d", len(resp.Content), fileContentMaxBytes)
	}
	if resp.Mime != "text/markdown; charset=utf-8" {
		t.Fatalf("mime=%q", resp.Mime)
	}
}
