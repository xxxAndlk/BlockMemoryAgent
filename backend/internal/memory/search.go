package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// VectorSearch 向量检索接口：抽象 pgvector 之上的语义检索能力。
// 实现方需基于查询向量 embedding 返回最相似的 topK 条 KnowledgeRecord。
type VectorSearch interface {
	// Search 按嵌入向量召回 topK 条全局知识记录。
	// 参数：ctx 取消信号；embedding 查询向量；topK 召回条数上限。
	// 返回：命中记录切片与错误；语义检索不可用时返回非 nil 错误。
	Search(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
}

// Embedder 文本嵌入接口：将文本转为向量供向量检索使用。
// 实现方通常封装 OpenAI / 本地 Embedding 模型。
type Embedder interface {
	// Embed 将一段文本编码为 float32 向量。
	// 参数：ctx 取消信号；text 待编码文本。
	// 返回：嵌入向量与错误；模型不可用时应返回非 nil 错误以便上层回退。
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SearchScorer 检索与评分器：对 Episode 列表做多信号相关性评分与排序。
// 持有 Embedder 与 VectorSearch 两个外部依赖，其余计算在进程内完成。
// 并发安全：本身无共享可变状态；底层依赖由实现方保证。
type SearchScorer struct {
	embedder Embedder     // 文本嵌入模型，用于查询向量化
	vectorDB VectorSearch // 向量库，用于全局知识语义召回
}

// NewSearchScorer 创建检索评分器。
// 参数：embedder 文本嵌入模型；vectorDB 向量检索后端。
// 返回：组装好的 *SearchScorer 实例。
// 副作用：无。
func NewSearchScorer(embedder Embedder, vectorDB VectorSearch) *SearchScorer {
	// 将两个依赖注入到结构体中，后续 ScoreEpisode / SearchAndScore 复用。
	return &SearchScorer{
		embedder: embedder,
		vectorDB: vectorDB,
	}
}

// ScoreEpisode 对单个 Episode 进行多信号相关性评分。
// 综合四路信号：语义相似度、实体重叠度、时间衰减、因果链匹配度，
// 再按固定权重融合为 FinalScore，用于后续排序。
//
// 参数：
//   - ctx: 上下文，用于 Embedder 调用的取消传递（M4 修复：原用 context.Background() 忽略上层取消）。
//   - ep: 待评分的 Episode 指针。
//   - query: 当前查询文本，用于实体重叠等字符串级匹配。
//   - queryEmbedding: 查询的嵌入向量；为 nil 时跳过语义相似度计算。
//
// 返回：填充了各分项与 FinalScore 的 *types.RelevanceScore。
// 副作用：无。并发安全：纯函数式计算，无共享状态。
func (s *SearchScorer) ScoreEpisode(ctx context.Context, ep *types.Episode, query string, queryEmbedding []float32) *types.RelevanceScore {
	// 初始化评分对象，各分项默认 0，后面逐步填充。
	score := &types.RelevanceScore{}

	// 1. 语义相似度：对 Episode 摘要做嵌入，与查询向量计算余弦相似度。
	if queryEmbedding != nil && s.embedder != nil {
		epEmbedding, err := s.embedder.Embed(ctx, ep.ObservationSummary)
		if err == nil && len(epEmbedding) > 0 {
			score.SemanticSim = cosineSimilarity(queryEmbedding, epEmbedding)
		}
	}

	// 2. 实体重叠度：基于关键词（字符级）匹配，衡量查询与观察摘要的字面相关度。
	score.EntityOverlap = calculateKeywordOverlap(ep.ObservationSummary, query)

	// 3. 时间衰减：越近期的 Episode 衰减值越接近 1，强化近期记忆。
	score.TemporalDecay = calculateTemporalDecay(ep.Timestamp)

	// 4. 因果链匹配：简化启发式，依据 Reflection / Facts / Importance 估算因果强度。
	score.CausalChain = calculateCausalChain(ep, query)

	// 加权融合：语义 0.4 + 实体重叠 0.25 + 时间衰减 0.15 + 因果链 0.2 = 1.0。
	score.FinalScore = score.SemanticSim*0.4 +
		score.EntityOverlap*0.25 +
		score.TemporalDecay*0.15 +
		score.CausalChain*0.2

	// 返回完整评分对象，供调用方排序使用。
	return score
}

// SearchAndScore 对一组 Episode 执行批量检索与评分，并按 FinalScore 降序排序。
//
// 参数：
//   - ctx: 取消信号。
//   - agentID: Agent 标识，保留以便后续扩展按 Agent 过滤（当前未使用）。
//   - topicID: 话题标识，保留以便后续扩展按话题过滤（当前未使用）。
//   - query: 当前查询文本。
//   - episodes: 待评分的 Episode 列表。
//
// 返回：按 FinalScore 降序排列的 ScoredEpisode 切片与错误。
// 副作用：无。并发安全：除读取传入列表外无共享状态。
// 注意：当 Embedder 调用失败时直接返回错误，避免在调用方不知情的情况下回退到非语义评分。
func (s *SearchScorer) SearchAndScore(ctx context.Context, agentID, topicID, query string, episodes []*types.Episode) ([]*ScoredEpisode, error) {
	// 向量化查询语句，作为语义相似度的输入。
	var queryEmbedding []float32
	if s.embedder != nil {
		emb, err := s.embedder.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		queryEmbedding = emb
	}

	// 预声明结果切片，遍历 Episode 逐个评分。
	var results []*ScoredEpisode
	for _, ep := range episodes {
		// 对当前 Episode 计算多信号评分（透传 ctx 以支持取消）。
		score := s.ScoreEpisode(ctx, ep, query, queryEmbedding)
		// 将 Episode 与其评分打包成一个条目。
		results = append(results, &ScoredEpisode{
			Episode: ep,
			Score:   score,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score.FinalScore > results[j].Score.FinalScore
	})

	// 返回排序后的结果切片，调用方可直接取头部作为最相关记忆。
	return results, nil
}

// ScoredEpisode 带评分的 Episode：将 Episode 与其相关性评分捆绑，作为检索结果单元。
type ScoredEpisode struct {
	Episode *types.Episode        // 原始 Episode 指针
	Score   *types.RelevanceScore // 该 Episode 的多信号相关性评分
}

// calculateKeywordOverlap 计算查询与摘要的关键词（字符级）重叠度。
// 简化实现：逐字符判断查询中的字符是否出现在摘要中，最终归一化为 [0,1]。
//
// 参数：
//   - summary: Episode 观察摘要文本。
//   - query: 当前查询文本。
//
// 返回：重叠度 [0,1]；查询为空时返回 0。
// 副作用：无。
func calculateKeywordOverlap(summary, query string) float64 {
	// 简化实现：字符级重叠，未来可替换为分词后的 token 重叠。
	if len(query) == 0 {
		// 查询为空时无重叠可言，直接返回 0 避免除零。
		return 0
	}

	// 统计查询中命中摘要的字符数。
	overlap := 0
	queryRunes := []rune(query) // 按 rune 处理中文等多字节字符
	summaryRunes := []rune(summary)

	// 双层循环：对每个查询字符检查是否出现在摘要中。
	for _, q := range queryRunes {
		for _, s := range summaryRunes {
			if q == s {
				// 命中即累加并跳出内层循环，避免对同一字符重复计数。
				overlap++
				break
			}
		}
	}

	// 归一化：命中数 / 查询长度，得到 [0,1] 重叠率。
	if len(queryRunes) == 0 {
		// 双重保护，避免除零（前面已 early return，此处防御性编程）。
		return 0
	}
	return float64(overlap) / float64(len(queryRunes))
}

// calculateTemporalDecay 计算 Episode 的时间衰减因子。
// 采用指数衰减 exp(-lambda * hours)，lambda=0.01 表示约 69 小时衰减到 50%。
//
// 参数：
//   - t: Episode 发生时间。
//
// 返回：衰减因子 (0,1]，越接近 1 表示越近期。
// 副作用：无。
func calculateTemporalDecay(t time.Time) float64 {
	// 计算从发生时间到现在的耗时（小时）。
	hours := time.Since(t).Hours()
	if hours < 0 {
		// 时钟回拨或未来时间时，按 0 处理，避免负指数导致超过 1 的异常值。
		hours = 0
	}
	// 指数衰减：exp(-lambda * hours)，lambda = 0.01（约 69 小时衰减到 50%）。
	lambda := 0.01
	// 返回衰减因子，范围 (0,1]。
	return math.Exp(-lambda * hours)
}

// calculateCausalChain 计算 Episode 与查询的因果链匹配度（简化启发式）。
// 当 Episode 携带 Reflection / Facts 或重要性较高时，认为其因果链更强，
// 更可能在因果上与当前查询相关。
//
// 参数：
//   - ep: 待评估 Episode。
//   - query: 当前查询文本（保留以便后续扩展真实因果匹配）。
//
// 返回：因果链匹配度 [0,1]。
// 副作用：无。
func calculateCausalChain(ep *types.Episode, query string) float64 {
	// 简化启发式：根据 Reflection / Facts / Importance 累加因果强度。
	score := 0.0
	if ep.Reflection != "" {
		// 有反思的 Episode 因果信息更完整，加 0.3。
		score += 0.3
	}
	if len(ep.Facts) > 0 {
		// 每条 Fact 贡献 0.1，反映结构化事实的因果可追溯性。
		score += float64(len(ep.Facts)) * 0.1
	}
	if ep.Importance > 0.5 {
		// 高重要性 Episode 更可能是因果链关键节点，加 0.2。
		score += 0.2
	}
	if score > 1.0 {
		// 钳制到上限 1.0，保持评分范围一致。
		score = 1.0
	}
	// 返回因果链匹配度。
	return score
}

// cosineSimilarity 计算两个等长 float32 向量的余弦相似度。
// 返回 [0,1] 的相似度值；向量为空或长度不等时返回 0。
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		da := float64(a[i])
		db := float64(b[i])
		dot += da * db
		normA += da * da
		normB += db * db
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	cos := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if cos < 0 {
		return 0
	}
	if cos > 1 {
		return 1
	}
	return cos
}
