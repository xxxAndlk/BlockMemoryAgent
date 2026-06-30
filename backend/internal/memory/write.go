package memory

import (
	"context"  // 上下文，用于取消与超时传递
	"fmt"      // 格式化与错误包装
	"strings"  // 字符串小写化与子串匹配
	"time"     // 时间戳生成与时间间隔判断

	"github.com/blockmemory/agent/backend/pkg/types" // Episode 等公共类型
)

// PrivateStore 私有记忆存储接口。
// 抽象 Episode 的持久化与查询能力，使 WriteProcessor/Compressor 可对接
// Postgres、Redis 或任意后端，便于单测注入 fake 实现。
type PrivateStore interface {
	// SaveEpisode 将单条 Episode 写入指定 Agent + Topic 的私有记忆空间。
	SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error
	// GetEpisodes 读取 Episode 列表；limit<=0 表示不限制条数。
	GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error)
	// CountEpisodes 统计 Episode 总数，供按预算压缩时估算 Token 占用。
	CountEpisodes(ctx context.Context, agentID, topicID string) (int, error)
}

// Summarizer 摘要生成器接口。
// 输入原始观察文本，输出短摘要；用于压缩层级的 Standard/Compact 展示。
type Summarizer interface {
	// Summarize 返回 content 的摘要文本。
	Summarize(content string) string
}

// FactExtractor 事实提取器接口。
// 从原始观察中抽取关键事实短句，参与实体重叠评分与重要性计算。
type FactExtractor interface {
	// ExtractFacts 返回提取出的事实列表。
	ExtractFacts(content string) []string
}

// ImportanceScorer 重要性评分器接口。
// 基于摘要与事实给出 [0,1] 重要性分数，决定 Episode 是否进入长期记忆与压缩层级。
type ImportanceScorer interface {
	// Score 返回重要性分数，范围 [0,1]。
	Score(summary string, facts []string) float64
}

// TopicDetector 话题边界检测器接口。
// 判断当前内容是否触发话题切换，用于 Episode 的话题绑定决策。
type TopicDetector interface {
	// DetectBoundary 返回 true 表示检测到话题边界。
	DetectBoundary(currentTopic string, content string) bool
}

// SimpleSummarizer 简单摘要器。
// 短文本原样返回，长文本截断到 200 字符并加省略号。
type SimpleSummarizer struct{}

// Summarize 生成摘要。
// 职责: 对 content 做长度截断式摘要。
// 参数: content - 原始观察文本。
// 返回: 不超过 200 字符的摘要字符串。
// 副作用: 无。
// 并发安全: 是（无共享状态）。
func (s *SimpleSummarizer) Summarize(content string) string {
	// 短文本无需摘要，直接原样返回
	if len(content) <= 200 {
		return content
	}
	// 长文本截断到前 200 字符并标记省略
	return content[:200] + "..."
}

// SimpleFactExtractor 简单事实提取器。
// 占位实现：取内容前 100 字符作为唯一事实条目。
type SimpleFactExtractor struct{}

// ExtractFacts 提取关键事实。
// 职责: 将 content 按句分割，取前 3 句作为事实条目。
// 参数: content - 原始观察文本。
// 返回: 事实字符串切片；空输入返回 nil。
// 副作用: 无。
// 并发安全: 是（无共享状态）。
func (e *SimpleFactExtractor) ExtractFacts(content string) []string {
	if len(content) == 0 {
		return nil
	}
	// 按中英文标点分句：。！？!?.\n
	sentences := splitSentences(content)
	var facts []string
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if len(s) > 0 {
			facts = append(facts, s)
		}
		if len(facts) >= 3 {
			break
		}
	}
	if len(facts) == 0 {
		facts = append(facts, content[:min(100, len(content))])
	}
	return facts
}

// splitSentences 按标点符号分割句子。
func splitSentences(text string) []string {
	var sentences []string
	start := 0
	runes := []rune(text)
	for i, r := range runes {
		switch r {
		case '。', '！', '？', '.', '!', '?', '\n':
			if i > start {
				sentences = append(sentences, string(runes[start:i+1]))
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		remain := strings.TrimSpace(string(runes[start:]))
		if len(remain) > 0 {
			sentences = append(sentences, remain)
		}
	}
	return sentences
}

// SimpleImportanceScorer 简单重要性评分器。
// 基于摘要长度、事实数量、错误/决策/变更关键词加权打分，上限 1.0。
type SimpleImportanceScorer struct{}

// Score 计算重要性分数。
// 职责: 综合长度、事实数、关键词命中给出重要性分数。
// 参数: summary - 已生成的摘要；facts - 已提取的事实列表。
// 返回: [0,1] 的重要性分数，越大越值得长期保留。
// 副作用: 无。
// 并发安全: 是（无共享状态）。
// 设计: 时间衰减由调用方处理，此处只反映内容本身的静态权重。
func (s *SimpleImportanceScorer) Score(summary string, facts []string) float64 {
	// 初始分数为 0，后续逐步累加各项权重
	score := 0.0

	// 长度因子: 摘要较长说明信息量足，加 0.1
	if len(summary) > 50 {
		score += 0.1
	}

	// 事实数量因子: 每条事实贡献 0.1
	score += float64(len(facts)) * 0.1

	// 包含错误关键词: 错误类事件通常高价值，加 0.3
	if containsAny(summary, []string{"error", "fail", "exception", "bug", "crash"}) {
		score += 0.3
	}

	// 包含决策关键词: 决策类事件影响后续走向，加 0.25
	if containsAny(summary, []string{"decide", "choose", "select", "确定", "决定"}) {
		score += 0.25
	}

	// 包含状态变更: 状态变更需留痕便于回溯，加 0.2
	if containsAny(summary, []string{"update", "modify", "change", "创建", "修改", "更新"}) {
		score += 0.2
	}

	// 时间衰减因子由调用方处理（本函数只做静态权重）
	// 封顶到 1.0，避免关键词叠加导致分数溢出
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// SimpleTopicDetector 简单话题边界检测器。
// 通过关键词匹配判断是否发生显式话题切换。
type SimpleTopicDetector struct{}

// DetectBoundary 检测话题边界。
// 职责: 判断 content 是否包含话题切换信号。
// 参数: currentTopic - 当前话题标识；content - 待检测文本。
// 返回: true 表示检测到话题边界。
// 副作用: 无。
// 并发安全: 是（无共享状态）。
func (d *SimpleTopicDetector) DetectBoundary(currentTopic, content string) bool {
	// 简单实现: 检查是否包含话题切换信号词
	return containsAny(content, []string{"switch topic", "change topic", "新话题", "话题切换"})
}

// WriteProcessor 写入处理器。
// 串联 Summarizer → FactExtractor → ImportanceScorer → TopicDetector 四阶段流水线，
// 完成一条 Episode 的构建与持久化。所有依赖以接口形式注入，便于替换实现。
type WriteProcessor struct {
	store      PrivateStore     // 私有记忆存储后端
	summarizer Summarizer       // 摘要生成器
	extractor  FactExtractor    // 事实提取器
	scorer     ImportanceScorer // 重要性评分器
	detector   TopicDetector    // 话题边界检测器
}

// NewWriteProcessor 创建写入处理器。
// 职责: 装配默认的 Simple* 实现并绑定存储后端。
// 参数: store - PrivateStore 实现，用于 Episode 持久化。
// 返回: 装配完成的 WriteProcessor 指针。
// 副作用: 无。
// 并发安全: 返回对象本身可被多协程共享使用（字段只读）。
func NewWriteProcessor(store PrivateStore) *WriteProcessor {
	return &WriteProcessor{
		store:      store,                     // 注入存储后端
		summarizer: &SimpleSummarizer{},       // 默认摘要器
		extractor:  &SimpleFactExtractor{},    // 默认事实提取器
		scorer:     &SimpleImportanceScorer{}, // 默认重要性评分器
		detector:   &SimpleTopicDetector{},    // 默认话题边界检测器
	}
}

// Process 处理写入。
// 职责: 将一条原始观察加工为 Episode 并持久化，是 write 阶段的主入口。
// 参数:
//   - ctx: 上下文，用于取消与超时。
//   - agentID: 归属 Agent 标识。
//   - topicID: 归属 Topic 标识。
//   - action: 本步动作摘要（如 "调用工具 X"）。
//   - rawContent: 原始完整观察文本。
// 返回: 构建完成的 Episode；持久化失败时返回 wrapped error。
// 副作用: 向 PrivateStore 写入一条 Episode。
// 并发安全: 自身无共享可变状态，并发安全性取决于底层 store 实现。
// 设计: 流水线四步——摘要 → 事实 → 评分 → 话题绑定，再组装与持久化。
func (wp *WriteProcessor) Process(ctx context.Context, agentID, topicID, action, rawContent string) (*types.Episode, error) {
	// 1. 生成摘要: 压缩原始内容供后续展示与评分
	summary := wp.summarizer.Summarize(rawContent)

	// 2. 提取关键事实: 供实体重叠评分与重要性计算
	facts := wp.extractor.ExtractFacts(rawContent)

	// 3. 重要性评分: 决定压缩层级与是否进入长期记忆
	importance := wp.scorer.Score(summary, facts)

	// 4. 话题边界检测: 判断是否触发话题切换
	topicBound := wp.detector.DetectBoundary(topicID, rawContent)

	// 5. 构建 Episode: 装配步骤 ID、时间戳与各字段
	episode := &types.Episode{
		StepID:             generateStepID(agentID), // 全局唯一步骤 ID
		Timestamp:          time.Now(),              // 当前时间作为发生时间
		Action:             action,                  // 动作摘要
		ObservationSummary: summary,                 // 观察摘要
		FullObservation:    rawContent,              // 原始观察全文
		Facts:              facts,                   // 提取的事实列表
		Importance:         importance,              // 重要性分数
		TopicBound:         topicBound,              // 话题绑定标志
	}

	// 6. 持久化: 写入私有记忆存储，失败时包装错误返回
	if err := wp.store.SaveEpisode(ctx, agentID, topicID, episode); err != nil {
		return nil, fmt.Errorf("save episode: %w", err)
	}

	return episode, nil
}

// generateStepID 生成步骤 ID。
// 职责: 以 agentID + 纳秒时间戳拼接出全局唯一步骤标识。
// 参数: agentID - 归属 Agent 标识。
// 返回: 形如 "{agentID}_{unixNano}" 的字符串。
// 副作用: 无。
// 并发安全: 是（time.Now 线程安全，无共享状态）。
func generateStepID(agentID string) string {
	return fmt.Sprintf("%s_%d", agentID, time.Now().UnixNano())
}

// containsAny 检查字符串是否包含任意关键词（大小写不敏感）。
// 职责: 对 s 做小写化后逐个匹配 keywords。
// 参数: s - 待检测文本；keywords - 关键词列表。
// 返回: 命中任意关键词返回 true，否则 false。
// 副作用: 无。
// 并发安全: 是（纯函数）。
func containsAny(s string, keywords []string) bool {
	// 统一小写化以做大小写不敏感匹配
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		// 关键词同样小写化后做子串包含判断
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
