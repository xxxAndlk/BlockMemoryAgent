package role

// model_tools.go 提供 list_models / set_role_model 两个模型选择工具（Part C）：
// MetaAgent 按模型条目描述为下层角色（domain + 固定角色）换档。
// 仅 MetaAgent 白名单可见（registry.go meta 默认列表 + roles.yaml meta_agent.tools）。
//
// 写路径完全委托 ModelFactory.SwitchModel（角色范围校验、条目存在、api_key 非空、
// 连通性探测 fail-closed、SetBinding 持久化、客户端缓存失效），工具层不重复实现。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// modelSwitchTimeout set_role_model 的整体墙钟上限（含连通性探测）。
// 探测内部自带 60s 超时（SwitchModel），此处 45s 子 ctx 作为更紧的外层上限，
// 保证 MetaAgent 循环阻塞可控。
const modelSwitchTimeout = 45 * time.Second

// AgentOverridesProvider 实例级模型覆盖清单（*model.ModelFactory 实现，可选）。
// 经接口断言使用：桩未实现时 list_models 不输出 agent_overrides 段。
type AgentOverridesProvider interface {
	AgentOverrides() []model.AgentOverrideInfo
}

// ModelSwitcher 模型切换能力契约，由 model.ModelFactory 满足。
type ModelSwitcher interface {
	// RegistryModels 返回模型注册表全部条目（按 ID 排序）。
	RegistryModels() []types.ModelEntry
	// SwitchableRoles 返回可动态切换的角色 ID（meta/domain/lightweight + fixed_roles）。
	SwitchableRoles() []string
	// CurrentModelInfo 返回角色当前生效模型状态。
	CurrentModelInfo(roleID string) (model.ModelStatus, error)
	// SwitchModel 切换角色模型绑定（校验/探活/持久化/缓存失效一体）。
	SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error)
}

// modelToolTargets 返回 set_role_model 允许的目标角色集：
// 可切换角色去掉 meta 与 lightweight。meta 是决策者本身（自换模型会移动后续所有
// 判断的基线）；lightweight 是全局基础设施工（总结/改写/检索），换档属用户侧决策。
// 二者仍可经 TUI /model 或 Web 选择器手动切换。
func modelToolTargets(switcher ModelSwitcher) []string {
	var out []string
	for _, r := range switcher.SwitchableRoles() {
		if r == "meta" || r == "lightweight" {
			continue
		}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// listModelsTool 实现 list_models 工具：列出注册表条目与各可切换角色的当前绑定。
type listModelsTool struct {
	switcher ModelSwitcher
}

// Name 返回工具标准名称 list_models。
func (t *listModelsTool) Name() string { return "list_models" }

// Aliases 返回 list_models 的别名。
func (t *listModelsTool) Aliases() []string { return []string{"models"} }

// Description 返回 LLM 可见描述。
func (t *listModelsTool) Description() string {
	return "列出模型注册表全部可选模型（id/name/provider/description/max_output_tokens）" +
		"以及每个可切换角色当前绑定的模型。无参数。" +
		"用于 set_role_model 前比对：任务形态（长程推理/峰值质量/高频小修）与当前模型的能力、成本是否匹配。"
}

// Execute 执行 list_models 工具调用。
func (t *listModelsTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	if t.switcher == nil {
		return &tool.Result{Tool: "list_models", Error: "model switcher not configured"}
	}
	entries := t.switcher.RegistryModels()
	modelsOut := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		item := map[string]any{
			"id":       e.ID,
			"provider": e.Provider,
			"model":    e.Model,
		}
		if e.Name != "" {
			item["name"] = e.Name
		}
		if e.Description != "" {
			item["description"] = e.Description
		}
		if e.MaxOutputTokens > 0 {
			item["max_output_tokens"] = e.MaxOutputTokens
		}
		modelsOut = append(modelsOut, item)
	}
	rolesOut := make([]map[string]any, 0)
	for _, roleID := range modelToolTargets(t.switcher) {
		item := map[string]any{"role_id": roleID}
		if st, err := t.switcher.CurrentModelInfo(roleID); err == nil {
			item["provider"] = st.Provider
			item["model"] = st.Model
			item["model_id"] = st.ModelID
			item["thinking"] = st.Thinking
		}
		rolesOut = append(rolesOut, item)
	}
	out := map[string]any{"models": modelsOut, "switchable_roles": rolesOut}
	// 实例级覆盖段（set_agent_model 设置）：提示覆盖优先于角色绑定，
	// 避免按角色绑定判断实际生效模型时被误导。
	if p, ok := t.switcher.(AgentOverridesProvider); ok {
		if ovs := p.AgentOverrides(); len(ovs) > 0 {
			items := make([]map[string]any, 0, len(ovs))
			for _, o := range ovs {
				items = append(items, map[string]any{
					"agent_id": o.AgentID, "role_id": o.RoleID,
					"model_id": o.ModelID, "model": o.Model, "thinking": o.Thinking,
				})
			}
			out["agent_overrides"] = items
		}
	}
	b, _ := json.Marshal(out)
	return &tool.Result{
		Tool:    "list_models",
		Success: true,
		Output:  string(b),
	}
}

// setRoleModelTool 实现 set_role_model 工具：为指定角色切换模型绑定。
type setRoleModelTool struct {
	switcher ModelSwitcher
}

// Name 返回工具标准名称 set_role_model。
func (t *setRoleModelTool) Name() string { return "set_role_model" }

// Aliases 返回 set_role_model 的别名。
func (t *setRoleModelTool) Aliases() []string { return []string{"switch_role_model"} }

// Description 返回 LLM 可见描述。
func (t *setRoleModelTool) Description() string {
	return "为下层角色切换模型（仅 domain 与固定角色；meta/lightweight 不可由本工具切换）。" +
		"参数：role_id（先用 list_models 查看可切换角色与候选模型）、model_id（注册表条目 ID）、" +
		"reason（必填，说明为何该任务形态需要换档——写入操作记录供用户审计）。" +
		"切换前做连通性探测，失败保持原模型不变。生效时点：domain/叶子下一次派发、进行中任务不中断。" +
		"绑定持久化到 models.json，用户可随时在 TUI/Web 手动覆盖。默认不切换：仅在任务与当前模型明显不匹配时使用。"
}

// Execute 执行 set_role_model 工具调用。
func (t *setRoleModelTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	if t.switcher == nil {
		return &tool.Result{Tool: "set_role_model", Error: "model switcher not configured"}
	}
	roleID := strings.TrimSpace(strArg(args, "role_id"))
	modelID := strings.TrimSpace(strArg(args, "model_id"))
	reason := strings.TrimSpace(strArg(args, "reason"))
	if roleID == "" || modelID == "" {
		return &tool.Result{
			Tool:     "set_role_model",
			Error:    "role_id 与 model_id 必填（先 list_models 查看可切换角色与候选模型）",
			Category: tool.ResultCategoryValidationRejected,
		}
	}
	if reason == "" {
		return &tool.Result{
			Tool:     "set_role_model",
			Error:    "reason 必填：说明为何该任务形态需要换档（写入操作记录供用户审计）",
			Category: tool.ResultCategoryValidationRejected,
		}
	}
	allowed := false
	for _, r := range modelToolTargets(t.switcher) {
		if r == roleID {
			allowed = true
			break
		}
	}
	if !allowed {
		return &tool.Result{
			Tool: "set_role_model",
			Error: fmt.Sprintf("role %q 不允许由本工具切换（仅 domain 与固定角色；meta/lightweight 由用户在 TUI/Web 手动切换）", roleID),
			Category: tool.ResultCategoryValidationRejected,
		}
	}

	switchCtx, cancel := context.WithTimeout(ctx, modelSwitchTimeout)
	defer cancel()
	cfg, err := t.switcher.SwitchModel(switchCtx, roleID, modelID, "")
	if err != nil {
		return &tool.Result{
			Tool:     "set_role_model",
			Error:    err.Error(),
			Category: tool.ResultCategoryExecutionFailed,
		}
	}
	return &tool.Result{
		Tool:    "set_role_model",
		Success: true,
		Output: fmt.Sprintf("角色 %s 已切换至模型 %s（provider=%s, model=%s）。原因：%s。下次派发/调用生效，进行中任务不中断。",
			roleID, modelID, cfg.Provider, cfg.Model, reason),
	}
}

// strArg 读取字符串参数（兼容 nil/非 string）。
func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// RegisterModelTools 把 list_models / set_role_model 安装到工具注册表。
// switcher 通常传 *model.ModelFactory；nil 时工具仍注册但执行时报未配置
// （保持与 RegisterTools 一致的降级行为）。
func RegisterModelTools(tr *tool.Registry, switcher ModelSwitcher) {
	if tr == nil {
		return
	}
	tr.Register(&listModelsTool{switcher: switcher})
	tr.Register(&setRoleModelTool{switcher: switcher})
}
