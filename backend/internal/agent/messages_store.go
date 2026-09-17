package agent

// messages_store.go 提供 Paused DomainAgent 完整 ReAct 消息历史的持久化。
//
// 用途:DomainAgent 触达 token 上限进入 Paused 时,把 result.History 落库到 agent_messages,
// resume 时 LoadMessages 重建上下文继续 RunWithHistory。叶子助手不持久化(返回部分产出给父)。
//
// 接口 MessagesStore 消费 agent.ReactMessage(本包类型),PG impl 在本包(复用 *sql.DB)。
// DDL 见 store.EnsureAgentMessagesSchema(schema.go),由 bootstrap 注册建表。
// session_id 从 agentID 派生(同 memory.pg_store: MetaAgent==sessionID, 子 Agent取"/"前段)。
//
// 依赖方向:agent 包 import database/sql(stdlib),无 store->agent 反向依赖。
// 与 memory.PostgresEventStore(domain/memory,import agent)同模式:impl 消费 agent 类型。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// MessagesStore 持久化 Paused Agent 的完整 ReAct 消息历史。
// 由 bootstrap 注入 PostgresMessagesStore;为 nil 时跳过持久化(测试场景)。
type MessagesStore interface {
	// SaveMessages 用 msgs 覆盖该 agent 的全部消息历史(tx 内 delete-then-insert)。
	SaveMessages(ctx context.Context, agentID, sessionID string, msgs []ReactMessage) error
	// LoadMessages 按 seq ASC 加载该 agent 的全部消息历史。无数据返回 nil。
	LoadMessages(ctx context.Context, agentID string) ([]ReactMessage, error)
	// DeleteMessages 删除该 agent 的全部消息历史（复活重跑前作废旧 run 的终态快照）。
	// 读路径在热层不足 limit 时会并 PG 兜底，旧快照会被当成当前对话续上去（对话面板
	// 显示新内容一瞬间又变回旧的）；而旧快照本就是待覆盖的死数据（终态 SaveMessages
	// 是 delete-then-insert 全量覆盖），提前作废不丢任何在用状态。
	DeleteMessages(ctx context.Context, agentID string) error
}

// PostgresMessagesStore 基于 *sql.DB 实现 MessagesStore,写入 agent_messages 表。
// db 为 nil 时所有方法无操作返回 nil(测试场景)。
type PostgresMessagesStore struct {
	db *sql.DB
}

// NewPostgresMessagesStore 创建 PG 消息历史存储。db 可为 nil(降级为无持久化)。
func NewPostgresMessagesStore(db *sql.DB) *PostgresMessagesStore {
	return &PostgresMessagesStore{db: db}
}

// SaveMessages 用 msgs 覆盖该 agent 的消息历史。
// tx 内先 DELETE 该 agent_id 的旧行,再按 seq 逐条 INSERT(全量覆盖语义,支持 re-pause 重写)。
// tool_calls 序列化为 JSONB 数组;reasoning/tool_call_id 逐字段存。
func (s *PostgresMessagesStore) SaveMessages(ctx context.Context, agentID, sessionID string, msgs []ReactMessage) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_messages WHERE agent_id=$1`, agentID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO agent_messages
(session_id, agent_id, seq, role, content, tool_call_id, tool_calls, reasoning)
VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, m := range msgs {
		content, reasoning, calls := sanitizeMessageFields(m)
		if _, err := stmt.ExecContext(ctx, sessionID, agentID, i, m.Role, content, m.ToolCallID, calls, reasoning); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sanitizeMessageFields 清洗入库的字符串字段（content/reasoning/tool_calls JSON）：
// 剥离 NUL 并替换非法 UTF-8。用户输入可能夹带 0x00（Windows 控制台 Ctrl+Space、
// 粘贴带 NUL 文本），原样 INSERT 会被 PG 拒绝（invalid byte sequence 22021）。
func sanitizeMessageFields(m ReactMessage) (content, reasoning, calls string) {
	callsJSON, _ := json.Marshal(m.ToolCalls)
	return sanitizeUTF8(m.Content), sanitizeUTF8(m.ReasoningContent), sanitizeUTF8(string(callsJSON))
}

// LoadMessages 按 seq ASC 加载该 agent 的全部消息历史。
// tool_calls 从 JSONB 反序列化回 []ToolCall。
func (s *PostgresMessagesStore) LoadMessages(ctx context.Context, agentID string) ([]ReactMessage, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role, content, tool_call_id, tool_calls, reasoning
FROM agent_messages WHERE agent_id=$1 ORDER BY seq ASC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReactMessage
	for rows.Next() {
		var m ReactMessage
		var callsJSON string
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &callsJSON, &m.ReasoningContent); err != nil {
			return nil, err
		}
		if callsJSON != "" && callsJSON != "[]" {
			_ = json.Unmarshal([]byte(callsJSON), &m.ToolCalls)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMessages 删除该 agent 的全部消息历史（复活重跑前作废旧 run 的终态快照），
// 幂等（无行也不报错）。
func (s *PostgresMessagesStore) DeleteMessages(ctx context.Context, agentID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_messages WHERE agent_id=$1`, agentID)
	return err
}

// messagesStoreSessionID 从 agentID 派生 sessionID(同 memory.pg_store.sessionIDFromAgentID)。
// "session-1" -> "session-1"(MetaAgent);"session-1/code_assistant-1" -> "session-1"(子 Agent)。
// 无 "/" 时原样返回。供调用方(resume 路径)派生 session_id 写入 agent_messages。
func messagesStoreSessionID(agentID string) string {
	if i := strings.Index(agentID, "/"); i > 0 {
		return agentID[:i]
	}
	return agentID
}
