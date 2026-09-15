package server

// export_http_test.go 数据导出端点（TODO #18-2 T29）：
// nil 依赖产出合法 zip（round-trip 解包核对条目名 + JSON 可解析）；.bma 产物树
// 打包（含跳过超大文件的截断语义）。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// serveZip 起 test 路由请求 handler 并解出 zip。
func serveZip(t *testing.T, h *APIHandler, path string, route func(*gin.Engine)) *zip.Reader {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	route(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %s", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip round-trip: %v", err)
	}
	return zr
}

// TestSessionExport_NilDepsValidZip 验证零注入依赖时导出仍是合法 zip，
// 四类数据条目齐全且 JSON 可解析（round-trip）。
func TestSessionExport_NilDepsValidZip(t *testing.T) {
	h := &APIHandler{}
	zr := serveZip(t, h, "/api/sessions/session-1/export", func(r *gin.Engine) {
		r.GET("/api/sessions/:id/export", h.SessionExportHandler)
	})
	want := map[string]bool{
		"session_history.json": false, // nil 库无历史 → 条目可缺
		"session_events.json":  false,
		"agent_events.json":    false,
		"agent_messages.json":  false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; !ok {
			t.Fatalf("意外条目 %s", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var v any
		err = json.NewDecoder(rc).Decode(&v)
		rc.Close()
		if err != nil {
			t.Fatalf("条目 %s JSON 不可解析: %v", f.Name, err)
		}
		want[f.Name] = true
	}
	for name, seen := range want {
		if name == "session_history.json" {
			continue // nil 库：历史条目允许缺席
		}
		if !seen {
			t.Fatalf("缺条目 %s", name)
		}
	}
}

// TestMemoryExport_NilDepsValidZip 验证记忆导出 nil 依赖时输出合法（空）zip。
func TestMemoryExport_NilDepsValidZip(t *testing.T) {
	h := &APIHandler{}
	zr := serveZip(t, h, "/api/export/memory", func(r *gin.Engine) {
		r.GET("/api/export/memory", h.MemoryExportHandler)
	})
	if len(zr.File) != 0 {
		t.Fatalf("nil 依赖应产出空 zip，got %d 条目", len(zr.File))
	}
}

// TestAddDirToZip_TreeAndSkip 验证 .bma 产物树打包：普通文件按相对路径进包，
// 超大文件跳过，目录不存在整体 no-op。
func TestAddDirToZip_TreeAndSkip(t *testing.T) {
	dir := t.TempDir()
	bma := filepath.Join(dir, ".bma")
	if err := os.MkdirAll(filepath.Join(bma, "tool_outputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bma, "tool_outputs", "a.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(bma, "images", "big.png")
	if err := os.MkdirAll(filepath.Join(bma, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, exportMaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	addDirToZip(zw, bma, "workspace/.bma")
	zw.Close()

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["workspace/.bma/tool_outputs/a.md"] {
		t.Fatalf("产物文件应进包: %v", names)
	}
	if names["workspace/.bma/images/big.png"] {
		t.Fatalf("超大文件应跳过: %v", names)
	}

	// 目录不存在：no-op 不 panic
	var buf2 bytes.Buffer
	zw2 := zip.NewWriter(&buf2)
	addDirToZip(zw2, filepath.Join(dir, "no-such"), "workspace/.bma")
	zw2.Close()
}
