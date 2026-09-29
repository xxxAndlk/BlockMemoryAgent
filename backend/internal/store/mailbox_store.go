package store

import (
	"context"      // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql" // 标准库 SQL 抽象层
	"encoding/json"
	"fmt" // 格式化错误信息
	"time"

	"github.com/lib/pq" // pq.Array 展开 IN 子句参数
)

// MailboxMessage 是 mailbox_messages 表的行类型（2026-09-28 mailbox 持久化）。
// 与 internal/mailbox.Message 字段一一对应；store 层不 import mailbox（分层纪律：
// 基础设施不 import runtime 组件），转换适配器在 bootstrap。
type MailboxMessage struct {
	ID        string
	Owner     string
	SessionID string
	FromAgent string
	ToAgent   string
	Type      string
	Subject   string
	Body      string
	Payload   []byte // JSONB 原文（json.Marshal 后的字节）；nil 落 NULL
	Priority  int
	ReplyTo   string
	ThreadID  string
	CreatedAt time.Time
}

// MailboxStore 持久化 Agent 间邮件：Send 双写、Drain 标已读、Purge 标死信、
// 重启后 LoadUnread 重投。append-only：状态翻转只 UPDATE status/read_at，不删行
//（物理删除仅会话级联 DeleteSessionData）。
type MailboxStore struct {
	db  *sql.DB
	log Logger // 结构化日志器；nil 时静默（写路径错误经返回值上抛）
}

// NewMailboxStore 创建邮箱持久化存储。
func NewMailboxStore(db *sql.DB) *MailboxStore {
	return &MailboxStore{db: db}
}

// Save 落库一条邮件（幂等：ON CONFLICT (id) DO NOTHING，恢复重投/重复投递安全）。
func (s *MailboxStore) Save(ctx context.Context, m MailboxMessage) error {
	if m.ID == "" {
		return fmt.Errorf("mailbox message id required")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mailbox_messages
    (id, owner, session_id, from_agent, to_agent, type, subject, body, payload,
     priority, status, reply_to, thread_id, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'unread',$11,$12,$13)
ON CONFLICT (id) DO NOTHING`,
		m.ID, m.Owner, m.SessionID, m.FromAgent, m.ToAgent, m.Type,
		sanitizeUTF8(m.Subject), sanitizeUTF8(m.Body), m.Payload,
		m.Priority, m.ReplyTo, m.ThreadID, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("save mailbox message %s: %w", m.ID, err)
	}
	return nil
}

// MarkRead 批量翻转已读（Drain 消费后）；空 ids 直通。
func (s *MailboxStore) MarkRead(ctx context.Context, ids []string, readAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE mailbox_messages SET status='read', read_at=$2
WHERE id = ANY($1) AND status='unread'`, pq.Array(ids), readAt)
	if err != nil {
		return fmt.Errorf("mark mailbox read: %w", err)
	}
	return nil
}

// MarkDead 批量标记死信（Purge 丢弃未读/会话终结）；append-only，行保留可查。
func (s *MailboxStore) MarkDead(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE mailbox_messages SET status='dead', read_at=NOW()
WHERE id = ANY($1) AND status='unread'`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("mark mailbox dead: %w", err)
	}
	return nil
}

// LoadUnread 取会话全部未读邮件（按创建时间升序），供崩溃恢复重投。
func (s *MailboxStore) LoadUnread(ctx context.Context, sessionID string) ([]MailboxMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner, from_agent, to_agent, type, subject, body, payload, priority,
       COALESCE(reply_to,''), COALESCE(thread_id,''), created_at
FROM mailbox_messages
WHERE session_id = $1 AND status = 'unread'
ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load unread mailbox: %w", err)
	}
	defer rows.Close()
	var out []MailboxMessage
	for rows.Next() {
		var m MailboxMessage
		m.SessionID = sessionID
		if err := rows.Scan(&m.ID, &m.Owner, &m.FromAgent, &m.ToAgent, &m.Type,
			&m.Subject, &m.Body, &m.Payload, &m.Priority, &m.ReplyTo, &m.ThreadID,
			&m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan mailbox message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MailboxPayloadToJSON 供 bootstrap 适配器把 map 载荷转 JSONB 字节；nil/空 map 返回 nil。
func MailboxPayloadToJSON(payload map[string]any) []byte {
	if len(payload) == 0 {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return b
}
