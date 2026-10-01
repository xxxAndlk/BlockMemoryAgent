package server

// files_d_http_test.go 钉死 TODO #26 阶段 D 的后端行为：
//   - raw 边界放宽：工作区内非 WriteFile 产物放行、工作区外仍 404；
//   - raw Range：单区间 206 + Content-Range + Accept-Ranges、越界 416、后缀区间；
//   - raw 缓存协商：全量 GET 带 ETag/Last-Modified，If-None-Match 命中 304；
//   - content 分段：offset/limit 翻页、total_size/next_offset 字段、limit 1MB 上限；
//   - files 列表：每项带 mime（消前端 N+1）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// newFilesDTestEnv 构造带工作区（WorkDir=root）会话的测试环境：
// root 下 chart.png 是 WriteFile 产物、notes.txt 是工作区内非产物文件；
// outside 目录在工作区外。返回挂好 raw/content/files 路由的 gin.Engine。
func newFilesDTestEnv(t *testing.T) (r *gin.Engine, root, product, inside, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "workspace")
	outsideDir := filepath.Join(base, "elsewhere")
	for _, d := range []string{root, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	product = filepath.Join(root, "chart.png")
	if err := os.WriteFile(product,
		append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("img-body")...), 0o644); err != nil {
		t.Fatal(err)
	}
	inside = filepath.Join(root, "notes.txt")
	if err := os.WriteFile(inside, []byte("hello workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := newMockAgentForServer()
	mock.sessions["session-1"] = &agent.Session{
		ID:      "session-1",
		Status:  "completed",
		WorkDir: root,
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
	r = gin.New()
	r.GET("/api/files/raw", h.FileRawHandler)
	r.GET("/api/files/content", h.FileContentHandler)
	r.GET("/api/files", h.FilesHandler)
	return r, root, product, inside, outside
}

// TestFileRawHandler_WorkspaceNonProductAllowed 验证阶段 D 边界放宽：
// 工作区内非 WriteFile 产物的文件放行（原 404），工作区外仍 404。
func TestFileRawHandler_WorkspaceNonProductAllowed(t *testing.T) {
	r, _, product, inside, outside := newFilesDTestEnv(t)

	// 产物文件：仍放行（isKnownWriteFilePath 命中路径兼容）。
	rec := rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(product))
	if rec.Code != http.StatusOK {
		t.Fatalf("product status=%d want 200", rec.Code)
	}

	// 工作区内非产物：阶段 D 起放行。
	rec = rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(inside))
	if rec.Code != http.StatusOK {
		t.Fatalf("inside status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hello workspace") {
		t.Fatal("响应体应透传文件字节")
	}

	// 工作区外：仍 404。
	rec = rawGet(t, r, "/api/files/raw?path="+url.QueryEscape(outside))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outside status=%d want 404", rec.Code)
	}
}

// TestFileRawHandler_Range 验证单区间 Range：206 + Content-Range + Accept-Ranges，
// 区间字节正确；终点越界自动截到末尾；后缀区间取末尾 N 字节；整体越界 416。
func TestFileRawHandler_Range(t *testing.T) {
	r, _, _, inside, _ := newFilesDTestEnv(t) // "hello workspace" = 15 字节

	get := func(rangeHeader string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/files/raw?path="+url.QueryEscape(inside), nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	// bytes=0-4 → 206 "hello"
	rec := get("bytes=0-4")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d want 206", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-4/15" {
		t.Fatalf("Content-Range=%q", cr)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatal("应带 Accept-Ranges: bytes")
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body=%q want %q", rec.Body.String(), "hello")
	}

	// bytes=6- → 206 到文件末尾
	rec = get("bytes=6-")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "workspace" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 6-14/15" {
		t.Fatalf("Content-Range=%q", cr)
	}

	// bytes=10-99 → 终点越界截到末尾
	rec = get("bytes=10-99")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "space" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	// bytes=-5 → 后缀区间 "space"
	rec = get("bytes=-5")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "space" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	// bytes=15-20 → 起点越界 416 + Content-Range: bytes */15
	rec = get("bytes=15-20")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status=%d want 416", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes */15" {
		t.Fatalf("Content-Range=%q", cr)
	}
}

// TestFileRawHandler_ETagNotModified 验证全量 GET 带 ETag/Last-Modified，
// If-None-Match 命中（含弱比较 W/ 前缀）回 304。
func TestFileRawHandler_ETagNotModified(t *testing.T) {
	r, _, _, inside, _ := newFilesDTestEnv(t)
	target := "/api/files/raw?path=" + url.QueryEscape(inside)

	rec := rawGet(t, r, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("全量 GET 应带 ETag")
	}
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag=%q 应为弱校验值", etag)
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Fatal("全量 GET 应带 Last-Modified")
	}

	// If-None-Match 命中 → 304
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, target, nil)
	req2.Header.Set("If-None-Match", etag)
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status=%d want 304", rec2.Code)
	}

	// 强比较形式（剥 W/）同样命中
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, target, nil)
	req3.Header.Set("If-None-Match", strings.TrimPrefix(etag, "W/"))
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("bare etag status=%d want 304", rec3.Code)
	}

	// Range 请求带 If-None-Match 不做协商，恒发 206
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodGet, target, nil)
	req4.Header.Set("Range", "bytes=0-1")
	req4.Header.Set("If-None-Match", etag)
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusPartialContent {
		t.Fatalf("range+inm status=%d want 206", rec4.Code)
	}
}

// TestFileContentHandler_OffsetLimitPaging 验证 content 端点 offset/limit 翻页：
// total_size/next_offset 字段、limit 上限 1MB 截断、取尽后 next_offset=null。
func TestFileContentHandler_OffsetLimitPaging(t *testing.T) {
	r, _, _, inside, _ := newFilesDTestEnv(t)
	body := strings.Repeat("abcdefghij", 100) // 1000 字节
	if err := os.WriteFile(inside, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Content    string `json:"content"`
		Size       int64  `json:"size"`
		TotalSize  int64  `json:"total_size"`
		Truncated  bool   `json:"truncated"`
		NextOffset *int64 `json:"next_offset"`
	}
	get := func(q string) resp {
		rec := rawGet(t, r, "/api/files/content?path="+url.QueryEscape(inside)+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var v resp
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 第一页 offset=0 limit=300
	v := get("&offset=0&limit=300")
	if len(v.Content) != 300 || v.Content != body[:300] {
		t.Fatalf("page1 len=%d", len(v.Content))
	}
	if v.TotalSize != 1000 || v.Size != 1000 {
		t.Fatalf("total=%d size=%d want 1000", v.TotalSize, v.Size)
	}
	if !v.Truncated || v.NextOffset == nil || *v.NextOffset != 300 {
		t.Fatalf("truncated=%v next=%v", v.Truncated, v.NextOffset)
	}

	// 第二页从 next_offset 续拉
	v = get("&offset=300&limit=300")
	if v.Content != body[300:600] || *v.NextOffset != 600 {
		t.Fatal("page2 内容或 next_offset 错误")
	}

	// 末页：取尽 → truncated=false、next_offset=null
	v = get("&offset=900&limit=300")
	if v.Content != body[900:] || v.Truncated || v.NextOffset != nil {
		t.Fatal("末页应 truncated=false 且 next_offset=null")
	}

	// offset 越界：空内容收尾不报错
	v = get("&offset=5000&limit=100")
	if v.Content != "" || v.Truncated || v.NextOffset != nil {
		t.Fatal("越界 offset 应返回空片段")
	}

	// limit 超 1MB 截到 1MB（此处文件只有 1000B，等价于取全量）
	v = get("&offset=0&limit=99999999")
	if v.Truncated || len(v.Content) != 1000 {
		t.Fatalf("limit clamp: truncated=%v len=%d", v.Truncated, len(v.Content))
	}

	// 非法参数 400
	rec := rawGet(t, r, "/api/files/content?path="+url.QueryEscape(inside)+"&offset=-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("offset=-1 status=%d want 400", rec.Code)
	}
	rec = rawGet(t, r, "/api/files/content?path="+url.QueryEscape(inside)+"&limit=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status=%d want 400", rec.Code)
	}
}

// TestFilesHandler_MimeField 验证 /api/files 每个文件项带 mime 字段。
func TestFilesHandler_MimeField(t *testing.T) {
	r, _, _, _, _ := newFilesDTestEnv(t)
	rec := rawGet(t, r, "/api/files?session=session-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Files []struct {
			Path string `json:"path"`
			Mime string `json:"mime"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("files=%d want 1", len(resp.Files))
	}
	if resp.Files[0].Mime != "image/png" {
		t.Fatalf("mime=%q want image/png", resp.Files[0].Mime)
	}
}

// TestParseBytesRange 单测区间解析器：非法输入按全量处理，越界区间 416。
func TestParseBytesRange(t *testing.T) {
	size := int64(100)
	cases := []struct {
		header   string
		ranged bool
		unsat  bool
		start  int64
		end    int64
	}{
		{"", false, false, 0, 0},
		{"bytes=", false, false, 0, 0},
		{"items=0-1", false, false, 0, 0},
		{"bytes=abc", false, false, 0, 0},
		{"bytes=0-1,4-5", false, false, 0, 0}, // 多区间不支持 → 全量
		{"bytes=0-9", true, false, 0, 9},
		{"bytes=50-", true, false, 50, 99},
		{"bytes=50-999", true, false, 50, 99},
		{"bytes=-10", true, false, 90, 99},
		{"bytes=-500", true, false, 0, 99},
		{"bytes=100-", false, true, 0, 0},
		{"bytes=99-0", false, true, 0, 0},
		{"bytes=-0", false, false, 0, 0},
	}
	for _, tc := range cases {
		s, e, ranged, unsat := parseBytesRange(tc.header, size)
		if ranged != tc.ranged || unsat != tc.unsat || (ranged && (s != tc.start || e != tc.end)) {
			t.Fatalf("%q => (%d,%d,ranged=%v,unsat=%v), want (%d,%d,ranged=%v,unsat=%v)",
				tc.header, s, e, ranged, unsat, tc.start, tc.end, tc.ranged, tc.unsat)
		}
	}
	// 空文件：任意区间不可满足
	if _, _, ranged, unsat := parseBytesRange("bytes=0-0", 0); ranged || !unsat {
		t.Fatal("空文件 bytes=0-0 应 416")
	}
}
