package agent

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// ModelManager 运行时动态切换角色模型的能力接口（可选能力，独立于 Agent 主接口：
// 避免给 agent.Agent 的全部测试桩追加方法）。由 ReactService 实现，HTTP 层与 TUI
// 通过类型断言/显式注入消费。
type ModelManager interface {
	// ListModels 返回模型目录：模型注册表清单 + 各角色当前生效模型（含绑定标记）。
	ListModels(ctx context.Context) (*ModelCatalog, error)
	// SwitchModel 把角色绑定到指定模型条目并可选覆盖思考强度（含连通性探测，
	// 失败 fail-closed）。生效语义：domain/叶子下一次派发、lightweight/judge
	// 下一次调用、meta 下一会话。
	SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error)
	// AddModelEntry 新增模型条目到注册表（config/models.json）并落盘。
	AddModelEntry(entry types.ModelEntry) error
}

// ModelCatalog GET /api/models 的响应结构（TUI /model 弹窗同源数据）。
// 视图结构：不含 api_key（注册表明文 key 不得经 HTTP 泄漏）。
type ModelCatalog struct {
	// Models 模型注册表清单（config/models.json models[]）。
	Models []ModelEntryView `json:"models"`
	// Roles 可切换角色及其当前模型状态。
	Roles []RoleModelStatus `json:"roles"`
}

// ModelEntryView 模型条目的对外视图（剔除 api_key）。
type ModelEntryView struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
	// MaxOutputTokens 条目声明的最大输出 token（0=未声明）。
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Description 能力/成本描述（MetaAgent 选模型与界面展示用）。
	Description string `json:"description,omitempty"`
	// SelectableRoles 切换允许的角色白名单（空=全员可用；界面按目标角色过滤候选）。
	SelectableRoles []string `json:"selectable_roles,omitempty"`
}

// RoleModelStatus 单个可切换角色的当前模型状态。
type RoleModelStatus struct {
	RoleID   string `json:"role_id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ModelID 生效来源条目 ID（绑定优先，其次 model_ref）；内联配置为空。
	ModelID string `json:"model_id,omitempty"`
	// Thinking 当前生效思考档位（原始值，空=端点默认）。
	Thinking string `json:"thinking,omitempty"`
	// Bound 是否存在运行时绑定（models.json role_bindings）。
	Bound bool `json:"bound"`
}

// ListModels 实现 ModelManager。
func (s *ReactService) ListModels(ctx context.Context) (*ModelCatalog, error) {
	entries := s.modelFactory.RegistryModels()
	catalog := &ModelCatalog{Models: make([]ModelEntryView, 0, len(entries))}
	for _, e := range entries {
		catalog.Models = append(catalog.Models, ModelEntryView{
			ID:              e.ID,
			Name:            e.Name,
			Provider:        e.Provider,
			Model:           e.Model,
			BaseURL:         e.BaseURL,
			MaxOutputTokens: e.MaxOutputTokens,
			Description:     e.Description,
			SelectableRoles: e.SelectableRoles,
		})
	}
	roles := s.modelFactory.SwitchableRoles()
	catalog.Roles = make([]RoleModelStatus, 0, len(roles))
	for _, roleID := range roles {
		st, err := s.modelFactory.CurrentModelInfo(roleID)
		if err != nil {
			// 解析失败的角色以空状态列出（前端可见，切换时可获具体报错）。
			catalog.Roles = append(catalog.Roles, RoleModelStatus{RoleID: roleID})
			continue
		}
		catalog.Roles = append(catalog.Roles, RoleModelStatus{
			RoleID:   roleID,
			Provider: st.Provider,
			Model:    st.Model,
			ModelID:  st.ModelID,
			Thinking: st.Thinking,
			Bound:    st.Bound,
		})
	}
	return catalog, nil
}

// SwitchModel 实现 ModelManager，委托工厂（内含探测 + 持久化 + 缓存失效）。
func (s *ReactService) SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	return s.modelFactory.SwitchModel(ctx, roleID, modelID, thinking)
}

// AddModelEntry 实现 ModelManager，委托工厂（校验 + 原子落盘）。
func (s *ReactService) AddModelEntry(entry types.ModelEntry) error {
	return s.modelFactory.AddModel(entry)
}
