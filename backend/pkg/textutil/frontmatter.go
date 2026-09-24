package textutil

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// RenderSkillFrontmatter 渲染经验技能文件头（name/title/when_to_use[/outcome] + 闭合围栏）。
// bootstrap/evolver、bootstrap/skill_consolidate、server/learned_skills 三处共用，
// 防 frontmatter 字段口径漂移。outcome 空串不写该行。
func RenderSkillFrontmatter(name, title, whenToUse, outcome string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "when_to_use: %s\n", whenToUse)
	if outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", outcome)
	}
	b.WriteString("---\n")
	return b.String()
}

// ParseFrontmatter 解析 Markdown frontmatter（`---` 围栏的 YAML 块）。
//
// 顶层标量映射为 map[string]string；字符串数组（如 tags: [a, b]）
// 折叠为逗号分隔串。无 frontmatter 或围栏未闭合时返回 nil map 与全文。
// 未知字段由调用方自行忽略。
func ParseFrontmatter(data []byte) (map[string]string, string) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, text
	}
	rest := text[4:]
	block, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, text
	}
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r\n")
	m := map[string]string{}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(block), &raw); err == nil {
		for k, v := range raw {
			switch tv := v.(type) {
			case string:
				m[k] = tv
			case []any:
				parts := make([]string, 0, len(tv))
				for _, e := range tv {
					if str, ok := e.(string); ok {
						parts = append(parts, str)
					}
				}
				m[k] = strings.Join(parts, ",")
			}
		}
	}
	return m, body
}

// SanitizeID 把任意目录/技能名规整为 ID 约定（小写字母数字下划线）。
func SanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
