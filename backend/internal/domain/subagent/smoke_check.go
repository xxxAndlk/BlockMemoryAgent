package subagent

// smoke_check.go 提供域完成机器校验冒烟层（TODO #56 校验三层方案第一层）：
// dispatcher 在子 Agent 完成收尾路径对 spec.files ∩ 本子 Agent 实际写入文件
// 自动派生并执行冒烟命令（.js → node --check、.go → gofmt -l、.ts → tsc --noEmit），
// 零 LLM 调用，结果以【机器校验】段入完成摘要（dispatcher 执行，非 agent 自述）。
// 失败走 verify_kind 反馈重试通道打回责任域（复用 #43 校验分层路由，不新建通路）。
//
// 校验对象收敛为 spec.files ∩ 本子 Agent 写入文件：责任归属精确——
// 并行兄弟域中途写入共享文件时，先完成的一方不会被兄弟的半成品误打回。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"time"
)

// smokeTimeout 单条冒烟命令的超时上限。毫秒级语法检查给 30s 余量已足够。
const smokeTimeout = 30 * time.Second

// smokeOutputTailRunes 失败输出保留的尾部 rune 数（防超长编译错误撑爆摘要）。
const smokeOutputTailRunes = 2000

// smokeCommand 描述一类文件扩展名对应的冒烟命令。
type smokeCommand struct {
	// probe 是工具链二进制名（exec.LookPath 探测，缺失则跳过并标注）。
	probe string
	// args 按文件路径生成命令参数。
	args func(file string) []string
	// outputEmptyMeansPass 为 true 时输出为空才算通过（gofmt -l 语义：
	// 有输出=文件未格式化，exit 恒 0，必须看输出判定）。
	outputEmptyMeansPass bool
}

// smokeCommandByExt 扩展名 -> 冒烟命令派生表（含探测降级语义）。
var smokeCommandByExt = map[string]smokeCommand{
	".js": {probe: "node", args: func(f string) []string { return []string{"--check", f} }},
	".go": {probe: "gofmt", args: func(f string) []string { return []string{"-l", f} }, outputEmptyMeansPass: true},
	".ts": {probe: "tsc", args: func(f string) []string { return []string{"--noEmit", f} }},
}

// smokeRunner 执行一条命令，返回合并输出/退出码/错误。测试注入假实现。
// exitCode 约定：进程正常退出=其退出码；无法启动等错误时 err 非空且 exitCode=-1。
type smokeRunner func(ctx context.Context, dir, name string, args ...string) (output string, exitCode int, err error)

// defaultSmokeRunner 用 exec.CommandContext 真跑命令，工作目录为 workdir。
func defaultSmokeRunner(ctx context.Context, dir, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), -1, err
}

// lookPathFunc 探测工具链二进制；测试注入假实现模拟"node 不存在"降级。
type lookPathFunc func(name string) (string, error)

// smokeResult 单条冒烟检查结果。
type smokeResult struct {
	file       string // 被检查文件路径（工作目录相对或绝对，按传入原样展示）
	cmdline    string // 实际执行的命令全文
	exitCode   int    // 进程退出码；无法启动为 -1
	output     string // 命令输出（失败时截断尾部）
	skipped    bool   // 跳过（无工具链/文件不存在/无可派生命令）
	skipReason string
}

// failed 判断该条是否判定为失败。
func (r smokeResult) failed() bool {
	if r.skipped {
		return false
	}
	return r.exitCode != 0
}

// runSmokeChecks 对 files 逐条派生并执行冒烟命令。
// 返回结果按输入顺序排列。ctx 控制整体截止（命令自身再套 smokeTimeout）。
func runSmokeChecks(ctx context.Context, workdir string, files []string, runner smokeRunner, lookPath lookPathFunc) []smokeResult {
	if runner == nil {
		runner = defaultSmokeRunner
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	results := make([]smokeResult, 0, len(files))
	for _, f := range files {
		results = append(results, runSmokeOne(ctx, workdir, f, runner, lookPath))
	}
	return results
}

// runSmokeOne 对单个文件派生并执行冒烟命令。
func runSmokeOne(ctx context.Context, workdir, file string, runner smokeRunner, lookPath lookPathFunc) smokeResult {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workdir, file)
	}
	if fi, err := osStat(abs); err != nil || fi.IsDir() {
		return smokeResult{file: file, skipped: true, skipReason: "文件不存在或为目录"}
	}
	cmd, ok := smokeCommandByExt[strings.ToLower(filepath.Ext(file))]
	if !ok {
		return smokeResult{file: file, skipped: true, skipReason: "无可派生冒烟命令"}
	}
	if _, err := lookPath(cmd.probe); err != nil {
		return smokeResult{file: file, skipped: true, skipReason: "工具链 " + cmd.probe + " 不可用"}
	}
	args := cmd.args(abs)
	cmdCtx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	out, code, err := runner(cmdCtx, workdir, cmd.probe, args...)
	res := smokeResult{
		file:     file,
		cmdline:  cmd.probe + " " + strings.Join(args, " "),
		exitCode: code,
		output:   tailRunes(strings.TrimSpace(out), smokeOutputTailRunes),
	}
	if err != nil {
		res.exitCode = -1
		res.output = tailRunes(strings.TrimSpace(err.Error()), smokeOutputTailRunes)
		return res
	}
	if cmd.outputEmptyMeansPass && res.exitCode == 0 && res.output != "" {
		// gofmt -l：exit 0 但输出非空 = 文件未格式化，判失败。
		res.exitCode = 1
	}
	return res
}

// tailRunes 保留字符串尾部 n 个 rune，超长时前置截断提示。
func tailRunes(s string, n int) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…(截断) " + string(r[len(r)-n:])
}

// osStat 是 os.Stat 的可替换包装，测试注入假实现。
var osStat = func(path string) (os.FileInfo, error) { return os.Stat(path) }

// smokeFailed 过滤失败条目（skipped 不算失败）。
func smokeFailed(results []smokeResult) []smokeResult {
	var out []smokeResult
	for _, r := range results {
		if r.failed() {
			out = append(out, r)
		}
	}
	return out
}

// smokeRunCount 返回实际执行（非跳过）的条目数。
func smokeRunCount(results []smokeResult) int {
	n := 0
	for _, r := range results {
		if !r.skipped {
			n++
		}
	}
	return n
}

// smokeFixMessage 渲染冒烟失败后的反馈重试消息（1 轮，复用 L0 反馈通道语义）。
func smokeFixMessage(failed []smokeResult) string {
	var b strings.Builder
	b.WriteString("【机器校验失败】dispatcher 对你的产出文件自动执行了冒烟检查，以下文件未通过（命令 + 退出码 + 输出尾部）：\n")
	for _, r := range failed {
		fmt.Fprintf(&b, "- %s: `%s` → exit %d\n%s\n", r.file, r.cmdline, r.exitCode, r.output)
	}
	b.WriteString("请修复这些问题后重新自检并给出终答。")
	return b.String()
}

// renderSmokeReport 渲染【机器校验】段（完成摘要追加用，含 dispatcher 执行标注）。
func renderSmokeReport(results []smokeResult) string {
	var b strings.Builder
	b.WriteString("【机器校验】dispatcher 自动执行（非 agent 自述）：\n")
	for _, r := range results {
		if r.skipped {
			fmt.Fprintf(&b, "- %s: 跳过（%s）\n", r.file, r.skipReason)
			continue
		}
		status := "通过"
		if r.failed() {
			status = "失败"
		}
		fmt.Fprintf(&b, "- %s: `%s` → %s（exit %d）", r.file, r.cmdline, status, r.exitCode)
		if r.output != "" {
			b.WriteString("\n" + indentLines(r.output, "  "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// indentLines 给多行输出统一缩进。
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// smokeTargets 求 spec.files ∩ 本子 Agent 实际写入文件的交集（保持修改历史顺序），
// 并把修改路径规范化为绝对路径后与 spec 绝对路径匹配。
func smokeTargets(specFiles []string, modified []string, workdir string) []string {
	if len(specFiles) == 0 || len(modified) == 0 {
		return nil
	}
	specSet := make(map[string]bool, len(specFiles))
	for _, f := range specFiles {
		specSet[filepath.Clean(f)] = true
	}
	seen := make(map[string]bool, len(modified))
	var out []string
	for _, m := range modified {
		abs := filepath.Clean(m)
		if !filepath.IsAbs(abs) {
			abs = filepath.Clean(filepath.Join(workdir, m))
		}
		if !specSet[abs] || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, m)
	}
	return out
}
