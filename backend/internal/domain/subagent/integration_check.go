package subagent

// integration_check.go 实现 TODO #59 验收分层第三层（集成层）：
// spec verify_levels 含 "integration" 时，dispatcher 在子 Agent 完成收尾路径对
// 前端入口做引用图探针（零 LLM、零命令执行、纯文件存在性检查）：
//   - HTML script src 解析：入口 index.html 引用的每个 script src 必须存在；
//   - 入口 module 引用图：入口 js/ts 的相对 import/require 路径必须可解析到文件
//     （深度 1，扩展名按 .js/.ts/.jsx/.tsx//index.* 逐级尝试）；
//   - 空壳提示：入口文件零 import/require 且项目多文件时给出提示
//     （实证 2026-08-24 塔防：vite build "成功"但产物 9 modules 空壳，
//     入口 main.ts 被 stub 覆盖成空渲染循环、全仓 30+ 文件未进 bundle）。
//
// 结果以【机器校验】段入完成摘要；违例走冒烟同款反馈重试通道打回责任域。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// importRegexps 提取入口文件中的相对/绝对导入路径（深度 1 引用图检查用）。
// 覆盖 ES module 与 CommonJS 两种形态；裸包名（无 ./ ../ / 前缀）跳过。
var importRegexps = []*regexp.Regexp{
	regexp.MustCompile(`from\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`import\s*\(?\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`require\s*\(\s*['"]([^'"]+)['"]\s*\)`),
}

// importExtCandidates 相对导入解析时的扩展名/入口候选（按序尝试）。
var importExtCandidates = []string{"", ".js", ".ts", ".jsx", ".tsx", ".mjs", "/index.js", "/index.ts"}

// integrationEntry 单条集成层检查结果：pass=false 为违例（打回），true 为通过/提示。
type integrationEntry struct {
	file   string
	detail string
	pass   bool
}

// integrationReport 集成层检查报告。
type integrationReport struct {
	entries []integrationEntry
}

// integrationFailed 过滤违例条目。
func integrationFailed(r integrationReport) []integrationEntry {
	var out []integrationEntry
	for _, e := range r.entries {
		if !e.pass {
			out = append(out, e)
		}
	}
	return out
}

// runIntegrationChecks 对 spec 涉及文件跑集成层探针。
// specFiles 为绝对路径（recordParentSpec 已归一化）；workdir 用于相对化展示。
func runIntegrationChecks(workdir string, specFiles []string) integrationReport {
	var rep integrationReport
	htmlFiles := make([]string, 0, len(specFiles))
	for _, f := range specFiles {
		switch strings.ToLower(filepath.Ext(f)) {
		case ".html", ".htm":
			htmlFiles = append(htmlFiles, f)
		}
	}
	if len(htmlFiles) == 0 {
		// 无 HTML 入口：跳过（非前端形态任务，集成层无可探对象）。
		return rep
	}
	for _, html := range htmlFiles {
		content, err := os.ReadFile(html)
		if err != nil {
			continue // spec 文件缺失：存在性层（冒烟）已报，此处不再重复。
		}
		htmlStr := string(content)
		htmlDir := filepath.Dir(html)
		// 1. script src 存在性：每个 <script src> 解析到存在的文件。
		seenSrc := map[string]bool{}
		for _, m := range scriptSrcRegexp.FindAllStringSubmatch(htmlStr, -1) {
			src := strings.TrimSpace(m[1])
			if src == "" || isExternalRef(src) {
				continue
			}
			resolved, ok := resolveEntryFile(htmlDir, src)
			if !ok {
				rep.entries = append(rep.entries, integrationEntry{
					file:   relOrAbs(workdir, html),
					detail: fmt.Sprintf("违例: script src `%s` 解析不到文件（防空壳产物断链）", src),
					pass:   false,
				})
				continue
			}
			seenSrc[resolved] = true
			rep.entries = append(rep.entries, integrationEntry{
				file:   relOrAbs(workdir, html),
				detail: fmt.Sprintf("通过: script `%s` → %s", src, relOrAbs(workdir, resolved)),
				pass:   true,
			})
		}
		// 2. 入口 module 引用图（深度 1）：module script 或首个 js/ts 入口的
		// 相对 import/require 路径必须可解析。
		entry := ""
		for _, m := range scriptSrcRegexp.FindAllStringSubmatch(htmlStr, -1) {
			src := strings.TrimSpace(m[1])
			if isExternalRef(src) {
				continue
			}
			resolved, ok := resolveEntryFile(htmlDir, src)
			if !ok {
				continue
			}
			switch strings.ToLower(filepath.Ext(resolved)) {
			case ".js", ".ts", ".jsx", ".tsx", ".mjs":
				entry = resolved
			}
			if entry != "" {
				break
			}
		}
		if entry == "" {
			continue
		}
		rep.entries = append(rep.entries, integrationEntry{
			file:   relOrAbs(workdir, html),
			detail: fmt.Sprintf("通过: 入口 `%s` 已解析", relOrAbs(workdir, entry)),
			pass:   true,
		})
		imports, importCount := scanEntryImports(entry)
		for _, imp := range imports {
			resolved, ok := resolveEntryFile(filepath.Dir(entry), imp)
			if !ok {
				rep.entries = append(rep.entries, integrationEntry{
					file:   relOrAbs(workdir, entry),
					detail: fmt.Sprintf("违例: 入口 import `%s` 解析不到文件（引用链断点）", imp),
					pass:   false,
				})
				continue
			}
			rep.entries = append(rep.entries, integrationEntry{
				file:   relOrAbs(workdir, entry),
				detail: fmt.Sprintf("通过: import `%s` → %s", imp, relOrAbs(workdir, resolved)),
				pass:   true,
			})
		}
		// 3. 空壳提示：入口零 import 且项目多文件（实证：stub 空渲染循环）。
		if importCount == 0 && len(specFiles) >= 2 {
			rep.entries = append(rep.entries, integrationEntry{
				file:   relOrAbs(workdir, entry),
				detail: "提示: 入口无任何 import/require，若项目含多文件则疑为空壳产物（bundle 未收敛，参考 2026-08-24 塔防 9 modules 空壳事故）",
				pass:   true,
			})
		}
	}
	return rep
}

// isExternalRef 判断引用是否为外部 URL（http/https/data:// 协议或 // 协议相对）。
func isExternalRef(ref string) bool {
	lower := strings.ToLower(ref)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "data:") || strings.HasPrefix(ref, "//")
}

// scanEntryImports 提取入口文件的相对/绝对导入路径（去重、保序）。
// 返回 (相对导入列表, 导入总数)：总数含裸包名（仅用于空壳判断）。
func scanEntryImports(entry string) ([]string, int) {
	content, err := os.ReadFile(entry)
	if err != nil {
		return nil, 0
	}
	seen := map[string]bool{}
	var rel []string
	total := 0
	for _, re := range importRegexps {
		for _, m := range re.FindAllStringSubmatch(string(content), -1) {
			imp := strings.TrimSpace(m[1])
			if imp == "" || strings.HasPrefix(imp, "//") {
				continue
			}
			total++
			if !isRelativeImport(imp) || seen[imp] {
				continue
			}
			seen[imp] = true
			rel = append(rel, imp)
		}
	}
	return rel, total
}

// isRelativeImport 判断导入路径是否为相对/绝对路径（./ ../ / 开头，非裸包名）。
func isRelativeImport(imp string) bool {
	return strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../") || strings.HasPrefix(imp, "/")
}

// resolveEntryFile 解析引用路径到存在的文件：baseDir 下按候选扩展名逐级尝试。
// 外部 URL 由调用方先过滤；相对路径按引用基目录解析。
func resolveEntryFile(baseDir, ref string) (string, bool) {
	ref = strings.SplitN(ref, "?", 2)[0] // 去 query
	ref = strings.SplitN(ref, "#", 2)[0] // 去 fragment
	if filepath.IsAbs(ref) {
		// 绝对路径按工作目录根解析（web 部署形态，Vite 常用 /src/...）。
		ref = strings.TrimPrefix(filepath.ToSlash(ref), "/")
	}
	for _, cand := range importExtCandidates {
		p := filepath.Join(baseDir, ref+cand)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// relOrAbs 把绝对路径相对化展示（workdir 内显示相对，外显示原样）。
func relOrAbs(workdir, p string) string {
	if workdir == "" {
		return p
	}
	rel, err := filepath.Rel(workdir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

// integrationFixMessage 渲染集成层违例反馈重试消息（1 轮，复用冒烟通道语义）。
func integrationFixMessage(failed []integrationEntry) string {
	var b strings.Builder
	b.WriteString("【机器校验失败】dispatcher 对你的产出做了集成层引用图探针，以下引用断点未通过（入口引用的文件不存在）：\n")
	for _, e := range failed {
		fmt.Fprintf(&b, "- %s: %s\n", e.file, e.detail)
	}
	b.WriteString("请补齐缺失文件/修正引用路径后重新自检并给出终答。")
	return b.String()
}

// renderIntegrationReport 渲染【机器校验】集成层段（完成摘要追加用）。
func renderIntegrationReport(r integrationReport) string {
	if len(r.entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【机器校验】集成层（入口引用图探针，dispatcher 执行）:\n")
	for _, e := range r.entries {
		marker := "通过"
		if !e.pass {
			marker = "失败"
		}
		fmt.Fprintf(&b, "- %s: %s\n", marker, strings.TrimPrefix(e.detail, marker+": "))
	}
	return strings.TrimRight(b.String(), "\n")
}
