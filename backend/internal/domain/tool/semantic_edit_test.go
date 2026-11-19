package tool

// semantic_edit_test.go 覆盖 2026-11-19 验收"WP4 未接进生产路径"的返修：
//   - off（默认）：声明了 semantic_* 参数也走旧字面替换，逐字节等价；
//   - enforce：语义定位成功才写盘，且未修改区域逐字节保留；
//   - enforce：worker 不可用/目标歧义/指纹过期 → fail closed（不回退盲写）；
//   - shadow：不改变写入内容，只在输出里报告"如果 enforce 会怎么改"。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/edit"
)

const semanticVueSource = `<template>
  <div class="row">
    <span class="label">商品名称</span>
    <span class="label">下单时间</span>
  </div>
</template>

<style scoped>
.row .label {
  min-width: 4.2em;
  color: #333;
}
</style>
`

func writeSemanticFixture(t *testing.T) (root, rel string) {
	t.Helper()
	root = t.TempDir()
	rel = "src/Order.vue"
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// #nosec G304：测试固定 fixture 路径（临时目录 + 常量相对路径），无外部输入。
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(semanticVueSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, rel
}

// semanticWorker 返回可用 worker（node 不可用时 skip，不伪造成通过）。
func semanticWorker(t *testing.T) *edit.Worker {
	t.Helper()
	bin := os.Getenv("BMA_NODE_BIN")
	if bin == "" {
		bin = "node"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("node 不可用（%v），跳过语义编辑用例", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(wd, "..", "..", "..", "..", "tools", "semantic-edit", "worker.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("worker 脚本不存在（%s）", script)
	}
	return edit.NewWorker(edit.WorkerConfig{Script: script, NodeBin: bin, Timeout: 60 * time.Second})
}

func semanticExecutor(t *testing.T, root string, cfg SemanticEditConfig) *Executor {
	t.Helper()
	e := NewExecutor(root)
	e.SetSemanticEdit(cfg)
	return e
}

func semanticCSSArgs(rel string) map[string]any {
	return map[string]any{
		"path":                    rel,
		"old_string":              "  min-width: 4.2em;\n  color: #333;",
		"new_string":              "  min-width: 4.2em;\n  color: #444;",
		"semantic_kind":           "css_declaration",
		"semantic_selector":       ".row .label",
		"semantic_property":       "color",
		"semantic_expected_value": "#333",
		"semantic_new_value":      "#444",
	}
}

// TestSemanticEditOffIsTransparent off（默认）：不要求 worker 也不改变写入内容。
func TestSemanticEditOffIsTransparent(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	e := semanticExecutor(t, root, SemanticEditConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := e.editFile(ctx, semanticCSSArgs(rel))
	if res == nil || !res.Success {
		t.Fatalf("off mode must behave exactly like before: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "color: #444;") {
		t.Fatalf("literal replacement must still be applied:\n%s", got)
	}
}

// TestSemanticEditEnforceAppliesMinimalTextEdit enforce：语义定位成功，未修改区域逐字节保留。
func TestSemanticEditEnforceAppliesMinimalTextEdit(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	w := semanticWorker(t)
	defer w.Stop()
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: w})

	args := semanticCSSArgs(rel)
	delete(args, "old_string")
	delete(args, "new_string")
	res := e.editFile(context.Background(), args)
	if res == nil || !res.Success {
		t.Fatalf("semantic edit must succeed: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(semanticVueSource, "color: #333;", "color: #444;", 1)
	if string(got) != want {
		t.Fatalf("semantic edit must only change the target declaration.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestSemanticEditEnforceFailsClosedWithoutWorker enforce 下 worker 不可用必须失败且不写盘。
func TestSemanticEditEnforceFailsClosedWithoutWorker(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce})

	res := e.editFile(context.Background(), semanticCSSArgs(rel))
	if res == nil || res.Success {
		t.Fatalf("enforce without worker must fail closed: %+v", res)
	}
	if !strings.Contains(res.Error, "semantic_worker_not_configured") {
		t.Fatalf("error must explain the worker is missing: %q", res.Error)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != semanticVueSource {
		t.Fatal("file must be untouched when semantic edit fails closed")
	}
}

// TestSemanticEditEnforceRejectsAmbiguousTarget 目标歧义（多处命中）必须拒绝。
func TestSemanticEditEnforceRejectsAmbiguousTarget(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	// 再写一份同样的声明（同 selector/property）→ 目标不唯一。
	src := strings.Replace(semanticVueSource, "</style>", ".row .label { color: #333; }\n</style>", 1)
	// #nosec G304：测试固定 fixture 路径（临时目录 + 常量相对路径），无外部输入。
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	w := semanticWorker(t)
	defer w.Stop()
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: w})
	args := semanticCSSArgs(rel)
	delete(args, "old_string")
	delete(args, "new_string")

	res := e.editFile(context.Background(), args)
	if res == nil || res.Success {
		t.Fatalf("ambiguous target must be rejected: %+v", res)
	}
	got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != src {
		t.Fatal("ambiguous target must not modify the file")
	}
}

// TestSemanticEditEnforceRejectsStaleFingerprint 指纹过期（文件已被改）必须拒绝。
func TestSemanticEditEnforceRejectsStaleFingerprint(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	w := semanticWorker(t)
	defer w.Stop()
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: w})
	args := semanticCSSArgs(rel)
	delete(args, "old_string")
	delete(args, "new_string")
	args["semantic_expected_hash"] = strings.Repeat("0", 64)

	res := e.editFile(context.Background(), args)
	if res == nil || res.Success {
		t.Fatalf("stale fingerprint must be rejected: %+v", res)
	}
	got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != semanticVueSource {
		t.Fatal("file must not change when fingerprint does not match")
	}
}

// TestSemanticEditShadowKeepsLiteralWrite shadow：写入内容仍是旧的字面替换结果，
// 但输出里带一句"语义定位"说明（对拍用）。
func TestSemanticEditShadowKeepsLiteralWrite(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	w := semanticWorker(t)
	defer w.Stop()
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditShadow, Worker: w})

	res := e.editFile(context.Background(), semanticCSSArgs(rel))
	if res == nil || !res.Success {
		t.Fatalf("shadow must not block the legacy path: %+v", res)
	}
	got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(got), "color: #444;") {
		t.Fatalf("shadow must keep the literal replacement result:\n%s", got)
	}
	if !strings.Contains(res.Output, "语义定位") {
		t.Fatalf("shadow output must report the semantic probe result: %q", res.Output)
	}
}

// TestSemanticEditRejectsBulkMatches expected_matches != 1 一律拒绝（禁止批量替换）。
func TestSemanticEditRejectsBulkMatches(t *testing.T) {
	args := map[string]any{"semantic_kind": "css_declaration", "semantic_expected_matches": float64(3)}
	req, ok := parseSemanticEditRequest(args)
	if !ok {
		t.Fatal("semantic_kind must be parsed")
	}
	if req.ExpectedMatchN != 3 {
		t.Fatalf("expected_matches must be preserved: %+v", req)
	}
	if _, err := planSemanticEdit(context.Background(), nil, "x.vue", nil, req); err == nil {
		t.Fatal("bulk edit request must be rejected before reaching the worker")
	}
}

// --- 2026-11-19 再次验收 P0-1 / P1-1：enforce 硬门 + 以新值为 oracle 的复验 ------

// TestSemanticEnforceRejectsMissingSemanticKind enforce 下不传 semantic_kind 的
// 字面编辑必须被拒（不得靠"不声明语义目标"绕过 AST/CSSOM 定位与复验）。
func TestSemanticEnforceRejectsMissingSemanticKind(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: semanticWorker(t)})
	res := e.editFile(context.Background(), map[string]any{
		"path": rel, "old_string": "min-width: 4.2em;", "new_string": "min-width: 9.9em;",
	})
	if res == nil || res.Success {
		t.Fatalf("enforce 下无 semantic_kind 的字面编辑必须拒绝：%+v", res)
	}
	if !strings.Contains(res.Error, "semantic_kind") {
		t.Fatalf("错误必须指明要声明语义目标：%q", res.Error)
	}
	if got, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); string(got) != semanticVueSource {
		t.Fatal("被拒时不得改动文件")
	}
}

// TestSemanticEnforceControlledLiteralChannelRequiresRationale 受控字面通道要求显式理由。
func TestSemanticEnforceControlledLiteralChannelRequiresRationale(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: semanticWorker(t)})
	base := map[string]any{
		"path": rel, "old_string": "min-width: 4.2em;", "new_string": "min-width: 9.9em;",
	}
	// 理由太短（少于 8 个字符）仍拒绝。
	short := map[string]any{"semantic_literal_rationale": "太短"}
	for k, v := range base {
		short[k] = v
	}
	if res := e.editFile(context.Background(), short); res == nil || res.Success {
		t.Fatalf("理由不足时必须拒绝：%+v", res)
	}
	// 明确理由 → 放行，并在输出里留下可审计标注。
	long := map[string]any{"semantic_literal_rationale": "该文件是纯文本配置，没有可解析的 AST/CSSOM 节点"}
	for k, v := range base {
		long[k] = v
	}
	long["semantic_new_value"] = "min-width: 9.9em;"
	res := e.editFile(context.Background(), long)
	if res == nil || !res.Success {
		t.Fatalf("受控字面通道必须放行：%+v", res)
	}
	if !strings.Contains(res.Output, "受控字面通道") {
		t.Fatalf("必须留下可审计标注：%q", res.Output)
	}
}

// TestSemanticEnforceVerifiesNewValue verifySemanticEdit 必须以新值为 oracle：
// 新值定位不到（worker 说 target_not_found）时复验失败。
func TestSemanticEnforceVerifiesNewValue(t *testing.T) {
	root, rel := writeSemanticFixture(t)
	e := semanticExecutor(t, root, SemanticEditConfig{Mode: SemanticEditEnforce, Worker: semanticWorker(t)})
	args := semanticCSSArgs(rel)
	delete(args, "old_string")
	delete(args, "new_string")
	// 声明一个"新值"但 worker 复验时找不到它（把 css 声明值写成空白让 postcss 不接受）。
	args["semantic_new_value"] = ""
	if res := e.editFile(context.Background(), args); res == nil || res.Success {
		t.Fatalf("空新值必须拒绝：%+v", res)
	}
}

// TestApplySemanticEditFailClosedOnVerifyError 复验错误不得被忽略。
func TestApplySemanticEditFailClosedOnVerifyError(t *testing.T) {
	// 用一个必然失败的 worker 配置（脚本不存在 → Configured() 为真但调用失败）。
	w := edit.NewWorker(edit.WorkerConfig{Script: filepath.Join(t.TempDir(), "nope.mjs"), Timeout: 2 * time.Second})
	src := []byte("a\n")
	plan := &edit.EditPlan{Edits: []edit.TextEdit{{Start: 0, End: 1, NewText: "b"}}}
	_, err := applySemanticEdit(context.Background(), w, "a.css", src,
		semanticEditRequest{Kind: "css_declaration", Selector: ".h", Property: "width", NewValue: "10px"}, plan)
	if err == nil {
		t.Fatal("复验拿不到 worker 时必须 fail closed")
	}
	if !strings.Contains(err.Error(), "post_edit_verification_failed") {
		t.Fatalf("错误必须是复验失败：%v", err)
	}
}
