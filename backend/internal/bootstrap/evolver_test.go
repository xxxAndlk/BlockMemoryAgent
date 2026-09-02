package bootstrap

// evolver_test.go 覆盖进化产出解析与技能包文件工具（2026-09-02 设计 §10；
// PG 落库路径由 test 模块集成测试覆盖）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// TestParseEvolveOutput JSON 解析：围栏剥离 / 截取 / 字段映射。
func TestParseEvolveOutput(t *testing.T) {
	resp := "前缀噪声\n```json\n{\"user_prefs\":[\"偏好简洁\"],\"project_lessons\":[\"先去白底\"]," +
		"\"skills\":[{\"name\":\"FrameRender\",\"title\":\"动画帧渲染去重\",\"when_to_use\":\"渲染重复底色时\"," +
		"\"steps\":[\"预处理\"],\"pitfalls\":[\"逐帧慢\"],\"verify\":\"抽查\"}]}\n```"
	out, err := parseEvolveOutput(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.UserPrefs) != 1 || out.UserPrefs[0] != "偏好简洁" {
		t.Fatalf("user_prefs = %v", out.UserPrefs)
	}
	if len(out.ProjectLessons) != 1 || out.ProjectLessons[0] != "先去白底" {
		t.Fatalf("project_lessons = %v", out.ProjectLessons)
	}
	if len(out.Skills) != 1 {
		t.Fatalf("skills = %+v", out.Skills)
	}
	sk := out.Skills[0]
	if sk.Name != "FrameRender" || sk.Title != "动画帧渲染去重" || sk.WhenToUse != "渲染重复底色时" ||
		len(sk.Steps) != 1 || len(sk.Pitfalls) != 1 || sk.Verify != "抽查" {
		t.Fatalf("skill fields = %+v", sk)
	}
}

// TestParseEvolveOutputNoJSON 无 JSON 对象返回错误。
func TestParseEvolveOutputNoJSON(t *testing.T) {
	if _, err := parseEvolveOutput("纯文本无对象"); err == nil {
		t.Fatal("expected error for response without JSON")
	}
}

// TestNormalizeSkillName 技能名规范：非法字符折叠 / 大写转小写 / title 兜底。
func TestNormalizeSkillName(t *testing.T) {
	cases := []struct{ name, title, want string }{
		{"Frame Render", "", "frame-render"},
		{"已存在技能", "", ""}, // 非 ASCII 全折叠为空
		{"", "动画帧渲染去重", ""}, // title 非 ASCII 同样折叠为空
		{"  --a--b--  ", "", "a-b"},
		{"ok-name", "", "ok-name"},
	}
	for _, c := range cases {
		if got := normalizeSkillName(c.name, c.title); got != c.want {
			t.Errorf("normalizeSkillName(%q, %q) = %q, want %q", c.name, c.title, got, c.want)
		}
	}
}

// TestLearnedSkillMDRoundtrip 渲染 -> 解析回环：frontmatter 字段与正文还原。
func TestLearnedSkillMDRoundtrip(t *testing.T) {
	sk := agent.EvolvedSkill{
		Name: "frame-render-dedup", Title: "动画帧渲染去重", WhenToUse: "批量渲染出现重复底色时",
		Steps: []string{"统一预处理", "批量渲染"}, Pitfalls: []string{"逐帧去底慢"}, Verify: "抽查首尾帧",
	}
	content := renderLearnedSkillMD("frame-render-dedup", "动画帧渲染去重", "批量渲染出现重复底色时", "failed", sk)
	path := filepath.Join(t.TempDir(), "frame-render-dedup.md")
	if err := writeSkillFile(path, content); err != nil {
		t.Fatalf("write: %v", err)
	}
	parsed, err := parseLearnedSkillFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.SkillID != "frame-render-dedup" || parsed.Name != "动画帧渲染去重" ||
		parsed.Description != "批量渲染出现重复底色时" || parsed.Source != "learned" {
		t.Fatalf("parsed meta = %+v", parsed)
	}
	for _, want := range []string{"统一预处理", "批量渲染", "逐帧去底慢", "抽查首尾帧"} {
		if !strings.Contains(parsed.Content, want) {
			t.Errorf("parsed body missing %q, content: %q", want, parsed.Content)
		}
	}
}

// TestParseLearnedSkillFileMissingFrontmatter 缺关键字段报错（孤儿修复跳过依据）。
func TestParseLearnedSkillFileMissingFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(path, []byte("---\nname: only-name\n---\n\n正文"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := parseLearnedSkillFile(path); err == nil {
		t.Fatal("expected error for missing title/when_to_use")
	}
}
