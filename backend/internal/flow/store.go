package flow

// store.go flow_runs / flow_node_runs 的持久化。纪律照 workflow/store.go 先例：
//   - 权威全在表，无内存权威（重启续跑的前提）；
//   - 状态迁移一律行内乐观更新（UPDATE ... WHERE status=旧值），单写者语义靠命中数裁决；
//   - DDL 由 NewPGStore 幂等 ensure（Go 侧为权威，migration 016_flow.sql 供审计参照）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrRunNotFound / ErrNodeRunNotFound 是查询哨兵（HTTP 层映射 404）。
var (
	ErrRunNotFound      = errors.New("flow run not found")
	ErrNodeRunNotFound  = errors.New("flow node run not found")
)

// Store 是引擎所需的持久化面（PG 实现 + 测试内存实现）。
type Store interface {
	CreateRun(ctx context.Context, run *Run, nodeRuns []*NodeRun) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]*Run, error)
	// UpdateRunStatus 乐观迁移 run 状态（WHERE status=from）；to 为终态时写 finished_at。
	// outputs/errMsg 非空时随行落库（answer 汇总/失败原因）。
	UpdateRunStatus(ctx context.Context, id, from, to string, outputs map[string]any, errMsg string) (bool, error)
	ListNodeRuns(ctx context.Context, runID string) ([]*NodeRun, error)
	GetNodeRun(ctx context.Context, id string) (*NodeRun, error)
	// StartNodeRun 乐观迁移 pending → running 并写 started_at。
	StartNodeRun(ctx context.Context, id string) error
	// FinishNodeRun running/waiting/pending → 终态，写 finished_at、入参快照、产出与错误。
	FinishNodeRun(ctx context.Context, id, status string, inputs, outputs map[string]any, errMsg string) error
	// ResetRunningNodes 重启恢复：全表 running 的 node_run 标回 pending（返回条数）。
	ResetRunningNodes(ctx context.Context) (int, error)
	// RunningRunIDs 重启恢复：status=running 的 run。
	RunningRunIDs(ctx context.Context) ([]string, error)
	// PendingRunIDs 排队中的 run（created_at 升序 = FIFO）。
	PendingRunIDs(ctx context.Context, limit int) ([]string, error)
}

// PGStore 是 Store 的 PostgreSQL 实现。
type PGStore struct{ db *sql.DB }

// NewPGStore 构造 PG 存储（db 为 *sql.DB，来自 store.PostgresStore.DB()），
// 并幂等建表（CREATE TABLE IF NOT EXISTS，照 dag.EnsureDAGSchema 先例）。
func NewPGStore(db *sql.DB) (*PGStore, error) {
	if db == nil {
		return nil, fmt.Errorf("flow store not ready: postgres unavailable")
	}
	s := &PGStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, fmt.Errorf("flow ensure schema: %w", err)
	}
	return s, nil
}

// ensureSchema 幂等建表 + 索引。
func (s *PGStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS flow_runs (
		  id           TEXT PRIMARY KEY,
		  flow_id      TEXT NOT NULL,
		  flow_name    TEXT NOT NULL,
		  flow_version INTEGER NOT NULL,
		  flow_json    TEXT NOT NULL,
		  inputs       JSONB,
		  status       TEXT NOT NULL,
		  outputs      JSONB,
		  error        TEXT,
		  session_id   TEXT,
		  workdir      TEXT NOT NULL,
		  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
		  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
		  finished_at  TIMESTAMPTZ
		)`,
		`CREATE TABLE IF NOT EXISTS flow_node_runs (
		  id          TEXT PRIMARY KEY,
		  run_id      TEXT NOT NULL REFERENCES flow_runs(id),
		  node_id     TEXT NOT NULL,
		  node_type   TEXT NOT NULL,
		  status      TEXT NOT NULL,
		  inputs      JSONB,                    -- 解析后的实际入参（prompt/args 模板展开结果）
		  outputs     JSONB,
		  error       TEXT,
		  started_at  TIMESTAMPTZ,
		  finished_at TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_flow_node_runs_run ON flow_node_runs(run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_flow_runs_status ON flow_runs(status)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *PGStore) ready() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("flow store not ready: postgres unavailable")
	}
	return nil
}

// CreateRun 事务内建 run 行 + 全部初始 node_run 行。
func (s *PGStore) CreateRun(ctx context.Context, run *Run, nodeRuns []*NodeRun) error {
	if err := s.ready(); err != nil {
		return err
	}
	if run == nil || strings.TrimSpace(run.ID) == "" {
		return fmt.Errorf("create run: id required")
	}
	inputsJSON, err := json.Marshal(run.Inputs)
	if err != nil {
		return fmt.Errorf("marshal inputs: %w", err)
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
INSERT INTO flow_runs
  (id, flow_id, flow_name, flow_version, flow_json, inputs, status, created_at, updated_at, session_id, workdir)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$8,$9,$10)`,
		run.ID, run.FlowID, run.FlowName, run.FlowVersion, run.FlowJSON, inputsJSON,
		run.Status, run.CreatedAt, nullIfEmpty(run.SessionID), run.Workdir,
	); err != nil {
		return fmt.Errorf("insert flow run %s: %w", run.ID, err)
	}
	for _, nr := range nodeRuns {
		if nr == nil || nr.ID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO flow_node_runs (id, run_id, node_id, node_type, status)
VALUES ($1,$2,$3,$4,$5)`,
			nr.ID, run.ID, nr.NodeID, nr.NodeType, nr.Status,
		); err != nil {
			return fmt.Errorf("insert node run %s: %w", nr.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run %s: %w", run.ID, err)
	}
	return nil
}

// scanRun 扫描一行 flow_runs。
func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var inputs, outputs []byte
	var errMsg, sessionID sql.NullString
	var finishedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.FlowID, &r.FlowName, &r.FlowVersion, &r.FlowJSON,
		&inputs, &r.Status, &outputs, &errMsg, &sessionID, &r.Workdir,
		&r.CreatedAt, &r.UpdatedAt, &finishedAt); err != nil {
		return nil, err
	}
	if len(inputs) > 0 {
		_ = json.Unmarshal(inputs, &r.Inputs)
	}
	if len(outputs) > 0 {
		_ = json.Unmarshal(outputs, &r.Outputs)
	}
	r.Error = errMsg.String
	r.SessionID = sessionID.String
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return &r, nil
}

const runCols = `id, flow_id, flow_name, flow_version, flow_json, inputs, status, outputs, error, session_id, workdir, created_at, updated_at, finished_at`

// GetRun 读取 run 行。
func (s *PGStore) GetRun(ctx context.Context, id string) (*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM flow_runs WHERE id=$1`, id)
	r, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	return r, nil
}

// ListRuns 运行列表（倒序，limit 默认 100 上限 500）。
func (s *PGStore) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	where := ` WHERE 1=1`
	args := []any{}
	if strings.TrimSpace(f.FlowID) != "" {
		args = append(args, f.FlowID)
		where += fmt.Sprintf(` AND flow_id=$%d`, len(args))
	}
	if strings.TrimSpace(f.Status) != "" {
		args = append(args, f.Status)
		where += fmt.Sprintf(` AND status=$%d`, len(args))
	}
	args = append(args, limit)
	q := `SELECT ` + runCols + ` FROM flow_runs` + where + fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, len(args))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRunStatus 乐观迁移 run 状态；to 为终态写 finished_at；outputs/errMsg 非空随行落库。
func (s *PGStore) UpdateRunStatus(ctx context.Context, id, from, to string, outputs map[string]any, errMsg string) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	sets := `status=$3, updated_at=now()`
	args := []any{id, from, to}
	if outputs != nil {
		b, err := json.Marshal(outputs)
		if err != nil {
			return false, fmt.Errorf("marshal outputs: %w", err)
		}
		args = append(args, b)
		sets += fmt.Sprintf(`, outputs=$%d::jsonb`, len(args))
	}
	if errMsg != "" {
		args = append(args, errMsg)
		sets += fmt.Sprintf(`, error=$%d`, len(args))
	}
	if IsRunTerminal(to) {
		sets += `, finished_at=now()`
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE flow_runs SET `+sets+` WHERE id=$1 AND status=$2`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListNodeRuns 读取 run 的全部 node_run（按 node_id 排序，展示稳定）。
func (s *PGStore) ListNodeRuns(ctx context.Context, runID string) ([]*NodeRun, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, node_id, node_type, status, inputs, outputs, error, started_at, finished_at
		 FROM flow_node_runs WHERE run_id=$1 ORDER BY node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NodeRun{}
	for rows.Next() {
		nr, err := scanNodeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, nr)
	}
	return out, rows.Err()
}

// GetNodeRun 读取单个 node_run。
func (s *PGStore) GetNodeRun(ctx context.Context, id string) (*NodeRun, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_id, node_id, node_type, status, inputs, outputs, error, started_at, finished_at
		 FROM flow_node_runs WHERE id=$1`, id)
	nr, err := scanNodeRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNodeRunNotFound
		}
		return nil, err
	}
	return nr, nil
}

func scanNodeRun(row interface{ Scan(...any) error }) (*NodeRun, error) {
	var nr NodeRun
	var inputs, outputs []byte
	var errMsg sql.NullString
	var startedAt, finishedAt sql.NullTime
	if err := row.Scan(&nr.ID, &nr.RunID, &nr.NodeID, &nr.NodeType, &nr.Status,
		&inputs, &outputs, &errMsg, &startedAt, &finishedAt); err != nil {
		return nil, err
	}
	if len(inputs) > 0 {
		_ = json.Unmarshal(inputs, &nr.Inputs)
	}
	if len(outputs) > 0 {
		_ = json.Unmarshal(outputs, &nr.Outputs)
	}
	nr.Error = errMsg.String
	if startedAt.Valid {
		nr.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		nr.FinishedAt = &finishedAt.Time
	}
	return &nr, nil
}

// StartNodeRun pending → running（乐观；未命中不报错——并发剪枝下可能已被标 skipped）。
func (s *PGStore) StartNodeRun(ctx context.Context, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE flow_node_runs SET status=$2, started_at=now() WHERE id=$1 AND status=$3`,
		id, NodeStatusRunning, NodeStatusPending)
	return err
}

// FinishNodeRun running/waiting/pending → 终态，写 finished_at、入参快照、产出与错误。
func (s *PGStore) FinishNodeRun(ctx context.Context, id, status string, inputs, outputs map[string]any, errMsg string) error {
	if err := s.ready(); err != nil {
		return err
	}
	var inJSON, outJSON any
	if inputs != nil {
		b, err := json.Marshal(inputs)
		if err != nil {
			return fmt.Errorf("marshal node inputs: %w", err)
		}
		inJSON = b
	}
	if outputs != nil {
		b, err := json.Marshal(outputs)
		if err != nil {
			return fmt.Errorf("marshal node outputs: %w", err)
		}
		outJSON = b
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE flow_node_runs SET status=$2, inputs=$3::jsonb, outputs=$4::jsonb, error=$5, finished_at=now()
		 WHERE id=$1 AND status IN ($6,$7,$8)`,
		id, status, inJSON, outJSON, nullIfEmpty(errMsg),
		NodeStatusPending, NodeStatusRunning, NodeStatusWaiting)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("node run %s 状态迁移未命中（可能已被并发迁移）", id)
	}
	return nil
}

// ResetRunningNodes 重启恢复：running 的 node_run 标回 pending。
func (s *PGStore) ResetRunningNodes(ctx context.Context) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE flow_node_runs SET status=$1, started_at=NULL WHERE status=$2`,
		NodeStatusPending, NodeStatusRunning)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RunningRunIDs status=running 的 run（重启恢复）。
func (s *PGStore) RunningRunIDs(ctx context.Context) ([]string, error) {
	return s.runIDsByStatus(ctx, RunStatusRunning)
}

// PendingRunIDs status=pending 的 run（FIFO）。
func (s *PGStore) PendingRunIDs(ctx context.Context, limit int) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM flow_runs WHERE status=$1 ORDER BY created_at ASC LIMIT $2`,
		RunStatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *PGStore) runIDsByStatus(ctx context.Context, status string) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM flow_runs WHERE status=$1 ORDER BY created_at ASC`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
