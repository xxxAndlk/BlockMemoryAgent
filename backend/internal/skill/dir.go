package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// conventionDirs 是主流 Agent 工具的项目级 skill 约定目录（按优先级排序）。
// 扫描顺序即优先级：同名技能先到先得（Claude Code > Codex > 通用 > Cursor > Gemini）。
// 根为进程 cwd（与 workDir 沙箱一致）；`.agent` 非规范目录，为用户约定保留。
var conventionDirs = []string{".claude", ".codex", ".agents", ".cursor", ".gemini", ".agent"}

// LoadFromDir 扫描 root 下各主流 Agent 工具约定目录中的 SKILL.md 并注册进池。
//
// 目录布局：<root>/<dir>/skills/<name>/SKILL.md。frontmatter 必须含
// name 与 description（缺失则跳过并记入 skipped）；allowed-tools 等
// 未知字段容忍忽略。Content 保存 SKILL.md 全文（渐进披露：由 load_skill
// 工具按需返回，不做截断），Path 记录文件绝对路径。
//
// 同名（frontmatter name）技能先到先得，后者跳过并记入 skipped。
// 返回：成功加载数量 + 跳过原因列表（含目录不存在时的说明）。
func (p *Pool) LoadFromDir(root string) (loaded int, skipped []string) {
	for _, dir := range conventionDirs {
		base := filepath.Join(root, dir, "skills")
		entries, err := os.ReadDir(base)
		if err != nil {
			// 目录不存在是该约定的常态（绝大多数项目只有 .claude），不算错误。
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(base, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s: 读 SKILL.md 失败: %v", e.Name(), err))
				continue
			}
			sk, reason := parseSkillMDFile(path, e.Name(), data)
			if reason != "" {
				skipped = append(skipped, e.Name()+": "+reason)
				continue
			}
			// 同名先到先得：约定目录顺序即优先级。
			if existing := p.FindByNameOrID(sk.Name); existing != nil {
				skipped = append(skipped, fmt.Sprintf("%s: 技能名 %q 与 %s 重复，忽略", e.Name(), sk.Name, dir))
				continue
			}
			p.Register(sk)
			loaded++
		}
	}
	return loaded, skipped
}

// parseSkillMDFile 解析单个 SKILL.md；失败时返回跳过原因（skill 为 nil）。
func parseSkillMDFile(path, dirName string, data []byte) (*types.Skill, string) {
	fm, body := textutil.ParseFrontmatter(data)
	name := strings.TrimSpace(fm["name"])
	desc := strings.TrimSpace(fm["description"])
	if name == "" {
		name = dirName
	}
	if desc == "" {
		// description 是渐进披露唯一进提示的信号，缺失则不可用。
		return nil, "缺少 description，跳过"
	}
	return &types.Skill{
		SkillID:  textutil.SanitizeID(dirName),
		Name:     name,
		Description: desc,
		Domain:   strings.TrimSpace(fm["domain"]),
		Source:   "dir",
		Path:     path,
		Content:  strings.TrimSpace(body),
	}, ""
}

// MetadataBlock 渲染 names 对应技能的元数据块（渐进披露第一层）。
//
// 输出形如：
//
//	【可用技能】
//	- pdf-extract: 提取PDF表格与文本
//	- db-migrate: 数据库迁移清单
//	用 load_skill(名称) 获取技能全文；派发子 Agent 时可用 skills 参数下放。
//
// names 按技能 Name（或 SkillID）匹配池内技能，未知项跳过；解析后按
// Name 字典序稳定排序。空集或池为 nil 时返回 ""。
func MetadataBlock(pool *Pool, names []string) string {
	if pool == nil || len(names) == 0 {
		return ""
	}
	type entry struct{ name, desc string }
	seen := map[string]struct{}{}
	var entries []entry
	for _, n := range names {
		s := pool.FindByNameOrID(n)
		if s == nil {
			continue
		}
		key := s.Name
		if key == "" {
			key = s.SkillID
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, entry{key, s.Description})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var b strings.Builder
	b.WriteString("【可用技能】\n")
	for _, e := range entries {
		b.WriteString("- ")
		b.WriteString(e.name)
		b.WriteString(": ")
		b.WriteString(e.desc)
		b.WriteByte('\n')
	}
	b.WriteString("用 load_skill(名称) 获取技能全文；派发子 Agent 时可用 skills 参数下放其中技能。")
	return b.String()
}
