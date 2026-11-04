package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
)

func validTeamCandidate(sc MemoryScopeContext) TeamCandidate {
	return TeamCandidate{
		Owner: "", ProjectKey: sc.ProjectKey, MemoryKey: "team.build", Content: "项目构建优先使用 vite",
		RoleID: "code_assistant", TaskType: "build", SourceSession: "sess-1", SourceTask: "set up build",
		SourceResultID: "res-1", SourceVerificationReportID: "vr-1",
		SourceStatus: "completed", VerificationStatus: "passed",
	}
}

// TestSubmitTeamCandidateRequiresEvidence 来源未 done/未通过机器验收不得成为候选。
func TestSubmitTeamCandidateRequiresEvidence(t *testing.T) {
	lc, _, sc := newLifecycleFixture(t, RetractionModeEnforce)
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeShadow})
	c := validTeamCandidate(sc)
	c.SourceStatus = "failed"
	if _, err := svc.SubmitTeamCandidate(context.Background(), c); err == nil {
		t.Fatal("failed source must be rejected")
	}
	c = validTeamCandidate(sc)
	c.VerificationStatus = "unverified"
	if _, err := svc.SubmitTeamCandidate(context.Background(), c); err == nil {
		t.Fatal("unverified source must be rejected")
	}
	c = validTeamCandidate(sc)
	c.ProjectKey = ""
	if _, err := svc.SubmitTeamCandidate(context.Background(), c); err == nil || !strings.Contains(err.Error(), "project_key") {
		t.Fatalf("project scope required: %v", err)
	}
}

// TestSubmitTeamCandidateResurrectionFailClosed 已撤回内容不得换措辞复活。
func TestSubmitTeamCandidateResurrectionFailClosed(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	old := addActive(t, repo, store.MemoryItem{Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey, MemoryKey: "team.build", Content: "项目构建优先使用 vite"})
	if _, err := lc.RetractByID(context.Background(), "", old.ID, "rejected"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeShadow})
	if _, err := svc.SubmitTeamCandidate(context.Background(), validTeamCandidate(sc)); err == nil {
		t.Fatal("resurrection candidate must be rejected")
	}
}

// TestPromoteShadowDoesNotActivate 默认 shadow 只记录晋升意图，不激活。
func TestPromoteShadowDoesNotActivate(t *testing.T) {
	lc, _, sc := newLifecycleFixture(t, RetractionModeEnforce)
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeShadow})
	item, err := svc.SubmitTeamCandidate(context.Background(), validTeamCandidate(sc))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if item.Status != store.MemoryStatusCandidate {
		t.Fatalf("candidate status = %s", item.Status)
	}
	if _, err := svc.VerifyCandidate(context.Background(), "", item.ID, 0, "admin", "eval passed"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := svc.PromoteCandidate(context.Background(), "", item.ID, 0, "admin", "ok"); err == nil {
		t.Fatal("shadow promotion must not activate")
	}
	got, _ := lc.repo.GetItem(context.Background(), "", item.ID)
	if got.Status != store.MemoryStatusCandidate {
		t.Fatalf("shadow changed status: %s", got.Status)
	}
}

// TestPromoteEnforceAtomicReplacement enforce + verified 才激活，并原子替换同 key active。
func TestPromoteEnforceAtomicReplacement(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	old := addActive(t, repo, store.MemoryItem{Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey, MemoryKey: "team.build", Content: "旧构建方式"})
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeEnforce, AutoActivate: false})
	item, err := svc.SubmitTeamCandidate(context.Background(), validTeamCandidate(sc))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 未验证不得晋升。
	if _, err := svc.PromoteCandidate(context.Background(), "", item.ID, 0, "admin", "ok"); err == nil {
		t.Fatal("unverified candidate must not promote")
	}
	if _, err := svc.VerifyCandidate(context.Background(), "", item.ID, 0, "admin", "eval passed"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// actor=auto 且 auto_activate=false 拒绝。
	if _, err := svc.PromoteCandidate(context.Background(), "", item.ID, 0, "auto", "ok"); err == nil {
		t.Fatal("auto promotion must be rejected when auto_activate=false")
	}
	promoted, err := svc.PromoteCandidate(context.Background(), "", item.ID, 0, "admin", "manual promote")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted.Status != store.MemoryStatusActive || promoted.Verification != store.MemoryVerificationVerified {
		t.Fatalf("promoted item mismatch: %+v", promoted)
	}
	gotOld, _ := repo.GetItem(context.Background(), "", old.ID)
	if gotOld.Status != store.MemoryStatusSuperseded {
		t.Fatalf("old active must be superseded, got %s", gotOld.Status)
	}
	// 同 key 不得出现两个 active。
	items, _ := repo.ListItems(context.Background(), store.MemoryQuery{Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey, Statuses: []string{store.MemoryStatusActive}})
	active := 0
	for _, it := range items {
		if it.MemoryKey == "team.build" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active count = %d, want 1", active)
	}
}

// TestSubmitTeamCandidateRedactsSecrets 候选内容中的凭据形态被脱敏。
func TestSubmitTeamCandidateRedactsSecrets(t *testing.T) {
	lc, _, sc := newLifecycleFixture(t, RetractionModeEnforce)
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeShadow})
	c := validTeamCandidate(sc)
	c.Content = "使用 api_key=sk-abcdefghijklmnop 构建"
	item, err := svc.SubmitTeamCandidate(context.Background(), c)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if strings.Contains(item.Content, "sk-abcdefghijklmnop") || !strings.Contains(item.Content, "[redacted]") {
		t.Fatalf("secret not redacted: %q", item.Content)
	}
}

// TestPromotionModeOff 关闭时拒绝候选。
func TestPromotionModeOff(t *testing.T) {
	lc, _, sc := newLifecycleFixture(t, RetractionModeEnforce)
	svc := NewMemoryPromotionService(lc, PromotionConfig{Mode: PromotionModeOff})
	if _, err := svc.SubmitTeamCandidate(context.Background(), validTeamCandidate(sc)); err == nil {
		t.Fatal("promotion_mode=off must reject candidates")
	}
}
