package store

// dispatch_attempts.go C-03 最小持久派发防重（team_dispatch_attempts）。
// 只做预留/状态/查询，不做 Worker 队列或自由认领（C-06 未立项）。

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/team"
)

// EnsureDispatchAttemptsSchema 幂等建表（opt-in：auto_dispatch 启用时调用）。
func EnsureDispatchAttemptsSchema(db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS team_dispatch_attempts (
	session_id            TEXT NOT NULL,
	plan_version          BIGINT NOT NULL DEFAULT 0,
	task_id               TEXT NOT NULL,
	attempt_no            INTEGER NOT NULL DEFAULT 0,
	attempt_id            TEXT NOT NULL,
	dispatch_key          TEXT NOT NULL,
	status                TEXT NOT NULL,
	parent_agent_id       TEXT,
	agent_id              TEXT,
	spec_version          TEXT,
	context_manifest_hash TEXT,
	reserved_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at            TIMESTAMPTZ,
	finished_at           TIMESTAMPTZ,
	result_id             TEXT,
	last_error            TEXT,
	PRIMARY KEY (session_id, plan_version, task_id, attempt_no),
	UNIQUE (dispatch_key)
)`)
	if err != nil {
		return fmt.Errorf("ensure team_dispatch_attempts: %w", err)
	}
	return nil
}

// PostgresAttemptStore 实现 team.AttemptStore。
type PostgresAttemptStore struct {
	db *sql.DB
}

// NewPostgresAttemptStore 构造 PG attempt 存储。
func NewPostgresAttemptStore(db *sql.DB) *PostgresAttemptStore {
	return &PostgresAttemptStore{db: db}
}

// Reserve 事务预留：唯一键冲突返回 created=false 并回读既有记录。
func (s *PostgresAttemptStore) Reserve(ctx context.Context, a *team.DispatchAttempt) (bool, *team.DispatchAttempt, error) {
	if err := a.Normalize(); err != nil {
		return false, nil, err
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO team_dispatch_attempts
 (session_id, plan_version, task_id, attempt_no, attempt_id, dispatch_key, status,
  parent_agent_id, agent_id, spec_version, context_manifest_hash, reserved_at, started_at, finished_at, result_id, last_error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (session_id, plan_version, task_id, attempt_no) DO NOTHING`,
		a.SessionID, a.PlanVersion, a.TaskID, a.AttemptNo, a.AttemptID, a.DispatchKey, a.Status,
		nullable(a.ParentAgentID), nullable(a.AgentID), nullable(a.SpecVersion), nullable(a.ContextManifestHash),
		a.ReservedAt, a.StartedAt, a.FinishedAt, nullable(a.ResultID), nullable(a.LastError))
	if err != nil {
		return false, nil, fmt.Errorf("reserve dispatch attempt: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, a, nil
	}
	var cur team.DispatchAttempt
	err = s.db.QueryRowContext(ctx, `
SELECT session_id, plan_version, task_id, attempt_no, attempt_id, dispatch_key, status,
       COALESCE(parent_agent_id,''), COALESCE(agent_id,''), COALESCE(spec_version,''),
       COALESCE(context_manifest_hash,''), reserved_at, started_at, finished_at,
       COALESCE(result_id,''), COALESCE(last_error,'')
FROM team_dispatch_attempts
WHERE session_id=$1 AND plan_version=$2 AND task_id=$3 AND attempt_no=$4`,
		a.SessionID, a.PlanVersion, a.TaskID, a.AttemptNo).
		Scan(&cur.SessionID, &cur.PlanVersion, &cur.TaskID, &cur.AttemptNo, &cur.AttemptID, &cur.DispatchKey,
			&cur.Status, &cur.ParentAgentID, &cur.AgentID, &cur.SpecVersion, &cur.ContextManifestHash,
			&cur.ReservedAt, &cur.StartedAt, &cur.FinishedAt, &cur.ResultID, &cur.LastError)
	if err != nil {
		return false, nil, fmt.Errorf("load existing dispatch attempt: %w", err)
	}
	return false, &cur, nil
}

// Update 更新状态字段（attempt_id 定位）。
func (s *PostgresAttemptStore) Update(ctx context.Context, a *team.DispatchAttempt) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE team_dispatch_attempts
SET status=$2, agent_id=$3, spec_version=$4, context_manifest_hash=$5,
    started_at=$6, finished_at=$7, result_id=$8, last_error=$9
WHERE attempt_id=$1`,
		a.AttemptID, a.Status, nullable(a.AgentID), nullable(a.SpecVersion), nullable(a.ContextManifestHash),
		a.StartedAt, a.FinishedAt, nullable(a.ResultID), nullable(a.LastError))
	if err != nil {
		return fmt.Errorf("update dispatch attempt: %w", err)
	}
	return nil
}

// ListBySession 返回会话全部 attempt。
func (s *PostgresAttemptStore) ListBySession(ctx context.Context, sessionID string) ([]team.DispatchAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, plan_version, task_id, attempt_no, attempt_id, dispatch_key, status,
       COALESCE(parent_agent_id,''), COALESCE(agent_id,''), COALESCE(spec_version,''),
       COALESCE(context_manifest_hash,''), reserved_at, started_at, finished_at,
       COALESCE(result_id,''), COALESCE(last_error,'')
FROM team_dispatch_attempts WHERE session_id=$1 ORDER BY plan_version, task_id, attempt_no`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list dispatch attempts: %w", err)
	}
	defer rows.Close()
	var out []team.DispatchAttempt
	for rows.Next() {
		var a team.DispatchAttempt
		if err := rows.Scan(&a.SessionID, &a.PlanVersion, &a.TaskID, &a.AttemptNo, &a.AttemptID, &a.DispatchKey,
			&a.Status, &a.ParentAgentID, &a.AgentID, &a.SpecVersion, &a.ContextManifestHash,
			&a.ReservedAt, &a.StartedAt, &a.FinishedAt, &a.ResultID, &a.LastError); err != nil {
			return nil, fmt.Errorf("scan dispatch attempt: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkInterrupted 把进程重启前 reserved/running 的 attempt 标为 interrupted。
func (s *PostgresAttemptStore) MarkInterrupted(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE team_dispatch_attempts
SET status=$1, finished_at=now()
WHERE status IN ($2,$3)`, team.AttemptInterrupted, team.AttemptReserved, team.AttemptRunning)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted attempts: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkTerminal 实现 team.AttemptStore。状态条件与更新同事务执行，迟到回调不能覆盖 interrupted/终态。
func (s *PostgresAttemptStore) MarkTerminal(ctx context.Context, attemptID, status, resultID, errText string) error {
	if !team.ValidAttemptTerminal(status) {
		return fmt.Errorf("invalid dispatch attempt terminal status: %s", status)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE team_dispatch_attempts
SET status=$2, result_id=$3, last_error=$4, finished_at=now()
WHERE attempt_id=$1 AND status IN ($5,$6)`,
		attemptID, status, nullable(resultID), nullable(errText), team.AttemptReserved, team.AttemptRunning)
	if err != nil {
		return fmt.Errorf("mark attempt terminal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: attempt=%s", team.ErrAttemptFenced, attemptID)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
