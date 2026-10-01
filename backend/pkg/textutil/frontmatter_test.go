package textutil

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseFrontmatter_Basic 验证标准围栏解析与标量/数组折叠。
func TestParseFrontmatter_Basic(t *testing.T) {
	data := []byte("---\nname: demo\ndescription: 演示技能\ntags: [a, b]\n---\n正文第一行\n正文第二行")
	fm, body := ParseFrontmatter(data)
	if fm["name"] != "demo" || fm["description"] != "演示技能" {
		t.Fatalf("frontmatter mismatch: %#v", fm)
	}
	if fm["tags"] != "a,b" {
		t.Fatalf("expected tags folded to a,b, got %q", fm["tags"])
	}
	if body != "正文第一行\n正文第二行" {
		t.Fatalf("unexpected body: %q", body)
	}
}

// TestParseFrontmatter_CRLF 验证 Windows 换行（\r\n）下围栏解析正常。
func TestParseFrontmatter_CRLF(t *testing.T) {
	data := []byte("---\r\nname: demo\r\ndescription: d\r\n---\r\nbody\r\n")
	fm, body := ParseFrontmatter(data)
	if fm["name"] != "demo" {
		t.Fatalf("expected name=demo, got %#v", fm)
	}
	if strings.Contains(body, "---") {
		t.Fatalf("body should not contain fence: %q", body)
	}
}

// TestParseFrontmatter_NoFrontmatter 验证无围栏时返回 nil map 与全文。
func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	text := "直接正文，没有围栏"
	fm, body := ParseFrontmatter([]byte(text))
	if fm != nil || body != text {
		t.Fatalf("expected nil map + full text, got %#v %q", fm, body)
	}
}

// TestParseFrontmatter_UnclosedFence 验证围栏未闭合时按无 frontmatter 处理。
func TestParseFrontmatter_UnclosedFence(t *testing.T) {
	text := "---\nname: demo\n没有闭合围栏"
	fm, body := ParseFrontmatter([]byte(text))
	if fm != nil || body != text {
		t.Fatalf("expected nil map + full text, got %#v %q", fm, body)
	}
}

// TestSanitizeID 验证 ID 规整规则（大写折叠、非法字符转下划线）。
func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"PDF-Extract.v2": "pdf_extract_v2",
		"中文 skill":      "___skill",
		"already_ok":     "already_ok",
	}
	for in, want := range cases {
		if got := SanitizeID(in); got != want {
			t.Errorf("SanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseFrontmatter_UnknownFieldsTolerated 验证未知字段进入 map（由调用方忽略）。
func TestParseFrontmatter_UnknownFieldsTolerated(t *testing.T) {
	fm, _ := ParseFrontmatter([]byte("---\nname: x\ndescription: y\nallowed-tools: A, B\n---\nbody"))
	if !reflect.DeepEqual(fm["allowed-tools"], "A, B") {
		t.Fatalf("expected unknown field preserved, got %#v", fm)
	}
}

// ---- TODO 25 阶段 C1：tools 字段与目录式路径解析 ----

// TestRenderSkillFrontmatterToolsRoundtrip tools 渲染 -> 解析回环（C1）。
func TestRenderSkillFrontmatterToolsRoundtrip(t *testing.T) {
	tools := []SkillTool{
		{Path: "scripts/check.py", Desc: "冒烟检查产物", Run: "python scripts/check.py"},
		{Path: "scripts/run.sh", Desc: "批量执行"},
	}
	head := RenderSkillFrontmatter("img-dedup", "图像去重", "批量渲染重复时", "success", tools)
	if !strings.Contains(head, "tools:\n") ||
		!strings.Contains(head, "- path: scripts/check.py") ||
		!strings.Contains(head, "run: python scripts/check.py") {
		t.Fatalf("frontmatter missing tools block:\n%s", head)
	}
	// desc 在 run 缺省时仍可解析。
	parsed := ParseSkillTools([]byte(head))
	if len(parsed) != 2 || parsed[0] != tools[0] || parsed[1].Path != "scripts/run.sh" || parsed[1].Run != "" {
		t.Fatalf("parsed tools = %+v", parsed)
	}
}

// TestRenderSkillFrontmatterNoToolsUnchanged 不传 tools 时输出与 C 阶段前完全一致（调用方兼容）。
func TestRenderSkillFrontmatterNoToolsUnchanged(t *testing.T) {
	got := RenderSkillFrontmatter("n", "标题", "场景", "success")
	want := "---\nname: n\ntitle: 标题\nwhen_to_use: 场景\noutcome: success\n---\n"
	if got != want {
		t.Fatalf("frontmatter changed:\n%s\nwant:\n%s", got, want)
	}
	if ParseSkillTools([]byte(got)) != nil {
		t.Fatal("no tools must parse to nil")
	}
}

// TestParseSkillToolsTolerant 缺 tools/缺 path/坏 YAML 宽容返回（读取侧不炸）。
func TestParseSkillToolsTolerant(t *testing.T) {
	if got := ParseSkillTools([]byte("正文无 frontmatter")); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	got := ParseSkillTools([]byte("---\nname: x\ntools:\n  - desc: 无path\n  - path: scripts/a.py\n    desc: ok\n---\n"))
	if len(got) != 1 || got[0].Path != "scripts/a.py" {
		t.Fatalf("tolerant parse = %+v", got)
	}
}

// TestSkillToolSection 渲染「配套工具」段：路径 + run + desc。
func TestSkillToolSection(t *testing.T) {
	sec := SkillToolSection([]SkillTool{{Path: "scripts/check.py", Desc: "检查产物", Run: "python scripts/check.py"}})
	if !strings.Contains(sec, "## 配套工具") ||
		!strings.Contains(sec, "`scripts/check.py`") ||
		!strings.Contains(sec, "`python scripts/check.py`") ||
		!strings.Contains(sec, "检查产物") {
		t.Fatalf("section = %q", sec)
	}
	if SkillToolSection(nil) != "" {
		t.Fatal("empty tools must render empty section")
	}
}

// TestResolveSkillMDPath 目录式优先、老 .md 回退、皆无则返回目录式（新建写目标）。
func TestResolveSkillMDPath(t *testing.T) {
	dir := t.TempDir()
	// 仅老格式。
	oldPath := filepath.Join(dir, "legacy.md")
	if err := os.WriteFile(oldPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveSkillMDPath(dir, "legacy"); got != oldPath {
		t.Fatalf("legacy fallback = %q, want %q", got, oldPath)
	}
	// 两种并存：目录式优先。
	if err := os.MkdirAll(filepath.Join(dir, "both"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(dir, "both", "SKILL.md")
	if err := os.WriteFile(newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "both.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveSkillMDPath(dir, "both"); got != newPath {
		t.Fatalf("dir-format priority = %q, want %q", got, newPath)
	}
	// 皆不存在：返回目录式路径。
	want := filepath.Join(dir, "fresh", "SKILL.md")
	if got := ResolveSkillMDPath(dir, "fresh"); got != want {
		t.Fatalf("fresh target = %q, want %q", got, want)
	}
}

// TestResolveSkillMDPathFromRef PG content_path 新老格式引用统一归一。
func TestResolveSkillMDPathFromRef(t *testing.T) {
	dir := t.TempDir()
	// 老引用（<name>.md）指向的 skills 已搬迁为目录式。
	if err := os.MkdirAll(filepath.Join(dir, "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(dir, "moved", "SKILL.md")
	if err := os.WriteFile(newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveSkillMDPathFromRef(filepath.Join(dir, "moved.md")); got != newPath {
		t.Fatalf("from old ref = %q, want %q", got, newPath)
	}
	// 新引用（<name>/SKILL.md）原样解析。
	if got := ResolveSkillMDPathFromRef(newPath); got != newPath {
		t.Fatalf("from new ref = %q, want %q", got, newPath)
	}
	// 都不存在：回退目录式写目标。
	want := filepath.Join(dir, "gone", "SKILL.md")
	if got := ResolveSkillMDPathFromRef(filepath.Join(dir, "gone.md")); got != want {
		t.Fatalf("missing ref = %q, want %q", got, want)
	}
}
