package server

// role_evolution.go 角色自进化准则管理 API：
// GET /api/roles/evolution-prompts（全量列表）、PUT /api/roles/:id/evolution-prompt（编辑）。
// 每个角色一条 ≤300 字的行为修正准则（人工/自动沉淀），存储侧 role_evolution_prompts 表
// 为真相源；角色注册表（*role.Registry）提供角色全集与显示名（meta/domain 由配置合成，
// fixed_roles 直接查，动态角色含在内）。

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/store"
)

// roleEvolutionPromptMaxRunes 单角色自进化准则字数上限（trim 后按 rune 计）。
const roleEvolutionPromptMaxRunes = 300

// roleEvolutionStoreOps 是 handler 依赖的存储窄接口：*store.RoleEvolutionStore 天然实现；
// 测试注入内存 fake（具体类型裹 *sql.DB，无法在单测里直接构造）。
type roleEvolutionStoreOps interface {
	Get(ctx context.Context, roleID string) (*store.RoleEvolutionPrompt, error)
	Upsert(ctx context.Context, rec *store.RoleEvolutionPrompt) error
	List(ctx context.Context) ([]store.RoleEvolutionPrompt, error)
}

// SetRoleEvolution 注入角色自进化准则存储与角色注册表（未注入时端点 503）。
func (h *APIHandler) SetRoleEvolution(s *store.RoleEvolutionStore, roles *role.Registry) {
	if s == nil {
		// 防御 typed-nil：nil 具体指针塞进接口字段会使 == nil 判定失效，端点误放行。
		h.roleEvolution = nil
	} else {
		h.roleEvolution = s
	}
	h.roleRegistry = roles
}

// roleEvolutionPromptItem 列表响应项：role_name 取 RoleDefinition.Name，
// 存储孤立 role_id（注册表已删/改名）回落为 role_id 本身。
type roleEvolutionPromptItem struct {
	RoleID        string    `json:"role_id"`
	RoleName      string    `json:"role_name"`
	Content       string    `json:"content"`
	SourceSession string    `json:"source_session,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListRoleEvolutionPromptsHandler 处理 GET /api/roles/evolution-prompts —
// 全量角色自进化准则列表：注册表全集（meta/domain/fixed/动态）∪ 存储侧孤立 role_id；
// 无准则的角色 content 为空串照常返回（前端显示「暂无准则」）。
func (h *APIHandler) ListRoleEvolutionPromptsHandler(c *gin.Context) {
	if h.roleEvolution == nil || h.roleRegistry == nil {
		c.String(http.StatusServiceUnavailable, "role evolution store not wired")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	recs, err := h.roleEvolution.List(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	byRole := make(map[string]*store.RoleEvolutionPrompt, len(recs))
	for i := range recs {
		byRole[recs[i].RoleID] = &recs[i]
	}
	items := make([]roleEvolutionPromptItem, 0, len(recs))
	seen := make(map[string]bool, len(recs))
	for _, rd := range h.roleRegistry.List() {
		seen[rd.ID] = true
		name := strings.TrimSpace(rd.Name)
		if name == "" {
			name = rd.ID
		}
		item := roleEvolutionPromptItem{RoleID: rd.ID, RoleName: name}
		if rec := byRole[rd.ID]; rec != nil {
			item.Content = rec.Content
			item.SourceSession = rec.SourceSession
			item.UpdatedAt = rec.UpdatedAt
		}
		items = append(items, item)
	}
	// 存储里有但注册表没有的 role_id：照常列出（排序保证输出稳定），role_name 回落 role_id。
	orphans := make([]string, 0)
	for id := range byRole {
		if !seen[id] {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	for _, id := range orphans {
		rec := byRole[id]
		items = append(items, roleEvolutionPromptItem{
			RoleID:        id,
			RoleName:      id,
			Content:       rec.Content,
			SourceSession: rec.SourceSession,
			UpdatedAt:     rec.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, map[string]any{"prompts": items})
}

// SaveRoleEvolutionPromptHandler 处理 PUT /api/roles/:id/evolution-prompt —
// 编辑单角色自进化准则（body {"content": "..."}，trim 后 ≤300 字；空串 = 清空该角色准则）。
// 校验顺序：未接线 503 → 角色不存在 404 → body 非法/缺字段 400 → 超限 400（文案含当前字数）。
func (h *APIHandler) SaveRoleEvolutionPromptHandler(c *gin.Context) {
	if h.roleEvolution == nil || h.roleRegistry == nil {
		c.String(http.StatusServiceUnavailable, "role evolution store not wired")
		return
	}
	roleID := c.Param("id")
	if h.roleRegistry.Get(roleID) == nil {
		c.String(http.StatusNotFound, "role not found")
		return
	}
	var req struct {
		Content *string `json:"content"` // 指针区分「缺字段/null」(400) 与「空串」(合法清空)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	if req.Content == nil {
		c.String(http.StatusBadRequest, "content required")
		return
	}
	content := strings.TrimSpace(*req.Content)
	if n := utf8.RuneCountInString(content); n > roleEvolutionPromptMaxRunes {
		c.String(http.StatusBadRequest, "超过 %d 字上限（当前 %d 字）", roleEvolutionPromptMaxRunes, n)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	// 手动编辑无来源会话，SourceSession 留空；UpdatedAt 本地填充（存储侧落库 now()，秒级一致）。
	rec := &store.RoleEvolutionPrompt{
		RoleID:    roleID,
		Content:   content,
		UpdatedAt: time.Now(),
	}
	if err := h.roleEvolution.Upsert(ctx, rec); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, rec)
}
