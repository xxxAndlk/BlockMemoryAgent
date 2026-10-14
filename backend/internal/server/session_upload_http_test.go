package server

// session_upload_http_test.go 对话输入文件附件（2026-10-14）的 HTTP 层用例：
// multipart 双模式解析（create/message 两端点）、文件落盘命名消毒与重名后缀、
// 上限拒绝（个数/单文件）、引用文本拼装、JSON 路径回归。

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// newUploadTestEnv 构造带上传能力的测试引擎：mock 注入默认工作目录与会话 session-1
// 的工作目录，并预置 session-1 会话（/message 成功路径末尾的 Get 需要命中）。
// 返回：引擎、mock、默认目录、会话目录。
func newUploadTestEnv(t *testing.T) (*gin.Engine, *mockAgentForServer, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mock := newMockAgentForServer()
	defDir := t.TempDir()
	sessDir := t.TempDir()
	mock.defaultWorkDir = defDir
	mock.workDirs = map[string]string{"session-1": sessDir}
	mock.sessions["session-1"] = &agent.Session{
		ID: "session-1", Goal: "g", Status: string(enums.SessionStatusCompleted), StartedAt: time.Now(),
	}
	r := gin.New()
	RegisterSessionRoutes(r.Group("/api"), NewSessionManager(mock))
	return r, mock, defDir, sessDir
}

// postMultipart 发一个 multipart POST：fields 为文本字段，files 为 文件名->内容。
func postMultipart(t *testing.T, r *gin.Engine, path string, fields, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("写字段 %s: %v", k, err)
		}
	}
	for name, content := range files {
		fw, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("创建文件 part %s: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("写文件 part %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// mustStat 断言文件已落盘。
func mustStat(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("文件应已落盘 %s: %v", p, err)
	}
}

// TestCreateSession_MultipartUpload 创建会话带文件：落盘到默认工作目录的
// .bma/uploads/，引用文本拼进 goal，结构化附件随 CreateRequest 透传。
func TestCreateSession_MultipartUpload(t *testing.T) {
	r, mock, defDir, _ := newUploadTestEnv(t)
	content := "CREATE TABLE t(id int);"
	rec := postMultipart(t, r, "/api/sessions",
		map[string]string{"goal": "分析这个SQL"},
		map[string]string{"app.sql": content})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mustStat(t, filepath.Join(defDir, ".bma", "uploads", "app.sql"))

	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if len(mock.createdReqs) != 1 {
		t.Fatalf("应收到 1 个创建请求, got %d", len(mock.createdReqs))
	}
	req := mock.createdReqs[0]
	if !strings.Contains(req.Goal, "分析这个SQL") || !strings.Contains(req.Goal, "[附件 1: app.sql | .bma/uploads/app.sql |") {
		t.Fatalf("goal 应含原文与附件引用行: %q", req.Goal)
	}
	if len(req.Attachments) != 1 || req.Attachments[0].Path != ".bma/uploads/app.sql" || req.Attachments[0].Size != int64(len(content)) {
		t.Fatalf("Attachments 不符: %+v", req.Attachments)
	}
	if req.WorkDir != "" {
		t.Fatalf("未显式 work_dir 应保持空串回落语义, got %q", req.WorkDir)
	}
}

// TestCreateSession_MultipartFileOnlyGoalOptional 只带文件不带 goal：放行，
// goal 即附件引用块（引用行拼入后非空）。
func TestCreateSession_MultipartFileOnlyGoalOptional(t *testing.T) {
	r, mock, defDir, _ := newUploadTestEnv(t)
	rec := postMultipart(t, r, "/api/sessions", nil, map[string]string{"only.sql": "select 1;"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mustStat(t, filepath.Join(defDir, ".bma", "uploads", "only.sql"))
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if req := mock.createdReqs[0]; !strings.HasPrefix(req.Goal, "[附件 1: only.sql | .bma/uploads/only.sql |") {
		t.Fatalf("goal 应为附件引用块: %q", req.Goal)
	}
}

// TestCreateSession_MultipartEmptyGoalNoFile 无 goal 且无文件：仍 400（原语义不变）。
func TestCreateSession_MultipartEmptyGoalNoFile(t *testing.T) {
	r, _, _, _ := newUploadTestEnv(t)
	rec := postMultipart(t, r, "/api/sessions", map[string]string{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

// TestCreateSession_MultipartExplicitWorkDir 显式 work_dir 带文件：落盘到该目录
// （不是默认目录），且 CreateRequest.WorkDir 原样透传。
func TestCreateSession_MultipartExplicitWorkDir(t *testing.T) {
	r, mock, _, _ := newUploadTestEnv(t)
	custom := t.TempDir()
	rec := postMultipart(t, r, "/api/sessions",
		map[string]string{"goal": "g", "work_dir": custom},
		map[string]string{"w.sql": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mustStat(t, filepath.Join(custom, ".bma", "uploads", "w.sql"))
	want, err := filepath.Abs(custom)
	if err != nil {
		t.Fatal(err)
	}
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if got := mock.createdReqs[0].WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
}

// TestSessionMessage_MultipartWithContent 发消息带文件（中文文件名保留）：
// 落盘到会话工作目录，content 尾部拼引用行，结构化附件随 Message 透传。
func TestSessionMessage_MultipartWithContent(t *testing.T) {
	r, mock, _, sessDir := newUploadTestEnv(t)
	rec := postMultipart(t, r, "/api/sessions/session-1/message",
		map[string]string{"content": "帮我看看"},
		map[string]string{"数据.sql": "select 1;"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mustStat(t, filepath.Join(sessDir, ".bma", "uploads", "数据.sql"))

	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if len(mock.sentMsgs) != 1 {
		t.Fatalf("应收到 1 条消息, got %d", len(mock.sentMsgs))
	}
	msg := mock.sentMsgs[0]
	if !strings.HasPrefix(msg.Content, "帮我看看\n\n[附件 1: 数据.sql | .bma/uploads/数据.sql |") {
		t.Fatalf("content 应含原文与引用行: %q", msg.Content)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "数据.sql" || msg.Attachments[0].Size != 9 {
		t.Fatalf("Attachments 不符: %+v", msg.Attachments)
	}
}

// TestSessionMessage_MultipartFileOnly 只带文件不带文字：不 400，content 即引用块
// （服务端拼装后非空，可正常注入/入历史）。
func TestSessionMessage_MultipartFileOnly(t *testing.T) {
	r, mock, _, _ := newUploadTestEnv(t)
	rec := postMultipart(t, r, "/api/sessions/session-1/message", nil, map[string]string{"a.txt": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if msg := mock.sentMsgs[0]; msg.Content != "[附件 1: a.txt | .bma/uploads/a.txt | 1 B]" {
		t.Fatalf("content 应为引用块: %q", msg.Content)
	}
}

// TestSessionMessage_MultipartCollisionSuffix 连续两次上传同名文件：第二次落
// -1 后缀副本，不覆盖原文件。
func TestSessionMessage_MultipartCollisionSuffix(t *testing.T) {
	r, mock, _, sessDir := newUploadTestEnv(t)
	for i := 0; i < 2; i++ {
		rec := postMultipart(t, r, "/api/sessions/session-1/message",
			map[string]string{"content": "x"}, map[string]string{"a.sql": "select 1;"})
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	mustStat(t, filepath.Join(sessDir, ".bma", "uploads", "a.sql"))
	mustStat(t, filepath.Join(sessDir, ".bma", "uploads", "a-1.sql"))
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if len(mock.sentMsgs) != 2 || !strings.Contains(mock.sentMsgs[1].Content, ".bma/uploads/a-1.sql") {
		t.Fatalf("第二次应引用 a-1.sql: %+v", mock.sentMsgs)
	}
}

// TestSessionMessage_MultipartTooManyFiles 超过个数上限（8+1）：400。
func TestSessionMessage_MultipartTooManyFiles(t *testing.T) {
	r, _, _, _ := newUploadTestEnv(t)
	files := map[string]string{}
	for i := 0; i < maxUploadFiles+1; i++ {
		files[fmt.Sprintf("f%d.sql", i)] = "x"
	}
	rec := postMultipart(t, r, "/api/sessions/session-1/message", map[string]string{"content": "x"}, files)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

// TestSessionMessage_MultipartFileTooLarge 单文件超过 64MiB 上限：400 且文案含超限说明，
// 半截文件不落盘。
func TestSessionMessage_MultipartFileTooLarge(t *testing.T) {
	r, _, _, sessDir := newUploadTestEnv(t)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("content", "x")
	fw, err := w.CreateFormFile("files", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := 0; i < (maxUploadFileBytes>>20)+1; i++ {
		if _, err := fw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/message", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "超过大小上限") {
		t.Fatalf("文案应含超限说明: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(sessDir, ".bma", "uploads", "big.bin")); err == nil {
		t.Fatal("超限文件不应落盘")
	}
}

// TestSessionMessage_JSONRegression JSON 路径零行为变化：Content 原样、无附件。
func TestSessionMessage_JSONRegression(t *testing.T) {
	r, mock, _, _ := newUploadTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/message", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if len(mock.sentMsgs) != 1 || mock.sentMsgs[0].Content != "hi" || len(mock.sentMsgs[0].Attachments) != 0 {
		t.Fatalf("JSON 路径应零变化: %+v", mock.sentMsgs)
	}
}

// TestSanitizeUploadName 落盘文件名消毒：剥目录/非法字符/首尾点与空白，保留中文，
// 非法 UTF-8 或空名回落 file。
func TestSanitizeUploadName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a.sql", "a.sql"},
		{"数据 表.sql", "数据 表.sql"},
		{"../evil.sql", "evil.sql"},
		{`C:\dir\a.sql`, "a.sql"},
		{"/tmp/a.sql", "a.sql"},
		{"a*b?.sql", "ab.sql"},
		{"a<b>c|d:e\"f.sql", "abcdef.sql"},
		{".hidden", "hidden"},
		{"....", "file"},
		{"", "file"},
		{"a\xffb.sql", "file"},
	}
	for _, c := range cases {
		if got := sanitizeUploadName(c.in); got != c.want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
