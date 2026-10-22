package store

// session_decisions.go —— 会话决策账本（2026-10-22 上下文事故修复）。
//
// 背景：历史压缩摘要里的【已定决策】会随 400 条滑动窗口被整段裁掉；用户经 ask_user
// 给出的关键指令（"帧渲染是正确做法""要到这种类型"）只作为 tool result 存在，压缩器
// 按工具输出处理。结果是正确指令在上下文里权重最低、最早消失，而陈旧的首条目标
// （session.Goal）与带时间戳的偏好行每轮全文注入，模型反复走错方向。
//
// 账本把"用户说过什么"从对话历史里独立出来：append-only、带 owner、按会话查询，
// 由 Agent 服务每轮以固定 system 块注入，不参与压缩、不被摘要覆盖。
// 状态只做 UPDATE（active/superseded/rejected），不物理删行（底账 append-only 纪律）。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// SessionDecision 是账本中的一条用户决策/指令/基准/否决记录。
type SessionDecision struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Owner     string    `json:"owner"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// 账本行类型。
const (
	DecisionKindDirective = "user_directive" // 普通用户指令（含目标）
	DecisionKindRoute     = "route"          // 路线级指示（渲染方案/技术选型等互斥选择）
	DecisionKindClarify   = "clarify_answer" // ask_user 答复（用户手输的关键裁决）
	DecisionKindPlan      = "plan"           // 已批准的执行计划（历史记录，不作现行路线）
	DecisionKindReference = "reference"      // 用户给的参考产物/验收基准
	DecisionKindRejection = "rejection"      // 用户明确否定当前成果/路线
)

// 账本行状态。
const (
	DecisionStatusActive     = "active"
	DecisionStatusSuperseded = "superseded"
	DecisionStatusRejected   = "rejected"
)

// SessionDecisionStore 持久化会话决策账本。
type SessionDecisionStore struct {
	db *sql.DB
}

// NewSessionDecisionStore 创建账本存储。
func NewSessionDecisionStore(db *sql.DB) *SessionDecisionStore {
	return &SessionDecisionStore{db: db}
}

// Append 追加一条账本行，返回自增 ID。append-only：重复内容由调用方去重。
func (s *SessionDecisionStore) Append(ctx context.Context, d SessionDecision) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if d.SessionID == "" {
		return 0, fmt.Errorf("decision session_id required")
	}
	if d.Status == "" {
		d.Status = DecisionStatusActive
	}
	if d.Kind == "" {
		d.Kind = DecisionKindDirective
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO session_decisions (session_id, owner, kind, content, source, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`,
		d.SessionID, d.Owner, d.Kind, d.Content, d.Source, d.Status).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("append decision %s: %w", d.SessionID, err)
	}
	return id, nil
}

// List 按时间正序返回会话账本（limit<=0 不限制）。owner 过滤留空=不限制（当前单租户）。
func (s *SessionDecisionStore) List(ctx context.Context, sessionID string, limit int) ([]SessionDecision, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, nil
	}
	q := `SELECT id, session_id, owner, kind, content, source, status, created_at
FROM session_decisions WHERE session_id = $1 ORDER BY id ASC`
	args := []any{sessionID}
	if limit > 0 {
		// 只取最近 limit 条，再翻回时间正序。
		q = `SELECT id, session_id, owner, kind, content, source, status, created_at FROM (
    SELECT id, session_id, owner, kind, content, source, status, created_at
    FROM session_decisions WHERE session_id = $1 ORDER BY id DESC LIMIT $2
) t ORDER BY id ASC`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []SessionDecision
	for rows.Next() {
		var d SessionDecision
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Owner, &d.Kind, &d.Content, &d.Source, &d.Status, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Supersede 把会话内指定 kind 的 active 行标记为 superseded（保留历史，不物理删）。
// keepID>0 时保留该行不动（新写入的现行路线）。返回受影响行数。
func (s *SessionDecisionStore) Supersede(ctx context.Context, sessionID string, kinds []string, keepID int64) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || len(kinds) == 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE session_decisions
SET status = $2, superseded_by = $3
WHERE session_id = $1 AND status = $4 AND kind = ANY($5) AND id <> $3`,
		sessionID, DecisionStatusSuperseded, keepID, DecisionStatusActive, pq.Array(kinds))
	if err != nil {
		return 0, fmt.Errorf("supersede decisions %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MarkRejected 把会话内指定 kind 的 active 行标记为 rejected（用户明确否决）。
func (s *SessionDecisionStore) MarkRejected(ctx context.Context, sessionID string, kinds []string) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || len(kinds) == 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE session_decisions SET status = $2
WHERE session_id = $1 AND status = $3 AND kind = ANY($4)`,
		sessionID, DecisionStatusRejected, DecisionStatusActive, pq.Array(kinds))
	if err != nil {
		return 0, fmt.Errorf("reject decisions %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
