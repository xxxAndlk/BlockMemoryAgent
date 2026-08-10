package retriever

// hybrid.go 实现 TODO #27 外部知识库检索层的热路径：
//   - SearchHybrid：向量 + 全文关键词 RRF 混合检索（external_kb 命名空间隔离）；
//   - IngestDir：本地 Markdown/文本目录批量摄入（切块 → embed → 落库）。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func logf(format string, args ...any) { log.Printf("[retriever] "+format, args...) }

// HybridSearchBackend 是混合检索的后端抽象（PG 实现：KnowledgeStore 直接满足）。
type HybridSearchBackend interface {
	// SearchByType 按类型过滤的向量相似检索（即 KnowledgeStore.SearchByType）。
	SearchByType(ctx context.Context, knowledgeType enums.KnowledgeType, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
	// SearchKeywords 按类型过滤的全文关键词检索（tsvector）。
	SearchKeywords(ctx context.Context, knowledgeType enums.KnowledgeType, query string, topK int) ([]*types.KnowledgeRecord, error)
}

// SetHybridBackend 注入混合检索后端（bootstrap 装配 PG 实现）。
// 未注入时 SearchHybrid 返回错误。
func (r *GlobalKnowledgeRetriever) SetHybridBackend(b HybridSearchBackend) {
	r.hybrid = b
}

// SearchHybrid 外部知识库混合检索：查询向量化 → 向量召回 + 关键词召回 → RRF 融合。
// 仅召回 knowledgeType（默认 external_kb）命名空间，与块记忆分层不串扰。
// 单路失败降级为另一路（容错）；两路都失败返回错误。
func (r *GlobalKnowledgeRetriever) SearchHybrid(ctx context.Context, query string, knowledgeType enums.KnowledgeType, topK int) ([]*types.KnowledgeRecord, error) {
	if r.hybrid == nil {
		return nil, fmt.Errorf("hybrid search backend not wired")
	}
	if knowledgeType == "" {
		knowledgeType = enums.KnowledgeTypeExternalKB
	}
	if topK <= 0 {
		topK = 5
	}

	embedding, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	ranks := make([][]*types.KnowledgeRecord, 0, 2)
	vectorHits, err := r.hybrid.SearchByType(ctx, knowledgeType, embedding, topK)
	if err != nil {
		logf("hybrid vector search failed: %v", err)
	} else {
		ranks = append(ranks, vectorHits)
	}
	keywordHits, err := r.hybrid.SearchKeywords(ctx, knowledgeType, query, topK)
	if err != nil {
		logf("hybrid keyword search failed: %v", err)
	} else {
		ranks = append(ranks, keywordHits)
	}
	if len(ranks) == 0 {
		return nil, fmt.Errorf("hybrid search: both vector and keyword backends failed")
	}
	return rrfMerge(ranks, topK), nil
}

// KnowledgeSaver 是摄入管线的落库抽象（PostgresStore.SaveKnowledge 实现）。
type KnowledgeSaver interface {
	Save(ctx context.Context, rec *types.KnowledgeRecord) error
}

// IngestOptions 是摄入参数。
type IngestOptions struct {
	KnowledgeType enums.KnowledgeType // 落库类型（默认 external_kb）
	ChunkRunes    int                 // 切块大小（默认 800）
	OverlapRunes  int                 // 块间重叠（默认 100）
	SourceTag     string              // meta.source 前缀（默认 "external"）
}

// IngestDir 批量摄入目录下 .md/.txt 文件：读文件 → 切块 → embed → 逐条 Save。
// meta 记录 namespace=external、source=相对路径、doc=文件名。
// 单文件失败仅记日志继续（不中断整批）；返回成功落库的块数。
func IngestDir(ctx context.Context, dir string, saver KnowledgeSaver, embedder Embedder, opts IngestOptions) (int, error) {
	if saver == nil || embedder == nil {
		return 0, fmt.Errorf("saver and embedder are required")
	}
	if opts.KnowledgeType == "" {
		opts.KnowledgeType = enums.KnowledgeTypeExternalKB
	}
	if opts.ChunkRunes <= 0 {
		opts.ChunkRunes = 800
	}
	if opts.OverlapRunes < 0 {
		opts.OverlapRunes = 100
	}
	if opts.SourceTag == "" {
		opts.SourceTag = "external"
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dir %s: %w", dir, err)
	}
	saved := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".md" && ext != ".txt" {
			continue
		}
		rel := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(rel)
		if err != nil {
			logf("ingest read %s failed: %v", rel, err)
			continue
		}
		chunks := Chunk(string(data), opts.ChunkRunes, opts.OverlapRunes)
		for i, chunk := range chunks {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			emb, err := embedder.Embed(ctx, chunk)
			if err != nil {
				logf("ingest embed %s chunk %d failed: %v", rel, i, err)
				continue
			}
			rec := &types.KnowledgeRecord{
				KnowledgeType: opts.KnowledgeType,
				Content:       chunk,
				Embedding:     emb,
				Meta: map[string]any{
					"namespace": "external",
					"source":    opts.SourceTag + ":" + rel,
					"doc":       e.Name(),
					"chunk":     i,
				},
			}
			if err := saver.Save(ctx, rec); err != nil {
				logf("ingest save %s chunk %d failed: %v", rel, i, err)
				continue
			}
			saved++
		}
	}
	return saved, nil
}
