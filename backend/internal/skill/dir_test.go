package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// writeSkill 在 root 下的约定目录中写入一个 SKILL.md 并返回其路径。
func writeSkill(t *testing.T, root, convDir, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, convDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestLoadFromDir_ConventionDirs 验证六类约定目录均被扫描，Source/Path/Content 正确。
func TestLoadFromDir_ConventionDirs(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, ".claude", "demo", "---\nname: demo\ndescription: 演示技能\n---\n正文A")
	writeSkill(t, root, ".codex", "codex-one", "---\nname: codex-one\ndescription: codex 技能\n---\n正文B")
	writeSkill(t, root, ".agents", "agents-one", "---\nname: agents-one\ndescription: agents 技能\n---\n正文C")
	writeSkill(t, root, ".cursor", "cursor-one", "---\nname: cursor-one\ndescription: cursor 技能\n---\n正文D")
	writeSkill(t, root, ".gemini", "gemini-one", "---\nname: gemini-one\ndescription: gemini 技能\n---\n正文E")
	writeSkill(t, root, ".agent", "agent-one", "---\nname: agent-one\ndescription: agent 技能\n---\n正文F")

	p := NewPool()
	loaded, skipped := p.LoadFromDir(root)
	if loaded != 6 {
		t.Fatalf("expected 6 loaded, got %d (skipped=%v)", loaded, skipped)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	s := p.Get("demo")
	if s == nil || s.Source != "dir" || s.Content != "正文A" || s.Description != "演示技能" {
		t.Fatalf("demo skill mismatch: %+v", s)
	}
	if !strings.HasSuffix(s.Path, filepath.Join(".claude", "skills", "demo", "SKILL.md")) {
		t.Fatalf("unexpected Path: %s", s.Path)
	}
}

// TestLoadFromDir_CollisionFirstWins 验证同名技能先到先得（.claude 优先于 .codex）。
func TestLoadFromDir_CollisionFirstWins(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, ".claude", "dup", "---\nname: dup\ndescription: 来自claude\n---\nclaude正文")
	writeSkill(t, root, ".codex", "dup", "---\nname: dup\ndescription: 来自codex\n---\ncodex正文")

	p := NewPool()
	loaded, skipped := p.LoadFromDir(root)
	if loaded != 1 {
		t.Fatalf("expected 1 loaded, got %d", loaded)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "dup") {
		t.Fatalf("expected collision skip, got %v", skipped)
	}
	if got := p.Get("dup"); got == nil || got.Content != "claude正文" {
		t.Fatalf("expected claude copy to win, got %+v", got)
	}
}

// TestLoadFromDir_MissingDescriptionSkipped 验证缺 description 的技能被跳过。
func TestLoadFromDir_MissingDescriptionSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, ".claude", "bad", "---\nname: bad\n---\n正文")
	writeSkill(t, root, ".claude", "good", "---\nname: good\ndescription: 有描述\n---\n正文")

	p := NewPool()
	loaded, skipped := p.LoadFromDir(root)
	if loaded != 1 || p.Get("good") == nil {
		t.Fatalf("expected only good loaded, loaded=%d", loaded)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "bad") {
		t.Fatalf("expected bad skipped, got %v", skipped)
	}
}

// TestLoadFromDir_UnknownFrontmatterTolerated 验证 allowed-tools 等未知字段被容忍忽略。
func TestLoadFromDir_UnknownFrontmatterTolerated(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, ".claude", "rich", "---\nname: rich\ndescription: 富字段\nallowed-tools: ReadFile, RunCommand\nlicense: MIT\n---\n正文")

	p := NewPool()
	loaded, skipped := p.LoadFromDir(root)
	if loaded != 1 || len(skipped) != 0 {
		t.Fatalf("expected rich loaded cleanly, loaded=%d skipped=%v", loaded, skipped)
	}
	if got := p.Get("rich"); got == nil || got.Content != "正文" {
		t.Fatalf("rich mismatch: %+v", got)
	}
}

// TestLoadFromDir_MissingDirsOK 验证约定目录全不存在时零加载且不报错（常态）。
func TestLoadFromDir_MissingDirsOK(t *testing.T) {
	p := NewPool()
	loaded, skipped := p.LoadFromDir(t.TempDir())
	if loaded != 0 || len(skipped) != 0 {
		t.Fatalf("expected clean zero load, loaded=%d skipped=%v", loaded, skipped)
	}
}

// TestMetadataBlock 验证元数据块渲染、去重、未知名跳过与空集返回空串。
func TestMetadataBlock(t *testing.T) {
	pool := NewPoolFromSkills([]*types.Skill{
		{SkillID: "pdf_extract", Name: "pdf-extract", Description: "提取PDF表格"},
		{SkillID: "db_migrate", Name: "db-migrate", Description: "迁移清单"},
	})
	got := MetadataBlock(pool, []string{"pdf-extract", "pdf-extract", "db-migrate", "unknown"})
	if !strings.Contains(got, "【可用技能】") ||
		!strings.Contains(got, "- pdf-extract: 提取PDF表格") ||
		!strings.Contains(got, "- db-migrate: 迁移清单") ||
		strings.Contains(got, "unknown") {
		t.Fatalf("unexpected block:\n%s", got)
	}
	if idxPdf := strings.Index(got, "pdf-extract"); idxPdf < strings.Index(got, "db-migrate") {
		t.Fatalf("expected deterministic sorted order:\n%s", got)
	}
	if strings.Count(got, "pdf-extract:") != 1 {
		t.Fatalf("expected dedup, block:\n%s", got)
	}
	if MetadataBlock(pool, nil) != "" || MetadataBlock(nil, []string{"x"}) != "" {
		t.Fatalf("expected empty string for empty inputs")
	}
}
