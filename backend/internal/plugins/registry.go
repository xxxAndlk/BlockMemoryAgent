// Package plugins 预留插件扩展接口（P3-5）。
//
// 设计意图：先定义接口，不提前绑定具体实现，便于后续接入 MCP / 自定义工具 /
// ComputerUse / RAG / LLM Wiki 等能力，同时避免当前代码腐烂。
package plugins

import (
	"context" // 上下文，用于工具执行取消与超时
)

// Tool 是 Agent 可调用的工具抽象。
// 与 graph.ToolResult 解耦，使插件包不依赖 graph 包，避免循环依赖。
type Tool interface {
	// Name 返回工具名（CamelCase），与 Skill.tool_ref 对应。
	Name() string
	// Description 返回工具功能描述，用于生成 LLM function calling schema。
	Description() string
	// Execute 执行工具调用；input 为反序列化后的 JSON 对象。
	Execute(ctx context.Context, input map[string]any) (output string, err error)
}

// ToolRegistry 工具注册表抽象。
// 预留运行时从 config/tools.yaml 或 MCP server 加载自定义工具的扩展点。
type ToolRegistry interface {
	// Register 注册一个工具；同名覆盖。
	Register(tool Tool) error
	// Get 按名称获取工具；不存在时返回 nil,false。
	Get(name string) (Tool, bool)
	// List 返回全部已注册工具名。
	List() []string
}

// StaticToolRegistry 是 ToolRegistry 的内存实现，作为默认兜底。
type StaticToolRegistry struct {
	tools map[string]Tool // 工具名 → Tool 实例
}

// NewStaticToolRegistry 创建空注册表。
func NewStaticToolRegistry() *StaticToolRegistry {
	return &StaticToolRegistry{tools: make(map[string]Tool)}
}

// Register 实现 ToolRegistry。
func (r *StaticToolRegistry) Register(tool Tool) error {
	// nil 工具静默忽略，避免 map 中存入空值。
	if tool == nil {
		return nil
	}
	r.tools[tool.Name()] = tool
	return nil
}

// Get 实现 ToolRegistry。
func (r *StaticToolRegistry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List 实现 ToolRegistry。
func (r *StaticToolRegistry) List() []string {
	// 预分配容量，遍历 map 收集工具名。
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	return out
}
