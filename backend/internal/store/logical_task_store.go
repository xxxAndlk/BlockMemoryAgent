package store

// logical_task_store.go WP1/WP2 逻辑任务与 Action 事件 PG 实现。
//
// 三张表（计划 §3.5）：
//   logical_tasks          —— 当前投影（每会话每任务一行，状态只 UPDATE）
//   logical_task_events    —— append-only 底账，(session_id, idempotency_key) 唯一
//   action_events          —— append-only Action 阶段事件，(action_id, seq) 唯一
//
// 硬约定：
//   - 三张表都带 owner（多租户预留，查询必带 owner 条件——当前单租户留空=不限制）；
//   - 事件只追加、投影可重建；重规划/压缩/回滚/file 恢复都不物理删行；
//   - 会话显式删除时由 sessionScopedTables 级联清理（见 session_store.go）。
//
// 运行时权威 DDL 在本文件（幂等 Ensure），migrations/020_logical_tasks.sql 供
// make migrate / 审计参照。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/team"
)

// EnsureLogicalTaskSchema 幂等建三表（logical_tasks / logical_task_events / action_events）。
func EnsureLogicalTaskSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS logical_tasks (
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    revision          BIGINT NOT NULL DEFAULT 1,
    parent_revision   BIGINT NOT NULL DEFAULT 0,
    status            VARCHAR(32) NOT NULL DEFAULT 'active',
    goal              TEXT NOT NULL DEFAULT '',
    content_hash      VARCHAR(80) NOT NULL DEFAULT '',
    base_checkpoint   TEXT NOT NULL DEFAULT '',
    contract          JSONB NOT NULL DEFAULT '{}'::jsonb,
    origin_event_id   BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, logical_task_id)
);
CREATE INDEX IF NOT EXISTS idx_logical_tasks_session_status
    ON logical_tasks (session_id, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_logical_tasks_owner
    ON logical_tasks (owner, session_id);

CREATE TABLE IF NOT EXISTS logical_task_events (
    event_id          BIGSERIAL PRIMARY KEY,
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    revision          BIGINT NOT NULL DEFAULT 1,
    kind              VARCHAR(32) NOT NULL,
    payload           TEXT NOT NULL DEFAULT '',
    origin_event_id   BIGINT NOT NULL DEFAULT 0,
    idempotency_key   VARCHAR(200) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_logical_task_events_task
    ON logical_task_events (session_id, logical_task_id, event_id);

CREATE TABLE IF NOT EXISTS action_events (
    id                BIGSERIAL PRIMARY KEY,
    action_id         VARCHAR(80) NOT NULL,
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL DEFAULT '',
    contract_revision BIGINT NOT NULL DEFAULT 0,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    seq               INTEGER NOT NULL,
    phase             VARCHAR(40) NOT NULL,
    tool              VARCHAR(80) NOT NULL DEFAULT '',
    request_ref       TEXT NOT NULL DEFAULT '',
    before_checkpoint TEXT NOT NULL DEFAULT '',
    after_checkpoint  TEXT NOT NULL DEFAULT '',
    mutation_ref      TEXT NOT NULL DEFAULT '',
    error_category    VARCHAR(40) NOT NULL DEFAULT '',
    payload           TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (action_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_action_events_session
    ON action_events (session_id, id);
CREATE INDEX IF NOT EXISTS idx_action_events_task
    ON action_events (session_id, logical_task_id, id);
`)
	return err
}

// PGLogicalTaskStore team.LogicalTaskStore 的 PG 实现。
type PGLogicalTaskStore struct{ db *sql.DB }

// NewPGLogicalTaskStore 构造 PG 存储（db 为空时所有方法返回错误，调用方应回退内存实现）。
func NewPGLogicalTaskStore(db *sql.DB) *PGLogicalTaskStore { return &PGLogicalTaskStore{db: db} }

func (s *PGLogicalTaskStore) ready() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("logical task store: postgres not configured")
	}
	return nil
}

// CreateTask 实现 team.LogicalTaskStore：单事务写投影 + created 事件，
// 并把同会话其它 active 投影标记为 superseded（历史保留，不删除）。
func (s *PGLogicalTaskStore) CreateTask(ctx context.Context, c *team.LogicalTaskContract, ev team.LogicalTaskEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("logical task: contract required")
	}
	if err := team.NormalizeContract(c); err != nil {
		return err
	}
	if err := team.ValidateContract(c); err != nil {
		return err
	}
	if strings.TrimSpace(ev.IdempotencyKey) == "" {
		return fmt.Errorf("logical task: idempotency_key required")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("logical task: marshal contract: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("logical task: begin: %w", err)
	}
	defer tx.Rollback()
	// 幂等：同 (session_id, idempotency_key) 已存在则整体跳过。
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM logical_task_events WHERE session_id=$1 AND idempotency_key=$2)`,
		c.SessionID, ev.IdempotencyKey).Scan(&exists); err != nil {
		return fmt.Errorf("logical task: check idempotency: %w", err)
	}
	if exists {
		return tx.Commit()
	}
	// 单 active 约束：旧 active/draft 投影转 superseded（只改状态）。
	if _, err := tx.ExecContext(ctx, `
UPDATE logical_tasks SET status='superseded', updated_at=now()
WHERE session_id=$1 AND logical_task_id<>$2 AND status IN ('active','draft')`,
		c.SessionID, c.LogicalTaskID); err != nil {
		return fmt.Errorf("logical task: supersede previous: %w", err)
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO logical_tasks
    (session_id, logical_task_id, owner, revision, parent_revision, status, goal,
     content_hash, base_checkpoint, contract, origin_event_id, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (session_id, logical_task_id) DO UPDATE SET
    owner=EXCLUDED.owner, revision=EXCLUDED.revision, parent_revision=EXCLUDED.parent_revision,
    status=EXCLUDED.status, goal=EXCLUDED.goal, content_hash=EXCLUDED.content_hash,
    base_checkpoint=EXCLUDED.base_checkpoint, contract=EXCLUDED.contract,
    origin_event_id=EXCLUDED.origin_event_id, updated_at=EXCLUDED.updated_at`,
		c.SessionID, c.LogicalTaskID, c.Owner, c.Revision, c.ParentRevision, c.Status, c.Goal,
		c.ContentHash, c.BaseCheckpoint, string(payload), c.OriginEventID, c.CreatedAt, now); err != nil {
		return fmt.Errorf("logical task: upsert projection: %w", err)
	}
	rev := ev.Revision
	if rev <= 0 {
		rev = c.Revision
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO logical_task_events
    (session_id, logical_task_id, owner, revision, kind, payload, origin_event_id, idempotency_key)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		c.SessionID, c.LogicalTaskID, c.Owner, rev, ev.Kind, string(payload), ev.OriginEventID, ev.IdempotencyKey); err != nil {
		return fmt.Errorf("logical task: insert event: %w", err)
	}
	return tx.Commit()
}

// AppendEvent 实现 team.LogicalTaskStore：事件先入账，再更新投影，单事务。
func (s *PGLogicalTaskStore) AppendEvent(ctx context.Context, c *team.LogicalTaskContract, ev team.LogicalTaskEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("logical task: contract required")
	}
	if err := team.NormalizeContract(c); err != nil {
		return err
	}
	if err := team.ValidateContract(c); err != nil {
		return err
	}
	if strings.TrimSpace(ev.IdempotencyKey) == "" {
		return fmt.Errorf("logical task: idempotency_key required")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("logical task: marshal contract: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("logical task: begin: %w", err)
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM logical_task_events WHERE session_id=$1 AND idempotency_key=$2)`,
		c.SessionID, ev.IdempotencyKey).Scan(&exists); err != nil {
		return fmt.Errorf("logical task: check idempotency: %w", err)
	}
	if exists {
		return tx.Commit()
	}
	rev := ev.Revision
	if rev <= 0 {
		rev = c.Revision
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO logical_task_events
    (session_id, logical_task_id, owner, revision, kind, payload, origin_event_id, idempotency_key)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		c.SessionID, c.LogicalTaskID, c.Owner, rev, ev.Kind, ev.Payload, ev.OriginEventID, ev.IdempotencyKey); err != nil {
		return fmt.Errorf("logical task: insert event: %w", err)
	}
	if c.Status == team.TaskStatusActive || c.Status == team.TaskStatusDraft {
		if _, err := tx.ExecContext(ctx, `
UPDATE logical_tasks SET status='superseded', updated_at=now()
WHERE session_id=$1 AND logical_task_id<>$2 AND status IN ('active','draft')`,
			c.SessionID, c.LogicalTaskID); err != nil {
			return fmt.Errorf("logical task: supersede previous: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE logical_tasks SET
    revision=$3, parent_revision=$4, status=$5, goal=$6, content_hash=$7,
    base_checkpoint=$8, contract=$9, updated_at=now()
WHERE session_id=$1 AND logical_task_id=$2`,
		c.SessionID, c.LogicalTaskID, c.Revision, c.ParentRevision, c.Status, c.Goal,
		c.ContentHash, c.BaseCheckpoint, string(payload)); err != nil {
		return fmt.Errorf("logical task: update projection: %w", err)
	}
	return tx.Commit()
}

// GetActive 实现 team.LogicalTaskStore。
func (s *PGLogicalTaskStore) GetActive(ctx context.Context, sessionID string) (*team.LogicalTaskContract, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT contract FROM logical_tasks
WHERE session_id=$1 AND status IN ('active','draft')
ORDER BY updated_at DESC LIMIT 1`, sessionID)
	return scanContract(row)
}

// GetByID 实现 team.LogicalTaskStore。
func (s *PGLogicalTaskStore) GetByID(ctx context.Context, sessionID, taskID string) (*team.LogicalTaskContract, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT contract FROM logical_tasks WHERE session_id=$1 AND logical_task_id=$2`, sessionID, taskID)
	return scanContract(row)
}

func scanContract(row *sql.Row) (*team.LogicalTaskContract, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrLogicalTaskNotFound
		}
		return nil, fmt.Errorf("logical task: query projection: %w", err)
	}
	var c team.LogicalTaskContract
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("logical task: decode projection: %w", err)
	}
	return &c, nil
}

// ListEvents 实现 team.LogicalTaskStore。
func (s *PGLogicalTaskStore) ListEvents(ctx context.Context, sessionID, taskID string) ([]team.LogicalTaskEvent, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	q := `SELECT event_id, session_id, logical_task_id, owner, revision, kind, payload,
	             origin_event_id, idempotency_key, created_at
	      FROM logical_task_events WHERE session_id=$1`
	args := []any{sessionID}
	if taskID != "" {
		q += ` AND logical_task_id=$2`
		args = append(args, taskID)
	}
	q += ` ORDER BY event_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("logical task: list events: %w", err)
	}
	defer rows.Close()
	out := make([]team.LogicalTaskEvent, 0, 8)
	for rows.Next() {
		var ev team.LogicalTaskEvent
		if err := rows.Scan(&ev.EventID, &ev.SessionID, &ev.LogicalTaskID, &ev.Owner, &ev.Revision,
			&ev.Kind, &ev.Payload, &ev.OriginEventID, &ev.IdempotencyKey, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("logical task: scan event: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListTasks 实现 team.LogicalTaskStore。
func (s *PGLogicalTaskStore) ListTasks(ctx context.Context, sessionID string) ([]*team.LogicalTaskContract, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT contract FROM logical_tasks WHERE session_id=$1 ORDER BY revision, logical_task_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("logical task: list tasks: %w", err)
	}
	defer rows.Close()
	out := make([]*team.LogicalTaskContract, 0, 4)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("logical task: scan task: %w", err)
		}
		var c team.LogicalTaskContract
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("logical task: decode task: %w", err)
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

// FindAcceptable 实现 team.LogicalTaskStore：取会话中“等用户拍板”的最新一条交付任务。
//
// 选型依据是**交付事实**（status 为 verified/delivered-unverified + updated_at 倒序），
// 不是数组末尾/最大 revision——旧任务被多次 refine 后 revision 会比新任务大，
// 按 revision 选会误操作废弃任务（2026-11-19 再次验收 P1-2）。
func (s *PGLogicalTaskStore) FindAcceptable(ctx context.Context, sessionID string) (*team.LogicalTaskContract, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT contract FROM logical_tasks
WHERE session_id=$1 AND status IN ('verified','delivered-unverified')
ORDER BY updated_at DESC, created_at DESC LIMIT 1`, sessionID)
	return scanContract(row)
}

// --- Action 事件存储（WP2） ---------------------------------------------------

// ActionEvent 一条 Action 阶段事件（append-only）。
type ActionEvent struct {
	ID               int64     `json:"id"`
	ActionID         string    `json:"action_id"`
	SessionID        string    `json:"session_id"`
	LogicalTaskID    string    `json:"logical_task_id,omitempty"`
	ContractRevision int64     `json:"contract_revision,omitempty"`
	Owner            string    `json:"owner,omitempty"`
	Seq              int       `json:"seq"`
	Phase            string    `json:"phase"`
	Tool             string    `json:"tool"`
	RequestRef       string    `json:"request_ref,omitempty"`
	BeforeCheckpoint string    `json:"before_checkpoint,omitempty"`
	AfterCheckpoint  string    `json:"after_checkpoint,omitempty"`
	MutationRef      string    `json:"mutation_ref,omitempty"`
	ErrorCategory    string    `json:"error_category,omitempty"`
	Payload          string    `json:"payload,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ActionEventStore 追加 Action 事件并读取（(action_id, seq) 唯一）。
type ActionEventStore interface {
	Append(ctx context.Context, ev ActionEvent) error
	ListByAction(ctx context.Context, actionID string) ([]ActionEvent, error)
	ListBySession(ctx context.Context, sessionID, taskID string, limit int) ([]ActionEvent, error)
}

// PGActionEventStore ActionEventStore 的 PG 实现。
type PGActionEventStore struct{ db *sql.DB }

// NewPGActionEventStore 构造 PG Action 事件存储。
func NewPGActionEventStore(db *sql.DB) *PGActionEventStore { return &PGActionEventStore{db: db} }

// Append 实现 ActionEventStore（(action_id, seq) 冲突即忽略，保证重试幂等）。
func (s *PGActionEventStore) Append(ctx context.Context, ev ActionEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("action event store: postgres not configured")
	}
	if strings.TrimSpace(ev.ActionID) == "" || ev.Seq <= 0 {
		return fmt.Errorf("action event: action_id and seq required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO action_events
    (action_id, session_id, logical_task_id, contract_revision, owner, seq, phase, tool,
     request_ref, before_checkpoint, after_checkpoint, mutation_ref, error_category, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (action_id, seq) DO NOTHING`,
		ev.ActionID, ev.SessionID, ev.LogicalTaskID, ev.ContractRevision, ev.Owner, ev.Seq, ev.Phase,
		ev.Tool, ev.RequestRef, ev.BeforeCheckpoint, ev.AfterCheckpoint, ev.MutationRef, ev.ErrorCategory, ev.Payload)
	if err != nil {
		return fmt.Errorf("action event: append: %w", err)
	}
	return nil
}

// ListByAction 实现 ActionEventStore。
func (s *PGActionEventStore) ListByAction(ctx context.Context, actionID string) ([]ActionEvent, error) {
	return s.query(ctx, `WHERE action_id=$1 ORDER BY seq`, actionID)
}

// ListBySession 实现 ActionEventStore（taskID 空=会话全部；limit<=0 默认 200）。
func (s *PGActionEventStore) ListBySession(ctx context.Context, sessionID, taskID string, limit int) ([]ActionEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	if taskID == "" {
		return s.query(ctx, `WHERE session_id=$1 ORDER BY id DESC LIMIT $2`, sessionID, limit)
	}
	return s.query(ctx, `WHERE session_id=$1 AND logical_task_id=$2 ORDER BY id DESC LIMIT $3`, sessionID, taskID, limit)
}

func (s *PGActionEventStore) query(ctx context.Context, where string, args ...any) ([]ActionEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("action event store: postgres not configured")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, action_id, session_id, logical_task_id, contract_revision, owner, seq, phase, tool,
       request_ref, before_checkpoint, after_checkpoint, mutation_ref, error_category, payload, created_at
FROM action_events `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("action event: query: %w", err)
	}
	defer rows.Close()
	out := make([]ActionEvent, 0, 16)
	for rows.Next() {
		var ev ActionEvent
		if err := rows.Scan(&ev.ID, &ev.ActionID, &ev.SessionID, &ev.LogicalTaskID, &ev.ContractRevision,
			&ev.Owner, &ev.Seq, &ev.Phase, &ev.Tool, &ev.RequestRef, &ev.BeforeCheckpoint,
			&ev.AfterCheckpoint, &ev.MutationRef, &ev.ErrorCategory, &ev.Payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("action event: scan: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MemoryActionEventStore 进程内实现（测试/无 PG）。
type MemoryActionEventStore struct {
	mu     sync.Mutex
	events []ActionEvent
	seen   map[string]bool
}

// NewMemoryActionEventStore 构造内存 Action 事件存储。
func NewMemoryActionEventStore() *MemoryActionEventStore {
	return &MemoryActionEventStore{seen: map[string]bool{}}
}

// Append 实现 ActionEventStore。
func (s *MemoryActionEventStore) Append(_ context.Context, ev ActionEvent) error {
	if strings.TrimSpace(ev.ActionID) == "" || ev.Seq <= 0 {
		return fmt.Errorf("action event: action_id and seq required")
	}
	key := fmt.Sprintf("%s\x00%d", ev.ActionID, ev.Seq)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[key] {
		return nil
	}
	s.seen[key] = true
	ev.ID = int64(len(s.events) + 1)
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	s.events = append(s.events, ev)
	return nil
}

// ListByAction 实现 ActionEventStore。
func (s *MemoryActionEventStore) ListByAction(_ context.Context, actionID string) ([]ActionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionEvent, 0, 8)
	for _, ev := range s.events {
		if ev.ActionID == actionID {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// ListBySession 实现 ActionEventStore。
func (s *MemoryActionEventStore) ListBySession(_ context.Context, sessionID, taskID string, limit int) ([]ActionEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionEvent, 0, 16)
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := s.events[i]
		if ev.SessionID != sessionID {
			continue
		}
		if taskID != "" && ev.LogicalTaskID != taskID {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}
