package server

// export_http.go 数据导出端点（TODO #18-2 T29）：
//   - GET /api/sessions/:id/export — 单会话全量导出（历史/事件/子 Agent 事件流与消息
//     历史 JSON + 会话工作目录 .bma 产物树），标准库 zip 流式输出。
//   - GET /api/export/memory — 记忆库导出（global_knowledge 未归档 JSONL +
//     user_profile.md + skills_learned 技能包全文）。
//
// 导入端点刻意后置（TODO #18 原文）：导出先行满足备份/迁移诉求，导入涉及合并
// 语义（ID 冲突/向量重建）单独立项。全部 nil 依赖降级——缺哪块就少哪个条目。

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 导出安全阈值：产物树单会话文件数上限 / 单文件大小上限（防误选巨大工作目录拖垮进程）。
const (
	exportMaxFiles        = 2000
	exportMaxFileBytes    = 64 << 20 // 64MB
	exportZipTemplateName = "session-__ID__.zip"
)

// SessionExportHandler 处理 GET /api/sessions/:id/export — 单会话全量导出 zip。
func (h *APIHandler) SessionExportHandler(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.String(http.StatusBadRequest, "session id required")
		return
	}
	ctx := c.Request.Context()

	data, err := h.pgStore.ExportSessionData(ctx, id)
	if err != nil {
		c.String(http.StatusInternalServerError, "读取会话数据失败: %s", err.Error())
		return
	}

	workDir := h.sessionWorkDirForExport(id)

	filename := strings.ReplaceAll(exportZipTemplateName, "__ID__", sanitizeZipName(id))
	setZipHeaders(c, filename)

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	// 四类持久化数据逐条目落 zip（历史/事件/子 Agent 事件流/子 Agent 消息）。
	if data.History != nil {
		writeZipJSON(zw, "session_history.json", data.History)
	}
	writeZipJSON(zw, "session_events.json", data.Events)
	writeZipJSON(zw, "agent_events.json", data.AgentEvents)
	writeZipJSON(zw, "agent_messages.json", data.AgentMessages)

	// 会话工作目录 .bma 产物树（工具输出全文/图片/HTML 原型等），best-effort。
	if workDir != "" {
		addDirToZip(zw, filepath.Join(workDir, ".bma"), "workspace/.bma")
	}
}

// MemoryExportHandler 处理 GET /api/export/memory — 记忆库导出 zip。
func (h *APIHandler) MemoryExportHandler(c *gin.Context) {
	ctx := c.Request.Context()
	setZipHeaders(c, fmt.Sprintf("memory-export-%s.zip", time.Now().Format("20060102-150405")))

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	// 1) global_knowledge 未归档全量（JSONL：一行一条，流式写出）。
	if h.pgStore != nil {
		recs, err := h.pgStore.Knowledge.ListAll(ctx)
		if err != nil {
			// 读侧失败不整体 500：已写入的 zip 前缀无法撤回，改为落一个错误说明条目。
			writeZipText(zw, "knowledge.error.txt", err.Error())
		} else {
			w, _ := zw.Create("knowledge.jsonl")
			enc := json.NewEncoder(w)
			for _, r := range recs {
				_ = enc.Encode(r) // Encode 自带换行，单条失败跳过继续
			}
		}
	}

	// 2) 用户画像全文。
	if h.sessionMgr != nil && h.sessionMgr.agent != nil {
		if p, err := h.sessionMgr.agent.Profile(ctx); err == nil && p != nil {
			writeZipText(zw, "user_profile.md", p.Content)
		}
	}

	// 3) 自进化技能包全文（按落盘路径逐个读，单文件失败跳过）。
	if h.learnedSkills != nil {
		if skills, err := h.learnedSkills.List(ctx, false); err == nil {
			for _, s := range skills {
				if s.ContentPath == "" {
					continue
				}
				content, err := os.ReadFile(s.ContentPath)
				if err != nil {
					continue
				}
				writeZipText(zw, "skills_learned/"+sanitizeZipName(s.Name)+".md", string(content))
			}
		}
	}
}

// sessionWorkDirForExport 解析会话有效工作目录（与 workspace_http 同款鸭子类型）；
// 未接线/无目录返回空串（zip 跳过产物树条目）。
func (h *APIHandler) sessionWorkDirForExport(sessionID string) string {
	if h.sessionMgr == nil || h.sessionMgr.agent == nil {
		return ""
	}
	wp, ok := h.sessionMgr.agent.(sessionWorkDirProvider)
	if !ok {
		return ""
	}
	return wp.SessionWorkDir(sessionID)
}

// setZipHeaders 统一下载响应头。
func setZipHeaders(c *gin.Context, filename string) {
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Status(http.StatusOK)
}

// writeZipJSON 序列化 v 为缩进 JSON 写入 zip 条目（错误吞掉：导出尽力而为）。
func writeZipJSON(zw *zip.Writer, name string, v any) {
	w, err := zw.Create(name)
	if err != nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeZipText 纯文本条目。
func writeZipText(zw *zip.Writer, name, content string) {
	w, err := zw.Create(name)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, content)
}

// addDirToZip 递归打包目录树到 zip 的 root/ 前缀下；跳过符号链接、超大文件，
// 文件数到 exportMaxFiles 截断。root 不存在时整体 no-op。
func addDirToZip(zw *zip.Writer, dir, root string) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	count := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单点不可读跳过，继续整树
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if count >= exportMaxFiles {
			return filepath.SkipAll
		}
		fi, err := d.Info()
		if err != nil || fi.Size() > exportMaxFileBytes {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		w, err := zw.Create(root + "/" + filepath.ToSlash(rel))
		if err != nil {
			return nil
		}
		_, _ = w.Write(content)
		count++
		return nil
	})
}

// sanitizeZipName 把 ID/技能名收敛为 zip 文件名安全字符（会话 ID 等本就受限，
// 防御用户直连场景的自定义 agent 名/技能名带路径分隔符）。
func sanitizeZipName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "export"
	}
	return b.String()
}
