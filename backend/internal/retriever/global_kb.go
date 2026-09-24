// Package retriever 提供全局知识库检索能力。
// 本文件定义了面向全局知识库的检索器，核心依赖 Embedder（文本向量化）；
// 混合检索后端经 HybridSearchBackend 注入（hybrid.go）。
package retriever

import (
	"context" // 上下文，用于超时/取消控制
)

// Embedder 文本嵌入接口。
// 将自然语言查询转换为向量，作为向量检索的输入。
// 抽象出来便于切换不同 embedding 模型（OpenAI / 本地模型等）。
type Embedder interface {
	// Embed 把单条文本 text 编码为 float32 向量。
	Embed(ctx context.Context, text string) ([]float32, error)
}

// GlobalKnowledgeRetriever 全局知识检索器。
// 负责跨域共享知识的混合检索（SearchHybrid）与本地目录摄入（IngestDir）。
type GlobalKnowledgeRetriever struct {
	embedder Embedder             // 文本向量化器，将查询转为向量
	hybrid   HybridSearchBackend  // 混合检索后端（SearchHybrid 用）
}

// NewGlobalKnowledgeRetriever 创建全局知识检索器。
// 职责：注入向量化器；混合检索后端经 SetHybridBackend 注入，可为 nil。
func NewGlobalKnowledgeRetriever(embedder Embedder) *GlobalKnowledgeRetriever {
	return &GlobalKnowledgeRetriever{embedder: embedder}
}
