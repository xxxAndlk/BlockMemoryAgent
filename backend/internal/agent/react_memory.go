// Package agent 定义 ReAct 循环所需的记忆流水线接口与空实现，
// 负责在每次 LLM 调用前组装上下文、写入可观察事件。
package agent

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// MemoryEvent 用于记录 ReAct 循环中一次可观察到的步骤。
// 事件流是唯一的持久化记忆；在新架构中没有压缩、RAG 或向量检索。
type MemoryEvent struct {
	Type     string    `json:"type"`                // 事件类型，例如 thought / action / observation
	AgentID  string    `json:"agent_id"`            // 产生该事件的 Agent 唯一标识
	Role     string    `json:"role,omitempty"`      // 触发事件时 Agent 所扮演的角色
	Content  string    `json:"content,omitempty"`   // 通用内容，例如思考文本或原始消息
	ToolName string    `json:"tool_name,omitempty"` // 工具调用场景下的工具名称
	Input    string    `json:"input,omitempty"`     // 工具调用的输入参数或提示内容
	Output   string    `json:"output,omitempty"`    // 工具调用的输出结果或观察内容
	Occurred time.Time `json:"occurred"`            // 事件发生的时间戳
}

// MemoryPipeline 是 ReAct 循环读写记忆所需的精简接口。
// nil 的 pipeline 是合法的，表示该 Agent 在无记忆模式下运行。
type MemoryPipeline interface {
	// Assemble 在每次调用 LLM 之前，将上下文记忆注入到对话历史中。
	// 返回的切片会作为实际发送给模型的消息列表。
	Assemble(role types.RoleDefinition, agentID string, history []ReactMessage) []ReactMessage
	// Write 将一个新事件追加到该 Agent 的事件流中。
	Write(agentID string, event MemoryEvent) error
}

// NopMemoryPipeline 是一个空实现，用于关闭记忆功能时使用。
// 它直接返回原始历史记录，并丢弃所有写入事件。
type NopMemoryPipeline struct{}

// Assemble 直接返回传入的历史记录，不做任何记忆注入。
// 参数 role 为当前角色定义，agentID 为 Agent 标识，history 为当前对话历史。
// 返回与 history 相同的内容，保证 LLM 调用接口一致。
func (NopMemoryPipeline) Assemble(_ types.RoleDefinition, _ string, history []ReactMessage) []ReactMessage {
	return history
}

// Write 忽略传入的事件并始终返回 nil。
// 参数 _ 为 Agent 标识，_ 为待写入事件。
// 返回 nil 表示写入“成功”，实际未执行任何持久化操作。
func (NopMemoryPipeline) Write(_ string, _ MemoryEvent) error { return nil }
