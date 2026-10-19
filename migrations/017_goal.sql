-- 017_goal.sql 会话指令模式（goal 目标 / loop 循环）：session_directive / goal_reminders。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 goal.NewPGStore 在启动时
-- 执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。
--
-- 语义：goal=会话挂目标自主多轮推进（每轮终答经轻量模型评估，未达成注入"继续推进"
-- 续跑，达成切回普通）；loop=固定间隔重复执行指令（每轮排下一次，到点调度器唤醒）。
-- 跨天等待（如开店审核）由 Agent 用 schedule_followup 工具自排 goal_reminders 回查。

CREATE TABLE IF NOT EXISTS session_directive (
  id                   TEXT PRIMARY KEY,
  session_id           TEXT NOT NULL UNIQUE,
  mode                 TEXT NOT NULL,              -- normal|goal|loop
  objective            TEXT NOT NULL DEFAULT '',  -- 目标/循环指令文本
  completion_criterion TEXT NOT NULL DEFAULT '',  -- goal 完成条件（空=按目标语义判定）
  max_turns            INTEGER NOT NULL DEFAULT 0, -- 预算轮数（0=不限）
  deadline             TIMESTAMPTZ,               -- 预算截止时间（可空）
  loop_interval_sec    INTEGER NOT NULL DEFAULT 0, -- loop 间隔秒（SetMode 按最小间隔收敛）
  loop_until           TEXT NOT NULL DEFAULT '',  -- loop 停止条件（空=手动停）
  status               TEXT NOT NULL,             -- active|complete|failed|stopped
  turn_count           INTEGER NOT NULL DEFAULT 0,
  last_eval            TEXT NOT NULL DEFAULT '',  -- 评估器最近结论
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS goal_reminders (
  id           TEXT PRIMARY KEY,
  session_id   TEXT NOT NULL,
  directive_id TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL,              -- once（Agent 自排回查）|interval（loop 下一轮）
  text         TEXT NOT NULL,              -- 到点注入会话的消息正文
  run_at       TIMESTAMPTZ NOT NULL,
  status       TEXT NOT NULL,              -- pending|fired|cancelled
  created_by   TEXT NOT NULL DEFAULT 'system', -- agent|user|system
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  fired_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_goal_reminders_due ON goal_reminders(status, run_at);
