package tool

// search_in_files_test.go 验证 TODO #31：SearchInFiles 零命中从"失败"改"成功+提示文案"，
// 不再计入单工具连续失败守卫（查无此物是有效信息不是失败）；pattern 含 | 按关键词拆分合并。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSearchFiles(t *testing.T, dir string) {
	t.Helper()
	content := "line one: everything works\nline two: ERROR happened here\nline three: warning only\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

// TestSearchInFiles_ZeroHitIsSuccess 不存在 pattern：success=true、提示文案、不计连败。
func TestSearchInFiles_ZeroHitIsSuccess(t *testing.T) {
	dir := t.TempDir()
	writeSearchFiles(t, dir)
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	res, err := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "zzz-no-such-token"})
	if err != nil {
		t.Fatalf("zero-hit search should not error: %v", err)
	}
	if !res.Success {
		t.Fatalf("zero-hit should be success (查无此物是有效信息), got success=%v", res.Success)
	}
	if !strings.Contains(res.Output, "无匹配") {
		t.Fatalf("output should carry 无匹配 hint, got: %q", res.Output)
	}

	// 连续零命中 3+ 次不触发连续失败守卫（回归：旧行为会累加连败导致 LoopExit 误杀）。
	for i := 1; i <= 4; i++ {
		res, err := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "zzz-no-such-token"})
		if err != nil || !res.Success {
			t.Fatalf("zero-hit %d should stay success: err=%v success=%v", i, err, res.Success)
		}
	}
}

// TestSearchInFiles_PipeSplitsKeywords pattern 含 | 时按关键词拆分，任意命中即记行。
func TestSearchInFiles_PipeSplitsKeywords(t *testing.T) {
	dir := t.TempDir()
	writeSearchFiles(t, dir)
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	res, err := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "ERROR|warning"})
	if err != nil || !res.Success {
		t.Fatalf("pipe search should succeed: err=%v success=%v", err, res.Success)
	}
	// a.txt 只有 ERROR 与 warning 两行命中（全小写转换后匹配）。
	if !strings.Contains(res.Output, "a.txt:2") || !strings.Contains(res.Output, "a.txt:3") {
		t.Fatalf("pipe pattern should match both keywords, got: %q", res.Output)
	}
	if strings.Contains(res.Output, "a.txt:1") {
		t.Fatalf("unrelated line leaked into result: %q", res.Output)
	}

	// 单关键词仍按字面匹配（大小写不敏感）。
	res2, _ := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "warning"})
	if !strings.Contains(res2.Output, "a.txt:3") {
		t.Fatalf("literal keyword should match, got: %q", res2.Output)
	}

	// 全部关键词都未命中 → 仍走零命中成功语义。
	res3, err := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "aaa|bbb"})
	if err != nil || !res3.Success || !strings.Contains(res3.Output, "无匹配") {
		t.Fatalf("all-keywords-miss should be zero-hit success: err=%v res=%+v", err, res3)
	}
}

// TestSearchInFiles_RealErrorStillFails 沙箱拒绝等真错误仍失败（零命中改语义不影响错误路径）。
func TestSearchInFiles_RealErrorStillFails(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	res, err := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": "x", "dir": "../..\\no-such-dir"})
	if err != nil {
		t.Fatalf("single ordinary failure should not error: %v", err)
	}
	if res.Success {
		t.Fatal("real sandbox/path error should not be success")
	}
}
