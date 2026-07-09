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
	"log"

	"github.com/blockmemory/agent/backend/pkg/types"
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

// errDAGStoreNotReady 在 Postgres 未初始化时返回，
// 让上层明确报错（记日志、跳过 DAG 调度），而不是 nil 指针 panic。
var errDAGStoreNotReady = fmt.Errorf("dag store not ready: postgres unavailable")

// checkReady 校验 PostgresStore 已初始化，未初始化时返回错误而非 panic。
// nil 接收者调用方法是合法的（Go 允许 typed-nil 调方法），真正的 panic 来自解引用 s.db。
func (s *PostgresStore) checkReady() error {
	if s == nil || s.db == nil {
		return errDAGStoreNotReady
	}
	return nil
}

// SaveDAG upsert 一条 DAG 定义。
func (s *PostgresStore) SaveDAG(ctx context.Context, d *types.DAG) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("nil dag")
	}
	tasksJSON, err := json.Marshal(d.Tasks)
	if err != nil {
		return fmt.Errorf("marshal dag tasks: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
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
func (s *PostgresStore) GetDAG(ctx context.Context, id string) (*types.DAG, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, cron, enabled, tasks, created_at, updated_at
		FROM dag_jobs WHERE id=$1
	`, id)
	var d types.DAG
	var tasksJSON []byte
	if err := row.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tasksJSON) > 0 {
		if err := json.Unmarshal(tasksJSON, &d.Tasks); err != nil {
			log.Printf("[store] unmarshal dag_jobs.tasks failed: id=%s err=%v", d.ID, err)
		}
	}
	return &d, nil
}

// ListDAGs 列出全部 DAG。
func (s *PostgresStore) ListDAGs(ctx context.Context) ([]*types.DAG, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron, enabled, tasks, created_at, updated_at
		FROM dag_jobs ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*types.DAG
	for rows.Next() {
		var d types.DAG
		var tasksJSON []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		if len(tasksJSON) > 0 {
			if err := json.Unmarshal(tasksJSON, &d.Tasks); err != nil {
				log.Printf("[store] unmarshal dag_jobs.tasks failed: id=%s err=%v", d.ID, err)
			}
		}
		out = append(out, &d)
	}
	return out, nil
}

// DeleteDAG 按 ID 删除 DAG。
func (s *PostgresStore) DeleteDAG(ctx context.Context, id string) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM dag_jobs WHERE id=$1`, id)
	return err
}
