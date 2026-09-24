package retriever

// rrf.go 实现 混合检索的 RRF（Reciprocal Rank Fusion）融合排序：
// 向量检索与全文关键词检索各自召回 topK，按 1/(k+rank) 加权融合，去重保序。

import (
	"sort"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// rrfConstant 是 RRF 标准常数（k=60），避免排名靠前的极端权重。
const rrfConstant = 60

// rrfMerge 融合多路检索结果（按记录 ID 去重），返回按融合分降序的 topK 条。
// ranks 的每路结果应已按相关度排序（第 i 条 rank=i，从 0 起）。
func rrfMerge(ranks [][]*types.KnowledgeRecord, topK int) []*types.KnowledgeRecord {
	scores := make(map[int64]float64)
	order := make([]int64, 0)
	for _, list := range ranks {
		for rank, rec := range list {
			if rec == nil || rec.ID == 0 {
				continue
			}
			if _, seen := scores[rec.ID]; !seen {
				order = append(order, rec.ID)
			}
			scores[rec.ID] += 1.0 / float64(rrfConstant+rank+1)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return scores[order[i]] > scores[order[j]]
	})
	if topK <= 0 || len(order) < topK {
		topK = len(order)
	}
	// 还原记录对象（取第一路的引用）。
	byID := make(map[int64]*types.KnowledgeRecord)
	for _, list := range ranks {
		for _, rec := range list {
			if rec != nil && rec.ID != 0 {
				byID[rec.ID] = rec
			}
		}
	}
	out := make([]*types.KnowledgeRecord, 0, topK)
	for _, id := range order[:topK] {
		out = append(out, byID[id])
	}
	return out
}
