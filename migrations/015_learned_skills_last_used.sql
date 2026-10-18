-- 015_learned_skills_last_used.sql 经验技能库冷淘汰（2026-10-17）：
-- learned_skills 增 last_used_at 列（load_skill 加载时刷新；从未使用为 NULL，
-- 冷淘汰按 COALESCE(last_used_at, created_at) 判定"最近使用"）。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 store.EnsureLearnedSkillsSchema
-- 在启动时执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。

ALTER TABLE learned_skills ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
