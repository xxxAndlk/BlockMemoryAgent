package server

// files_write.go PUT /api/files/content 在线编辑保存（TODO #26 阶段 G）。
//
// 与 GET content / raw 的边界差异：
//   - raw（isKnownWriteFilePath）：仅限会话 WriteFile 产物，只读；
//   - PUT（isWithinAnyWorkspace）：放宽为「任一现存会话 WorkDir 目录内」的任意路径，
//     越界一律 404（不区分"不存在"与"越界"，避免路径探测）。
//
// 四重保护：工作区边界 + base_mtime 冲突 409 + 原子写（temp+rename）+
// 一次性 .bak 备份（原文件存在且尚无 .bak 时才备份，不覆盖旧备份）；
// 全部成功写入落 session_logs（phase=file_write），pgStore 不可用时退 fmt 日志。

import (
	"fmt"          // 审计消息格式化
	"log"          // 审计落库失败/无 pgStore 时的回落日志
	"net/http"     // HTTP 状态码
	"os"           // 文件读写 / Stat / Rename
	"path/filepath" // 路径规范化与前缀比较
	stdruntime "runtime" // GOOS 判断（Windows 大小写不敏感）
	"strings"      // 前缀比较
	"time"         // 审计时间戳

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/store" // session_logs 落库
)

// fileWriteMaxBytes PUT 单文件内容上限：5MB（超限 413，与 raw 的 20MB 区分——
// 在线编辑是轻量 textarea 场景，5MB 以上应走本机编辑器）。
const fileWriteMaxBytes = 5 << 20

// isWithinAnyWorkspace 校验 path 是否位于任一现存会话的 WorkDir 目录内。
// 返回：命中的会话 ID 与是否命中。这是 PUT 写边界（比 raw 的 WriteFile 产物白名单宽）。
//
// 实现口径：
//   - 两侧都取绝对路径 + filepath.Clean 统一分隔符；
//   - 命中条件：path == WorkDir，或以 WorkDir + 路径分隔符 为前缀（前缀自带分隔符，
//     杜绝 C:\foo 误配 C:\foobar）；
//   - Windows 下两侧 ToLower 后比较（文件系统不区分大小写）；
//   - WorkDir 为空的会话（=进程默认目录，无明确边界）不参与匹配；
//   - 不解析符号链接：WorkDir 经软链指到别处时以登记字符串为准（遗留限制）。
func (h *APIHandler) isWithinAnyWorkspace(path string) (string, bool) {
	if h.sessionMgr == nil {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	abs = filepath.Clean(abs)
	if stdruntime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	for _, s := range h.sessionMgr.ListSessions() {
		wd := s.WorkDir
		if wd == "" {
			continue // 空 WorkDir = 进程默认，无明确目录边界，不作为写白名单
		}
		wa, err := filepath.Abs(wd)
		if err != nil {
			continue
		}
		wa = filepath.Clean(wa)
		if stdruntime.GOOS == "windows" {
			wa = strings.ToLower(wa)
		}
		if abs == wa || strings.HasPrefix(abs, wa+string(os.PathSeparator)) {
			return s.ID, true
		}
	}
	return "", false
}

// SaveFileContentHandler 处理 PUT /api/files/content — 在线编辑保存。
// 请求：{path, content, base_mtime?}
//   - path 必须位于任一会话 WorkDir 内（否则 404）；目标必须存在且非目录（否则 404）；
//   - content 超 fileWriteMaxBytes → 413；
//   - base_mtime（Unix 毫秒）与磁盘当前 mtime 不一致 → 409 {code:"conflict", current_mtime}；
//   - 写前一次性 .bak 备份（原文件存在且尚无 .bak，不覆盖旧备份）；
//   - 同目录 temp + rename 原子写；成功后落 session_logs 审计。
// 响应：{ok, mtime, size}（mtime 为 Unix 毫秒）。
func (h *APIHandler) SaveFileContentHandler(c *gin.Context) {
	var req struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		BaseMtime *int64 `json:"base_mtime"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	if req.Path == "" {
		c.String(http.StatusBadRequest, "path required")
		return
	}

	sessionID, ok := h.isWithinAnyWorkspace(req.Path)
	if !ok {
		c.String(http.StatusNotFound, "file not found") // 越界与不存在同码，避免路径探测
		return
	}
	if len(req.Content) > fileWriteMaxBytes {
		c.String(http.StatusRequestEntityTooLarge, "file too large (max 5MB)")
		return
	}
	info, err := os.Stat(req.Path)
	if err != nil || info.IsDir() {
		c.String(http.StatusNotFound, "file not found") // 在线编辑只针对已存在文件
		return
	}
	if req.BaseMtime != nil && info.ModTime().UnixMilli() != *req.BaseMtime {
		c.JSON(http.StatusConflict, gin.H{
			"code":          "conflict",
			"current_mtime": info.ModTime().UnixMilli(),
		})
		return
	}

	// 一次性 .bak 备份：仅当原文件存在且尚无 .bak（os.Stat 已确认原文件存在；
	// .bak 已存在说明此前备份过，不覆盖旧备份）。备份失败不阻断保存。
	bakPath := req.Path + ".bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		if data, err := os.ReadFile(req.Path); err == nil {
			_ = os.WriteFile(bakPath, data, 0o644)
		}
	}

	// 原子写：同目录 temp 文件 + rename（同分区保证 rename 原子性）。
	tmp, err := os.CreateTemp(filepath.Dir(req.Path), ".bma-save-*")
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 成功 rename 后路径已不存在，Remove 是 no-op 兜底
	if _, err := tmp.WriteString(req.Content); err != nil {
		tmp.Close()
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if err := os.Rename(tmpName, req.Path); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}

	newInfo, err := os.Stat(req.Path)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	h.auditFileWrite(c, sessionID, req.Path, newInfo.Size())
	c.JSON(http.StatusOK, gin.H{
		"ok":    true,
		"mtime": newInfo.ModTime().UnixMilli(),
		"size":  newInfo.Size(),
	})
}

// auditFileWrite 把一次成功的在线保存落 session_logs（phase=file_write，
// 记 path/大小/会话 id）；pgStore 不可用（测试/降级环境）退 fmt 日志，不影响响应。
func (h *APIHandler) auditFileWrite(c *gin.Context, sessionID, path string, size int64) {
	msg := fmt.Sprintf("[file_write] 在线编辑保存 path=%s size=%d", path, size)
	if h.pgStore == nil {
		log.Printf("%s session=%s", msg, sessionID)
		return
	}
	rec := &store.SessionLogRecord{
		SessionID: sessionID,
		Agent:     "web",
		Level:     "info",
		Phase:     "file_write",
		Message:   msg,
		CreatedAt: time.Now(),
		Meta: map[string]any{
			"path":   path,
			"size":   size,
			"remote": c.ClientIP(),
		},
	}
	if err := h.pgStore.SaveSessionLog(c.Request.Context(), rec); err != nil {
		log.Printf("[file_write] session_logs 落库失败（保存本身已成功）: %v", err)
	}
}
