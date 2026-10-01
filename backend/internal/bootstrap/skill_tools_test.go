package bootstrap

// skill_tools_test.go 覆盖 TODO 25 阶段 C 后端：
// C1 老格式 .md 回退读 + 新格式 roundtrip + frontmatter tools 解析；
// C3 带 tools 技能落盘（scripts 文件/frontmatter tools/has_tools）+ 冒烟失败降级 + 无运行时降级；
// C2 加载注入「配套工具」段（Content 含 run 命令）。
// 冒烟执行经 skillToolLookPath/skillSmokeRun 注入替身，测试不依赖本机 python/bash。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// stubSkillRuntime 注入运行时查找与冒烟执行替身。
func stubSkillRuntime(t *testing.T, exe string, lookErr error, smokeErr error) {
	t.Helper()
	oldLook, oldSmoke := skillToolLookPath, skillSmokeRun
	skillToolLookPath = func(string) (string, error) {
		if lookErr != nil {
			return "", lookErr
		}
		return exe, nil
	}
	skillSmokeRun = func(context.Context, string, string) error { return smokeErr }
	t.Cleanup(func() { skillToolLookPath, skillSmokeRun = oldLook, oldSmoke })
}

func toolSkill() agent.EvolvedSkill {
	return agent.EvolvedSkill{
		Name: "img-dedup", Title: "图像去重", WhenToUse: "批量渲染出现重复底色时",
		Steps: []string{"运行去重脚本"}, Verify: "抽查首尾帧",
		Tools: []agent.EvolvedSkillTool{{
			Filename: "scripts/dedup.py", Language: "py", Code: "print('dedup')\n", Desc: "按哈希去重",
		}},
	}
}

// ---- C3：带 tools 技能落盘（冒烟通过） ----

func TestPersistOneWithToolsSolidifiesScripts(t *testing.T) {
	stubSkillRuntime(t, "python", nil, nil)
	st := newFakeSkillStore()
	dir := t.TempDir()
	s := newTestSink(st, dir)
	if err := s.persistOne(context.Background(), "sess-c3", toolSkill(), "success"); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	name := "img-dedup"
	// scripts/ 文件落盘（0644）且内容与模型产出一致。
	scriptPath := filepath.Join(dir, name, "scripts", "dedup.py")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("script file must exist: %v", err)
	}
	if string(data) != "print('dedup')\n" {
		t.Fatalf("script content = %q", data)
	}
	// 目录式 SKILL.md + frontmatter tools 清单。
	skillPath := filepath.Join(dir, name, "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("SKILL.md must exist: %v", err)
	}
	for _, want := range []string{"tools:", "- path: scripts/dedup.py", "run: python scripts/dedup.py", "按哈希去重"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("SKILL.md missing %q:\n%s", want, raw)
		}
	}
	// tools 不进正文步骤。
	_, body := textutil.ParseFrontmatter(raw)
	if strings.Contains(body, "tools:") || !strings.Contains(body, "## 步骤") {
		t.Errorf("tools must stay out of body steps:\n%s", body)
	}
	// PG 元数据：has_tools + 目录式 content_path。
	rec := st.items[name]
	if rec == nil || !rec.HasTools {
		t.Fatalf("HasTools = %+v", rec)
	}
	if rec.ContentPath != skillPath {
		t.Fatalf("ContentPath = %q, want %q", rec.ContentPath, skillPath)
	}
	// evolution_log 记 tools 数与冒烟结果。
	if lastLog(st).kind != "skill_create" || !strings.Contains(lastLog(st).summary, "冒烟通过") {
		t.Fatalf("evolution log = %+v", lastLog(st))
	}
}

// ---- C3：冒烟失败降级正文附录、has_tools=false ----

func TestPersistOneSmokeFailureDegradesToAppendix(t *testing.T) {
	stubSkillRuntime(t, "python", nil, errors.New("exit status 1: SyntaxError"))
	st := newFakeSkillStore()
	dir := t.TempDir()
	s := newTestSink(st, dir)
	if err := s.persistOne(context.Background(), "sess-c3b", toolSkill(), "success"); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	name := "img-dedup"
	// 不建 scripts/ 目录。
	if _, err := os.Stat(filepath.Join(dir, name, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("scripts dir must not exist, stat err=%v", err)
	}
	// 正文末尾附录代码块（仅供参考），frontmatter 无 tools。
	raw, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if strings.Contains(string(raw), "tools:") {
		t.Errorf("frontmatter must not carry tools after degrade:\n%s", raw)
	}
	_, body := textutil.ParseFrontmatter(raw)
	for _, want := range []string{"## 附带脚本（未通过冒烟验证，仅供参考）", "scripts/dedup.py", "```py", "print('dedup')"} {
		if !strings.Contains(body, want) {
			t.Errorf("appendix missing %q:\n%s", want, body)
		}
	}
	if st.items[name] == nil || st.items[name].HasTools {
		t.Fatalf("HasTools must be false, rec=%+v", st.items[name])
	}
	if !strings.Contains(lastLog(st).summary, "冒烟失败") {
		t.Fatalf("evolution log = %+v", lastLog(st))
	}
}

// ---- C3：无可用运行时降级 ----

func TestPersistOneNoRuntimeDegrades(t *testing.T) {
	stubSkillRuntime(t, "", errors.New("python not found"), nil)
	st := newFakeSkillStore()
	dir := t.TempDir()
	s := newTestSink(st, dir)
	if err := s.persistOne(context.Background(), "sess-c3c", toolSkill(), "success"); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	name := "img-dedup"
	if _, err := os.Stat(filepath.Join(dir, name, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("scripts dir must not exist without runtime")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	if !strings.Contains(string(raw), "## 附带脚本（未通过冒烟验证，仅供参考）") {
		t.Fatalf("expected appendix degrade:\n%s", raw)
	}
	if st.items[name] == nil || st.items[name].HasTools {
		t.Fatalf("HasTools must be false without runtime")
	}
}

// ---- C1：老格式回退读 + 新格式 roundtrip ----

// TestParseLearnedSkillFileLegacyFallback 老 <name>.md 回退读（C1 向后兼容）。
func TestParseLearnedSkillFileLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: legacy-skill\ntitle: 老技能\nwhen_to_use: 场景\n---\n\n## 步骤\n1. 照做\n"
	if err := os.WriteFile(filepath.Join(dir, "legacy-skill.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 解析器直接读老路径（reconcile 第二遍路径）。
	sk, err := parseLearnedSkillFile(filepath.Join(dir, "legacy-skill.md"))
	if err != nil {
		t.Fatalf("parse legacy: %v", err)
	}
	if sk.SkillID != "legacy-skill" || !strings.Contains(sk.Content, "照做") {
		t.Fatalf("legacy skill = %+v", sk)
	}
	// ResolveSkillMDPath 回退老格式；写入方应原样用它。
	if got := textutil.ResolveSkillMDPath(dir, "legacy-skill"); got != filepath.Join(dir, "legacy-skill.md") {
		t.Fatalf("resolver = %q", got)
	}
}

// TestParseLearnedSkillFileDirFormatTools 目录式 + tools：C1 roundtrip + C2 注入段。
func TestParseLearnedSkillFileDirFormatTools(t *testing.T) {
	dir := t.TempDir()
	tools := []textutil.SkillTool{{Path: "scripts/dedup.py", Desc: "按哈希去重", Run: "python scripts/dedup.py"}}
	head := textutil.RenderSkillFrontmatter("img-dedup", "图像去重", "批量渲染重复时", "success", tools)
	skillDir := filepath.Join(dir, "img-dedup")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(head+"\n## 步骤\n1. 运行去重脚本\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, err := parseLearnedSkillFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("parse dir-format: %v", err)
	}
	// C2：Content 含「配套工具」段与 run 命令。
	for _, want := range []string{"## 配套工具", "`scripts/dedup.py`", "`python scripts/dedup.py`", "按哈希去重", "运行去重脚本"} {
		if !strings.Contains(sk.Content, want) {
			t.Errorf("Content missing %q:\n%s", want, sk.Content)
		}
	}
	if !strings.Contains(sk.Content, "## 步骤") {
		t.Errorf("body steps lost:\n%s", sk.Content)
	}
}

// ---- C3：prompt / 解析 ----

// TestBuildEvolvePromptToolsRules C3 prompt 规则：仅实际用过、≤60 行、filename 规范。
func TestBuildEvolvePromptToolsRules(t *testing.T) {
	p := buildEvolvePrompt(agent.EvolveInput{Goal: "g", Summary: "s", Outcome: "success", EventsDigest: "e", UserMessages: "u"})
	for _, want := range []string{"tools", "filename", "language", "code", "desc", "≤60 行", "实际自写并执行成功", "scripts/", "py|sh|js|ts"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestParseEvolveOutputTools tools 数组解析进 EvolvedSkill。
func TestParseEvolveOutputTools(t *testing.T) {
	resp := `{"user_prefs": [], "project_lessons": [], "skills": [{"name": "img-dedup", "title": "图像去重", "when_to_use": "重复时", "steps": ["运行脚本"], "pitfalls": [], "verify": "", "tools": [{"filename": "scripts/dedup.py", "language": "py", "code": "print(1)", "desc": "去重"}]}]}`
	out, err := parseEvolveOutput(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Skills) != 1 || len(out.Skills[0].Tools) != 1 {
		t.Fatalf("skills = %+v", out.Skills)
	}
	tool := out.Skills[0].Tools[0]
	if tool.Filename != "scripts/dedup.py" || tool.Language != "py" || tool.Code != "print(1)" || tool.Desc != "去重" {
		t.Fatalf("tool = %+v", tool)
	}
	// 无 tools 字段：空清单不报错。
	out, err = parseEvolveOutput(`{"skills": [{"name": "a-b", "title": "t", "when_to_use": "w"}]}`)
	if err != nil || len(out.Skills) != 1 || len(out.Skills[0].Tools) != 0 {
		t.Fatalf("no-tools parse = %+v, err=%v", out, err)
	}
}

// ---- C3 固化判定单测 ----

func TestSolidifySkillToolsValidation(t *testing.T) {
	stubSkillRuntime(t, "python", nil, nil)
	// 文件名不合法 → 降级（含 reason）。
	solid, failed, _ := solidifySkillTools([]agent.EvolvedSkillTool{{Filename: "../evil.py", Language: "py", Code: "x", Desc: "d"}})
	if len(solid) != 0 || len(failed) != 1 || !strings.Contains(failed[0].reason, "不合法") {
		t.Fatalf("illegal filename: solid=%v failed=%+v", solid, failed)
	}
	// 超长（>60 行）→ 不固化也不进附录。
	long := strings.Repeat("x = 1\n", 61)
	solid, failed, notes := solidifySkillTools([]agent.EvolvedSkillTool{{Filename: "scripts/big.py", Language: "py", Code: long, Desc: "d"}})
	if len(solid) != 0 || len(failed) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "超 60 行") {
		t.Fatalf("overlong: solid=%v failed=%v notes=%v", solid, failed, notes)
	}
	// 语言别名 python → py 归一 + 冒烟通过 → 固化，run 用查到的解释器。
	solid, _, _ = solidifySkillTools([]agent.EvolvedSkillTool{{Filename: "scripts/ok.py", Language: "python", Code: "print(1)", Desc: "d"}})
	if len(solid) != 1 || solid[0].meta.Run != "python scripts/ok.py" || solid[0].meta.Path != "scripts/ok.py" {
		t.Fatalf("solid = %+v", solid)
	}
}
