package tool

// search_knowledge.go 实现 外部知识库检索工具（热路径 a）：
// meta/domain Agent 显式按需检索外部预置知识（与 ReadFile 读 wiki 页互补）。

import (
	"context"
	"fmt"
	"strings"
)

// KnowledgeSearchHookFunc 是 search_knowledge 工具的会话层回调：
// 对外部知识库执行混合检索（向量 + 关键词 RRF），返回格式化结果文本。
type KnowledgeSearchHookFunc func(ctx context.Context, query string, topK int) (string, error)

// searchKnowledgeTool 实现 search_knowledge 工具。
type searchKnowledgeTool struct {
	hook KnowledgeSearchHookFunc
}

// SetKnowledgeSearchHook 注入外部知识库检索回调。
// bootstrap 接线到 retriever.SearchHybrid + 结果格式化；nil 时工具返回未配置错误。
func (r *Registry) SetKnowledgeSearchHook(h KnowledgeSearchHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["search_knowledge"].(*searchKnowledgeTool); ok {
		t.hook = h
	}
}

// Name 返回工具名称。
func (t *searchKnowledgeTool) Name() string { return "search_knowledge" }

// Aliases 返回工具别名列表，当前无别名。
func (t *searchKnowledgeTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *searchKnowledgeTool) Description() string {
	return "检索外部知识库（预置领域文档，只读）：按查询文本做向量+关键词混合检索，返回命中的知识片段。" +
		"适用场景：任务涉及项目 wiki / 领域规范 / 技术文档等预置知识时，先检索再行动，避免凭记忆臆测。" +
		"参数 query 为检索问句或关键词（<=200 字）；top_k 可选（默认 3）。" +
		"返回每条命中片段（含来源文件）；未命中返回空。"
}

// Execute 执行 search_knowledge 工具调用。
func (t *searchKnowledgeTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "search_knowledge", Error: "外部知识库未接线（服务未注入 KnowledgeSearchHook）"}
	}
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return &Result{Tool: "search_knowledge", Error: "query is required"}
	}
	topK, _ := args["top_k"].(float64)
	k := int(topK)
	if k <= 0 {
		k = 3
	}
	results, err := t.hook(ctx, strings.TrimSpace(query), k)
	if err != nil {
		return &Result{Tool: "search_knowledge", Error: fmt.Sprintf("检索失败: %v", err)}
	}
	return &Result{Tool: "search_knowledge", Success: true, Output: results}
}
