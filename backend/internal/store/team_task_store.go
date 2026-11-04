package store

// team_task_store.go C-06 自认领三表事务存储（默认关闭）。
// team_tasks 是任务投影真相源；team_task_attempts 是执行代数/fencing；
// team_task_events 是同事务 outbox 与审计。old board 仅作 UI 投影。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/team"
)

// EnsureTeamTaskSchema 幂等建三表。
func EnsureTeamTaskSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS team_tasks (
    session_id         TEXT NOT NULL,
    task_id            TEXT NOT NULL,
    owner              TEXT NOT NULL DEFAULT '',
    plan_version       BIGINT NOT NULL DEFAULT 1,
    spec_version       TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'pending',
    role_id            TEXT NOT NULL,
    domain             TEXT NOT NULL DEFAULT '',
    priority           INTEGER NOT NULL DEFAULT 0,
    contract           JSONB NOT NULL DEFAULT '{}'::jsonb,
    depends_on         JSONB NOT NULL DEFAULT '[]'::jsonb,
    acceptance         JSONB NOT NULL DEFAULT '[]'::jsonb,
    write_set          JSONB NOT NULL DEFAULT '[]'::jsonb,
    current_attempt    INTEGER NOT NULL DEFAULT 0,
    lease_owner        TEXT NOT NULL DEFAULT '',
    lease_until        TIMESTAMPTZ,
    max_attempts       INTEGER NOT NULL DEFAULT 1,
    result             TEXT NOT NULL DEFAULT '',
    execution_mode     TEXT NOT NULL DEFAULT 'self_claim',
    goal               TEXT NOT NULL DEFAULT '',
    integration_task_id TEXT NOT NULL DEFAULT '',
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, task_id)
);
CREATE TABLE IF NOT EXISTS team_task_attempts (
    attempt_id       TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL,
    task_id          TEXT NOT NULL,
    attempt_no       INTEGER NOT NULL,
    worker_id        TEXT NOT NULL DEFAULT '',
    agent_id         TEXT NOT NULL DEFAULT '',
    plan_version     BIGINT NOT NULL DEFAULT 1,
    spec_version     TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'claimed',
    heartbeat_at     TIMESTAMPTZ,
    lease_until      TIMESTAMPTZ,
    idempotency_key  TEXT NOT NULL DEFAULT '',
    result           TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, task_id, attempt_no)
);
CREATE TABLE IF NOT EXISTS team_task_events (
    event_id     BIGSERIAL PRIMARY KEY,
    session_id   TEXT NOT NULL,
    task_id      TEXT NOT NULL,
    attempt_id   TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    spec_version TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_team_tasks_ready ON team_tasks(session_id, status, priority, updated_at);
CREATE INDEX IF NOT EXISTS idx_team_attempts_lease ON team_task_attempts(lease_until);
CREATE INDEX IF NOT EXISTS idx_team_events_session ON team_task_events(session_id, event_id);
`)
	if err != nil {
		return fmt.Errorf("ensure team task schema: %w", err)
	}
	return nil
}

// TeamTaskStore PG 实现 team.ClaimStore。
type TeamTaskStore struct{ db *sql.DB }

// NewTeamTaskStore 构造 PG 自认领存储。
func NewTeamTaskStore(db *sql.DB) *TeamTaskStore { return &TeamTaskStore{db: db} }

type pgTaskRow struct {
	Contract    team.TaskContract
	Status      string
	AttemptNo   int
	AttemptID   string
	WorkerID    string
	LeaseUntil  time.Time
	Result      string
	PlanVersion int64
	SpecVersion string
}

// InitPlan 新版本计划：拒绝覆盖 in_progress，其余全量替换 pending/projection。
func (s *TeamTaskStore) InitPlan(ctx context.Context, env team.PlanEnvelope) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin init plan: %w", err)
	}
	defer tx.Rollback()
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_tasks WHERE session_id=$1 AND status=$2`, env.SessionID, team.TaskInProgress).Scan(&running); err != nil {
		return fmt.Errorf("check running tasks: %w", err)
	}
	if running > 0 {
		return fmt.Errorf("plan: session %s has %d in-progress tasks; finish or cancel before replanning", env.SessionID, running)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_tasks WHERE session_id=$1`, env.SessionID); err != nil {
		return fmt.Errorf("replace plan tasks: %w", err)
	}
	for _, t := range env.Tasks {
		cj, _ := json.Marshal(t)
		deps, _ := json.Marshal(t.DependsOn)
		acc, _ := json.Marshal(t.Acceptance)
		ws, _ := json.Marshal(t.WriteSet)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO team_tasks (session_id, task_id, plan_version, spec_version, status, role_id, domain, priority,
  contract, depends_on, acceptance, write_set, max_attempts, execution_mode, goal, integration_task_id, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now())`,
			env.SessionID, t.TaskID, env.PlanVersion, t.SpecVersion, team.TaskPending, t.RoleID, t.Domain, t.Priority,
			cj, deps, acc, ws, t.MaxAttempts, env.ExecutionMode, env.Goal, env.IntegrationTaskID); err != nil {
			return fmt.Errorf("insert task %s: %w", t.TaskID, err)
		}
	}
	if err := insertTeamEvent(ctx, tx, env.SessionID, "", "", "plan_frozen", map[string]any{"plan_version": env.PlanVersion, "tasks": len(env.Tasks)}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit init plan: %w", err)
	}
	return nil
}

// ListSessions 实现 ClaimStore。
func (s *TeamTaskStore) ListSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM team_tasks WHERE execution_mode='self_claim' ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// ListTasks 实现 ClaimStore。
func (s *TeamTaskStore) ListTasks(ctx context.Context, sessionID string) ([]team.TaskState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT contract, status, current_attempt, COALESCE(lease_owner,''), COALESCE(lease_until, now()), result, plan_version, spec_version
FROM team_tasks WHERE session_id=$1 ORDER BY priority DESC, task_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []team.TaskState
	for rows.Next() {
		var raw []byte
		var st team.TaskState
		var lease time.Time
		if err := rows.Scan(&raw, &st.Status, &st.AttemptNo, &st.WorkerID, &lease, &st.Result, new(int64), new(string)); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &st.Contract)
		st.LeaseUntil = lease
		out = append(out, st)
	}
	return out, rows.Err()
}

// ClaimNext 事务认领：锁会话任务 → 依赖/写集/角色过滤 → 选优先级最高。
func (s *TeamTaskStore) ClaimNext(ctx context.Context, sessionID, roleID, workerID string, lease time.Duration) (*team.Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT task_id, contract, status, current_attempt, plan_version, spec_version
FROM team_tasks WHERE session_id=$1 FOR UPDATE`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("lock tasks: %w", err)
	}
	var tasks []pgTaskRow
	for rows.Next() {
		var t pgTaskRow
		var raw []byte
		if err := rows.Scan(&t.Contract.TaskID, &raw, &t.Status, &t.AttemptNo, &t.PlanVersion, &t.SpecVersion); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(raw, &t.Contract)
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	done := map[string]bool{}
	activeWrites := [][]string{}
	for _, t := range tasks {
		if t.Status == team.TaskDone {
			done[t.Contract.TaskID] = true
		}
		if t.Status == team.TaskInProgress {
			activeWrites = append(activeWrites, t.Contract.WriteSet)
		}
	}
	var pick *pgTaskRow
	for i := range tasks {
		t := &tasks[i]
		if t.Status != team.TaskPending || t.Contract.RoleID != roleID || t.AttemptNo >= t.Contract.MaxAttempts || t.Contract.MaxAttempts <= 0 {
			continue
		}
		ready := true
		for _, d := range t.Contract.DependsOn {
			if !done[d] {
				ready = false
				break
			}
		}
		if !ready || writeOverlap(t.Contract.WriteSet, activeWrites) {
			continue
		}
		if pick == nil || t.Contract.Priority > pick.Contract.Priority ||
			(t.Contract.Priority == pick.Contract.Priority && t.Contract.TaskID < pick.Contract.TaskID) {
			pick = t
		}
	}
	if pick == nil {
		return nil, team.ErrNoClaimableTask
	}
	attemptNo := pick.AttemptNo + 1
	attemptID := fmt.Sprintf("%s|%d|%s|a%d", sessionID, pick.PlanVersion, pick.Contract.TaskID, attemptNo)
	leaseUntil := time.Now().Add(lease)
	if _, err := tx.ExecContext(ctx, `UPDATE team_tasks SET status=$3, current_attempt=$4, lease_owner=$5, lease_until=$6, updated_at=now()
WHERE session_id=$1 AND task_id=$2`, sessionID, pick.Contract.TaskID, team.TaskInProgress, attemptNo, workerID, leaseUntil); err != nil {
		return nil, fmt.Errorf("update task claim: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_task_attempts
(attempt_id, session_id, task_id, attempt_no, worker_id, plan_version, spec_version, status, lease_until)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		attemptID, sessionID, pick.Contract.TaskID, attemptNo, workerID, pick.PlanVersion, pick.SpecVersion, team.ClaimAttemptRunning, leaseUntil); err != nil {
		return nil, fmt.Errorf("insert attempt: %w", err)
	}
	if err := insertTeamEvent(ctx, tx, sessionID, pick.Contract.TaskID, attemptID, "task_claimed", map[string]any{"worker_id": workerID}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &team.Claim{
		SessionID: sessionID, TaskID: pick.Contract.TaskID, AttemptID: attemptID, AttemptNo: attemptNo,
		PlanVersion: pick.PlanVersion, SpecVersion: pick.SpecVersion, WorkerID: workerID,
		LeaseUntil: leaseUntil, Task: pick.Contract,
	}, nil
}

// Heartbeat 续租（attempt+worker+plan 全匹配）。
func (s *TeamTaskStore) Heartbeat(ctx context.Context, b team.ClaimBinding, lease time.Duration) error {
	until := time.Now().Add(lease)
	res, err := s.db.ExecContext(ctx, `UPDATE team_task_attempts SET heartbeat_at=now(), lease_until=$4
WHERE attempt_id=$1 AND worker_id=$2 AND plan_version=$3 AND status=$5`,
		b.AttemptID, b.WorkerID, b.PlanVersion, until, team.ClaimAttemptRunning)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return team.ErrAttemptFence
	}
	_, err = s.db.ExecContext(ctx, `UPDATE team_tasks SET lease_until=$3, updated_at=now()
WHERE session_id=$1 AND task_id=$2 AND current_attempt=$4 AND lease_owner=$5`,
		b.SessionID, b.TaskID, until, b.AttemptNo, b.WorkerID)
	return err
}

// Complete 写终态（fencing）。
func (s *TeamTaskStore) Complete(ctx context.Context, b team.ClaimBinding, res team.ClaimResult) error {
	return s.finish(ctx, b, res.Status, res.Summary)
}

// Fail 写失败/阻塞（fencing）。
func (s *TeamTaskStore) Fail(ctx context.Context, b team.ClaimBinding, status, errText string) error {
	return s.finish(ctx, b, status, errText)
}

func (s *TeamTaskStore) finish(ctx context.Context, b team.ClaimBinding, status, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	attemptStatus := team.ClaimAttemptCompleted
	if status == team.TaskFailed || status == team.TaskBlocked || status == team.TaskCancelled {
		attemptStatus = team.ClaimAttemptFailed
	}
	res, err := tx.ExecContext(ctx, `UPDATE team_task_attempts SET status=$4, result=$5, error=$6, lease_until=NULL
WHERE attempt_id=$1 AND worker_id=$2 AND plan_version=$3 AND status=$7`,
		b.AttemptID, b.WorkerID, b.PlanVersion, attemptStatus, text, text, team.ClaimAttemptRunning)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return team.ErrAttemptFence
	}
	res, err = tx.ExecContext(ctx, `UPDATE team_tasks SET status=$4, result=$5, lease_owner='', lease_until=NULL, updated_at=now()
WHERE session_id=$1 AND task_id=$2 AND current_attempt=$3 AND lease_owner=$6`,
		b.SessionID, b.TaskID, b.AttemptNo, status, text, b.WorkerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return team.ErrAttemptFence
	}
	if err := insertTeamEvent(ctx, tx, b.SessionID, b.TaskID, b.AttemptID, "task_terminal", map[string]any{"status": status}); err != nil {
		return err
	}
	return tx.Commit()
}

// Release 放回 pending（不重放 attempt，仅释放租约）。
func (s *TeamTaskStore) Release(ctx context.Context, b team.ClaimBinding) error {
	_, err := s.db.ExecContext(ctx, `UPDATE team_tasks SET status=$3, lease_owner='', lease_until=NULL, updated_at=now()
WHERE session_id=$1 AND task_id=$2 AND current_attempt=$4 AND lease_owner=$5`,
		b.SessionID, b.TaskID, team.TaskPending, b.AttemptNo, b.WorkerID)
	return err
}

func insertTeamEvent(ctx context.Context, tx *sql.Tx, sid, taskID, attemptID, kind string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	_, err := tx.ExecContext(ctx, `INSERT INTO team_task_events (session_id, task_id, attempt_id, kind, payload)
VALUES ($1,$2,$3,$4,$5)`, sid, taskID, attemptID, kind, raw)
	return err
}

func writeOverlap(a []string, active [][]string) bool {
	if len(a) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, p := range a {
		set[p] = true
	}
	for _, ws := range active {
		for _, p := range ws {
			if set[p] {
				return true
			}
		}
	}
	return false
}
