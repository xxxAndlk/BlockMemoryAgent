package memory

// pg_store.go 提供 domain/memory.Store 的 PostgreSQL 实现 PostgresEventStore。
//
// 用途: ReAct Agent 事件流 (Pipeline.Write 落入) 持久化到 agent_events 表,
// 会话结束后事件不丢失,供事后审计与(未来)重启恢复。与 session_events 分表:
// session_events 记 UI 进度事件 (旧, 只读); agent_events 记记忆流水线事件。
//
// 读路径仍走 Pipeline 的内存 events map (Assemble 用), 本 Store 仅写侧持久化;
// LoadEvents 供未来按需重载 (当前未接热路径), 按 occurred DESC 取最近 limit 条。
//
// session_id 从 agentID 派生: MetaAgent 的 agentID 即 sessionID ("session-1");
// 子 Agent 形如 "session-1/code_assistant-1", 取 "/" 前段。

import (
	"context"
	"database/sql"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// PostgresEventStore 是基于 *sql.DB 的 memory.Store 实现,把事件写入 agent_events 表。
// db 为 nil 时所有方法无操作返回 nil (测试场景)。
type PostgresEventStore struct {
	db *sql.DB
}

// NewPostgresEventStore 创建 PG 事件存储。db 可为 nil (降级为无持久化)。
func NewPostgresEventStore(db *sql.DB) *PostgresEventStore {
	return &PostgresEventStore{db: db}
}

// SaveEvent 把单条事件插入 agent_events。
// session_id 从 agentID 派生 (MetaAgent==sessionID, 子 Agent 取 "/" 前段)。
// 持久化失败向上返回错误, 由 Pipeline.Write 包装为 "persist event" 错误。
func (s *PostgresEventStore) SaveEvent(ctx context.Context, agentID string, event agent.MemoryEvent) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_events
(session_id, agent_id, type, role, content, tool_name, input, output, occurred)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sessionIDFromAgentID(agentID), agentID, event.Type, event.Role, event.Content,
		event.ToolName, event.Input, event.Output, event.Occurred)
	return err
}

// LoadEvents 按 agent_id 取最近 limit 条事件, 按 occurred DESC (最新在前) 返回。
// limit<=0 时用 DefaultEventLimit。当前未接热路径 (Assemble 读内存), 供未来重载。
func (s *PostgresEventStore) LoadEvents(ctx context.Context, agentID string, limit int) ([]agent.MemoryEvent, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT type, agent_id, role, content, tool_name, input, output, occurred
FROM agent_events WHERE agent_id=$1 ORDER BY occurred DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.MemoryEvent
	for rows.Next() {
		var ev agent.MemoryEvent
		if err := rows.Scan(&ev.Type, &ev.AgentID, &ev.Role, &ev.Content, &ev.ToolName, &ev.Input, &ev.Output, &ev.Occurred); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// sessionIDFromAgentID 从 agentID 派生 sessionID:
// "session-1" -> "session-1" (MetaAgent); "session-1/code_assistant-1" -> "session-1" (子 Agent)。
// 无 "/" 时原样返回。
func sessionIDFromAgentID(agentID string) string {
	if i := strings.Index(agentID, "/"); i > 0 {
		return agentID[:i]
	}
	return agentID
}
