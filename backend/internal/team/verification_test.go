package team

import (
	"strings"
	"testing"
)

func TestVerificationPolicyDefaults(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Verification == nil || s.Verification.Risk != RiskR2 {
		t.Fatalf("default risk = %+v, want R2", s.Verification)
	}
	if len(s.Verification.RequiredLevels) != 3 {
		t.Fatalf("R2 levels = %v", s.Verification.RequiredLevels)
	}
	if s.Verification.MaxRepairRounds != 1 {
		t.Fatalf("default max_repair_rounds = %d", s.Verification.MaxRepairRounds)
	}
}

func TestProductionApprovalForcesR3(t *testing.T) {
	yaml := strings.Replace(validSpec, "policies:\n", "policies:\n  approval: production\n", 1)
	s, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Verification.Risk != RiskR3 || !contains(s.Verification.RequiredLevels, "runtime") {
		t.Fatalf("production must force R3 with runtime level: %+v", s.Verification)
	}
}

func TestVerificationPolicyRejectsInvalid(t *testing.T) {
	bad := strings.Replace(validSpec, "policies:\n", "verification:\n  risk: R9\npolicies:\n", 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("invalid risk must be rejected")
	}
	badLevel := strings.Replace(validSpec, "policies:\n", "verification:\n  risk: R2\n  required_levels: [bogus]\npolicies:\n", 1)
	if _, err := Parse([]byte(badLevel)); err == nil {
		t.Fatal("invalid level must be rejected")
	}
}

func TestNestedVerificationCannotDowngrade(t *testing.T) {
	parent, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	child := &Spec{
		ID: "child", Version: "v1",
		Roles:    []RoleSpec{{ID: "writer", RoleID: "code_assistant", Tools: []string{"ReadFile"}}},
		Policies: Policies{MaxDepth: 1, MaxNodes: 4}, DefaultBudget: 50000,
		Verification: &VerificationPolicy{Risk: RiskR1},
	}
	if err := ValidateNested(parent, child); err == nil || !strings.Contains(err.Error(), "lower than parent") {
		t.Fatalf("risk downgrade should fail: %v", err)
	}

	child.Verification = &VerificationPolicy{Risk: RiskR3}
	child.Roles[0].Verification = &VerificationPolicy{Risk: RiskR3, RequiredLevels: []string{"existence"}}
	if err := ValidateNested(parent, child); err == nil || !strings.Contains(err.Error(), "missing from child") {
		t.Fatalf("dropping required level should fail: %v", err)
	}

	child.Roles[0].Verification = nil
	if err := ValidateNested(parent, child); err != nil {
		t.Fatalf("equal-or-stricter child should pass: %v", err)
	}
}

func TestNestedVerificationCannotRelaxManual(t *testing.T) {
	parent, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	parent.Verification.AllowManual = false
	parent.Verification.RequireHumanSignoff = true
	child := &Spec{
		ID: "child", Version: "v1",
		Roles:        []RoleSpec{{ID: "writer", RoleID: "code_assistant", Tools: []string{"ReadFile"}}},
		Policies:     Policies{MaxDepth: 1, MaxNodes: 4},
		Verification: &VerificationPolicy{Risk: RiskR2, AllowManual: true, RequireHumanSignoff: false},
	}
	if err := ValidateNested(parent, child); err == nil {
		t.Fatal("child must not relax manual/signoff requirements")
	}
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
