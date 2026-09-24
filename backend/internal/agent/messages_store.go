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
	// SaveMessages 保存该 agent 的消息历史（TODO #20① append-only，不物理删行）：
	// 前缀稳定（run 内 history 只追加）时只补插尾部；分叉时旧版整体移归档后全量插入。
	SaveMessages(ctx context.Context, agentID, sessionID string, msgs []ReactMessage) error
	// LoadMessages 按 seq ASC 加载该 agent 的在用（archived=false）消息历史。无数据返回 nil。
	LoadMessages(ctx context.Context, agentID string) ([]ReactMessage, error)
	// ArchiveMessages 归档该 agent 的全部在用消息历史（复活重跑前作废旧 run 的终态快照）。
	// 只改 archived 标记不物理删行（TODO #20①）：读路径（LoadMessages/对话面板 PG 兜底）
	// 过滤 archived=false——旧快照不会被当成当前对话续上去（对话面板显示新内容一瞬间
	// 又变回旧的，2026-09-17 用户实证）；旧 run 完整轨迹经 SQL WHERE archived=true 查回
	//（#20④ 失败分支剪枝的底账）。
	ArchiveMessages(ctx context.Context, agentID string) error
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

// SaveMessages 保存该 agent 的消息历史（TODO #20① append-only，零物理删除）。
// 旧语义是 delete-then-insert 全量覆盖——旧行物理消失，失败轨迹不可 SQL 查回（违反
// 底账纪律）。现语义：
//   - 前缀稳定（run 内 history 只追加、re-pause 重写是超集）：只补插 msgs[activeCount:] 尾部，
//     旧行原样保留，O(新增) 写入；
//   - 分叉（复活新 run / 热驻换任务 / 快照变短——边界行对不上）：旧版整体移归档
//     （archived=true）后按 seq=0..n 全量插入，同旧覆盖语义但旧行可查。
// 重复保存同一快照幂等（尾插 0 条）。tool_calls 序列化为 JSONB 数组；reasoning/tool_call_id 逐字段存。
func (s *PostgresMessagesStore) SaveMessages(ctx context.Context, agentID, sessionID string, msgs []ReactMessage) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 同 agent 的保存串行化（advisory xact lock，事务结束自动释放）：并发"计数-补插"
	// 窗口重叠会双双看到同一 activeCount → 同 seq 重复行（表无唯一键，旧 delete-then-insert
	// 由 DELETE 行锁天然串行，并发语义在 append-only 下须显式补齐）。
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, "agent_messages:"+agentID); err != nil {
		return err
	}

	// 在用行数 + 边界行一致性探测：在用行 seq 恒为 0..count-1（插入侧保证）。
	var activeCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_messages WHERE agent_id=$1 AND archived=FALSE`, agentID).Scan(&activeCount); err != nil {
		return err
	}
	tailFrom := activeCount
	if activeCount > len(msgs) {
		tailFrom = 0 // 快照变短：视为分叉（覆盖语义）
	} else if activeCount > 0 {
		var role, content string
		err := tx.QueryRowContext(ctx,
			`SELECT role, LEFT(content, 200) FROM agent_messages
WHERE agent_id=$1 AND seq=$2 AND archived=FALSE`, agentID, activeCount-1).Scan(&role, &content)
		if err != nil || role != msgs[activeCount-1].Role ||
			content != contentPrefix200(sanitizeUTF8(msgs[activeCount-1].Content)) {
			tailFrom = 0
		}
	}
	if tailFrom == 0 && activeCount > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_messages SET archived=TRUE WHERE agent_id=$1 AND archived=FALSE`, agentID); err != nil {
			return err
		}
	}
	if tailFrom < len(msgs) {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO agent_messages
(session_id, agent_id, seq, role, content, tool_call_id, tool_calls, reasoning)
VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := tailFrom; i < len(msgs); i++ {
			m := msgs[i]
			content, reasoning, calls := sanitizeMessageFields(m)
			if _, err := stmt.ExecContext(ctx, sessionID, agentID, i, m.Role, content, m.ToolCallID, calls, reasoning); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// contentPrefix200 取内容前 200 个字符（与 SQL LEFT(content,200) 同字符口径），
// 供 SaveMessages 边界行一致性探测。
func contentPrefix200(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200])
	}
	return s
}

// sanitizeMessageFields 清洗入库的字符串字段（content/reasoning/tool_calls JSON）：
// 剥离 NUL 并替换非法 UTF-8。用户输入可能夹带 0x00（Windows 控制台 Ctrl+Space、
// 粘贴带 NUL 文本），原样 INSERT 会被 PG 拒绝（invalid byte sequence 22021）。
func sanitizeMessageFields(m ReactMessage) (content, reasoning, calls string) {
	callsJSON, _ := json.Marshal(m.ToolCalls)
	return sanitizeUTF8(m.Content), sanitizeUTF8(m.ReasoningContent), sanitizeUTF8(string(callsJSON))
}

// LoadMessages 按 seq ASC 加载该 agent 的在用（archived=false）消息历史。
// tool_calls 从 JSONB 反序列化回 []ToolCall。归档行（复活前旧 run 快照）不进读路径，
// 失败轨迹考古直接 SQL WHERE archived=true。
func (s *PostgresMessagesStore) LoadMessages(ctx context.Context, agentID string) ([]ReactMessage, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role, content, tool_call_id, tool_calls, reasoning
FROM agent_messages WHERE agent_id=$1 AND archived=FALSE ORDER BY seq ASC`, agentID)
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

// ArchiveMessages 归档该 agent 的全部在用消息历史（复活重跑前作废旧 run 的终态快照），
// 幂等（无行也不报错）。TODO #20①：UPDATE archived 标记替代物理 DELETE——
// 读路径过滤 archived=false 拿到隔离效果，旧 run 轨迹留底账供失败分支剪枝/考古 SQL 查回。
func (s *PostgresMessagesStore) ArchiveMessages(ctx context.Context, agentID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_messages SET archived=TRUE WHERE agent_id=$1 AND archived=FALSE`, agentID)
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
