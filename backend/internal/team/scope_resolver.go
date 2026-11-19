package team

// scope_resolver.go 合同资源解析（Harness 根治计划 §5.2 / §6.3）。
//
// 历史问题：生产入口构造 TaskInput 时**从不解析资源**（WriteSet/Targets 恒空），
// 空 WriteSet 的语义是"尚未解析，不得执行生产写入"，于是 enforce 下模型一写就被拒；
// 而若信任模式把 ask 当批准，又退化成"任意写权限"（事故形态）。
//
// 2026-11-19 再次验收 P0-4 的实锤：路径识别把中文动词当路径字符。输入
// `修改src/a.css的间距` 得到 `修改src/a.css` 与目录 `修改src/**` —— 既不是精确授权，
// 也让后续路径检查与语义目标归属全部失效。根因是正则把所有 CJK 字符并入路径体，
// 且 `src/` 这种"文件路径中间的分隔符"被误判成目录 token。
//
// 现行纪律（确定性、不调 LLM、宁少不多）：
//   1. 路径体先按**扩展名锚点**反向遍历，再按"中文前缀剥离"规则修边界：
//      前导汉字段若**紧跟 ASCII 路径字符**（`修改src`），是动词前缀 → 剥离；
//      若紧跟分隔符（`文档/a.css`），是真实中文目录名 → 保留。
//   2. 目录 token 必须**独立**：`src/` 后面紧跟路径字符时它是文件路径的一部分，
//      不是目录授权（这正是 `修改src/**` 的来源）。
//   3. workDir 可用时做**候选验证**：候选不存在就尝试按 `/` 逐段截短的最长存在前缀，
//      避免把"动词+目录名"混成一个不存在的路径。
//   4. 解析不出任何资源 → Ambiguous=true，合同保持空 WriteSet（写入被拒）；
//      提示模型用 ask_user 或显式路径把范围说清楚，**绝不扩大权限**。
//   5. "不要动/保持不动 X" 进 ExcludedSet（恒 deny，优先于 allow）。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ScopeResolution 一次资源解析结果。
type ScopeResolution struct {
	WriteSet    []ResourceRule
	ExcludedSet []ResourceRule
	// Targets 语义目标（来自调用方给的候选标签匹配；本解析器只做去重与文件归属）。
	Targets []SemanticTarget
	// Paths 命中的资源路径（有序，便于注入上下文与断言）。
	Paths []string
	// Ambiguous 未能解析出任何可写资源：合同不得据此放行生产写入。
	Ambiguous bool
	// Notes 解析过程的说明（不存在的路径、歧义点），供事件审计。
	Notes []string
}

// codeFileExtPattern 代码类文件扩展名（扩展名锚点：路径识别从这里反向走）。
// 用非捕获组 + 大小写不敏感，避免把 `a.CSS` 漏掉。
var codeFileExtPattern = regexp.MustCompile(`(?i)\.(?:vue|tsx?|jsx?|mjs|cjs|css|scss|sass|less|less|go|py|java|kt|rs|sql|html|htm|json|ya?ml|toml|md|txt|sh|ps1|bat|ini|cfg|conf|xml|svg|proto|gradle|lock)($|[^A-Za-z0-9_])`)

// freezePatterns 用户显式"不要动"的句式（与 invariant_extract 同族，但这里只取目标）。
var freezePatterns = []string{
	"不要动", "别动", "不要碰", "别碰", "不要改", "别改", "保持不动", "保持原样", "保持不变",
}

// ResolveScope 从用户输入解析合同的写范围与禁止集合。
//
// workDir 用于候选验证（是否存在/是否目录）；为空时按纯文本解析（不校验存在性）。
func ResolveScope(input, workDir string, candidates []SemanticTarget) ScopeResolution {
	res := ScopeResolution{}
	text := strings.TrimSpace(input)
	if text == "" {
		res.Ambiguous = true
		return res
	}
	allow := map[string]ResourceRule{}
	addAllow := func(path string, isDir bool) {
		path = normalizeRelPath(path)
		if path == "" || !isCleanRelPath(path) {
			return
		}
		kind := RuleKindPath
		value := path
		if isDir {
			kind = RuleKindGlob
			value = strings.TrimSuffix(path, "/") + "/**"
		}
		key := kind + ":" + value
		if _, ok := allow[key]; ok {
			return
		}
		allow[key] = ResourceRule{Kind: kind, Value: value, Effect: RuleEffectAllow, Source: RuleSourceResolver}
		res.Paths = append(res.Paths, value)
	}

	// 1) 文件路径（扩展名锚点 + 中文边界修复 + 可选存在性验证）。
	fileTokens := extractFileTokens(text, workDir, &res)
	isDirToken := func(rel string) bool {
		if workDir == "" {
			return false
		}
		abs := filepath.Join(workDir, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		return err == nil && info.IsDir()
	}
	coveredDirs := map[string]bool{}
	for _, tok := range fileTokens {
		// 只有"在磁盘上确实是目录"时才按目录授权（`src` vs `src/a.css` 不会互相污染）。
		if isDirToken(tok) {
			addAllow(tok, true)
			coveredDirs[tok] = true
			continue
		}
		addAllow(tok, false)
		if workDir != "" {
			abs := filepath.Join(workDir, filepath.FromSlash(tok))
			if _, err := os.Stat(abs); err != nil {
				res.Notes = append(res.Notes, "路径不存在（按允许新建处理）："+tok)
			}
		}
	}

	// 2) 独立目录 token（用户说"这个目录下的…"）：必须以分隔符结尾，且**后面不接
	//    路径字符**——`src/` 后接 `a.css` 时它属于文件路径，不是目录授权。
	for _, tok := range extractDirTokens(text) {
		rel := normalizeRelPath(tok)
		if rel == "" || !isCleanRelPath(rel) || coveredDirs[rel] {
			continue
		}
		if workDir != "" {
			abs := filepath.Join(workDir, filepath.FromSlash(rel))
			if info, err := os.Stat(abs); err != nil || !info.IsDir() {
				continue // 目录不存在：不授予（宁少不多）
			}
		} else if strings.Contains(rel, ".") {
			// 无 workDir 时保守：带点的"目录"更可能是文件片段，不当目录授权。
			continue
		}
		addAllow(rel, true)
	}

	// 3) "不要动 X" → ExcludedSet 恒 deny。
	for _, phrase := range freezePatterns {
		for _, seg := range segmentsWith(text, phrase) {
			for _, tok := range extractFileTokens(seg, workDir, &res) {
				rel := normalizeRelPath(tok)
				if rel == "" || !isCleanRelPath(rel) {
					continue
				}
				res.ExcludedSet = append(res.ExcludedSet, ResourceRule{
					Kind: RuleKindPath, Value: rel, Effect: RuleEffectDeny, Source: RuleSourceUser,
				})
			}
		}
	}

	// 4) 语义目标：只保留文件在写范围内的候选（超出范围的候选不得当成授权）。
	seenTarget := map[string]bool{}
	var dangled []string
	for _, t := range candidates {
		if seenTarget[t.ID] {
			continue
		}
		if t.File != "" && !allowCoversPath(allow, normalizeRelPath(t.File)) {
			dangled = append(dangled, t.ID)
			continue
		}
		seenTarget[t.ID] = true
		res.Targets = append(res.Targets, t)
	}
	if len(dangled) > 0 {
		sort.Strings(dangled)
		res.Notes = append(res.Notes, "语义目标不在写范围内，已忽略："+strings.Join(dangled, ","))
	}

	// 5) 稳定输出。
	keys := make([]string, 0, len(allow))
	for k := range allow {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.WriteSet = append(res.WriteSet, allow[k])
	}
	sort.Slice(res.ExcludedSet, func(i, j int) bool { return res.ExcludedSet[i].Value < res.ExcludedSet[j].Value })
	sort.Strings(res.Paths)
	if len(res.WriteSet) == 0 {
		res.Ambiguous = true
	}
	return res
}

// --- 路径 token 抽取 ---------------------------------------------------------

// extractFileTokens 抽取文本中的文件路径 token（扩展名锚点反向遍历）。
//
// 算法（2026-11-19 再次验收 P0-4 重写）：
//  1. 用扩展名正则找到每个扩展名锚点的**起始**位置（锚点 = 真路径的右边界）；
//  2. 从锚点向左逐 **rune** 回扫，遇空白/标点（全角括号、逗号、引号、反引号…）停；
//     同时**不得跨过前一个锚点的末尾**——否则 `a.css和b.vue` 会被吃成一个巨路径；
//  3. 边界修复：前导汉字段若紧跟 ASCII 路径字符，判定为中文动词前缀并剥离
//     （`修改src/a.css` → `src/a.css`）；紧跟分隔符则保留（`文档/a.css`）；
//  4. workDir 可用时做存在性验证：不存在则按 rune 边界取**最长的存在后缀**
//     （`改一下文档/样式/a.css` → `文档/样式/a.css`）；无任何存在后缀则原样保留并记 Note。
func extractFileTokens(text, workDir string, res *ScopeResolution) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	prevEnd := 0
	locs := codeFileExtPattern.FindAllStringIndex(text, -1)
	for _, loc := range locs {
		end := loc[0] + extLenAt(text[loc[0]:])
		if end <= 0 || end > len(text) {
			continue
		}
		start := scanPathStart(text, end, prevEnd)
		prevEnd = end
		raw := text[start:end]
		rel := refinePathToken(raw, workDir, res)
		rel = normalizeRelPath(rel)
		if rel == "" || !isCleanRelPath(rel) || seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out
}

// scanPathStart 从 end 向左逐 rune 回扫，返回路径体起点（下限为 floor）。
//
// 逐 rune（而非逐字节）扫描的关键：全角标点 `（），、` 与反引号/引号不能被当成
// 路径体——它们是多字节字符，按字节判会把它连同后面一切吞进“路径”。
func scanPathStart(text string, end, floor int) int {
	start := end
	for start > floor {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if size <= 0 {
			break
		}
		if !isPathBodyRune(r) {
			break
		}
		start -= size
	}
	return start
}

// isPathBodyRune 判定一个 rune 是否可属于路径体：
// ASCII 字母/数字/下划线/短横/点/分隔符，或 CJK 汉字。其余（空白、标点、符号）均终止。
func isPathBodyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_' || r == '-' || r == '.' || r == '/' || r == '\\':
		return true
	case isHanRune(r):
		return true
	}
	return false
}

// extLenAt 返回从 s（以 `.` 开头）起的扩展名长度；不匹配返回 0。
func extLenAt(s string) int {
	loc := codeFileExtPattern.FindStringIndex(s)
	if loc == nil || loc[0] != 0 {
		return 0
	}
	// FindStringIndex 的 loc[1] 可能含尾部一个非路径字符（正则的 `($|[^...])`），
	// 这里按"点号后连续字母数字"重新计算，避免把尾字符算进路径。
	i := 0
	if i < len(s) && s[i] == '.' {
		i++
	}
	for i < len(s) && (isASCIIAlnum(s[i]) || s[i] == '_') {
		i++
	}
	return i
}

// refinePathToken 修正路径 token 的左右边界。
func refinePathToken(raw, workDir string, res *ScopeResolution) string {
	// 右侧：截掉尾部非路径字符（防御性，正常情况下锚点已对齐）。
	raw = strings.TrimRight(raw, ".,;:，。；：、）)】]」』\"'`")
	if raw == "" {
		return ""
	}
	// 先用真实工作区消除歧义，再做文本启发式处理。这样“修改样式.css”会按最长
	// 存在后缀解析为真实的“样式.css”，而不是把所有汉字都当动词剥成“.css”；
	// 本身就存在的纯中文/中文+ASCII 文件名则原样保留。
	if workDir != "" {
		if pathExists(workDir, raw) {
			return raw
		}
		if alt, ok := longestExistingSuffix(workDir, raw); ok {
			if res != nil {
				res.Notes = append(res.Notes, "路径按存在性校正："+raw+" → "+alt)
			}
			return alt
		}
	}
	// 无现存候选时才剥离可识别的中文动词前缀。无法证明边界的拟新建文件保持
	// 原样并记 Note，绝不能靠删除文件名片段制造一个看似合法的错误授权。
	if stripped, ok := stripHanVerbPrefix(raw); ok {
		raw = stripped
	}
	return raw
}

// stripHanVerbPrefix 剥离"中文动词 + 路径"里的动词前缀。
//
// 只有前导汉字后紧跟 ASCII 路径字符时才尝试剥离；点号不在此列——纯中文文件名
// （样式.css、订单.md）的扩展名前也是汉字，若允许点号触发会把文件名剥成空 basename。
func stripHanVerbPrefix(tok string) (string, bool) {
	i := 0
	for i < len(tok) {
		if tok[i] < 0x80 {
			break
		}
		r, size := utf8.DecodeRuneInString(tok[i:])
		if size <= 0 || !isHanRune(r) {
			break
		}
		i += size
	}
	if i == 0 {
		return tok, false
	}
	if i >= len(tok) {
		return tok, false // 全中文：不是路径
	}
	next := tok[i]
	if next == '/' || next == '\\' {
		return tok, false // 中文目录名，保留
	}
	if isASCIIAlnum(next) || next == '_' {
		return tok[i:], true
	}
	return tok, false
}

// longestExistingSuffix 在 workDir 下按 **rune 边界**逐位截短，返回最长的存在后缀。
//
// 按 rune 而非按 `/` 分段的理由：中文动词前缀往往与目录名粘在一起
// （`改一下文档/样式/a.css`），按段截短得不出 `文档/样式/a.css`。
func longestExistingSuffix(workDir, rel string) (string, bool) {
	for i := 1; i < len(rel); i++ {
		if !utf8.RuneStart(rel[i]) {
			continue
		}
		candidate := rel[i:]
		if !isCleanRelPath(candidate) {
			continue
		}
		if pathExists(workDir, candidate) {
			return candidate, true
		}
	}
	return "", false
}

func pathExists(workDir, rel string) bool {
	abs := filepath.Join(workDir, filepath.FromSlash(rel))
	_, err := os.Lstat(abs)
	return err == nil
}

// extractDirTokens 抽取"独立目录"token：以 `/` 或 `\` 结尾，且其后不接路径字符。
//
// `修改src/a.css的间距` 里的 `src/` 后面紧跟 `a`（路径字符），因此**不是**目录 token——
// 这是旧实现凭空多给 `修改src/**` 目录授权的原因。
func extractDirTokens(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '/' && text[i] != '\\' {
			continue
		}
		// 后面必须不是路径字符（否则是文件路径中间的分隔符）。
		if i+1 < len(text) {
			if r, _ := utf8.DecodeRuneInString(text[i+1:]); isPathBodyRune(r) {
				continue
			}
		}
		start := scanPathStart(text, i, 0)
		raw := text[start:i]
		raw = strings.TrimLeft(raw, "/\\")
		if raw == "" {
			continue
		}
		if stripped, ok := stripHanVerbPrefix(raw); ok {
			raw = stripped
		}
		raw = strings.TrimSuffix(raw, "/")
		if raw == "" {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// isASCIIAlnum 判定 ASCII 字母/数字。
func isASCIIAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// isHanRune 判定是否为 CJK 统一表意文字（含扩展 A 与兼容表意）。
func isHanRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0x3400 && r <= 0x4DBF: // Extension A
		return true
	case r >= 0xF900 && r <= 0xFAFF: // Compatibility Ideographs
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // Extension B-F
		return true
	}
	return false
}

// isCleanRelPath 拒绝绝对路径与上跳片段（合同写范围只接受工作区内相对路径）。
func isCleanRelPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	if len(p) >= 2 && p[1] == ':' {
		return false // 盘符
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// allowCoversPath 判定某文件路径是否被 allow 集合覆盖（path 精确或 glob 前缀）。
func allowCoversPath(allow map[string]ResourceRule, path string) bool {
	if path == "" {
		return false
	}
	if _, ok := allow[RuleKindPath+":"+path]; ok {
		return true
	}
	for _, rule := range allow {
		if rule.Kind != RuleKindGlob {
			continue
		}
		prefix := strings.TrimSuffix(rule.Value, "/**")
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// segmentsWith 返回包含 phrase 的句子片段（按中文/英文句末标点切分）。
func segmentsWith(text, phrase string) []string {
	if !strings.Contains(text, phrase) {
		return nil
	}
	splitter := func(r rune) bool {
		switch r {
		case '。', '；', '！', '？', '\n', ';', '!', '?':
			return true
		}
		return false
	}
	var out []string
	for _, seg := range strings.FieldsFunc(text, splitter) {
		if strings.Contains(seg, phrase) {
			out = append(out, seg)
		}
	}
	return out
}
