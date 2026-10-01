package server

// files_reveal_test.go 钉死 GET /api/files/reveal 的行为（TODO #26 阶段 C）：
// 路径边界与 /api/files/raw 一致（WriteFile 产物或会话工作区内，越界/不存在 404）、
// 拉起函数注入桩（不真弹文件管理器）、拉起失败 5xx + 信息。

import (
	"errors"
	"net/http"
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

// stubRevealFn 临时替换 launchRevealFn（测试桩点），测试结束自动还原。
func stubRevealFn(t *testing.T, fn func(path string) error) {
	t.Helper()
	old := launchRevealFn
	launchRevealFn = fn
	t.Cleanup(func() { launchRevealFn = old })
}

// newRevealTestEnv 构造带 WriteFile 产物 + 会话工作区的测试环境：
// 产物 report.md（WriteFile 事件登记）、工作区内非产物 inner.txt（仅 WorkDir 边界）、
// 越界 stray.md（磁盘存在但无任何登记）。
func newRevealTestEnv(t *testing.T) (*gin.Engine, string, string, string) {
	t.Helper()
	root := t.TempDir()
	product := filepath.Join(root, "report.md")
	if err := os.WriteFile(product, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(root, "inner.txt")
	if err := os.WriteFile(inner, []byte("in workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	strayRoot := t.TempDir()
	stray := filepath.Join(strayRoot, "stray.md")
	if err := os.WriteFile(stray, []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := newMockAgentForServer()
	mock.sessions["session-1"] = &agent.Session{
		ID:      "session-1",
		Status:  "completed",
		WorkDir: root, // 工作区边界：root 内任意路径可 reveal
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
	r.GET("/api/files/reveal", h.FileRevealHandler)
	return r, product, inner, stray
}

func revealGet(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/files/reveal?path="+path, nil)
	r.ServeHTTP(rec, req)
	return rec
}

// TestFileReveal_RejectsOutOfBounds 验证越界路径一律 404：
// 磁盘存在但不属于任何 WriteFile 事件/会话工作区，且拉起函数不得被调用。
func TestFileReveal_RejectsOutOfBounds(t *testing.T) {
	called := false
	stubRevealFn(t, func(path string) error { called = true; return nil })
	r, _, _, stray := newRevealTestEnv(t)

	rec := revealGet(t, r, stray)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stray status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("越界路径不应触发拉起")
	}
}

// TestFileReveal_RejectsMissing400 验证缺 path 400。
func TestFileReveal_RejectsMissing400(t *testing.T) {
	stubRevealFn(t, func(path string) error { return nil })
	r, _, _, _ := newRevealTestEnv(t)
	rec := revealGet(t, r, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

// TestFileReveal_RejectsDeleted404 验证已登记但磁盘已删除的文件 404（与 raw 语义一致）。
func TestFileReveal_RejectsDeleted404(t *testing.T) {
	stubRevealFn(t, func(path string) error { return nil })
	r, product, _, _ := newRevealTestEnv(t)
	if err := os.Remove(product); err != nil {
		t.Fatal(err)
	}
	rec := revealGet(t, r, product)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// TestFileReveal_LaunchesForProduct 验证 WriteFile 产物触发拉起（参数为原路径）。
func TestFileReveal_LaunchesForProduct(t *testing.T) {
	var got string
	stubRevealFn(t, func(path string) error { got = path; return nil })
	r, product, _, _ := newRevealTestEnv(t)

	rec := revealGet(t, r, product)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got != product {
		t.Fatalf("reveal path=%q want %q", got, product)
	}
}

// TestFileReveal_LaunchesForWorkspaceNonProduct 验证阶段 D 放宽后的工作区边界：
// 非 WriteFile 产物但位于会话 WorkDir 内的文件同样可 reveal（与 raw 一致）。
func TestFileReveal_LaunchesForWorkspaceNonProduct(t *testing.T) {
	var got string
	stubRevealFn(t, func(path string) error { got = path; return nil })
	r, _, inner, _ := newRevealTestEnv(t)

	rec := revealGet(t, r, inner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got != inner {
		t.Fatalf("reveal path=%q want %q", got, inner)
	}
}

// TestFileReveal_LaunchFailure500 验证拉起失败回 5xx + 错误信息（无桌面环境前端据此降级）。
func TestFileReveal_LaunchFailure500(t *testing.T) {
	stubRevealFn(t, func(path string) error { return errors.New("no desktop") })
	r, product, _, _ := newRevealTestEnv(t)

	rec := revealGet(t, r, product)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if body := rec.Body.String(); !containsAll(body, "在文件夹中显示失败", "no desktop") {
		t.Fatalf("body=%q", body)
	}
}

// TestFileReveal_RevealsDirectory 验证目录也可 reveal（面包屑定位目录段用）。
func TestFileReveal_RevealsDirectory(t *testing.T) {
	var got string
	stubRevealFn(t, func(path string) error { got = path; return nil })
	r, product, _, _ := newRevealTestEnv(t)
	dir := filepath.Dir(product)

	rec := revealGet(t, r, dir)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got != dir {
		t.Fatalf("reveal path=%q want %q", got, dir)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
