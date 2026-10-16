package workflow

// drift.go 漂移检测（best-effort，可降级，开发文档 §6.4）：
// 对采纳产物的"生成提示词文本"算向量，与同阶段已采纳样本的组质心求余弦距离，
// 离群标 check_result.drift.flag=true——不阻断流程，只供联系表人审高亮。
// embed 失败静默降级（对齐既有 best-effort 风格）；跨重启不持久化（每 run 内存内累计）。

import (
	"context"
	"math"
	"sync"
)

// driftDistanceThreshold 离群阈值（余弦距离，启发式初值，观测后调）。
const driftDistanceThreshold = 0.4

// DriftInfo 是一次漂移检测结果（落 unit.check_result.drift）。
type DriftInfo struct {
	Flag     bool    `json:"flag"`               // 离群标记（人审高亮关注）
	Distance float64 `json:"distance,omitempty"` // 与组质心的余弦距离
	Samples  int     `json:"samples,omitempty"`  // 参与质心的既有样本数
	Note     string  `json:"note,omitempty"`     // 降级/跳过说明
}

// driftTracker 按阶段维护已采纳样本向量（run 内存态；best-effort 无权威）。
type driftTracker struct {
	mu      sync.Mutex
	byStage map[string][][]float32
}

func newDriftTracker() *driftTracker {
	return &driftTracker{byStage: make(map[string][][]float32)}
}

// Observe 以本 unit 的提示词文本观察一次漂移：与既有样本质心比距离并入库。
// 少于 2 个既有样本不出结论（None）；embed 失败返回降级说明。返回 nil 表示无可报告。
func (d *driftTracker) Observe(ctx context.Context, emb Embedder, stage, text string) *DriftInfo {
	if d == nil || emb == nil || stage == "" || text == "" {
		return nil
	}
	vec, err := emb.Embed(ctx, text)
	if err != nil || len(vec) == 0 {
		return &DriftInfo{Note: "embed 不可用，跳过漂移检测"}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	prior := d.byStage[stage]
	info := &DriftInfo{Samples: len(prior)}
	if len(prior) >= 2 {
		centroid := make([]float64, len(vec))
		for _, p := range prior {
			for i := range centroid {
				if i < len(p) {
					centroid[i] += float64(p[i])
				}
			}
		}
		for i := range centroid {
			centroid[i] /= float64(len(prior))
		}
		dist := cosineDistanceF64(vec, centroid)
		info.Distance = round3(dist)
		info.Flag = dist > driftDistanceThreshold
	}
	d.byStage[stage] = append(prior, vec)
	return info
}

// cosineDistanceF64 余弦距离（1 - 相似度；空向量返回 1）。
func cosineDistanceF64(vec []float32, centroid []float64) float64 {
	var dot, na, nb float64
	for i := range centroid {
		v := float64(0)
		if i < len(vec) {
			v = float64(vec[i])
		}
		dot += v * centroid[i]
		na += v * v
		nb += centroid[i] * centroid[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/math.Sqrt(na*nb)
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
