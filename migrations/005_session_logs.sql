-- 005_session_logs.sql: 结构化会话日志表，支撑 Agent IO 可视化与 Token 消耗分析。

CREATE TABLE IF NOT EXISTS session_logs (
    id SERIAL PRIMARY KEY,
    session_id TEXT NOT NULL,
    agent TEXT,
    level TEXT NOT NULL DEFAULT 'info',
    phase TEXT,
    message TEXT NOT NULL,
    prompt TEXT,
    response TEXT,
    input_tokens INT DEFAULT 0,
    output_tokens INT DEFAULT 0,
    model TEXT,
    latency_ms INT DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    meta_json JSONB
);

CREATE INDEX IF NOT EXISTS idx_session_logs_session_id ON session_logs(session_id);
CREATE INDEX IF NOT EXISTS idx_session_logs_agent ON session_logs(agent);
CREATE INDEX IF NOT EXISTS idx_session_logs_level ON session_logs(level);
CREATE INDEX IF NOT EXISTS idx_session_logs_created_at ON session_logs(created_at);
