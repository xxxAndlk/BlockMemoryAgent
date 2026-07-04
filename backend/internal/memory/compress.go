package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Compressor 遗忘与压缩器。
// 负责 Episode 的两级压缩（Raw → Standard），按重要性 + 时间分层决策，降低长期记忆的 Token 占用。
type Compressor struct {
	store PrivateStore       // 私有记忆存储后端
	cfg   *config.AgentConfig // Agent 运行时配置（压缩阈值）
}

// NewCompressor 创建压缩器。
// 职责: 绑定存储后端，返回可用的 Compressor。
// 参数: store - PrivateStore 实现；cfg - Agent 配置（可为 nil，使用默认）。
// 返回: Compressor 指针。
func NewCompressor(store PrivateStore, cfg *config.AgentConfig) *Compressor {
	return &Compressor{store: store, cfg: cfg}
}

// importanceThreshold 返回保留 Raw 的重要性阈值。
func (c *Compressor) importanceThreshold() float64 {
	if c.cfg != nil && c.cfg.CompressImportanceThreshold > 0 {
		return c.cfg.CompressImportanceThreshold
	}
	return 0.7
}

// maxAge 返回保留 Raw 的最大年龄。
func (c *Compressor) maxAge() time.Duration {
	hours := 24
	if c.cfg != nil && c.cfg.CompressAgeHours > 0 {
		hours = c.cfg.CompressAgeHours
	}
	return time.Duration(hours) * time.Hour
}

// Compress 压缩指定 Agent 的私有记忆。
// 职责: 按重要性 + 时间排序后分层压缩，是 compress 阶段的主入口。
// 设计: 高重要且新鲜 → 保留 Raw；否则压缩为 Standard（丢弃 FullObservation）。
func (c *Compressor) Compress(ctx context.Context, agentID, topicID string) error {
	episodes, err := c.store.GetEpisodes(ctx, agentID, topicID, 0)
	if err != nil {
		return fmt.Errorf("get episodes: %w", err)
	}

	sorted := sortByImportanceAndTime(episodes)

	threshold := c.importanceThreshold()
	maxAge := c.maxAge()
	var compressed []*types.Episode
	for _, ep := range sorted {
		// 高重要且新鲜：保留 Raw。
		// 加入轻微缓冲：重要性 >= threshold 且年龄 < maxAge 即保留，避免硬阈值附近震荡。
		if ep.Importance >= threshold && time.Since(ep.Timestamp) < maxAge {
			continue
		}
		// 其余压缩为 Standard：丢弃原始全文，保留摘要
		c.compressToStandard(ep)
		compressed = append(compressed, ep)
	}

	for _, ep := range compressed {
		if err := c.store.SaveEpisode(ctx, agentID, topicID, ep); err != nil {
			return fmt.Errorf("persist compressed episode %s: %w", ep.StepID, err)
		}
	}

	return nil
}

// compressToStandard 压缩到标准级（Standard）。
// 职责：丢弃原始全文，保留摘要，信息损失较小。
func (c *Compressor) compressToStandard(ep *types.Episode) {
	ep.FullObservation = ""
}

// sortByImportanceAndTime 按重要性降序、时间降序排序。
// 优先级为重要性高 → 时间新。
func sortByImportanceAndTime(episodes []*types.Episode) []*types.Episode {
	result := make([]*types.Episode, len(episodes))
	copy(result, episodes)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Importance != result[j].Importance {
			return result[i].Importance > result[j].Importance
		}
		return result[i].Timestamp.After(result[j].Timestamp)
	})
	return result
}
