-- 019_long_term_memory_lifecycle.sql 长期记忆条目化生命周期（2026-10-24）
--
-- 背景（doc/计划_长期记忆撤回与污染修复.md）：
-- 用户画像（config/user_profile.md）与项目经验（.bma/project_preferences.md）长期以
-- Markdown 自然语言行存储，"撤回"只能靠会话结束时的 LLM Merge 去猜：
--   - 会话取消/进程退出/提取失败/Merge 失败时旧结论永远留在现行区；
--   - 归档小节会被容量裁剪（默认 20 条），不能兼任永久墓碑；
--   - 没有条目 ID，无法证明"用户撤回的正是这一条"。
--
-- 本迁移引入两张表：
--   long_term_memory_items  —— 当前状态投影（candidate|active|quarantined|superseded|retracted）
--   long_term_memory_events —— 只追加的审计流（撤回/替换/隔离/恢复/导入/复活嫌疑）
--
-- 语义要点（与 doc/计划 一致）：
--   1. 两张表都带 owner（硬约定），查询必须带 owner 条件；
--   2. events 只追加，不更新不物理删除；items 状态只 UPDATE（append-only 底账纪律）；
--   3. 同一 (owner, scope, project_key, memory_key) 只允许一条 active（部分唯一索引）；
--   4. owner+scope=project 时按规范化 project_key 隔离，跨项目不串；
--   5. 撤回墓碑不因 Markdown 归档容量淘汰——权威在 events/items，不在文件。
--
-- 运行时权威 DDL 在 store.EnsureLongTermMemorySchema（Go 侧幂等），
-- 本文件供 make migrate / 审计参照。

CREATE TABLE IF NOT EXISTS long_term_memory_items (
    id              TEXT PRIMARY KEY,
    owner           VARCHAR(64) NOT NULL DEFAULT '',
    scope           VARCHAR(16) NOT NULL,
    project_key     TEXT NOT NULL DEFAULT '',
    memory_key      TEXT NOT NULL,
    content         TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'candidate',
    origin          VARCHAR(24) NOT NULL DEFAULT 'evolver',
    verification    VARCHAR(16) NOT NULL DEFAULT 'unverified',
    source_session  TEXT NOT NULL DEFAULT '',
    source_message  TEXT NOT NULL DEFAULT '',
    revision        BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (scope IN ('user', 'project')),
    CHECK (status IN ('candidate', 'active', 'quarantined', 'superseded', 'retracted')),
    CHECK (verification IN ('unverified', 'verified', 'rejected')),
    CHECK (scope = 'project' OR project_key = '')
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_long_term_memory_active_key
    ON long_term_memory_items(owner, scope, project_key, memory_key)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_long_term_memory_effective
    ON long_term_memory_items(owner, scope, project_key, status, memory_key);

CREATE INDEX IF NOT EXISTS idx_long_term_memory_hash
    ON long_term_memory_items(owner, scope, project_key, content_hash);

CREATE TABLE IF NOT EXISTS long_term_memory_events (
    id              BIGSERIAL PRIMARY KEY,
    owner           VARCHAR(64) NOT NULL DEFAULT '',
    memory_id       TEXT,
    event_type      VARCHAR(24) NOT NULL,
    actor           VARCHAR(16) NOT NULL,
    source_session  TEXT NOT NULL DEFAULT '',
    source_message  TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (actor IN ('user', 'system', 'admin'))
);

CREATE INDEX IF NOT EXISTS idx_long_term_memory_events_item
    ON long_term_memory_events(owner, memory_id, id);

COMMENT ON TABLE long_term_memory_items IS
    '长期记忆条目当前状态投影：status=active 才进入注入视图；撤回/替换/隔离只改状态，不删行';
COMMENT ON TABLE long_term_memory_events IS
    '长期记忆生命周期事件（只追加）：撤回/替换/隔离/恢复/导入/复活嫌疑，含来源与原因';
