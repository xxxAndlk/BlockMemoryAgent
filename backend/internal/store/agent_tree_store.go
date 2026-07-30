package store

// agent_tree_store.go 提供 Agent 树节点的 PostgreSQL 持久化实现。
// orchestrator.Tree 通过 TreeStore 接口注入,Register/Finish/Cancel 后 best-effort 写入。
// 服务重启后 TreeFor 调 LoadNodes 恢复历史节点(元数据恢复, cancel func 无法恢复)。
//
// 表 schema 见 EnsureAgentTreeSchema(agent_tree_nodes)。upsert 语义:同 (session_id, node_id)
// 覆盖,保证 Register 后 Finish/Cancel 的状态更新生效。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// PostgresTreeStore 实现 orchestrator.TreeStore 接口,把 Agent 树节点持久化到 PG。
type PostgresTreeStore struct {
	db *sql.DB
}

// NewPostgresTreeStore 构造一个基于 *sql.DB 的树存储实例。
func NewPostgresTreeStore(db *sql.DB) *PostgresTreeStore {
	return &PostgresTreeStore{db: db}
}

// SaveNode upsert 一个节点到 agent_tree_nodes 表。
// 同 (session_id, node_id) 覆盖,保证状态更新生效。
func (s *PostgresTreeStore) SaveNode(ctx context.Context, sessionID string, node orchestrator.Node) error {
	if s.db == nil {
		return fmt.Errorf("postgres tree store: db is nil")
	}
	if sessionID == "" || node.ID == "" {
		return fmt.Errorf("postgres tree store: session_id and node_id are required")
	}
	var finished any
	if !node.Finished.IsZero() {
		finished = node.Finished
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO agent_tree_nodes
    (session_id, node_id, parent_id, role, domain, task, status, started, finished, summary, err)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (session_id, node_id) DO UPDATE SET
    parent_id = EXCLUDED.parent_id,
    role = EXCLUDED.role,
    domain = EXCLUDED.domain,
    task = EXCLUDED.task,
    status = EXCLUDED.status,
    started = EXCLUDED.started,
    finished = EXCLUDED.finished,
    summary = EXCLUDED.summary,
    err = EXCLUDED.err
`,
		sessionID, node.ID, node.ParentID, node.Role, node.Domain, node.Task,
		node.Status.String(), node.Started, finished, node.Summary, node.Err,
	)
	if err != nil {
		return fmt.Errorf("upsert agent tree node: %w", err)
	}
	return nil
}

// LoadNodes 按 sessionID 加载全部节点,按 started 升序。
// 空切片表示 session 无持久化节点(新会话或首次访问)。
func (s *PostgresTreeStore) LoadNodes(ctx context.Context, sessionID string) ([]orchestrator.Node, error) {
	if s.db == nil {
		return nil, fmt.Errorf("postgres tree store: db is nil")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT node_id, parent_id, role, domain, task, status, started, finished, summary, err
FROM agent_tree_nodes
WHERE session_id = $1
ORDER BY started ASC
`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load agent tree nodes: %w", err)
	}
	defer rows.Close()
	var out []orchestrator.Node
	for rows.Next() {
		var n orchestrator.Node
		var statusStr, role, domain, task, summary, errMsg string
		var started time.Time
		var finished sql.NullTime
		if err := rows.Scan(&n.ID, &n.ParentID, &role, &domain, &task, &statusStr, &started, &finished, &summary, &errMsg); err != nil {
			return nil, fmt.Errorf("scan agent tree node: %w", err)
		}
		n.Role = role
		n.Domain = domain
		n.Task = task
		n.Summary = summary
		n.Err = errMsg
		n.Started = started
		if finished.Valid {
			n.Finished = finished.Time
		}
		n.Status = parseStatus(statusStr)
		out = append(out, n)
	}
	return out, rows.Err()
}

// parseStatus 把字符串状态映射回 Status 枚举。未知值视为 StatusRunning(安全默认)。
func parseStatus(s string) orchestrator.Status {
	switch s {
	case "running":
		return orchestrator.StatusRunning
	case "done":
		return orchestrator.StatusDone
	case "failed":
		return orchestrator.StatusFailed
	case "cancelled":
		return orchestrator.StatusCancelled
	}
	return orchestrator.StatusRunning
}

// EnsureAgentTreeSchema 自动创建 agent_tree_nodes 表 (幂等)。
// 持久化 Agent 树节点元数据,服务重启后 TreeFor 调 LoadNodes 恢复。
func EnsureAgentTreeSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS agent_tree_nodes (
    session_id  VARCHAR(64) NOT NULL,
    node_id     VARCHAR(128) NOT NULL,
    parent_id   VARCHAR(128) NOT NULL DEFAULT '',
    role        VARCHAR(64) NOT NULL DEFAULT '',
    domain      TEXT NOT NULL DEFAULT '',
    task        TEXT NOT NULL DEFAULT '',
    status      VARCHAR(32) NOT NULL DEFAULT 'running',
    started     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished    TIMESTAMPTZ,
    summary     TEXT NOT NULL DEFAULT '',
    err         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_tree_nodes_session
    ON agent_tree_nodes (session_id);
`)
	return err
}
