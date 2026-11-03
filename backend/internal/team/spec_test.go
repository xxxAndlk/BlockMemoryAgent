package team

import (
	"strings"
	"testing"
)

const validSpec = `
id: demo-team
version: v1
roles:
  - id: writer
    role_id: code_assistant
    tools: [ReadFile, WriteFile]
  - id: reviewer
    role_id: code_reviewer
    tools: [ReadFile]
policies:
  max_depth: 2
  max_nodes: 8
default_budget: 100000
dod: [tests-pass]
memory_scopes: [project]
`

func TestParseSpec(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.ID != "demo-team" || s.Version != "v1" || len(s.Roles) != 2 {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if s.Policies.MaxDepth != 2 || s.Policies.MaxNodes != 8 || s.Policies.Approval != "development" {
		t.Fatalf("policies not normalized: %+v", s.Policies)
	}
	if err := s.ValidateRoleIDs(map[string]bool{"code_assistant": true, "code_reviewer": true}); err != nil {
		t.Fatalf("role ids should validate: %v", err)
	}
	if err := s.ValidateRoleIDs(map[string]bool{"code_assistant": true}); err == nil {
		t.Fatal("missing role should fail validation")
	}
}

func TestParseSpecRejectsInvalid(t *testing.T) {
	cases := []string{
		`version: v1
roles: [{id: a, role_id: r}]`, // 缺 id
		`id: x
roles: [{id: a, role_id: r}]`, // 缺 version
		`id: x
version: v1`, // 缺 roles
		`id: x
version: v1
roles: [{id: a, role_id: r}, {id: a, role_id: r2}]`, // 重复 id
		`id: x
version: v1
roles: [{id: a, role_id: r}]
policies: {max_depth: 9}`, // 超硬上限
		`id: x
version: v1
roles: [{id: a, role_id: r}]
policies: {approval: bogus}`,
	}
	for i, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestValidateNested(t *testing.T) {
	parent, _ := Parse([]byte(validSpec))
	child := &Spec{
		ID:      "child-team",
		Version: "v1",
		Roles: []RoleSpec{
			{ID: "writer", RoleID: "code_assistant", Tools: []string{"ReadFile"}},
		},
		Policies:      Policies{MaxDepth: 1, MaxNodes: 4},
		DefaultBudget: 50000,
	}
	if err := ValidateNested(parent, child); err != nil {
		t.Fatalf("valid child rejected: %v", err)
	}

	// 未在父级声明的角色。
	badRole := *child
	badRole.Roles = []RoleSpec{{ID: "intruder", RoleID: "x"}}
	if err := ValidateNested(parent, &badRole); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared role should fail: %v", err)
	}
	// 工具越界。
	badTools := *child
	badTools.Roles = []RoleSpec{{ID: "writer", RoleID: "code_assistant", Tools: []string{"RunCommand"}}}
	if err := ValidateNested(parent, &badTools); err == nil || !strings.Contains(err.Error(), "tools exceed") {
		t.Fatalf("tool escalation should fail: %v", err)
	}
	// 预算越界。
	badBudget := *child
	badBudget.DefaultBudget = 999999
	if err := ValidateNested(parent, &badBudget); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("budget escalation should fail: %v", err)
	}
	// 深度越界。
	badDepth := *child
	badDepth.Policies.MaxDepth = 2
	if err := ValidateNested(parent, &badDepth); err == nil || !strings.Contains(err.Error(), "max_depth") {
		t.Fatalf("depth escalation should fail: %v", err)
	}
}
