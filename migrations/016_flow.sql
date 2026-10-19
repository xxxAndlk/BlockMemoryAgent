-- 016_flow.sql 可视化编排（flow）运行表：flow_runs / flow_node_runs。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 flow.NewPGStore 在启动时
-- 执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。
-- flow 定义不落库：真相源 = 工作区 .bma/flows/<id>.json（git 可版本化）。

CREATE TABLE IF NOT EXISTS flow_runs (
  id           TEXT PRIMARY KEY,
  flow_id      TEXT NOT NULL,
  flow_name    TEXT NOT NULL,
  flow_version INTEGER NOT NULL,
  flow_json    TEXT NOT NULL,              -- 启动快照
  inputs       JSONB,                      -- start 节点输入
  status       TEXT NOT NULL,              -- pending|running|waiting_approval|done|failed|aborted
  outputs      JSONB,                      -- answer 汇总输出
  error        TEXT,
  session_id   TEXT,
  workdir      TEXT NOT NULL,              -- flow 定义所在工作区
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS flow_node_runs (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES flow_runs(id),
  node_id     TEXT NOT NULL,
  node_type   TEXT NOT NULL,               -- start|llm|tool|branch|approval|answer
  status      TEXT NOT NULL,               -- pending|running|success|failed|waiting|skipped
  inputs      JSONB,                       -- 解析后的实际入参（prompt/args 模板展开结果）
  outputs     JSONB,
  error       TEXT,
  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_flow_node_runs_run ON flow_node_runs(run_id);
CREATE INDEX IF NOT EXISTS idx_flow_runs_status ON flow_runs(status);
