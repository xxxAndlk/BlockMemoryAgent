// Package retriever 提供全局知识库检索能力。
// 本文件定义了面向全局知识库的检索器，依赖三个抽象接口：
// VectorDB（向量数据库）、Embedder（文本向量化）、MetaStore（元数据存储）。
// 通过接口抽象解耦底层实现，便于切换 pgvector / Redis / 其他存储后端。
package retriever

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// VectorDB 向量数据库抽象接口。
// 仅暴露语义检索能力：根据查询向量召回 Top-K 条知识记录。
// 由具体实现（如 pgvector 封装）注入，便于替换底层向量存储。
type VectorDB interface {
	// Search 基于查询向量 embedding 召回 topK 条最相似的知识记录。
	Search(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
}

// Embedder 文本嵌入接口。
// 将自然语言查询转换为向量，作为向量检索的输入。
// 抽象出来便于切换不同 embedding 模型（OpenAI / 本地模型等）。
type Embedder interface {
	// Embed 把单条文本 text 编码为 float32 向量。
	Embed(ctx context.Context, text string) ([]float32, error)
}

// MetaStore 元数据存储接口。
// 提供基于知识类型的查询与访问计数自增能力，
// 用于支撑按类型检索与访问热度统计（影响后续排序/衰减权重）。
type MetaStore interface {
	// GetKnowledgeByType 按知识类型 knowledgeType 召回至多 limit 条记录。
	GetKnowledgeByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error)
	// IncrementAccessCount 将指定 id 的知识记录访问计数加一。
	IncrementAccessCount(ctx context.Context, id int64) error
}

// GlobalKnowledgeRetriever 全局知识检索器。
// 组合 VectorDB / MetaStore / Embedder 三个抽象，
// 负责跨域共享知识的语义检索与按类型检索，
// 并在检索命中时更新访问计数以支撑时间衰减与热度排序。
type GlobalKnowledgeRetriever struct {
	vectorDB VectorDB  // 向量数据库，用于语义召回
	metaDB   MetaStore // 元数据存储，用于按类型查询与访问计数
	embedder Embedder  // 文本向量化器，将查询转为向量
}

// NewGlobalKnowledgeRetriever 创建全局知识检索器。
// 职责：将三个抽象依赖注入并组装为可用的检索器实例。
// 参数：
//   - vectorDB: 向量数据库实现，提供 Top-K 语义召回。
//   - metaDB: 元数据存储实现，提供按类型查询与访问计数自增。
//   - embedder: 文本嵌入实现，将查询字符串转为向量。
//
// 返回：组装完成的 *GlobalKnowledgeRetriever 指针。
// 副作用：无；依赖的具体实现若未就绪需由调用方保证可用性。
func NewGlobalKnowledgeRetriever(vectorDB VectorDB, metaDB MetaStore, embedder Embedder) *GlobalKnowledgeRetriever {
	// 将传入依赖绑定到结构体字段，返回组装好的检索器实例。
	return &GlobalKnowledgeRetriever{
		vectorDB: vectorDB,
		metaDB:   metaDB,
		embedder: embedder,
	}
}

// Retrieve 基于自然语言查询进行全局知识语义检索。
// 职责：将查询向量化 → 向量库召回 Top-K → 命中记录访问计数自增。
// 参数：
//   - ctx: 上下文，用于控制超时与取消。
//   - query: 自然语言查询字符串。
//   - topK: 期望召回的条数；若 <=0 则使用默认值 5。
//
// 返回：命中的知识记录切片，或在向量化/检索失败时返回 error。
// 副作用：对每条命中记录调用 IncrementAccessCount 更新访问计数，
//
//	计数失败被静默忽略，不影响主流程返回。
func (r *GlobalKnowledgeRetriever) Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error) {
	// topK 非正数时回退为默认召回数量 5，避免无效参数导致空召回。
	if topK <= 0 {
		topK = 5
	}

	// 1. 向量化查询：把自然语言 query 编码为向量，作为语义检索输入。
	embedding, err := r.embedder.Embed(ctx, query)
	if err != nil {
		// 向量化失败直接返回，包装错误以标注出错阶段。
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// 2. 向量检索：用 embedding 在向量库中召回 Top-K 条相似记录。
	results, err := r.vectorDB.Search(ctx, embedding, topK)
	if err != nil {
		// 向量检索失败直接返回，包装错误以标注出错阶段。
		return nil, fmt.Errorf("vector search: %w", err)
	}

	// 3. 更新访问计数：对每条命中记录累加访问次数，
	//    用于后续时间衰减权重计算与热度排序。
	//    计数失败不影响检索结果返回，故忽略错误。
	for _, rec := range results {
		_ = r.metaDB.IncrementAccessCount(ctx, rec.ID)
	}

	// 返回召回结果，调用方可直接使用。
	return results, nil
}

// RetrieveByType 按知识类型检索全局知识。
// 职责：直接从元数据存储按类型召回，不走向量检索路径，
//
//	适用于需要枚举某一类知识（如某领域全量条目）的场景。
//
// 参数：
//   - ctx: 上下文，用于控制超时与取消。
//   - knowledgeType: 知识类型标识，由上层业务定义。
//   - limit: 最大返回条数。
//
// 返回：该类型下的知识记录切片，或底层存储错误。
// 副作用：无（不更新访问计数）。
func (r *GlobalKnowledgeRetriever) RetrieveByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
	// 直接委托给元数据存储按类型查询并返回结果。
	return r.metaDB.GetKnowledgeByType(ctx, knowledgeType, limit)
}
