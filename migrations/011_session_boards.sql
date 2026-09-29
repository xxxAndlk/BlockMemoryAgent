-- 011_session_boards.sql: 会话任务看板持久化（2026-09-28，P1 资源治理）
-- 运行时由 store.EnsureBoardSchema 幂等建表，本文件为文档性副本。
CREATE TABLE IF NOT EXISTS session_boards (
    owner      VARCHAR(64) NOT NULL DEFAULT '',
    session_id VARCHAR(64) PRIMARY KEY,
    goal       TEXT,
    snapshot   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
