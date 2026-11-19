-- 020_logical_tasks.sql 逻辑任务合同与 Action 事件底账（Harness 根治 WP1/WP2，2026-11-18）
--
-- 背景（doc/计划_Harness合同事务与语义编辑根治.md §3.5）：
-- 长会话里 Goal 只是最初目标、决策账本只是"用户说过什么"的流水，二者都无法回答
-- "当前逻辑任务是什么、本轮允许哪些资源、哪些历史约束仍然活跃、实际改了什么"。
-- 事故会话（session-1794974762351783800-1d9520d6-1）第 15 轮把两个页面的共享标签
-- 列宽一起改掉、第 21 轮"修省略"顺手改了 30px/60px 间距，就是因为没有一个共同的
-- 一次执行事实。
--
-- 三张表：
--   logical_tasks          —— 当前投影（每会话每任务一行；状态只 UPDATE，不物理删）
--   logical_task_events    —— append-only 底账（(session_id, idempotency_key) 唯一）
--   action_events          —— append-only Action 阶段事件（(action_id, seq) 唯一）
--
-- 语义要点（与计划一致）：
--   1. 三张表都带 owner（硬约定：新建表/新字段统一带 owner），查询必带 owner 条件；
--   2. 事件只追加；投影可重建（RebuildProjection），压缩/重规划/回滚不删行；
--   3. 会话显式删除时纳入既有 7 表级联清单（store.sessionScopedTables）；
--   4. 旧会话不回填伪造的 accepted/verified —— legacy fallback 由调用层显式处理。
--
-- 运行时权威 DDL 在 store.EnsureLogicalTaskSchema（Go 侧幂等补列），
-- 本文件供 make migrate / 审计参照。

CREATE TABLE IF NOT EXISTS logical_tasks (
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    revision          BIGINT NOT NULL DEFAULT 1,
    parent_revision   BIGINT NOT NULL DEFAULT 0,
    status            VARCHAR(32) NOT NULL DEFAULT 'active',
    goal              TEXT NOT NULL DEFAULT '',
    content_hash      VARCHAR(80) NOT NULL DEFAULT '',
    base_checkpoint   TEXT NOT NULL DEFAULT '',
    contract          JSONB NOT NULL DEFAULT '{}'::jsonb,
    origin_event_id   BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, logical_task_id),
    CHECK (status IN ('draft','active','blocked','awaiting_clarification','awaiting_verification',
                      'verified','delivered-unverified','failed','cancelled','accepted','superseded'))
);

CREATE INDEX IF NOT EXISTS idx_logical_tasks_session_status
    ON logical_tasks (session_id, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_logical_tasks_owner
    ON logical_tasks (owner, session_id);

CREATE TABLE IF NOT EXISTS logical_task_events (
    event_id          BIGSERIAL PRIMARY KEY,
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    revision          BIGINT NOT NULL DEFAULT 1,
    kind              VARCHAR(32) NOT NULL,
    payload           TEXT NOT NULL DEFAULT '',
    origin_event_id   BIGINT NOT NULL DEFAULT 0,
    idempotency_key   VARCHAR(200) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_logical_task_events_task
    ON logical_task_events (session_id, logical_task_id, event_id);

CREATE TABLE IF NOT EXISTS action_events (
    id                BIGSERIAL PRIMARY KEY,
    action_id         VARCHAR(80) NOT NULL,
    session_id        VARCHAR(64) NOT NULL,
    logical_task_id   VARCHAR(160) NOT NULL DEFAULT '',
    contract_revision BIGINT NOT NULL DEFAULT 0,
    owner             VARCHAR(64) NOT NULL DEFAULT '',
    seq               INTEGER NOT NULL,
    phase             VARCHAR(40) NOT NULL,
    tool              VARCHAR(80) NOT NULL DEFAULT '',
    request_ref       TEXT NOT NULL DEFAULT '',
    before_checkpoint TEXT NOT NULL DEFAULT '',
    after_checkpoint  TEXT NOT NULL DEFAULT '',
    mutation_ref      TEXT NOT NULL DEFAULT '',
    error_category    VARCHAR(40) NOT NULL DEFAULT '',
    payload           TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (action_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_action_events_session
    ON action_events (session_id, id);
CREATE INDEX IF NOT EXISTS idx_action_events_task
    ON action_events (session_id, logical_task_id, id);
