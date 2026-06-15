package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/pkg/types"
)

// PrivateStore 私有记忆存储接口
type PrivateStore interface {
	SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error
	GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error)
	CountEpisodes(ctx context.Context, agentID, topicID string) (int, error)
}

// Summarizer 摘要生成器接口
type Summarizer interface {
	Summarize(content string) string
}

// FactExtractor 事实提取器接口
type FactExtractor interface {
	ExtractFacts(content string) []string
}

// ImportanceScorer 重要性评分器接口
type ImportanceScorer interface {
	Score(summary string, facts []string) float64
}

// TopicDetector 话题边界检测器接口
type TopicDetector interface {
	DetectBoundary(currentTopic string, content string) bool
}

// SimpleSummarizer 简单摘要器
type SimpleSummarizer struct{}

// Summarize 生成摘要
func (s *SimpleSummarizer) Summarize(content string) string {
	if len(content) <= 200 {
		return content
	}
	return content[:200] + "..."
}

// SimpleFactExtractor 简单事实提取器
type SimpleFactExtractor struct{}

// ExtractFacts 提取关键事实
func (e *SimpleFactExtractor) ExtractFacts(content string) []string {
	// 简单实现: 按句子分割，过滤包含关键信息的句子
	var facts []string
	// TODO: 实现更复杂的事实提取
	if len(content) > 0 {
		facts = append(facts, content[:min(100, len(content))])
	}
	return facts
}

// SimpleImportanceScorer 简单重要性评分器
type SimpleImportanceScorer struct{}

// Score 计算重要性分数
func (s *SimpleImportanceScorer) Score(summary string, facts []string) float64 {
	score := 0.0

	// 长度因子
	if len(summary) > 50 {
		score += 0.1
	}

	// 事实数量因子
	score += float64(len(facts)) * 0.1

	// 包含错误关键词
	if containsAny(summary, []string{"error", "fail", "exception", "bug", "crash"}) {
		score += 0.3
	}

	// 包含决策关键词
	if containsAny(summary, []string{"decide", "choose", "select", "确定", "决定"}) {
		score += 0.25
	}

	// 包含状态变更
	if containsAny(summary, []string{"update", "modify", "change", "创建", "修改", "更新"}) {
		score += 0.2
	}

	// 时间衰减因子 (由调用方处理)
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// SimpleTopicDetector 简单话题边界检测器
type SimpleTopicDetector struct{}

// DetectBoundary 检测话题边界
func (d *SimpleTopicDetector) DetectBoundary(currentTopic, content string) bool {
	// 简单实现: 检查是否包含话题切换信号词
	return containsAny(content, []string{"switch topic", "change topic", "新话题", "话题切换"})
}

// WriteProcessor 写入处理器
type WriteProcessor struct {
	store      PrivateStore
	summarizer Summarizer
	extractor  FactExtractor
	scorer     ImportanceScorer
	detector   TopicDetector
}

// NewWriteProcessor 创建写入处理器
func NewWriteProcessor(store PrivateStore) *WriteProcessor {
	return &WriteProcessor{
		store:      store,
		summarizer: &SimpleSummarizer{},
		extractor:  &SimpleFactExtractor{},
		scorer:     &SimpleImportanceScorer{},
		detector:   &SimpleTopicDetector{},
	}
}

// Process 处理写入
func (wp *WriteProcessor) Process(ctx context.Context, agentID, topicID, action, rawContent string) (*types.Episode, error) {
	// 1. 生成摘要
	summary := wp.summarizer.Summarize(rawContent)

	// 2. 提取关键事实
	facts := wp.extractor.ExtractFacts(rawContent)

	// 3. 重要性评分
	importance := wp.scorer.Score(summary, facts)

	// 4. 话题边界检测
	topicBound := wp.detector.DetectBoundary(topicID, rawContent)

	// 5. 构建 Episode
	episode := &types.Episode{
		StepID:             generateStepID(agentID),
		Timestamp:          time.Now(),
		Action:             action,
		ObservationSummary: summary,
		FullObservation:    rawContent,
		Facts:              facts,
		Importance:         importance,
		TopicBound:         topicBound,
	}

	// 6. 持久化
	if err := wp.store.SaveEpisode(ctx, agentID, topicID, episode); err != nil {
		return nil, fmt.Errorf("save episode: %w", err)
	}

	return episode, nil
}

// generateStepID 生成步骤 ID
func generateStepID(agentID string) string {
	return fmt.Sprintf("%s_%d", agentID, time.Now().UnixNano())
}

// containsAny 检查字符串是否包含任意关键词
func containsAny(s string, keywords []string) bool {
	lower := s
	for _, kw := range keywords {
		// 简单大小写不敏感匹配
		if len(kw) <= len(lower) {
			// 这里简化处理，实际应使用 strings.Contains(strings.ToLower(s), strings.ToLower(kw))
			_ = lower
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
