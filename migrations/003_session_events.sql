-- 会话事件归档（跨重启事件流恢复）
-- 每个 session 运行时将事件逐条写入此表，重启后 RestoreSessions
-- 可完整回放历史会话的执行过程，不再仅含 goal/summary 骨架。

CREATE TABLE IF NOT EXISTS session_events (
    id            BIGSERIAL PRIMARY KEY,
    session_id    VARCHAR(64) NOT NULL,
    type          VARCHAR(32) NOT NULL DEFAULT '',
    agent         VARCHAR(128) NOT NULL DEFAULT '',
    message       TEXT NOT NULL DEFAULT '',
    kind          VARCHAR(32) NOT NULL DEFAULT '',
    tool          VARCHAR(128) NOT NULL DEFAULT '',
    tool_path     TEXT NOT NULL DEFAULT '',
    tool_output   TEXT NOT NULL DEFAULT '',
    tool_error    TEXT NOT NULL DEFAULT '',
    success       BOOLEAN NOT NULL DEFAULT false,
    timestamp     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    prompt        TEXT NOT NULL DEFAULT '',
    input_tokens  INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    detail_json   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_session_events_session_ts
    ON session_events (session_id, timestamp);
