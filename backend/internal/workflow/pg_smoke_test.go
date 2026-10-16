package workflow

// pg_smoke_test.go PGStore 的真库冒烟（默认跳过；本机验 SQL 用）：
//
//	cd backend && WORKFLOW_PG_DSN='postgres://...' GOTOOLCHAIN=local go test ./internal/workflow/ -run TestPGStoreSmoke -v
//
// 覆盖 EnsureWorkflowSchema 建表 + run/unit 全 SQL（乐观迁移/JSONB 往返/聚合/恢复复位）。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/blockmemory/agent/backend/internal/store"
)

func TestPGStoreSmoke(t *testing.T) {
	dsn := os.Getenv("WORKFLOW_PG_DSN")
	if dsn == "" {
		t.Skip("WORKFLOW_PG_DSN 未设置，跳过真库冒烟")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 注意：不能 defer db.Close()——t.Cleanup 在 defer 之后执行，先关库会让清行失败。
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := store.EnsureWorkflowSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// 前置清理：上次中断残留的 session-smoke 行（幂等重跑）。
	_, _ = db.ExecContext(ctx, `DELETE FROM workflow_units WHERE run_id IN (SELECT id FROM workflow_runs WHERE session_id='session-smoke')`)
	_, _ = db.ExecContext(ctx, `DELETE FROM workflow_runs WHERE session_id='session-smoke'`)
	s := NewPGStore(db)
	runID := newRunID()
	run := &Run{
		ID: runID, Owner: "", SessionID: "session-smoke", Workdir: "/tmp/wf-smoke",
		RecipeID: "smoke", RecipeVersion: 2, RecipeYAML: "id: smoke\ntool: t\n",
		Roster: []RosterItem{{ID: "a", Name: "件A", Fields: map[string]string{"element": "ice"}}, {ID: "b"}},
		Status: RunStatusPending, BudgetCap: 10, Concurrency: 2, FailFast: true,
	}
	u1 := newAttemptUnit(run, run.Roster[0], &Stage{ID: "concept"}, 1)
	u2 := newAttemptUnit(run, run.Roster[1], &Stage{ID: "walk"}, 1)
	if err := s.CreateRun(ctx, run, []*Unit{u1, u2}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = db.ExecContext(cctx, `DELETE FROM workflow_units WHERE run_id=$1`, runID)
		_, _ = db.ExecContext(cctx, `DELETE FROM workflow_runs WHERE id=$1`, runID)
	})
	// 读回（roster JSONB 往返）。
	got, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.Roster) != 2 || got.Roster[0].Name != "件A" || got.Roster[0].Fields["element"] != "ice" {
		t.Fatalf("roster 往返不符: %+v", got.Roster)
	}
	if got.Status != RunStatusPending || !got.FailFast || got.RecipeVersion != 2 {
		t.Fatalf("run 字段不符: %+v", got)
	}
	// 乐观迁移：pending→running 命中；重复迁移不命中。
	if ok, err := s.UpdateRunStatus(ctx, runID, RunStatusPending, RunStatusRunning, ""); err != nil || !ok {
		t.Fatalf("pending→running 应命中: %v %v", ok, err)
	}
	if ok, _ := s.UpdateRunStatus(ctx, runID, RunStatusPending, RunStatusRunning, ""); ok {
		t.Fatalf("重复迁移不应命中")
	}
	// unit 状态迁移 + patch + JSONB。
	if ok, err := s.TransitionUnit(ctx, u1.ID, []string{UnitStatusPending}, UnitStatusRunning, UnitPatch{ItemName: "件A"}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	cr := `{"program":{"pass":true}}`
	art := "units/a/concept/a_concept.png"
	if ok, err := s.TransitionUnit(ctx, u1.ID, []string{UnitStatusRunning}, UnitStatusDone, UnitPatch{ArtifactPath: &art, CheckResult: &cr}); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	u1r, err := s.GetUnit(ctx, u1.ID)
	if err != nil {
		t.Fatalf("get unit: %v", err)
	}
	// JSONB 读回会被 PG 规范化空白（{"program": {"pass": true}}），按语义比对。
	if u1r.Status != UnitStatusDone || u1r.ArtifactPath != art || u1r.ItemName != "件A" ||
		!strings.Contains(u1r.CheckResult, `"program"`) || !strings.Contains(u1r.CheckResult, `"pass"`) {
		t.Fatalf("unit 字段不符: %+v", u1r)
	}
	// 计数 / latest / 列表。
	if counts, err := s.UnitStatusCounts(ctx, runID); err != nil || counts[UnitStatusDone] != 1 || counts[UnitStatusPending] != 1 {
		t.Fatalf("counts 不符: %+v %v", counts, err)
	}
	latest, err := s.LatestUnit(ctx, runID, "a", "concept")
	if err != nil || latest == nil || latest.AttemptNo != 1 {
		t.Fatalf("latest 不符: %+v %v", latest, err)
	}
	// spent / 提额。
	if spent, err := s.AddRunSpent(ctx, runID, 3); err != nil || spent != 3 {
		t.Fatalf("spent 不符: %d %v", spent, err)
	}
	if cap, err := s.RaiseRunBudget(ctx, runID, 5); err != nil || cap != 15 {
		t.Fatalf("提额不符: %d %v", cap, err)
	}
	// reroll：新 attempt 行（idem_key 唯一）。
	if ok, _ := s.TransitionUnit(ctx, u2.ID, []string{UnitStatusPending}, UnitStatusRunning, UnitPatch{}); !ok {
		t.Fatalf("u2 claim 失败")
	}
	u2b := newAttemptUnit(run, run.Roster[1], &Stage{ID: "walk"}, 2)
	if err := s.CreateUnit(ctx, u2b); err != nil {
		t.Fatalf("reroll 建行失败: %v", err)
	}
	// 重启恢复：running → pending。
	if n, err := s.ResetRunningUnits(ctx); err != nil || n < 1 {
		t.Fatalf("reset running 不符: %d %v", n, err)
	}
	if u2r, _ := s.GetUnit(ctx, u2.ID); u2r.Status != UnitStatusPending {
		t.Fatalf("u2 应回 pending: %s", u2r.Status)
	}
	// 列表检索。
	if runs, err := s.ListRuns(ctx, RunFilter{SessionID: "session-smoke"}); err != nil || len(runs) != 1 {
		t.Fatalf("list runs 不符: %v %v", runs, err)
	}
	if ids, err := s.RunningRunIDs(ctx); err != nil || len(ids) != 1 || ids[0] != runID {
		t.Fatalf("running runs 不符: %v %v", ids, err)
	}
	// 终态收口：running→done（finished_at 落）。
	if ok, err := s.UpdateRunStatus(ctx, runID, RunStatusRunning, RunStatusDone, ""); err != nil || !ok {
		t.Fatalf("running→done: %v %v", ok, err)
	}
	final, _ := s.GetRun(ctx, runID)
	if final.FinishedAt == nil {
		t.Fatalf("done 应写 finished_at")
	}
	if ids, _ := s.RunningRunIDs(ctx); len(ids) != 0 {
		t.Fatalf("done 后不应在 running 列表")
	}
}
