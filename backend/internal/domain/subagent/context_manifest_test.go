package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/team"
)

func src(source, id, scope, trust, key, content string, required bool) contextSource {
	return contextSource{
		Source: source, SourceID: id, Scope: scope, Trust: trust,
		ConflictKey: key, Content: content, Required: required,
	}
}

// TestArbitrateDedupePrefersAuthority 同 hash 去重保留高优先级副本。
func TestArbitrateDedupePrefersAuthority(t *testing.T) {
	sources := []contextSource{
		src(team.SourceBlockRecall, "b1", team.ScopeProject, team.TrustUntrusted, "k", "same", false),
		src(team.SourceProjectMemory, "M-1", team.ScopeProject, team.TrustVerified, "k", "same", false),
	}
	var probs manifestProblems
	entries, decisions := arbitrateContextSources(sources, &probs)
	if len(entries) != 1 || entries[0].Source != team.SourceProjectMemory {
		t.Fatalf("dedupe must keep verified memory: %+v", entries)
	}
	if len(decisions) != 1 || decisions[0].Action != team.DecisionDuplicate ||
		decisions[0].WinnerID != entryIDFor(sources[1]) {
		t.Fatalf("unexpected dedupe decision: %+v", decisions)
	}
}

// TestArbitrateHumanOverridesAutoVerified 人工/已验证项目经验覆盖未验证同 key 经验。
func TestArbitrateHumanOverridesAutoVerified(t *testing.T) {
	sources := []contextSource{
		src(team.SourceProjectMemory, "M-auto", team.ScopeProject, team.TrustUntrusted, "build", "用 webpack", false),
		src(team.SourceProjectMemory, "M-human", team.ScopeProject, team.TrustVerified, "build", "用 vite", false),
	}
	var probs manifestProblems
	entries, decisions := arbitrateContextSources(sources, &probs)
	if len(entries) != 1 || entries[0].SourceID != "M-human" {
		t.Fatalf("human memory must win: %+v", entries)
	}
	if len(decisions) != 1 || decisions[0].Action != team.DecisionSuperseded || decisions[0].LoserIDs[0] != entryIDFor(sources[0]) {
		t.Fatalf("unexpected supersede decision: %+v", decisions)
	}
}

// TestArbitrateUnresolvedSameRank 同 rank/trust 不同内容 → unresolved，required 记问题。
func TestArbitrateUnresolvedSameRank(t *testing.T) {
	sources := []contextSource{
		src(team.SourceTaskSpec, "spec-a", team.ScopeTask, team.TrustAuthoritative, "task", "目标 A", true),
		src(team.SourceTaskSpec, "spec-b", team.ScopeTask, team.TrustAuthoritative, "task", "目标 B", true),
	}
	var probs manifestProblems
	entries, decisions := arbitrateContextSources(sources, &probs)
	if len(entries) != 0 {
		t.Fatalf("unresolved group must not inject: %+v", entries)
	}
	if !probs.UnresolvedReq || len(probs.RequiredDropped) != 2 {
		t.Fatalf("required unresolved must be flagged: %+v", probs)
	}
	if len(decisions) != 1 || decisions[0].Action != team.DecisionUnresolved {
		t.Fatalf("unexpected unresolved decision: %+v", decisions)
	}
}

// TestArbitrateScopeIsolation 跨作用域同 key 不冲突。
func TestArbitrateScopeIsolation(t *testing.T) {
	sources := []contextSource{
		src(team.SourceProjectMemory, "M-p", team.ScopeProject, team.TrustVerified, "style", "项目风格", false),
		src(team.SourceUserMemory, "M-u", team.ScopeUser, team.TrustVerified, "style", "用户偏好", false),
	}
	var probs manifestProblems
	entries, decisions := arbitrateContextSources(sources, &probs)
	if len(entries) != 2 || len(decisions) != 0 {
		t.Fatalf("cross-scope entries must coexist: entries=%+v decisions=%+v", entries, decisions)
	}
}

// TestManifestRequiredDroppedEnforce 必传条目被覆盖/丢弃时 enforce 拒派。
func TestManifestRequiredDroppedEnforce(t *testing.T) {
	sources := []contextSource{
		src(team.SourceUserInstruction, "task", team.ScopeTask, team.TrustAuthoritative, "task", "当前任务", true),
		src(team.SourceTaskSpec, "spec", team.ScopeTask, team.TrustVerified, "task", "旧规范", true),
	}
	d := NewDispatcher(nil, nil, nil, nil, nil)
	m, probs := d.buildTaskContextManifest("s1", "code_assistant", sources, false)
	if m == nil || len(m.Required) != 1 || m.Required[0].Source != team.SourceUserInstruction {
		t.Fatalf("winner required entry missing: %+v", m)
	}
	if len(probs.RequiredDropped) != 1 {
		t.Fatalf("required dropped not reported: %+v", probs)
	}
	if err := verifyManifestEnforce(probs); err == nil {
		t.Fatal("enforce must refuse when required entry is superseded")
	}
	// shadow 不拒派由调用方保证；这里只验证 enforce 判定。
	probs = manifestProblems{}
	if err := verifyManifestEnforce(probs); err != nil {
		t.Fatalf("clean problems should pass: %v", err)
	}
}

func TestManifestIsolatedRequired(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	sources := []contextSource{
		src(team.SourceUserInstruction, "task", team.ScopeTask, team.TrustAuthoritative, "task", "任务", true),
		src(team.SourceAgentsMD, "AGENTS.md", team.ScopeProject, team.TrustAuthoritative, "project_convention", "约定", true),
	}
	_, probs := d.buildTaskContextManifest("s1", "code_assistant", sources, true)
	if !probs.IsolatedRequired || len(probs.RequiredDropped) != 2 {
		t.Fatalf("isolated required not flagged: %+v", probs)
	}
	if err := verifyManifestEnforce(probs); err == nil || !strings.Contains(err.Error(), "fork hard cap") {
		t.Fatalf("enforce must refuse isolated required: %v", err)
	}
}

// TestContextMemorySourcesTrustMapping 记忆条目按 verification 映射 trust 与来源。
func TestContextMemorySourcesTrustMapping(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.WithMemoryContextItems(func(context.Context) []MemoryContextItem {
		return []MemoryContextItem{
			{ID: "M-1", Owner: "u", Scope: "project", ProjectKey: "pk", MemoryKey: "build", Content: "vite", Verification: "verified", Revision: 2},
			{ID: "M-2", Owner: "u", Scope: "project", ProjectKey: "pk", MemoryKey: "old", Content: "webpack", Verification: "unverified", Revision: 1},
		}
	})
	sources := d.contextMemorySources(context.Background())
	if len(sources) != 2 {
		t.Fatalf("want 2 memory sources, got %d", len(sources))
	}
	if sources[0].Trust != team.TrustVerified || sources[0].ProjectKey != "pk" || sources[0].Owner != "u" {
		t.Fatalf("verified mapping mismatch: %+v", sources[0])
	}
	if sources[1].Trust != team.TrustUntrusted {
		t.Fatalf("unverified memory must be untrusted: %+v", sources[1])
	}
}

// TestTrimOptionalBudget 超预算按来源优先级保留，被裁条目留 dropped_budget 裁决。
func TestTrimOptionalBudget(t *testing.T) {
	entries := []team.ContextEntry{
		{EntryID: "e-low", Source: team.SourceBlockRecall, Trust: team.TrustUntrusted, RuneBudget: 10},
		{EntryID: "e-high", Source: team.SourceProjectMemory, Trust: team.TrustVerified, RuneBudget: 10},
	}
	kept, decisions := trimOptionalBudget(entries, 10, nil)
	if len(kept) != 1 || kept[0].EntryID != "e-high" {
		t.Fatalf("higher priority entry must survive: %+v", kept)
	}
	if len(decisions) != 1 || decisions[0].Action != team.DecisionDropped || decisions[0].LoserIDs[0] != "e-low" {
		t.Fatalf("unexpected budget decision: %+v", decisions)
	}
}

func TestContextManifestModeNormalize(t *testing.T) {
	cases := map[string]string{
		"":         ContextManifestShadow,
		"off":      ContextManifestOff,
		" shadow ": ContextManifestShadow,
		"ENFORCE":  ContextManifestEnforce,
		"bogus":    ContextManifestShadow,
	}
	for in, want := range cases {
		if got := normalizeContextManifestMode(in); got != want {
			t.Fatalf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}
