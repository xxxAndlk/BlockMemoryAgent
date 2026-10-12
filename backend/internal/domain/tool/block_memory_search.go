package tool

// block_memory_search.go 实现 search_block_memory 工具（顶层 Agent 的块记忆读侧）：
// 系统提示词尾部【沉淀索引】是块记忆 global_knowledge 的一行式目录，写着"详情按需向量召回"，
// 但顶层 Agent 此前没有任何工具能取回详情——本工具补上这个断档：按语义检索跨会话
// 块记忆（历史会话沉淀的事实/结果/工艺要点），返回按相关度排序的条目全文。
// hook 注入模式同 search_knowledge：注册始终发生使 Schema 可见，
// bootstrap 经 SetBlockMemorySearchHook 接线；未接线时 Execute 返回"未接线"错误。

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// searchBlockMemoryTool 实现 search_block_memory 工具。
type searchBlockMemoryTool struct {
	// hook 是块记忆语义检索回调（bootstrap 接 KnowledgeStore，返回格式化结果）；
	// nil 时工具返回未接线错误。
	hook func(ctx context.Context, query string) (string, error)
}

// Name 返回工具名称。
func (t *searchBlockMemoryTool) Name() string { return "search_block_memory" }

// Aliases 返回工具别名列表，当前无别名。
func (t *searchBlockMemoryTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *searchBlockMemoryTool) Description() string {
	return "按语义检索跨会话块记忆（历史会话沉淀的事实/结果/工艺要点）。" +
		"当系统提示词【沉淀索引】中出现与当前任务相关的条目、或你需要查证历史项目经验时调用。" +
		"返回按相关度排序的条目全文（截断）。"
}

// InputSchema 返回 LLM 可见的参数 schema。
func (t *searchBlockMemoryTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query": {
				Type:        "string",
				Description: "检索问句或关键词（语义检索，描述要查证的历史经验/事实）",
			},
		},
		Required: []string{"query"},
	}
}

// Execute 执行 search_block_memory 工具调用。
// 结果文本的格式化由 hook 侧完成，此处原样透传为 Output。
func (t *searchBlockMemoryTool) Execute(ctx context.Context, args map[string]any) *Result {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return &Result{Tool: "search_block_memory", Error: "query is required"}
	}
	if t.hook == nil {
		return &Result{Tool: "search_block_memory", Error: "块记忆检索未接线"}
	}
	out, err := t.hook(ctx, query)
	if err != nil {
		return &Result{Tool: "search_block_memory", Error: fmt.Sprintf("检索失败: %v", err)}
	}
	return &Result{Tool: "search_block_memory", Success: true, Output: out}
}
