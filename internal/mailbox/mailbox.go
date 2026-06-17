// Package mailbox 实现 v3 §7.2 设计的 Agent 异步邮箱。
//
// Agent 之间不直接共享上下文。它们将"事件消息"投递到目标 Agent 的
// 邮箱，主 Agent 在调度循环中拉取邮箱并把每条事件转化为该 Agent
// 上下文中的一段简短注入。这种异步解耦保证了上下文不会因为多
// Agent 并发执行而交叉污染。
//
// 邮箱实现要点：
//   - 每个 Agent 一个独立的"收件箱队列"；
//   - 投递与拉取均是线程安全的；
//   - 每条消息有 Status 标记，已读后不再被重复拉取；
//   - 当目标 Agent 不存在时，消息将转入"广播桶"由主 Agent 决议。
package mailbox

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MessageType 邮件类型
type MessageType string

const (
	MsgMilestone   MessageType = "milestone"    // 完成里程碑
	MsgRequest     MessageType = "request"      // 请求协助
	MsgInfo        MessageType = "info"         // 普通通知
	MsgEscalate    MessageType = "escalate"     // 升级请求
	MsgDependency  MessageType = "dependency"   // 依赖完成事件
)

// Status 邮件状态
type Status string

const (
	StatusUnread Status = "unread"
	StatusRead   Status = "read"
)

// Message 邮件
type Message struct {
	ID        string         `json:"id"`
	From      string         `json:"from"`           // 发送者 agent 实例 ID
	To        string         `json:"to"`             // 收件者 agent 实例 ID（"*" 广播）
	Type      MessageType    `json:"type"`
	Subject   string         `json:"subject"`        // 一行摘要
	Body      string         `json:"body,omitempty"` // 详情，可空
	Payload   map[string]any `json:"payload,omitempty"`
	Priority  int            `json:"priority"`       // 数字越大越紧急（默认 0）
	Status    Status         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	ReadAt    *time.Time     `json:"read_at,omitempty"`
}

// Mailbox 多 Agent 邮箱管理器
type Mailbox struct {
	mu     sync.RWMutex
	inbox  map[string][]*Message // agentID -> messages
	bcast  []*Message            // To == "*" 等待主 Agent 决议
	seq    atomic.Int64
}

// New 创建邮箱
func New() *Mailbox {
	return &Mailbox{
		inbox: make(map[string][]*Message),
	}
}

// Send 投递一条邮件，自动填充 ID/CreatedAt/Status
func (m *Mailbox) Send(msg *Message) string {
	if msg == nil {
		return ""
	}
	id := msg.ID
	if id == "" {
		id = formatID(m.seq.Add(1))
	}
	msg.ID = id
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	msg.Status = StatusUnread

	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.To == "" || msg.To == "*" {
		m.bcast = append(m.bcast, msg)
	} else {
		m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	}
	return id
}

// Peek 拉取目标 Agent 的所有未读消息（不修改状态）
func (m *Mailbox) Peek(agentID string) []*Message {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.inbox[agentID]
	out := make([]*Message, 0, len(src))
	for _, msg := range src {
		if msg.Status == StatusUnread {
			out = append(out, msg)
		}
	}
	sortByPriority(out)
	return out
}

// Drain 拉取并标记为已读，返回 unread → read 的列表
func (m *Mailbox) Drain(agentID string) []*Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.inbox[agentID]
	now := time.Now()
	var out []*Message
	for _, msg := range src {
		if msg.Status == StatusUnread {
			msg.Status = StatusRead
			msg.ReadAt = &now
			out = append(out, msg)
		}
	}
	sortByPriority(out)
	return out
}

// DrainBroadcast 主 Agent 用：拉取广播桶里的全部未读消息
func (m *Mailbox) DrainBroadcast() []*Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []*Message
	for _, msg := range m.bcast {
		if msg.Status == StatusUnread {
			msg.Status = StatusRead
			msg.ReadAt = &now
			out = append(out, msg)
		}
	}
	sortByPriority(out)
	return out
}

// Count 返回某 Agent 当前未读邮件数（用于 Watchdog 判断）
func (m *Mailbox) Count(agentID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, msg := range m.inbox[agentID] {
		if msg.Status == StatusUnread {
			count++
		}
	}
	return count
}

// Forward 把广播消息转交到具体 Agent（主 Agent 决议后调用）
func (m *Mailbox) Forward(msgID, targetAgent string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, msg := range m.bcast {
		if msg.ID == msgID {
			msg.To = targetAgent
			msg.Status = StatusUnread
			msg.ReadAt = nil
			m.inbox[targetAgent] = append(m.inbox[targetAgent], msg)
			// 从广播桶移除
			m.bcast = append(m.bcast[:i], m.bcast[i+1:]...)
			return true
		}
	}
	return false
}

// Purge 清空指定 Agent 的全部邮件（Agent 销毁时调用）
func (m *Mailbox) Purge(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inbox, agentID)
}

// sortByPriority 按 Priority 降序、CreatedAt 升序
func sortByPriority(msgs []*Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].Priority != msgs[j].Priority {
			return msgs[i].Priority > msgs[j].Priority
		}
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
}

func formatID(n int64) string {
	return "msg_" + time.Now().Format("150405") + "_" + itoa(n)
}

// itoa 极简 int64 → string，避免依赖 strconv
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
