package agent

import (
	// context 提供带取消、超时、键值对的能力，用于在工具调用链路中传递请求上下文。
	"context"

	// domain/tool 是领域层的工具注册表，避免 agent 包直接依赖它而导致循环导入。
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	// blades/tools 提供工具描述的通用结构，用于与大模型交互时的工具模式声明。
	"github.com/go-kratos/blades/tools"
)

// toolRegistryAdapter 是一个适配器，将领域层 domain/tool.Registry 适配为
// agent 包内部的 ToolRegistry 接口，同时避免 agent 包与 domain/tool 包之间产生循环导入。
type toolRegistryAdapter struct {
	// inner 是被包装的领域层工具注册表，所有调用最终都会委托给它。
	inner *tool.Registry
}

// NewToolRegistryAdapter 接收一个领域层工具注册表 r，返回一个实现了 agent.ToolRegistry
// 接口的适配器实例，供 ReActAgent 使用。
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

// Schema 返回当前注册表下所有工具的 JSON Schema 描述。
// 返回值:
//
//	[]tools.Tool - 工具列表，每个元素包含工具名、描述与参数模式，供大模型决策使用。
func (a *toolRegistryAdapter) Schema() []tools.Tool {
	// 直接委托给领域层注册表，保持代理层不做额外转换。
	return a.inner.Schema()
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
	return ToolResult{
		Tool:    res.Tool,
		Success: res.Success,
		Output:  res.Output,
		Error:   res.Error,
	}, nil
}
