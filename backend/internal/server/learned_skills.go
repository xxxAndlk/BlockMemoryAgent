package server

// learned_skills.go 自进化技能库管理 API（2026-09-02 设计 §8）：
// GET/PUT /api/skills/learned[/{name}]、POST /{name}/enable|/disable、GET /api/evolution/log。
// 文件（config/skills_learned/<name>.md）为正文真相源，PG 存元数据+召回向量；
// 手动编辑刷新文件+向量+技能池注册项。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// learnedSkillToolPathRe 校验 PUT 提交的 tools.path（与 bootstrap/skill_tools.go skillToolFileRe 同口径）。
var learnedSkillToolPathRe = regexp.MustCompile(`^scripts/[a-z0-9][a-z0-9-]*\.(py|sh|js|ts)$`)

// sanitizeSkillTools 规整并校验 PUT 提交的 tools 清单：去空白、path 走安全文件名正则、
// desc/run 限长。返回错误文案（空串=通过）。
func sanitizeSkillTools(in []textutil.SkillTool) ([]textutil.SkillTool, string) {
	out := make([]textutil.SkillTool, len(in))
	for i, t := range in {
		out[i] = textutil.SkillTool{
			Path: strings.TrimSpace(t.Path),
			Desc: strings.TrimSpace(t.Desc),
			Run:  strings.TrimSpace(t.Run),
		}
		if !learnedSkillToolPathRe.MatchString(out[i].Path) {
			return nil, "invalid tool path: " + out[i].Path
		}
		if len(out[i].Desc) > 200 || len(out[i].Run) > 200 {
			return nil, "tool desc/run too long"
		}
	}
	return out, ""
}

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
	// C1：content_path 新老格式混存，读路径统一走目录式优先解析。
	if data, err := os.ReadFile(textutil.ResolveSkillMDPathFromRef(rec.ContentPath)); err == nil {
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
		Title     string              `json:"title"`
		WhenToUse string              `json:"when_to_use"`
		Content   string              `json:"content"`
		Tools     *[]textutil.SkillTool `json:"tools"` // 可选（C4）：提交则整体替换 frontmatter tools 清单并同步 scripts/ 文件
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
	// C1：路径解析走统一函数（目录式 <name>/SKILL.md 优先，老 .md 回退）。
	targetPath := textutil.ResolveSkillMDPathFromRef(rec.ContentPath)
	// C1/C2：保留既有 tools 清单——PUT 只改 title/when_to_use/正文，frontmatter tools
	// 原样带回（否则手动编辑会静默丢掉配套脚本声明）。请求显式提交 tools 时（C4）整体替换。
	var tools []textutil.SkillTool
	if data, err := os.ReadFile(targetPath); err == nil {
		tools = textutil.ParseSkillTools(data)
	}
	if req.Tools != nil {
		submitted, err := sanitizeSkillTools(*req.Tools)
		if err != "" {
			c.String(http.StatusBadRequest, "%s", err)
			return
		}
		// 目录式技能：删除已从清单移除的 scripts/ 文件（仅限技能目录内，防穿越）。
		if filepath.Base(targetPath) == "SKILL.md" {
			keep := map[string]bool{}
			for _, t := range submitted {
				keep[filepath.Base(t.Path)] = true
			}
			scriptsDir := filepath.Join(filepath.Dir(targetPath), "scripts")
			if entries, err := os.ReadDir(scriptsDir); err == nil {
				for _, e := range entries {
					if !e.IsDir() && !keep[e.Name()] {
						_ = os.Remove(filepath.Join(scriptsDir, e.Name()))
					}
				}
			}
		}
		tools = submitted
	}
	// 重写文件：frontmatter（name/title/when_to_use/outcome[/tools]）+ 正文原样。
	var b strings.Builder
	b.WriteString(textutil.RenderSkillFrontmatter(name, title, whenToUse, rec.Outcome, tools))
	b.WriteString("\n")
	b.WriteString(strings.ReplaceAll(req.Content, "\r\n", "\n"))
	if err := os.WriteFile(targetPath, []byte(b.String()), 0o644); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// 重嵌入失败不阻塞编辑（向量置 NULL，召回侧过滤，等下次同名更新重建）。
	emb, embErr := h.learnedSkills.Embed(ctx, title+" "+whenToUse)
	if embErr != nil {
		emb = nil
	}
	if err := h.learnedSkills.UpdateMeta(ctx, name, title, whenToUse, emb, len(tools) > 0); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	h.registerLearnedSkill(name, title, whenToUse, targetPath, b.String())
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
			// C1 统一解析 + C2 配套工具段注入（与 bootstrap 侧注册口径一致）。
			content := ""
			if data, err := os.ReadFile(textutil.ResolveSkillMDPathFromRef(rec.ContentPath)); err == nil {
				_, body := textutil.ParseFrontmatter(data)
				content = body + textutil.SkillToolSection(textutil.ParseSkillTools(data))
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
// C2：content 为整份 SKILL.md（含 frontmatter）——剥 frontmatter 取正文，tools 非空时
// 追加「配套工具」段（load_skill 全文可见每个脚本的相对路径/运行命令/用途）。
func (h *APIHandler) registerLearnedSkill(name, title, whenToUse, path, content string) {
	if h.rt == nil || h.rt.Skills == nil {
		return
	}
	_, body := textutil.ParseFrontmatter([]byte(content))
	body += textutil.SkillToolSection(textutil.ParseSkillTools([]byte(content)))
	h.rt.Skills.Register(&types.Skill{
		SkillID: name, Name: title, Description: whenToUse,
		Source: "learned", Path: path, Content: body,
	})
}
