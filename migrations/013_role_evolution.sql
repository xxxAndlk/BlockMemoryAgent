-- 013_role_evolution.sql 角色自进化提示词（≤300字、AI 经 update_evolution_prompt 工具自维护、每轮注入系统提示词）。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 store.EnsureRoleEvolutionSchema
-- 在启动时执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。

CREATE TABLE IF NOT EXISTS role_evolution_prompts (
    role_id TEXT PRIMARY KEY,
    content TEXT NOT NULL DEFAULT '',
    source_session TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
