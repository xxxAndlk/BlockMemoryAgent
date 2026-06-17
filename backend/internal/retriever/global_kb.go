package retriever

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// VectorDB 向量数据库接口
type VectorDB interface {
	Search(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
}

// Embedder 文本嵌入接口
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// MetaStore 元数据存储接口
type MetaStore interface {
	GetKnowledgeByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error)
	IncrementAccessCount(ctx context.Context, id int64) error
}

// GlobalKnowledgeRetriever 全局知识检索器
type GlobalKnowledgeRetriever struct {
	vectorDB VectorDB
	metaDB   MetaStore
	embedder Embedder
}

// NewGlobalKnowledgeRetriever 创建全局知识检索器
func NewGlobalKnowledgeRetriever(vectorDB VectorDB, metaDB MetaStore, embedder Embedder) *GlobalKnowledgeRetriever {
	return &GlobalKnowledgeRetriever{
		vectorDB: vectorDB,
		metaDB:   metaDB,
		embedder: embedder,
	}
}

// Retrieve 检索全局知识 (实现 Eino Retriever 接口风格)
func (r *GlobalKnowledgeRetriever) Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}

	// 1. 向量化查询
	embedding, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// 2. 向量检索
	results, err := r.vectorDB.Search(ctx, embedding, topK)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	// 3. 更新时间衰减权重并记录访问
	for _, rec := range results {
		_ = r.metaDB.IncrementAccessCount(ctx, rec.ID)
	}

	return results, nil
}

// RetrieveByType 按类型检索
func (r *GlobalKnowledgeRetriever) RetrieveByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
	return r.metaDB.GetKnowledgeByType(ctx, knowledgeType, limit)
}
