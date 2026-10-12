package store

// role_evolution.go 角色自进化准则存储（migration 013）：
// role_evolution_prompts 存每个角色的自进化提示词（≤300字、AI 经 update_evolution_prompt
// 工具自维护、每轮注入系统提示词）。纯 SQL 表，不涉及向量/embedding。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RoleEvolutionPrompt role_evolution_prompts 表记录。
type RoleEvolutionPrompt struct {
	RoleID        string    `json:"role_id"`
	Content       string    `json:"content"`
	SourceSession string    `json:"source_session,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// EnsureRoleEvolutionSchema 幂等建 role_evolution_prompts 表（migration 013）。
func EnsureRoleEvolutionSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS role_evolution_prompts (
    role_id TEXT PRIMARY KEY,
    content TEXT NOT NULL DEFAULT '',
    source_session TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`)
	if err != nil {
		return fmt.Errorf("ensure role_evolution_prompts schema: %w", err)
	}
	return nil
}

// RoleEvolutionStore role_evolution_prompts 表存储。
type RoleEvolutionStore struct {
	db *sql.DB
}

// NewRoleEvolutionStore 创建角色自进化准则存储。
func NewRoleEvolutionStore(db *sql.DB) *RoleEvolutionStore {
	return &RoleEvolutionStore{db: db}
}

// Get 读取角色准则；不存在返回 (nil, nil)（不是错误）。
func (s *RoleEvolutionStore) Get(ctx context.Context, roleID string) (*RoleEvolutionPrompt, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `
SELECT role_id, content, source_session, updated_at
FROM role_evolution_prompts WHERE role_id = $1`, roleID)
	rec, err := scanRoleEvolutionPrompt(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get role evolution prompt %s: %w", roleID, err)
	}
	return rec, nil
}

// Upsert 全量替换角色准则（INSERT ... ON CONFLICT (role_id) DO UPDATE，updated_at=now()）。
func (s *RoleEvolutionStore) Upsert(ctx context.Context, rec *RoleEvolutionPrompt) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO role_evolution_prompts (role_id, content, source_session, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (role_id) DO UPDATE SET
    content = EXCLUDED.content,
    source_session = EXCLUDED.source_session,
    updated_at = now()`,
		rec.RoleID, rec.Content, rec.SourceSession)
	if err != nil {
		return fmt.Errorf("upsert role evolution prompt %s: %w", rec.RoleID, err)
	}
	return nil
}

// List 全量列出（按 role_id 排序，稳定输出）。
func (s *RoleEvolutionStore) List(ctx context.Context) ([]RoleEvolutionPrompt, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT role_id, content, source_session, updated_at
FROM role_evolution_prompts ORDER BY role_id`)
	if err != nil {
		return nil, fmt.Errorf("list role evolution prompts: %w", err)
	}
	defer rows.Close()
	var out []RoleEvolutionPrompt
	for rows.Next() {
		rec, err := scanRoleEvolutionPrompt(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan role evolution prompt: %w", err)
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// scanRoleEvolutionPrompt 行扫描：source_session 可空，NULL 归一为空串。
func scanRoleEvolutionPrompt(scan func(dest ...any) error) (*RoleEvolutionPrompt, error) {
	var rec RoleEvolutionPrompt
	var sourceSession sql.NullString
	if err := scan(&rec.RoleID, &rec.Content, &sourceSession, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.SourceSession = sourceSession.String
	return &rec, nil
}
