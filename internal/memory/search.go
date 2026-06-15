package memory

import (
	"context"
	"math"
	"time"

	"github.com/blockmemory/agent/pkg/types"
)

// VectorSearch 向量搜索接口
type VectorSearch interface {
	Search(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
}

// Embedder 嵌入模型接口
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SearchScorer 检索与评分器
type SearchScorer struct {
	embedder Embedder
	vectorDB VectorSearch
}

// NewSearchScorer 创建检索评分器
func NewSearchScorer(embedder Embedder, vectorDB VectorSearch) *SearchScorer {
	return &SearchScorer{
		embedder: embedder,
		vectorDB: vectorDB,
	}
}

// ScoreEpisode 对单个 Episode 进行多信号评分
func (s *SearchScorer) ScoreEpisode(ep *types.Episode, query string, queryEmbedding []float32) *types.RelevanceScore {
	score := &types.RelevanceScore{}

	// 1. 语义相似度 (需要嵌入模型)
	if queryEmbedding != nil {
		// TODO: 计算 Episode 摘要的嵌入向量并计算余弦相似度
		score.SemanticSim = 0.5 // 占位
	}

	// 2. 实体重叠度 (关键词匹配)
	score.EntityOverlap = calculateKeywordOverlap(ep.ObservationSummary, query)

	// 3. 时间衰减
	score.TemporalDecay = calculateTemporalDecay(ep.Timestamp)

	// 4. 因果链匹配 (简化: 检查引用关系)
	score.CausalChain = calculateCausalChain(ep, query)

	// 加权总和
	score.FinalScore = score.SemanticSim*0.4 +
		score.EntityOverlap*0.25 +
		score.TemporalDecay*0.15 +
		score.CausalChain*0.2

	return score
}

// SearchAndScore 搜索并评分
func (s *SearchScorer) SearchAndScore(ctx context.Context, agentID, topicID, query string, episodes []*types.Episode) ([]*ScoredEpisode, error) {
	// 向量化查询
	queryEmbedding, err := s.embedder.Embed(ctx, query)
	if err != nil {
		queryEmbedding = nil
	}

	var results []*ScoredEpisode
	for _, ep := range episodes {
		score := s.ScoreEpisode(ep, query, queryEmbedding)
		results = append(results, &ScoredEpisode{
			Episode: ep,
			Score:   score,
		})
	}

	// 按 FinalScore 降序排序
	for i := 0; i < len(results)-1; i++ {
		for j := 0; j < len(results)-1-i; j++ {
			if results[j].Score.FinalScore < results[j+1].Score.FinalScore {
				results[j], results[j+1] = results[j+1], results[j]
			}
		}
	}

	return results, nil
}

// ScoredEpisode 带评分的 Episode
type ScoredEpisode struct {
	Episode *types.Episode
	Score   *types.RelevanceScore
}

// calculateKeywordOverlap 计算关键词重叠度
func calculateKeywordOverlap(summary, query string) float64 {
	// 简化实现: 字符级重叠
	if len(query) == 0 {
		return 0
	}

	overlap := 0
	queryRunes := []rune(query)
	summaryRunes := []rune(summary)

	for _, q := range queryRunes {
		for _, s := range summaryRunes {
			if q == s {
				overlap++
				break
			}
		}
	}

	// 归一化
	if len(queryRunes) == 0 {
		return 0
	}
	return float64(overlap) / float64(len(queryRunes))
}

// calculateTemporalDecay 计算时间衰减
func calculateTemporalDecay(t time.Time) float64 {
	hours := time.Since(t).Hours()
	if hours < 0 {
		hours = 0
	}
	// 指数衰减: exp(-lambda * hours), lambda = 0.01 (约 69 小时衰减到 50%)
	lambda := 0.01
	return math.Exp(-lambda * hours)
}

// calculateCausalChain 计算因果链匹配度
func calculateCausalChain(ep *types.Episode, query string) float64 {
	// 简化: 如果有 Reflection 或 Facts，认为因果链较强
	score := 0.0
	if ep.Reflection != "" {
		score += 0.3
	}
	if len(ep.Facts) > 0 {
		score += float64(len(ep.Facts)) * 0.1
	}
	if ep.Importance > 0.5 {
		score += 0.2
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}
