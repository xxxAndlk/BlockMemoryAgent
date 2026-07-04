package graph

// 本文件承载 blades 工具循环的工具列表构造与 prompt 拼装辅助函数。
// 从 llm_tools.go 拆出（P0-3），保持单一职责：工具清单合并 + 行解析 + OS 提示。

import (
	"strings"
)

// mergeToolList 把 skillBrief（来自 SkillSet.PromptList，以 ToolRef 为工具名）
// 与 defaultTools 合并，按行首工具名去重。skillBrief 行优先保留。
// 两者都空时返回 defaultTools。
//
// 职责：将动态装配的技能简介与默认工具列表合并为一份去重的工具清单，
//
//	用于在 system prompt 中向 LLM 展示当前可用的工具集合。
//
// 参数：
//   - skillBrief：来自 SkillSet 的技能简介文本，每行一个工具描述。
//   - defaultTools：兜底的默认工具列表。
//
// 返回：合并去重后的多行字符串（每行一个工具简介）。
// 副作用：无。
// 并发安全：纯函数，无共享状态。
func mergeToolList(skillBrief, defaultTools string) string {
	// skillBrief 为空时直接返回默认列表，避免空合并产生噪声
	if skillBrief == "" {
		return defaultTools
	}
	// seen 记录已收集的工具名，用于跨两个列表去重
	seen := make(map[string]bool)
	// out 收集去重后的工具描述行
	var out []string
	// 第一轮：遍历 skillBrief，技能简介优先保留
	for _, line := range strings.Split(skillBrief, "\n") {
		line = strings.TrimSpace(line) // 去除首尾空白
		if line == "" {
			continue // 跳过空行
		}
		name := toolNameFromLine(line) // 提取行首工具名
		if name == "" {
			continue // 无法识别工具名的行跳过
		}
		if seen[name] {
			continue // 同名工具已存在，跳过
		}
		seen[name] = true
		out = append(out, line) // 保留该行
	}
	// 第二轮：补充 defaultTools 中未出现过的工具
	for _, line := range strings.Split(defaultTools, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := toolNameFromLine(line)
		if name == "" || seen[name] {
			continue // 无名或已存在则跳过
		}
		seen[name] = true
		out = append(out, line)
	}
	// 用换行拼接为多行字符串返回
	return strings.Join(out, "\n")
}

// toolNameFromLine 从 "- WriteFile: 写入文件..." 中提取 "WriteFile"
//
// 职责：解析一行工具简介文本，提取行首的工具名。
// 参数：
//   - line：形如 "- WriteFile: 写入文件" 的工具描述行。
//
// 返回：工具名（如 "WriteFile"）；无法解析时返回空串。
// 副作用：无。
// 并发安全：纯函数。
func toolNameFromLine(line string) string {
	// 去除前导 "-" 与空白
	s := strings.TrimPrefix(strings.TrimSpace(line), "-")
	s = strings.TrimSpace(s)
	// 以第一个冒号为分隔，前半部分即工具名
	if idx := strings.Index(s, ":"); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	return "" // 无冒号则无法识别
}


