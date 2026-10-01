package main

// main_test.go 覆盖 A2 存量清洗：双层序号剥离正确性 + dry-run 不写盘。

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanContentStripsDoubleMarkers 保守清洗：只剥 "N." 后紧跟的第二层序号/符号，
// 合法数字开头（"5. 已经带.的点"）与其余段落（验证段）不动。
func TestCleanContentStripsDoubleMarkers(t *testing.T) {
	src := strings.Join([]string{
		"---",
		"name: sample-skill",
		"---",
		"",
		"## 步骤",
		"1. 1. 运行测试",
		"2. 2、检查输出",
		"3. - 清理缓存",
		"5. 已经带.的点",
		"",
		"## 坑点",
		"1. - 误删文件",
		"- 正常坑",
		"",
		"## 验证",
		"1. 1. 验证段不动",
		"",
	}, "\n")
	fixed, changes := cleanContent([]byte(src))
	if len(changes) != 4 {
		t.Fatalf("changes = %d, want 4: %+v", len(changes), changes)
	}
	out := string(fixed)
	for _, want := range []string{"1. 运行测试", "2. 检查输出", "3. 清理缓存", "5. 已经带.的点", "1. 误删文件", "1. 1. 验证段不动"} {
		if !strings.Contains(out, want) {
			t.Errorf("cleaned content missing %q:\n%s", want, out)
		}
	}
	// 变更报告带行号与 before/after。
	if changes[0].line != 6 || changes[0].before != "1. 1. 运行测试" || changes[0].after != "1. 运行测试" {
		t.Fatalf("first change = %+v", changes[0])
	}
	// 无双层序号的文件原样返回（字节级稳定，不写盘）。
	clean, _ := cleanContent([]byte("## 步骤\n1. 正常\n"))
	if string(clean) != "## 步骤\n1. 正常\n" {
		t.Fatalf("clean file must pass through, got %q", clean)
	}
}

// TestRunDryRunDoesNotWrite dry-run 只报告不改文件；非 dry-run 才写盘。
func TestRunDryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	src := "## 步骤\n1. 1. 运行测试\n"
	path := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	files, total, err := run(dir, true, io.Discard)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if files != 1 || total != 1 {
		t.Fatalf("dry-run files=%d total=%d", files, total)
	}
	data, _ := os.ReadFile(path)
	if string(data) != src {
		t.Fatal("dry-run must not write file")
	}

	files, total, err = run(dir, false, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if files != 1 || total != 1 {
		t.Fatalf("run files=%d total=%d", files, total)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "## 步骤\n1. 运行测试\n" {
		t.Fatalf("write mode must clean file, got %q", data)
	}
}
