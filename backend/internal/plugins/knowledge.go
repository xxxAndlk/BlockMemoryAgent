package plugins

import (
	"context"
)

// Chunk 表示一段检索到的知识片段。
type Chunk struct {
	Source  string  // 来源标识（如 wiki 页面 ID / 文档路径）
	Content string  // 片段文本
	Score   float64 // 相关性分数 [0,1]
}

// KnowledgeSource 外部知识源抽象（P3-5）。
// 后续 RAG / LLM Wiki / 企业知识库等能力各自实现本接口，
// 通过统一的 GlobalKnowledgeRetriever 接入记忆注入流程。
type KnowledgeSource interface {
	// Name 返回知识源名称，用于日志与可观测性。
	Name() string
	// Retrieve 根据查询返回最相关的知识片段；无命中时返回空切片。
	Retrieve(ctx context.Context, query string, topK int) ([]Chunk, error)
}
