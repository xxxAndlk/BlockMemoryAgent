package workflow

// store.go workflow_runs / workflow_units 的持久化（开发文档 §4.1、§5.5）。
// 纪律：
//   - 权威全在表 + 事件账本，无内存权威（重启续跑的前提）；
//   - 状态迁移一律行内乐观更新（UPDATE ... WHERE status=旧值），单写者语义靠命中数裁决；
//   - 被否 attempt 只标不删（拒绝理由是配方演化的输入）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ErrRunNotFound / ErrUnitNotFound 是查询哨兵（HTTP 层映射 404）。
var (
	ErrRunNotFound  = errors.New("workflow run not found")
	ErrUnitNotFound = errors.New("workflow unit not found")
)

// RunFilter 是运行列表过滤条件（零值=全部）。
type RunFilter struct {
	SessionID string
	Status    string
	Limit     int
}

// UnitPatch 是 unit 迁移时随行更新的字段（nil=不改）。
type UnitPatch struct {
	ArtifactPath *string
	CheckResult  *string
	RejectReason *string
	ItemName     string // 非空时一并回填展示名（建行后引擎补写）
}

// Store 是引擎所需的持久化面（PG 实现 + 测试内存实现）。
type Store interface {
	CreateRun(ctx context.Context, run *Run, units []*Unit) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]*Run, error)
	// UpdateRunStatus 乐观迁移 run 状态（WHERE status=from）；返回是否命中。
	// to 为终态时写 finished_at；to=running 时清 finished_at（reroll 复活）。
	UpdateRunStatus(ctx context.Context, id, from, to, pauseReason string) (bool, error)
	// AddRunSpent 累加已消耗估算，返回新值。
	AddRunSpent(ctx context.Context, id string, delta int) (int, error)
	// RaiseRunBudget 预算提额（续跑前置），返回新上限。
	RaiseRunBudget(ctx context.Context, id string, extra int) (int, error)
	ListUnits(ctx context.Context, runID string) ([]*Unit, error)
	GetUnit(ctx context.Context, id string) (*Unit, error)
	// LatestUnit 返回 (item, stage) 的最新 attempt（无则 nil, nil）。
	LatestUnit(ctx context.Context, runID, itemID, stage string) (*Unit, error)
	CreateUnit(ctx context.Context, u *Unit) error
	// TransitionUnit 乐观迁移 unit 状态（WHERE status ∈ from）；返回是否命中。
	TransitionUnit(ctx context.Context, id string, from []string, to string, patch UnitPatch) (bool, error)
	// UnitStatusCounts 按状态聚合该 run 的 unit 数（寻 next attempt_no / 进度）。
	UnitStatusCounts(ctx context.Context, runID string) (map[string]int, error)
	// ResetRunningUnits 重启恢复：全表 running 的 unit 标回 pending（返回条数）。
	ResetRunningUnits(ctx context.Context) (int, error)
	// RunningRunIDs 重启恢复：status=running 的 run。
	RunningRunIDs(ctx context.Context) ([]string, error)
	// PendingRunIDs 排队中的 run（created_at 升序 = FIFO）。
	PendingRunIDs(ctx context.Context, limit int) ([]string, error)
}

// PGStore 是 Store 的 PostgreSQL 实现。
type PGStore struct{ db *sql.DB }

// NewPGStore 构造 PG 存储（db 为 *sql.DB，来自 store.PostgresStore.DB()）。
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

func (s *PGStore) ready() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow store not ready: postgres unavailable")
	}
	return nil
}

// CreateRun 事务内建 run 行 + 全部初始 unit 行。
func (s *PGStore) CreateRun(ctx context.Context, run *Run, units []*Unit) error {
	if err := s.ready(); err != nil {
		return err
	}
	if run == nil || strings.TrimSpace(run.ID) == "" {
		return fmt.Errorf("create run: id required")
	}
	rosterJSON, err := json.Marshal(run.Roster)
	if err != nil {
		return fmt.Errorf("marshal roster: %w", err)
	}
	now := time.Now()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_runs
  (id, owner, session_id, workdir, recipe_id, recipe_version, recipe_yaml, roster,
   status, pause_reason, budget_cap, spent_estimate, concurrency, fail_fast, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,$11,$12,$13,$14,$14)`,
		run.ID, run.Owner, run.SessionID, run.Workdir, run.RecipeID, run.RecipeVersion, run.RecipeYAML,
		rosterJSON, run.Status, run.BudgetCap, run.SpentEstimate, run.Concurrency, run.FailFast, run.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert run %s: %w", run.ID, err)
	}
	for _, u := range units {
		if err := insertUnitTx(ctx, tx, u, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run %s: %w", run.ID, err)
	}
	return nil
}

func insertUnitTx(ctx context.Context, tx *sql.Tx, u *Unit, now time.Time) error {
	if u == nil || u.ID == "" {
		return fmt.Errorf("create unit: id required")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	var cr any
	if strings.TrimSpace(u.CheckResult) != "" {
		cr = u.CheckResult
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_units
  (id, run_id, owner, item_id, item_name, stage, attempt_no, status, artifact_path, check_result, reject_reason, idem_key, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$13)`,
		u.ID, u.RunID, u.Owner, u.ItemID, nullIfEmpty(u.ItemName), u.Stage, u.AttemptNo, u.Status,
		nullIfEmpty(u.ArtifactPath), cr, nullIfEmpty(u.RejectReason), u.IdemKey, u.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert unit %s: %w", u.ID, err)
	}
	return nil
}

// CreateUnit 建一行新 attempt（reroll）。
func (s *PGStore) CreateUnit(ctx context.Context, u *Unit) error {
	if err := s.ready(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := insertUnitTx(ctx, tx, u, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

const runColumns = `id, owner, session_id, workdir, recipe_id, recipe_version, recipe_yaml, roster,
  status, COALESCE(pause_reason,''), budget_cap, spent_estimate, concurrency, fail_fast, created_at, updated_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var rosterJSON []byte
	var finished sql.NullTime
	if err := row.Scan(&r.ID, &r.Owner, &r.SessionID, &r.Workdir, &r.RecipeID, &r.RecipeVersion,
		&r.RecipeYAML, &rosterJSON, &r.Status, &r.PauseReason, &r.BudgetCap, &r.SpentEstimate,
		&r.Concurrency, &r.FailFast, &r.CreatedAt, &r.UpdatedAt, &finished); err != nil {
		return nil, err
	}
	if len(rosterJSON) > 0 {
		_ = json.Unmarshal(rosterJSON, &r.Roster)
	}
	if finished.Valid {
		t := finished.Time
		r.FinishedAt = &t
	}
	return &r, nil
}

// GetRun 按 id 取 run（不存在返回 ErrRunNotFound）。
func (s *PGStore) GetRun(ctx context.Context, id string) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM workflow_runs WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}
	return r, nil
}

// ListRuns 运行列表（session/status 过滤；created_at 倒序）。
func (s *PGStore) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	q := `SELECT ` + runColumns + ` FROM workflow_runs WHERE 1=1`
	var args []any
	if f.SessionID != "" {
		args = append(args, f.SessionID)
		q += fmt.Sprintf(` AND session_id=$%d`, len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		q += fmt.Sprintf(` AND status=$%d`, len(args))
	}
	q += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRunStatus 乐观迁移（WHERE status=from）；终态写 finished_at，回 running 清 finished_at。
func (s *PGStore) UpdateRunStatus(ctx context.Context, id, from, to, pauseReason string) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	var pr any
	if strings.TrimSpace(pauseReason) != "" {
		pr = pauseReason
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE workflow_runs SET
  status=$3,
  pause_reason=$4,
  updated_at=now(),
  finished_at=CASE
    WHEN $3 IN ('done','failed','aborted') THEN now()
    WHEN $3='running' THEN NULL
    ELSE finished_at
  END
WHERE id=$1 AND status=$2`, id, from, to, pr)
	if err != nil {
		return false, fmt.Errorf("update run %s status %s->%s: %w", id, from, to, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AddRunSpent 累加消耗估算，返回新值。
func (s *PGStore) AddRunSpent(ctx context.Context, id string, delta int) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var spent int
	err := s.db.QueryRowContext(ctx, `
UPDATE workflow_runs SET spent_estimate = spent_estimate + $2, updated_at=now()
WHERE id=$1 RETURNING spent_estimate`, id, delta).Scan(&spent)
	if err == sql.ErrNoRows {
		return 0, ErrRunNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("add run spent %s: %w", id, err)
	}
	return spent, nil
}

// RaiseRunBudget 预算提额，返回新上限。
func (s *PGStore) RaiseRunBudget(ctx context.Context, id string, extra int) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	if extra <= 0 {
		extra = 0
	}
	var cap int
	err := s.db.QueryRowContext(ctx, `
UPDATE workflow_runs SET budget_cap = budget_cap + $2, updated_at=now()
WHERE id=$1 RETURNING budget_cap`, id, extra).Scan(&cap)
	if err == sql.ErrNoRows {
		return 0, ErrRunNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("raise run budget %s: %w", id, err)
	}
	return cap, nil
}

const unitColumns = `id, run_id, owner, item_id, COALESCE(item_name,''), stage, attempt_no, status,
  COALESCE(artifact_path,''), COALESCE(check_result::text,''), COALESCE(reject_reason,''), idem_key, created_at, updated_at`

func scanUnit(row interface{ Scan(...any) error }) (*Unit, error) {
	var u Unit
	if err := row.Scan(&u.ID, &u.RunID, &u.Owner, &u.ItemID, &u.ItemName, &u.Stage, &u.AttemptNo,
		&u.Status, &u.ArtifactPath, &u.CheckResult, &u.RejectReason, &u.IdemKey, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUnits 返回该 run 全部 attempt 行（时间序；前端按 (item,stage) 取最新）。
func (s *PGStore) ListUnits(ctx context.Context, runID string) ([]*Unit, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+unitColumns+` FROM workflow_units WHERE run_id=$1 ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list units %s: %w", runID, err)
	}
	defer rows.Close()
	out := []*Unit{}
	for rows.Next() {
		u, err := scanUnit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unit: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUnit 按 id 取 unit（不存在返回 ErrUnitNotFound）。
func (s *PGStore) GetUnit(ctx context.Context, id string) (*Unit, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	u, err := scanUnit(s.db.QueryRowContext(ctx, `SELECT `+unitColumns+` FROM workflow_units WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, ErrUnitNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get unit %s: %w", id, err)
	}
	return u, nil
}

// LatestUnit 返回 (item, stage) 的最新 attempt（无则 nil, nil）。
func (s *PGStore) LatestUnit(ctx context.Context, runID, itemID, stage string) (*Unit, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	u, err := scanUnit(s.db.QueryRowContext(ctx, `
SELECT `+unitColumns+` FROM workflow_units
WHERE run_id=$1 AND item_id=$2 AND stage=$3
ORDER BY attempt_no DESC LIMIT 1`, runID, itemID, stage))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest unit %s/%s/%s: %w", runID, itemID, stage, err)
	}
	return u, nil
}

// TransitionUnit 乐观迁移 unit 状态（WHERE status ∈ from）；patch 非 nil 字段随行更新。
func (s *PGStore) TransitionUnit(ctx context.Context, id string, from []string, to string, patch UnitPatch) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	if len(from) == 0 {
		from = []string{""}
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE workflow_units SET
  status=$2,
  artifact_path=CASE WHEN $3::text IS NULL THEN artifact_path ELSE $3 END,
  check_result=CASE WHEN $4::text IS NULL THEN check_result ELSE $4::jsonb END,
  reject_reason=CASE WHEN $5::text IS NULL THEN reject_reason ELSE $5 END,
  item_name=CASE WHEN $6='' THEN item_name ELSE $6 END,
  updated_at=now()
WHERE id=$1 AND status = ANY($7)`,
		id, to, patch.ArtifactPath, patch.CheckResult, patch.RejectReason, patch.ItemName, pq.Array(from))
	if err != nil {
		return false, fmt.Errorf("transition unit %s -> %s: %w", id, to, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UnitStatusCounts 按状态聚合（进度/熔断统计）。
func (s *PGStore) UnitStatusCounts(ctx context.Context, runID string) (map[string]int, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM workflow_units WHERE run_id=$1 GROUP BY status`, runID)
	if err != nil {
		return nil, fmt.Errorf("unit counts %s: %w", runID, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("scan unit count: %w", err)
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ResetRunningUnits 重启恢复：全表 running 的 unit 标回 pending（幂等键不变，重跑重执行该 attempt 行）。
func (s *PGStore) ResetRunningUnits(ctx context.Context) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE workflow_units SET status='pending', updated_at=now() WHERE status='running'`)
	if err != nil {
		return 0, fmt.Errorf("reset running units: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RunningRunIDs 返回 status=running 的 run（重启恢复入调度）。
func (s *PGStore) RunningRunIDs(ctx context.Context) ([]string, error) {
	return s.runIDsByStatus(ctx, RunStatusRunning, 0)
}

// PendingRunIDs 返回排队中的 run（FIFO）。
func (s *PGStore) PendingRunIDs(ctx context.Context, limit int) ([]string, error) {
	return s.runIDsByStatus(ctx, RunStatusPending, limit)
}

func (s *PGStore) runIDsByStatus(ctx context.Context, status string, limit int) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	q := `SELECT id FROM workflow_runs WHERE status=$1 ORDER BY created_at ASC`
	args := []any{status}
	if limit > 0 {
		args = append(args, limit)
		q += ` LIMIT $2`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs by status %s: %w", status, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
