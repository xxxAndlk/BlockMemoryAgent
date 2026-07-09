package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误信息
	"time"          // 时间戳

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
)

// SnapshotStore 是 Agent 快照相关的 PostgreSQL 存储子层。
// 职责: agent_snapshots 表的 UPSERT 与读取，为 Redis 热存提供冷存回源。
type SnapshotStore struct {
	db *sql.DB // 共享连接池
}

// Save 保存 Agent 快照 (UPSERT)。
// 参数:
//   - snapshot: 含 AgentID/TopicID 与完整上下文状态
//
// 返回: SQL 执行错误。
// 设计意图: 每个 (agent, topic) 仅保留一份最新快照,供热加载使用。
// 副作用: ON CONFLICT 命中主键则更新 snapshot 与 updated_at。
func (s *SnapshotStore) Save(ctx context.Context, snapshot *types.AgentSnapshot) error {
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

// Get 读取 Agent 快照。
// 返回:
//   - *types.AgentSnapshot: 命中时返回;不存在时返回 (nil, nil)
//   - error: 其他 SQL/反序列化错误
//
// 设计意图: 配合 Redis 缓存做热加载,未命中时回源 PG。
func (s *SnapshotStore) Get(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT snapshot FROM agent_snapshots
		WHERE agent_id = $1 AND topic_id = $2
	`, agentID, topicID).Scan(&raw)
	// 行不存在视为正常情况,返回 (nil, nil)
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
