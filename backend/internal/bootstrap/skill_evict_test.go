package bootstrap

// skill_evict_test.go 覆盖确定性冷淘汰（2026-10-17）三条路径：
// a) evolver 满库腾位：库满时有冷技能，persistOne 先归档最冷的再新建；
// b) 全热库：库满但无冷技能可淘，persistOne 走 skill_dropped；
// c) consolidate 每日兜底：LLM 方案空（无合并无归档）但池满且有冷技能，Run 后冷技能被归档。
// 存储用 fakeSkillStore（skill_gate_test.go），LLM 经 callLLM 注入 fake（空方案）。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
)

// seedHotSkill 造一个"热"技能：高使用数且最近刚用（冷淘汰两档都不沾）。
func seedHotSkill(st *fakeSkillStore, name string, useCount int) {
	now := time.Now()
	st.items[name] = &store.LearnedSkill{
		Name: name, Title: name, WhenToUse: "高频场景",
		Enabled: true, UseCount: useCount, LastUsedAt: &now,
		CreatedAt: now, Embedding: []float32{0, 1, 0},
	}
}

// seedColdSkill 造一个"冷"技能：use_count=0 且创建于 days 天前（第一档冷淘汰候选）。
func seedColdSkill(st *fakeSkillStore, name string, daysAgo int) {
	created := time.Now().AddDate(0, 0, -daysAgo)
	st.items[name] = &store.LearnedSkill{
		Name: name, Title: name, WhenToUse: "低频场景",
		Enabled: true, UseCount: 0, CreatedAt: created,
		Embedding: []float32{0, 1, 0},
	}
}

// newEvictTestSink 组一个带冷淘汰参数的 sink（score=nil 跳过 B2 门禁，专注满库分支）。
func newEvictTestSink(st *fakeSkillStore, dir string, maxCount int) *learnedSkillSink {
	return &learnedSkillSink{skills: st, dir: dir, maxCount: maxCount,
		evictColdDays: 30, evictIdleDays: 90, evictIdleMaxUse: 1}
}

func hasLog(st *fakeSkillStore, kind string) bool {
	for _, l := range st.logs {
		if l.kind == kind {
			return true
		}
	}
	return false
}

// TestPersistEvictsColdWhenFull 满库腾位：50 个 enabled（5 个 use_count=0 且创建 40+ 天前），
// persistOne 一个新技能 → 最冷的被归档（skill_evict）、新技能照常建出、总量回到 50。
func TestPersistEvictsColdWhenFull(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	for i := 0; i < 45; i++ {
		seedHotSkill(st, fmt.Sprintf("hot-skill-%02d", i), 5)
	}
	// 冷技能按创建时间 40..44 天前递增，最冷 = cold-skill-44（排序取最前者）。
	for i := 40; i < 45; i++ {
		seedColdSkill(st, fmt.Sprintf("cold-skill-%d", i), i)
	}
	s := newEvictTestSink(st, dir, 50)
	sk := agent.EvolvedSkill{Name: "brand-new-skill", Title: "全新技能",
		WhenToUse: "与既有技能完全不同的新场景", Steps: []string{"运行 brand-new 命令"}}
	if err := s.persistOne(context.Background(), "sess-evict", sk, "success"); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	// 新技能建出。
	if rec := st.items["brand-new-skill"]; rec == nil || !rec.Enabled {
		t.Fatalf("new skill must be created after cold eviction, items=%v", st.items)
	}
	// 最冷的被归档（软删：记录保留、enabled=false）。
	if rec := st.items["cold-skill-44"]; rec == nil || rec.Enabled {
		t.Fatalf("coldest skill must be archived (enabled=false), got %+v", rec)
	}
	// 其余冷技能未动。
	if rec := st.items["cold-skill-40"]; rec == nil || !rec.Enabled {
		t.Fatalf("warmer cold skill must stay enabled, got %+v", rec)
	}
	// enabled 总数：50 - 1 + 1 = 50。
	n, _ := st.CountEnabled(context.Background())
	if n != 50 {
		t.Fatalf("enabled count = %d, want 50", n)
	}
	// 审计：skill_evict 指向最冷者，随后 skill_create 指向新技能。
	if !hasLog(st, "skill_evict") {
		t.Fatalf("expect skill_evict log, got %+v", st.logs)
	}
	for _, l := range st.logs {
		if l.kind == "skill_evict" && l.target != "cold-skill-44" {
			t.Fatalf("skill_evict target = %s, want cold-skill-44", l.target)
		}
	}
	if lastLog(st).kind != "skill_create" || lastLog(st).target != "brand-new-skill" {
		t.Fatalf("expect last log skill_create/brand-new-skill, got %+v", lastLog(st))
	}
}

// TestPersistDropsWhenAllHot 全热库：50 个 enabled 全部近期高使用，无冷技能可淘 →
// persistOne 走 skill_dropped，新技能不建出，且不产生 skill_evict。
func TestPersistDropsWhenAllHot(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		seedHotSkill(st, fmt.Sprintf("hot-skill-%02d", i), 5)
	}
	s := newEvictTestSink(st, dir, 50)
	sk := agent.EvolvedSkill{Name: "brand-new-skill", Title: "全新技能",
		WhenToUse: "与既有技能完全不同的新场景", Steps: []string{"运行 brand-new 命令"}}
	if err := s.persistOne(context.Background(), "sess-hot", sk, "success"); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	if st.items["brand-new-skill"] != nil {
		t.Fatal("new skill must be dropped when library full and all hot")
	}
	if hasLog(st, "skill_evict") {
		t.Fatalf("hot library must not evict, logs=%+v", st.logs)
	}
	if lastLog(st).kind != "skill_dropped" {
		t.Fatalf("expect skill_dropped log, got %+v", lastLog(st))
	}
	if !strings.Contains(lastLog(st).summary, "已尝试冷淘汰腾位") {
		t.Fatalf("dropped summary must mention cold eviction attempt, got %q", lastLog(st).summary)
	}
}

// TestConsolidateColdEvictionFallback consolidate 兜底：LLM 方案空（无合并无归档）但池满
//（50/50）且有冷技能 → Run 后最冷的被归档，summary 报"冷淘汰 1 个"。
func TestConsolidateColdEvictionFallback(t *testing.T) {
	st := newFakeSkillStore()
	for i := 0; i < 45; i++ {
		seedHotSkill(st, fmt.Sprintf("hot-skill-%02d", i), 5)
	}
	for i := 40; i < 45; i++ {
		seedColdSkill(st, fmt.Sprintf("cold-skill-%d", i), i)
	}
	c := &skillConsolidator{
		skills:          st,
		pool:            skill.NewPool(),
		threshold:       30,
		floor:           50,
		evictColdDays:   30,
		evictIdleDays:   90,
		evictIdleMaxUse: 1,
		// 空方案：LLM 不合并不归档，逼出确定性冷淘汰兜底。
		callLLM: func(context.Context, string) (string, error) {
			return `{"merges":[],"archives":[]}`, nil
		},
	}
	summary, err := c.Run(context.Background(), true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec := st.items["cold-skill-44"]; rec == nil || rec.Enabled {
		t.Fatalf("coldest skill must be archived by fallback, got %+v", rec)
	}
	n, _ := st.CountEnabled(context.Background())
	if n != 49 {
		t.Fatalf("enabled count = %d, want 49", n)
	}
	if !strings.Contains(summary, "冷淘汰 1 个") {
		t.Fatalf("summary must report cold eviction, got %q", summary)
	}
	if !hasLog(st, "skill_evict") {
		t.Fatalf("expect skill_evict log, got %+v", st.logs)
	}
}

// TestArchiveColdSkillsDirect archiveColdSkills 直测：need 大于候选数时按实际候选归档。
func TestArchiveColdSkillsDirect(t *testing.T) {
	st := newFakeSkillStore()
	seedHotSkill(st, "hot-skill", 5)
	seedColdSkill(st, "cold-skill-a", 60)
	seedColdSkill(st, "cold-skill-b", 45)
	// need=5 但只有 2 个冷候选 → 归档 2 个即止。
	if got := archiveColdSkills(context.Background(), st, nil, 5, 30, 90, 1); got != 2 {
		t.Fatalf("archiveColdSkills = %d, want 2", got)
	}
	if st.items["cold-skill-a"].Enabled || st.items["cold-skill-b"].Enabled {
		t.Fatal("cold skills must be disabled")
	}
	if !st.items["hot-skill"].Enabled {
		t.Fatal("hot skill must stay enabled")
	}
}
