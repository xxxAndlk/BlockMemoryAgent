package edit

// worker_test.go Go 侧语义编辑客户端测试（不需要 node 也能跑的部分）：
//   - utf16 → 字节偏移换算（含 CJK 与 surrogate pair）；
//   - 只允许单条 TextEdit（批量编辑被拒绝）；
//   - 越界范围/半代理位置安全失败；
//   - 未配置 worker 时明确报错（不做静默回退）。
//
// 依赖 node 的端到端用例走 `requireNode` 守卫：环境无 node 时 skip 并说明，
// 不把"没跑"写成"通过"。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyUTF16Edits_ASCII(t *testing.T) {
	// worker 的 CSS 声明范围是**含结尾分号**的（postcss source.end 口径）：
	// TextEdit 必须把分号一并替换，否则会把下一条声明粘坏。
	src := []byte("width: 360px; margin-left: 30px;\n")
	plan := &EditPlan{Edits: []TextEdit{{Start: 7, End: 13, NewText: "500px;"}}}
	out, err := ApplyUTF16Edits(src, plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "width: 500px; margin-left: 30px;\n" {
		t.Fatalf("got %q", out)
	}
}

func TestApplyUTF16Edits_CJKContent(t *testing.T) {
	// "收货人：张三" —— 中文占 3 字节但 1 个 utf16 code unit。
	src := []byte("<span>收货人：张三</span>")
	// 替换 "张三"（起点在 utf16 偏移 10：<span> 是 6 个单元 + 收货人： 是 4 个单元）。
	plan := &EditPlan{Edits: []TextEdit{{Start: 10, End: 12, NewText: "李四"}}}
	out, err := ApplyUTF16Edits(src, plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "<span>收货人：李四</span>" {
		t.Fatalf("got %q", out)
	}
}

func TestApplyUTF16Edits_SurrogatePair(t *testing.T) {
	// emoji 占 4 字节 UTF-8、2 个 utf16 code unit。
	src := []byte("a😀b")
	plan := &EditPlan{Edits: []TextEdit{{Start: 1, End: 3, NewText: "X"}}}
	out, err := ApplyUTF16Edits(src, plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "aXb" {
		t.Fatalf("got %q", out)
	}
}

func TestApplyUTF16Edits_RejectsSplitSurrogate(t *testing.T) {
	src := []byte("a😀b")
	// 偏移 2 落在 surrogate pair 中间：必须报错，不得写出半个字符。
	plan := &EditPlan{Edits: []TextEdit{{Start: 2, End: 3, NewText: "X"}}}
	if _, err := ApplyUTF16Edits(src, plan); err == nil {
		t.Fatal("splitting a surrogate pair must fail")
	}
}

func TestApplyUTF16Edits_RejectsBulkEdit(t *testing.T) {
	src := []byte("a")
	plan := &EditPlan{Edits: []TextEdit{{Start: 0, End: 1, NewText: "b"}, {Start: 0, End: 1, NewText: "c"}}}
	if _, err := ApplyUTF16Edits(src, plan); err == nil {
		t.Fatal("multiple edits must be rejected (bulk replace is forbidden)")
	}
}

func TestApplyUTF16Edits_RejectsOutOfRange(t *testing.T) {
	src := []byte("abc")
	plan := &EditPlan{Edits: []TextEdit{{Start: 0, End: 99, NewText: "x"}}}
	if _, err := ApplyUTF16Edits(src, plan); err == nil {
		t.Fatal("out of range edit must fail")
	}
}

func TestWorker_NotConfiguredFailsClosed(t *testing.T) {
	w := NewWorker(WorkerConfig{})
	if w.Configured() {
		t.Fatal("empty script must not be configured")
	}
	_, err := w.Call(context.Background(), "ping", nil)
	if err == nil {
		t.Fatal("unconfigured worker must return an explicit error")
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Fatalf("error must name the worker: %v", err)
	}
}

// requireNode 返回 node 可执行文件；不可用时 skip（并说明未跑，不伪造成通过）。
func requireNode(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("BMA_NODE_BIN")
	if bin == "" {
		bin = "node"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("node 不可用（%v），跳过 worker 端到端用例", err)
	}
	return bin
}

// workerScript 定位仓库内 worker 脚本（测试从 backend/internal/domain/edit 起算）。
func workerScript(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// backend/internal/domain/edit -> repo root
	root := filepath.Join(wd, "..", "..", "..", "..")
	script := filepath.Join(root, "tools", "semantic-edit", "worker.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("worker 脚本不存在（%s），跳过端到端用例", script)
	}
	return script
}

func TestWorker_EndToEndCSSPlan(t *testing.T) {
	bin := requireNode(t)
	script := workerScript(t)
	w := NewWorker(WorkerConfig{Script: script, NodeBin: bin, Timeout: 60 * time.Second})
	defer w.Stop()

	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	if w.Version() == "" {
		t.Fatal("ping must report a version")
	}
	src := "a\n<style>\n.h { width: 360px; margin-left: 30px; }\n</style>\n"
	plan, err := w.PlanCSSEdit(ctx, CSSDeclarationParams{
		File: "x.vue", Source: src, Selector: ".h", Property: "width",
		ExpectedValue: "360px", NewValue: "420px",
	})
	if err != nil {
		t.Fatalf("plan css edit: %v", err)
	}
	out, err := ApplyUTF16Edits([]byte(src), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(out), ".h { width: 420px; margin-left: 30px; }") {
		t.Fatalf("unexpected result:\n%s", out)
	}
}
