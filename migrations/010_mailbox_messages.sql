-- 010_mailbox_messages.sql: mailbox 持久化（2026-09-28，P0 消息可靠性）
-- 运行时由 store.EnsureMailboxSchema 幂等建表，本文件为文档性副本。
CREATE TABLE IF NOT EXISTS mailbox_messages (
    id          VARCHAR(128) PRIMARY KEY,
    owner       VARCHAR(64) NOT NULL DEFAULT '',
    session_id  VARCHAR(64) NOT NULL,
    from_agent  VARCHAR(128) NOT NULL DEFAULT '',
    to_agent    VARCHAR(128) NOT NULL DEFAULT '',
    type        VARCHAR(16) NOT NULL,
    subject     TEXT,
    body        TEXT,
    payload     JSONB,
    priority    INT NOT NULL DEFAULT 0,
    status      VARCHAR(16) NOT NULL DEFAULT 'unread',  -- unread/read/dead
    reply_to    VARCHAR(128),
    thread_id   VARCHAR(128),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_session
    ON mailbox_messages (session_id, status);
