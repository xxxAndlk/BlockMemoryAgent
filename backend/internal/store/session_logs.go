package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SessionLogRecord 单条结构化会话日志。
type SessionLogRecord struct {
	ID           int64
	SessionID    string
	Agent        string
	Level        string
	Phase        string
	Message      string
	Prompt       string
	Response     string
	InputTokens  int
	OutputTokens int
	Model        string
	LatencyMs    int
	CreatedAt    time.Time
	Meta         map[string]any
}

// TokenAggregation 按 agent/model 聚合的 token 消耗。
type TokenAggregation struct {
	TotalInputTokens  int
	TotalOutputTokens int
	TotalCalls        int
	ByAgentModel      map[string]AgentModelTokenStats
}

// AgentModelTokenStats 单个 agent + model 的 token 统计。
type AgentModelTokenStats struct {
	Agent        string
	Model        string
	InputTokens  int
	OutputTokens int
	Calls        int
}

// SaveSessionLog 写入一条结构化会话日志。
func (s *PostgresStore) SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error {
	if rec == nil {
		return fmt.Errorf("nil session log record")
	}
	metaRaw, err := json.Marshal(rec.Meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
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
func (s *PostgresStore) QuerySessionLogs(ctx context.Context, sessionID, agent, level string, limit, offset int) ([]*SessionLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `
		SELECT id, session_id, agent, level, phase, message,
		       prompt, response, input_tokens, output_tokens, model, latency_ms, created_at, meta_json
		FROM session_logs
		WHERE session_id = $1
	`
	args := []any{sessionID}
	argIdx := 1
	if agent != "" {
		argIdx++
		q += fmt.Sprintf(" AND agent = $%d", argIdx)
		args = append(args, agent)
	}
	if level != "" {
		argIdx++
		q += fmt.Sprintf(" AND level = $%d", argIdx)
		args = append(args, level)
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", argIdx+1, argIdx+2)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query session_logs: %w", err)
	}
	defer rows.Close()
	return scanSessionLogRows(rows)
}

// AggregateSessionTokens 按 agent + model 聚合会话的 token 消耗。
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
func scanSessionLogRows(rows *sql.Rows) ([]*SessionLogRecord, error) {
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
			_ = json.Unmarshal(metaRaw, &r.Meta)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
