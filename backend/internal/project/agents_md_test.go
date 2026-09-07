// agents_md_test.go 覆盖 TODO 第10项⑦ 项目自述加载：
// AGENTS.md 优先、CLAUDE.md 兜底、maxRunes 截断、mtime+size 缓存失效、缺失/关闭零注入。
package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadProjectBrief_Preference 两者并存时 AGENTS.md 优先；仅 CLAUDE.md 时兜底。
func TestLoadProjectBrief_Preference(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief(dir, 1000); got != "agents doc" {
		t.Fatalf("AGENTS.md must win, got %q", got)
	}
	// 删掉 AGENTS.md 后落回 CLAUDE.md。
	if err := os.Remove(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief(dir, 1000); got != "claude doc" {
		t.Fatalf("CLAUDE.md fallback expected, got %q", got)
	}
}

// TestLoadProjectBrief_Truncate 超限截断并附标记。
func TestLoadProjectBrief_Truncate(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("字", 500)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadProjectBrief(dir, 100)
	if !strings.HasPrefix(got, strings.Repeat("字", 100)) {
		t.Fatalf("truncated prefix mismatch, len=%d", len([]rune(got)))
	}
	if !strings.Contains(got, "已截断") {
		t.Fatalf("truncation marker missing: %q", got)
	}
	// 不超限原样返回。
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief(dir, 1000); got != "short" {
		t.Fatalf("short file must pass through, got %q", got)
	}
}

// TestLoadProjectBrief_CacheInvalidation 修改文件（size 变化）后缓存自动失效。
func TestLoadProjectBrief_CacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief(dir, 1000); got != "v1" {
		t.Fatalf("first read = %q", got)
	}
	// size 不同 → 指纹必失效（不依赖 mtime 粒度）。
	if err := os.WriteFile(p, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief(dir, 1000); got != "v2-longer" {
		t.Fatalf("cache must invalidate on change, got %q", got)
	}
	// 同指纹重复读返回缓存（不重读也不炸）。
	time.Sleep(2 * time.Millisecond)
	if got := LoadProjectBrief(dir, 1000); got != "v2-longer" {
		t.Fatalf("cached read = %q", got)
	}
}

// TestLoadProjectBrief_MissingOrDisabled 缺失/关闭返回空串。
func TestLoadProjectBrief_MissingOrDisabled(t *testing.T) {
	dir := t.TempDir()
	if got := LoadProjectBrief(dir, 1000); got != "" {
		t.Fatalf("missing files must return empty, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectBrief("", 1000); got != "" {
		t.Fatalf("empty workDir must return empty, got %q", got)
	}
	if got := LoadProjectBrief(dir, 0); got != "" {
		t.Fatalf("disabled (<=0) must return empty, got %q", got)
	}
}
