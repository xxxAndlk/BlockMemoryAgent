package store

// efficiency.go 提供会话效率一等指标的聚合查询（TODO 第9项⑥ / 第10项③）。
// 纯聚合，无新采集管道：LLM 调用数据来自 session_logs（agent 列 = 角色 ID），
// 逐轮事件来自 agent_events（agent_id 列 = 实例 ID，tool_call/answer 事件）。
//
// 口径说明：
//   - 单呼输入 P50/P95 用 percentile_cont（输入 token 连续分布的分位数，order by input_tokens）；
//   - 校验开销占比 / meta:domain token 比在查询侧按角色名分组后由 service 层计算；
//   - WriteFile/EditFile 去重交付路径在 Go 侧从 tool_call input JSON 提取
//     （input 为 TEXT 列，路径键位置不固定，SQL JSON 解析对截断/非 JSON 输入脆弱）。

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RoleEfficiencyStats 按角色聚合的 LLM 调用效率统计（session_logs.agent = 角色 ID）。
type RoleEfficiencyStats struct {
	Agent          string        // 角色 ID（session_logs.agent）
	Calls          int           // LLM 调用次数
	InputTokens    int64         // 输入 token 合计
	OutputTokens   int64         // 输出 token 合计
	AvgLatency     time.Duration // 平均调用耗时
	P50InputTokens float64       // 单呼输入 token 中位数（percentile_cont 0.5）
	P95InputTokens float64       // 单呼输入 token P95（percentile_cont 0.95）
}

// AgentEventStats 按实例聚合的事件统计（agent_events.agent_id = 实例 ID）。
type AgentEventStats struct {
	AgentID     string   // 实例 ID（树节点 ID）
	ToolCalls   int      // tool_call 事件数（≈ 轮次-1，每轮至多一次工具批次）
	Answers     int      // answer 事件数（终答，正常 1）
	WrittenFiles []string // WriteFile/EditFile 去重交付路径（Go 侧解析，保序）
}

// pathExtractRegex 从工具入参 JSON 文本中提取 "path" 值（WriteFile/EditFile 首参）。
// input 可能被截断/非严格 JSON，用正则而非 json.Unmarshal：容错且避免全量解析开销。
var pathExtractRegex = regexp.MustCompile(`"path"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// QuerySessionRoleEfficiency 按角色聚合会话全部 LLM 调用的效率指标。
// 仅统计 phase 前缀 llm 的行（模型调用；辅助/提取类轻量调用同前缀一并计入）。
func (s *PostgresStore) QuerySessionRoleEfficiency(ctx context.Context, sessionID string) ([]*RoleEfficiencyStats, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent,
       COUNT(*)                                AS calls,
       COALESCE(SUM(input_tokens), 0)          AS in_tok,
       COALESCE(SUM(output_tokens), 0)         AS out_tok,
       COALESCE(AVG(latency_ms), 0)            AS avg_latency,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY input_tokens), 0) AS p50,
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY input_tokens), 0) AS p95
FROM session_logs
WHERE session_id = $1 AND phase LIKE 'llm%'
GROUP BY agent
ORDER BY in_tok DESC
`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query role efficiency: %w", err)
	}
	defer rows.Close()
	out := make([]*RoleEfficiencyStats, 0, 8)
	for rows.Next() {
		var r RoleEfficiencyStats
		var agent sql.NullString
		var avgLatencyMs float64
		if err := rows.Scan(&agent, &r.Calls, &r.InputTokens, &r.OutputTokens, &avgLatencyMs, &r.P50InputTokens, &r.P95InputTokens); err != nil {
			continue
		}
		r.Agent = agent.String
		r.AvgLatency = time.Duration(avgLatencyMs * float64(time.Millisecond)).Round(time.Millisecond)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// QuerySessionInputPercentiles 返回会话全量 LLM 调用的单呼输入 token P50/P95（会话级口径）。
func (s *PostgresStore) QuerySessionInputPercentiles(ctx context.Context, sessionID string) (p50, p95 float64, err error) {
	row := s.db.QueryRowContext(ctx, `
SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY input_tokens), 0),
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY input_tokens), 0)
FROM session_logs
WHERE session_id = $1 AND phase LIKE 'llm%'
`, sessionID)
	if err := row.Scan(&p50, &p95); err != nil {
		return 0, 0, fmt.Errorf("query input percentiles: %w", err)
	}
	return p50, p95, nil
}

// QueryAgentEventStats 按实例聚合 agent_events 的工具调用数与去重交付文件清单。
// WriteFile/EditFile 的 path 从 input 文本正则提取（截断容错），按首次出现去重保序。
func (s *PostgresStore) QueryAgentEventStats(ctx context.Context, sessionID string) ([]*AgentEventStats, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent_id,
       COUNT(*) FILTER (WHERE type = 'tool_call') AS tool_calls,
       COUNT(*) FILTER (WHERE type = 'answer')    AS answers
FROM agent_events
WHERE session_id = $1
GROUP BY agent_id
`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query agent event stats: %w", err)
	}
	defer rows.Close()
	out := make([]*AgentEventStats, 0, 16)
	byID := make(map[string]*AgentEventStats)
	for rows.Next() {
		st := &AgentEventStats{}
		var agentID sql.NullString
		if err := rows.Scan(&agentID, &st.ToolCalls, &st.Answers); err != nil {
			continue
		}
		st.AgentID = agentID.String
		out = append(out, st)
		byID[st.AgentID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 交付文件清单：仅 WriteFile/EditFile 行，input 截 4K 提取 path（足够容纳路径键）。
	fileRows, err := s.db.QueryContext(ctx, `
SELECT agent_id, LEFT(input, 4096)
FROM agent_events
WHERE session_id = $1 AND type = 'tool_call' AND tool_name IN ('WriteFile', 'EditFile')
ORDER BY occurred ASC
`, sessionID)
	if err != nil {
		return out, nil // 文件清单失败不阻断主统计
	}
	defer fileRows.Close()
	seen := make(map[string]map[string]bool)
	for fileRows.Next() {
		var agentID, input string
		if err := fileRows.Scan(&agentID, &input); err != nil {
			continue
		}
		m := pathExtractRegex.FindStringSubmatch(input)
		if len(m) < 2 || m[1] == "" {
			continue
		}
		path := unescapeJSONString(m[1])
		if seen[agentID] == nil {
			seen[agentID] = make(map[string]bool)
		}
		if seen[agentID][path] {
			continue
		}
		seen[agentID][path] = true
		if st := byID[agentID]; st != nil {
			st.WrittenFiles = append(st.WrittenFiles, path)
		}
	}
	return out, fileRows.Err()
}

// QueryAgentEvents 按 session + 实例 ID 取 agent_events 逐轮事件（审计下钻数据源），
// 按时间正序（最旧在前），limit<=0 默认 200，offset 分页。
// output 截断防止单行超大工具输出撑爆响应（全文在 .bma/tool_outputs 落盘体系内）。
func (s *PostgresStore) QueryAgentEvents(ctx context.Context, sessionID, agentID string, limit, offset int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT type, role, content, tool_name, LEFT(input, 2000), LEFT(output, 2000), occurred
FROM agent_events
WHERE session_id = $1 AND agent_id = $2
ORDER BY occurred ASC
LIMIT $3 OFFSET $4
`, sessionID, agentID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query agent events: %w", err)
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var typ, role, content, toolName, input, output string
		var occurred time.Time
		if err := rows.Scan(&typ, &role, &content, &toolName, &input, &output, &occurred); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"type":      typ,
			"role":      role,
			"content":   content,
			"tool_name": toolName,
			"input":     input,
			"output":    output,
			"occurred":  occurred,
		})
	}
	return out, rows.Err()
}

// jsonUnescapeReplacer 还原 JSON 字符串值中的常见转义（路径场景：反斜杠/引号/斜杠）。
var jsonUnescapeReplacer = strings.NewReplacer(
	`\"`, `"`, `\\`, `\`, `\/`, `/`, `\n`, "\n", `\t`, "\t", `\r`, "\r",
)

// unescapeJSONString 还原 JSON 字符串转义序列。
func unescapeJSONString(s string) string {
	return jsonUnescapeReplacer.Replace(s)
}
