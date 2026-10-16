-- 014_workflow.sql 工作流量产流水线（配方/名册/运行三元组，表见 2026-10-16 工作流量产流水线开发文档 §4.1）。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 store.EnsureWorkflowSchema
-- 在启动时执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。

CREATE TABLE IF NOT EXISTS workflow_runs (
  id            TEXT PRIMARY KEY,
  owner         TEXT NOT NULL,
  session_id    TEXT NOT NULL,             -- 发起会话（工作区/事件展示锚点）
  workdir       TEXT NOT NULL,             -- 配方与产物所在工作区
  recipe_id     TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_yaml   TEXT NOT NULL,             -- 启动快照
  roster        JSONB NOT NULL,            -- [{id,name,element,family,stages:[...]}]
  status        TEXT NOT NULL,             -- pending|running|paused|done|failed|aborted
  pause_reason  TEXT,                      -- budget_exceeded | user | circuit_breaker
  budget_cap    INTEGER NOT NULL,          -- 轻量调用次数上限
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
  status        TEXT NOT NULL,             -- pending|running|done|needs_review|failed|skipped
  artifact_path TEXT,
  check_result  JSONB,                     -- {program:{...}, judge:{items:[...]}, drift:{...}}
  reject_reason TEXT,
  idem_key      TEXT NOT NULL UNIQUE,      -- run_id+item_id+stage+attempt_no
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_workflow_units_run ON workflow_units(run_id);
