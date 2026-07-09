package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
)

// TopicStore 是话题元数据与归档相关的 PostgreSQL 存储子层。
// 职责: topics 表与 topic_archives 表的读写。
type TopicStore struct {
	db *sql.DB // 共享连接池
}

// Create 创建话题元数据。
// 参数:
//   - topic: 含 ID/Goal/Status/CreatedAt/ExpiresAt
//
// 返回: SQL 执行错误。
// 副作用: constraints 字段写入空 JSON 对象占位。
func (s *TopicStore) Create(ctx context.Context, topic *types.TopicMeta) error {
	// 暂以空 map 占位,后续通过 SetTopicConstraints 维护
	constraints, _ := json.Marshal(map[string]string{})
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topics (id, goal, status, constraints, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, topic.ID, topic.Goal, topic.Status, constraints, topic.CreatedAt, topic.ExpiresAt)
	return err
}

// Get 读取话题元数据。
// 参数:
//   - topicID: 话题 ID
//
// 返回: 命中返回 *TopicMeta;不存在返回 (nil, nil)。
// 副作用: expires_at 可能为 NULL,通过 sql.NullTime 安全读取。
func (s *TopicStore) Get(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	var t types.TopicMeta
	var constraintsRaw []byte
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, goal, status, constraints, created_at, expires_at
		FROM topics WHERE id = $1
	`, topicID).Scan(&t.ID, &t.Goal, &t.Status, &constraintsRaw, &t.CreatedAt, &expiresAt)
	// 行不存在视为正常情况
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 仅在有效时填充 ExpiresAt 指针
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return &t, nil
}

// SaveArchive 持久化话题归档摘要 (UPSERT)。
// 参数:
//   - topicID:   话题 ID
//   - summary:   归档摘要文本
//   - outputs:   产出 JSON (RawMessage)
//   - decisions: 决策 JSON (RawMessage)
//   - embedding: 摘要向量,用于跨话题检索
//
// 返回: SQL 执行错误。
// 副作用: 同一 topic_id 重复归档会覆盖原记录。
func (s *TopicStore) SaveArchive(ctx context.Context, topicID, summary string, outputs, decisions json.RawMessage, embedding []float32) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topic_archives (topic_id, summary, outputs, decisions, embedding, archived_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (topic_id) DO UPDATE
		SET summary = $2, outputs = $3, decisions = $4, embedding = $5, archived_at = NOW()
	`, topicID, summary, outputs, decisions, pgVector(embedding))
	return err
}
