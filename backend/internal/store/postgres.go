package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
	_ "github.com/lib/pq"
)

// PostgresStore PostgreSQL 存储层
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore 创建 PostgreSQL 存储
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)
	return &PostgresStore{db: db}, nil
}

// Close 关闭连接
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB 暴露原始连接 (用于迁移)
func (s *PostgresStore) DB() *sql.DB {
	return s.db
}

// SaveEpisode 保存 Episode
func (s *PostgresStore) SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error {
	data, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("marshal episode: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_private_memory (agent_id, topic_id, episode, importance_score, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, agentID, topicID, data, ep.Importance, ep.Timestamp)
	return err
}

// GetEpisodes 获取 Agent 在话题下的所有 Episode
func (s *PostgresStore) GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT episode FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2
		ORDER BY created_at DESC
		LIMIT $3
	`, agentID, topicID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []*types.Episode
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ep types.Episode
		if err := json.Unmarshal(raw, &ep); err != nil {
			continue
		}
		episodes = append(episodes, &ep)
	}
	return episodes, rows.Err()
}

// GetEpisodesByLevel 按压缩层级获取
func (s *PostgresStore) GetEpisodesByLevel(ctx context.Context, agentID, topicID string, level types.CompressionLevel, limit int) ([]*types.Episode, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT episode FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2 AND compression_level = $3
		ORDER BY importance_score DESC, created_at DESC
		LIMIT $4
	`, agentID, topicID, int(level), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []*types.Episode
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ep types.Episode
		if err := json.Unmarshal(raw, &ep); err != nil {
			continue
		}
		episodes = append(episodes, &ep)
	}
	return episodes, rows.Err()
}

// CountEpisodes 统计 Episode 数量
func (s *PostgresStore) CountEpisodes(ctx context.Context, agentID, topicID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2
	`, agentID, topicID).Scan(&count)
	return count, err
}

// CountEpisodesByLevel 按层级统计
func (s *PostgresStore) CountEpisodesByLevel(ctx context.Context, agentID, topicID string) (map[types.CompressionLevel]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT compression_level, COUNT(*) FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2
		GROUP BY compression_level
	`, agentID, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[types.CompressionLevel]int)
	for rows.Next() {
		var level int
		var count int
		if err := rows.Scan(&level, &count); err != nil {
			continue
		}
		result[types.CompressionLevel(level)] = count
	}
	return result, rows.Err()
}

// BatchUpdateEpisodes 批量更新 Episode
func (s *PostgresStore) BatchUpdateEpisodes(ctx context.Context, agentID, topicID string, episodes []*types.Episode) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, ep := range episodes {
		data, err := json.Marshal(ep)
		if err != nil {
			continue
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE agent_private_memory
			SET episode = $1, importance_score = $2, updated_at = $3
			WHERE agent_id = $4 AND topic_id = $5 AND episode->>'step_id' = $6
		`, data, ep.Importance, time.Now(), agentID, topicID, ep.StepID)
		if err != nil {
			continue
		}
	}
	return tx.Commit()
}

// SaveSnapshot 保存 Agent 快照
func (s *PostgresStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_snapshots (agent_id, topic_id, snapshot, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent_id, topic_id)
		DO UPDATE SET snapshot = $3, updated_at = $4
	`, snapshot.AgentID, snapshot.TopicID, data, time.Now())
	return err
}

// GetSnapshot 获取 Agent 快照
func (s *PostgresStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT snapshot FROM agent_snapshots
		WHERE agent_id = $1 AND topic_id = $2
	`, agentID, topicID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap types.AgentSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return &snap, nil
}

// SaveKnowledge 保存知识记录
func (s *PostgresStore) SaveKnowledge(ctx context.Context, rec *types.KnowledgeRecord) error {
	meta, _ := json.Marshal(rec.Meta)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO global_knowledge (knowledge_type, topic_id, content, embedding, meta, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, rec.KnowledgeType, rec.TopicID, rec.Content, pgVector(rec.Embedding), meta, rec.CreatedAt)
	return err
}

// GetKnowledgeByType 按类型获取知识
func (s *PostgresStore) GetKnowledgeByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE knowledge_type = $1 AND archived = false
		ORDER BY last_accessed DESC NULLS LAST
		LIMIT $2
	`, knowledgeType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanKnowledgeRows(rows)
}

// SearchKnowledge 向量相似搜索 (需 pgvector)
func (s *PostgresStore) SearchKnowledge(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE archived = false
		ORDER BY embedding <=> $1
		LIMIT $2
	`, pgVector(embedding), topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanKnowledgeRows(rows)
}

// ArchiveKnowledge 归档知识
func (s *PostgresStore) ArchiveKnowledge(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge SET archived = true WHERE id = $1
	`, id)
	return err
}

// IncrementAccessCount 增加访问计数
func (s *PostgresStore) IncrementAccessCount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge
		SET access_count = access_count + 1, last_accessed = NOW()
		WHERE id = $1
	`, id)
	return err
}

// CreateTopic 创建话题
func (s *PostgresStore) CreateTopic(ctx context.Context, topic *types.TopicMeta) error {
	constraints, _ := json.Marshal(map[string]string{})
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topics (id, goal, status, constraints, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, topic.ID, topic.Goal, topic.Status, constraints, topic.CreatedAt, topic.ExpiresAt)
	return err
}

// GetTopic 获取话题
func (s *PostgresStore) GetTopic(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	var t types.TopicMeta
	var constraintsRaw []byte
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, goal, status, constraints, created_at, expires_at
		FROM topics WHERE id = $1
	`, topicID).Scan(&t.ID, &t.Goal, &t.Status, &constraintsRaw, &t.CreatedAt, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return &t, nil
}

// SaveTopicArchive 归档话题
func (s *PostgresStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions json.RawMessage, embedding []float32) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topic_archives (topic_id, summary, outputs, decisions, embedding, archived_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (topic_id) DO UPDATE
		SET summary = $2, outputs = $3, decisions = $4, embedding = $5, archived_at = NOW()
	`, topicID, summary, outputs, decisions, pgVector(embedding))
	return err
}

// RegisterAgent 注册 Agent
func (s *PostgresStore) RegisterAgent(ctx context.Context, id, name, description, moduleID string, keywords, dependencies, capabilities []string) error {
	kw, _ := json.Marshal(keywords)
	deps, _ := json.Marshal(dependencies)
	caps, _ := json.Marshal(capabilities)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_registry (id, name, description, module_id, keywords, dependencies, capabilities, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (id) DO UPDATE
		SET name = $2, description = $3, module_id = $4, keywords = $5, dependencies = $6, capabilities = $7
	`, id, name, description, moduleID, kw, deps, caps)
	return err
}

// GetAgentRegistry 获取所有注册 Agent
func (s *PostgresStore) GetAgentRegistry(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, module_id, keywords, dependencies, capabilities
		FROM agent_registry
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, name, description, moduleID string
		var keywords, dependencies, capabilities []byte
		if err := rows.Scan(&id, &name, &description, &moduleID, &keywords, &dependencies, &capabilities); err != nil {
			continue
		}
		results = append(results, map[string]any{
			"id":           id,
			"name":         name,
			"description":  description,
			"module_id":    moduleID,
			"keywords":     keywords,
			"dependencies": dependencies,
			"capabilities": capabilities,
		})
	}
	return results, rows.Err()
}

// SaveDecisionLog 保存决策日志
func (s *PostgresStore) SaveDecisionLog(ctx context.Context, topicID, agentID, decision string, context map[string]any) error {
	ctxData, _ := json.Marshal(context)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO decision_logs (topic_id, agent_id, decision, context, created_at)
		VALUES ($1, $2, $3, $4, NOW())
	`, topicID, agentID, decision, ctxData)
	return err
}

// SessionHistoryRecord 跨会话历史摘要
type SessionHistoryRecord struct {
	SessionID   string    `json:"session_id"`
	Goal        string    `json:"goal"`
	Summary     string    `json:"summary"`
	ToolResults []map[string]any `json:"tool_results"`
	CreatedAt   time.Time `json:"created_at"`
}

// SaveSessionHistory 持久化一次会话的 goal/summary/工具调用结果
func (s *PostgresStore) SaveSessionHistory(ctx context.Context, rec *SessionHistoryRecord) error {
	if rec.ToolResults == nil {
		rec.ToolResults = []map[string]any{}
	}
	data, err := json.Marshal(rec.ToolResults)
	if err != nil {
		return fmt.Errorf("marshal tool_results: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO session_history (session_id, goal, summary, tool_results, created_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, NOW()))
		ON CONFLICT DO NOTHING
	`, rec.SessionID, rec.Goal, rec.Summary, data, rec.CreatedAt)
	return err
}

// RecentSessionHistories 返回最近 limit 条会话历史（按时间倒序）
func (s *PostgresStore) RecentSessionHistories(ctx context.Context, limit int) ([]*SessionHistoryRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, goal, summary, tool_results, created_at
		FROM session_history
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*SessionHistoryRecord
	for rows.Next() {
		var r SessionHistoryRecord
		var raw []byte
		if err := rows.Scan(&r.SessionID, &r.Goal, &r.Summary, &raw, &r.CreatedAt); err != nil {
			continue
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &r.ToolResults)
		}
		out = append(out, &r)
	}
	return out, nil
}

// pgVector 将 float32 切片转为 pgvector 字符串格式 [1,2,3]
func pgVector(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%f", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func scanKnowledgeRows(rows *sql.Rows) ([]*types.KnowledgeRecord, error) {
	var results []*types.KnowledgeRecord
	for rows.Next() {
		var r types.KnowledgeRecord
		var metaRaw []byte
		var lastAccessed sql.NullTime
		err := rows.Scan(&r.ID, &r.KnowledgeType, &r.TopicID, &r.Content, &metaRaw,
			&r.AccessCount, &lastAccessed, &r.CreatedAt, &r.Archived)
		if err != nil {
			continue
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &r.Meta)
		}
		if lastAccessed.Valid {
			r.LastAccessed = &lastAccessed.Time
		}
		results = append(results, &r)
	}
	return results, rows.Err()
}
