package server

// editors.go 编辑器扫码与「打开方式」端点（TODO #26 阶段 F）。
//
// GET  /api/editors      → [{id, name, exe}]：扫描本机已安装编辑器（60s 进程内缓存）。
// POST /api/editors/open → {editor_id?, path}：以指定编辑器或系统默认方式打开文件。
//
// 安全边界：path 复用 /api/files/raw 的 isKnownWriteFilePath（仅会话 WriteFile 产物，
// 越界/不存在一律 404）；editor_id 必须命中扫码结果，未知 id 一律 404——
// 否则等于开放"服务端任意进程拉起"（exe 来自本机探测，不接受前端传 exe）。
//
// 平台差异用编译约束切分：注册表读取在 editors_windows.go，macOS/Linux 实现在
// editors_other.go，本文件只放平台无关的目录、去重、缓存与 HTTP 层。

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// EditorInfo 一个已安装的编辑器（/api/editors 列表元素）。
type EditorInfo struct {
	ID   string `json:"id"`   // 稳定小写标识（notepad/vscode/trae/...，兼作 editor_id 白名单）
	Name string `json:"name"` // 给人看的名字
	Exe  string `json:"exe"`  // 可执行文件（或 macOS .app 包）绝对路径
}

// editorCatalogEntry 编辑器目录条目：一个稳定 id 在各平台上的探测线索。
// 平台探测函数只读取自己平台的字段，其余字段忽略。
type editorCatalogEntry struct {
	id   string
	name string
	// Windows：App Paths 注册表键名（HKLM+HKCU，双注册表视图）。
	winReg []string
	// Windows：常见安装目录名（在 ProgramFiles / ProgramFiles(x86) /
	// LOCALAPPDATA\Programs 三个根下逐个探测）与其下的 exe 文件名。dirs 为空=不做路径探测。
	winDirs []string
	winExe  string
	// Windows：%SystemRoot% 下的相对路径探测（系统自带程序，如记事本）。
	winSystem []string
	// Linux：exec.LookPath 探测的命令名（按序第一个命中为准）。
	linuxBins []string
	// macOS：/Applications 下的 .app 包名（exe 直接取 .app 路径，open -a 使用）。
	darwinApps []string
}

// editorCatalog 全部受支持编辑器的探测目录（顺序即返回列表顺序，UI 稳定）。
var editorCatalog = []editorCatalogEntry{
	{id: "vscode", name: "Visual Studio Code",
		winReg: []string{"Code.exe"}, winDirs: []string{"Microsoft VS Code"}, winExe: "Code.exe",
		linuxBins: []string{"code"}, darwinApps: []string{"Visual Studio Code.app"}},
	{id: "vscode-insiders", name: "Visual Studio Code Insiders",
		winReg: []string{"Code - Insiders.exe"}, winDirs: []string{"Microsoft VS Code Insiders"}, winExe: "Code - Insiders.exe",
		linuxBins: []string{"code-insiders"}, darwinApps: []string{"Visual Studio Code - Insiders.app"}},
	{id: "trae", name: "Trae",
		winReg: []string{"Trae.exe"}, winDirs: []string{"Trae"}, winExe: "Trae.exe",
		linuxBins: []string{"trae"}, darwinApps: []string{"Trae.app"}},
	{id: "cursor", name: "Cursor",
		winReg: []string{"cursor.exe"}, winDirs: []string{"Cursor", "cursor"}, winExe: "Cursor.exe",
		linuxBins: []string{"cursor"}, darwinApps: []string{"Cursor.app"}},
	{id: "notepad", name: "记事本",
		winReg: []string{"notepad.exe"}, winSystem: []string{`System32\notepad.exe`},
		linuxBins: nil, darwinApps: nil},
	{id: "notepad++", name: "Notepad++",
		winReg: []string{"notepad++.exe"}, winDirs: []string{"Notepad++"}, winExe: "notepad++.exe",
		linuxBins: []string{"notepad++"}, darwinApps: nil},
	{id: "sublime", name: "Sublime Text",
		winReg: []string{"sublime_text.exe"}, winDirs: []string{"Sublime Text", "Sublime Text 3", "Sublime Text 4"}, winExe: "sublime_text.exe",
		linuxBins: []string{"sublime_text"}, darwinApps: []string{"Sublime Text.app"}},
	{id: "webstorm", name: "WebStorm",
		winReg: []string{"webstorm64.exe"}, // JetBrains 目录带版本号，只做注册表
		linuxBins: []string{"webstorm"}, darwinApps: []string{"WebStorm.app"}},
}

// editorCatalogIndex id → 目录条目（启动时建一次，probe 反查用）。
var editorCatalogIndex = func() map[string]editorCatalogEntry {
	m := make(map[string]editorCatalogEntry, len(editorCatalog))
	for _, e := range editorCatalog {
		m[e.id] = e
	}
	return m
}()

// 可注入的实现（测试桩点）：detectEditorsFn 扫码、launchEditorFn 拉起编辑器、
// openDefaultFn 系统默认打开。生产值由 editors_windows.go / editors_other.go 提供。
var (
	detectEditorsFn = detectInstalledEditors
	launchEditorFn  = launchEditorDetached
	openDefaultFn   = openDefaultApp
)

// editorsCacheTTL 扫码结果进程内缓存时长（前端每次展开下拉都重查，靠它防抖）。
const editorsCacheTTL = 60 * time.Second

// editorScanMu 保护 detectEditorsFn 的并发调用（注册表/磁盘探测非纯函数且无重入必要）。
var editorScanMu sync.Mutex

// scanEditors 用注入的探测函数扫一遍目录：按 id 去重（每 id 取第一个命中）、
// 结果按目录顺序排序。probe 返回候选 exe 列表，空=未安装。
// 探测逻辑整体可注入，测试不依赖真机安装。
func scanEditors(probe func(entry editorCatalogEntry) []string) []EditorInfo {
	out := make([]EditorInfo, 0, len(editorCatalog))
	for _, e := range editorCatalog {
		hit := ""
		for _, exe := range probe(e) {
			if exe = strings.TrimSpace(exe); exe != "" {
				hit = exe
				break // 每 id 取第一个命中即可（注册表/路径双通道在 probe 内已合并）
			}
		}
		if hit == "" {
			continue // 未安装
		}
		out = append(out, EditorInfo{ID: e.id, Name: e.name, Exe: hit})
	}
	return out
}

// cachedEditors 返回扫码结果，60s 内走缓存（含"扫过但零命中"）。
func (h *APIHandler) cachedEditors() []EditorInfo {
	h.editorsMu.Lock()
	defer h.editorsMu.Unlock()
	if h.editorsScanned && time.Since(h.editorsAt) < editorsCacheTTL {
		return h.editorsCached
	}
	editorScanMu.Lock()
	h.editorsCached = detectEditorsFn()
	editorScanMu.Unlock()
	h.editorsAt = time.Now()
	h.editorsScanned = true
	return h.editorsCached
}

// EditorsHandler 处理 GET /api/editors — 返回本机已安装编辑器列表 [{id,name,exe}]。
// 零命中不报错，返回空数组（前端下拉退化为只剩「系统默认打开」）。
func (h *APIHandler) EditorsHandler(c *gin.Context) {
	c.JSON(http.StatusOK, h.cachedEditors())
}

// EditorOpenHandler 处理 POST /api/editors/open — {editor_id?, path}。
// editor_id 为空=系统默认打开；否则必须命中扫码列表（防任意进程拉起）。
// path 边界同 /api/files/raw 与 reveal：会话 WriteFile 产物 或 任一会话工作区内，
// 越界/不存在 404。（2026-10-01 对齐：此前仅产物白名单，工作区树里非产物文件
// 可预览/编辑/reveal 却无法用编辑器打开，口径收窄属遗漏。）
func (h *APIHandler) EditorOpenHandler(c *gin.Context) {
	var req struct {
		EditorID string `json:"editor_id"`
		Path     string `json:"path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}

	// 路径边界：同 raw/reveal——WriteFile 产物白名单 或 任一会话工作区内，
	// 不区分"不存在"与"越界"。
	if !h.isKnownWriteFilePath(req.Path) {
		if _, ok := h.isWithinAnyWorkspace(req.Path); !ok {
			c.String(http.StatusNotFound, "file not found")
			return
		}
	}
	if info, err := os.Stat(req.Path); err != nil || info.IsDir() {
		c.String(http.StatusNotFound, "file not found")
		return
	}

	if req.EditorID == "" {
		// 系统默认打开（Windows rundll32 / macOS open / Linux xdg-open）。
		if err := openDefaultFn(req.Path); err != nil {
			c.String(http.StatusInternalServerError, "系统默认打开失败: %v", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "editor_id": ""})
		return
	}

	// 指定编辑器：exe 只从扫码结果里取，前端无从注入进程路径。
	exe := ""
	for _, e := range h.cachedEditors() {
		if e.ID == req.EditorID {
			exe = e.Exe
			break
		}
	}
	if exe == "" {
		c.String(http.StatusNotFound, "unknown editor_id: %s", req.EditorID)
		return
	}
	if err := launchEditorFn(exe, req.Path); err != nil {
		c.String(http.StatusInternalServerError, "拉起编辑器失败: %v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "editor_id": req.EditorID})
}

// sortEditorInfos 让外部注入的列表也保持目录顺序（测试/未来动态源用）。
func sortEditorInfos(in []EditorInfo) {
	order := make(map[string]int, len(editorCatalog))
	for i, e := range editorCatalog {
		order[e.id] = i
	}
	sort.SliceStable(in, func(i, j int) bool {
		oi, okI := order[in[i].ID]
		oj, okJ := order[in[j].ID]
		if !okI {
			oi = len(editorCatalog)
		}
		if !okJ {
			oj = len(editorCatalog)
		}
		return oi < oj
	})
}
