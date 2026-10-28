package server

// memory_admin_test.go 长期记忆条目管理 API（2026-10-24 撤回与污染修复 §9.1）：
// 列表/详情/事件时间线 + 撤回/隔离/恢复；未装配 503；状态迁移留痕。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/store"
)

// newMemoryAdminRouter 构造只挂记忆管理端点的测试路由 + 内存仓储。
func newMemoryAdminRouter(t *testing.T) (*gin.Engine, *agent.MemoryLifecycle, *store.MemoryLongTermStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := store.NewMemoryLongTermStore()
	lc := agent.NewMemoryLifecycle(repo, agent.RetractionModeEnforce)
	h := NewAPIHandler(nil)
	h.SetMemoryLifecycle(lc)

	r := gin.New()
	r.GET("/api/memory/items", h.ListMemoryItemsHandler)
	r.GET("/api/memory/items/:id", h.GetMemoryItemHandler)
	r.GET("/api/memory/items/:id/events", h.ListMemoryItemEventsHandler)
	r.POST("/api/memory/items/:id/retract", h.RetractMemoryItemHandler)
	r.POST("/api/memory/items/:id/quarantine", h.QuarantineMemoryItemHandler)
	r.POST("/api/memory/items/:id/restore", h.RestoreMemoryItemHandler)
	return r, lc, repo
}

// doMemoryJSON 发一个请求并返回 recorder。
func doMemoryJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestMemoryAdminNotWired 未装配生命周期服务时端点 503（不静默返回空数据）。
func TestMemoryAdminNotWired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(nil)
	r := gin.New()
	r.GET("/api/memory/items", h.ListMemoryItemsHandler)
	w := doMemoryJSON(t, r, http.MethodGet, "/api/memory/items", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503，got %d", w.Code)
	}
}

// TestMemoryAdminLifecycleFlow 列表 → 撤回 → 隔离 → 恢复 → 事件时间线。
func TestMemoryAdminLifecycleFlow(t *testing.T) {
	r, _, repo := newMemoryAdminRouter(t)
	ctx := t.Context()
	item, err := repo.InsertItem(ctx, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "response.style",
		Content: "输出要简洁", Status: store.MemoryStatusActive,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 列表（默认全部状态）
	w := doMemoryJSON(t, r, http.MethodGet, "/api/memory/items?scope=user", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d body %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Count int                `json:"count"`
		Items []store.MemoryItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.Count != 1 || listResp.Items[0].ID != item.ID {
		t.Fatalf("列表应返回 1 条，got %+v", listResp)
	}

	// 撤回
	w = doMemoryJSON(t, r, http.MethodPost, "/api/memory/items/"+item.ID+"/retract", map[string]string{"reason": "用户否决"})
	if w.Code != http.StatusOK {
		t.Fatalf("retract status %d body %s", w.Code, w.Body.String())
	}
	got, _ := repo.GetItem(ctx, "", item.ID)
	if got.Status != store.MemoryStatusRetracted {
		t.Fatalf("撤回后应为 retracted，got %s", got.Status)
	}

	// 隔离
	w = doMemoryJSON(t, r, http.MethodPost, "/api/memory/items/"+item.ID+"/quarantine", map[string]string{"reason": "先停注入"})
	if w.Code != http.StatusOK {
		t.Fatalf("quarantine status %d", w.Code)
	}
	got, _ = repo.GetItem(ctx, "", item.ID)
	if got.Status != store.MemoryStatusQuarantined {
		t.Fatalf("隔离后应为 quarantined，got %s", got.Status)
	}

	// 恢复
	w = doMemoryJSON(t, r, http.MethodPost, "/api/memory/items/"+item.ID+"/restore", map[string]string{"reason": "确认仍有效"})
	if w.Code != http.StatusOK {
		t.Fatalf("restore status %d body %s", w.Code, w.Body.String())
	}
	got, _ = repo.GetItem(ctx, "", item.ID)
	if got.Status != store.MemoryStatusActive {
		t.Fatalf("恢复后应为 active，got %s", got.Status)
	}

	// 事件时间线：撤回/隔离/恢复全部留痕（只追加，不删除）
	w = doMemoryJSON(t, r, http.MethodGet, "/api/memory/items/"+item.ID+"/events", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("events status %d", w.Code)
	}
	var evResp struct {
		Count  int                 `json:"count"`
		Events []store.MemoryEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &evResp); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var types []string
	for _, ev := range evResp.Events {
		types = append(types, ev.EventType)
	}
	for _, want := range []string{store.MemoryEventRetracted, store.MemoryEventQuarantined, store.MemoryEventRestored} {
		found := false
		for _, got := range types {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("事件时间线缺少 %s，got %v", want, types)
		}
	}
}

// TestMemoryAdminNotFound 不存在的条目 404（不假报成功）。
func TestMemoryAdminNotFound(t *testing.T) {
	r, _, _ := newMemoryAdminRouter(t)
	w := doMemoryJSON(t, r, http.MethodPost, "/api/memory/items/M-000000/retract", map[string]string{"reason": "x"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("应 404，got %d body %s", w.Code, w.Body.String())
	}
}
