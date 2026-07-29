// role 包提供基于 config/roles.yaml 的角色定义运行时访问能力。
// Registry 是 ReActAgent 系统提示词与子代理调用权限的唯一可信来源。
package role

// 导入 Registry 依赖的外部包。
import (
	"strings"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// defaultDomainAgentSystemPrompt 是 DomainAgent 系统提示词的兜底默认值。
// roles.yaml 未配置 domain_agent.system_prompt 时使用，保证空配置可启动。
// 内容与 config/roles.yaml 中 domain_agent.system_prompt 保持一致；
// 任一处修改需同步另一处，避免行为漂移。
const defaultDomainAgentSystemPrompt = `你是领域负责人（DomainAgent），负责把父 Agent 交办的目标在你负责的领域内落地。你可读文件/联网采集上下文，拆分到单函数级后派发或自执行。

【可用工具】
- call_sub_agent(role_id, task)：派发子 Agent 执行叶子任务。
- ReadFile / ListDir / SearchInFiles：读文件、列目录、符号检索，用于采集上下文、定位关键代码。
- HTTPGet：联网获取文档/API/参考资料。
- WriteSharedMemory(content)：把采集到的关键上下文写入共享记忆，被派发的子 Agent 自动读取。
- WriteFile / RunCommand：自执行改动时使用；若任务可完全交给助手则不必动用。

【工作模式】
1. 接到领域目标先采集上下文：用 SearchInFiles / ListDir / ReadFile 定位关键文件、函数签名、行号。
   ReadFile 同一文件不超过 1 次，单次不超 200 行；只取路径、行号、签名、关键结论，不抄全文。
2. 拆分到单函数级：把领域目标拆到"单函数 / 单文件 / 单个具体改动"级别，每个子任务边界清晰、可独立验收。
3. 决策派发 vs 自执行：
   - 涉及多文件/多函数/复杂逻辑的子任务 -> call_sub_agent 派给固定助手。
   - 单点改动/快速修复/验证性命令 -> 可自执行 (WriteFile/RunCommand) 后整合进结论。
4. 派发工具选择：
   - code_assistant：代码编写、审查、重构、调试
   - ui_assistant：前端 UI、样式、组件
   - test_assistant：测试编写与执行
   - doc_assistant：技术文档与注释
   - prompt_reviewer：提示词审查

【拆分粒度纪律】
- 一个子任务对应一个函数或一个文件改动，不要"把整个模块实现"塞给一个子任务。
- 子任务之间有依赖时，等前一个 mailbox 摘要回来再派发下一个；无依赖可并行。
- task 必须自包含但 <= 500 字：背景、目标、相关文件路径与行号、前置结论、验收标准。
  规格原文写入 WriteSharedMemory，task 只写目标+验收标准；子 Agent 看不到本次对话历史。
- 不要把 MetaAgent 注入的共享记忆原文塞进 task；只提炼子 Agent 落地所需的关键点。

【不要做的事】
- 不要把整个领域目标不拆分就丢给一个子 Agent。
- 不要重复派发同一子任务；mailbox 摘要回来就整合进结论。
- 不要 ReadFile 全文后把原文塞进 task；用 WriteSharedMemory 传关键点。

【结果汇总】
- 子 Agent 完成后你会收到 [mailbox from <id>] 的结果摘要，按拆分顺序整合为最终结论。
- 自执行的部分直接写入结论。
- 你的最终答复就是回灌给父 Agent 的交付物：结论先行、自包含、附关键文件路径与验收证据；不要写过程流水账。`

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
		// Tools 暴露 call_sub_agent + WriteSharedMemory + HTTPGet：MetaAgent context 最贵
		// （长跑 + 累积 mailbox 摘要），架构层禁 ReadFile/ListDir/SearchInFiles 防止越位读
		// 文件 + 把原文粘进 task（log 实证 MetaAgent 违反 prompt 自律）。读文件交给 DomainAgent。
		// 保留 HTTPGet：MetaAgent 偶尔需联网查文档/API 参考，不涉及大块上下文。
		return &types.RoleDefinition{
			ID:           "meta",
			Name:         "MetaAgent",
			Type:         enums.RoleTypeMeta,
			SystemPrompt: r.cfg.MetaAgent.SystemPrompt,
			ModelConfig:  r.cfg.MetaAgent.ModelConfig,
			Tools:        []string{"call_sub_agent", "WriteSharedMemory", "WriteSpec", "HTTPGet"},
			CanBeCalled:  false,
		}
	case "domain":
		// 合成 DomainAgent：复用配置的 DomainAgent 模型配置与系统提示词。
		// CanBeCalled 为 true，允许上层编排者（MetaAgent）将其作为子代理调用。
		// Tools 暴露读+执行+派发全工具：DomainAgent 是 per-task 短命 agent，可承担读文件/联网
		// 采集上下文 + 拆分到单函数级 + 派发助手 OR 自执行的完整职责。
		// 提示词默认值兜底：未在 roles.yaml 配置时使用内置默认值，保证空配置可启动。
		domainPrompt := r.cfg.DomainAgent.SystemPrompt
		if strings.TrimSpace(domainPrompt) == "" {
			domainPrompt = defaultDomainAgentSystemPrompt
		}
		return &types.RoleDefinition{
			ID:           "domain",
			Name:         "DomainAgent",
			Type:         enums.RoleTypeDomain,
			SystemPrompt: domainPrompt,
			ModelConfig:  r.cfg.DomainAgent.ModelConfig,
			Tools: []string{
				"call_sub_agent",
				"ReadFile", "ListDir", "SearchInFiles", "HTTPGet",
				"WriteSharedMemory", "WriteSpec", "WriteFile", "RunCommand",
			},
			CanBeCalled: true,
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
	// 例外：MetaAgent（Type=Meta）允许调用 DomainAgent（Type=Domain），
	// DomainAgent 是 MetaAgent 的下一级拆分者，非平级互调；DomainAgent 调用 DomainAgent
	// 仍属平级互调，被下面的 RoleTypeDomain 分支拒绝，避免递归。
	if caller.Type == enums.RoleTypeMeta || caller.Type == enums.RoleTypeDomain {
		// DomainAgent 作为 MetaAgent 的下级拆分者，允许被 MetaAgent 调用。
		if caller.Type == enums.RoleTypeMeta && callee.Type == enums.RoleTypeDomain {
			return callee.CanBeCalled
		}
		// 目标必须是固定角色或动态角色，才允许被编排者调用。
		if callee.Type == enums.RoleTypeFixed || callee.Type == enums.RoleTypeDynamic {
			// 返回目标角色是否显式声明可调用。
			return callee.CanBeCalled
		}
		// 编排者不能直接调用其他编排者角色（Meta->Meta、Domain->Domain、Domain->Meta），避免循环调用。
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
