// Package project 维护工作目录级的 PROJECT.md 概览文档。
//
// 类比 CLAUDE.md 对 Claude Code 的作用：.bma/PROJECT.md 是当前 workDir 给 Agent
// 看的项目说明书。首个 session 启动时若缺失则启发式扫描生成（无 LLM），包含：
// 模块/语言/命令/推荐领域拆分（每顶层目录一领域，附文件数/语言/入口/子目录）/文档地图。
//
// 大改动后 MetaAgent/DomainAgent 显式调用 RefreshProjectDoc 工具重写 managed 区。
// 文件用 <!-- bma:managed begin/end --> 标记自动生成区，标记区外的人手补充保留。
package project

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ManagedBegin / ManagedEnd 标记自动生成区边界。RefreshProjectDoc 只重写标记区内部。
const (
	ManagedBegin = "<!-- bma:managed begin -->"
	ManagedEnd   = "<!-- bma:managed end -->"

	// maxScanFiles 单个领域目录扫描的文件数硬上限，防止超大仓库拖慢首 session。
	maxScanFiles = 50000

	// maxSubdirs / maxDocs 单领域子目录与文档地图的展示上限。
	maxSubdirs = 12
	maxDocs    = 30
	maxTargets = 20
)

// skipDirs 是扫描时整体跳过的目录名（依赖/产物/版本控制/IDE 等）。
var skipDirs = map[string]bool{
	".git": true, ".bma": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "tmp": true, "temp": true,
	".idea": true, ".vscode": true, "__pycache__": true, ".next": true,
	"target": true, ".cache": true, "out": true, "bin": true,
}

// domainPurpose 按顶层目录名猜测领域用途。未命中标"未知领域，待人工标注"。
var domainPurpose = map[string]string{
	"backend":       "后端服务",
	"server":        "后端服务",
	"api":           "后端服务",
	"service":       "后端服务",
	"services":      "后端服务",
	"frontend":      "前端界面",
	"web":           "前端界面",
	"ui":            "前端界面",
	"client":        "前端/客户端",
	"app":           "应用入口",
	"doc":           "文档",
	"docs":          "文档",
	"documentation": "文档",
	"test":          "测试",
	"tests":         "测试",
	"e2e":           "端到端测试",
	"spec":          "规格/测试用例",
	"specs":         "规格/测试用例",
	"config":        "配置",
	"configs":       "配置",
	"conf":          "配置",
	"scripts":       "脚本",
	"script":        "脚本",
	"tools":         "辅助工具",
	"cmd":           "命令入口",
	"internal":      "内部实现",
	"pkg":           "可复用包",
	"deploy":        "部署",
	"docker":        "容器化",
	"k8s":           "编排",
	"migrations":    "数据库迁移",
	"migration":     "数据库迁移",
	"assets":        "静态资源",
	"public":        "静态资源",
	"static":        "静态资源",
	"locales":       "国际化",
	"i18n":          "国际化",
}

// extLang 把文件扩展名映射到语言名（领域级语言探测用）。
var extLang = map[string]string{
	".go":      "Go",
	".ts":      "TypeScript",
	".tsx":     "TypeScript",
	".js":      "JavaScript",
	".jsx":     "JavaScript",
	".mjs":     "JavaScript",
	".py":      "Python",
	".rs":      "Rust",
	".java":    "Java",
	".kt":      "Kotlin",
	".rb":      "Ruby",
	".cs":      "C#",
	".cpp":     "C++",
	".cc":      "C++",
	".c":       "C",
	".vue":     "Vue",
	".svelte":  "Svelte",
	".php":     "PHP",
	".swift":   "Swift",
	".dart":    "Dart",
}

// ProjectDocPath 返回 <workDir>/.bma/PROJECT.md 的绝对路径。
func ProjectDocPath(workDir string) string {
	return filepath.Join(workDir, ".bma", "PROJECT.md")
}

// EnsureProjectDoc 保证 workDir 下存在 PROJECT.md。已存在则不动；缺失则启发式生成。
// 在首个 session 启动时调用，幂等。
func EnsureProjectDoc(workDir string) error {
	if workDir == "" {
		return fmt.Errorf("workDir is empty")
	}
	p := ProjectDocPath(workDir)
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir .bma: %w", err)
	}
	return writeFresh(workDir)
}

// writeFresh 首次生成 PROJECT.md：managed 区 + 人手补充提示脚注。
func writeFresh(workDir string) error {
	body, err := scanProject(workDir)
	if err != nil {
		return err
	}
	content := ManagedBegin + "\n" + body + "\n" + ManagedEnd + "\n\n" +
		"<!-- 标记区外可写人手补充；RefreshProjectDoc 只重写上方 managed 区，不覆盖本提示以下内容。 -->\n"
	return os.WriteFile(ProjectDocPath(workDir), []byte(content), 0o644)
}

// RefreshProjectDoc 重写 PROJECT.md 的 managed 区，保留标记区外的人手补充。
// 文件缺失时等价于 EnsureProjectDoc；无标记时把 managed 区前置，保留既有内容。
func RefreshProjectDoc(workDir string) error {
	if workDir == "" {
		return fmt.Errorf("workDir is empty")
	}
	body, err := scanProject(workDir)
	if err != nil {
		return err
	}
	p := ProjectDocPath(workDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mkdir .bma: %w", err)
	}
	existing, _ := os.ReadFile(p)
	content := rebuildManaged(string(existing), body)
	return os.WriteFile(p, []byte(content), 0o644)
}

// rebuildManaged 把 body 包进标记区，替换 existing 中原有标记区；无标记则前置。
func rebuildManaged(existing, body string) string {
	managedBlock := ManagedBegin + "\n" + body + "\n" + ManagedEnd
	if existing == "" {
		return managedBlock + "\n\n<!-- 标记区外可写人手补充；RefreshProjectDoc 只重写上方 managed 区。 -->\n"
	}
	startIdx := strings.Index(existing, ManagedBegin)
	endIdx := strings.Index(existing, ManagedEnd)
	if startIdx >= 0 && endIdx > startIdx {
		before := existing[:startIdx]
		after := existing[endIdx+len(ManagedEnd):]
		return before + managedBlock + after
	}
	return managedBlock + "\n\n" + existing
}

// LoadProjectDoc 读取 managed 区正文供注入系统提示词。缺失或无标记返回空串。
func LoadProjectDoc(workDir string) string {
	if workDir == "" {
		return ""
	}
	data, err := os.ReadFile(ProjectDocPath(workDir))
	if err != nil {
		return ""
	}
	s := string(data)
	startIdx := strings.Index(s, ManagedBegin)
	endIdx := strings.Index(s, ManagedEnd)
	if startIdx < 0 || endIdx <= startIdx {
		return ""
	}
	return strings.TrimSpace(s[startIdx+len(ManagedBegin):endIdx])
}

// domainInfo 是单个顶层领域目录的摘要。
type domainInfo struct {
	Name      string
	Purpose   string
	FileCount int
	Language  string
	Entry     string
	Subdirs   []string
}

// scanProject 启发式扫描 workDir，返回 managed 区正文（不含标记）。
func scanProject(workDir string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(workDir))
	if err != nil {
		abs = workDir
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("read workDir %s: %w", abs, err)
	}

	var b strings.Builder
	b.WriteString("# 项目概览\n\n")
	fmt.Fprintf(&b, "- 根目录: %s\n", abs)
	fmt.Fprintf(&b, "- 生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))

	module, lang, goVer := detectModule(abs)
	if module != "" {
		fmt.Fprintf(&b, "- 模块: %s\n", module)
	}
	if lang != "" {
		fmt.Fprintf(&b, "- 语言: %s\n", lang)
	}
	if goVer != "" {
		fmt.Fprintf(&b, "- Go 版本: %s\n", goVer)
	}

	if targets := detectMakeTargets(abs); len(targets) > 0 {
		b.WriteString("\n## 命令\n")
		b.WriteString("通过 Makefile 检测到以下目标（`make <target>`）：\n")
		for _, t := range targets {
			fmt.Fprintf(&b, "- `make %s`\n", t)
		}
	}

	domains := scanDomains(abs, entries)
	if len(domains) > 0 {
		b.WriteString("\n## 推荐领域拆分\n\n")
		b.WriteString("按顶层目录划分领域，详情如下。Agent 无明确领域归属时参考本表定位。\n\n")
		for _, d := range domains {
			fmt.Fprintf(&b, "### `%s/` - %s\n", d.Name, d.Purpose)
			fmt.Fprintf(&b, "- 文件数: %d\n", d.FileCount)
			if d.Language != "" {
				fmt.Fprintf(&b, "- 语言: %s\n", d.Language)
			}
			if d.Entry != "" {
				fmt.Fprintf(&b, "- 入口: %s\n", d.Entry)
			}
			if len(d.Subdirs) > 0 {
				fmt.Fprintf(&b, "- 子目录: %s\n", strings.Join(d.Subdirs, ", "))
			}
			b.WriteByte('\n')
		}
	}

	if docs := scanDocs(abs, entries); len(docs) > 0 {
		b.WriteString("## 文档地图\n")
		for _, d := range docs {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// scanDomains 遍历顶层目录，每个非跳过目录生成一条 domainInfo。
func scanDomains(root string, entries []os.DirEntry) []domainInfo {
	var out []domainInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if skipDirs[e.Name()] {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fc, lang, entry, subdirs := inspectDir(dir)
		purpose := domainPurpose[e.Name()]
		if purpose == "" {
			purpose = "（未知领域，待人工标注）"
		}
		out = append(out, domainInfo{
			Name:      e.Name(),
			Purpose:   purpose,
			FileCount: fc,
			Language:  lang,
			Entry:     entry,
			Subdirs:   subdirs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// inspectDir 统计目录文件数、语言、入口候选、一级子目录。
// 仅深入一级子目录（再深用 SkipDir 剪枝），文件数超 maxScanFiles 停止。
func inspectDir(dir string) (fileCount int, lang, entry string, subdirs []string) {
	extCounts := make(map[string]int)
	subSet := make(map[string]bool)
	entryCandidates := []string{"main.go", "index.ts", "index.js", "main.py", "app.py", "main.rs", "Main.java", "main.ts"}

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			// 仅保留一级子目录；更深层剪枝。
			if strings.Contains(rel, string(filepath.Separator)) {
				return filepath.SkipDir
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			subSet[rel] = true
			return nil
		}
		fileCount++
		if fileCount > maxScanFiles {
			return filepath.SkipAll
		}
		name := d.Name()
		if ext := strings.ToLower(filepath.Ext(name)); ext != "" {
			extCounts[ext]++
		}
		if entry == "" {
			if slices.Contains(entryCandidates, name) {
				rel, _ := filepath.Rel(dir, path)
				entry = rel
			}
		}
		return nil
	})

	lang = langFromExts(extCounts)
	for s := range subSet {
		subdirs = append(subdirs, s)
	}
	sort.Strings(subdirs)
	if len(subdirs) > maxSubdirs {
		subdirs = subdirs[:maxSubdirs]
	}
	return
}

// langFromExts 按扩展名计数选主导语言。同计数时按 extLang 迭代顺序不稳定，
// 故取计数最大者；全空返回空串。
func langFromExts(counts map[string]int) string {
	best := ""
	bestN := 0
	for ext, n := range counts {
		if l, ok := extLang[ext]; ok && n > bestN {
			best = l
			bestN = n
		}
	}
	return best
}

// detectModule 从 go.mod / package.json / Cargo.toml / pyproject.toml 探测模块名与语言。
func detectModule(root string) (module, lang, goVer string) {
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		lang = "Go"
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if m, ok := strings.CutPrefix(line, "module "); ok {
				module = strings.TrimSpace(m)
			} else if v, ok := strings.CutPrefix(line, "go "); ok {
				goVer = strings.TrimSpace(v)
			}
		}
		return
	}
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		lang = "JavaScript"
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			module = pkg.Name
		}
		// 探测是否含 TypeScript（tsconfig.json 同目录则标 TypeScript）。
		if _, err := os.Stat(filepath.Join(root, "tsconfig.json")); err == nil {
			lang = "TypeScript"
		}
		return
	}
	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		lang = "Rust"
		module = filepath.Base(root)
		return
	}
	if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err == nil {
		lang = "Python"
		module = filepath.Base(root)
		return
	}
	if _, err := os.Stat(filepath.Join(root, "requirements.txt")); err == nil {
		lang = "Python"
		module = filepath.Base(root)
		return
	}
	return
}

// detectMakeTargets 解析 Makefile 顶层 target（非变量赋值、非命令行）。
func detectMakeTargets(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		// 跳过变量赋值：VAR := / ?= / += / =
		if strings.ContainsAny(line, "?+") && strings.Contains(line, "=") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= maxTargets {
			break
		}
	}
	return out
}

// scanDocs 收集根目录 *.md 与 doc/ docs/ 一级 *.md，作为文档地图。
func scanDocs(root string, entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != "doc" && name != "docs" {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, s := range sub {
			if s.IsDir() {
				continue
			}
			if strings.HasSuffix(s.Name(), ".md") {
				out = append(out, name+"/"+s.Name())
			}
		}
	}
	sort.Strings(out)
	if len(out) > maxDocs {
		out = out[:maxDocs]
	}
	return out
}
