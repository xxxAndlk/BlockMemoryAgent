-- 018_session_decision_axes.sql 多轴决策账本（2026-10-24 媒体/记忆质量修复 P1）
--
-- 背景：session_decisions 原为"单路线账本"——任何 route 级指令都用 Supersede 把
-- 全部旧 route 一刀切作废。实测后果（会话 #65/#66）：
--   - "重新 AI 生图"撤销了动画路线；
--   - "你说不用开始工作"撤销了整条执行路线；
--   - 视角/审批门/验收标准等不同维度约束被同一把刀误伤。
--
-- 本次新增 axis（决策轴）+ source_message_id（来源消息）：
--   - 同一 axis 内新指令只替换该 axis 的 active 行（SupersedeAxis）；
--   - 跨 axis 同时有效，互不覆盖；
--   - 仍为 append-only：状态只 UPDATE（active/superseded/rejected），不物理删行。
--
-- owner 列已存在于建表 DDL（硬约定：新表/新字段带 owner），本次不新增业务表。
-- 运行时权威 DDL 在 store.EnsureSessionDecisionsSchema（Go 侧幂等补列），
-- 本文件供 make migrate / 审计参照。

ALTER TABLE session_decisions
    ADD COLUMN IF NOT EXISTS axis VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_message_id VARCHAR(128) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_session_decisions_axis
    ON session_decisions (session_id, axis, status, id DESC);

COMMENT ON COLUMN session_decisions.axis IS
    '决策轴：execution_mode/asset_source/animation_method/identity/view/approval_anchor/approval_keyframe/acceptance/delivery；空=无轴';
