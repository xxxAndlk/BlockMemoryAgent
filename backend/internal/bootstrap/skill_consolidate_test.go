package bootstrap

// skill_consolidate_test.go 覆盖整理方案的解析与文件渲染（PG 应用路径由人工/集成验证）。

import (
	"strings"
	"testing"
)

// TestParseConsolidatePlan 围栏剥离 + 字段映射。
func TestParseConsolidatePlan(t *testing.T) {
	resp := "噪声前缀\n```json\n{\"merges\":[{\"keep\":\"a-skill\",\"merge\":[\"b-skill\"],\"note\":\"同类\"}]," +
		"\"archives\":[{\"name\":\"c-skill\",\"reason\":\"零使用琐碎\"}]}\n```"
	plan, err := parseConsolidatePlan(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plan.Merges) != 1 || plan.Merges[0].Keep != "a-skill" || len(plan.Merges[0].Merge) != 1 || plan.Merges[0].Merge[0] != "b-skill" {
		t.Fatalf("merges = %+v", plan.Merges)
	}
	if len(plan.Archives) != 1 || plan.Archives[0].Name != "c-skill" {
		t.Fatalf("archives = %+v", plan.Archives)
	}
}

// TestParseConsolidatePlanEmpty 无动作方案（两个空数组）解析为空计划而非报错。
func TestParseConsolidatePlanEmpty(t *testing.T) {
	plan, err := parseConsolidatePlan(`{"merges":[],"archives":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plan.Merges) != 0 || len(plan.Archives) != 0 {
		t.Fatalf("expect empty plan, got %+v", plan)
	}
}

// TestParseConsolidatePlanNoJSON 无 JSON 对象返回错误。
func TestParseConsolidatePlanNoJSON(t *testing.T) {
	if _, err := parseConsolidatePlan("纯文本无对象"); err == nil {
		t.Fatal("expected error for response without JSON")
	}
}

// TestRenderSkillFile frontmatter（含 outcome）+ 正文渲染。
func TestRenderSkillFile(t *testing.T) {
	got := renderSkillFile("my-skill", "我的技能", "需要时用", "mixed", "## 步骤\n1. 第一步")
	for _, want := range []string{"name: my-skill", "title: 我的技能", "when_to_use: 需要时用", "outcome: mixed", "## 步骤", "1. 第一步"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderSkillFile missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "---\n") || !strings.Contains(got, "\n---\n\n") {
		t.Fatalf("frontmatter block malformed:\n%s", got)
	}
	// 空 outcome 不写该行。
	if got := renderSkillFile("a", "t", "w", "", "body"); strings.Contains(got, "outcome:") {
		t.Fatalf("empty outcome should be omitted:\n%s", got)
	}
}

// TestTruncateHeadTail 超长保留头+尾（结论段在尾部时不丢）。
func TestTruncateHeadTail(t *testing.T) {
	head := strings.Repeat("过", 4000)
	tail := strings.Repeat("结", 2000)
	got := truncateHeadTail(head+"中段填充"+tail, 4000, 2000)
	if !strings.Contains(got, "(中间省略)") {
		t.Fatalf("expected ellipsis marker")
	}
	if !strings.HasPrefix(got, strings.Repeat("过", 100)) || !strings.HasSuffix(got, strings.Repeat("结", 100)) {
		t.Fatalf("head/tail not preserved")
	}
	// 未超限原样返回。
	if got := truncateHeadTail("短文本", 4000, 2000); got != "短文本" {
		t.Fatalf("short input should pass through, got %q", got)
	}
}

// TestFilterJunkFacts 失败通知原文被丢弃、正常事实保留。
func TestFilterJunkFacts(t *testing.T) {
	got := filterJunkFacts([]string{
		"[failure kind=killed retryable=false] 子 Agent session-1 被停止",
		"  ",
		"sprite 资产脚本需先生成 palette.json 再跑 extract_assets.py",
	})
	if len(got) != 1 || !strings.Contains(got[0], "palette.json") {
		t.Fatalf("filterJunkFacts = %v", got)
	}
}
