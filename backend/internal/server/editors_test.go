package server

// editors_test.go 钉死 /api/editors 与 /api/editors/open 的行为（TODO #26 阶段 F）：
// 列表解析/按 id 去重/目录序（探测函数注入，不依赖真机安装）；open 的路径边界
// （WriteFile 产物 或 任一会话工作区内，越界 404——2026-10-01 起与 raw/reveal 同口径）
// 与 editor_id 白名单（未知 id 404，防"前端传 exe 拉起任意进程"）。

import (
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
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// stubEditorFuncs 临时替换可注入实现（detectEditorsFn/launchEditorFn/openDefaultFn），
// 测试结束自动还原。
func stubEditorFuncs(t *testing.T, detect func() []EditorInfo, launch func(exe, path string) error, openDefault func(path string) error) {
	t.Helper()
	oldDetect, oldLaunch, oldOpen := detectEditorsFn, launchEditorFn, openDefaultFn
	if detect != nil {
		detectEditorsFn = detect
	}
	if launch != nil {
		launchEditorFn = launch
	}
	if openDefault != nil {
		openDefaultFn = openDefault
	}
	t.Cleanup(func() {
		detectEditorsFn, launchEditorFn, openDefaultFn = oldDetect, oldLaunch, oldOpen
	})
}

// TestScanEditors_DedupAndOrder 验证：同一 id 多个候选只取第一个；未安装的不出现；
// 结果顺序与目录一致；name 来自目录而非探测返回值。
func TestScanEditors_DedupAndOrder(t *testing.T) {
	installed := map[string][]string{
		"vscode": {`C:\Program Files\Microsoft VS Code\Code.exe`, `D:\green\Code.exe`},
		"cursor": {`C:\Users\me\AppData\Local\Programs\Cursor\Cursor.exe`},
	}
	got := scanEditors(func(e editorCatalogEntry) []string {
		return installed[e.id]
	})
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (%+v)", len(got), got)
	}
	// 顺序：目录里 vscode 在 cursor 之前。
	if got[0].ID != "vscode" || got[1].ID != "cursor" {
		t.Fatalf("order=%s,%s want vscode,cursor", got[0].ID, got[1].ID)
	}
	// 去重：vscode 只取第一个候选。
	if got[0].Exe != installed["vscode"][0] {
		t.Fatalf("exe=%q want first candidate", got[0].Exe)
	}
	if got[0].Name != "Visual Studio Code" {
		t.Fatalf("name=%q", got[0].Name)
	}
}

// TestScanEditors_EmptyProbe 验证零命中返回空数组（不报错、不 nil——前端 JSON 解析需要 []）。
func TestScanEditors_EmptyProbe(t *testing.T) {
	got := scanEditors(func(e editorCatalogEntry) []string { return nil })
	if got == nil || len(got) != 0 {
		t.Fatalf("got=%v want empty non-nil slice", got)
	}
}

// TestScanEditors_WhitespaceSkipped 验证空白候选被跳过、取下一个非空。
func TestScanEditors_WhitespaceSkipped(t *testing.T) {
	got := scanEditors(func(e editorCatalogEntry) []string {
		if e.id == "trae" {
			return []string{"   ", `C:\Tools\Trae\Trae.exe`}
		}
		return nil
	})
	if len(got) != 1 || got[0].ID != "trae" || got[0].Exe != `C:\Tools\Trae\Trae.exe` {
		t.Fatalf("got=%+v", got)
	}
}

// TestEditorCatalog_UniqueIDs 目录自身不变量：id 唯一（id 是 open 白名单基准，重复即漏洞）。
func TestEditorCatalog_UniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range editorCatalog {
		if seen[e.id] {
			t.Fatalf("duplicate catalog id: %s", e.id)
		}
		seen[e.id] = true
		if e.name == "" {
			t.Fatalf("id=%s empty name", e.id)
		}
	}
}

// newEditorsTestEnv 构造带 WriteFile 产物的测试环境（复用 files_http_test 的 mock 模式）。
// 返回挂好 editors 路由的 gin.Engine 与产物路径。
func newEditorsTestEnv(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	root := t.TempDir()
	product := filepath.Join(root, "report.md")
	if err := os.WriteFile(product, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(root, "stray.md")
	if err := os.WriteFile(stray, []byte("no"), 0o644); err != nil {
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
	r.GET("/api/editors", h.EditorsHandler)
	r.POST("/api/editors/open", h.EditorOpenHandler)
	return r, product
}

// editorOpenBody 用 json.Marshal 构造请求体，避免手写 JSON 转义与 Windows 路径反斜杠冲突。
func editorOpenBody(t *testing.T, editorID, path string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"editor_id": editorID, "path": path})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func editorsPost(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/editors/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// TestEditorsHandler_ReturnsScanList 验证 GET /api/editors 透传扫码结果（JSON 数组）。
func TestEditorsHandler_ReturnsScanList(t *testing.T) {
	stubEditorFuncs(t, func() []EditorInfo {
		return []EditorInfo{{ID: "vscode", Name: "Visual Studio Code", Exe: `/opt/code`}}
	}, nil, nil)

	r, _ := newEditorsTestEnv(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/editors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var list []EditorInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "vscode" || list[0].Exe != `/opt/code` {
		t.Fatalf("list=%+v", list)
	}
}

// TestEditorOpen_RejectsUnknownPath 验证未登记的磁盘文件一律 404（与 raw 同边界）。
func TestEditorOpen_RejectsUnknownPath(t *testing.T) {
	stubEditorFuncs(t, nil, nil, nil)
	r, _ := newEditorsTestEnv(t)

	// 越界：磁盘存在但不属于任何 WriteFile 事件。
	rec := editorsPost(t, r, `{"path":"C:\\Windows\\system.ini"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stray status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// TestEditorOpen_RejectsUnknownEditor 验证 editor_id 不在扫码列表时 404
//（防任意进程拉起：exe 只能来自服务端探测）。
func TestEditorOpen_RejectsUnknownEditor(t *testing.T) {
	launched := false
	stubEditorFuncs(t,
		func() []EditorInfo { return []EditorInfo{{ID: "vscode", Name: "VS Code", Exe: `/opt/code`}} },
		func(exe, path string) error { launched = true; return nil },
		nil)
	r, product := newEditorsTestEnv(t)

	rec := editorsPost(t, r, editorOpenBody(t, "evil-bin", product))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	if launched {
		t.Fatal("未知 editor_id 不应触发拉起")
	}
}

// TestEditorOpen_LaunchesKnownEditor 验证命中扫码列表时以登记 exe 拉起（参数正确）。
func TestEditorOpen_LaunchesKnownEditor(t *testing.T) {
	var gotExe, gotPath string
	stubEditorFuncs(t,
		func() []EditorInfo { return []EditorInfo{{ID: "vscode", Name: "VS Code", Exe: `/opt/code`}} },
		func(exe, path string) error { gotExe, gotPath = exe, path; return nil },
		nil)
	r, product := newEditorsTestEnv(t)

	rec := editorsPost(t, r, editorOpenBody(t, "vscode", product))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotExe != `/opt/code` || gotPath != product {
		t.Fatalf("launch exe=%q path=%q", gotExe, gotPath)
	}
}

// TestEditorOpen_DefaultApp 验证 editor_id 缺省走系统默认打开（openDefaultFn）。
func TestEditorOpen_DefaultApp(t *testing.T) {
	var opened string
	stubEditorFuncs(t, nil, nil, func(path string) error { opened = path; return nil })
	r, product := newEditorsTestEnv(t)

	rec := editorsPost(t, r, editorOpenBody(t, "", product))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if opened != product {
		t.Fatalf("opened=%q want %q", opened, product)
	}
}

// TestEditorOpen_LaunchFailure500 验证桌面环境拉起失败回 5xx + 错误信息。
func TestEditorOpen_LaunchFailure500(t *testing.T) {
	stubEditorFuncs(t,
		func() []EditorInfo { return []EditorInfo{{ID: "vscode", Name: "VS Code", Exe: `/opt/code`}} },
		func(exe, path string) error { return os.ErrNotExist },
		nil)
	r, product := newEditorsTestEnv(t)

	rec := editorsPost(t, r, editorOpenBody(t, "vscode", product))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "拉起编辑器失败") {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

// TestEditorOpen_DeletedFile404 验证已登记但磁盘已删除的文件 404（与 raw 语义一致）。
func TestEditorOpen_DeletedFile404(t *testing.T) {
	stubEditorFuncs(t, nil, nil, nil)
	r, product := newEditorsTestEnv(t)
	if err := os.Remove(product); err != nil {
		t.Fatal(err)
	}
	rec := editorsPost(t, r, editorOpenBody(t, "", product))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

// TestEditorOpen_MissingPath400 验证缺 path 的请求 400。
func TestEditorOpen_MissingPath400(t *testing.T) {
	stubEditorFuncs(t, nil, nil, nil)
	r, _ := newEditorsTestEnv(t)
	rec := editorsPost(t, r, editorOpenBody(t, "vscode", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

// TestEditorOpen_WorkspaceNonArtifactAllowed 验证工作区内非 WriteFile 产物文件可打开
//（2026-10-01 边界对齐 raw/reveal：此前仅产物白名单，目录树面板里大量非产物文件
// 可预览/编辑/reveal 却点不开编辑器，属 #26 阶段 F 的口径遗漏）。
func TestEditorOpen_WorkspaceNonArtifactAllowed(t *testing.T) {
	launched := ""
	stubEditorFuncs(t,
		func() []EditorInfo { return []EditorInfo{{ID: "vscode", Name: "VS Code", Exe: `/opt/code`}} },
		func(exe, path string) error { launched = path; return nil },
		nil)

	root := t.TempDir()
	normal := filepath.Join(root, "notes.txt") // 工作区内、但无任何 WriteFile 事件登记
	if err := os.WriteFile(normal, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := newMockAgentForServer()
	mock.sessions["session-ws"] = &agent.Session{ID: "session-ws", Status: "running", WorkDir: root}
	h := NewAPIHandler(nil)
	h.SetSessionManager(NewSessionManager(mock))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/editors/open", h.EditorOpenHandler)

	rec := editorsPost(t, r, editorOpenBody(t, "vscode", normal))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if launched != normal {
		t.Fatalf("launched=%q want %q", launched, normal)
	}
}

