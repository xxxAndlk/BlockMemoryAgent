// Package agent 实现领域层工具注册表到 agent 层 ToolRegistry 接口的适配，
// 避免 agent 包与 domain/tool 包之间产生循环导入。
package agent

import (
	// context 提供带取消、超时、键值对的能力，用于在工具调用链路中传递请求上下文。
	"context"
	"strings"

	// domain/tool 是领域层的工具注册表，避免 agent 包直接依赖它而导致循环导入。
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	// blades/tools 提供工具描述的通用结构，用于与大模型交互时的工具模式声明。
	"github.com/go-kratos/blades/tools"
)

// ToolVisibilityFunc 是插件工具可见性回调（设计文档 §4.3）：
// 返回 (owned, visible)：owned=true 表示该工具由插件注册（非内置工具），
// visible 表示插件 Manifest.Roles 是否允许当前角色可见。
// 由 bootstrap 注入 plugins.Manager.ToolVisibility。
type ToolVisibilityFunc func(roleID, toolName string) (owned, visible bool)

// toolRegistryAdapter 是一个适配器，将领域层 domain/tool.Registry 适配为
// agent 包内部的 ToolRegistry 接口，同时避免 agent 包与 domain/tool 包之间产生循环导入。
type toolRegistryAdapter struct {
	// inner 是被包装的领域层工具注册表，所有调用最终都会委托给它。
	inner *tool.Registry
	// allowed 是工具白名单；非空时 Schema() 只暴露白名单内的工具。
	// nil/空 map 表示不过滤（向后兼容：所有注册工具都暴露给 LLM）。
	// Dispatch 不受白名单限制，仍可执行任意已注册工具——verifyloop 的 ExecuteChild
	// 直接走 inner.Dispatch，绕过 adapter，不受此白名单影响。
	allowed map[string]bool
	// roleID 是持有该适配器的 Agent 角色 ID（"meta"/"domain"/动态角色），
	// 供 pluginVisibility 判定插件工具可见性。
	roleID string
	// scope 是持有该适配器的 Agent 作用域（agentID）：MetaAgent=sessionID、
	// 子 Agent=subAgentID。供 Schema() 读取本 Agent 已挂载的插件工具集（TODO #52）。
	scope string
	// pluginVisibility 热插拔插件动态可见集回调（设计文档 §4.3）：
	// fn(roleID, toolName) -> (owned, visible)；nil 时插件工具不做角色过滤。
	pluginVisibility ToolVisibilityFunc
}

// NewToolRegistryAdapter 接收一个领域层工具注册表 r，返回一个实现了 agent.ToolRegistry
// 接口的适配器实例，供 ReActAgent 使用。不做工具过滤，所有已注册工具都暴露给 LLM。
//
// 参数:
//
//	r - 领域层的工具注册表，包含实际工具的注册、模式与分发能力。
//
// 返回值:
//
//	ToolRegistry - 实现了 agent 包 ToolRegistry 接口的适配器对象。
func NewToolRegistryAdapter(r *tool.Registry) ToolRegistry {
	// 构造适配器实例，将领域层注册表保存在 inner 字段中，后续所有方法都通过它转发。
	return &toolRegistryAdapter{inner: r}
}

// NewToolRegistryAdapterWithFilter 构造带工具白名单的适配器。
// allowed 为 nil 或空切片时等价于 NewToolRegistryAdapter（不过滤）。
// 非空时 Schema() 只返回 allowed 中列出的工具，用于按角色约束可调用工具集
// （例如 MetaAgent 只暴露 call_sub_agent，固定助手不暴露 call_sub_agent）。
//
// 参数:
//
//	r       - 领域层的工具注册表。
//	allowed - 允许暴露给 LLM 的工具名白名单。
//
// 返回值:
//
//	ToolRegistry - 带过滤的适配器对象。
func NewToolRegistryAdapterWithFilter(r *tool.Registry, allowed []string) ToolRegistry {
	m := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		if name != "" {
			m[name] = true
		}
	}
	return &toolRegistryAdapter{inner: r, allowed: m}
}

// NewToolRegistryAdapterForRole 构造带角色上下文的适配器（热插拔插件可见性）。
// allowed 为 nil 或空切片时等价于不过滤；roleID 是持有者的角色 ID；
// scope 是持有者的作用域（agentID，供读取本 Agent 已挂载插件工具集）；
// visibility 为 nil 时等价于 NewToolRegistryAdapterWithFilter（白名单语义不变）。
// Schema() 过滤逻辑 = 「静态白名单 ∪（插件动态可见集 ∩ 本 Agent 已挂载集）」（TODO #52）：
//   - 白名单非空：白名单命中直接暴露；白名单外仅插件工具（owned=true）、角色可见
//     （visible=true）**且已挂载**（tool_mount / 派发 tools_hint / plugin_install 自动挂载）
//     才暴露——默认收窄为角色基础工具，插件工具按需挂载，缓解全量 schema 注入的上下文膨胀；
//   - 白名单为空：插件工具按（可见性 ∩ 已挂载）隐藏，其余工具全部暴露。
//
// 参数:
//
//	r          - 领域层的工具注册表。
//	scope     - 持有该适配器的 Agent 作用域（agentID）。
//	allowed    - 允许暴露给 LLM 的工具名白名单。
//	roleID     - 持有该适配器的 Agent 角色 ID。
//	visibility - 插件工具可见性回调 fn(roleID, toolName) -> (owned, visible)；nil 表示不过滤插件工具。
//
// 返回值:
//
//	ToolRegistry - 带过滤的适配器对象。
func NewToolRegistryAdapterForRole(r *tool.Registry, scope string, allowed []string, roleID string, visibility ToolVisibilityFunc) ToolRegistry {
	m := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		if name != "" {
			m[name] = true
		}
	}
	return &toolRegistryAdapter{inner: r, scope: scope, allowed: m, roleID: roleID, pluginVisibility: visibility}
}

// Schema 返回当前注册表下所有工具的 JSON Schema 描述。
// 过滤语义：静态白名单（allowed）∪（插件动态可见集 ∩ 本 Agent 已挂载集）（TODO #52）。
// 白名单为空且无可见性回调时原样返回全部工具（向后兼容）。
// 返回值:
//
//	[]tools.Tool - 工具列表，每个元素包含工具名、描述与参数模式，供大模型决策使用。
func (a *toolRegistryAdapter) Schema() []tools.Tool {
	all := a.inner.Schema()
	// 无过滤配置：原样返回。
	if len(a.allowed) == 0 && a.pluginVisibility == nil {
		return all
	}
	// 已挂载插件工具集（tool_mount / tools_hint / plugin_install 自动挂载，TODO #52）。
	mounted := a.inner.MountedTools(a.scope)
	// 按白名单 + 插件可见性∩挂载过滤；保持原注册顺序，便于工具列表稳定。
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		owned, visible := false, true
		if a.pluginVisibility != nil {
			owned, visible = a.pluginVisibility(a.roleID, t.Name())
		}
		if len(a.allowed) > 0 {
			if a.allowed[t.Name()] {
				out = append(out, t)
				continue
			}
			// 白名单未命中：仅插件工具可经（可见性 ∩ 已挂载）加入（非插件工具不越权）。
			if !owned || !visible || !mounted[t.Name()] {
				continue
			}
		} else if owned && (!visible || !mounted[t.Name()]) {
			// 无白名单：插件工具按（可见性 ∩ 已挂载）隐藏；非插件工具全部保留。
			continue
		}
		out = append(out, t)
	}
	return out
}

// Dispatch 将 agent 层的 ToolCall 转换为领域层调用格式，再映射返回的 domain Result
// 为 agent 层可识别的 ToolResult。
//
// 参数:
//
//	ctx  - 请求上下文，用于超时、取消和链路追踪。
//	call - agent 层发起的工具调用请求，包含工具名与输入参数。
//
// 返回值:
//
//	ToolResult - 封装工具执行结果，包括工具名、是否成功、输出与错误信息。
//	error      - 当领域层调用失败时返回的 Go 错误。
func (a *toolRegistryAdapter) Dispatch(ctx context.Context, call ToolCall) (ToolResult, error) {
	// 调用领域层注册表的分发能力，将工具名 call.Name 与输入参数 call.Input 透传过去。
	// res 是领域层返回的结果，err 是执行过程中可能发生的错误。
	res, err := a.inner.Dispatch(ctx, call.Name, call.Input)
	if err != nil {
		// 如果领域层调用失败，构造一个失败的 ToolResult：
		// Tool 字段记录被调用工具名；Error 字段保存错误的字符串描述；
		// 同时把原始 err 返回，让上层能够感知并处理这个错误。
		return ToolResult{Tool: call.Name, Error: err.Error()}, err
	}
	// 调用成功时，将领域层结果 res 的各字段原样映射到 agent 层 ToolResult 中返回。
	// Success 表示工具是否执行成功，Output 存放工具输出，Error 存放工具自身报告的错误。
	// Images 为 image_passthrough 插件工具返回的图片（截图回显），仅内存透传。
	out := ToolResult{
		Tool:    res.Tool,
		Success: res.Success,
		Output:  res.Output,
		Error:   res.Error,
		Images:  res.Images,
	}
	// 视觉能力门控兜底（2026-09-16）：模型不支持图片输入时，任何来源的工具图片
	// （ui_preview 截图透传等，ReadMedia 已在工具内自拦）在此统一剥离，避免下一轮
	// 请求带 image block 被 provider 400 拒绝整轮。文本 Output 保留（含路径/说明）。
	if len(out.Images) > 0 && !tool.ImageInputSupportedOf(ctx) {
		out.Output = strings.TrimSpace(out.Output + "\n（图像已省略：当前模型不支持图片输入，" +
			"需要看图请换视觉模型或用 escalate_gear 升档。）")
		out.Images = nil
	}
	// 截图/图片降采样（TODO 第9项④）：全部工具图片的唯一汇流点。png/jpeg 长边超上限
	// 等比缩小（不放大小图），原图落盘 <workDir>/.bma/images/ 并在 Output 追加「原图已落盘」；
	// gif/webp 与缩放失败原图直通。上限 <=0 时零行为（bootstrap 未注入默认关闭）。
	if maxEdge := int(imageMaxEdgeCfg.Load()); maxEdge > 0 {
		downsampleResultImages(ctx, call.Name, &out, maxEdge)
	}
	return out, nil
}
