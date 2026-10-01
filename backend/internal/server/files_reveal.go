package server

// files_reveal.go GET /api/files/reveal —— 「在文件夹中显示」（TODO #26 阶段 C）。
//
// GET /api/files/reveal?path=... → {ok:true}：在系统文件管理器中定位该路径。
//   - Windows：explorer /select,<path>（选中该文件）；
//   - macOS：open -R <path>（Finder 中显示）；
//   - Linux：xdg-open <dir>（打开所在目录；xdg-open 无/select 等价物）。
//
// 安全边界与 /api/files/raw 完全一致（阶段 D 放宽后）：path 命中某会话成功 WriteFile
// 产物（isKnownWriteFilePath，兼容）或位于任一会话工作区内（isWithinAnyWorkspace），
// 否则一律 404（不区分"不存在"与"越界"，避免路径探测）。目录也允许 reveal（打开该目录）。
//
// 平台差异用编译约束切分（参照 editors 先例）：本文件只放平台无关的 HTTP 层与
// 可注入桩点（launchRevealFn），生产实现在 files_reveal_windows.go / files_reveal_other.go。

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// launchRevealFn 在系统文件管理器中定位 path 的实现（测试注入桩点）。
// 生产值由 files_reveal_windows.go / files_reveal_other.go 按平台提供。
var launchRevealFn = revealInFileManager

// FileRevealHandler 处理 GET /api/files/reveal?path=... — 在文件管理器中定位。
// 边界同 /api/files/raw（WriteFile 产物或工作区内）；不存在/越界 404；
// 无桌面/拉起失败 5xx + 错误信息（前端据 501 降级隐藏入口）。
func (h *APIHandler) FileRevealHandler(c *gin.Context) {
	path := c.Query("path")
	if strings.TrimSpace(path) == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}
	// 边界同 raw：WriteFile 产物白名单（兼容）或任一会话工作区内。
	if !h.isKnownWriteFilePath(path) {
		if _, ok := h.isWithinAnyWorkspace(path); !ok {
			c.String(http.StatusNotFound, "file not found")
			return
		}
	}
	if _, err := os.Stat(path); err != nil {
		c.String(http.StatusNotFound, "file not found") // 已删除与越界同码
		return
	}
	if err := launchRevealFn(path); err != nil {
		c.String(http.StatusInternalServerError, "在文件夹中显示失败: %v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
