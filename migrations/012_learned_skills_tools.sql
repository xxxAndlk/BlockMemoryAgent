-- 012_learned_skills_tools.sql 经验技能"技能携带工具"（TODO 25 阶段 C1，2026-10-01）：
-- learned_skills 增 has_tools 列（列表页"⚙ 带工具"角标；true = 技能目录含 scripts/）。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 store.EnsureLearnedSkillsSchema
-- 在启动时执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。

ALTER TABLE learned_skills ADD COLUMN IF NOT EXISTS has_tools BOOLEAN NOT NULL DEFAULT FALSE;
