package store

// long_term_memory.go —— 长期记忆条目化生命周期存储（2026-10-24 长期记忆撤回与污染修复）。
//
// 背景（doc/计划_长期记忆撤回与污染修复.md）：用户画像与项目经验以 Markdown 自然语言
// 行存储，"撤回"只能靠会话结束时的 LLM Merge 去猜——会话取消/进程退出/合并失败时
// 旧结论永远留在现行区并持续注入；归档小节又会被容量裁剪，不能兼任永久墓碑。
//
// 本文件把长期记忆变成**有稳定 ID 的条目**：
//   - items：当前状态投影（candidate/active/quarantined/superseded/retracted）；
//   - events：只追加的审计流（撤回/替换/隔离/恢复/导入），历史不可抹除；
//   - 撤回用状态标记实现（append-only 纪律），不物理删行；
//   - 同一 (owner, scope, project_key, memory_key) 只允许一条 active（部分唯一索引）。
//
// 权威视图 = items 中 status=active 的行；Markdown 只作为人类可读投影/迁移来源。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// 记忆条目作用域。
const (
	MemoryScopeUser    = "user"
	MemoryScopeProject = "project"
)

// 记忆条目状态。
const (
	MemoryStatusCandidate   = "candidate"
	MemoryStatusActive      = "active"
	MemoryStatusQuarantined = "quarantined"
	MemoryStatusSuperseded  = "superseded"
	MemoryStatusRetracted   = "retracted"
)

// 记忆条目来源。
const (
	MemoryOriginHuman        = "human"
	MemoryOriginExplicitTool = "explicit_tool"
	MemoryOriginEvolver      = "evolver"
	MemoryOriginLegacyImport = "legacy_import"
)

// 记忆条目的验证状态。
const (
	MemoryVerificationUnverified = "unverified"
	MemoryVerificationVerified   = "verified"
	MemoryVerificationRejected   = "rejected"
)

// 事件类型。
const (
	MemoryEventCreated         = "created"
	MemoryEventActivated       = "activated"
	MemoryEventRetracted       = "retracted"
	MemoryEventSuperseded      = "superseded"
	MemoryEventQuarantined     = "quarantined"
	MemoryEventRestored        = "restored"
	MemoryEventImported        = "imported"
	MemoryEventResurrectionHit = "resurrection_suspect"
	MemoryEventVerificationSet = "verification_set"
)

// 事件操作者。
const (
	MemoryActorUser   = "user"
	MemoryActorSystem = "system"
	MemoryActorAdmin  = "admin"
)

// MemoryItem 一条长期记忆条目。
type MemoryItem struct {
	ID            string    `json:"id"`
	Owner         string    `json:"owner"`
	Scope         string    `json:"scope"`
	ProjectKey    string    `json:"project_key"`
	MemoryKey     string    `json:"memory_key"`
	Content       string    `json:"content"`
	ContentHash   string    `json:"content_hash"`
	Status        string    `json:"status"`
	Origin        string    `json:"origin"`
	Verification  string    `json:"verification"`
	SourceSession string    `json:"source_session"`
	SourceMessage string    `json:"source_message"`
	Revision      int64     `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// MemoryEvent 一条生命周期审计事件（只追加）。
type MemoryEvent struct {
	ID            int64     `json:"id"`
	Owner         string    `json:"owner"`
	MemoryID      string    `json:"memory_id"`
	EventType     string    `json:"event_type"`
	Actor         string    `json:"actor"`
	SourceSession string    `json:"source_session"`
	SourceMessage string    `json:"source_message"`
	Reason        string    `json:"reason"`
	Payload       string    `json:"payload"` // JSON 字符串（前后状态/替换目标/匹配依据）
	CreatedAt     time.Time `json:"created_at"`
}

// MemoryQuery 条目查询条件。Statuses 为空 = 全部状态。
type MemoryQuery struct {
	Owner      string
	Scope      string // 空 = user+project
	ProjectKey string // scope=project 时使用
	Statuses   []string
	MemoryKeys []string // 空 = 不限制
	Limit      int
}

// ErrMemoryItemNotFound 条目不存在（或不属于该 owner）。
var ErrMemoryItemNotFound = errors.New("long-term memory item not found")

// ErrMemoryRevisionConflict 乐观锁冲突：调用方持有的是旧快照。
var ErrMemoryRevisionConflict = errors.New("long-term memory revision conflict")

// MemoryRepo 是长期记忆生命周期存储的读写接口。
// PG 实现（LongTermMemoryStore）与内存实现（MemoryLongTermStore）都满足它；
// agent 侧只依赖接口，便于单测与无库降级。
type MemoryRepo interface {
	InsertItem(ctx context.Context, item MemoryItem) (MemoryItem, error)
	GetItem(ctx context.Context, owner, id string) (*MemoryItem, error)
	ListItems(ctx context.Context, q MemoryQuery) ([]MemoryItem, error)
	// SetStatus 状态迁移（带 revision 乐观锁），返回更新后的条目。
	SetStatus(ctx context.Context, owner, id, status string, expectedRevision int64) (*MemoryItem, error)
	// TransitionStatus 在同一事务中迁移状态并追加对应审计事件。
	TransitionStatus(ctx context.Context, owner, id, status string, expectedRevision int64, ev MemoryEvent) (*MemoryItem, error)
	SetVerification(ctx context.Context, owner, id, verification string, expectedRevision int64) (*MemoryItem, error)
	AppendEvent(ctx context.Context, ev MemoryEvent) error
	ListEvents(ctx context.Context, owner, memoryID string, limit int) ([]MemoryEvent, error)
	// EventCount 单调递增，用作注入视图的 effective_revision。
	EventCount(ctx context.Context, owner string) (int64, error)
}

// NormalizeProjectKey 把工作目录规范化为项目键（跨平台一致、大小写不敏感）。
// 记忆不因会话工作目录字符串差一个斜杠/大小写就串项目（计划 §7.3）。
func NormalizeProjectKey(workdir string) string {
	w := strings.TrimSpace(workdir)
	if w == "" {
		return ""
	}
	w = filepath.ToSlash(filepath.Clean(w))
	// Windows 盘符/路径大小写不敏感：统一小写，避免 "D:/WebData/x" 与 "d:/webdata/x" 串项目。
	w = strings.ToLower(w)
	w = strings.TrimRight(w, "/")
	return w
}

// NewMemoryID 生成稳定条目 ID（M-<纳秒 base36>，可读、单调、无冲突概率）。
func NewMemoryID(now time.Time) string {
	return fmt.Sprintf("M-%d", now.UnixNano())
}

// ---------------------------------------------------------------- PG 实现

// LongTermMemoryStore PG 版长期记忆存储（items/events 两张表）。
type LongTermMemoryStore struct {
	db *sql.DB
}

// NewLongTermMemoryStore 创建 PG 存储。db 为 nil 时各方法返回错误（调用方应改用内存实现）。
func NewLongTermMemoryStore(db *sql.DB) *LongTermMemoryStore {
	return &LongTermMemoryStore{db: db}
}

// DB 暴露底层连接（供 schema 确保逻辑复用）。
func (s *LongTermMemoryStore) DB() *sql.DB { return s.db }

const memoryItemColumns = `id, owner, scope, project_key, memory_key, content, content_hash,
status, origin, verification, source_session, source_message, revision, created_at, updated_at`

func scanMemoryItem(sc interface{ Scan(...any) error }) (MemoryItem, error) {
	var it MemoryItem
	err := sc.Scan(&it.ID, &it.Owner, &it.Scope, &it.ProjectKey, &it.MemoryKey, &it.Content,
		&it.ContentHash, &it.Status, &it.Origin, &it.Verification, &it.SourceSession,
		&it.SourceMessage, &it.Revision, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

// InsertItem 插入条目（ID 为空时自动生成；content_hash/时间戳缺省补齐）。
// status=active 时受 (owner,scope,project_key,memory_key) 部分唯一索引约束。
func (s *LongTermMemoryStore) InsertItem(ctx context.Context, item MemoryItem) (MemoryItem, error) {
	if s == nil || s.db == nil {
		return MemoryItem{}, fmt.Errorf("long-term memory store not wired")
	}
	now := time.Now()
	if item.ID == "" {
		item.ID = NewMemoryID(now)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if item.Status == "" {
		item.Status = MemoryStatusCandidate
	}
	if item.Origin == "" {
		item.Origin = MemoryOriginEvolver
	}
	if item.Verification == "" {
		item.Verification = MemoryVerificationUnverified
	}
	if item.ContentHash == "" {
		item.ContentHash = MemoryContentHash(item.Content)
	}
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.Scope == MemoryScopeUser {
		item.ProjectKey = ""
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO long_term_memory_items (`+memoryItemColumns+`)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		item.ID, item.Owner, item.Scope, item.ProjectKey, item.MemoryKey, item.Content,
		item.ContentHash, item.Status, item.Origin, item.Verification, item.SourceSession,
		item.SourceMessage, item.Revision, item.CreatedAt, item.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "idx_long_term_memory_active_key") ||
			strings.Contains(err.Error(), "duplicate key") {
			return MemoryItem{}, fmt.Errorf("active memory already exists for key %q (scope=%s project=%s): %w",
				item.MemoryKey, item.Scope, item.ProjectKey, err)
		}
		return MemoryItem{}, fmt.Errorf("insert memory item: %w", err)
	}
	return item, nil
}

// GetItem 按 owner+id 取条目（owner 隔离）。
func (s *LongTermMemoryStore) GetItem(ctx context.Context, owner, id string) (*MemoryItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+memoryItemColumns+` FROM long_term_memory_items WHERE owner=$1 AND id=$2`, owner, id)
	it, err := scanMemoryItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMemoryItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get memory item %s: %w", id, err)
	}
	return &it, nil
}

// ListItems 按条件列出条目（按 updated_at 倒序）。
func (s *LongTermMemoryStore) ListItems(ctx context.Context, q MemoryQuery) ([]MemoryItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	add("owner=$%d", q.Owner)
	if q.Scope != "" {
		add("scope=$%d", q.Scope)
	}
	if q.ProjectKey != "" {
		add("project_key=$%d", q.ProjectKey)
	}
	if len(q.Statuses) > 0 {
		args = append(args, pq.Array(q.Statuses))
		where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	if len(q.MemoryKeys) > 0 {
		args = append(args, pq.Array(q.MemoryKeys))
		where = append(where, fmt.Sprintf("memory_key = ANY($%d)", len(args)))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	args = append(args, limit)
	query := `SELECT ` + memoryItemColumns + ` FROM long_term_memory_items WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY updated_at DESC, id DESC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory items: %w", err)
	}
	defer rows.Close()
	var out []MemoryItem
	for rows.Next() {
		it, err := scanMemoryItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan memory item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetStatus 状态迁移（revision 乐观锁：并发会话用旧快照提交会失败，不得覆盖新决定）。
func (s *LongTermMemoryStore) SetStatus(ctx context.Context, owner, id, status string, expectedRevision int64) (*MemoryItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	if !validMemoryStatus(status) {
		return nil, fmt.Errorf("invalid memory status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE long_term_memory_items
SET status=$3, revision=revision+1, updated_at=now()
WHERE owner=$1 AND id=$2 AND revision=$4`, owner, id, status, expectedRevision)
	if err != nil {
		return nil, fmt.Errorf("set memory status %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrMemoryRevisionConflict
	}
	return s.GetItem(ctx, owner, id)
}

// TransitionStatus 状态迁移与审计事件同事务提交，任一步失败都回滚。
func (s *LongTermMemoryStore) TransitionStatus(ctx context.Context, owner, id, status string, expectedRevision int64, ev MemoryEvent) (*MemoryItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	if !validMemoryStatus(status) {
		return nil, fmt.Errorf("invalid memory status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin memory status transition: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
UPDATE long_term_memory_items
SET status=$3, revision=revision+1, updated_at=now()
WHERE owner=$1 AND id=$2 AND revision=$4`, owner, id, status, expectedRevision)
	if err != nil {
		return nil, fmt.Errorf("set memory status %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrMemoryRevisionConflict
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+memoryItemColumns+` FROM long_term_memory_items WHERE owner=$1 AND id=$2`, owner, id)
	updated, err := scanMemoryItem(row)
	if err != nil {
		return nil, fmt.Errorf("read transitioned memory item %s: %w", id, err)
	}
	if ev.Payload == "" {
		ev.Payload = "{}"
	}
	if ev.Actor == "" {
		ev.Actor = MemoryActorSystem
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO long_term_memory_events
  (owner, memory_id, event_type, actor, source_session, source_message, reason, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ev.Owner, nullIfEmpty(ev.MemoryID), ev.EventType, ev.Actor, ev.SourceSession,
		ev.SourceMessage, ev.Reason, ev.Payload); err != nil {
		return nil, fmt.Errorf("append memory transition event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit memory status transition: %w", err)
	}
	return &updated, nil
}

// SetVerification 验证状态迁移（与 status 正交：verified 不能覆盖 retracted）。
func (s *LongTermMemoryStore) SetVerification(ctx context.Context, owner, id, verification string, expectedRevision int64) (*MemoryItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE long_term_memory_items
SET verification=$3, revision=revision+1, updated_at=now()
WHERE owner=$1 AND id=$2 AND revision=$4`, owner, id, verification, expectedRevision)
	if err != nil {
		return nil, fmt.Errorf("set memory verification %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrMemoryRevisionConflict
	}
	return s.GetItem(ctx, owner, id)
}

// AppendEvent 追加审计事件（只追加，不更新不删除）。
func (s *LongTermMemoryStore) AppendEvent(ctx context.Context, ev MemoryEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("long-term memory store not wired")
	}
	if ev.Payload == "" {
		ev.Payload = "{}"
	}
	if ev.Actor == "" {
		ev.Actor = MemoryActorSystem
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO long_term_memory_events
  (owner, memory_id, event_type, actor, source_session, source_message, reason, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ev.Owner, nullIfEmpty(ev.MemoryID), ev.EventType, ev.Actor, ev.SourceSession,
		ev.SourceMessage, ev.Reason, ev.Payload)
	if err != nil {
		return fmt.Errorf("append memory event: %w", err)
	}
	return nil
}

// ListEvents 按条目列出事件（时间正序）。
func (s *LongTermMemoryStore) ListEvents(ctx context.Context, owner, memoryID string, limit int) ([]MemoryEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("long-term memory store not wired")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner, COALESCE(memory_id,''), event_type, actor, source_session, source_message, reason, payload::text, created_at
FROM long_term_memory_events
WHERE owner=$1 AND memory_id=$2
ORDER BY id ASC LIMIT $3`, owner, memoryID, limit)
	if err != nil {
		return nil, fmt.Errorf("list memory events: %w", err)
	}
	defer rows.Close()
	var out []MemoryEvent
	for rows.Next() {
		var ev MemoryEvent
		if err := rows.Scan(&ev.ID, &ev.Owner, &ev.MemoryID, &ev.EventType, &ev.Actor,
			&ev.SourceSession, &ev.SourceMessage, &ev.Reason, &ev.Payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan memory event: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// EventCount 事件总数（effective_revision：撤回/替换/恢复都会让它变化）。
func (s *LongTermMemoryStore) EventCount(ctx context.Context, owner string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("long-term memory store not wired")
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM long_term_memory_events WHERE owner=$1`, owner).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count memory events: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------- 内存实现

// MemoryLongTermStore 内存版长期记忆存储：单测与无 PG 场景降级。
// 语义与 PG 版一致（含 active 唯一键与 revision 乐观锁）。
type MemoryLongTermStore struct {
	mu     sync.Mutex
	items  map[string]MemoryItem // id -> item
	events []MemoryEvent
	seq    int64
}

// NewMemoryLongTermStore 创建内存版存储。
func NewMemoryLongTermStore() *MemoryLongTermStore {
	return &MemoryLongTermStore{items: map[string]MemoryItem{}}
}

// InsertItem 实现 MemoryRepo。
func (m *MemoryLongTermStore) InsertItem(_ context.Context, item MemoryItem) (MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if item.ID == "" {
		m.seq++
		item.ID = fmt.Sprintf("M-%d-%d", now.UnixNano(), m.seq)
	}
	if item.Status == "" {
		item.Status = MemoryStatusCandidate
	}
	if item.Origin == "" {
		item.Origin = MemoryOriginEvolver
	}
	if item.Verification == "" {
		item.Verification = MemoryVerificationUnverified
	}
	if item.ContentHash == "" {
		item.ContentHash = MemoryContentHash(item.Content)
	}
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.Scope == MemoryScopeUser {
		item.ProjectKey = ""
	}
	if item.Status == MemoryStatusActive {
		for _, other := range m.items {
			if other.Owner == item.Owner && other.Scope == item.Scope &&
				other.ProjectKey == item.ProjectKey && other.MemoryKey == item.MemoryKey &&
				other.Status == MemoryStatusActive {
				return MemoryItem{}, fmt.Errorf("active memory already exists for key %q", item.MemoryKey)
			}
		}
	}
	item.CreatedAt = now
	item.UpdatedAt = now
	m.items[item.ID] = item
	return item, nil
}

// GetItem 实现 MemoryRepo。
func (m *MemoryLongTermStore) GetItem(_ context.Context, owner, id string) (*MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok || it.Owner != owner {
		return nil, ErrMemoryItemNotFound
	}
	return &it, nil
}

// ListItems 实现 MemoryRepo（按更新时间倒序）。
func (m *MemoryLongTermStore) ListItems(_ context.Context, q MemoryQuery) ([]MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MemoryItem
	for _, it := range m.items {
		if it.Owner != q.Owner {
			continue
		}
		if q.Scope != "" && it.Scope != q.Scope {
			continue
		}
		if q.ProjectKey != "" && it.ProjectKey != q.ProjectKey {
			continue
		}
		if len(q.Statuses) > 0 && !containsString(q.Statuses, it.Status) {
			continue
		}
		if len(q.MemoryKeys) > 0 && !containsString(q.MemoryKeys, it.MemoryKey) {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// SetStatus 实现 MemoryRepo。
func (m *MemoryLongTermStore) SetStatus(_ context.Context, owner, id, status string, expectedRevision int64) (*MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validMemoryStatus(status) {
		return nil, fmt.Errorf("invalid memory status %q", status)
	}
	it, ok := m.items[id]
	if !ok || it.Owner != owner {
		return nil, ErrMemoryItemNotFound
	}
	if expectedRevision > 0 && it.Revision != expectedRevision {
		return nil, ErrMemoryRevisionConflict
	}
	it.Status = status
	it.Revision++
	it.UpdatedAt = time.Now()
	m.items[id] = it
	return &it, nil
}

// TransitionStatus 实现 MemoryRepo，状态与事件在同一临界区提交。
func (m *MemoryLongTermStore) TransitionStatus(_ context.Context, owner, id, status string, expectedRevision int64, ev MemoryEvent) (*MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validMemoryStatus(status) {
		return nil, fmt.Errorf("invalid memory status %q", status)
	}
	it, ok := m.items[id]
	if !ok || it.Owner != owner {
		return nil, ErrMemoryItemNotFound
	}
	if expectedRevision > 0 && it.Revision != expectedRevision {
		return nil, ErrMemoryRevisionConflict
	}
	it.Status = status
	it.Revision++
	it.UpdatedAt = time.Now()
	m.items[id] = it
	m.seq++
	ev.ID = m.seq
	ev.CreatedAt = time.Now()
	if ev.Payload == "" {
		ev.Payload = "{}"
	}
	if ev.Actor == "" {
		ev.Actor = MemoryActorSystem
	}
	m.events = append(m.events, ev)
	return &it, nil
}

// SetVerification 实现 MemoryRepo。
func (m *MemoryLongTermStore) SetVerification(_ context.Context, owner, id, verification string, expectedRevision int64) (*MemoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok || it.Owner != owner {
		return nil, ErrMemoryItemNotFound
	}
	if expectedRevision > 0 && it.Revision != expectedRevision {
		return nil, ErrMemoryRevisionConflict
	}
	it.Verification = verification
	it.Revision++
	it.UpdatedAt = time.Now()
	m.items[id] = it
	return &it, nil
}

// AppendEvent 实现 MemoryRepo。
func (m *MemoryLongTermStore) AppendEvent(_ context.Context, ev MemoryEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev.ID = m.seq
	ev.CreatedAt = time.Now()
	if ev.Payload == "" {
		ev.Payload = "{}"
	}
	if ev.Actor == "" {
		ev.Actor = MemoryActorSystem
	}
	m.events = append(m.events, ev)
	return nil
}

// ListEvents 实现 MemoryRepo。
func (m *MemoryLongTermStore) ListEvents(_ context.Context, owner, memoryID string, limit int) ([]MemoryEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MemoryEvent
	for _, ev := range m.events {
		if ev.Owner != owner || ev.MemoryID != memoryID {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// EventCount 实现 MemoryRepo。
func (m *MemoryLongTermStore) EventCount(_ context.Context, owner string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, ev := range m.events {
		if ev.Owner == owner {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- 辅助

func validMemoryStatus(s string) bool {
	switch s {
	case MemoryStatusCandidate, MemoryStatusActive, MemoryStatusQuarantined,
		MemoryStatusSuperseded, MemoryStatusRetracted:
		return true
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// nullIfEmpty 空字符串映射为 SQL NULL（memory_id 可空：保存不针对具体条目的系统事件）。
func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
