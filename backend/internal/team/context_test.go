package team

import (
	"testing"
	"time"
)

func TestSourceRankOrder(t *testing.T) {
	order := []string{
		SourceUserInstruction, SourceTaskSpec, SourceAgentsMD,
		SourceProjectMemory, SourceUserMemory, SourceSkill, SourceSharedFact,
	}
	last := 0
	for _, s := range order {
		r := SourceRank(s)
		if r < last {
			t.Fatalf("source %s rank %d should be >= %d", s, r, last)
		}
		last = r
	}
	if SourceRank("unknown") != 7 {
		t.Fatalf("unknown source must rank 7 (untrusted)")
	}
	if TrustRank(TrustAuthoritative) >= TrustRank(TrustVerified) || TrustRank(TrustVerified) >= TrustRank(TrustUntrusted) {
		t.Fatal("trust ordering must be authoritative < verified < untrusted")
	}
}

func TestManifestContentHashStable(t *testing.T) {
	mk := func(entries []ContextEntry) *TaskContextManifest {
		return &TaskContextManifest{
			Version: ContextManifestVersion, RoleID: "code_assistant", ProjectKey: "p",
			ExecutionPath: ExecutionPathLegacyDelegate, Required: entries,
		}
	}
	a := ContextEntry{EntryID: "e-a", Source: SourceTaskSpec, SourceID: "spec", Scope: ScopeTask, Trust: TrustAuthoritative, ContentHash: "h1", Required: true}
	b := ContextEntry{EntryID: "e-b", Source: SourceUserInstruction, SourceID: "task", Scope: ScopeTask, Trust: TrustAuthoritative, ContentHash: "h2", Required: true}

	m1 := mk([]ContextEntry{a, b})
	m2 := mk([]ContextEntry{b, a}) // 输入顺序不同，规范化后 hash 相同
	if m1.ComputeContentHash() == "" || m1.ComputeContentHash() != m2.ComputeContentHash() {
		t.Fatalf("content hash not order-independent: %s vs %s", m1.ComputeContentHash(), m2.ComputeContentHash())
	}
	b.ContentHash = "h3"
	if m1.ComputeContentHash() == mk([]ContextEntry{a, b}).ComputeContentHash() {
		t.Fatal("content change must alter hash")
	}
}

func TestManifestIDStablePrefix(t *testing.T) {
	at := time.Unix(1700000000, 0)
	id := NewManifestID("0123456789abcdefdeadbeef", at)
	if len(id) < 20 || id[:4] != "ctx-" {
		t.Fatalf("unexpected manifest id: %s", id)
	}
}
