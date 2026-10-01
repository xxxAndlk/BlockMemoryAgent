package server

// files_write_http_test.go 钉死 PUT /api/files/content（TODO #26 阶段 G）的行为：
// 工作区内写入成功（内容/mtime 正确 + .bak 生成）、工作区外 404、
// base_mtime 冲突 409 {code:"conflict"}、超 5MB 413、.bak 一次性备份不被二次覆盖。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// newFilesWriteTestEnv 构造带一个"已登记 WorkDir"会话的测试环境。
// 返回：挂好 PUT 路由的 gin.Engine、会话工作区根、工作区外目录。
func newFilesWriteTestEnv(t *testing.T) (*gin.Engine, string, string) {
	t.Helper()
	root := t.TempDir()    // 会话工作区
	outside := t.TempDir() // 工作区外

	mock := newMockAgentForServer()
	mock.sessions["session-1"] = &agent.Session{
		ID:      "session-1",
		Status:  "completed",
		WorkDir: root,
	}

	h := NewAPIHandler(nil)
	h.SetSessionManager(NewSessionManager(mock))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/api/files/content", h.SaveFileContentHandler)
	return r, root, outside
}

// putContent 发 PUT /api/files/content，baseMtime 非 nil 时带上冲突检测。
func putContent(t *testing.T, r *gin.Engine, path, content string, baseMtime *int64) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"path": path, "content": content, "base_mtime": baseMtime})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/files/content", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// fileMtimeMilli 读磁盘 mtime（Unix 毫秒），测试构造 base_mtime 用。
func fileMtimeMilli(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixMilli()
}

// TestSaveFileContent_Success 验证工作区内写入成功：磁盘内容变更、
// 响应 {ok,mtime,size} 正确、生成 .bak 且内容为写入前原稿。
func TestSaveFileContent_Success(t *testing.T) {
	r, root, _ := newFilesWriteTestEnv(t)
	target := filepath.Join(root, "notes.md")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := fileMtimeMilli(t, target)

	rec := putContent(t, r, target, "edited content", &base)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Ok    bool  `json:"ok"`
		Mtime int64 `json:"mtime"`
		Size  int64 `json:"size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Ok || resp.Mtime == 0 || resp.Size != int64(len("edited content")) {
		t.Fatalf("resp=%+v", resp)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "edited content" {
		t.Fatalf("disk content=%q err=%v", data, err)
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil || string(bak) != "original" {
		t.Fatalf(".bak=%q err=%v（应含写入前原稿）", bak, err)
	}
}

// TestSaveFileContent_RejectsOutsideWorkspace 验证工作区外路径一律 404（不做任意写）。
func TestSaveFileContent_RejectsOutsideWorkspace(t *testing.T) {
	r, _, outside := newFilesWriteTestEnv(t)
	stray := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(stray, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := putContent(t, r, stray, "hacked", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
	data, _ := os.ReadFile(stray)
	if string(data) != "keep me" {
		t.Fatal("工作区外文件不应被改动")
	}
	// 不存在的工作区内路径：同样 404（在线编辑只针对已存在文件）。
	rec2 := putContent(t, r, filepath.Join(outside, "ghost.txt"), "x", nil)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("missing file status=%d want 404", rec2.Code)
	}
}

// TestSaveFileContent_Conflict 验证 base_mtime 与磁盘 mtime 不一致时 409，
// 响应带 code=conflict 与 current_mtime，且磁盘不被改动。
func TestSaveFileContent_Conflict(t *testing.T) {
	r, root, _ := newFilesWriteTestEnv(t)
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := fileMtimeMilli(t, target) - 60_000 // 模拟"打开后外部已改"

	rec := putContent(t, r, target, "v2", &stale)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code         string `json:"code"`
		CurrentMtime int64  `json:"current_mtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "conflict" || resp.CurrentMtime != fileMtimeMilli(t, target) {
		t.Fatalf("resp=%+v", resp)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "v1" {
		t.Fatal("冲突时磁盘不应被改动")
	}
}

// TestSaveFileContent_TooLarge 验证超 5MB 内容返回 413。
func TestSaveFileContent_TooLarge(t *testing.T) {
	r, root, _ := newFilesWriteTestEnv(t)
	target := filepath.Join(root, "big.txt")
	if err := os.WriteFile(target, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := putContent(t, r, target, strings.Repeat("a", fileWriteMaxBytes+1), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", rec.Code)
	}
}

// TestSaveFileContent_BakNotOverwritten 验证 .bak 一次性语义：
// 第一次保存生成 .bak（原稿），此后无论再保存多少次、外部如何改动，.bak 保持首次备份。
func TestSaveFileContent_BakNotOverwritten(t *testing.T) {
	r, root, _ := newFilesWriteTestEnv(t)
	target := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 第一次保存：v1 → v2，.bak 应为 v1。
	rec := putContent(t, r, target, "v2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	// 外部修改（模拟别的进程写入 v3）后第二次保存 v4：.bak 不得覆盖成 v3。
	time.Sleep(10 * time.Millisecond) // 保证 mtime 变化
	if err := os.WriteFile(target, []byte("v3-external"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec2 := putContent(t, r, target, "v4", nil) // 不带 base_mtime = 前端「覆盖」路径
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d", rec2.Code)
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil || string(bak) != "v1" {
		t.Fatalf(".bak=%q err=%v（应保持首次备份 v1）", bak, err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "v4" {
		t.Fatalf("content=%q want v4", data)
	}
}
