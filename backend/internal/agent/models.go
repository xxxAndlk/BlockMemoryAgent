package agent

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// ModelManager 运行时动态切换角色模型的能力接口（可选能力，独立于 Agent 主接口：
// 避免给 agent.Agent 的全部测试桩追加方法）。由 ReactService 实现，HTTP 层与 TUI
// 通过类型断言/显式注入消费。
type ModelManager interface {
	// ListModels 返回模型目录：可切换预设清单 + 各角色当前生效模型（含覆写标记）。
	ListModels(ctx context.Context) (*ModelCatalog, error)
	// SwitchModel 把角色切换到指定预设的模型（含连通性探测，失败 fail-closed）。
	// 生效语义：domain/叶子下一次派发、lightweight/judge 下一次调用、meta 下一会话。
	SwitchModel(ctx context.Context, roleID, presetID string) (types.AgentModelConfig, error)
}

// ModelCatalog GET /api/models 的响应结构（TUI /model 弹窗同源数据）。
type ModelCatalog struct {
	// Presets 可切换预设清单（roles.yaml model_presets[]）。
	Presets []types.ModelPreset `json:"presets"`
	// Roles 可切换角色及其当前模型状态。
	Roles []RoleModelStatus `json:"roles"`
}

// RoleModelStatus 单个可切换角色的当前模型状态。
type RoleModelStatus struct {
	RoleID     string `json:"role_id"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	PresetID   string `json:"preset_id,omitempty"`
	Overridden bool   `json:"overridden"`
}

// ListModels 实现 ModelManager。
func (s *ReactService) ListModels(ctx context.Context) (*ModelCatalog, error) {
	catalog := &ModelCatalog{Presets: s.modelFactory.Presets()}
	roles := s.modelFactory.SwitchableRoles()
	catalog.Roles = make([]RoleModelStatus, 0, len(roles))
	for _, roleID := range roles {
		provider, modelName, presetID, overridden := s.modelFactory.CurrentModelInfo(roleID)
		catalog.Roles = append(catalog.Roles, RoleModelStatus{
			RoleID:     roleID,
			Provider:   provider,
			Model:      modelName,
			PresetID:   presetID,
			Overridden: overridden,
		})
	}
	return catalog, nil
}

// SwitchModel 实现 ModelManager，委托工厂（内含探测 + 持久化 + 缓存失效）。
func (s *ReactService) SwitchModel(ctx context.Context, roleID, presetID string) (types.AgentModelConfig, error) {
	return s.modelFactory.SwitchModel(ctx, roleID, presetID)
}
