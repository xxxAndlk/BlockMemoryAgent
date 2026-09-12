package server

// fs_pick_test.go 覆盖系统目录选择端点的**不会被弹窗卡住**的那部分契约：
// 单例锁（同时只允许一个原生对话框）。成功路径需要真人在原生框里点选，无法自动化，
// 故在此只锁并发语义——它恰恰是"反复点按钮弹出一堆窗口"这类问题的防线。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestPickDirHandler_BusyConflict 已有对话框打开时，第二个请求应 409 而不是再弹一个。
func TestPickDirHandler_BusyConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 模拟"已有一个对话框在等用户操作"。
	pickDirBusy.Lock()
	defer pickDirBusy.Unlock()

	h := &APIHandler{}
	r := gin.New()
	r.POST("/api/fs/pick-dir", h.PickDirHandler)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/fs/pick-dir", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("占用中应 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	// 不得阻塞：占用时立即返回（若实现改成等锁，这里会超时挂住）。
}
