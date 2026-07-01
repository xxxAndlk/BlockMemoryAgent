package memory

import (
	"context"  // 上下文，用于取消与超时传递
	"fmt"      // 格式化与错误包装
	"sort"     // sort.Slice 替代手写冒泡，稳定性与可读性更佳
	"time"     // 时间间隔判断（24h、7d 等压缩阈值）
	"unicode/utf8" // rune 计数与按字符截断，避免破坏 UTF-8

	"github.com/blockmemory/agent/backend/pkg/types" // Episode 等公共类型
)

// Compressor 遗忘与压缩器。
// 负责 Episode 的四级压缩（Raw → Standard → Compact → Marker），
// 按重要性 + 时间分层决策，降低长期记忆的 Token 占用。
type Compressor struct {
	store PrivateStore // 私有记忆存储后端
}

// NewCompressor 创建压缩器。
// 职责: 绑定存储后端，返回可用的 Compressor。
// 参数: store - PrivateStore 实现。
// 返回: Compressor 指针。
// 副作用: 无。
// 并发安全: 返回对象可被多协程共享使用（字段只读）。
func NewCompressor(store PrivateStore) *Compressor {
	return &Compressor{store: store}
}

// Compress 压缩指定 Agent 的私有记忆。
// 职责: 按重要性 + 时间排序后分层压缩，是 compress 阶段的主入口。
// 参数:
//   - ctx: 上下文。
//   - agentID: Agent 标识。
//   - topicID: Topic 标识。
// 返回: 持久化失败时返回 wrapped error。
// 副作用: 修改 Episode 字段并批量写回 PrivateStore。
// 并发安全: 自身无共享可变状态；并发安全性取决于底层 store 实现。
// 设计: 高重要且新鲜→保留；中重要→Level1；低重要→Level2；超 7 天→Level3。
func (c *Compressor) Compress(ctx context.Context, agentID, topicID string) error {
	// 获取所有 Episode: limit=0 表示不限制条数
	episodes, err := c.store.GetEpisodes(ctx, agentID, topicID, 0)
	if err != nil {
		return fmt.Errorf("get episodes: %w", err)
	}

	// 按重要性 + 时间排序: 优先保留高价值、新鲜的 Episode
	sorted := sortByImportanceAndTime(episodes)

	// 分层压缩: 收集本轮被压缩的 Episode 待批量回写
	var compressed []*types.Episode
	for _, ep := range sorted {
		// 高重要且新鲜: 不压缩，原样保留（Raw 层）
		if ep.Importance > 0.8 && time.Since(ep.Timestamp) < 24*time.Hour {
			continue
		}

		// 中等重要: 压缩到 Level 1（Standard，丢弃全文保留摘要+事实）
		if ep.Importance > 0.5 {
			c.compressToLevel1(ep)
			compressed = append(compressed, ep)
			continue
		}

		// 低重要但较新: 压缩到 Level 2（Compact，仅留短摘要）
		if ep.Importance > 0.2 {
			c.compressToLevel2(ep)
			compressed = append(compressed, ep)
			continue
		}

		// 超过 7 天: 压缩到 Level 3（Marker，仅留 ID 与分数）
		if time.Since(ep.Timestamp) > 7*24*time.Hour {
			c.compressToLevel3(ep)
			compressed = append(compressed, ep)
			continue
		}
	}

	// 批量持久化压缩后的 Episode: 将就地修改的字段写回存储
	for _, ep := range compressed {
		if err := c.store.SaveEpisode(ctx, agentID, topicID, ep); err != nil {
			return fmt.Errorf("persist compressed episode %s: %w", ep.StepID, err)
		}
	}

	return nil
}

// CompressByBudget 按 Token 预算强制压缩。
// 职责: 当估算 Token 超过预算时，从最低重要性开始批量压缩到 Level 2。
// 参数:
//   - ctx: 上下文。
//   - agentID: Agent 标识。
//   - topicID: Topic 标识。
//   - maxTokens: 允许的最大 Token 预算。
// 返回: 读取或统计失败时返回 error；无需压缩返回 nil。
// 副作用: 就地修改低重要性 Episode 的字段并逐条写回 PrivateStore（C2 修复：原实现未持久化，导致压缩管线形同虚设）。
// 并发安全: 自身无共享可变状态；并发安全性取决于底层 store 实现。
// 设计: 以"每条平均 200 token"粗略估算，按超出比例压缩对应条数。
func (c *Compressor) CompressByBudget(ctx context.Context, agentID, topicID string, maxTokens int) error {
	// 统计当前 Episode 条数，作为 Token 估算基础
	count, err := c.store.CountEpisodes(ctx, agentID, topicID)
	if err != nil {
		return err
	}

	// 假设每条平均 200 token: 粗略估算总 Token 占用
	estimatedTokens := count * 200
	if estimatedTokens <= maxTokens {
		return nil // 不需要压缩
	}

	// 需要压缩的比例: 超出部分占总量的比例
	compressRatio := float64(estimatedTokens-maxTokens) / float64(estimatedTokens)

	// 重新拉取全部 Episode 待压缩
	episodes, err := c.store.GetEpisodes(ctx, agentID, topicID, 0)
	if err != nil {
		return err
	}

	// 按重要性升序排序: 优先压缩低价值条目
	sorted := sortByImportanceAsc(episodes)

	// 从最低重要性开始压缩: 压缩目标条数 = 总条数 × 压缩比例
	targetCompress := int(float64(len(sorted)) * compressRatio)
	for i := 0; i < targetCompress && i < len(sorted); i++ {
		c.compressToLevel2(sorted[i])
		// 持久化压缩后的 Episode：原实现遗漏此步导致压缩结果丢失
		if err := c.store.SaveEpisode(ctx, agentID, topicID, sorted[i]); err != nil {
			return fmt.Errorf("persist budget-compressed episode %s: %w", sorted[i].StepID, err)
		}
	}

	return nil
}

// compressToLevel1 压缩到标准级（Standard）。
// 职责: 丢弃原始全文，保留摘要 + 事实，信息损失较小。
// 参数: ep - 待压缩的 Episode 指针（就地修改）。
// 副作用: 清空 ep.FullObservation。
// 并发安全: 非并发安全（调用方需保证对同一 ep 的独占访问）。
func (c *Compressor) compressToLevel1(ep *types.Episode) {
	ep.FullObservation = ""
	// 保留 Summary + Facts: 供实体重叠评分与摘要展示
}

// compressToLevel2 压缩到精简级（Compact）。
// 职责: 丢弃全文与事实，仅保留截断到 100 个 rune 的短摘要。
// 参数: ep - 待压缩的 Episode 指针（就地修改）。
// 副作用: 清空 FullObservation、Facts、ToolCalls，按 rune 截断 ObservationSummary（避免破坏 UTF-8 多字节字符）。
// 并发安全: 非并发安全（调用方需保证对同一 ep 的独占访问）。
func (c *Compressor) compressToLevel2(ep *types.Episode) {
	ep.FullObservation = ""
	// 仅保留 Summary 一句话: 超过 100 个 rune 则截断并加省略号（按 rune 截断避免中文 UTF-8 断裂）
	if utf8.RuneCountInString(ep.ObservationSummary) > 100 {
		runes := []rune(ep.ObservationSummary)
		ep.ObservationSummary = string(runes[:100]) + "..."
	}
	ep.Facts = nil    // 丢弃事实列表
	ep.ToolCalls = nil // 丢弃工具调用明细
}

// compressToLevel3 压缩到标记级（Marker）。
// 职责: 仅保留步骤 ID 与重要性分数，作为存在性标记，信息损失最大。
// 参数: ep - 待压缩的 Episode 指针（就地修改）。
// 副作用: 清空 FullObservation、Facts、Reflection、ToolCalls，覆写 ObservationSummary。
// 并发安全: 非并发安全（调用方需保证对同一 ep 的独占访问）。
func (c *Compressor) compressToLevel3(ep *types.Episode) {
	ep.FullObservation = ""
	// 用 ID + 分数覆盖摘要，仅留存在性标记
	ep.ObservationSummary = fmt.Sprintf("[%s] importance=%.2f", ep.StepID, ep.Importance)
	ep.Facts = nil      // 丢弃事实
	ep.Reflection = ""  // 丢弃反思
	ep.ToolCalls = nil  // 丢弃工具调用
}

// sortByImportanceAndTime 按重要性降序、时间降序排序。
// 职责: 返回新的排序切片，优先级为重要性高 → 时间新。
// 参数: episodes - 待排序的 Episode 切片。
// 返回: 排序后的新切片（不修改原切片）。
// 副作用: 无（拷贝后排序）。
// 并发安全: 是（纯函数，不修改入参）。
// 实现: sort.Slice 替代原 O(n²) 冒泡，长列表性能更佳。
func sortByImportanceAndTime(episodes []*types.Episode) []*types.Episode {
	result := make([]*types.Episode, len(episodes))
	copy(result, episodes)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Importance != result[j].Importance {
			return result[i].Importance > result[j].Importance // 重要性降序
		}
		return result[i].Timestamp.After(result[j].Timestamp) // 时间降序
	})
	return result
}

// sortByImportanceAsc 按重要性升序排序。
// 职责: 返回新的排序切片，重要性低的在前（压缩时优先处理）。
// 参数: episodes - 待排序的 Episode 切片。
// 返回: 排序后的新切片（不修改原切片）。
// 副作用: 无（拷贝后排序）。
// 并发安全: 是（纯函数，不修改入参）。
// 实现: sort.Slice，用于 CompressByBudget 从最低价值开始压缩。
func sortByImportanceAsc(episodes []*types.Episode) []*types.Episode {
	result := make([]*types.Episode, len(episodes))
	copy(result, episodes)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Importance < result[j].Importance // 重要性升序
	})
	return result
}
