// Package eventkind 定义 server 包中 SessionEvent.Type 与 SessionEvent.Kind 使用的事件类型与种类常量。
package eventkind

const (
	Think        = "think"         // 思考/推理事件
	Prompt       = "prompt"        // Prompt 事件
	TokenUsage   = "token_usage"   // Token 消耗统计事件
	GraphStep    = "graph_step"    // Graph 执行步骤事件
	ToolExec     = "tool_exec"     // 工具执行事件
	ToolCall     = "tool_call"     // 工具调用事件
	MemoryRecall = "memory_recall" // 记忆召回事件
	TopicSwitch  = "topic_switch"  // 话题切换事件
	LLMResult    = "llm_result"    // LLM 结果事件
	AgentDone    = "agent_done"    // Agent 完成事件
	System       = "system"        // 系统事件
	Error        = "error"         // 错误事件
	Intend       = "intend"        // 意图事件
	ToolResult   = "tool_result"   // 工具结果事件
	Wait         = "wait"          // 等待事件
	AgentCreated = "agent_created" // Agent 创建事件

	Progress      = "progress"       // 进度事件
	AssistantText = "assistant_text" // 工具调用之间的助手正文（口播）事件
	Clarify       = "clarify"        // 需要澄清事件
	ClarifyDetail = "clarify_detail" // 澄清附加长上下文事件（任务 140：先于问题展示的 detail 块）
	Stats         = "stats"          // 统计事件
	LLM           = "llm"            // LLM 通用事件
	UserMessage   = "user_message"   // 用户消息事件
	Message       = "message"        // 普通消息事件
	Interrupt     = "interrupt"      // 中断事件
	Enqueue       = "enqueue"        // 入队事件

	// 以下为历史字面量收编（2026-10-16 工作流量产：新 kind 入库时顺带统一；值不变）。
	SubAgentDispatch = "sub_agent_dispatch" // 子 Agent 派发事件（对话栏子 Agent 列表）
	SubAgentDone     = "sub_agent_done"     // 子 Agent 完成事件
	PeerAsk          = "peer_ask"           // 跨 Agent 提问（收到询问）事件
	Milestone        = "milestone"          // 子 Agent 中途里程碑播报
	WorkflowUnit     = "workflow_unit"      // 工作流量产单元状态迁移事件（2026-10-16）
)
