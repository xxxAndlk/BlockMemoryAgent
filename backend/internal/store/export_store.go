package store

// export_store.go 会话数据导出查询（TODO #18-2 T29 GET /api/sessions/:id/export）。
// 聚合会话的四类持久化数据（历史/事件/子 Agent 事件流/子 Agent 消息历史）为一份
// 内存结构，HTTP 层打包成 zip；workspace/.bma 产物树由 HTTP 层按会话工作目录补齐。
// 纯读侧聚合，零写入。

import (
	"context"
	"encoding/json"
	"time"
)

// SessionExportData 单会话导出数据集（JSON 序列化后进 zip）。
type SessionExportData struct {
	SessionID     string                `json:"session_id"`
	History       *SessionHistoryRecord `json:"history,omitempty"`
	Events        []SessionEventRecord  `json:"events"`
	AgentEvents   []map[string]any      `json:"agent_events"`
	AgentMessages []map[string]any      `json:"agent_messages"`
}

// ExportSessionData 聚合单会话全部持久化数据。
// nil 库 / nil 接收者返回仅含 session_id 的空数据集（HTTP 层仍产出合法 zip）。
func (s *PostgresStore) ExportSessionData(ctx context.Context, sessionID string) (*SessionExportData, error) {
	out := &SessionExportData{SessionID: sessionID}
	if s == nil || s.db == nil || s.Session == nil {
		return out, nil
	}
	hist, err := s.Session.GetHistoryByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out.History = hist

	events, err := s.Session.GetEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []SessionEventRecord{}
	}
	out.Events = events

	agentEvents, err := s.queryAgentEventsForExport(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out.AgentEvents = agentEvents

	agentMsgs, err := s.queryAgentMessagesForExport(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out.AgentMessages = agentMsgs
	return out, nil
}

// queryAgentEventsForExport 子 Agent 事件流全量（按时间升序）。
func (s *PostgresStore) queryAgentEventsForExport(ctx context.Context, sessionID string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, agent_id, type, role, content, tool_name, input, output, occurred
			FROM agent_events
			WHERE session_id = $1
			ORDER BY occurred ASC, id ASC
		`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var agentID, typ, role, content, toolName, input, output string
		var occurred time.Time
		if err := rows.Scan(&id, &agentID, &typ, &role, &content, &toolName, &input, &output, &occurred); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "agent_id": agentID, "type": typ, "role": role,
			"content": content, "tool_name": toolName, "input": input,
			"output": output, "occurred": occurred,
		})
	}
	return out, rows.Err()
}

// queryAgentMessagesForExport 子 Agent 完整消息历史（按 agent_id + seq 升序）。
// 归档行（archived=true，复活/分叉前旧 run 快照）一并导出并带 archived 标记——
// 导出是全量备份（TODO #20①），失败轨迹考古不该被读路径过滤挡住。
func (s *PostgresStore) queryAgentMessagesForExport(ctx context.Context, sessionID string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, agent_id, seq, role, content, tool_call_id, tool_calls, reasoning, archived, created_at
			FROM agent_messages
			WHERE session_id = $1
			ORDER BY agent_id ASC, seq ASC
		`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var agentID, role, content, toolCallID, reasoning string
		var seq int
		var archived bool
		var toolCalls []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &agentID, &seq, &role, &content, &toolCallID, &toolCalls, &reasoning, &archived, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "agent_id": agentID, "seq": seq, "role": role,
			"content": content, "tool_call_id": toolCallID,
			"tool_calls": json.RawMessage(toolCalls), "reasoning": reasoning,
			"archived": archived, "created_at": createdAt,
		})
	}
	return out, rows.Err()
}
