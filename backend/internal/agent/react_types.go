// Package agent 定义 ReActAgent 运行时使用的基础类型、接口与消息转换函数，
// 使 agent 包对外提供稳定的小型 DTO。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/tools"
)

// ReactMessage 表示 ReActAgent 对话历史中的一轮消息。
// 它故意与 blades.Message 解耦，这样 agent 包可以向调用方和测试提供一个小而稳定的 DTO。
type ReactMessage struct {
	Role      string     `json:"role"`                 // Role 表示消息角色，例如 user/assistant/tool/system
	Content   string     `json:"content"`              // Content 是消息的文本内容
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // ToolCalls 是 assistant 消息附带的工具调用请求列表
	// ToolCallID 仅 role="tool" 使用：标记该工具结果对应的工具调用 ID。
	// Anthropic/OpenAI 原生工具协议要求 tool_result 必须引用存在的 tool_use id，
	// 缺失会导致下一轮请求被 API 拒绝（400 tool_call_id is not found）。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall 表示模型请求执行某个命名工具的调用。
// 输入参数以 JSON 形状的 map 形式承载。
type ToolCall struct {
	ID    string         `json:"id"`    // ID 是本次工具调用的唯一标识，用于结果回传时匹配
	Name  string         `json:"name"`  // Name 是要调用的工具名称
	Input map[string]any `json:"input"` // Input 是传递给工具的参数键值对
}

// LastAssistantText 从 ReAct 历史中提取最后一条非空 assistant 文本，
// 用于执行失败/超时时向调用方回传已达成的部分进度；没有时返回空串。
func LastAssistantText(history []ReactMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			return history[i].Content
		}
	}
	return ""
}

// ToolResult 表示执行一次 ToolCall 后的结果。
type ToolResult struct {
	Tool    string `json:"tool"`            // Tool 是产生该结果的工具名称
	Success bool   `json:"success"`         // Success 标记工具执行是否成功
	Output  string `json:"output"`          // Output 是工具执行成功时的输出内容
	Error   string `json:"error,omitempty"` // Error 是工具执行失败时的错误信息
}

// ReactResult 是 ReActAgent 一次运行结束时返回的结果。
type ReactResult struct {
	Text    string         `json:"text"`    // Text 是模型最终生成的文本答案
	History []ReactMessage `json:"history"` // History 是本次完整对话历史，包含所有轮次
	// LimitReached 为 true 表示达到最大轮数上限而暂停（非错误）：
	// Text 为空，History 保留全部进度，上层应暂停会话并等待用户消息续跑。
	LimitReached bool `json:"limit_reached,omitempty"`
}

// 实时进度事件种类：用于向 UI 推送 ReAct 运行过程中的中间状态。
const (
	// LiveEventLLMDelta 是 LLM 流式输出增量事件，Text 为截至当前的累积文本（非单块增量）。
	LiveEventLLMDelta = "llm_delta"
	// LiveEventToolCall 是工具调用开始事件（工具尚未执行完）。
	LiveEventToolCall = "tool_call"
	// LiveEventToolExec 是工具执行完成事件。
	LiveEventToolExec = "tool_exec"
	// LiveEventThinkDelta 是模型"思考过程"增量事件，Text 为截至当前的累积思考文本。
	// 思考内容是瞬时的：仅用于展示当前 LLM 调用思考阶段的实时过程，
	// 答复文本开始输出或会话结束时即被清除，不持久化。
	LiveEventThinkDelta = "think_delta"
	// LiveEventSubAgentDone 是子 Agent 完成事件（mailbox 收到子 Agent 结果摘要时触发），
	// Tool 字段携带子 Agent ID。
	LiveEventSubAgentDone = "sub_agent_done"
)

// LiveEvent 是 ReAct 运行过程中的实时进度事件，
// 由 WithLiveEvents 注册的回调接收，供会话层写入事件流/流式状态以驱动 UI 实时渲染。
type LiveEvent struct {
	Kind    string // Kind 事件种类（LiveEventLLMDelta / LiveEventToolCall / LiveEventToolExec）
	Agent   string // Agent 产生事件的 Agent 标识（emit 时自动填充）
	Text    string // Text 仅 llm_delta 使用：截至当前的累积文本
	Tool    string // Tool 工具名（tool_call / tool_exec 使用）
	Input   string // Input 工具入参 JSON（tool_call 使用）
	Output  string // Output 工具输出（tool_exec 使用）
	Error   string // Error 工具错误信息（tool_exec 使用）
	Success bool   // Success 工具是否执行成功（tool_exec 使用）
}

// ToolRegistry 抽象了 ReActAgent 可调用的工具集合。
// 通过接口屏蔽底层实现，方便在测试中替换为模拟对象。
type ToolRegistry interface {
	// Schema 返回暴露给模型的 blades 工具定义列表。
	Schema() []tools.Tool
	// Dispatch 执行单个工具调用并返回其结果。
	// ctx 用于控制调用生命周期；call 是模型发起的工具调用请求。
	Dispatch(ctx context.Context, call ToolCall) (ToolResult, error)
}

// RoleProvider 负责解析角色定义，ReActAgent 需要它来构建系统提示并校验子代理调用权限。
type RoleProvider interface {
	// Get 根据 roleID 获取对应的角色定义。
	Get(roleID string) *types.RoleDefinition
	// CanCall 判断 callerID 是否有权限调用 calleeID 对应的子代理。
	CanCall(callerID, calleeID string) bool
}

// PendingChildrenChecker 抽象"父 Agent 当前是否有未决子 Agent"的查询与等待能力，
// 由 subagent.Dispatcher 实现。ReActAgent 在给出终答前调用它检查：若有未决子 Agent，
// 则阻塞等待其完成而非终结会话，防止迟到 mailbox 消息随会话销毁丢失
// （多 Agent 协作验证闭环的关键正确性保障）。
type PendingChildrenChecker interface {
	// PendingChildren 返回指定父 Agent 当前未完成的子 Agent 数量。
	PendingChildren(parentID string) int
	// WaitForAnyChild 阻塞等待父 Agent 任一子 Agent 完成，最长 timeout。
	// 返回 true 表示收到完成信号（或调用时已无未决）；false 表示超时。
	WaitForAnyChild(parentID string, timeout time.Duration) bool
}

// ModelProvider 是 ReActAgent 所需的 blades.ModelProvider 的最小子集。
// 保留一个窄接口，使测试只需模拟 Generate 方法即可。
type ModelProvider interface {
	// Generate 向模型发送请求并返回模型响应。
	// ctx 控制请求上下文；req 包含对话历史与工具定义等请求信息。
	Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error)
}

// ToBladesMessages 将我们的对话历史转换为 blades 消息切片，用于构造模型请求。
// 返回的切片不会与源切片共享可变状态，避免后续修改相互影响。
func ToBladesMessages(history []ReactMessage) []*blades.Message {
	// 预分配与 history 长度相同的容量，减少 append 过程中的内存分配。
	out := make([]*blades.Message, 0, len(history))
	// 遍历每一轮对话消息，根据角色转换为 blades 对应的消息类型。
	for _, m := range history {
		// 根据消息角色进入不同分支处理。
		switch m.Role {
		case "user":
			// user 角色直接转换为 blades 的用户消息。
			out = append(out, blades.UserMessage(m.Content))
		case "assistant":
			// assistant 角色需要同时处理文本内容和可能的工具调用。
			msg := blades.AssistantMessage()
			// 当文本内容非空时，追加文本部分到消息中。
			if m.Content != "" {
				msg.Parts = append(msg.Parts, blades.TextPart{Text: m.Content})
			}
			// 遍历所有工具调用，将其序列化为 blades.ToolPart。
			for _, tc := range m.ToolCalls {
				// 将工具输入 map 序列化为 JSON 字符串作为 Request 字段。
				// 这里忽略序列化错误，因为输入来自已解析的合法 JSON。
				req, _ := json.Marshal(tc.Input)
				msg.Parts = append(msg.Parts, blades.ToolPart{
					ID:      tc.ID,
					Name:    tc.Name,
					Request: string(req),
				})
			}
			// 将组装好的 assistant 消息追加到结果切片。
			out = append(out, msg)
		case "tool":
			// tool 角色表示工具执行结果，编码为一个带有 Response 字段的 ToolPart。
			// 调用方应把工具产生的 JSON 结果设置到 Content 中传入；
			// ID 必须携带对应的工具调用 ID，否则 API 会因 tool_call_id 不匹配拒绝请求。
			out = append(out, &blades.Message{
				Role: blades.RoleTool,
				Parts: []blades.Part{
					blades.ToolPart{ID: m.ToolCallID, Response: m.Content},
				},
			})
		case "system":
			// system 角色转换为系统提示消息。
			out = append(out, blades.SystemMessage(m.Content))
		default:
			// 兜底分支：未知角色按 user 消息处理，这样模型至少能收到内容，而不是被静默丢弃。
			out = append(out, blades.UserMessage(fmt.Sprintf("[%s] %s", m.Role, m.Content)))
		}
	}
	// 返回转换后的 blades 消息切片。
	return out
}

// AssistantMessageFromBlades 将 blades 返回的 assistant 消息转换为我们的 ReactMessage。
// 该函数会把纯文本与工具调用分离到不同字段中。
func AssistantMessageFromBlades(m *blades.Message) ReactMessage {
	// 如果输入为空，直接返回一个默认的 assistant 角色消息。
	if m == nil {
		return ReactMessage{Role: "assistant"}
	}
	// 初始化结果消息，角色固定为 assistant。
	msg := ReactMessage{Role: "assistant"}
	// 遍历消息的所有 Part，根据类型分别提取文本或工具调用。
	for _, part := range m.Parts {
		// 通过类型断言区分文本部分和工具调用部分。
		switch v := part.(type) {
		case blades.TextPart:
			// 如果已经存在文本内容，先用换行分隔，再追加新的文本。
			if msg.Content != "" {
				msg.Content += "\n"
			}
			msg.Content += v.Text
		case blades.ToolPart:
			// 将工具调用的 Request JSON 反序列化为 map。
			// 忽略反序列化错误，若失败则 input 为 nil，后续调用方可以处理。
			var input map[string]any
			_ = json.Unmarshal([]byte(v.Request), &input)
			// 把解析后的工具调用追加到结果中。
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:    v.ID,
				Name:  v.Name,
				Input: input,
			})
		}
	}
	// 返回组装好的 ReactMessage。
	return msg
}

// ToolResultJSON 将 ToolResult 序列化为 JSON 字符串，用于填充 tool 消息的内容。
func ToolResultJSON(r ToolResult) string {
	// 把结果结构体序列化为 JSON，忽略错误以简化接口。
	b, _ := json.Marshal(r)
	// 返回 JSON 字符串。
	return string(b)
}

// agentIDKey 是用于在 context 中携带当前代理 ID 的键类型。
// 工具处理器（例如 call_sub_agent）需要通过它识别父代理。
type agentIDKey struct{}

// WithAgentID 返回一个携带当前代理 ID 的 context。
// ctx 是基础上下文；agentID 是要携带的代理标识。
func WithAgentID(ctx context.Context, agentID string) context.Context {
	// 使用私有类型作为键，避免与其他包使用字符串键发生冲突。
	return context.WithValue(ctx, agentIDKey{}, agentID)
}

// AgentIDFromContext 从 ctx 中取出存储的代理 ID。
// 如果不存在则返回空字符串。
func AgentIDFromContext(ctx context.Context) string {
	// 尝试从 context 中读取并类型断言为字符串。
	if v, ok := ctx.Value(agentIDKey{}).(string); ok {
		// 类型断言成功，返回代理 ID。
		return v
	}
	// context 中没有值或类型不匹配，返回空字符串。
	return ""
}
