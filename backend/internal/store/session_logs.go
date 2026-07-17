package store

import (
	"bytes"         // 清洗 JSON 字节中的非法 UTF-8
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误与动态 SQL 拼接
	"strings"       // 校验/清洗 UTF-8
	"time"          // 时间戳
)

// sanitizeUTF8 清洗字符串中的非法 UTF8 字节序列。
// Postgres 拒绝 0xe7 0xbb 0x2e 这类不完整的多字节序列（错误码 22021），
// 用 strings.ToValidUTF8 把非法字节替换为 U+FFFD。
// 参数:
//   - s: 原始字符串。
//
// 返回: 清洗后的合法 UTF-8 字符串；空字符串原样返回。
func sanitizeUTF8(s string) string {
	if s == "" {
		return s
	}
	// 如果已经合法，直接返回，避免拷贝
	if strings.ToValidUTF8(s, "") == s {
		return s
	}
	// 替换非法字节为 �
	return strings.ToValidUTF8(s, "�")
}

// SessionLogRecord 单条结构化会话日志。
type SessionLogRecord struct {
	ID           int64          // 自增主键
	SessionID    string         // 会话 ID
	Agent        string         // Agent 标识
	Level        string         // 日志级别
	Phase        string         // 阶段
	Message      string         // 消息
	Prompt       string         // 提示词
	Response     string         // 模型响应
	InputTokens  int            // 输入 token 数
	OutputTokens int            // 输出 token 数
	Model        string         // 模型名
	LatencyMs    int            // 延迟毫秒
	CreatedAt    time.Time      // 创建时间
	Meta         map[string]any // 扩展元数据
}

// TokenAggregation 按 agent/model 聚合的 token 消耗。
type TokenAggregation struct {
	TotalInputTokens  int                             // 总会话输入 token
	TotalOutputTokens int                             // 总会话输出 token
	TotalCalls        int                             // 总调用次数
	ByAgentModel      map[string]AgentModelTokenStats // 按 agent|model 分组统计
}

// AgentModelTokenStats 单个 agent + model 的 token 统计。
type AgentModelTokenStats struct {
	Agent        string // Agent 标识
	Model        string // 模型名
	InputTokens  int    // 输入 token 数
	OutputTokens int    // 输出 token 数
	Calls        int    // 调用次数
}

// SaveSessionLog 写入一条结构化会话日志。
// 参数:
//   - ctx: 请求上下文。
//   - rec: 日志记录指针，nil 会返回错误。
//
// 返回: 清洗、序列化或插入错误。
func (s *PostgresStore) SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error {
	if rec == nil {
		return fmt.Errorf("nil session log record")
	}
	// 对可能来自外部模型的文本字段做 UTF-8 清洗，防止 Postgres 22021 错误
	rec.SessionID = sanitizeUTF8(rec.SessionID)
	rec.Agent = sanitizeUTF8(rec.Agent)
	rec.Level = sanitizeUTF8(rec.Level)
	rec.Phase = sanitizeUTF8(rec.Phase)
	rec.Message = sanitizeUTF8(rec.Message)
	rec.Prompt = sanitizeUTF8(rec.Prompt)
	rec.Response = sanitizeUTF8(rec.Response)
	rec.Model = sanitizeUTF8(rec.Model)
	// 序列化扩展元数据
	metaRaw, err := json.Marshal(rec.Meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	// 对元数据 JSON 也做 UTF-8 清洗，防止非法字节落库
	metaRaw = bytes.ToValidUTF8(metaRaw, []byte("�"))
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO session_logs (
				session_id, agent, level, phase, message,
				prompt, response, input_tokens, output_tokens, model, latency_ms, created_at, meta_json
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, rec.SessionID, rec.Agent, rec.Level, rec.Phase, rec.Message,
		rec.Prompt, rec.Response, rec.InputTokens, rec.OutputTokens, rec.Model, rec.LatencyMs,
		rec.CreatedAt, metaRaw)
	if err != nil {
		return fmt.Errorf("insert session_log: %w", err)
	}
	return nil
}

// QuerySessionLogs 按会话/Agent/Level 查询日志，按时间降序。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID，必填。
//   - agent:     Agent 过滤，空表示不过滤。
//   - level:     级别过滤，空表示不过滤。
//   - limit:     返回上限，<=0 默认 100。
//   - offset:    分页偏移。
//
// 返回: 日志记录切片与错误。
func (s *PostgresStore) QuerySessionLogs(ctx context.Context, sessionID, agent, level string, limit, offset int) ([]*SessionLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	// 基础 WHERE 条件
	q := `
			SELECT id, session_id, agent, level, phase, message,
			       prompt, response, input_tokens, output_tokens, model, latency_ms, created_at, meta_json
			FROM session_logs
			WHERE session_id = $1
		`
	args := []any{sessionID}
	argIdx := 1
	// 动态追加 agent 过滤条件
	if agent != "" {
		argIdx++
		q += fmt.Sprintf(" AND agent = $%d", argIdx)
		args = append(args, agent)
	}
	// 动态追加 level 过滤条件
	if level != "" {
		argIdx++
		q += fmt.Sprintf(" AND level = $%d", argIdx)
		args = append(args, level)
	}
	// 追加排序与分页
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", argIdx+1, argIdx+2)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query session_logs: %w", err)
	}
	defer rows.Close()
	return s.scanSessionLogRows(ctx, rows)
}

// AggregateSessionTokens 按 agent + model 聚合会话的 token 消耗。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID。
//
// 返回: TokenAggregation 指针与错误。
func (s *PostgresStore) AggregateSessionTokens(ctx context.Context, sessionID string) (*TokenAggregation, error) {
	rows, err := s.db.QueryContext(ctx, `
			SELECT agent, model, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COUNT(*)
			FROM session_logs
			WHERE session_id = $1 AND (input_tokens > 0 OR output_tokens > 0)
			GROUP BY agent, model
		`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("aggregate tokens: %w", err)
	}
	defer rows.Close()

	agg := &TokenAggregation{ByAgentModel: make(map[string]AgentModelTokenStats)}
	for rows.Next() {
		var agent, model sql.NullString
		var input, output, calls int
		if err := rows.Scan(&agent, &model, &input, &output, &calls); err != nil {
			continue
		}
		// NullString 可能为 NULL，用 String 字段兜底
		aKey := agent.String
		mKey := model.String
		key := aKey + "|" + mKey
		agg.ByAgentModel[key] = AgentModelTokenStats{
			Agent:        aKey,
			Model:        mKey,
			InputTokens:  input,
			OutputTokens: output,
			Calls:        calls,
		}
		agg.TotalInputTokens += input
		agg.TotalOutputTokens += output
		agg.TotalCalls += calls
	}
	return agg, rows.Err()
}

// scanSessionLogRows 扫描 session_logs 结果集。
// 参数:
//   - ctx: 请求上下文，用于记录反序列化失败日志。
//   - rows: 已执行的 *sql.Rows。
//
// 返回: 日志记录切片与迭代错误；扫描/反序列化失败的单行被跳过。
func (s *PostgresStore) scanSessionLogRows(ctx context.Context, rows *sql.Rows) ([]*SessionLogRecord, error) {
	var out []*SessionLogRecord
	for rows.Next() {
		var r SessionLogRecord
		var metaRaw []byte
		err := rows.Scan(
			&r.ID, &r.SessionID, &r.Agent, &r.Level, &r.Phase, &r.Message,
			&r.Prompt, &r.Response, &r.InputTokens, &r.OutputTokens, &r.Model, &r.LatencyMs,
			&r.CreatedAt, &metaRaw,
		)
		if err != nil {
			continue
		}
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &r.Meta); err != nil {
				logError(s.log, ctx, fmt.Sprintf("[store] unmarshal session_logs.meta_json failed: id=%d", r.ID), err)
			}
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
