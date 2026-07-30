package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FactExtractor 从子 Agent 输出中提取关键事实、决策或结论。
//
// 设计意图：替代 saveBlockMemory 直接存原始 result.Text 的做法。
// 原始结果冗长（含推理过程/工具输出），召回时注入污染上下文且语义模糊。
// 提取后存为多条独立 KnowledgeRecord，每条单独向量化，召回精度与 token 效率双升。
//
// 实现方负责选择模型与提示词；调用方（Dispatcher）负责失败回退到原始保存。
type FactExtractor interface {
	// Extract 返回 1-5 条独立成立的事实。
	// text 为子 Agent 原始输出；goal/roleID 提供任务上下文用于聚焦提取。
	// 返回空切片或 error 时，调用方回退到原始文本保存。
	Extract(ctx context.Context, text, goal, roleID string) ([]string, error)
}

// ParseFactsJSON 解析 LLM 返回的 JSON 字符串数组。
// 兼容三种格式：
//   - 纯 JSON 数组：["fact1", "fact2"]
//   - markdown 代码块包裹：```json\n[...]\n```
//   - 含前后解释文本（取首个 JSON 数组片段）
func ParseFactsJSON(s string) ([]string, error) {
	return parseFactsJSON(s)
}

// parseFactsJSON 是 ParseFactsJSON 的内部实现。
func parseFactsJSON(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty response")
	}
	// 剥除 markdown 代码围栏。
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimPrefix(s, "\n")
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	// 截取首个 JSON 数组片段（LLM 可能输出多余文本）。
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found in response")
	}
	arr := s[start : end+1]
	var facts []string
	if err := json.Unmarshal([]byte(arr), &facts); err != nil {
		return nil, fmt.Errorf("unmarshal facts: %w", err)
	}
	// 过滤空串与纯空白。
	out := facts[:0]
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}
