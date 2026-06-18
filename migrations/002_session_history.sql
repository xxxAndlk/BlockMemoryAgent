-- 会话历史归档（跨会话记忆）
-- 每个 session 结束时写入 goal/summary/tool_results，供后续会话的 MetaAgent
-- 在 handleInitial 中读取并拼进 system prompt，实现"记得上次做过什么"。

CREATE TABLE IF NOT EXISTS session_history (
    id           BIGSERIAL PRIMARY KEY,
    session_id   VARCHAR(64) NOT NULL,
    goal         TEXT NOT NULL,
    summary      TEXT NOT NULL,
    tool_results JSONB DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_session_history_created_at
    ON session_history (created_at DESC);
