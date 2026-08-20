// Package agent 定义 ReActAgent 运行时使用的基础类型、接口与消息转换函数，
// 使 agent 包对外提供稳定的小型 DTO。
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/model"
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
	// ReasoningContent 是思考模型（DeepSeek V4 reasoner 等）返回的推理过程文本。
	// DeepSeek V4 API 要求后续请求把 reasoning_content 回传到对应 assistant 消息，
	// 否则 400 "reasoning_content must be passed back"。
	// R1 等"不回传"模型在序列化时由 provider 决定是否携带。
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Images 仅 role="tool" 使用：工具结果携带的图片（ui_preview 截图等）。
	// 仅内存透传（json:"-" 不持久化、不计入 token 估算）：ToBladesMessages 只为
	// 历史末尾最新一轮工具结果挂载图片，旧轮次只留 Content 里的文本占位符，
	// 防 base64 反复进上下文烧毁前缀缓存（与 tool.Result.Images 同语义）。
	Images []tool.ResultImage `json:"-"`
}

// ToolCall 表示模型请求执行某个命名工具的调用。
// 输入参数以 JSON 形状的 map 形式承载。
type ToolCall struct {
	ID    string         `json:"id"`    // ID 是本次工具调用的唯一标识，用于结果回传时匹配
	Name  string         `json:"name"`  // Name 是要调用的工具名称
	Input map[string]any `json:"input"` // Input 是传递给工具的参数键值对
}

// EstimateMessagesTokens 粗估消息切片占用的 token 数：逐条累加 Content + ReasoningContent
// + 各 ToolCall 的 JSON 体积估算。注入 Pipeline 作用上下文阈值触发压缩（避免 domain/memory
// 反向依赖 model 包），亦供 ReActAgent 在 Assemble 后判定是否触达上下文预算上限。
// 估算仅字符串扫描，成本低；不追求与 provider tokenizer 对齐，偏高中均可由步频兜底吸收。
func EstimateMessagesTokens(msgs []ReactMessage) int {
	var total int
	for _, m := range msgs {
		total += model.EstimateTokens(m.Content)
		total += model.EstimateTokens(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			// Input 是 map[string]any，序列化后按字符估算；序列化失败按 Name+ID 文本估。
			if raw, err := json.Marshal(tc.Input); err == nil {
				total += model.EstimateTokens(string(raw))
			} else {
				total += model.EstimateTokens(tc.Name) + model.EstimateTokens(tc.ID)
			}
			total += model.EstimateTokens(tc.Name) + model.EstimateTokens(tc.ID)
		}
		total += model.EstimateTokens(m.ToolCallID)
	}
	return total
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

// FilesModifiedFromHistory 从 ReAct 历史中收集所有成功执行之外不可知，仅按
// assistant 消息携带的 WriteFile/EditFile 工具调用入参 path 提取，返回去重保序的路径切片。
// 用于 Layer 5：子 Agent 完成后把修改文件清单挂到 mailbox.Message.FilesModified，
// 父 Agent drainMailbox 时展示。空 history 或无写类调用时返回 nil。
// 注意：仅扫 assistant 请求的 WriteFile/EditFile，未校验对应 tool 结果是否 Success（历史中
// 失败的写调用较少且路径信息对父 Agent 仍有参考价值，保守纳入）。
func FilesModifiedFromHistory(history []ReactMessage) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "WriteFile" && tc.Name != "EditFile" {
				continue
			}
			p, _ := tc.Input["path"].(string)
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			cleaned := filepath.Clean(p)
			if seen[cleaned] {
				continue
			}
			seen[cleaned] = true
			out = append(out, cleaned)
		}
	}
	return out
}

// ToolResult 表示执行一次 ToolCall 后的结果。
type ToolResult struct {
	Tool    string `json:"tool"`            // Tool 是产生该结果的工具名称
	Success bool   `json:"success"`         // Success 标记工具执行是否成功
	Output  string `json:"output"`          // Output 是工具执行成功时的输出内容
	Error   string `json:"error,omitempty"` // Error 是工具执行失败时的错误信息
	// Images 是工具结果携带的图片（仅 image_passthrough 插件，如 ui_preview 截图）。
	// 仅内存透传：不持久化、不进 token 估算；见 tool.Result.Images。
	Images []tool.ResultImage `json:"-"`
}

// ReactResult 是 ReActAgent 一次运行结束时返回的结果。
type ReactResult struct {
	Text    string         `json:"text"`    // Text 是模型最终生成的文本答案
	History []ReactMessage `json:"history"` // History 是本次完整对话历史，包含所有轮次
	// LimitReached 为 true 表示达到最大轮数上限而暂停（非错误）：
	// Text 为空，History 保留全部进度，上层应暂停会话并等待用户消息续跑。
	LimitReached bool `json:"limit_reached,omitempty"`
	// PausedOnChild 为 true 表示因有子 DomainAgent 触达 token 上限进入 Paused 而暂停。
	// 与 LimitReached 互斥语义:LimitReached=自身到限,PausedOnChild=子到限。
	// 上层据此将会话置 PausedOnChild 态,等用户"继续"恢复该 domain。
	PausedOnChild bool `json:"paused_on_child,omitempty"`
	// Unverified 为 true 表示校验层判定"结论未验证"（TODO #43 fail-closed）：
	// judge LLM 不可用/坏 JSON 时置位，上层不得当成功交付，须显式上抛父 Agent 自决
	// （替代旧 fail-open 静默放行——2026-08-13 实证全天 reflection 形同虚设）。
	Unverified bool `json:"unverified,omitempty"`
	// VerifyNote 是校验层状态标签：非空=校验通过（"L0 证据"/"L2 rubric"）；
	// Unverified 时为 judge 失败原因。上层据此在完成摘要里标注校验状态。
	VerifyNote string `json:"verify_note,omitempty"`
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
	// LiveEventTokenUsage 是单次 LLM 调用 token 用量事件，
	// InputTokens/OutputTokens 字段携带用量；provider 未返回用量时不发射。
	LiveEventTokenUsage = "token_usage"
)

// LiveEvent 是 ReAct 运行过程中的实时进度事件，
// 由 WithLiveEvents 注册的回调接收，供会话层写入事件流/流式状态以驱动 UI 实时渲染。
type LiveEvent struct {
	Kind    string // Kind 事件种类（LiveEventLLMDelta / LiveEventToolCall / LiveEventToolExec）
	Agent   string // Agent 产生事件的展示名（emit 时自动填充 role.Name）
	AgentID string // AgentID 产生事件的 Agent 实例 ID（emit 时自动填充；MetaAgent=session ID，子 Agent=session-1/code_assistant-5）
	Text    string // Text 仅 llm_delta 使用：截至当前的累积文本
	Tool    string // Tool 工具名（tool_call / tool_exec 使用）
	Input   string // Input 工具入参 JSON（tool_call 使用）
	Output  string // Output 工具输出（tool_exec 使用）
	Error   string // Error 工具错误信息（tool_exec 使用）
	Success bool   // Success 工具是否执行成功（tool_exec 使用）
	// InputTokens/OutputTokens 仅 token_usage 使用：本次 LLM 调用的输入/输出 token 数。
	InputTokens  int64
	OutputTokens int64
	// CacheHitTokens/CacheMissTokens 仅 token_usage 使用（TODO #40 可观测）：
	// 本次调用的缓存命中/未命中 token 数（provider 未返回时为零）。
	CacheHitTokens  int64
	CacheMissTokens int64
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

// PausedChildChecker 抽象"父 Agent 是否有 Paused 子 DomainAgent"的查询能力,
// 由 subagent.Dispatcher 实现。ReActAgent 在父终结保护 wait loop 中调用它检查:
// 若有 Paused 子 Agent(触达 token 上限),父 MetaAgent 无限 budget 不会自行暂停,
// 需靠此检查跳出 wait loop 返回 PausedOnChild,由上层 pauseSession 置会话暂停态。
type PausedChildChecker interface {
	// HasPausedChild 返回父 Agent 是否有 StatusPaused 的子节点。
	HasPausedChild(parentID string) bool
}

// PausedDomainResumer 抽象"恢复一个 Paused DomainAgent 续跑"的能力,
// 由 subagent.Dispatcher 实现。ReactService 在 sendMessage 检测会话处于 PausedOnChild
// 时调用,从 agent_messages 加载历史重建 domain Agent 用 fresh budget 续跑(各 Agent 独立上下文)。
type PausedDomainResumer interface {
	// ResumePaused 恢复指定 Paused 节点的 DomainAgent。
	// 返回 result.LimitReached=true 表示再触限(已 re-pause);err 非 nil 表示恢复出错。
	ResumePaused(ctx context.Context, pausedNodeID string) (ReactResult, error)
}

// DispatchCountResetter 由 subagent.Dispatcher 实现：清空指定 session 的派发计数。
// ReactService 在新用户消息到达时调用，使全局派发限额按任务粒度重置。
type DispatchCountResetter interface {
	ResetDispatchCounts(sessionID string)
}

// SoftStopMarker 由 subagent.Dispatcher 实现（TODO #37）：会话软停止标记。
// ReactService.Stop 先标记再触发子 Agent cancel，dispatcher 的 context.Canceled 收尾
// 分支据此刻意落 Paused（domain，可续跑）/部分回灌（叶子），而非静默跳过；
// 续跑触发时清除标记。
type SoftStopMarker interface {
	SetSoftStop(sessionID string)
	ClearSoftStop(sessionID string)
}

// SuspendGate 抽象"会话级挂起检查点"能力，由 subagent.Dispatcher 侧实现。
// ReActAgent 在主循环顶部与 waitForChildren 内调用 Park：会话挂起期间阻塞
// （goroutine 真挂起，内存态保留），恢复返回 nil 继续；销毁/取消返回非 nil error。
// nil gate = 完全零变化（热驻关闭时所有存量路径不注入）。
type SuspendGate interface {
	// Park 在 Agent 处于挂起态时阻塞；恢复返回 nil，销毁/取消返回非 nil error。
	Park(ctx context.Context) error
}

// IdleDomainInfo 描述一个热驻 Idle DomainAgent 的可复用信息，
// 供 MetaAgent 上下文注入（复用判定）。由 subagent.Dispatcher 的 IdleRoster 填充。
type IdleDomainInfo struct {
	AgentID     string        `json:"agent_id"`     // AgentID 热驻 domain 的树节点 ID（reuse_agent_id 参数值）
	Domain      string        `json:"domain"`       // Domain 领域标签
	LastTask    string        `json:"last_task"`    // LastTask 最近一次任务摘要（截断）
	LastSummary string        `json:"last_summary"` // LastSummary 最近一次结果摘要（截断）
	ReuseCount  int           `json:"reuse_count"`  // ReuseCount 已被复用次数（权重）
	IdleLeft    time.Duration `json:"idle_left"`    // IdleLeft 加权倒计时剩余（未武装为 0）
	Busy        bool          `json:"busy"`         // Busy 正在执行任务（派发将入队）
}

// IdleRosterProvider 抽象"查询某 session 的热驻 Idle DomainAgent 清单"，
// 由 subagent.Dispatcher 实现。ReactService 组装 MetaAgent 记忆管线时轮询注入
// 【空闲领域Agent】上下文段，MetaAgent 据此自主判定强相关复用 vs 弱相关新建。
type IdleRosterProvider interface {
	IdleRoster(sessionID string) []IdleDomainInfo
}

// IdleTTLArmer 由 subagent.Dispatcher 实现：用户下一条消息到达时武装全部
// Idle domain 的加权销毁倒计时（完成后一直热存，TTL 只在新用户消息后才启动）。
type IdleTTLArmer interface {
	ArmIdleTTLs(sessionID string)
}

// SessionAgentWaker 由 subagent.Dispatcher 实现（Domain 热驻）：唤醒会话全部挂起
// Agent（触限暂停波及全树后的恢复入口）。ReactService.sendMessage 在非 Running 态
// 恢复时调用：wake 广播 + Paused 树节点置回 Running + 恢复冻结的 idle TTL。
// 为 nil 时走旧路径（逐个 resumePausedDomain）。
type SessionAgentWaker interface {
	ResumeSessionAgents(sessionID string)
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
	// 预先建立 tool_call_id -> ToolCall 索引，供后续 tool 结果消息回填 Name + Request。
	// blades contrib openai v0.3.0 的 toToolCallMessage 期望 RoleTool 消息的 ToolPart
	// 携带全部字段（ID/Name/Request/Response）；若 Name 与 Request 为空，
	// ChatCompletionMessageFunctionToolCallFunctionParam 为零值，被 omitzero 标签省略，
	// 序列化出的 tool_call 对象缺 `function` 字段，DeepSeek/OpenAI 严格校验会回 400
	// "missing field `function`"。回填 Name + Request 后即与 blades 协议契合。
	toolCallsByID := make(map[string]ToolCall, len(history))
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			toolCallsByID[tc.ID] = tc
		}
	}

	// 预分配与 history 长度相同的容量，减少 append 过程中的内存分配。
	out := make([]*blades.Message, 0, len(history))
	// 图片透传边界：定位最新一批 tool 结果（从尾部越过 mailbox 注入的 user 尾巴，
	// 取最后一个连续 tool 消息块）。仅为该批次挂载图片，更早轮次不挂——
	// 其 Content 文本占位符仍在，base64 不反复进上下文烧毁前缀缓存。
	imgBatchEnd := len(history)
	for imgBatchEnd > 0 && history[imgBatchEnd-1].Role != "tool" {
		imgBatchEnd--
	}
	imgBatchStart := imgBatchEnd
	for imgBatchStart > 0 && history[imgBatchStart-1].Role == "tool" {
		imgBatchStart--
	}
	// 遍历每一轮对话消息，根据角色转换为 blades 对应的消息类型。
	for i, m := range history {
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
			// 回传思考模型的推理过程：DeepSeek V4 reasoner 等要求 assistant 消息携带
			// reasoning_content 字段，否则 400。通过 Metadata 传递，由 openai-chat
			// provider 序列化到请求 JSON；其他 provider 忽略。
			if m.ReasoningContent != "" {
				if msg.Metadata == nil {
					msg.Metadata = make(map[string]any)
				}
				msg.Metadata["reasoning_content"] = m.ReasoningContent
			}
			// 将组装好的 assistant 消息追加到结果切片。
			out = append(out, msg)
		case "tool":
			// tool 角色表示工具执行结果。blades contrib 期望 RoleTool 消息携带
			// 完整 ToolPart（ID/Name/Request/Response），由 toToolCallMessage 抽取
			// Name+Request 构造 assistant tool_call，再由 ToolMessage 抽取 Response
			// 构造 tool 结果。回填 Name + Request 避免 function 字段缺失。
			part := blades.ToolPart{ID: m.ToolCallID, Response: m.Content}
			if tc, ok := toolCallsByID[m.ToolCallID]; ok {
				part.Name = tc.Name
				req, _ := json.Marshal(tc.Input)
				part.Request = string(req)
			}
			parts := []blades.Part{part}
			// 最新一批工具结果挂载图片：DataPart 承载原始字节（base64 解码失败跳过，
			// Response 里的文本占位符仍保留痕迹）；是否序列化进请求由 provider 决定
			//（anthropic 转 image block；openai 系忽略 DataPart，天然降级）。
			if i >= imgBatchStart && i < imgBatchEnd {
				for _, img := range m.Images {
					raw, err := base64.StdEncoding.DecodeString(string(img.Data))
					if err != nil || len(raw) == 0 || img.MIMEType == "" {
						continue
					}
					parts = append(parts, blades.DataPart{MIMEType: blades.MIMEType(img.MIMEType), Bytes: raw})
				}
			}
			out = append(out, &blades.Message{
				Role:  blades.RoleTool,
				Parts: parts,
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
			// 解析失败（半截/损坏 JSON，如 max_tokens 截断后端点补齐的假合法参数）
			// 时丢弃该调用：以 nil 入参执行工具会静默写坏文件（如 WriteFile 空写）。
			var input map[string]any
			if err := json.Unmarshal([]byte(v.Request), &input); err != nil {
				continue
			}
			// 把解析后的工具调用追加到结果中。
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:    v.ID,
				Name:  v.Name,
				Input: input,
			})
		}
	}
	// 返回组装好的 ReactMessage。
	// 提取思考模型的推理过程（DeepSeek V4 reasoner 等），存入 ReasoningContent，
	// 供下一轮请求经 ToBladesMessages 回传给 API。
	if m.Metadata != nil {
		if v, ok := m.Metadata["reasoning_content"].(string); ok {
			msg.ReasoningContent = v
		}
	}
	return msg
}

// ToolResultJSON 将 ToolResult 序列化为 JSON 字符串，用于填充 tool 消息的内容。
func ToolResultJSON(r ToolResult) string {
	// 把结果结构体序列化为 JSON，忽略错误以简化接口。
	b, _ := json.Marshal(r)
	// 返回 JSON 字符串。
	return string(b)
}

// WithAgentID 返回一个携带当前代理 ID 的 context。
// 委托给 tool.WithAgentID，使 tool 包的 WriteSharedMemory 等工具能读到同一值；
// 保留 agent 包侧导出仅为不破坏既有调用方（dispatcher_test.go 等）。
func WithAgentID(ctx context.Context, agentID string) context.Context {
	return tool.WithAgentID(ctx, agentID)
}

// AgentIDFromContext 从 ctx 中取出存储的代理 ID。
// 委托给 tool.AgentIDFromContext，与 WithAgentID 共享同一 key 类型。
func AgentIDFromContext(ctx context.Context) string {
	return tool.AgentIDFromContext(ctx)
}

// WithRoleID 返回一个携带当前角色 ID 的 context。
// 委托给 tool.WithRoleID，供 WriteFile 的角色级写沙箱（Layer 4）读取。
func WithRoleID(ctx context.Context, roleID string) context.Context {
	return tool.WithRoleID(ctx, roleID)
}

// agentDisplayNameKey 用于在 context 中携带代理展示名（"MetaAgent" / "代码助手" / "领域Agent:xxx"）。
// 与 agentIDKey 解耦：agentIDKey 走 mailbox 路由，agentDisplayNameKey 专供日志与 UI 展示。
type agentDisplayNameKey struct{}

// WithAgentDisplayName 返回一个携带代理展示名的 context。
// displayName 为空时直接返回原 ctx，避免覆盖既有展示名。
func WithAgentDisplayName(ctx context.Context, displayName string) context.Context {
	if displayName == "" {
		return ctx
	}
	return context.WithValue(ctx, agentDisplayNameKey{}, displayName)
}

// AgentDisplayNameFromContext 从 ctx 中取出代理展示名。
// 不存在时返回空串，调用方应回退到 AgentIDFromContext。
func AgentDisplayNameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(agentDisplayNameKey{}).(string); ok {
		return v
	}
	return ""
}
