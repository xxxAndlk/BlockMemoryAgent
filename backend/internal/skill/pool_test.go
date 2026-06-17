package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

type fakeLLM struct {
	resp string
	err  error
}

func (f *fakeLLM) Generate(_ context.Context, _ string) (string, error) {
	return f.resp, f.err
}

func TestPool_FilterByDomain(t *testing.T) {
	p := BuiltinPool()
	got := p.FilterByDomain("code")
	if len(got) == 0 {
		t.Fatalf("expected at least one code-domain skill, got 0")
	}
	for _, s := range got {
		if !strings.Contains(s.Domain, "code") && !strings.Contains(s.Domain, "*") {
			t.Errorf("skill %s leaked into code domain (domain=%s)", s.SkillID, s.Domain)
		}
	}
}

func TestPool_AssembleSet_NoLLMReturnsAllOrTrim(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "a", Description: "desc a", Domain: "x"},
		{SkillID: "b", Description: "desc b", Domain: "x"},
		{SkillID: "c", Description: "desc c", Domain: "x"},
		{SkillID: "d", Description: "desc d", Domain: "x"},
	})
	set := p.AssembleSet(context.Background(), nil, "agentX", "x", "goal", 3)
	if len(set.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(set.Skills))
	}
}

func TestPool_AssembleSet_WithLLM(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "alpha", Description: "alpha", Domain: "x"},
		{SkillID: "beta", Description: "beta", Domain: "x"},
		{SkillID: "gamma", Description: "gamma", Domain: "x"},
		{SkillID: "delta", Description: "delta", Domain: "x"},
	})
	llm := &fakeLLM{resp: `["alpha","gamma"]`}
	set := p.AssembleSet(context.Background(), llm, "agentX", "x", "goal", 2)
	if len(set.Skills) != 2 {
		t.Fatalf("expected 2 picks, got %d", len(set.Skills))
	}
	ids := []string{set.Skills[0].SkillID, set.Skills[1].SkillID}
	if !contains(ids, "alpha") || !contains(ids, "gamma") {
		t.Fatalf("LLM picks not honored: %v", ids)
	}
}

func TestSelectOne(t *testing.T) {
	set := &types.SkillSet{
		Skills: []*types.Skill{
			{SkillID: "read_file", Description: "读取文件"},
			{SkillID: "run_command", Description: "执行命令"},
		},
	}
	llm := &fakeLLM{resp: "run_command"}
	if got := SelectOne(context.Background(), llm, set, "执行 ls"); got != "run_command" {
		t.Errorf("expected run_command, got %q", got)
	}
	llm.resp = "NONE"
	if got := SelectOne(context.Background(), llm, set, "做不了的事情"); got != "" {
		t.Errorf("expected empty when NONE, got %q", got)
	}
}

func TestRegistry_BindAndAdd(t *testing.T) {
	pool := BuiltinPool()
	reg := NewRegistry(pool)
	reg.Bind(&types.SkillSet{
		OwnerAgent: "a1",
		Skills:     []*types.Skill{pool.Get("read_file")},
	})
	if got := reg.GetForAgent("a1"); got == nil || len(got.Skills) != 1 {
		t.Fatalf("bind failed: %#v", got)
	}
	if !reg.AddSkillToAgent("a1", "write_file") {
		t.Fatalf("expected AddSkillToAgent to succeed")
	}
	if got := reg.GetForAgent("a1"); len(got.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(got.Skills))
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
