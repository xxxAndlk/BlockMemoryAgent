package store

// workflow.go 工作流量产流水线的建表（migration 014）。
// 表结构见《2026-10-16 工作流量产流水线开发文档》§4.1；owner 列硬约定（多租户预留）；
// 被否 attempt 只标不删（拒绝理由是配方演化的输入）。运行时建表以本函数为权威，
// migrations/014_workflow.sql 为文档性副本（供 make migrate / 审计参照）。

import (
	"context"
	"database/sql"
	"fmt"
)

// EnsureWorkflowSchema 自动创建 workflow_runs / workflow_units 表（幂等）。
func EnsureWorkflowSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS workflow_runs (
  id            TEXT PRIMARY KEY,
  owner         TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  workdir       TEXT NOT NULL,
  recipe_id     TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_yaml   TEXT NOT NULL,
  roster        JSONB NOT NULL,
  status        TEXT NOT NULL,
  pause_reason  TEXT,
  budget_cap    INTEGER NOT NULL,
  spent_estimate INTEGER NOT NULL DEFAULT 0,
  concurrency   INTEGER NOT NULL DEFAULT 2,
  fail_fast     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS workflow_units (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL REFERENCES workflow_runs(id),
  owner         TEXT NOT NULL,
  item_id       TEXT NOT NULL,
  item_name     TEXT,
  stage         TEXT NOT NULL,
  attempt_no    INTEGER NOT NULL DEFAULT 1,
  status        TEXT NOT NULL,
  artifact_path TEXT,
  check_result  JSONB,
  reject_reason TEXT,
  idem_key      TEXT NOT NULL UNIQUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workflow_units_run ON workflow_units(run_id);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_session ON workflow_runs(session_id);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs(status);
`)
	if err != nil {
		return fmt.Errorf("ensure workflow schema: %w", err)
	}
	return nil
}
