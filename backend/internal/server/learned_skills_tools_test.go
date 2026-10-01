package server

// learned_skills_tools_test.go 钉死 PUT /api/skills/learned/:name 提交 tools 清单的
// 校验口径（sanitizeSkillTools）：合法 scripts/ 路径放行、路径穿越/危险扩展名拒绝、
// desc/run 限长。对应 C4 前端「配套脚本」移除后保存的契约。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/textutil"
)

func TestSanitizeSkillToolsAcceptsValid(t *testing.T) {
	got, msg := sanitizeSkillTools([]textutil.SkillTool{
		{Path: " scripts/dedup.py ", Desc: " 去重脚本 ", Run: " python scripts/dedup.py "},
		{Path: "scripts/run.sh", Desc: "", Run: ""},
	})
	if msg != "" {
		t.Fatalf("unexpected reject: %s", msg)
	}
	if len(got) != 2 || got[0].Path != "scripts/dedup.py" || got[0].Run != "python scripts/dedup.py" {
		t.Fatalf("sanitize/trim wrong: %+v", got[0])
	}
}

func TestSanitizeSkillToolsRejectsBadPath(t *testing.T) {
	cases := []string{
		"../escape.py",       // 路径穿越
		"scripts/evil.exe",   // 危险扩展名
		"scripts/UPPER.PY",   // 大小写越规
		"scripts/noext",      // 无扩展名
		"scripts/a b.py",     // 空格
		"/abs/path.py",       // 绝对路径
		"other/dir.py",       // 非 scripts/ 前缀
	}
	for _, p := range cases {
		if _, msg := sanitizeSkillTools([]textutil.SkillTool{{Path: p}}); msg == "" {
			t.Errorf("path %q should be rejected", p)
		} else if !strings.Contains(msg, "invalid tool path") {
			t.Errorf("path %q reject msg = %q", p, msg)
		}
	}
}

func TestSanitizeSkillToolsRejectsOverlong(t *testing.T) {
	long := strings.Repeat("x", 201)
	if _, msg := sanitizeSkillTools([]textutil.SkillTool{{Path: "scripts/a.py", Desc: long}}); msg == "" {
		t.Error("overlong desc should be rejected")
	}
	if _, msg := sanitizeSkillTools([]textutil.SkillTool{{Path: "scripts/a.py", Run: long}}); msg == "" {
		t.Error("overlong run should be rejected")
	}
}
