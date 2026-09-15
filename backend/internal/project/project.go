// Package project 维护工作目录级的 PROJECT.md 概览文档。
//
// 类比 CLAUDE.md 对 Claude Code 的作用：.bma/PROJECT.md 是当前 workDir 给 Agent
// 看的项目说明书。首个 session 启动时若缺失则启发式扫描生成（无 LLM），包含：
// 模块/语言/命令/推荐领域拆分（按依赖关系聚类，无依赖信号的孤立文件按顶层目录兜底）/文档地图。
//
// 领域拆分按实体依赖关系（HTML script/link、JS import/require、Go import、Python import、
// CSS @import）连通分量聚类，而非顶层文件夹。簇命名由 LLM 在 RefreshProjectDoc 工具
// （MetaAgent 侧）给出语义名；boot 的 EnsureProjectDoc 用启发式兜底（domainPurpose 提示
// 或锚点文件名），永不留"待人工标注"空标注。
//
// 大改动后 MetaAgent/DomainAgent 显式调用 RefreshProjectDoc 工具重写 managed 区。
// 文件用 <!-- bma:managed begin/end --> 标记自动生成区，标记区外的人手补充保留。
package project

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ManagedBegin / ManagedEnd 标记自动生成区边界。RefreshProjectDoc 只重写标记区内部。
const (
	ManagedBegin = "<!-- bma:managed begin -->"
	ManagedEnd   = "<!-- bma:managed end -->"

	// maxScanFiles 单次扫描的文件数硬上限，防止超大仓库拖慢首 session。
	maxScanFiles = 50000

	// maxDocs 文档地图的展示上限。
	maxDocs    = 30
	maxTargets = 20

	// maxDepFileBytes 单文件依赖解析读取的内容上限，防超大文件拖慢。
	maxDepFileBytes = 1 << 20
	// maxDepNodes 参与依赖解析的节点数上限，超过则跳过 dep 解析退文件夹兜底。
	maxDepNodes = 5000
)

// skipDirs 是扫描时整体跳过的目录名（依赖/产物/版本控制/IDE/运行时输出等）。
var skipDirs = map[string]bool{
	".git": true, ".bma": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "tmp": true, "temp": true,
	".idea": true, ".vscode": true, "__pycache__": true, ".next": true,
	"target": true, ".cache": true, "out": true, "bin": true,
	"logs": true, ".workbuddy": true, "screenshots": true,
	".turbo": true, "coverage": true, ".parcel-cache": true, ".claude": true,
	".opencode": true,
}

// domainPurpose 按顶层目录名猜测领域用途，作启发式命名提示（非聚类依据）。
// 簇主导顶层目录命中本表时用对应 purpose；未命中走锚点文件名兜底，不再标"待人工标注"。
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
	".go":     "Go",
	".ts":     "TypeScript",
	".tsx":    "TypeScript",
	".js":     "JavaScript",
	".jsx":    "JavaScript",
	".mjs":    "JavaScript",
	".py":     "Python",
	".rs":     "Rust",
	".java":   "Java",
	".kt":     "Kotlin",
	".rb":     "Ruby",
	".cs":     "C#",
	".cpp":    "C++",
	".cc":     "C++",
	".c":      "C",
	".vue":    "Vue",
	".svelte": "Svelte",
	".php":    "PHP",
	".swift":  "Swift",
	".dart":   "Dart",
	".html":   "HTML",
	".htm":    "HTML",
	".css":    "CSS",
	".scss":   "SCSS",
}

// sourceExts 是参与依赖图聚类的源码/入口文件扩展名。
var sourceExts = map[string]bool{
	".html": true, ".htm": true,
	".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true,
	".go": true, ".py": true, ".rs": true, ".java": true, ".kt": true,
	".rb": true, ".cs": true, ".c": true, ".cpp": true, ".cc": true,
	".vue": true, ".svelte": true, ".php": true,
	".css": true, ".scss": true,
}

// rootMetaNames 是根级 meta 文件名（不参与聚类，避免 go.mod/Makefile 等冒充领域）。
var rootMetaNames = map[string]bool{
	"makefile": true, "dockerfile": true, "package.json": true, "package-lock.json": true,
	"go.sum": true, "go.mod": true, "tsconfig.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "cargo.toml": true, "pyproject.toml": true,
	"requirements.txt": true, ".env": true,
}

// entryCandidates 是簇内入口/锚点文件名候选，用于选锚点与展示入口。
var entryCandidates = map[string]bool{
	"index.html": true, "index.htm": true,
	"main.go": true, "index.ts": true, "index.js": true, "main.ts": true,
	"main.py": true, "app.py": true, "main.rs": true, "main.java": true,
	"index.vue": true,
}

// tryExts 是无扩展名引用解析时尝试追加的扩展名顺序。
var tryExts = []string{".js", ".mjs", ".ts", ".tsx", ".jsx", ".vue", ".svelte", ".css", ".scss", ".html"}

// DomainCluster 是一个依赖聚类簇，供 nil-cls 兜底路径使用（clusterByDeps 产出）。
type DomainCluster struct {
	Files          []string `json:"files"`
	Anchor         string   `json:"anchor"`
	DominantTopDir string   `json:"-"` // 启发式命名提示，不进 LLM prompt
	Lang           string   `json:"lang"`
}

// DomainPartition 是 LLM 返回的单个领域分区：语义名 + 职责 + 归属文件列表。
// LLM 按文件职责/实体把文件分组，跨文件夹亦可同域。
type DomainPartition struct {
	Name    string   `json:"name"`
	Purpose string   `json:"purpose"`
	Files   []string `json:"files"`
}

// DomainClassifier 用轻量 LLM 读文件内容按职责/实体把文件分区成领域。
// 实现方负责读文件样本、构造 prompt、调用模型；调用方（RefreshProjectDoc/EnsureProjectDoc）
// 负责失败/未覆盖文件回退到依赖图启发式兜底。nil 表示走启发式（boot/测试路径）。
type DomainClassifier interface {
	Partition(ctx context.Context, root string, files []string) ([]DomainPartition, error)
}

// ParsePartitionJSON 解析 LLM 返回的领域分区 JSON 数组。
// 兼容纯数组、markdown 围栏包裹、含前后解释文本（取首个 JSON 数组片段）。
func ParsePartitionJSON(s string) ([]DomainPartition, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty response")
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimPrefix(s, "\n")
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found in response")
	}
	arr := s[start : end+1]
	var parts []DomainPartition
	if err := json.Unmarshal([]byte(arr), &parts); err != nil {
		return nil, fmt.Errorf("unmarshal partitions: %w", err)
	}
	out := parts[:0]
	for _, p := range parts {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			continue
		}
		var clean []string
		for _, f := range p.Files {
			f = strings.TrimSpace(f)
			if f != "" {
				clean = append(clean, f)
			}
		}
		p.Files = clean
		out = append(out, p)
	}
	return out, nil
}

// ProjectDocPath 返回 <workDir>/.bma/PROJECT.md 的绝对路径。
func ProjectDocPath(workDir string) string {
	return filepath.Join(workDir, ".bma", "PROJECT.md")
}

// EnsureProjectDoc 保证 workDir 下存在 PROJECT.md。已存在则不动；缺失则生成。
// 在首个 session 启动时调用，幂等。cls 非 nil 时调 LLM 按职责分区，失败回退启发式；
// cls nil 走启发式（依赖图聚类 + 文件夹兜底）。
func EnsureProjectDoc(ctx context.Context, workDir string, cls DomainClassifier) error {
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
	return writeFresh(ctx, workDir, cls)
}

// writeFresh 首次生成 PROJECT.md：managed 区 + 人手补充提示脚注。
func writeFresh(ctx context.Context, workDir string, cls DomainClassifier) error {
	body, domains, err := scanProject(ctx, workDir, cls)
	if err != nil {
		return err
	}
	seedDomainProfiles(ctx, workDir, domains)
	content := ManagedBegin + "\n" + body + "\n" + ManagedEnd + "\n\n" +
		"<!-- 标记区外可写人手补充；RefreshProjectDoc 只重写上方 managed 区，不覆盖本提示以下内容。 -->\n"
	return os.WriteFile(ProjectDocPath(workDir), []byte(content), 0o644)
}

// RefreshProjectDoc 重写 PROJECT.md 的 managed 区，保留标记区外的人手补充。
// 文件缺失时等价于 EnsureProjectDoc；无标记时把 managed 区前置，保留既有内容。
// cls 非 nil 时调 LLM 给簇语义命名，失败回退启发式；cls nil（或 EnsureProjectDoc 路径）走启发式。
func RefreshProjectDoc(ctx context.Context, workDir string, cls DomainClassifier) error {
	if workDir == "" {
		return fmt.Errorf("workDir is empty")
	}
	body, domains, err := scanProject(ctx, workDir, cls)
	if err != nil {
		return err
	}
	seedDomainProfiles(ctx, workDir, domains)
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

// DomainSeed 是 PROJECT.md 生成/刷新后产生的领域档案种子（TODO #17 T26）：
// 领域分区名 + 职责描述 + 影响文件清单（工作目录相对路径）。
type DomainSeed struct {
	Name    string
	Purpose string
	Files   []string
}

// domainSeedHook 领域档案种子回调（TODO #17 T26）：EnsureProjectDoc/RefreshProjectDoc
// 成功产生领域分区后调用，bootstrap 接 KnowledgeStore.UpsertDomainProfile（source=project_md），
// 把项目结构里解析出的领域预先登记进领域注册表（跨场景冷复用的冷启动种子）。
// 包级钩子是刻意取舍：Ensure/Refresh 有 4 个调用点（agent 层×2 + tool 层×2），
// 逐层穿针会污染 tool.Executor 与 agent service 的构造签名；project 是底层叶子包，
// 钩子由 bootstrap 装配期一次性设置，运行期不可变。
var domainSeedHook func(ctx context.Context, workDir string, seeds []DomainSeed)

// SetDomainSeedHook 设置领域档案种子回调（TODO #17 T26）；nil 关闭。装配期调用一次。
func SetDomainSeedHook(fn func(ctx context.Context, workDir string, seeds []DomainSeed)) {
	domainSeedHook = fn
}

// seedDomainProfiles 在 PROJECT.md 生成/刷新成功后触发种子回调（best-effort：回调
// 内部自行容错；分区为空或未接线时零开销）。
func seedDomainProfiles(ctx context.Context, workDir string, domains []domainInfo) {
	if domainSeedHook == nil || len(domains) == 0 {
		return
	}
	seeds := make([]DomainSeed, 0, len(domains))
	for _, d := range domains {
		if strings.TrimSpace(d.Name) == "" {
			continue
		}
		seeds = append(seeds, DomainSeed{Name: d.Name, Purpose: d.Purpose, Files: d.Files})
	}
	if len(seeds) == 0 {
		return
	}
	domainSeedHook(ctx, workDir, seeds)
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
	return strings.TrimSpace(s[startIdx+len(ManagedBegin) : endIdx])
}

// domainInfo 是单个领域簇的摘要（managed 区展示用）。
type domainInfo struct {
	Name      string
	Purpose   string
	FileCount int
	Files     []string // 影响文件（相对路径，已排序）
	Dirs      []string // 影响目录（顶层目录去重排序，根级标 "(根级)"）
	Language  string
	Entry     string
}

// scanProject 启发式扫描 workDir，返回 managed 区正文（不含标记）与领域分区列表
//（TODO #17 T26 领域档案种子导入用）。
func scanProject(ctx context.Context, workDir string, cls DomainClassifier) (string, []domainInfo, error) {
	abs, err := filepath.Abs(filepath.Clean(workDir))
	if err != nil {
		abs = workDir
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, fmt.Errorf("read workDir %s: %w", abs, err)
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

	domains := scanDomains(ctx, abs, cls)
	if len(domains) > 0 {
		b.WriteString("\n## 推荐领域拆分\n\n")
		b.WriteString("按文件职责/实体由 LLM 分区（无 LLM 时按依赖图聚类兜底）。Agent 无明确领域归属时参考本表定位。\n")
		b.WriteString("源码文件后括注总行数与顶层符号（`名称 L行号`，`()` 后缀为函数），可直接按符号带 offset 精读，跳过逐页扫描。\n\n")
		for _, d := range domains {
			fmt.Fprintf(&b, "### `%s` - %s\n", d.Name, d.Purpose)
			if len(d.Dirs) > 0 {
				fmt.Fprintf(&b, "- 影响目录: %s\n", strings.Join(d.Dirs, ", "))
			}
			if len(d.Files) > 0 {
				fmt.Fprintf(&b, "- 影响文件 (%d):\n", len(d.Files))
				if len(d.Files) > 50 {
					for _, f := range d.Files[:20] {
						fmt.Fprintf(&b, "  - %s\n", f)
					}
					fmt.Fprintf(&b, "  ...(+%d more)\n", len(d.Files)-20)
				} else {
					for _, f := range d.Files {
						fmt.Fprintf(&b, "  - %s%s\n", f, fileOutline(abs, f))
					}
				}
			} else {
				fmt.Fprintf(&b, "- 文件数: %d\n", d.FileCount)
			}
			if d.Language != "" {
				fmt.Fprintf(&b, "- 语言: %s\n", d.Language)
			}
			if d.Entry != "" {
				fmt.Fprintf(&b, "- 入口: %s\n", d.Entry)
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

	return strings.TrimRight(b.String(), "\n"), domains, nil
}

// outlineExts 是需要抽取符号轮廓的源码扩展名（其余文件只列路径与行数意义不大，不注）。
var outlineExts = map[string]bool{
	".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true,
	".go": true, ".py": true, ".vue": true, ".svelte": true,
}

// outlineRule 是一条顶层符号抽取规则：近行首锚定正则 + 展示前缀/后缀。
// 不解析语法树：允许 0-3 列缩进（兼容 `(function(){...})()` IIFE 包裹一层的 JS 风格），
// 更深层级的类方法/局部变量天然跳过，只收近顶层符号。
type outlineRule struct {
	re     *regexp.Regexp
	prefix string
	suffix string
}

var outlineRules = []outlineRule{
	{regexp.MustCompile(`^[ \t]{0,3}(?:export\s+)?(?:default\s+)?class\s+([A-Za-z_$][\w$]*)`), "class ", ""}, // JS/TS class
	{regexp.MustCompile(`^[ \t]{0,3}(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), "", "()"}, // JS/TS function
	{regexp.MustCompile(`^[ \t]{0,3}(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=`), "", ""},     // JS/TS 顶层变量（含 CONFIG 等配置对象与箭头函数）
	{regexp.MustCompile(`^[ \t]{0,3}window\.([A-Za-z_$][\w$]*)\s*=`), "window.", ""},                        // 浏览器全局暴露
	{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][\w]*)`), "", "()"},                             // Go func / method
	{regexp.MustCompile(`^type\s+([A-Za-z_][\w]*)\s+`), "type ", ""},                                        // Go type
	{regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_][\w]*)`), "", "()"},                                  // Python def
	{regexp.MustCompile(`^class\s+([A-Za-z_][\w]*)`), "class ", ""},                                         // Python 裸 class
}

const (
	// maxOutlineFileBytes 单文件轮廓抽取的读取上限，防超大/压缩文件拖慢扫描。
	maxOutlineFileBytes = 1 << 20
	// maxOutlineSymbols 单文件展示符号数上限，超出截断，防概览膨胀（注入每个 Agent 系统提示词）。
	maxOutlineSymbols = 12
)

// fileOutline 是 FileOutline 的项目根 + 相对路径包装，供领域文件列表括注使用。
func fileOutline(absRoot, rel string) string {
	return FileOutline(filepath.Join(absRoot, filepath.FromSlash(rel)))
}

// FileOutline 读取单个源码文件并返回紧凑轮廓后缀：" (355 行): class Tower L15, fire() L120"。
// 非轮廓扩展名返回空串；读失败/无符号时至少给出行数（拿不到行数才返回空串）。
// best-effort：行首正则抽取，供 Agent 按符号带 offset 精读，消灭"找结构"式逐页盲扫。
// 单文件粒度导出：除 PROJECT.md 领域括注外，任务级文件小地图（tool 包触碰文件追踪器）
// 也复用同一抽取逻辑。
func FileOutline(absPath string) string {
	if !outlineExts[strings.ToLower(filepath.Ext(absPath))] {
		return ""
	}
	f, err := os.Open(absPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var symbols []string
	seen := map[string]bool{}
	lineNo := 0
	sc := bufio.NewScanner(io.LimitReader(f, maxOutlineFileBytes))
	sc.Buffer(make([]byte, 0, 64*1024), maxOutlineFileBytes)
	for sc.Scan() {
		lineNo++
		// 符号到上限后仍继续扫描：行数统计需要完整遍历（只多付正则匹配，best-effort）。
		if len(symbols) >= maxOutlineSymbols {
			continue
		}
		line := sc.Text()
		for _, rule := range outlineRules {
			m := rule.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if !seen[m[1]] {
				seen[m[1]] = true
				symbols = append(symbols, fmt.Sprintf("%s%s%s L%d", rule.prefix, m[1], rule.suffix, lineNo))
			}
			break // 一行只取首个命中规则，避免 class 行被多条规则重复消费
		}
	}
	if lineNo == 0 {
		return ""
	}
	if len(symbols) == 0 {
		return fmt.Sprintf(" (%d 行)", lineNo)
	}
	return fmt.Sprintf(" (%d 行): %s", lineNo, strings.Join(symbols, ", "))
}

// scanDomains 划分领域，返领域摘要列表（按 Name 排序，确定性）。永不留空标注。
// cls 非 nil 时调 LLM 按文件职责/实体分区；LLM 失败/空/未覆盖文件回退依赖图启发式兜底。
// cls nil 走启发式（clusterByDeps + 文件夹兜底）。
func scanDomains(ctx context.Context, root string, cls DomainClassifier) []domainInfo {
	nodes, _ := enumerateNodes(root)
	files := make([]string, len(nodes))
	for i, n := range nodes {
		files[i] = n.rel
	}

	if cls != nil && len(files) > 0 {
		parts, err := cls.Partition(ctx, root, files)
		if err == nil && len(parts) > 0 {
			return mergePartitionsAndFallback(parts, files, root)
		}
	}

	clusters := clusterByDeps(root)
	out := make([]domainInfo, 0, len(clusters))
	for _, c := range clusters {
		name, purpose := heuristicName(c)
		out = append(out, makeDomainInfo(name, purpose, c.Files))
	}
	sortByDomainName(out)
	return out
}

// mergePartitionsAndFallback 把 LLM 分区转 domainInfo，未覆盖文件走 clusterByDeps 兜底补齐。
func mergePartitionsAndFallback(parts []DomainPartition, allFiles []string, root string) []domainInfo {
	nodeSet := map[string]bool{}
	for _, f := range allFiles {
		nodeSet[f] = true
	}
	covered := map[string]bool{}
	out := make([]domainInfo, 0, len(parts))
	for _, p := range parts {
		var valid []string
		for _, f := range p.Files {
			if !nodeSet[f] || covered[f] {
				continue // 忽略 LLM 臆造路径与重复归集（取首个）
			}
			covered[f] = true
			valid = append(valid, f)
		}
		if p.Name == "" || len(valid) == 0 {
			continue
		}
		out = append(out, makeDomainInfo(p.Name, p.Purpose, valid))
	}
	// 未覆盖文件走依赖图启发式兜底（按文件夹/依赖簇合并），不丢任何文件。
	if len(covered) < len(allFiles) {
		for _, c := range clusterByDeps(root) {
			var uncovered []string
			for _, f := range c.Files {
				if !covered[f] {
					uncovered = append(uncovered, f)
					covered[f] = true
				}
			}
			if len(uncovered) > 0 {
				name, purpose := heuristicNameForFiles(uncovered)
				out = append(out, makeDomainInfo(name, purpose, uncovered))
			}
		}
	}
	sortByDomainName(out)
	return out
}

// sortByDomainName 按 Name 排序领域摘要（确定性）。
func sortByDomainName(out []domainInfo) {
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
}

// makeDomainInfo 把领域名+职责+文件列表构造成 domainInfo，计算影响目录/语言/入口。
func makeDomainInfo(name, purpose string, files []string) domainInfo {
	sort.Strings(files)
	return domainInfo{
		Name:      name,
		Purpose:   purpose,
		FileCount: len(files),
		Files:     files,
		Dirs:      dirsFromFiles(files),
		Language:  langFromFiles(files),
		Entry:     entryFromFiles(files),
	}
}

// dirsFromFiles 返回文件列表的影响目录（顶层目录去重排序，根级标 "(根级)"）。
func dirsFromFiles(files []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		td := topSeg(f)
		label := td
		if td == "" {
			label = "(根级)"
		}
		if !seen[label] {
			seen[label] = true
			dirs = append(dirs, label)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// langFromFiles 按文件扩展名计数选主导语言。
func langFromFiles(files []string) string {
	counts := map[string]int{}
	for _, f := range files {
		if ext := strings.ToLower(filepath.Ext(f)); ext != "" {
			counts[ext]++
		}
	}
	return langFromExts(counts)
}

// entryFromFiles 返回文件列表中的入口/锚点文件（首个 entryCandidates 命中），无则空。
func entryFromFiles(files []string) string {
	for _, f := range files {
		if entryCandidates[filepath.Base(f)] {
			return f
		}
	}
	return ""
}

// heuristicName 单簇启发式命名（委托 heuristicNameForFiles）。永不返回空 Name。
func heuristicName(c DomainCluster) (name, purpose string) {
	return heuristicNameForFiles(c.Files)
}

// heuristicNameForFiles 按文件列表启发式命名：主导顶层目录命中 domainPurpose 则用之，否则锚点文件名。
// 永不返回空 Name。
func heuristicNameForFiles(files []string) (name, purpose string) {
	topCount := map[string]int{}
	for _, f := range files {
		if td := topSeg(f); td != "" {
			topCount[td]++
		}
	}
	dom := ""
	bestN := 0
	for td, n := range topCount {
		if n > bestN {
			bestN = n
			dom = td
		}
	}
	if dom != "" {
		if p, ok := domainPurpose[dom]; ok && p != "" {
			return dom + "/", p
		}
	}
	anchor := ""
	for _, f := range files {
		if entryCandidates[filepath.Base(f)] {
			anchor = f
			break
		}
	}
	if anchor == "" && len(files) > 0 {
		anchor = files[0]
	}
	if anchor == "" {
		return "未分类", "孤立文件"
	}
	return anchor, "入口聚合（" + filepath.Base(anchor) + "）"
}

// fileNode 是依赖图节点。
type fileNode struct {
	rel    string // 相对 workDir 的 posix 路径
	topDir string // 顶层目录名（"" 表示根级）
	ext    string
	base   string
}

// edge 是无向依赖边。
type edge struct{ a, b string }

// clusterByDeps 扫描 root 建依赖图，连通分量 -> 簇；无边单例按顶层目录兜底合并。
func clusterByDeps(root string) []DomainCluster {
	nodes, nodeSet := enumerateNodes(root)
	uf := newUnionFind()
	for _, n := range nodes {
		uf.find(n.rel)
	}
	if len(nodes) <= maxDepNodes {
		for _, e := range extractEdges(root, nodes, nodeSet) {
			uf.union(e.a, e.b)
		}
	}
	groups := map[string][]fileNode{}
	for _, n := range nodes {
		r := uf.find(n.rel)
		groups[r] = append(groups[r], n)
	}

	var clusters []DomainCluster
	singleByTop := map[string][]fileNode{}
	for _, grp := range groups {
		if len(grp) >= 2 {
			clusters = append(clusters, makeCluster(grp))
			continue
		}
		f := grp[0]
		singleByTop[f.topDir] = append(singleByTop[f.topDir], f)
	}
	// 无边单例按顶层目录兜底合并（根级单例各成一簇）。
	for topDir, files := range singleByTop {
		if topDir == "" {
			for _, f := range files {
				clusters = append(clusters, makeCluster([]fileNode{f}))
			}
			continue
		}
		clusters = append(clusters, makeCluster(files))
	}

	sort.Slice(clusters, func(i, j int) bool {
		if len(clusters[i].Files) == 0 {
			return true
		}
		if len(clusters[j].Files) == 0 {
			return false
		}
		return clusters[i].Files[0] < clusters[j].Files[0]
	})
	return clusters
}

// makeCluster 把一组节点构造成 DomainCluster，计算语言/主导顶层目录/锚点。
func makeCluster(files []fileNode) DomainCluster {
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	rels := make([]string, len(files))
	extCounts := map[string]int{}
	topCount := map[string]int{}
	for i, f := range files {
		rels[i] = f.rel
		if f.ext != "" {
			extCounts[f.ext]++
		}
		if f.topDir != "" {
			topCount[f.topDir]++
		}
	}
	dom := ""
	bestN := 0
	for td, n := range topCount {
		if n > bestN {
			bestN = n
			dom = td
		}
	}
	if dom != "" && domainPurpose[dom] == "" {
		dom = ""
	}
	anchor := ""
	for _, f := range files {
		if entryCandidates[f.base] {
			anchor = f.rel
			break
		}
	}
	if anchor == "" && len(rels) > 0 {
		anchor = rels[0]
	}
	return DomainCluster{Files: rels, Anchor: anchor, DominantTopDir: dom, Lang: langFromExts(extCounts)}
}

// enumerateNodes 遍历 root 收集依赖图节点与节点集合。
// 顶层目录下所有文件（任意扩展名，含 .sh/.md 等）均收作节点：无依赖边的非源码文件
// 走文件夹兜底，使 scripts/ doc/ 等非源码目录仍能成领域（与旧按目录扫描行为一致）。
// 根级文件仅收源码/入口扩展名且非 meta（index.html/main.go 等），根级 .md/go.mod 不作节点。
func enumerateNodes(root string) ([]fileNode, map[string]bool) {
	var nodes []fileNode
	nodeSet := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := strings.ToLower(filepath.Ext(name))
		rel, _ := filepath.Rel(root, path)
		relPosix := filepath.ToSlash(rel)
		top := topSeg(relPosix)
		if top == "" {
			// 根级文件：仅源码/入口扩展名，非 meta，非 .md。
			if !sourceExts[ext] || rootMetaNames[strings.ToLower(name)] || ext == ".md" {
				return nil
			}
		}
		if len(nodes) >= maxScanFiles {
			return filepath.SkipAll
		}
		nodes = append(nodes, fileNode{rel: relPosix, topDir: top, ext: ext, base: name})
		nodeSet[relPosix] = true
		return nil
	})
	return nodes, nodeSet
}

// topSeg 返回 posix 相对路径的首段（顶层目录名），无分隔符则返回 ""。
func topSeg(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// extractEdges 读各节点内容按语言解析依赖引用，返无向边列表。best-effort，只连节点集内目标。
func extractEdges(root string, nodes []fileNode, nodeSet map[string]bool) []edge {
	module, _, _ := detectModule(root)
	var edges []edge
	for _, n := range nodes {
		if !sourceExts[n.ext] {
			continue // 非源码文件无依赖边，跳过解析。
		}
		data, err := os.ReadFile(filepath.Join(root, n.rel))
		if err != nil {
			continue
		}
		if len(data) > maxDepFileBytes {
			data = data[:maxDepFileBytes]
		}
		content := string(data)
		if n.ext == ".go" {
			for _, imp := range goImportPaths(content) {
				dir := goImportToDir(imp, module)
				if dir == "" {
					continue
				}
				for target := range nodeSet {
					if target == n.rel {
						continue
					}
					if underDir(target, dir) {
						edges = append(edges, edge{a: n.rel, b: target})
					}
				}
			}
			continue
		}
		for _, ref := range extractRefs(n.ext, content) {
			target := resolveRef(n.rel, ref, nodeSet)
			if target != "" && target != n.rel && nodeSet[target] {
				edges = append(edges, edge{a: n.rel, b: target})
			}
		}
	}
	return edges
}

// extractRefs 按扩展名用正则提取依赖引用字符串（未解析路径）。
func extractRefs(ext, content string) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	switch ext {
	case ".html", ".htm":
		for _, m := range reHTMLScript.FindAllStringSubmatch(content, -1) {
			add(m[1])
		}
		for _, m := range reHTMLLink.FindAllStringSubmatch(content, -1) {
			add(m[1])
		}
	case ".js", ".mjs", ".ts", ".tsx", ".jsx", ".vue", ".svelte":
		for _, re := range jsImportRes {
			for _, m := range re.FindAllStringSubmatch(content, -1) {
				add(m[1])
			}
		}
	case ".css", ".scss":
		for _, m := range reCSSImport.FindAllStringSubmatch(content, -1) {
			add(m[1])
		}
		for _, m := range reCSSURL.FindAllStringSubmatch(content, -1) {
			add(m[1])
		}
	case ".py":
		for _, m := range rePyFrom.FindAllStringSubmatch(content, -1) {
			if strings.HasPrefix(m[1], ".") {
				add(m[1])
			}
		}
	}
	return refs
}

// resolveRef 把引用字符串解析为节点集内的目标 posix 相对路径，无匹配返回 ""。
func resolveRef(refRel, target string, nodeSet map[string]bool) string {
	if target == "" {
		return ""
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return ""
	}
	base := filepath.ToSlash(filepath.Dir(refRel))
	var cand string
	if strings.HasPrefix(target, "/") {
		cand = strings.TrimPrefix(target, "/")
	} else {
		cand = filepath.ToSlash(filepath.Clean(filepath.Join(base, target)))
	}
	if nodeSet[cand] {
		return cand
	}
	if filepath.Ext(cand) == "" {
		for _, e := range tryExts {
			if nodeSet[cand+e] {
				return cand + e
			}
		}
	}
	return ""
}

// goImportPaths 从 Go 源码提取 import 路径（括号块与单行 import 均覆盖）。
func goImportPaths(content string) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, m := range reGoImportBlock.FindAllStringSubmatch(content, -1) {
		for _, mm := range reGoString.FindAllStringSubmatch(m[1], -1) {
			add(mm[1])
		}
	}
	for _, m := range reGoImportSingle.FindAllStringSubmatch(content, -1) {
		add(m[1])
	}
	return paths
}

// goImportToDir 把 Go import path 映射到本模块内目录（"." 表根），外部包返回 ""。
func goImportToDir(imp, module string) string {
	if module == "" {
		return ""
	}
	imp = strings.TrimSpace(imp)
	if imp == module {
		return "."
	}
	if strings.HasPrefix(imp, module+"/") {
		return strings.TrimPrefix(imp, module+"/")
	}
	return ""
}

// underDir 判断 target 是否在 dir 目录下（dir=="." 表根级文件）。
func underDir(target, dir string) bool {
	if dir == "." {
		return !strings.Contains(target, "/")
	}
	return target == dir || strings.HasPrefix(target, dir+"/")
}

// langFromExts 按扩展名计数选主导语言。同计数时取计数最大者；全空返回空串。
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

// unionFind 是简化并查集，用于依赖图连通分量。
type unionFind struct{ parent map[string]string }

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) find(x string) string {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		return x
	}
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}

// 依赖引用正则（编译一次）。JS/TS 用多行匹配覆盖跨行 import。
var (
	reHTMLScript     = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']+)["']`)
	reHTMLLink       = regexp.MustCompile(`(?i)<link[^>]+href\s*=\s*["']([^"']+)["']`)
	reCSSImport      = regexp.MustCompile(`@import\s+["']([^"']+)["']`)
	reCSSURL         = regexp.MustCompile(`@import\s+url\(\s*["']?([^"')]+)["']?\s*\)`)
	rePyFrom         = regexp.MustCompile(`(?m)^\s*from\s+(\S+)\s+import`)
	reGoImportBlock  = regexp.MustCompile(`(?s)import\s*\(([\s\S]*?)\)`)
	reGoImportSingle = regexp.MustCompile(`(?m)^\s*import\s+(?:[A-Za-z_]\w*\s+)?"([^"]+)"`)
	reGoString       = regexp.MustCompile(`"([^"]+)"`)

	jsImportRes = []*regexp.Regexp{
		regexp.MustCompile(`(?s)import\b[\s\S]*?from\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`import\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`(?s)export\b[\s\S]*?from\s*['"]([^'"]+)['"]`),
		regexp.MustCompile(`require\s*\(\s*['"]([^'"]+)['"]\s*\)`),
	}
)
