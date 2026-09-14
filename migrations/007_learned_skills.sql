-- 007_learned_skills.sql 偏好与自进化系统（2026-09-02 设计 §7）：
-- learned_skills 自进化技能库（SKILL.md 文件 + PG 元数据 + 向量）+ evolution_log 审计。
-- 幂等：与既有迁移一致，重复执行无副作用。运行时由 store.EnsureLearnedSkillsSchema
-- 在启动时执行同样 DDL（Go 侧为权威，本文件供 make migrate / 审计参照）。

CREATE TABLE IF NOT EXISTS learned_skills (
    name TEXT PRIMARY KEY,             -- 技能唯一名（小写连字符，= 文件名主干）
    title TEXT NOT NULL,               -- 展示标题
    when_to_use TEXT NOT NULL,         -- 适用场景（与 title 一起构成召回向量）
    content_path TEXT NOT NULL,        -- SKILL.md 文件绝对路径
    embedding VECTOR(1024),            -- embed(title + when_to_use)，维度对齐 global_knowledge
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    use_count INT NOT NULL DEFAULT 0,  -- load_skill 加载计数（排序与审计）
    source_session TEXT,               -- 沉淀来源会话
    outcome TEXT,                      -- 来源会话结果：success|failed|mixed
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_learned_skills_embedding ON learned_skills
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);
CREATE INDEX IF NOT EXISTS idx_learned_skills_updated ON learned_skills(updated_at DESC);

CREATE TABLE IF NOT EXISTS evolution_log (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,                -- user_pref|project_lesson|skill_create|skill_update
    target TEXT NOT NULL,              -- 小节名或技能名
    summary TEXT NOT NULL,
    source_session TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_evolution_log_created ON evolution_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evolution_log_kind ON evolution_log(kind);
