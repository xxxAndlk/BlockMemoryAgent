package subagent

// smoke_check_test.go 覆盖域完成机器校验冒烟层（TODO #56）：
// 命令派生、工具链缺失降级跳过、输出截断、gofmt 输出判定、目标交集收敛、
// 失败反馈重试打回链路。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSmokeRunner 记录调用并按文件名/命令返回预设结果，不执行真实命令。
type fakeSmokeRunner struct {
	calls  []string
	byCmd  map[string]string // cmdline 后缀匹配 -> "exit:0"/"exit:1"/"err:msg"
	byFile map[string]string // 文件名 -> 同上
}

func (f *fakeSmokeRunner) run(ctx context.Context, dir, name string, args ...string) (string, int, error) {
	cmdline := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmdline)
	for key, spec := range f.byCmd {
		if strings.Contains(cmdline, key) {
			return f.apply(spec, cmdline)
		}
	}
	for key, spec := range f.byFile {
		if strings.Contains(cmdline, key) {
			return f.apply(spec, cmdline)
		}
	}
	return "ok", 0, nil
}

func (f *fakeSmokeRunner) apply(spec, cmdline string) (string, int, error) {
	switch {
	case strings.HasPrefix(spec, "err:"):
		return "", -1, fmt.Errorf("%s", strings.TrimPrefix(spec, "err:"))
	case spec == "exit:1":
		return cmdline + " failed with syntax error", 1, nil
	case spec == "exit:1-long":
		return strings.Repeat("error line\n", 1000), 1, nil
	case spec == "exit:0-with-output":
		return "some output", 0, nil
	default:
		return "", 0, nil
	}
}

// fakeLookPath 模拟工具链探测；missing 集合中的二进制返回错误。
func fakeLookPath(missing ...string) lookPathFunc {
	set := map[string]bool{}
	for _, m := range missing {
		set[m] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "", fmt.Errorf("%s not found", name)
		}
		return "/usr/bin/" + name, nil
	}
}

// writeSmokeFixture 在工作目录下写文件，返回绝对路径。
func writeSmokeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestSmokeCommandDerivation 多文件派生命令正确（.js/.go/.ts 各自命令；未知扩展跳过）。
func TestSmokeCommandDerivation(t *testing.T) {
	dir := t.TempDir()
	js := writeSmokeFixture(t, dir, "a.js", "x")
	goF := writeSmokeFixture(t, dir, "b.go", "x")
	ts := writeSmokeFixture(t, dir, "c.ts", "x")
	md := writeSmokeFixture(t, dir, "d.md", "x")

	runner := &fakeSmokeRunner{}
	results := runSmokeChecks(context.Background(), dir, []string{js, goF, ts, md}, runner.run, fakeLookPath())

	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	if results[0].cmdline != "node --check "+js {
		t.Fatalf("js cmdline = %q", results[0].cmdline)
	}
	if results[1].cmdline != "gofmt -l "+goF {
		t.Fatalf("go cmdline = %q", results[1].cmdline)
	}
	if results[2].cmdline != "tsc --noEmit "+ts {
		t.Fatalf("ts cmdline = %q", results[2].cmdline)
	}
	if !results[3].skipped || results[3].skipReason != "无可派生冒烟命令" {
		t.Fatalf("md should skip with derive reason, got skipped=%v reason=%q", results[3].skipped, results[3].skipReason)
	}
}

// TestSmokeToolchainMissing 工具链缺失（node 不存在）降级跳过并标注。
func TestSmokeToolchainMissing(t *testing.T) {
	dir := t.TempDir()
	js := writeSmokeFixture(t, dir, "a.js", "x")

	results := runSmokeChecks(context.Background(), dir, []string{js}, (&fakeSmokeRunner{}).run, fakeLookPath("node"))
	if len(results) != 1 || !results[0].skipped {
		t.Fatalf("expected skip when node missing, got %+v", results)
	}
	if !strings.Contains(results[0].skipReason, "node") {
		t.Fatalf("skip reason should name missing toolchain, got %q", results[0].skipReason)
	}
}

// TestSmokeMissingFile 文件不存在时跳过并标注（不视为失败）。
func TestSmokeMissingFile(t *testing.T) {
	dir := t.TempDir()
	results := runSmokeChecks(context.Background(), dir, []string{filepath.Join(dir, "nope.js")}, (&fakeSmokeRunner{}).run, fakeLookPath())
	if len(results) != 1 || !results[0].skipped {
		t.Fatalf("expected skip when file missing, got %+v", results)
	}
}

// TestSmokeFailureOutputTruncation 失败输出按尾部截断，防超长编译错误撑爆摘要。
func TestSmokeFailureOutputTruncation(t *testing.T) {
	dir := t.TempDir()
	js := writeSmokeFixture(t, dir, "bad.js", "x")
	runner := &fakeSmokeRunner{byFile: map[string]string{"bad.js": "exit:1-long"}}

	results := runSmokeChecks(context.Background(), dir, []string{js}, runner.run, fakeLookPath())
	if len(results) != 1 || !results[0].failed() {
		t.Fatalf("expected failure, got %+v", results)
	}
	if len([]rune(results[0].output)) > smokeOutputTailRunes+20 {
		t.Fatalf("output not truncated: %d runes", len([]rune(results[0].output)))
	}
	if !strings.HasPrefix(results[0].output, "…(截断)") {
		t.Fatalf("expected truncation marker, got %q", results[0].output[:40])
	}
}

// TestSmokeGofmtOutputMeansFail gofmt -l exit 0 + 非空输出 = 未格式化，判失败。
func TestSmokeGofmtOutputMeansFail(t *testing.T) {
	dir := t.TempDir()
	goF := writeSmokeFixture(t, dir, "ugly.go", "x")
	runner := &fakeSmokeRunner{byFile: map[string]string{"ugly.go": "exit:0-with-output"}}

	results := runSmokeChecks(context.Background(), dir, []string{goF}, runner.run, fakeLookPath())
	if len(results) != 1 || !results[0].failed() {
		t.Fatalf("gofmt non-empty output should fail, got %+v", results)
	}
}

// TestSmokeTargets 目标收敛：spec.files ∩ 修改文件，相对/绝对路径同基准归一化，去重。
func TestSmokeTargets(t *testing.T) {
	workdir := t.TempDir()
	specFiles := []string{
		filepath.Join(workdir, "a.js"),
		filepath.Join(workdir, "b.js"),
	}
	modified := []string{
		"a.js",                             // 相对路径（工作目录基准）
		filepath.Join(workdir, "b.js"),     // 绝对路径
		filepath.Join(workdir, "other.js"), // 不在 spec 范围
	}
	got := smokeTargets(specFiles, modified, workdir)
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %v", got)
	}
	if got[0] != "a.js" || got[1] != filepath.Join(workdir, "b.js") {
		t.Fatalf("unexpected targets: %v", got)
	}

	if got := smokeTargets(specFiles, nil, workdir); got != nil {
		t.Fatalf("no modified files should yield nil, got %v", got)
	}
	if got := smokeTargets(nil, modified, workdir); got != nil {
		t.Fatalf("no spec files should yield nil, got %v", got)
	}
}

// TestSmokeFixMessage 反馈重试消息含命令 + 退出码 + 输出尾部。
func TestSmokeFixMessage(t *testing.T) {
	failed := []smokeResult{{file: "a.js", cmdline: "node --check a.js", exitCode: 1, output: "boom"}}
	msg := smokeFixMessage(failed)
	for _, want := range []string{"【机器校验失败】", "a.js", "node --check a.js", "exit 1", "boom"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("fix message missing %q: %s", want, msg)
		}
	}
}

// TestSmokeReport 报告含 dispatcher 执行标注与跳过原因。
func TestSmokeReport(t *testing.T) {
	results := []smokeResult{
		{file: "a.js", cmdline: "node --check a.js", exitCode: 0},
		{file: "b.go", skipped: true, skipReason: "工具链 gofmt 不可用"},
	}
	rep := renderSmokeReport(results)
	for _, want := range []string{"【机器校验】", "非 agent 自述", "通过", "跳过（工具链 gofmt 不可用）"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report missing %q: %s", want, rep)
		}
	}
}
