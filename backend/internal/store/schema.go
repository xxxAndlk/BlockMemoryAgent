package store

import (
	"context"      // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql" // 标准库 SQL 抽象层
	"fmt"          // 格式化错误信息与 pgvector 字符串
	"strconv"      // 解析 vector(768) 维度数字（ValidateEmbeddingDimension）
	"strings"      // 拼接 pgvector 的逗号分隔向量分量
)

// pgVector 将 float32 切片转为 pgvector 字符串格式 [1,2,3]。
// 设计意图: database/sql 不直接支持 pgvector 类型,需以文本字面量形式传入 SQL。
// 参数:
//   - v: 待转换的向量分量切片。
//
// 返回: 形如 "[0.123000,0.456000]" 的字符串;空切片返回 "[]"。
func pgVector(v []float32) string {
	if len(v) == 0 {
		// 空向量返回 "[]",避免插入 NULL，保持与 pgvector 空向量语义一致
		return "[]"
	}
	// 预分配 parts 切片，每个元素对应一个分量字符串
	parts := make([]string, len(v))
	for i, f := range v {
		// 每个分量按 %f 格式化，保留小数位，确保 pgvector 能正确解析
		parts[i] = fmt.Sprintf("%f", f)
	}
	// 用逗号拼接并加上方括号，得到 pgvector 文本格式
	return "[" + strings.Join(parts, ",") + "]"
}

// EnsureSessionHistorySchema 自动创建 session_history 表 (幂等)。
// 同时确保 006_session_history_meta_memory.sql 中声明的 meta_memory 列已存在，
// 避免 SaveSessionHistory 因列缺失而失败。
// 参数:
//   - ctx: 超时与取消控制。
//   - db:  *sql.DB 连接池。
//
// 返回: 建表/索引错误。
func EnsureSessionHistorySchema(ctx context.Context, db *sql.DB) error {
	// 执行幂等 DDL：IF NOT EXISTS 保证重复调用不会报错
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS session_history (
    id           BIGSERIAL PRIMARY KEY,
    session_id   VARCHAR(64) NOT NULL,
    goal         TEXT NOT NULL,
    summary      TEXT NOT NULL,
    tool_results JSONB DEFAULT '[]',
    meta_memory  JSONB DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_session_history_created_at
    ON session_history (created_at DESC);
-- 老库缺 meta_memory 列时补齐（CREATE TABLE IF NOT EXISTS 对存量表是空操作）。
ALTER TABLE session_history ADD COLUMN IF NOT EXISTS meta_memory JSONB DEFAULT '[]';
`)
	return err
}

// EnsureSessionLogsSchema 自动创建 session_logs 表 (幂等)。
// 对应 migrations/005_session_logs.sql，供 logger 持久化结构化 Agent IO 日志。
// 参数:
//   - ctx: 超时与取消控制。
//   - db:  *sql.DB 连接池。
//
// 返回: 建表/索引错误。
func EnsureSessionLogsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS session_logs (
    id            BIGSERIAL PRIMARY KEY,
    session_id    VARCHAR(64) NOT NULL,
    agent         VARCHAR(128) NOT NULL DEFAULT '',
    level         VARCHAR(32) NOT NULL DEFAULT 'info',
    phase         VARCHAR(128) NOT NULL DEFAULT '',
    message       TEXT NOT NULL DEFAULT '',
    prompt        TEXT NOT NULL DEFAULT '',
    response      TEXT NOT NULL DEFAULT '',
    input_tokens  INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    model         VARCHAR(128) NOT NULL DEFAULT '',
    latency_ms    INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    meta_json     JSONB DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_session_logs_session_id ON session_logs(session_id);
CREATE INDEX IF NOT EXISTS idx_session_logs_agent ON session_logs(agent);
CREATE INDEX IF NOT EXISTS idx_session_logs_level ON session_logs(level);
CREATE INDEX IF NOT EXISTS idx_session_logs_created_at ON session_logs(created_at);
`)
	return err
}

// EnsureSessionEventsSchema 自动创建 session_events 表 (幂等)。
// 参数:
//   - ctx: 超时与取消控制。
//   - db:  *sql.DB 连接池。
//
// 返回: 建表/索引错误。
func EnsureSessionEventsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS session_events (
    id            BIGSERIAL PRIMARY KEY,
    session_id    VARCHAR(64) NOT NULL,
    type          VARCHAR(32) NOT NULL DEFAULT '',
    agent         VARCHAR(128) NOT NULL DEFAULT '',
    message       TEXT NOT NULL DEFAULT '',
    kind          VARCHAR(32) NOT NULL DEFAULT '',
    tool          VARCHAR(128) NOT NULL DEFAULT '',
    tool_path     TEXT NOT NULL DEFAULT '',
    tool_output   TEXT NOT NULL DEFAULT '',
    tool_error    TEXT NOT NULL DEFAULT '',
    success       BOOLEAN NOT NULL DEFAULT false,
    timestamp     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    prompt        TEXT NOT NULL DEFAULT '',
    input_tokens  INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    detail_json   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_session_events_session_ts
    ON session_events (session_id, timestamp);
`)
	return err
}

// EnsureAgentEventsSchema 自动创建 agent_events 表 (幂等)。
// 供 domain/memory.PostgresEventStore 持久化 ReAct Agent 事件流
// (tool_call / answer / sub_agent_summary 等)。
// 与 session_events 分表:session_events 记 UI 进度事件 (旧,只读),
// agent_events 记记忆流水线事件 (Pipeline.Write 落库)。
// session_id 从 agentID 派生 (MetaAgent agentID==sessionID; 子 Agent "session-N/role-K" 取前段)。
func EnsureAgentEventsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS agent_events (
    id          BIGSERIAL PRIMARY KEY,
    session_id  VARCHAR(64) NOT NULL DEFAULT '',
    agent_id    VARCHAR(256) NOT NULL DEFAULT '',
    type        VARCHAR(64) NOT NULL DEFAULT '',
    role        VARCHAR(64) NOT NULL DEFAULT '',
    content     TEXT NOT NULL DEFAULT '',
    tool_name   VARCHAR(128) NOT NULL DEFAULT '',
    input       TEXT NOT NULL DEFAULT '',
    output      TEXT NOT NULL DEFAULT '',
    occurred    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_events_agent_occurred
    ON agent_events (agent_id, occurred DESC);
CREATE INDEX IF NOT EXISTS idx_agent_events_session
    ON agent_events (session_id);
`)
	return err
}

// EnsureAgentMessagesSchema 自动创建 agent_messages 表 (幂等)。
// 存 Paused DomainAgent 的完整 ReAct 消息历史,供 resume 时 LoadMessages 重建上下文。
// 与 agent_events 分表:agent_events 记事件流(tool_call/answer 摘要,按 occurred DESC);
// agent_messages 记完整消息(role/content/tool_calls/tool_call_id/reasoning,按 seq ASC)。
// 仅 DomainAgent 触达 token 上限时写入;叶子助手不持久化 history。
func EnsureAgentMessagesSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS agent_messages (
    id           BIGSERIAL PRIMARY KEY,
    session_id   VARCHAR(64)  NOT NULL DEFAULT '',
    agent_id     VARCHAR(256) NOT NULL DEFAULT '',
    seq          INT          NOT NULL,
    role         VARCHAR(16)  NOT NULL DEFAULT '',
    content      TEXT         NOT NULL DEFAULT '',
    tool_call_id VARCHAR(64)  NOT NULL DEFAULT '',
    tool_calls   JSONB        NOT NULL DEFAULT '[]',
    reasoning    TEXT         NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_messages_agent_seq
    ON agent_messages (agent_id, seq ASC);
CREATE INDEX IF NOT EXISTS idx_agent_messages_session
    ON agent_messages (session_id);
`)
	return err
}

// EnsureInitialMemorySchema 自动创建 001_init.sql 中定义的记忆/知识/注册表相关表 (幂等)。
// 负责在启动时补齐 global_knowledge / agent_private_memory / agent_snapshots / topics /
// agent_registry / decision_logs / topic_archives 等表,避免块记忆、私有记忆、快照写入失败。
// 注意: agent_private_memory / agent_snapshots 两张表为 legacy——仅 EpisodeStore/SnapshotStore
// （dormant，无运行时代码路径）与 server snapshot 兼容接口消费，保留 DDL 仅为不丢历史数据。
// 参数:
//   - ctx: 超时与取消控制。
//   - db:  *sql.DB 连接池。
//
// 返回: 建表/索引/迁移错误。
func EnsureInitialMemorySchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS agent_private_memory (
    id BIGSERIAL PRIMARY KEY,
    agent_id VARCHAR(64) NOT NULL,
    topic_id VARCHAR(64) NOT NULL,
    episode JSONB NOT NULL,
    snapshot_ref VARCHAR(128),
    compression_level INT DEFAULT 0, -- 0=Raw 完整记录, 1=Standard 摘要; 仅两级
    importance_score FLOAT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
ALTER TABLE agent_private_memory ADD COLUMN IF NOT EXISTS step_count INT DEFAULT 0;

-- 为已有数据分配唯一 step_count，避免创建唯一索引时冲突（P0-2 迁移兼容）
UPDATE agent_private_memory SET step_count = id WHERE step_count = 0 OR step_count IS NULL;

CREATE INDEX IF NOT EXISTS idx_apm_agent_topic ON agent_private_memory(agent_id, topic_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_apm_importance ON agent_private_memory(importance_score DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_apm_episode_gin ON agent_private_memory USING GIN (episode);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_private_memory_idempotent ON agent_private_memory(agent_id, topic_id, step_count);

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

CREATE TABLE IF NOT EXISTS topics (
    id VARCHAR(64) PRIMARY KEY,
    goal TEXT NOT NULL,
    status VARCHAR(32) DEFAULT 'active',
    constraints JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS global_knowledge (
    id BIGSERIAL PRIMARY KEY,
    knowledge_type VARCHAR(32) NOT NULL,
    topic_id VARCHAR(64),
    content TEXT NOT NULL,
    embedding VECTOR(1024),
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
-- 外部知识库全文检索（TODO #27）：生成列 + GIN 索引。
-- 'simple' 配置对英文分词可用；中文需部署 zhparser/pg_jieba 后改 'zhparser'（见 retriever 文档）。
ALTER TABLE global_knowledge ADD COLUMN IF NOT EXISTS content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;
CREATE INDEX IF NOT EXISTS idx_gk_content_tsv ON global_knowledge USING GIN (content_tsv);

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

CREATE TABLE IF NOT EXISTS decision_logs (
    id BIGSERIAL PRIMARY KEY,
    topic_id VARCHAR(64) NOT NULL,
    agent_id VARCHAR(64) NOT NULL,
    decision TEXT NOT NULL,
    context JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_decision_topic ON decision_logs(topic_id, created_at DESC);

CREATE TABLE IF NOT EXISTS topic_archives (
    id BIGSERIAL PRIMARY KEY,
    topic_id VARCHAR(64) NOT NULL UNIQUE,
    summary TEXT NOT NULL,
    outputs JSONB DEFAULT '[]',
    decisions JSONB DEFAULT '[]',
    embedding VECTOR(1024),
    archived_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_gk_embedding ON global_knowledge
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);

CREATE INDEX IF NOT EXISTS idx_archive_embedding ON topic_archives
USING ivfflat (embedding vector_cosine_ops)
WITH (lists = 100);
`)
	return err
}

// ValidateEmbeddingDimension 校验 global_knowledge.embedding 列的实际向量维度与配置一致。
//
// 职责：pgvector 的 VECTOR(N) 列维度在建表时固定；若 config 的 pgvector.dimensions 与列维度
// 不一致，所有 SaveKnowledge 的 INSERT 会因维度不匹配静默失败（仅日志），导致块记忆/归档
// 无法落库。启动期显式校验，不一致则返回错误，由调用方 fatal 退出（TODO #4 D3）。
//
// 参数：
//   - ctx：请求上下文。
//   - db：数据库连接。
//   - expectedDim：期望维度（来自 config pgvector.dimensions）。
//
// 返回：一致返回 nil；不一致或查询失败返回描述性错误。
func ValidateEmbeddingDimension(ctx context.Context, db *sql.DB, expectedDim int) error {
	var typeStr string
	// format_type 返回形如 "vector(768)"；pg_attribute 取 embedding 列的类型
	err := db.QueryRowContext(ctx, `
SELECT format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a
WHERE a.attrelid = 'global_knowledge'::regclass AND a.attname = 'embedding'
`).Scan(&typeStr)
	if err != nil {
		return fmt.Errorf("查询 embedding 列类型失败: %w", err)
	}
	actual := parseVectorDim(typeStr)
	if actual <= 0 {
		// 列可能不存在或非 vector 类型；不阻断（建表逻辑会处理）
		return nil
	}
	if actual != expectedDim {
		return fmt.Errorf("embedding 维度不一致: 数据库 VECTOR(%d) ≠ 配置 pgvector.dimensions(%d);"+
			"请调整配置或重建 global_knowledge 表（DROP 后重启自动建表）", actual, expectedDim)
	}
	return nil
}

// parseVectorDim 从 "vector(768)" 这类类型字符串中解析出维度数字。
// 参数:
//   - typeStr: pgvector 类型文本，例如 "vector(768)" 或 "public.vector(768)"。
//
// 返回: 解析出的维度；格式不符时返回 0。
func parseVectorDim(typeStr string) int {
	// 取 '(' 与 ')' 之间的数字
	start := strings.Index(typeStr, "(")
	end := strings.Index(typeStr, ")")
	if start < 0 || end <= start {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(typeStr[start+1 : end]))
	if err != nil {
		return 0
	}
	return n
}
