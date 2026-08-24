package subagent

// contract_check.go 提供跨域契约静态校验（TODO #57 校验三层方案第二层）：
// dispatcher 在父节点下全部兄弟域完成时对 WriteSpec contract 字段逐条核对
// （regex/文本解析，零 LLM）。违例按文件归属批量打回责任域（一次消息列全部违例），
// 通过结果以【机器校验】段入摘要供 meta 纸面对照。
//
// 设计约束（doc/TODO.md #57 不做项）：不做 AST 级精确解析（regex 够用，
// 漏报优于复杂化）；契约为空时跳过；契约只保证静态一致性。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// contractViolation 单条契约违例：file 为责任文件（打回归属依据）。
type contractViolation struct {
	file   string
	detail string
}

// contractReport 契约检查报告：entries 为逐条检查结果（含通过/违例），
// violations 为违例子集（按 file 聚合后供打回消息）。
type contractReport struct {
	entries    []string
	violations []contractViolation
}

// pass 判断契约检查是否全过。
func (r contractReport) pass() bool { return len(r.violations) == 0 }

// identifierRegexp 单词边界匹配单标识符（防 CONFIG 匹配 MYCONFIGX）。
var identifierRegexp = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// symbolFound 判断符号是否出现在文件内容中：
//   - 字面包含（复合符号如 `GameEngine.init` 的引用形态 `GameEngine.init()`）直接命中；
//   - 否则对复合符号按 "." 拆段，逐段词边界匹配——覆盖声明形态
//     `var GameEngine = { init: function() {} }`（字面不含 `GameEngine.init`）。
//
// 静态文本解析不做 AST（doc/TODO.md #57 不做项）：段匹配会放宽误放行，
// 漏报优于复杂化，契约只保证静态一致性兜底。
func symbolFound(content, symbol string) bool {
	if strings.Contains(content, symbol) {
		return true
	}
	segs := strings.Split(symbol, ".")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		seg = strings.Trim(seg, "()[]")
		if seg == "" || !identifierRegexp.MatchString(seg) {
			continue
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(seg) + `\b`).MatchString(content) {
			return false
		}
	}
	return true
}

// contractFileAbs 把契约文件路径解析为绝对路径（相对工作目录）。
func contractFileAbs(workdir, p string) string {
	p = strings.TrimSpace(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(workdir, p))
}

// readContractFile 读契约涉及文件；缺失返回 ok=false。
func readContractFile(workdir, p string) (string, bool) {
	abs := contractFileAbs(workdir, p)
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// runContractChecks 逐条核对契约。workdir 为工作目录（文件相对路径解析基准）。
func (d *Dispatcher) runContractChecks(c *tool.Contract, specFiles []string) contractReport {
	workdir := d.subAgentWorkDir()
	var rep contractReport

	// 1. 跨域符号：声明文件存在 + 引用方含引用点。
	for _, sy := range c.Symbols {
		sym := strings.TrimSpace(sy.Symbol)
		decl, ok := readContractFile(workdir, sy.File)
		if !ok {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: 契约符号 `%s` 的声明文件 `%s` 缺失", sy.Symbol, sy.File))
			rep.violations = append(rep.violations, contractViolation{file: sy.File, detail: fmt.Sprintf("契约符号 `%s` 的声明文件缺失", sy.Symbol)})
			continue
		}
		if !symbolFound(decl, sym) {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: 符号 `%s` 未在声明文件 `%s` 中找到", sy.Symbol, sy.File))
			rep.violations = append(rep.violations, contractViolation{file: sy.File, detail: fmt.Sprintf("契约符号 `%s` 未在声明文件中找到", sy.Symbol)})
			continue
		}
		for _, ref := range sy.Refs {
			content, ok := readContractFile(workdir, ref)
			if !ok {
				rep.entries = append(rep.entries, fmt.Sprintf("违例: 符号 `%s` 的引用方文件 `%s` 缺失", sy.Symbol, ref))
				rep.violations = append(rep.violations, contractViolation{file: ref, detail: fmt.Sprintf("契约符号 `%s` 的引用方文件缺失", sy.Symbol)})
				continue
			}
			if !symbolFound(content, sym) {
				rep.entries = append(rep.entries, fmt.Sprintf("违例: 引用方 `%s` 未引用符号 `%s`", ref, sy.Symbol))
				rep.violations = append(rep.violations, contractViolation{file: ref, detail: fmt.Sprintf("未引用契约符号 `%s`", sy.Symbol)})
				continue
			}
			rep.entries = append(rep.entries, fmt.Sprintf("通过: 符号 `%s` 声明于 `%s`，引用方 `%s` 引用点存在", sy.Symbol, sy.File, ref))
		}
		if len(sy.Refs) == 0 {
			rep.entries = append(rep.entries, fmt.Sprintf("通过: 符号 `%s` 声明于 `%s`", sy.Symbol, sy.File))
		}
	}

	// 2. DOM id：声明文件中存在 id="X" / id='X'。
	for _, id := range c.DOMIDs {
		re := regexp.MustCompile(`id\s*=\s*["']` + regexp.QuoteMeta(strings.TrimSpace(id.ID)) + `["']`)
		content, ok := readContractFile(workdir, id.File)
		if !ok {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: DOM id `%s` 的声明文件 `%s` 缺失", id.ID, id.File))
			rep.violations = append(rep.violations, contractViolation{file: id.File, detail: fmt.Sprintf("契约 DOM id `%s` 的声明文件缺失", id.ID)})
			continue
		}
		if !re.MatchString(content) {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: DOM id `%s` 未在 `%s` 中声明", id.ID, id.File))
			rep.violations = append(rep.violations, contractViolation{file: id.File, detail: fmt.Sprintf("契约 DOM id `%s` 未声明", id.ID)})
			continue
		}
		rep.entries = append(rep.entries, fmt.Sprintf("通过: DOM id `%s` 声明于 `%s`", id.ID, id.File))
	}

	// 3. script 加载顺序：契约条目顺序即加载顺序，在 HTML 文件中核对相对顺序。
	// 只检查出现在同一 HTML 中的契约 script 相对顺序；某 script 未出现在任何
	// HTML 时跳过（漏报优于复杂化）。
	checkScriptOrder(workdir, specFiles, c.Scripts, &rep)

	// 4. 跨域函数签名：签名文本字面出现在声明文件中。
	for _, sg := range c.Signatures {
		content, ok := readContractFile(workdir, sg.File)
		if !ok {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: 签名 `%s` 的声明文件 `%s` 缺失", sg.Signature, sg.File))
			rep.violations = append(rep.violations, contractViolation{file: sg.File, detail: fmt.Sprintf("契约签名 `%s` 的声明文件缺失", sg.Signature)})
			continue
		}
		if !strings.Contains(content, sg.Signature) {
			rep.entries = append(rep.entries, fmt.Sprintf("违例: 签名 `%s` 未在 `%s` 中找到", sg.Signature, sg.File))
			rep.violations = append(rep.violations, contractViolation{file: sg.File, detail: fmt.Sprintf("契约签名 `%s` 未在声明文件中找到", sg.Signature)})
			continue
		}
		rep.entries = append(rep.entries, fmt.Sprintf("通过: 签名 `%s` 声明于 `%s`", sg.Signature, sg.File))
	}

	return rep
}

// scriptSrcRegexp 提取 HTML 中 <script src="..."> 的 src 值。
var scriptSrcRegexp = regexp.MustCompile(`<script[^>]*\bsrc\s*=\s*["']([^"']+)["']`)

// checkScriptOrder 核对契约 script 在 HTML 中的相对顺序。
func checkScriptOrder(workdir string, specFiles []string, scripts []tool.ContractScript, rep *contractReport) {
	if len(scripts) < 2 {
		return
	}
	basenames := make([]string, 0, len(scripts))
	for _, sc := range scripts {
		basenames = append(basenames, filepath.Base(strings.TrimSpace(sc.File)))
	}
	for _, sf := range specFiles {
		ext := strings.ToLower(filepath.Ext(sf))
		if ext != ".html" && ext != ".htm" {
			continue
		}
		content, ok := readContractFile(workdir, sf)
		if !ok {
			continue
		}
		matches := scriptSrcRegexp.FindAllStringSubmatch(content, -1)
		var order []int // 契约条目下标，按 HTML 中出现顺序
		for _, m := range matches {
			src := m[1]
			for i, bn := range basenames {
				if strings.HasSuffix(strings.TrimSpace(src), bn) {
					order = append(order, i)
					break
				}
			}
		}
		// 顺序核对：出现顺序必须严格递增；重复出现取首次。
		seen := map[int]bool{}
		var seq []int
		for _, i := range order {
			if !seen[i] {
				seen[i] = true
				seq = append(seq, i)
			}
		}
		violated := false
		for i := 1; i < len(seq); i++ {
			if seq[i] < seq[i-1] {
				violated = true
				break
			}
		}
		if violated {
			detail := "契约 script 加载顺序不一致: " + strings.Join(basenames, " -> ") + "（实际: " + scriptOrderActual(seq, basenames) + "）"
			rep.entries = append(rep.entries, "违例: "+detail+"（文件 "+sf+"）")
			rep.violations = append(rep.violations, contractViolation{file: sf, detail: detail})
		} else if len(seq) > 0 {
			rep.entries = append(rep.entries, fmt.Sprintf("通过: `%s` 中契约 script 顺序一致", sf))
		}
	}
}

// scriptOrderActual 渲染 HTML 中实际出现的契约 script 顺序（诊断用）。
func scriptOrderActual(seq []int, basenames []string) string {
	parts := make([]string, 0, len(seq))
	for _, i := range seq {
		parts = append(parts, basenames[i])
	}
	return strings.Join(parts, " -> ")
}

// contractReportText 渲染契约检查报告正文（pass=true 为通过段，false 为违例段）。
func contractReportText(rep contractReport) string {
	var b strings.Builder
	entries := rep.entries
	if len(entries) == 0 {
		entries = []string{"契约条目为空，无检查项"}
	}
	b.WriteString("【机器校验】跨域契约（dispatcher 执行，非 agent 自述）:\n")
	for _, e := range entries {
		b.WriteString("- " + e + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// contractViolationText 按文件聚合违例，渲染打回消息正文（一次列全部违例）。
func contractViolationText(rep contractReport) string {
	byFile := map[string][]string{}
	var order []string
	for _, v := range rep.violations {
		if _, ok := byFile[v.file]; !ok {
			order = append(order, v.file)
		}
		byFile[v.file] = append(byFile[v.file], v.detail)
	}
	sort.Strings(order)
	var b strings.Builder
	for _, f := range order {
		b.WriteString("- " + f + ": " + strings.Join(byFile[f], "；") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
