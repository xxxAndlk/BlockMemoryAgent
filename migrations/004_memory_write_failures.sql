-- 004_memory_write_failures.sql
--  legacy migration: the step_count column and its idempotent unique index
--  are already created by 001_init.sql. This file now only keeps the
--  memory_write_failures table definition for historical compatibility.
--  P0-2 write-path optimization removed the background worker / dead-letter
--  mechanism, but the table remains to avoid dropping existing data.

-- 死信表：记录后台 worker 重试 3 次后仍失败的写入（已弃用，保留作历史兼容）
CREATE TABLE IF NOT EXISTS memory_write_failures (
    id SERIAL PRIMARY KEY,
    agent_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    step_count INT NOT NULL,
    action TEXT,
    raw_content TEXT,
    error TEXT NOT NULL,
    retry_count INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    resolved_at TIMESTAMPTZ
);

-- 未解决失败索引：便于运维定时扫描待处理死信
CREATE INDEX IF NOT EXISTS idx_memory_write_failures_unresolved
    ON memory_write_failures(created_at) WHERE resolved_at IS NULL;
