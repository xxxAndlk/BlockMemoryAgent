package server

// learned_skills.go 自进化技能库管理 API（2026-09-02 设计 §8）：
// GET/PUT /api/skills/learned[/{name}]、POST /{name}/enable|/disable、GET /api/evolution/log。
// 文件（config/skills_learned/<name>.md）为正文真相源，PG 存元数据+召回向量；
// 手动编辑刷新文件+向量+技能池注册项。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SetLearnedSkills 注入经验技能库存储（未注入时相关端点返回 503）。
func (h *APIHandler) SetLearnedSkills(s *store.LearnedSkillStore) {
	h.learnedSkills = s
}

// SetSkillConsolidation 注入技能库整理器（C 库存治理，bootstrap 装配；nil 时端点 503）。
// 手动触发忽略每日阈值直接整理，同步返回摘要（轻量模型调用，最长约 2 分钟）。
func (h *APIHandler) SetSkillConsolidation(fn func(ctx context.Context) (string, error)) {
	h.skillConsolidation = fn
}

// ConsolidateSkillsHandler 处理 POST /api/skills/consolidate —
// 手动触发一轮技能库整理（合并语义重复技能 + 归档零使用技能），返回摘要。
func (h *APIHandler) ConsolidateSkillsHandler(c *gin.Context) {
	if h.skillConsolidation == nil {
		c.String(http.StatusServiceUnavailable, "skill consolidation not wired")
		return
	}
	// 整理含轻量模型调用（最长 ~2 分钟），超时与整理器内部预算对齐。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 150*time.Second)
	defer cancel()
	summary, err := h.skillConsolidation(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "summary": summary})
}

// ListLearnedSkillsHandler 处理 GET /api/skills/learned — 技能库全量列表（含禁用项）。
func (h *APIHandler) ListLearnedSkillsHandler(c *gin.Context) {
	if h.learnedSkills == nil {
		c.String(http.StatusServiceUnavailable, "learned skills store not wired")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.learnedSkills.List(ctx, false)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rows == nil {
		rows = []*store.LearnedSkill{}
	}
	c.JSON(http.StatusOK, map[string]any{"skills": rows})
}

// GetLearnedSkillHandler 处理 GET /api/skills/learned/:name — 技能元数据 + 文件全文。
func (h *APIHandler) GetLearnedSkillHandler(c *gin.Context) {
	if h.learnedSkills == nil {
		c.String(http.StatusServiceUnavailable, "learned skills store not wired")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rec, err := h.learnedSkills.Get(ctx, c.Param("name"))
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rec == nil {
		c.String(http.StatusNotFound, "skill not found")
		return
	}
	content := ""
	if data, err := os.ReadFile(rec.ContentPath); err == nil {
		content = string(data)
	}
	c.JSON(http.StatusOK, map[string]any{"skill": rec, "content": content})
}

// SaveLearnedSkillHandler 处理 PUT /api/skills/learned/:name — 手动编辑
//（title/when_to_use/content → 重写文件 + 重嵌入向量 + 刷新技能池注册项）。
func (h *APIHandler) SaveLearnedSkillHandler(c *gin.Context) {
	if h.learnedSkills == nil {
		c.String(http.StatusServiceUnavailable, "learned skills store not wired")
		return
	}
	name := c.Param("name")
	var req struct {
		Title     string `json:"title"`
		WhenToUse string `json:"when_to_use"`
		Content   string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	whenToUse := strings.TrimSpace(req.WhenToUse)
	if title == "" || whenToUse == "" {
		c.String(http.StatusBadRequest, "title and when_to_use required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rec, err := h.learnedSkills.Get(ctx, name)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rec == nil {
		c.String(http.StatusNotFound, "skill not found")
		return
	}
	// 重写文件：frontmatter（name/title/when_to_use/outcome）+ 正文原样。
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "when_to_use: %s\n", whenToUse)
	if rec.Outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", rec.Outcome)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.ReplaceAll(req.Content, "\r\n", "\n"))
	if err := os.WriteFile(rec.ContentPath, []byte(b.String()), 0o644); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// 重嵌入失败不阻塞编辑（向量置 NULL，召回侧过滤，等下次同名更新重建）。
	emb, embErr := h.learnedSkills.Embed(ctx, title+" "+whenToUse)
	if embErr != nil {
		emb = nil
	}
	if err := h.learnedSkills.UpdateMeta(ctx, name, title, whenToUse, emb); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	h.registerLearnedSkill(name, title, whenToUse, rec.ContentPath, b.String())
	c.JSON(http.StatusOK, map[string]any{"ok": true, "embedding_refreshed": embErr == nil})
}

// EnableLearnedSkillHandler 处理 POST /api/skills/learned/:name/enable —
// 启用并注册进共享技能池（渐进披露可见）。
func (h *APIHandler) EnableLearnedSkillHandler(c *gin.Context) {
	h.setLearnedSkillEnabled(c, true)
}

// DisableLearnedSkillHandler 处理 POST /api/skills/learned/:name/disable —
// 禁用并从共享技能池移除（list_skills/召回均不再可见）。
func (h *APIHandler) DisableLearnedSkillHandler(c *gin.Context) {
	h.setLearnedSkillEnabled(c, false)
}

// setLearnedSkillEnabled 启用/禁用共用实现：更新 PG → 同步技能池。
func (h *APIHandler) setLearnedSkillEnabled(c *gin.Context, enabled bool) {
	if h.learnedSkills == nil {
		c.String(http.StatusServiceUnavailable, "learned skills store not wired")
		return
	}
	name := c.Param("name")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rec, err := h.learnedSkills.Get(ctx, name)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rec == nil {
		c.String(http.StatusNotFound, "skill not found")
		return
	}
	if err := h.learnedSkills.SetEnabled(ctx, name, enabled); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if h.rt != nil && h.rt.Skills != nil {
		if enabled {
			content := ""
			if data, err := os.ReadFile(rec.ContentPath); err == nil {
				content = string(data)
			}
			h.rt.Skills.Register(&types.Skill{
				SkillID: name, Name: rec.Title, Description: rec.WhenToUse,
				Source: "learned", Path: rec.ContentPath, Content: content,
			})
		} else {
			h.rt.Skills.Remove(name)
		}
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

// EvolutionLogHandler 处理 GET /api/evolution/log — 进化审计流水倒序
//（?limit=N 默认 200）。
func (h *APIHandler) EvolutionLogHandler(c *gin.Context) {
	if h.learnedSkills == nil {
		c.String(http.StatusServiceUnavailable, "learned skills store not wired")
		return
	}
	limit := 200
	if q := c.Query("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	entries, err := h.learnedSkills.ListEvolutionLog(ctx, limit)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if entries == nil {
		entries = []*store.EvolutionLogEntry{}
	}
	c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

// registerLearnedSkill 把技能元数据注册进共享技能池（已启用时可见）。
func (h *APIHandler) registerLearnedSkill(name, title, whenToUse, path, content string) {
	if h.rt == nil || h.rt.Skills == nil {
		return
	}
	h.rt.Skills.Register(&types.Skill{
		SkillID: name, Name: title, Description: whenToUse,
		Source: "learned", Path: path, Content: content,
	})
}
