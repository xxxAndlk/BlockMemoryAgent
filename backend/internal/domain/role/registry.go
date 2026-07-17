// role 包提供基于 config/roles.yaml 的角色定义运行时访问能力。
// Registry 是 ReActAgent 系统提示词与子代理调用权限的唯一可信来源。
package role

// 导入 Registry 依赖的外部包。
import (
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Registry 运行时角色注册表，封装已加载的角色配置。
// 它向上层提供统一接口，用于查询角色定义、判断调用权限以及枚举可调用的固定角色。
type Registry struct {
	// cfg 指向已加载的角色配置文件，是 Registry 的唯一依赖。
	cfg *config.RoleConfigFile
}

// NewRegistry 使用一个已经加载的角色配置构造 Registry。
// 参数 cfg 是从 config/roles.yaml 解析得到的角色配置；返回指向新 Registry 实例的指针。
func NewRegistry(cfg *config.RoleConfigFile) *Registry {
	// 直接封装配置指针，Registry 不负责深拷贝，保持轻量。
	return &Registry{cfg: cfg}
}

// Get 根据逻辑角色 ID 返回对应的角色定义。
// 内置 ID "meta" 与 "domain" 由配置顶层字段合成；
// 其余固定角色 ID 从 fixed_roles 列表中查找。
// 参数 roleID 为待查询的角色逻辑 ID；若 Registry 未初始化或角色不存在则返回 nil。
func (r *Registry) Get(roleID string) *types.RoleDefinition {
	// 防御性检查：Registry 未初始化或配置未加载时无法提供角色定义，直接返回 nil。
	if r == nil || r.cfg == nil {
		return nil
	}

	// 根据角色 ID 分发到内置合成角色或固定角色。
	switch roleID {
	case "meta":
		// 合成 MetaAgent：从配置的 MetaAgent 字段提取系统提示词与模型配置。
		// CanBeCalled 为 false，因为元代理作为顶层协调者，不应被其他角色直接调用。
		return &types.RoleDefinition{
			ID:           "meta",
			Name:         "MetaAgent",
			Type:         enums.RoleTypeMeta,
			SystemPrompt: r.cfg.MetaAgent.SystemPrompt,
			ModelConfig:  r.cfg.MetaAgent.ModelConfig,
			CanBeCalled:  false,
		}
	case "domain":
		// 合成 DomainAgent：复用配置的 DomainAgent 模型配置。
		// CanBeCalled 为 true，允许上层编排者将其作为子代理调用。
		return &types.RoleDefinition{
			ID:   "domain",
			Name: "DomainAgent",
			Type: enums.RoleTypeDomain,
			// 领域负责人提示词：明确"自己做 vs 派发"的决策标准、任务派发纪律，
			// 以及"最终答复即回灌给父 Agent 的交付物"这一输出契约。
			SystemPrompt: `你是领域负责人（DomainAgent），负责把父 Agent 交办的目标在你负责的领域内落地。

【工作方式】
1. 先分析任务：能直接完成的，自己用工具完成，不要派发。
2. 需要专业分工或多步骤并行时，用 call_sub_agent 派给合适的固定助手（如 code_assistant / ui_assistant / test_assistant / doc_assistant）。
3. 派发的 task 必须自包含：背景、目标、相关文件路径、前置结论与验收标准；前置已读过的内容不要让助手重读。

【结果汇总】
- 子 Agent 完成后你会收到 [mailbox from <id>] 的结果摘要，将其与你的产出整合为最终结论。
- 你的最终答复就是回灌给父 Agent 的交付物：结论先行、自包含、附关键文件路径；不要写过程流水账。`,
			ModelConfig:  r.cfg.DomainAgent.ModelConfig,
			CanBeCalled:  true,
		}
	default:
		// 非内置角色：交给底层配置查找 fixed_roles 中是否存在对应 ID 的角色。
		return r.cfg.GetFixedRole(roleID)
	}
}

// CanCall 判断 callerRoleID 是否有权调用 calleeRoleID。
// 元代理和领域代理可以调用任意声明为 callable 的固定角色或动态角色；
// 固定角色只能调用其 parents 允许列表中显式列出的角色。
func (r *Registry) CanCall(callerRoleID, calleeRoleID string) bool {
	// 若 Registry 或配置未初始化，无法判断权限，默认拒绝调用。
	if r == nil || r.cfg == nil {
		return false
	}

	// 获取调用方与被调用方的角色定义，为后续权限判断做准备。
	caller := r.Get(callerRoleID)
	callee := r.Get(calleeRoleID)
	// 任意一方角色不存在，说明 ID 非法或未配置，禁止调用。
	if caller == nil || callee == nil {
		return false
	}

	// 元代理与领域代理属于编排者：它们可以把任务分发给固定角色或动态角色，
	// 但前提是目标角色自身声明了可被调用（CanBeCalled 为 true）。
	if caller.Type == enums.RoleTypeMeta || caller.Type == enums.RoleTypeDomain {
		// 目标必须是固定角色或动态角色，才允许被编排者调用。
		if callee.Type == enums.RoleTypeFixed || callee.Type == enums.RoleTypeDynamic {
			// 返回目标角色是否显式声明可调用。
			return callee.CanBeCalled
		}
		// 编排者不能直接调用其他编排者角色，避免循环调用。
		return false
	}

	// 固定角色：必须遵守其 parents 字段中的显式允许列表。
	for _, parent := range caller.Parents {
		// 若 calleeRoleID 出现在 caller 的父角色列表中，则允许调用。
		if parent == calleeRoleID {
			return true
		}
	}
	// 未在允许列表中找到匹配项，拒绝调用。
	return false
}

// CallableFixedRoles 返回所有标记为可调用的固定角色。
// 这些角色可以被暴露为子代理工具供上层调用。
func (r *Registry) CallableFixedRoles() []types.RoleDefinition {
	// Registry 或配置未初始化时，没有可用角色，返回 nil。
	if r == nil || r.cfg == nil {
		return nil
	}

	// out 用于收集可调用的固定角色切片，初始为空。
	var out []types.RoleDefinition
	// 遍历配置中所有固定角色，筛选出 CanBeCalled 为 true 的角色。
	for _, role := range r.cfg.FixedRoles {
		// 仅当角色显式声明可被调用时才加入结果集。
		if role.CanBeCalled {
			// 将符合条件的角色追加到结果切片。
			out = append(out, role)
		}
	}
	// 返回筛选后的角色列表；若无匹配角色则返回空切片或 nil。
	return out
}
