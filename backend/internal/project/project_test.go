package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestProject 在临时目录构造一个最小多语言项目骨架供扫描测试。
func writeTestProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mustWrite("go.mod", "module github.com/example/demo\n\ngo 1.25\n")
	mustWrite("Makefile", "run:\n\tgo run ./cmd\n\ntest:\n\tgo test ./...\n\nVAR := x\n")
	mustWrite("README.md", "# demo\n")
	mustWrite("CLAUDE.md", "# guide\n")
	mustWrite("backend/cmd/main.go", "package main\nfunc main() {}\n")
	mustWrite("backend/internal/foo/foo.go", "package foo\n")
	mustWrite("doc/design.md", "# design\n")
	mustWrite("node_modules/skip.js", "// skip\n") // 应被跳过
	mustWrite(".git/config", "[core]\n")            // 应被跳过
	return root
}

func TestEnsureProjectDoc_GeneratesAndIdempotent(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(root); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := ProjectDocPath(root)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, ManagedBegin) || !strings.Contains(s, ManagedEnd) {
		t.Fatalf("generated file missing managed markers")
	}
	if !strings.Contains(s, "github.com/example/demo") {
		t.Fatalf("generated file missing module name")
	}
	if !strings.Contains(s, "Go 版本: 1.25") {
		t.Fatalf("generated file missing go version")
	}
	if !strings.Contains(s, "`make run`") {
		t.Fatalf("generated file missing make target run")
	}
	if !strings.Contains(s, "### `backend/` - 后端服务") {
		t.Fatalf("generated file missing backend domain entry")
	}
	if !strings.Contains(s, "## 文档地图") || !strings.Contains(s, "doc/design.md") {
		t.Fatalf("generated file missing doc map")
	}
	if strings.Contains(s, "node_modules") {
		t.Fatalf("node_modules should be skipped but appeared in output")
	}

	// 二次调用幂等：不覆盖。
	before, _ := os.ReadFile(p)
	if err := EnsureProjectDoc(root); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatalf("Ensure not idempotent: file changed on second call")
	}
}

func TestLoadProjectDoc_ReturnsManagedBody(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(root); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	body := LoadProjectDoc(root)
	if body == "" {
		t.Fatal("Load returned empty for existing doc")
	}
	if strings.Contains(body, ManagedBegin) || strings.Contains(body, ManagedEnd) {
		t.Fatalf("Load should return body without markers")
	}
	if !strings.Contains(body, "项目概览") {
		t.Fatalf("Load body missing title")
	}
}

func TestLoadProjectDoc_EmptyWhenMissing(t *testing.T) {
	root := t.TempDir()
	if body := LoadProjectDoc(root); body != "" {
		t.Fatalf("expected empty load for missing doc, got %q", body)
	}
}

func TestRefreshProjectDoc_PreservesHumanEdits(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(root); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := ProjectDocPath(root)
	// 追加人手补充到标记区外。
	orig, _ := os.ReadFile(p)
	humanNote := "\n## 人手补充\n\n这是 Agent 不会覆盖的备注。\n"
	if err := os.WriteFile(p, append([]byte(string(orig)), []byte(humanNote)...), 0o644); err != nil {
		t.Fatalf("append human note: %v", err)
	}
	// 新增一个顶层目录，验证 Refresh 重写 managed 区能反映结构变化。
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "build.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write build.sh: %v", err)
	}

	if err := RefreshProjectDoc(root); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	refreshed, _ := os.ReadFile(p)
	s := string(refreshed)
	if !strings.Contains(s, "人手补充") || !strings.Contains(s, "这是 Agent 不会覆盖的备注。") {
		t.Fatalf("Refresh lost human note outside managed region")
	}
	if !strings.Contains(s, "### `scripts/` - 脚本") {
		t.Fatalf("Refresh did not pick up new scripts/ domain")
	}
	// 标记区应仍只出现一次。
	if c := strings.Count(s, ManagedBegin); c != 1 {
		t.Fatalf("expected 1 managed begin marker, got %d", c)
	}
}

func TestRefreshProjectDoc_CreatesWhenMissing(t *testing.T) {
	root := writeTestProject(t)
	// 不先 Ensure，直接 Refresh 应等价于生成。
	if err := RefreshProjectDoc(root); err != nil {
		t.Fatalf("Refresh on missing: %v", err)
	}
	if _, err := os.ReadFile(ProjectDocPath(root)); err != nil {
		t.Fatalf("Refresh did not create file: %v", err)
	}
}

func TestRefreshProjectDoc_NoMarkersPrepends(t *testing.T) {
	root := writeTestProject(t)
	p := ProjectDocPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	humanOnly := "# 我手写的 PROJECT\n\n纯人手内容，无标记。\n"
	if err := os.WriteFile(p, []byte(humanOnly), 0o644); err != nil {
		t.Fatalf("write human-only: %v", err)
	}
	if err := RefreshProjectDoc(root); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s, _ := os.ReadFile(p)
	body := string(s)
	if !strings.Contains(body, ManagedBegin) {
		t.Fatalf("Refresh should insert managed block into markerless file")
	}
	if !strings.Contains(body, "纯人手内容，无标记。") {
		t.Fatalf("Refresh should preserve existing human content when no markers")
	}
	// managed 区在前，人手内容在后。
	if strings.Index(body, ManagedEnd) > strings.Index(body, "纯人手内容") {
		t.Fatalf("managed block should be prepended before existing human content")
	}
}
