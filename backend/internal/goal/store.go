package goal

// store.go session_directive / goal_reminders 的持久化。纪律照 flow/store.go 先例：
//   - 权威全在表，无内存权威（重启恢复 = 扫 due 表）；
//   - DDL 由 NewPGStore 幂等 ensure（Go 侧为权威，migration 017_goal.sql 供审计参照）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store 是 Manager 所需的持久化面（PG 实现 + 测试内存实现）。
type Store interface {
	// UpsertDirective 按 session_id upsert 指令行（全字段覆盖，status 重置为入参值）。
	UpsertDirective(ctx context.Context, d *Directive) error
	// GetDirectiveBySession 读会话当前指令；无行返回 (nil, nil)。
	GetDirectiveBySession(ctx context.Context, sessionID string) (*Directive, error)
	// TouchTurn 成功评估一轮：turn_count+last_eval+updated_at。
	TouchTurn(ctx context.Context, id string, turn int, lastEval string) error
	// FinishDirective 乐观迁移 active → 终态（complete/failed/stopped），写轮数与评估。
	FinishDirective(ctx context.Context, id, status string, turn int, lastEval string) (bool, error)
	// AddReminder 插一条提醒。
	AddReminder(ctx context.Context, r *Reminder) error
	// ClaimDueReminders 原子认领到期提醒（pending AND run_at<=now → fired）。
	ClaimDueReminders(ctx context.Context, limit int) ([]*Reminder, error)
	// CancelPendingReminders 取消会话全部未触发提醒（模式切换/停止时清场）。
	CancelPendingReminders(ctx context.Context, sessionID string) (int64, error)
	// ListPendingReminders 会话未触发提醒（run_at 升序，展示用）。
	ListPendingReminders(ctx context.Context, sessionID string) ([]*Reminder, error)
}

// PGStore 是 Store 的 PostgreSQL 实现。
type PGStore struct{ db *sql.DB }

// NewPGStore 构造 PG 存储并幂等建表（db 为 *sql.DB，来自 store.PostgresStore.DB()）。
func NewPGStore(db *sql.DB) (*PGStore, error) {
	if db == nil {
		return nil, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	s := &PGStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, fmt.Errorf("goal ensure schema: %w", err)
	}
	return s, nil
}

// ensureSchema 幂等建表 + 索引。
func (s *PGStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS session_directive (
		  id                   TEXT PRIMARY KEY,
		  session_id           TEXT NOT NULL UNIQUE,
		  mode                 TEXT NOT NULL,              -- normal|goal|loop
		  objective            TEXT NOT NULL DEFAULT '',
		  completion_criterion TEXT NOT NULL DEFAULT '',
		  max_turns            INTEGER NOT NULL DEFAULT 0, -- 0=不限预算轮数
		  deadline             TIMESTAMPTZ,
		  loop_interval_sec    INTEGER NOT NULL DEFAULT 0,
		  loop_until           TEXT NOT NULL DEFAULT '',
		  status               TEXT NOT NULL,              -- active|complete|failed|stopped
		  turn_count           INTEGER NOT NULL DEFAULT 0,
		  last_eval            TEXT NOT NULL DEFAULT '',
		  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
		  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS goal_reminders (
		  id           TEXT PRIMARY KEY,
		  session_id   TEXT NOT NULL,
		  directive_id TEXT NOT NULL DEFAULT '',
		  kind         TEXT NOT NULL,              -- once|interval
		  text         TEXT NOT NULL,              -- 到点注入会话的消息正文
		  run_at       TIMESTAMPTZ NOT NULL,
		  status       TEXT NOT NULL,              -- pending|fired|cancelled
		  created_by   TEXT NOT NULL DEFAULT 'system', -- agent|user|system
		  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
		  fired_at     TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_goal_reminders_due ON goal_reminders(status, run_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

const directiveCols = `id, session_id, mode, objective, completion_criterion, max_turns, deadline, loop_interval_sec, loop_until, status, turn_count, last_eval, created_at, updated_at`

// UpsertDirective 按 session_id upsert（INSERT ... ON CONFLICT 全字段覆盖）。
func (s *PGStore) UpsertDirective(ctx context.Context, d *Directive) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("goal store not ready: postgres unavailable")
	}
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	var deadline any
	if d.Deadline != nil {
		deadline = *d.Deadline
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_directive (`+directiveCols+`)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)
ON CONFLICT (session_id) DO UPDATE SET
  mode=EXCLUDED.mode, objective=EXCLUDED.objective,
  completion_criterion=EXCLUDED.completion_criterion, max_turns=EXCLUDED.max_turns,
  deadline=EXCLUDED.deadline, loop_interval_sec=EXCLUDED.loop_interval_sec,
  loop_until=EXCLUDED.loop_until, status=EXCLUDED.status, turn_count=EXCLUDED.turn_count,
  last_eval=EXCLUDED.last_eval, updated_at=EXCLUDED.updated_at`,
		d.ID, d.SessionID, d.Mode, d.Objective, d.CompletionCriterion, d.MaxTurns, deadline,
		d.LoopIntervalSec, d.LoopUntil, d.Status, d.TurnCount, d.LastEval, d.CreatedAt)
	return err
}

func scanDirective(row interface{ Scan(...any) error }) (*Directive, error) {
	var d Directive
	var deadline sql.NullTime
	if err := row.Scan(&d.ID, &d.SessionID, &d.Mode, &d.Objective, &d.CompletionCriterion,
		&d.MaxTurns, &deadline, &d.LoopIntervalSec, &d.LoopUntil, &d.Status,
		&d.TurnCount, &d.LastEval, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if deadline.Valid {
		d.Deadline = &deadline.Time
	}
	return &d, nil
}

// GetDirectiveBySession 读会话当前指令；无行返回 (nil, nil)。
func (s *PGStore) GetDirectiveBySession(ctx context.Context, sessionID string) (*Directive, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+directiveCols+` FROM session_directive WHERE session_id=$1`, sessionID)
	d, err := scanDirective(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

// TouchTurn 评估一轮：turn_count + last_eval + updated_at。
func (s *PGStore) TouchTurn(ctx context.Context, id string, turn int, lastEval string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("goal store not ready: postgres unavailable")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE session_directive SET turn_count=$2, last_eval=$3, updated_at=now() WHERE id=$1`,
		id, turn, lastEval)
	return err
}

// FinishDirective 乐观迁移 active → 终态；未命中（已被并发停止/完成）返回 false。
func (s *PGStore) FinishDirective(ctx context.Context, id, status string, turn int, lastEval string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE session_directive SET status=$2, turn_count=$3, last_eval=$4, updated_at=now()
		 WHERE id=$1 AND status=$5`,
		id, status, turn, lastEval, StatusActive)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AddReminder 插一条提醒（ID 由调用方生成）。
func (s *PGStore) AddReminder(ctx context.Context, r *Reminder) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("goal store not ready: postgres unavailable")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO goal_reminders (id, session_id, directive_id, kind, text, run_at, status, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		r.ID, r.SessionID, r.DirectiveID, r.Kind, r.Text, r.RunAt, r.Status, r.CreatedBy, r.CreatedAt)
	return err
}

// ClaimDueReminders 原子认领到期提醒：逐条 pending→fired（UPDATE 命中才算认领，
// 单进程内也防 ticker 重入）。唤醒失败不回滚 fired（消息至多丢一次，日志可查）。
func (s *PGStore) ClaimDueReminders(ctx context.Context, limit int) ([]*Reminder, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, directive_id, kind, text, run_at, status, created_by, created_at, fired_at
		 FROM goal_reminders WHERE status=$1 AND run_at<=now() ORDER BY run_at ASC LIMIT $2`,
		ReminderStatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	due := []*Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	claimed := []*Reminder{}
	for _, r := range due {
		res, err := s.db.ExecContext(ctx,
			`UPDATE goal_reminders SET status=$2, fired_at=now() WHERE id=$1 AND status=$3`,
			r.ID, ReminderStatusFired, ReminderStatusPending)
		if err != nil {
			return claimed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			now := time.Now()
			r.Status = ReminderStatusFired
			r.FiredAt = &now
			claimed = append(claimed, r)
		}
	}
	return claimed, nil
}

// CancelPendingReminders 取消会话全部未触发提醒；返回取消条数。
func (s *PGStore) CancelPendingReminders(ctx context.Context, sessionID string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE goal_reminders SET status=$2 WHERE session_id=$1 AND status=$3`,
		sessionID, ReminderStatusCancelled, ReminderStatusPending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPendingReminders 会话未触发提醒（run_at 升序）。
func (s *PGStore) ListPendingReminders(ctx context.Context, sessionID string) ([]*Reminder, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("goal store not ready: postgres unavailable")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, directive_id, kind, text, run_at, status, created_by, created_at, fired_at
		 FROM goal_reminders WHERE session_id=$1 AND status=$2 ORDER BY run_at ASC`,
		sessionID, ReminderStatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanReminder(row interface{ Scan(...any) error }) (*Reminder, error) {
	var r Reminder
	var firedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.SessionID, &r.DirectiveID, &r.Kind, &r.Text, &r.RunAt,
		&r.Status, &r.CreatedBy, &r.CreatedAt, &firedAt); err != nil {
		return nil, err
	}
	if firedAt.Valid {
		r.FiredAt = &firedAt.Time
	}
	return &r, nil
}
