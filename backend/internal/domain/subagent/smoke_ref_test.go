package subagent

// smoke_ref_test.go 测试 TODO #71 JS/HTML 引用完整性档：
// tsc 档硬判、轻量扫描标存疑、行数阈值、HTML 内联 script 提取。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// longJS 生成长 JS 内容（> jsRefCheckMinLines 行），可选注入未定义调用。
func longJS(undefinedCall string) string {
	var b strings.Builder
	b.WriteString("function playSwoosh() { return 1; }\n")
	for i := 0; i < jsRefCheckMinLines+10; i++ {
		b.WriteString("const v")
		b.WriteString(strings.ReplaceAll(strings.Repeat("x", 3), "x", ""))
		b.WriteString(" = ")
		b.WriteString("0;\n")
	}
	if undefinedCall != "" {
		b.WriteString(undefinedCall)
		b.WriteString("();\n")
	}
	return b.String()
}

func TestRunJSReferenceCheck_TSCFail(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "game.js")
	if err := os.WriteFile(f, []byte(longJS("playSwooshSound")), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// tsc 可用：注入假 runner 返回 exit 1（Cannot find name 类）。
	lookPath := func(string) (string, error) { return "tsc", nil }
	runner := func(ctx context.Context, wd, name string, args ...string) (string, int, error) {
		return "game.js(320,1): error TS2304: Cannot find name 'playSwooshSound'", 1, nil
	}
	note, failed := runJSReferenceCheck(context.Background(), dir, f, runner, lookPath)
	if !failed {
		t.Fatalf("tsc exit 1 should hard-fail, note=%q", note)
	}
	if !strings.Contains(note, "引用完整性") || !strings.Contains(note, "playSwooshSound") {
		t.Fatalf("note should carry tsc output, got %q", note)
	}

	// tsc 通过（exit 0）：无报告不失败。
	runnerOK := func(ctx context.Context, wd, name string, args ...string) (string, int, error) {
		return "", 0, nil
	}
	note, failed = runJSReferenceCheck(context.Background(), dir, f, runnerOK, lookPath)
	if failed || note != "" {
		t.Fatalf("tsc pass should be silent, got failed=%v note=%q", failed, note)
	}
}

func TestRunJSReferenceCheck_LightweightFallback(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "game.js")
	// 未定义调用 playSwooshSound（定义的是 playSwoosh）。
	if err := os.WriteFile(f, []byte(longJS("playSwooshSound")), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// tsc 不可用 -> 轻量扫描档。
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	note, failed := runJSReferenceCheck(context.Background(), dir, f, nil, lookPath)
	if failed {
		t.Fatal("lightweight scan must not hard-fail (存疑语义)")
	}
	if !strings.Contains(note, "存疑") || !strings.Contains(note, "playSwooshSound") {
		t.Fatalf("lightweight note should flag suspect, got %q", note)
	}
}

func TestRunJSReferenceCheck_SmallFileSkipped(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "small.js")
	// < 300 行：跳过（即使含未定义调用）。
	small := "function playSwoosh(){}\nplaySwooshSound();\n"
	if err := os.WriteFile(f, []byte(small), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	note, failed := runJSReferenceCheck(context.Background(), dir, f, nil, lookPath)
	if failed || note != "" {
		t.Fatalf("small file should be skipped, got failed=%v note=%q", failed, note)
	}
}

func TestRunJSReferenceCheck_HTMLInlineScript(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "index.html")
	// 内联 script 含未定义调用；外链 script（src）跳过。
	var b strings.Builder
	b.WriteString("<html><body>\n")
	b.WriteString("<script src=\"lib.js\"></script>\n")
	b.WriteString("<script>\n")
	b.WriteString(longJS("playSwooshSound"))
	b.WriteString("</script>\n")
	b.WriteString("</body></html>\n")
	if err := os.WriteFile(f, []byte(b.String()), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	note, failed := runJSReferenceCheck(context.Background(), dir, f, nil, lookPath)
	if failed {
		t.Fatal("lightweight scan must not hard-fail")
	}
	if !strings.Contains(note, "playSwooshSound") {
		t.Fatalf("inline script suspect should be flagged, got %q", note)
	}
}

func TestExtractInlineScripts(t *testing.T) {
	html := `<html>
<script src="a.js"></script>
<script>
function f(){ return 1; }
</script>
<script>
function g(){ return 2; }
</script>
</html>`
	got := extractInlineScripts(html)
	if !strings.Contains(got, "function f()") || !strings.Contains(got, "function g()") {
		t.Fatalf("inline scripts missing: %q", got)
	}
	if strings.Contains(got, "a.js") {
		t.Fatalf("external src script should be excluded: %q", got)
	}
}

func TestScanUndefinedCalls(t *testing.T) {
	// playSwooshSound 调用无定义 -> 存疑；playSwoosh 有定义 -> 放行。
	content := "function playSwoosh(){ return 1; }\n" + longJS("playSwooshSound")
	suspects := scanUndefinedCalls(content)
	found := false
	for _, s := range suspects {
		if s == "playSwooshSound" {
			found = true
		}
		if s == "playSwoosh" {
			t.Fatalf("defined function must not be flagged: %v", suspects)
		}
	}
	if !found {
		t.Fatalf("undefined call should be flagged: %v", suspects)
	}
	// 全局白名单放行（setTimeout 等）。
	clean := longJS("") + "setTimeout(function(){}, 100);\nrequestAnimationFrame(step);\n"
	for _, s := range scanUndefinedCalls(clean) {
		t.Fatalf("globals should be whitelisted, got suspect %q", s)
	}
}

func TestRunJSRefChecks_Aggregation(t *testing.T) {
	dir := t.TempDir()
	// .js 命中、.txt 跳过。
	jsF := filepath.Join(dir, "a.js")
	txtF := filepath.Join(dir, "note.txt")
	_ = os.WriteFile(jsF, []byte(longJS("playSwooshSound")), 0644)
	_ = os.WriteFile(txtF, []byte("text"), 0644)
	lookPath := func(string) (string, error) { return "", errors.New("nf") }
	note, failed := runJSRefChecks(context.Background(), dir, []string{jsF, txtF}, nil, lookPath)
	if failed {
		t.Fatal("lightweight aggregation should not hard-fail")
	}
	if !strings.Contains(note, "a.js") || strings.Contains(note, "note.txt") {
		t.Fatalf("aggregation wrong: %q", note)
	}
}
