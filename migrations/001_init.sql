-- 多Agent记忆管理系统 - 初始Schema

-- 0. pgvector 扩展（必须在所有使用 VECTOR 类型的表之前创建）
CREATE EXTENSION IF NOT EXISTS vector;

-- 1. 私有记忆层
CREATE TABLE IF NOT EXISTS agent_private_memory (
    id BIGSERIAL PRIMARY KEY,
    agent_id VARCHAR(64) NOT NULL,
    topic_id VARCHAR(64) NOT NULL,
    episode JSONB NOT NULL,
    snapshot_ref VARCHAR(128),
    compression_level INT DEFAULT 0,
    importance_score FLOAT DEFAULT 0,
    step_count INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_apm_agent_topic ON agent_private_memory(agent_id, topic_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_apm_importance ON agent_private_memory(importance_score DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_apm_episode_gin ON agent_private_memory USING GIN (episode);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_private_memory_idempotent ON agent_private_memory(agent_id, topic_id, step_count);

-- 2. Agent 快照表
CREATE TABLE IF NOT EXISTS agent_snapshots (
    id BIGSERIAL PRIMARY KEY,
    agent_id VARCHAR(64) NOT NULL,
    topic_id VARCHAR(64) NOT NULL,
    snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT uniq_agent_topic_snapshot UNIQUE (agent_id, topic_id)
);

CREATE INDEX IF NOT EXISTS idx_snap_agent_topic ON agent_snapshots(agent_id, topic_id);

-- 3. 话题元数据
CREATE TABLE IF NOT EXISTS topics (
    id VARCHAR(64) PRIMARY KEY,
    goal TEXT NOT NULL,
    status VARCHAR(32) DEFAULT 'active',
    constraints JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);

-- 4. 全局知识库
CREATE TABLE IF NOT EXISTS global_knowledge (
    id BIGSERIAL PRIMARY KEY,
    knowledge_type VARCHAR(32) NOT NULL,
    topic_id VARCHAR(64),
    content TEXT NOT NULL,
    embedding VECTOR(768),
    meta JSONB DEFAULT '{}',
    access_count INT DEFAULT 0,
    last_accessed TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    archived BOOL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_gk_type ON global_knowledge(knowledge_type);
CREATE INDEX IF NOT EXISTS idx_gk_topic ON global_knowledge(topic_id);
CREATE INDEX IF NOT EXISTS idx_gk_access ON global_knowledge(last_accessed, access_count);
CREATE INDEX IF NOT EXISTS idx_gk_archived ON global_knowledge(archived);
CREATE INDEX IF NOT EXISTS idx_global_knowledge_domain ON global_knowledge USING btree ((meta->>'domain'));

-- 5. Agent 能力注册表
CREATE TABLE IF NOT EXISTS agent_registry (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description TEXT,
    module_id VARCHAR(64) NOT NULL,
    keywords JSONB DEFAULT '[]',
    dependencies JSONB DEFAULT '[]',
    capabilities JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- 6. 决策日志
CREATE TABLE IF NOT EXISTS decision_logs (
    id BIGSERIAL PRIMARY KEY,
    topic_id VARCHAR(64) NOT NULL,
    agent_id VARCHAR(64) NOT NULL,
    decision TEXT NOT NULL,
    context JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_decision_topic ON decision_logs(topic_id, created_at DESC);

-- 7. 话题归档
CREATE TABLE IF NOT EXISTS topic_archives (
    id BIGSERIAL PRIMARY KEY,
    topic_id VARCHAR(64) NOT NULL UNIQUE,
    summary TEXT NOT NULL,
    outputs JSONB DEFAULT '[]',
    decisions JSONB DEFAULT '[]',
    embedding VECTOR(768),
    archived_at TIMESTAMPTZ DEFAULT NOW()
);

-- 8. 向量索引 (ivfflat 适合中等规模数据)
CREATE INDEX IF NOT EXISTS idx_gk_embedding ON global_knowledge
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);

CREATE INDEX IF NOT EXISTS idx_archive_embedding ON topic_archives
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);

CREATE TABLE IF NOT EXISTS memory_write_failures (
    id SERIAL PRIMARY KEY,
    agent_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    step_count INT NOT NULL,
    action TEXT,
    raw_content TEXT,
    error TEXT NOT NULL,
    retry_count INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    resolved_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_memory_write_failures_unresolved ON memory_write_failures(created_at) WHERE resolved_at IS NULL;
