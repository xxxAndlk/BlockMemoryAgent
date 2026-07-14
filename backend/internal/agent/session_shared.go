// Package agent 提供会话事件的内部表示与事件流的辅助处理函数
// （如调试事件裁剪、UTF-8 清理），供旧版会话存储与 ReAct 会话存储共用。
package agent

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// internalEvent 是会话事件在运行时的内部表示。
// 它保留了原先 server.SessionEvent 的结构，
// 同时被旧版会话存储和 ReAct 会话存储共用，
// 用于在内存中统一传递事件数据。
type internalEvent struct {
	Type         string    `json:"type"`                    // Type 表示事件类型，例如消息、工具调用等
	Agent        string    `json:"agent"`                   // Agent 表示产生该事件的 Agent 标识
	Message      string    `json:"message"`                 // Message 是事件的主要文本内容
	Kind         string    `json:"kind,omitempty"`          // Kind 是事件的细分类别，如 think/prompt 等调试类别
	Tool         string    `json:"tool,omitempty"`          // Tool 表示被调用的工具名称
	ToolPath     string    `json:"tool_path,omitempty"`     // ToolPath 表示工具在配置中的路径
	ToolOutput   string    `json:"tool_output,omitempty"`   // ToolOutput 是工具执行返回的输出内容
	ToolError    string    `json:"tool_error,omitempty"`    // ToolError 是工具执行时产生的错误信息
	Success      bool      `json:"success,omitempty"`       // Success 标记工具调用是否成功
	Timestamp    time.Time `json:"timestamp"`               // Timestamp 记录事件产生的时间点
	Prompt       string    `json:"prompt,omitempty"`        // Prompt 保存发送给模型的提示词内容
	InputTokens  int       `json:"input_tokens,omitempty"`  // InputTokens 是输入 token 数量，用于统计成本
	OutputTokens int       `json:"output_tokens,omitempty"` // OutputTokens 是输出 token 数量，用于统计成本
	DetailJSON   string    `json:"detail_json,omitempty"`   // DetailJSON 用于保存额外的结构化详情
}

// toAgentEvent 将内部使用的 internalEvent 转换为对外暴露的 Event DTO。
// 参数 e 是内部事件指针；如果 e 为空指针，直接返回 nil，避免空指针解引用。
func toAgentEvent(e *internalEvent) *Event {
	// 入参校验：空指针没有可转换的数据，返回 nil
	if e == nil {
		return nil
	}
	// 逐个字段拷贝，保持数据语义不变
	return &Event{
		Type:         e.Type,
		Agent:        e.Agent,
		Message:      e.Message,
		Kind:         e.Kind,
		Tool:         e.Tool,
		ToolPath:     e.ToolPath,
		ToolOutput:   e.ToolOutput,
		ToolError:    e.ToolError,
		Success:      e.Success,
		Timestamp:    e.Timestamp,
		Prompt:       e.Prompt,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		DetailJSON:   e.DetailJSON,
	}
}

// trimDebugEvents 从事件流头部裁剪最多 maxDrop 个调试级别事件，
// 调试级别包括 think、prompt、token usage 和 graph step。
// 这样可以防止调试事件无限累积导致内存和上下文膨胀。
// 参数 events 是原始事件切片；maxDrop 是最多允许删除的调试事件数量。
// 返回裁剪后的新切片，原切片不会被修改。
func trimDebugEvents(events []internalEvent, maxDrop int) []internalEvent {
	// dropped 记录本次已经删除的调试事件数量
	dropped := 0
	// out 预分配与输入相同容量的新切片，避免后续 append 频繁扩容
	out := make([]internalEvent, 0, len(events))
	// 遍历输入事件切片，按顺序判断是否丢弃
	for _, ev := range events {
		// debugKind 标记当前事件是否属于调试级别事件
		debugKind := ev.Kind == eventkind.Think || ev.Kind == eventkind.Prompt || ev.Kind == eventkind.TokenUsage || ev.Kind == eventkind.GraphStep
		// 如果是调试事件且尚未达到删除上限，则跳过该事件
		if debugKind && dropped < maxDrop {
			dropped++ // 删除计数加一
			continue  // 不加入结果切片，实现头部裁剪效果
		}
		// 非调试事件或已达到删除上限，保留到结果切片中
		out = append(out, ev)
	}
	// 返回裁剪后的新切片
	return out
}

// sanitizeUTF8 清理字符串中的非法 UTF-8 序列。
// 如果字符串已经是合法 UTF-8，则直接返回原字符串以节省拷贝。
// 否则使用 Unicode 替换字符 "�" 替换非法字节序列。
func sanitizeUTF8(s string) string {
	// 快速路径：字符串合法则直接返回
	if utf8.ValidString(s) {
		return s
	}
	// 慢速路径：替换非法字节为 Unicode 替换字符
	return strings.ToValidUTF8(s, "�")
}
