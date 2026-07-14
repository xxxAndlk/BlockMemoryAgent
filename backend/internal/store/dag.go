// Package store dag.go 实现特性1 DAG 调度的持久化层。
//
// 复用一张 dag_jobs 表（schema 由 EnsureDAGSchema 自动应用）：
//   - id          text primary key  // 主键
//   - name        text              // 名称
//   - cron        text              // cron 表达式
//   - enabled     boolean           // 是否启用
//   - tasks       jsonb             // Task 列表
//   - created_at  timestamptz       // 创建时间
//   - updated_at  timestamptz       // 更新时间
package store

import (
	"context"       // 上下文，控制数据库操作生命周期
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // Task 列表 JSON 序列化
	"fmt"           // 格式化错误
	"log"           // 反序列化失败日志

	"github.com/blockmemory/agent/backend/pkg/types" // DAG 领域模型
)

// EnsureDAGSchema 自动创建 dag_jobs 表（幂等）。
// 参数:
//   - ctx: 超时与取消控制。
//   - db:  通常是 PostgresStore.DB() 返回的 *sql.DB。
//
// 返回: 建表错误。
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
// 参数:
//   - s: PostgresStore 指针，可能为 nil 或尚未初始化 db。
//
// 返回: nil 表示可用；errDAGStoreNotReady 表示不可用。
func (s *PostgresStore) checkReady() error {
	if s == nil || s.db == nil {
		return errDAGStoreNotReady
	}
	return nil
}

// SaveDAG upsert 一条 DAG 定义。
// 参数:
//   - ctx: 请求上下文。
//   - d:   待保存的 DAG 指针，nil 会返回错误。
//
// 返回: SQL 执行或序列化错误。
func (s *PostgresStore) SaveDAG(ctx context.Context, d *types.DAG) error {
	// 先检查存储是否就绪，避免 nil 解引用 panic
	if err := s.checkReady(); err != nil {
		return err
	}
	// 防御 nil DAG
	if d == nil {
		return fmt.Errorf("nil dag")
	}
	// 将 tasks 切片序列化为 JSONB 可接受的文本
	tasksJSON, err := json.Marshal(d.Tasks)
	if err != nil {
		return fmt.Errorf("marshal dag tasks: %w", err)
	}
	// 使用 ON CONFLICT 实现 upsert，保留 created_at 不变
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
// 参数:
//   - ctx: 请求上下文。
//   - id:  DAG 主键。
//
// 返回: 命中返回 *types.DAG；未命中返回 SQL 错误；反序列化失败记录日志但不阻断。
func (s *PostgresStore) GetDAG(ctx context.Context, id string) (*types.DAG, error) {
	// 先检查存储是否就绪
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	// 查询单条记录
	row := s.db.QueryRowContext(ctx, `
			SELECT id, name, cron, enabled, tasks, created_at, updated_at
			FROM dag_jobs WHERE id=$1
		`, id)
	var d types.DAG
	var tasksJSON []byte
	if err := row.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	// 反序列化 tasks JSONB
	if len(tasksJSON) > 0 {
		if err := json.Unmarshal(tasksJSON, &d.Tasks); err != nil {
			log.Printf("[store] unmarshal dag_jobs.tasks failed: id=%s err=%v", d.ID, err)
		}
	}
	return &d, nil
}

// ListDAGs 列出全部 DAG。
// 参数:
//   - ctx: 请求上下文。
//
// 返回: DAG 切片与 SQL 错误。
func (s *PostgresStore) ListDAGs(ctx context.Context) ([]*types.DAG, error) {
	// 先检查存储是否就绪
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	// 按更新时间倒序列出全部 DAG
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, name, cron, enabled, tasks, created_at, updated_at
			FROM dag_jobs ORDER BY updated_at DESC
		`)
	if err != nil {
		return nil, err
	}
	// 确保结果集关闭
	defer rows.Close()
	var out []*types.DAG
	for rows.Next() {
		var d types.DAG
		var tasksJSON []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.Cron, &d.Enabled, &tasksJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		// 逐行反序列化 tasks
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
// 参数:
//   - ctx: 请求上下文。
//   - id:  待删除 DAG 主键。
//
// 返回: SQL 执行错误。
func (s *PostgresStore) DeleteDAG(ctx context.Context, id string) error {
	// 先检查存储是否就绪
	if err := s.checkReady(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM dag_jobs WHERE id=$1`, id)
	return err
}
