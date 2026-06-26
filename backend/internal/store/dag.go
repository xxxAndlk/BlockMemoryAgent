// Package store dag.go 实现特性1 DAG 调度的持久化层。
//
// 复用一张 dag_jobs 表（schema 由 EnsureDAGSchema 自动应用）：
//   - id          text primary key
//   - name        text
//   - cron        text
//   - enabled     boolean
//   - tasks       jsonb  (Task 列表)
//   - created_at  timestamptz
//   - updated_at  timestamptz
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/dag"
)

// EnsureDAGSchema 自动创建 dag_jobs 表（幂等）。
// 参数 db 通常是 PostgresStore.DB() 返回的 *sql.DB。
func EnsureDAGSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dag_jobs (
			id text primary key,
			name text not null,
			cron text not null default '',
			enabled boolean not null default true,
			tasks jsonb not null default '[]',
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		)
	`)
	return err
}

// SaveDAG upsert 一条 DAG 定义。
func (s *PostgresStore) SaveDAG(ctx context.Context, d *dag.DAG) error {
	if d == nil {
		return fmt.Errorf("nil dag")
	}
	tasksJSON, _ := json.Marshal(d.Tasks)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dag_jobs (id, name, cron, enabled, tasks, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			cron = EXCLUDED.cron,
			enabled = EXCLUDED.enabled,
			tasks = EXCLUDED.tasks,
			updated_at = EXCLUDED.updated_at
	`, d.ID, d.Name, d.Cron, d.Enabled, tasksJSON, d.CreatedAt, d.UpdatedAt)
	return err
}

// GetDAG 按 ID 取单条 DAG。
func (s *PostgresStore) GetDAG(ctx context.Context, id string) (*dag.DAG, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, cron, enabled, tasks, created_at, updated_at
		FROM dag_jobs WHERE id=$1
	`, id)
	var d dag.DAG
	var tasksJSON []byte
	if err := row.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tasksJSON) > 0 {
		_ = json.Unmarshal(tasksJSON, &d.Tasks)
	}
	return &d, nil
}

// ListDAGs 列出全部 DAG。
func (s *PostgresStore) ListDAGs(ctx context.Context) ([]*dag.DAG, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron, enabled, tasks, created_at, updated_at
		FROM dag_jobs ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dag.DAG
	for rows.Next() {
		var d dag.DAG
		var tasksJSON []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		if len(tasksJSON) > 0 {
			_ = json.Unmarshal(tasksJSON, &d.Tasks)
		}
		out = append(out, &d)
	}
	return out, nil
}

// DeleteDAG 按 ID 删除 DAG。
func (s *PostgresStore) DeleteDAG(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dag_jobs WHERE id=$1`, id)
	return err
}
