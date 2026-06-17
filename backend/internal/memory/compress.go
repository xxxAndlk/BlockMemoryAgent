package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Compressor 遗忘与压缩器
type Compressor struct {
	store PrivateStore
}

// NewCompressor 创建压缩器
func NewCompressor(store PrivateStore) *Compressor {
	return &Compressor{store: store}
}

// Compress 压缩指定 Agent 的私有记忆
func (c *Compressor) Compress(ctx context.Context, agentID, topicID string) error {
	// 获取所有 Episode
	episodes, err := c.store.GetEpisodes(ctx, agentID, topicID, 0)
	if err != nil {
		return fmt.Errorf("get episodes: %w", err)
	}

	// 按重要性 + 时间排序
	sorted := sortByImportanceAndTime(episodes)

	// 分层压缩
	var compressed []*types.Episode
	for _, ep := range sorted {
		// 高重要且新鲜: 不压缩
		if ep.Importance > 0.8 && time.Since(ep.Timestamp) < 24*time.Hour {
			continue
		}

		// 中等重要: 压缩到 Level 1
		if ep.Importance > 0.5 {
			c.compressToLevel1(ep)
			compressed = append(compressed, ep)
			continue
		}

		// 低重要但较新: 压缩到 Level 2
		if ep.Importance > 0.2 {
			c.compressToLevel2(ep)
			compressed = append(compressed, ep)
			continue
		}

		// 超过 7 天: 压缩到 Level 3 (标记)
		if time.Since(ep.Timestamp) > 7*24*time.Hour {
			c.compressToLevel3(ep)
			compressed = append(compressed, ep)
			continue
		}
	}

	// 批量持久化压缩后的 Episode
	for _, ep := range compressed {
		if err := c.store.SaveEpisode(ctx, agentID, topicID, ep); err != nil {
			return fmt.Errorf("persist compressed episode %s: %w", ep.StepID, err)
		}
	}

	return nil
}

// CompressByBudget 按 Token 预算强制压缩
func (c *Compressor) CompressByBudget(ctx context.Context, agentID, topicID string, maxTokens int) error {
	// 统计当前总 Token
	count, err := c.store.CountEpisodes(ctx, agentID, topicID)
	if err != nil {
		return err
	}

	// 假设每条平均 200 token
	estimatedTokens := count * 200
	if estimatedTokens <= maxTokens {
		return nil // 不需要压缩
	}

	// 需要压缩的比例
	compressRatio := float64(estimatedTokens-maxTokens) / float64(estimatedTokens)

	episodes, err := c.store.GetEpisodes(ctx, agentID, topicID, 0)
	if err != nil {
		return err
	}

	// 按重要性排序 (低到高)
	sorted := sortByImportanceAsc(episodes)

	// 从最低重要性开始压缩
	targetCompress := int(float64(len(sorted)) * compressRatio)
	for i := 0; i < targetCompress && i < len(sorted); i++ {
		c.compressToLevel2(sorted[i])
	}

	return nil
}

// compressToLevel1 压缩到标准级
func (c *Compressor) compressToLevel1(ep *types.Episode) {
	ep.FullObservation = ""
	// 保留 Summary + Facts
}

// compressToLevel2 压缩到精简级
func (c *Compressor) compressToLevel2(ep *types.Episode) {
	ep.FullObservation = ""
	// 仅保留 Summary 一句话
	if len(ep.ObservationSummary) > 100 {
		ep.ObservationSummary = ep.ObservationSummary[:100] + "..."
	}
	ep.Facts = nil
	ep.ToolCalls = nil
}

// compressToLevel3 压缩到标记级
func (c *Compressor) compressToLevel3(ep *types.Episode) {
	ep.FullObservation = ""
	ep.ObservationSummary = fmt.Sprintf("[%s] importance=%.2f", ep.StepID, ep.Importance)
	ep.Facts = nil
	ep.Reflection = ""
	ep.ToolCalls = nil
}

// sortByImportanceAndTime 按重要性降序、时间降序排序
func sortByImportanceAndTime(episodes []*types.Episode) []*types.Episode {
	result := make([]*types.Episode, len(episodes))
	copy(result, episodes)

	for i := 0; i < len(result)-1; i++ {
		for j := 0; j < len(result)-1-i; j++ {
			a, b := result[j], result[j+1]
			// 先按重要性降序
			if a.Importance < b.Importance {
				result[j], result[j+1] = result[j+1], result[j]
			} else if a.Importance == b.Importance {
				// 再按时间降序
				if a.Timestamp.Before(b.Timestamp) {
					result[j], result[j+1] = result[j+1], result[j]
				}
			}
		}
	}
	return result
}

// sortByImportanceAsc 按重要性升序排序
func sortByImportanceAsc(episodes []*types.Episode) []*types.Episode {
	result := make([]*types.Episode, len(episodes))
	copy(result, episodes)

	for i := 0; i < len(result)-1; i++ {
		for j := 0; j < len(result)-1-i; j++ {
			if result[j].Importance > result[j+1].Importance {
				result[j], result[j+1] = result[j+1], result[j]
			}
		}
	}
	return result
}
