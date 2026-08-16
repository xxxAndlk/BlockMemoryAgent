// Package bundle 实现外部插件目录包加载（设计文档 §3.3、§7 生态兼容）。
//
// 兼容 Claude Code / Codex 插件包格式：
//   - .claude-plugin/plugin.json → Manifest 元数据；
//   - .mcp.json → 展开为若干 mcp 插件（支持 ${CLAUDE_PLUGIN_ROOT} 路径替换，
//     并自动注入 CLAUDE_PLUGIN_ROOT 环境变量，与 Claude Code 行为一致）；
//   - skills/*/SKILL.md → frontmatter 解析，注入现有 skill.Pool。
//
// hooks/hooks.json 暂不支持（事件模型为 Claude Code 专有）。
//
// 目录约定：外部插件包目录丢进 plugins.d/ 即被 Scan 发现，经管理 API 显式
// Reload 装载（不引入 fsnotify）。
package bundle

import (
	"fmt"     // 错误包装
	"os"      // 文件读取
	"path/filepath" // 路径拼接
	"sort"    // 稳定输出
	"strings" // 字符串处理

	"github.com/blockmemory/agent/backend/pkg/types"
	"gopkg.in/yaml.v3" // SKILL.md frontmatter 解析
)

// MCPServer 是 .mcp.json 中一个 MCP server 的描述。
type MCPServer struct {
	// Name server 名（插件内唯一，构成展开插件 ID 的一部分）。
	Name string
	// Command 启动命令（stdio 传输）。
	Command string
	// Args 启动参数。
	Args []string
	// Env 附加环境变量（已做 ${CLAUDE_PLUGIN_ROOT} 替换并注入该变量本身）。
	Env map[string]string
	// URL streamable HTTP 端点；非空时走 http 传输。
	URL string
}

// Bundle 是一个插件目录包展开后的结果。
type Bundle struct {
	// Dir 目录名（展开插件 ID 组成部分："bundle/<Dir>/<server>"）。
	Dir string
	// Name 展示名（plugin.json name，缺省取目录名）。
	Name string
	// Version 版本号（plugin.json version，可空）。
	Version string
	// Description 一句话描述（plugin.json description，可空）。
	Description string
	// Servers .mcp.json 展开出的 MCP server 列表。
	Servers []MCPServer
	// Skills skills/*/SKILL.md 解析出的 Skill 条目。
	Skills []*types.Skill
}

// mcpServersFile 对应 .mcp.json 的根结构。
type mcpServersFile struct {
	Servers map[string]mcpServerEntry `yaml:"mcpServers"`
}

// mcpServerEntry 对应 .mcp.json 中单个 server 条目。
type mcpServerEntry struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
}

// pluginJSON 对应 .claude-plugin/plugin.json 的元数据子集。
type pluginJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// Scan 扫描 plugins.d/ 目录下的插件包目录，返回 Bundle 列表与逐目录警告。
// 目录判定：含 .claude-plugin/plugin.json 或 .mcp.json 才算插件包，否则跳过。
// 输出按目录名排序，保证 Reload 差异比对稳定。
func Scan(dir string) ([]*Bundle, []error) {
	var bundles []*Bundle
	var warnings []error
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // plugins.d/ 未创建 = 无外部插件
		}
		return nil, []error{fmt.Errorf("read plugins.d %s: %w", dir, err)}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := loadBundle(filepath.Join(dir, name), name)
		if err != nil {
			if err != errNotABundle {
				warnings = append(warnings, fmt.Errorf("bundle %s: %w", name, err))
			}
			continue
		}
		bundles = append(bundles, b)
	}
	return bundles, warnings
}

// errNotABundle 表示目录不是插件包（无 plugin.json/.mcp.json）：Scan 静默跳过。
var errNotABundle = fmt.Errorf("缺少 .claude-plugin/plugin.json 或 .mcp.json，不是插件包")

// loadBundle 加载单个插件包目录。
func loadBundle(dir, dirName string) (*Bundle, error) {
	b := &Bundle{Dir: dirName, Name: dirName}

	// 1. .claude-plugin/plugin.json → 元数据（可选）。
	pluginPath := filepath.Join(dir, ".claude-plugin", "plugin.json")
	if data, err := os.ReadFile(pluginPath); err == nil {
		var pj pluginJSON
		if err := yaml.Unmarshal(data, &pj); err != nil {
			return nil, fmt.Errorf("parse plugin.json: %w", err)
		}
		b.Name = firstNonEmpty(pj.Name, dirName)
		b.Version = pj.Version
		b.Description = pj.Description
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read plugin.json: %w", err)
	}

	// 2. .mcp.json → MCP server 展开（可选；两文件皆无则不是插件包）。
	mcpPath := filepath.Join(dir, ".mcp.json")
	hasPlugin := false
	if data, err := os.ReadFile(mcpPath); err == nil {
		var mf mcpServersFile
		if err := yaml.Unmarshal(data, &mf); err != nil {
			return nil, fmt.Errorf("parse .mcp.json: %w", err)
		}
		hasPlugin = true
		names := make([]string, 0, len(mf.Servers))
		for name := range mf.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			se := mf.Servers[name]
			server := MCPServer{
				Name:    name,
				Command: expandRoot(se.Command, dir),
				URL:     expandRoot(se.URL, dir),
				Env:     map[string]string{"CLAUDE_PLUGIN_ROOT": dir},
			}
			for _, a := range se.Args {
				server.Args = append(server.Args, expandRoot(a, dir))
			}
			for k, v := range se.Env {
				server.Env[k] = expandRoot(v, dir)
			}
			b.Servers = append(b.Servers, server)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read .mcp.json: %w", err)
	}

	// 3. skills/*/SKILL.md → skill 条目（可选）。
	// Claude 约定技能名即子目录名（skills/<name>/SKILL.md）；直接平铺的 *.md 也兼容。
	skillsDir := filepath.Join(dir, "skills")
	if skillEntries, err := os.ReadDir(skillsDir); err == nil {
		for _, e := range skillEntries {
			var skillPath string
			if e.IsDir() {
				skillPath = filepath.Join(skillsDir, e.Name(), "SKILL.md")
				if _, err := os.Stat(skillPath); err != nil {
					continue
				}
			} else if strings.HasSuffix(e.Name(), ".md") {
				skillPath = filepath.Join(skillsDir, e.Name())
			} else {
				continue
			}
			s, err := parseSkillMD(skillPath, dirName)
			if err != nil {
				return nil, fmt.Errorf("parse skill %s: %w", skillPath, err)
			}
			if s != nil {
				b.Skills = append(b.Skills, s)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	if !hasPlugin {
		// plugin.json 存在也算插件包（纯 skill 包）。
		if _, err := os.Stat(pluginPath); err != nil {
			return nil, errNotABundle
		}
	}
	return b, nil
}

// parseSkillMD 解析 skills/*/SKILL.md：
// frontmatter（name/description/domain/tags）映射为 Skill 元数据，
// 正文作为 UsageExample（截断上限，仅 LLM 选择失败时作为 few-shot 使用）。
func parseSkillMD(path, dirName string) (*types.Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body := parseFrontmatter(data)
	name := strings.TrimSpace(fm["name"])
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	s := &types.Skill{
		// SkillID 全局唯一：目录名 + 技能名（防不同插件包同名技能互相覆盖）。
		SkillID:     sanitizeID(dirName + "_" + name),
		Name:        name,
		Description: strings.TrimSpace(fm["description"]),
		Domain:      strings.TrimSpace(fm["domain"]),
		ToolRef:     strings.TrimSpace(fm["tool_ref"]),
	}
	if tags, ok := fm["tags"]; ok {
		for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ',' || r == ';' }) {
			if t = strings.TrimSpace(t); t != "" {
				s.Tags = append(s.Tags, t)
			}
		}
	}
	// 正文压缩为 few-shot 示例：保留开头，截断过长正文。
	body = strings.TrimSpace(body)
	if len(body) > 4000 {
		body = body[:4000] + "\n…(截断)"
	}
	s.UsageExample = body
	return s, nil
}

// parseFrontmatter 解析 Markdown frontmatter（--- 围栏的 YAML 块）。
// 无 frontmatter 时返回空 map 与全文。
func parseFrontmatter(data []byte) (map[string]string, string) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, text
	}
	rest := text[4:]
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		block := rest[:idx]
		body := rest[idx+4:]
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
	return nil, text
}

// expandRoot 替换 ${CLAUDE_PLUGIN_ROOT} 占位符为插件包绝对路径。
func expandRoot(s, dir string) string {
	if s == "" {
		return s
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", abs)
}

// sanitizeID 把任意目录/技能名规整为 SkillID 约定（小写字母数字下划线）。
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// firstNonEmpty 返回第一个非空字符串；全空返回空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
