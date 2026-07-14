package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误信息
	"log"           // 反序列化失败时记录坏数据
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
	db *sql.DB // 共享连接池
}

// SaveHistory 持久化一次会话的 goal/summary/工具调用结果/调度记忆。
// 参数:
//   - ctx: 请求上下文。
//   - rec: 会话历史记录;ToolResults/MetaMemory 为 nil 时补为空数组,保证 JSONB 非 null
//
// 返回: SQL 执行错误。
// 副作用: ON CONFLICT DO NOTHING 保证同 session_id 重复写入幂等。
func (s *SessionStore) SaveHistory(ctx context.Context, rec *SessionHistoryRecord) error {
	if rec.ToolResults == nil {
		// 避免写入 null,统一为空数组便于下游读取
		rec.ToolResults = []map[string]any{}
	}
	if rec.MetaMemory == nil {
		rec.MetaMemory = []map[string]any{}
	}
	toolData, err := json.Marshal(rec.ToolResults)
	if err != nil {
		return fmt.Errorf("marshal tool_results: %w", err)
	}
	memData, err := json.Marshal(rec.MetaMemory)
	if err != nil {
		return fmt.Errorf("marshal meta_memory: %w", err)
	}
	// COALESCE 保证 created_at 为零值时回退到 NOW()
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO session_history (session_id, goal, summary, tool_results, meta_memory, created_at)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6, NOW()))
			ON CONFLICT DO NOTHING
		`, rec.SessionID, rec.Goal, rec.Summary, toolData, memData, rec.CreatedAt)
	return err
}

// SaveEvents 批量持久化会话事件。
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
		_, err := stmt.ExecContext(ctx, sessionID, e.Type, e.Agent, e.Message, e.Kind, e.Tool, e.ToolPath, e.ToolOutput, e.ToolError, e.Success, e.Timestamp, e.Prompt, e.InputTokens, e.OutputTokens, e.DetailJSON)
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
			SELECT session_id, goal, summary, tool_results, meta_memory, created_at
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
		if err := rows.Scan(&r.SessionID, &r.Goal, &r.Summary, &toolRaw, &memRaw, &r.CreatedAt); err != nil {
			// 单行扫描失败跳过
			continue
		}
		// 仅在非空时反序列化 tool_results / meta_memory
		if len(toolRaw) > 0 {
			if err := json.Unmarshal(toolRaw, &r.ToolResults); err != nil {
				log.Printf("[store] unmarshal session_history.tool_results failed: session=%s err=%v", r.SessionID, err)
			}
		}
		if len(memRaw) > 0 {
			if err := json.Unmarshal(memRaw, &r.MetaMemory); err != nil {
				log.Printf("[store] unmarshal session_history.meta_memory failed: session=%s err=%v", r.SessionID, err)
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
			SELECT session_id, goal, summary, tool_results, meta_memory, created_at
			FROM session_history
			WHERE session_id = $1
		`, id).Scan(&r.SessionID, &r.Goal, &r.Summary, &toolRaw, &memRaw, &r.CreatedAt)
	if err == sql.ErrNoRows {
		// 未找到是正常情况
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(toolRaw) > 0 {
		if err := json.Unmarshal(toolRaw, &r.ToolResults); err != nil {
			log.Printf("[store] unmarshal session_history.tool_results failed: session=%s err=%v", r.SessionID, err)
		}
	}
	if len(memRaw) > 0 {
		if err := json.Unmarshal(memRaw, &r.MetaMemory); err != nil {
			log.Printf("[store] unmarshal session_history.meta_memory failed: session=%s err=%v", r.SessionID, err)
		}
	}
	return &r, nil
}
