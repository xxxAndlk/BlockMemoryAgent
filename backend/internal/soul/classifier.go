package soul

import "strings"

// TaskKindClassifier 根据任务文本推断任务类别。
//
// 用于将自然语言描述映射到 TaskKind，以决定后续温度、路由等策略。
type TaskKindClassifier interface {
	// Classify 推断任务类别。
	//
	// 参数 text：任务描述文本。
	// 返回：对应的 TaskKind。
	Classify(text string) TaskKind
}

// KeywordClassifier 基于关键词集合启发式推断 TaskKind，
// 复用 soul.go 中既有的关键词匹配逻辑。
type KeywordClassifier struct{}

// Classify 实现 TaskKindClassifier。
//
// 匹配顺序按优先级排列：路由 > 总结 > 代码 > 创意 > 分析，先命中先返回；
// 无匹配时返回 KindGeneric。
func (KeywordClassifier) Classify(text string) TaskKind {
	// 统一转小写，使中英文关键词都能大小写无关地匹配。
	lower := strings.ToLower(text)
	// switch 语句按优先级依次检查各类别关键词。
	switch {
	case containsAny(lower, []string{"路由", "选择 skill", "select skill", "判断", "决定"}):
		return KindRouting
	case containsAny(lower, []string{"总结", "摘要", "压缩", "summary", "summarize"}):
		return KindSummarize
	case containsAny(lower, []string{"代码", "code", "bug", "重构", "math"}):
		return KindCode
	case containsAny(lower, []string{"创意", "头脑风暴", "brainstorm", "文案", "营销"}):
		return KindCreative
	case containsAny(lower, []string{"分析", "排查", "诊断", "为什么", "why"}):
		return KindAnalysis
	}
	// 所有关键词均未命中，返回通用类别。
	return KindGeneric
}

// containsAny 判断 s 是否包含 words 中任意一个子串。
func containsAny(s string, words []string) bool {
	// 遍历关键词列表，命中任一即返回 true。
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
