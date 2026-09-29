package store

import (
	"context"      // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql" // 标准库 SQL 抽象层
	"fmt"          // 格式化错误信息
)

// BoardStore 持久化会话任务看板（2026-09-28 P1）：整板快照 JSONB 写穿，
// 重启后 Manager.GetOrCreate 惰性恢复。快照编解码（board.Snapshot ↔ JSONB）
// 在 bootstrap 适配器——store 层只认 []byte（分层纪律：不 import board）。
type BoardStore struct {
	db *sql.DB
}

// NewBoardStore 创建看板持久化存储。
func NewBoardStore(db *sql.DB) *BoardStore {
	return &BoardStore{db: db}
}

// SaveBoard 整板 UPSERT（幂等；看板变更低频，每次 mutator 后整板覆盖）。
func (s *BoardStore) SaveBoard(ctx context.Context, sessionID, goal string, snapshot []byte) error {
	if sessionID == "" {
		return fmt.Errorf("board session_id required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_boards (owner, session_id, goal, snapshot, updated_at)
VALUES ('', $1, $2, $3, NOW())
ON CONFLICT (session_id) DO UPDATE
SET goal = EXCLUDED.goal, snapshot = EXCLUDED.snapshot, updated_at = NOW()`,
		sessionID, goal, snapshot)
	if err != nil {
		return fmt.Errorf("save board %s: %w", sessionID, err)
	}
	return nil
}

// LoadBoard 读会话看板快照；无记录 found=false（err=nil）。
func (s *BoardStore) LoadBoard(ctx context.Context, sessionID string) (goal string, snapshot []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT goal, snapshot FROM session_boards WHERE session_id = $1`, sessionID).Scan(&goal, &snapshot)
	if err == sql.ErrNoRows {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("load board %s: %w", sessionID, err)
	}
	return goal, snapshot, true, nil
}
