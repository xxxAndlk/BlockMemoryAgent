package textutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillTool 技能配套工具条目（frontmatter tools 清单，TODO 25 阶段 C1）。
// Path 为相对技能目录的路径（scripts/xxx.py），Desc 是用途，Run 是运行命令。
type SkillTool struct {
	Path string `json:"path" yaml:"path"`
	Desc string `json:"desc" yaml:"desc"`
	Run  string `json:"run" yaml:"run"`
}

// RenderSkillFrontmatter 渲染经验技能文件头（name/title/when_to_use[/outcome] + 闭合围栏）。
// bootstrap/evolver、bootstrap/skill_consolidate、server/learned_skills 三处共用，
// 防 frontmatter 字段口径漂移。outcome 空串不写该行。
// 可选 tools 参数（C1）：非空时渲染 tools YAML list（{path, desc, run}），不传则行为不变。
func RenderSkillFrontmatter(name, title, whenToUse, outcome string, tools ...[]SkillTool) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "when_to_use: %s\n", whenToUse)
	if outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", outcome)
	}
	if len(tools) > 0 && len(tools[0]) > 0 {
		b.WriteString("tools:\n")
		for _, t := range tools[0] {
			fmt.Fprintf(&b, "  - path: %s\n", t.Path)
			fmt.Fprintf(&b, "    desc: %s\n", t.Desc)
			if t.Run != "" {
				fmt.Fprintf(&b, "    run: %s\n", t.Run)
			}
		}
	}
	b.WriteString("---\n")
	return b.String()
}

// ParseSkillTools 解析 SKILL.md frontmatter 的 tools 清单（YAML list of {path, desc, run}）。
// 缺 frontmatter/tools 或单项缺 path 时跳过该项；解析失败返回空清单（宽容读取）。
// tools 只进 frontmatter，不进正文步骤（C3 规则）。
func ParseSkillTools(data []byte) []SkillTool {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil
	}
	rest := text[4:]
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil
	}
	var raw struct {
		Tools []SkillTool `yaml:"tools"`
	}
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return nil
	}
	out := make([]SkillTool, 0, len(raw.Tools))
	for _, t := range raw.Tools {
		if strings.TrimSpace(t.Path) == "" {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SkillToolSection 渲染「配套工具」说明段（C2 加载注入）：每个脚本一行——
// 相对路径 + 运行命令 + 用途，追加进技能 Content，load_skill 取全文时 agent 可知怎么执行。
// 空清单返回空串。
func SkillToolSection(tools []SkillTool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 配套工具\n")
	for _, t := range tools {
		run := strings.TrimSpace(t.Run)
		if run == "" {
			fmt.Fprintf(&b, "- `%s`：%s\n", t.Path, strings.TrimSpace(t.Desc))
		} else {
			fmt.Fprintf(&b, "- `%s`：运行 `%s`——%s\n", t.Path, run, strings.TrimSpace(t.Desc))
		}
	}
	return b.String()
}

// ResolveSkillMDPath 经验技能文件路径解析（C1 目录式，向后兼容）：
// dir/<name>/SKILL.md 存在用之；否则回退 dir/<name>.md（老格式只读，不写回、不搬迁）；
// 两者都不存在时返回目录式路径（作为新建写目标）。所有读路径统一走本函数。
func ResolveSkillMDPath(dir, name string) string {
	dirPath := filepath.Join(dir, name, "SKILL.md")
	if _, err := os.Stat(dirPath); err == nil {
		return dirPath
	}
	flatPath := filepath.Join(dir, name+".md")
	if _, err := os.Stat(flatPath); err == nil {
		return flatPath
	}
	return dirPath
}

// ResolveSkillMDPathFromRef 从任意历史路径引用（dir/<name>.md 或 dir/<name>/SKILL.md）
// 解析当前生效路径；PG content_path 新老格式混存时统一归一到 ResolveSkillMDPath。
func ResolveSkillMDPathFromRef(refPath string) string {
	dir := filepath.Dir(refPath)
	base := filepath.Base(refPath)
	name := strings.TrimSuffix(base, ".md")
	if base == "SKILL.md" {
		name = filepath.Base(dir)
		dir = filepath.Dir(dir)
	}
	return ResolveSkillMDPath(dir, name)
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
