package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误信息
	"time"          // 时间戳
)

// SessionHistoryRecord 跨会话历史摘要。
// 设计意图: 长期沉淀每次会话的 goal/summary/工具调用结果/调度记忆,供后续会话检索复用。
type SessionHistoryRecord struct {
	SessionID   string           `json:"session_id"`   // 会话唯一标识
	Goal        string           `json:"goal"`         // 会话目标
	Summary     string           `json:"summary"`      // 会话总结
	ToolResults []map[string]any `json:"tool_results"` // 工具调用结果数组
	MetaMemory  []map[string]any `json:"meta_memory"`  // P0-1: MetaAgent 调度记忆
	CreatedAt   time.Time        `json:"created_at"`   // 创建时间
	WorkDir     string           `json:"work_dir"`     // S2: 每会话工作目录(空串=进程默认)
	Status      string           `json:"status"`       // 009: 会话最后一次落库时的状态(running/completed/error/awaiting_clarify/paused_on_child)
}

// SessionEventRecord 会话事件归档记录。
type SessionEventRecord struct {
	SessionID    string    `json:"session_id"`    // 所属会话 ID
	Type         string    `json:"type"`          // 事件类型
	Agent        string    `json:"agent"`         // 产生事件的 Agent
	Message      string    `json:"message"`       // 事件消息
	Kind         string    `json:"kind"`          // 事件子类
	Tool         string    `json:"tool"`          // 涉及工具名
	ToolPath     string    `json:"tool_path"`     // 工具调用路径
	ToolOutput   string    `json:"tool_output"`   // 工具输出
	ToolError    string    `json:"tool_error"`    // 工具错误
	Success      bool      `json:"success"`       // 是否成功
	Timestamp    time.Time `json:"timestamp"`     // 事件时间戳
	Prompt       string    `json:"prompt"`        // 提示词
	InputTokens  int       `json:"input_tokens"`  // 输入 token 数
	OutputTokens int       `json:"output_tokens"` // 输出 token 数
	DetailJSON   string    `json:"detail_json"`   // 扩展详情 JSON
}

// SessionStore 是会话历史/事件相关的 PostgreSQL 存储子层。
// 职责: session_history 与 session_events 表的读写。
type SessionStore struct {
	db  *sql.DB // 共享连接池
	log Logger  // 结构化日志器，由 PostgresStore.SetLogger 传播注入；nil 时回退标准库 log
}

// SaveHistory 持久化一次会话的 goal/summary/工具调用结果/调度记忆/工作目录/状态。
// 参数:
//   - ctx: 请求上下文。
//   - rec: 会话历史记录;ToolResults/MetaMemory 为 nil 时补为空数组,保证 JSONB 非 null
//
// 返回: SQL 执行错误。
// 副作用: ON CONFLICT (session_id) DO UPDATE 保证同 session_id 幂等且始终保留最新一轮
// 的 goal/summary/status(009 唯一索引 uniq_session_history_session_id 支撑冲突目标)。
func (s *SessionStore) SaveHistory(ctx context.Context, rec *SessionHistoryRecord) error {
	if rec.ToolResults == nil {
		// 避免写入 null,统一为空数组便于下游读取
		rec.ToolResults = []map[string]any{}
	}
	if rec.MetaMemory == nil {
		rec.MetaMemory = []map[string]any{}
	}
	// 先递归清洗字符串值中的 NUL/非法 UTF-8：jsonb 拒绝  转义与裸 0x00，
	// 工具输出夹带 NUL 会导致整行写入失败（调用方清洗不完整时在此兜底）。
	toolData, err := json.Marshal(sanitizeJSONValue(rec.ToolResults))
	if err != nil {
		return fmt.Errorf("marshal tool_results: %w", err)
	}
	memData, err := json.Marshal(sanitizeJSONValue(rec.MetaMemory))
	if err != nil {
		return fmt.Errorf("marshal meta_memory: %w", err)
	}
	// COALESCE 保证 created_at 为零值时回退到 NOW()
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO session_history (session_id, goal, summary, tool_results, meta_memory, created_at, work_dir, status)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6, NOW()), $7, $8)
			ON CONFLICT (session_id) DO UPDATE SET
				goal = EXCLUDED.goal,
				summary = EXCLUDED.summary,
				tool_results = EXCLUDED.tool_results,
				meta_memory = EXCLUDED.meta_memory,
				created_at = EXCLUDED.created_at,
				work_dir = EXCLUDED.work_dir,
				status = EXCLUDED.status
		`, rec.SessionID, sanitizeUTF8(rec.Goal), sanitizeUTF8(rec.Summary), toolData, memData, rec.CreatedAt, sanitizeUTF8(rec.WorkDir), sanitizeUTF8(rec.Status))
	return err
}

// SaveEvents 批量持久化会话事件(全量覆盖语义)。
// 每轮结束调用方都会重写该会话的全部事件,先 DELETE 再 INSERT 保证幂等,
// 恢复-续跑场景不会与已落库的旧行叠加重复(与 PostgresMessagesStore.SaveMessages 同语义)。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID。
//   - events:    待写入事件列表。
//
// 返回: 事务开始、准备语句、执行或提交错误。
func (s *SessionStore) SaveEvents(ctx context.Context, sessionID string, events []SessionEventRecord) error {
	// 开启事务保证批量写入原子性
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// 任何返回路径都回滚；成功提交会覆盖回滚操作
	defer tx.Rollback()

	// 先清空该会话的旧事件,再全量写入本轮快照(delete-then-insert)
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_events WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("delete old events: %w", err)
	}

	// 预编译 INSERT 语句，提升批量写入性能
	stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO session_events (session_id, type, agent, message, kind, tool, tool_path, tool_output, tool_error, success, timestamp, prompt, input_tokens, output_tokens, detail_json)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		// 存储层兜底清洗：调用方未必都做过 sanitize，
		// NUL/非法 UTF-8 会让整批写入被 Postgres 拒绝（22021）。
		_, err := stmt.ExecContext(ctx, sessionID, e.Type, e.Agent, sanitizeUTF8(e.Message), e.Kind, e.Tool, sanitizeUTF8(e.ToolPath), sanitizeUTF8(e.ToolOutput), sanitizeUTF8(e.ToolError), e.Success, e.Timestamp, sanitizeUTF8(e.Prompt), e.InputTokens, e.OutputTokens, sanitizeUTF8(e.DetailJSON))
		if err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
	}
	// 全部执行成功后提交事务
	return tx.Commit()
}

// GetEvents 读取某个会话的全部事件（按时间升序）。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID。
//
// 返回: 事件切片与 SQL 错误。
func (s *SessionStore) GetEvents(ctx context.Context, sessionID string) ([]SessionEventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
			SELECT session_id, type, agent, message, kind, tool, tool_path, tool_output, tool_error, success, timestamp, prompt, input_tokens, output_tokens, detail_json
			FROM session_events
			WHERE session_id = $1
			ORDER BY timestamp ASC
		`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []SessionEventRecord
	for rows.Next() {
		var e SessionEventRecord
		if err := rows.Scan(&e.SessionID, &e.Type, &e.Agent, &e.Message, &e.Kind, &e.Tool, &e.ToolPath, &e.ToolOutput, &e.ToolError, &e.Success, &e.Timestamp, &e.Prompt, &e.InputTokens, &e.OutputTokens, &e.DetailJSON); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// sessionScopedTables 列出全部带 session_id 列的会话从属表，会话硬删除逐表清扫。
// agent_events/agent_messages/agent_compress_states 的 session_id 由 agentID 派生
//（MetaAgent agentID==sessionID；子 Agent "session-N/role-K" 取首段），DELETE 按
// session_id 等价即可覆盖该会话的全部子 Agent 行。
var sessionScopedTables = []string{
	"session_history",
	"session_events",
	"session_logs",
	"agent_events",
	"agent_messages",
	"agent_compress_states",
	"agent_tree_nodes",
}

// DeleteSessionData 硬删除会话的全部持久化数据（sessionScopedTables 逐表 DELETE）。
// 单事务保证一致性；幂等——目标 session 无数据时各 DELETE 影响 0 行，返回 nil。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID。
//
// 返回: 事务错误（任一表删除失败整体回滚）。
func (s *SessionStore) DeleteSessionData(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	for _, table := range sessionScopedTables {
		// 表名为编译期常量清单，非用户输入，直接内插安全。
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id = $1`, sessionID); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return tx.Commit()
}

// RecentHistories 返回最近 limit 条会话历史 (按时间倒序)。
// 参数:
//   - ctx:   请求上下文。
//   - limit: 返回上限,<=0 时默认 10
//
// 返回: 会话历史切片与 SQL 错误。
// 设计意图: 给新会话提供"最近发生过什么"的上下文。
func (s *SessionStore) RecentHistories(ctx context.Context, limit int) ([]*SessionHistoryRecord, error) {
	if limit <= 0 {
		// 兜底默认值
		limit = 10
	}
	// 查询最近 limit 条历史，按创建时间倒序
	rows, err := s.db.QueryContext(ctx, `
			SELECT session_id, goal, summary, tool_results, meta_memory, created_at, work_dir, status
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
		var toolRaw, memRaw []byte
		if err := rows.Scan(&r.SessionID, &r.Goal, &r.Summary, &toolRaw, &memRaw, &r.CreatedAt, &r.WorkDir, &r.Status); err != nil {
			// 单行扫描失败跳过
			continue
		}
		// 仅在非空时反序列化 tool_results / meta_memory
		if len(toolRaw) > 0 {
			if err := json.Unmarshal(toolRaw, &r.ToolResults); err != nil {
				logError(s.log, ctx, fmt.Sprintf("[store] unmarshal session_history.tool_results failed: session=%s", r.SessionID), err)
			}
		}
		if len(memRaw) > 0 {
			if err := json.Unmarshal(memRaw, &r.MetaMemory); err != nil {
				logError(s.log, ctx, fmt.Sprintf("[store] unmarshal session_history.meta_memory failed: session=%s", r.SessionID), err)
			}
		}
		out = append(out, &r)
	}
	return out, nil
}

// GetHistoryByID 按 session_id 查询单条会话历史。
// 设计意图: 内存中只保留最近 N 条会话,旧会话从 DB 按需查。
// 参数:
//   - ctx: 上下文。
//   - id:  会话 ID (session-N)。
//
// 返回: 历史记录指针; 未找到返回 (nil, nil)。
func (s *SessionStore) GetHistoryByID(ctx context.Context, id string) (*SessionHistoryRecord, error) {
	var r SessionHistoryRecord
	var toolRaw, memRaw []byte
	err := s.db.QueryRowContext(ctx, `
			SELECT session_id, goal, summary, tool_results, meta_memory, created_at, work_dir, status
			FROM session_history
			WHERE session_id = $1
		`, id).Scan(&r.SessionID, &r.Goal, &r.Summary, &toolRaw, &memRaw, &r.CreatedAt, &r.WorkDir, &r.Status)
	if err == sql.ErrNoRows {
		// 未找到是正常情况
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(toolRaw) > 0 {
		if err := json.Unmarshal(toolRaw, &r.ToolResults); err != nil {
			logError(s.log, ctx, fmt.Sprintf("[store] unmarshal session_history.tool_results failed: session=%s", r.SessionID), err)
		}
	}
	if len(memRaw) > 0 {
		if err := json.Unmarshal(memRaw, &r.MetaMemory); err != nil {
			logError(s.log, ctx, fmt.Sprintf("[store] unmarshal session_history.meta_memory failed: session=%s", r.SessionID), err)
		}
	}
	return &r, nil
}
