package store

// learned_skills.go 自进化技能库 + 进化日志存储（2026-09-02 设计 §7，migration 007）。
//
// learned_skills：SKILL.md 文件（config/skills_learned/）为正文真相源，本表存元数据 +
// 召回向量（embed(title + when_to_use)）+ enabled/use_count。文件与 PG 一致性由
// 启动重扫修复（设计 §9：文件在 PG 无记录则补注册，PG 有文件无则标记禁用）。
//
// evolution_log：全部自进化沉淀的审计流水（user_pref/project_lesson/skill_create/skill_update）。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LearnedSkill learned_skills 表记录。
type LearnedSkill struct {
	Name          string    `json:"name"`
	Title         string    `json:"title"`
	WhenToUse     string    `json:"when_to_use"`
	ContentPath   string    `json:"content_path"`
	Embedding     []float32 `json:"-"`
	Enabled       bool      `json:"enabled"`
	UseCount      int       `json:"use_count"`
	HasTools      bool      `json:"has_tools"` // 带配套脚本（C1：技能目录含 scripts/，列表页角标）
	SourceSession string    `json:"source_session"`
	Outcome       string    `json:"outcome"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Score 召回相似度（仅 SearchSkills 填充，落库时忽略）。
	Score float64 `json:"score,omitempty"`
}

// EvolutionLogEntry evolution_log 表记录。
type EvolutionLogEntry struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"` // user_pref|project_lesson|skill_create|skill_update
	Target        string    `json:"target"`
	Summary       string    `json:"summary"`
	SourceSession string    `json:"source_session"`
	CreatedAt     time.Time `json:"created_at"`
}

// EnsureLearnedSkillsSchema 幂等建 learned_skills / evolution_log 表（migration 007）。
func EnsureLearnedSkillsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS learned_skills (
    name TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    when_to_use TEXT NOT NULL,
    content_path TEXT NOT NULL,
    embedding VECTOR(1024),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    use_count INT NOT NULL DEFAULT 0,
    has_tools BOOLEAN NOT NULL DEFAULT FALSE,
    source_session TEXT,
    outcome TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 老库补 has_tools 列（012_learned_skills_tools.sql；CREATE TABLE IF NOT EXISTS 对存量表是空操作）。
ALTER TABLE learned_skills ADD COLUMN IF NOT EXISTS has_tools BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_learned_skills_embedding ON learned_skills
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);
CREATE INDEX IF NOT EXISTS idx_learned_skills_updated ON learned_skills(updated_at DESC);

CREATE TABLE IF NOT EXISTS evolution_log (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,
    target TEXT NOT NULL,
    summary TEXT NOT NULL,
    source_session TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_evolution_log_created ON evolution_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evolution_log_kind ON evolution_log(kind);
`)
	return err
}

// LearnedSkillStore learned_skills 表存储。
type LearnedSkillStore struct {
	db  *sql.DB
	pg  *PostgresStore
	log Logger
}

// NewLearnedSkillStore 创建技能库存储。
func NewLearnedSkillStore(db *sql.DB, pg *PostgresStore) *LearnedSkillStore {
	return &LearnedSkillStore{db: db, pg: pg}
}

// SetLogger 注入结构化日志器。
func (s *LearnedSkillStore) SetLogger(l Logger) { s.log = l }

// Upsert 新建或同名更新技能（设计 §6.4 create-or-update）：
// ON CONFLICT(name) 刷新 title/when_to_use/embedding/outcome/updated_at，
// use_count 与 enabled 保留既有值（更新不重置使用统计）。
func (s *LearnedSkillStore) Upsert(ctx context.Context, rec *LearnedSkill) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 空向量写 NULL（列可空，召回侧已过滤 NULL）；pgVector 空串 "[]" 不被 pgvector 接受。
	var emb any
	if len(rec.Embedding) > 0 {
		emb = pgVector(rec.Embedding)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO learned_skills (name, title, when_to_use, content_path, embedding, enabled, use_count, has_tools, source_session, outcome, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), now())
ON CONFLICT (name) DO UPDATE SET
    title = EXCLUDED.title,
    when_to_use = EXCLUDED.when_to_use,
    content_path = EXCLUDED.content_path,
    embedding = EXCLUDED.embedding,
    has_tools = EXCLUDED.has_tools,
    outcome = EXCLUDED.outcome,
    updated_at = now()`,
		rec.Name, rec.Title, rec.WhenToUse, rec.ContentPath, emb,
		rec.Enabled, rec.UseCount, rec.HasTools, rec.SourceSession, rec.Outcome)
	return err
}

// Get 按名取单条技能。
func (s *LearnedSkillStore) Get(ctx context.Context, name string) (*LearnedSkill, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `
SELECT name, title, when_to_use, content_path, enabled, use_count, has_tools, source_session, outcome, created_at, updated_at
FROM learned_skills WHERE name = $1`, name)
	rec, err := scanLearnedSkill(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

// List 全量列表（enabledOnly=false 含禁用项，供管理 API）。
func (s *LearnedSkillStore) List(ctx context.Context, enabledOnly bool) ([]*LearnedSkill, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	q := `SELECT name, title, when_to_use, content_path, enabled, use_count, has_tools, source_session, outcome, created_at, updated_at
FROM learned_skills`
	if enabledOnly {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LearnedSkill
	for rows.Next() {
		rec, err := scanLearnedSkill(rows.Scan)
		if err != nil {
			if s.log != nil {
				s.log.Error(ctx, "learned_skills scan row failed", err)
			}
			continue
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SearchSkills 向量召回 enabled 技能：cosine 距离 <= maxDistance，按距离升序取 top limit。
// embedding 为空或查询失败返回空列表（召回 fail-open，不阻塞派发）。
func (s *LearnedSkillStore) SearchSkills(ctx context.Context, embedding []float32, limit int, maxDistance float64) ([]*LearnedSkill, error) {
	if len(embedding) == 0 || limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT name, title, when_to_use, content_path, enabled, use_count, has_tools, source_session, outcome, created_at, updated_at,
       1 - (embedding <=> $1) AS score
FROM learned_skills
WHERE enabled AND embedding IS NOT NULL AND embedding <=> $1 <= $2
ORDER BY embedding <=> $1
LIMIT $3`, pgVector(embedding), maxDistance, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LearnedSkill
	for rows.Next() {
		var rec LearnedSkill
		if err := rows.Scan(&rec.Name, &rec.Title, &rec.WhenToUse, &rec.ContentPath, &rec.Enabled,
			&rec.UseCount, &rec.HasTools, &rec.SourceSession, &rec.Outcome, &rec.CreatedAt, &rec.UpdatedAt, &rec.Score); err != nil {
			continue
		}
		out = append(out, &rec)
	}
	return out, rows.Err()
}

// CountEnabled 统计 enabled 技能数（技能库上限闸门用；写门在 persistOne 建新技能前调用）。
func (s *LearnedSkillStore) CountEnabled(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM learned_skills WHERE enabled`).Scan(&n)
	return n, err
}

// TopEnabled 返回 enabled 技能按价值排序的前 limit 条（use_count 降序，同分 updated_at 降序）。
// 供 meta 系统提示【可用技能】块收敛：只列常用 top-N，其余经 list_skills(query) 按需检索。
func (s *LearnedSkillStore) TopEnabled(ctx context.Context, limit int) ([]*LearnedSkill, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT name, title, when_to_use, content_path, enabled, use_count, has_tools, source_session, outcome, created_at, updated_at
FROM learned_skills WHERE enabled
ORDER BY use_count DESC, updated_at DESC
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LearnedSkill
	for rows.Next() {
		rec, err := scanLearnedSkill(rows.Scan)
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetEnabled 启用/禁用技能（禁用即从向量预筛与 list_skills 过滤，设计 §9）。
func (s *LearnedSkillStore) SetEnabled(ctx context.Context, name string, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE learned_skills SET enabled = $2, updated_at = now() WHERE name = $1`, name, enabled)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IncrementUseCount load_skill 加载计数 +1（设计 §6.5）。
func (s *LearnedSkillStore) IncrementUseCount(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE learned_skills SET use_count = use_count + 1, updated_at = now() WHERE name = $1`, name)
	return err
}

// UpdateMeta 手动编辑（HTTP PUT）：刷新 title/when_to_use/重嵌入向量/has_tools。
func (s *LearnedSkillStore) UpdateMeta(ctx context.Context, name, title, whenToUse string, embedding []float32, hasTools bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var emb any
	if len(embedding) > 0 {
		emb = pgVector(embedding)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE learned_skills SET title = $2, when_to_use = $3, embedding = $4, has_tools = $5, updated_at = now()
WHERE name = $1`, name, title, whenToUse, emb, hasTools)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Embed 用共享 embedder 生成文本向量（pseudo 兜底）。
func (s *LearnedSkillStore) Embed(ctx context.Context, text string) ([]float32, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("learned skill store not wired to postgres store")
	}
	return s.pg.Embed(ctx, text)
}

func scanLearnedSkill(scan func(dest ...any) error) (*LearnedSkill, error) {
	var rec LearnedSkill
	var sourceSession, outcome sql.NullString
	if err := scan(&rec.Name, &rec.Title, &rec.WhenToUse, &rec.ContentPath, &rec.Enabled,
		&rec.UseCount, &rec.HasTools, &sourceSession, &outcome, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.SourceSession = sourceSession.String
	rec.Outcome = outcome.String
	return &rec, nil
}

// AppendEvolutionLog 写一条进化审计流水。
func (s *LearnedSkillStore) AppendEvolutionLog(ctx context.Context, kind, target, summary, sourceSession string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO evolution_log (kind, target, summary, source_session, created_at)
VALUES ($1, $2, $3, $4, now())`, kind, target, summary, sourceSession)
	return err
}

// ListEvolutionLog 进化日志倒序列表（limit<=0 取最近 200 条）。
func (s *LearnedSkillStore) ListEvolutionLog(ctx context.Context, limit int) ([]*EvolutionLogEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT id, kind, target, summary, source_session, created_at
FROM evolution_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EvolutionLogEntry
	for rows.Next() {
		var e EvolutionLogEntry
		var sourceSession sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.Target, &e.Summary, &sourceSession, &e.CreatedAt); err != nil {
			continue
		}
		e.SourceSession = sourceSession.String
		out = append(out, &e)
	}
	return out, rows.Err()
}
