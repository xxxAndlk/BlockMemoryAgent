package store

// agent_mail_trace.go mailbox 留痕查询（编排页"交互留痕"数据源）：
// 读 agent_events 中 type='mailbox' 的行（bootstrap 的 mailbox trace 钩子双写）。

import (
	"context"
	"fmt"
	"time"
)

// QueryAgentMailboxTrace 取某 Agent 最近 limit 条 mailbox 留痕事件（返回按时间正序）。
// 字段口径：role=发送方, content=subject\nbody, tool_name=邮件类型, input=接收方。
// limit<=0 默认 100。
//
// 取的是"最近"而非"最早"：每封邮件落 1-2 行，长会话轻易超过 limit，按 ASC 取会永远
// 停在会话开头的旧邮件上（用户直连/复活通知/最新回传全部看不到）。SQL 取 DESC 后
// 在内存反转，保持调用方"升序"口径不变。
func (s *PostgresStore) QueryAgentMailboxTrace(ctx context.Context, sessionID, agentID string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT role, content, tool_name, input, occurred
FROM agent_events
WHERE session_id = $1 AND agent_id = $2 AND type = 'mailbox'
ORDER BY occurred DESC
LIMIT $3
`, sessionID, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("query agent mailbox trace: %w", err)
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var from, content, typ, to string
		var occurred time.Time
		if err := rows.Scan(&from, &content, &typ, &to, &occurred); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"role":      from,
			"content":   content,
			"tool_name": typ,
			"input":     to,
			"occurred":  occurred,
		})
	}
	// DESC 取出后反转回升序（调用方/前端按"倒序展示最新在上"消费）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// QuerySessionMailboxTrace 取整个会话的 mailbox 留痕（全部 Agent 的收发，按时间正序）。
// 供主对话栏展示"上级 ↔ 下级"的往来（父侧视角此前完全看不到邮件）。
//
// 去重：留痕钩子对每封邮件写**两行**（收发双方 agent_id 各一条，字段完全一致），
// 会话级查询会把它们都捞出来——DISTINCT 掉重复行，否则同一封邮件在界面上出现两次。
// DISTINCT 在 LIMIT 之前生效（ORDER BY occurred DESC → 取最近 limit 封 → 内存反转升序）。
func (s *PostgresStore) QuerySessionMailboxTrace(ctx context.Context, sessionID string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT role, content, tool_name, input, occurred
FROM agent_events
WHERE session_id = $1 AND type = 'mailbox'
ORDER BY occurred DESC
LIMIT $2
`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query session mailbox trace: %w", err)
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var from, content, typ, to string
		var occurred time.Time
		if err := rows.Scan(&from, &content, &typ, &to, &occurred); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"role":      from,
			"content":   content,
			"tool_name": typ,
			"input":     to,
			"occurred":  occurred,
		})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
