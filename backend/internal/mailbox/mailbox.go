// Package mailbox 实现 v3 §7.2 设计的 Agent 异步邮箱。
//
// 设计意图：Agent 之间不直接共享上下文，而是把"事件消息"投递到目标 Agent
// 的邮箱；主 Agent 在调度循环中拉取邮箱，并把每条事件转化为目标 Agent
// 上下文中的一段简短注入。这种异步解耦避免了多 Agent 并发执行时
// 上下文交叉污染。
//
// 实现要点：
//   - 每个 Agent 拥有独立的收件箱队列（inbox map）。
//   - 投递（Send）与拉取（Peek/Drain）均通过 sync.RWMutex 保证线程安全。
//   - 每条消息有 Status 标记，Drain 后置为已读，不再被重复拉取。
//   - 当目标 Agent 不存在（To == "*"）时，消息转入广播桶 bcast，
//     由主 Agent 决议后再 Forward 到具体 Agent。
package mailbox

import (
	"sort"        // 用于按优先级排序消息
	"strconv"     // int64 → string 转换
	"sync"        // 提供 RWMutex 保护并发访问
	"sync/atomic" // 提供原子计数器生成消息 ID
	"time"        // 用于时间戳与 ID 格式化
)

// MessageType 邮件类型，区分 Agent 间事件语义。
type MessageType string

const (
	// MsgMilestone 里程碑达成事件，表示某 Agent 完成关键节点。
	MsgMilestone MessageType = "milestone" // 完成里程碑
	// MsgRequest 协助请求，要求目标 Agent 提供支持。
	MsgRequest MessageType = "request" // 请求协助
	// MsgInfo 普通通知，仅作信息同步，无需响应。
	MsgInfo MessageType = "info" // 普通通知
	// MsgEscalate 升级请求，需要上层 Agent 介入裁决。
	MsgEscalate MessageType = "escalate" // 升级请求
	// MsgDependency 依赖完成事件，通知等待方前置任务已就绪。
	MsgDependency MessageType = "dependency" // 依赖完成事件
)

// Status 邮件状态，用于区分未读/已读，避免重复拉取。
type Status string

const (
	// StatusUnread 未读：消息尚未被目标 Agent 消费。
	StatusUnread Status = "unread"
	// StatusRead 已读：消息已被 Drain 拉取并注入过上下文。
	StatusRead Status = "read"
)

// Message 邮件结构体，描述一次 Agent 间异步事件。
type Message struct {
	// ID 唯一标识，由 Send 自动生成（msg_HHMMSS_seq）。
	ID string `json:"id"`
	// From 发送者 Agent 实例 ID。
	From string `json:"from"` // 发送者 agent 实例 ID
	// To 收件者 Agent 实例 ID；"*" 表示广播，由主 Agent 决议。
	To string `json:"to"` // 收件者 agent 实例 ID（"*" 广播）
	// Type 事件类型，影响接收方的处理策略。
	Type MessageType `json:"type"`
	// Subject 一行摘要，注入上下文时作为标题展示。
	Subject string `json:"subject"` // 一行摘要
	// Body 详情正文，可为空（仅靠 Subject 表意时）。
	Body string `json:"body,omitempty"` // 详情，可空
	// Payload 结构化附加数据，供接收方按需解析。
	Payload map[string]any `json:"payload,omitempty"`
	// Priority 优先级，数字越大越紧急，默认 0；排序时靠前。
	Priority int `json:"priority"` // 数字越大越紧急（默认 0）
	// Status 当前邮件状态（unread/read）。
	Status Status `json:"status"`
	// CreatedAt 创建时间，由 Send 在投递时填充。
	CreatedAt time.Time `json:"created_at"`
	// ReadAt 首次被 Drain 标记为已读的时间；未读时为 nil。
	ReadAt *time.Time `json:"read_at,omitempty"`
}

// Mailbox 多 Agent 邮箱管理器，维护每个 Agent 的收件箱与广播桶。
//
// 字段说明：
//   - mu：读写锁，保护 inbox 与 bcast 的并发访问。
//   - inbox：按 agentID 索引的消息队列，存放定向投递的消息。
//   - bcast：广播桶，存放 To == "*" 的消息，等待主 Agent 决议。
//   - seq：原子计数器，用于生成全局唯一的消息 ID。
//
// 并发安全：所有公开方法均自行加锁，可被多 goroutine 同时调用。
type Mailbox struct {
	mu    sync.RWMutex          // 读写锁：保护 inbox 与 bcast 的并发访问
	inbox map[string][]*Message // agentID -> messages
	bcast []*Message            // To == "*" 等待主 Agent 决议
	seq   atomic.Int64          // 全局递增序号，用于生成消息 ID
}

// New 创建并返回一个新的邮箱管理器实例。
//
// 职责：初始化 inbox map，返回空邮箱。
// 参数：无。
// 返回：*Mailbox，可直接使用。
// 副作用：无。
// 并发安全：构造本身无并发风险。
func New() *Mailbox {
	return &Mailbox{
		inbox: make(map[string][]*Message),
	}
}

// Send 投递一条邮件到目标 Agent 的收件箱或广播桶。
//
// 职责：填充 ID/CreatedAt/Status，并按 To 字段路由消息。
// 参数：
//   - msg：待投递消息指针；若为 nil 直接返回空串。
//
// 返回：消息 ID（若 msg 为 nil 则返回空串）。
// 副作用：
//   - 修改 msg 的 ID/CreatedAt/Status 字段。
//   - 若 To 为空或 "*"，消息进入 bcast；否则进入 inbox[To]。
//
// 并发安全：通过 m.mu 写锁保护 map 写入。
func (m *Mailbox) Send(msg *Message) string {
	// 防御 nil 入参，避免后续解引用 panic。
	if msg == nil {
		return ""
	}
	// 优先复用调用方传入的 ID。
	id := msg.ID
	if id == "" {
		// 未提供 ID 时，用原子自增计数器生成全局唯一序号。
		id = formatID(m.seq.Add(1))
	}
	msg.ID = id
	// 创建时间为零值时，以当前时间填充。
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	// 投递时统一标记为未读，等待接收方拉取。
	msg.Status = StatusUnread

	// 加写锁保护 inbox/bcast 的写入。
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.To == "" || msg.To == "*" {
		// 无明确收件人或广播：进入广播桶，交主 Agent 决议。
		m.bcast = append(m.bcast, msg)
	} else {
		// 定向投递：追加到目标 Agent 的收件箱末尾。
		m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	}
	return id
}

// Peek 拉取目标 Agent 的所有未读消息，但不修改其状态。
//
// 职责：只读视图，供调用方预览待处理消息。
// 参数：
//   - agentID：目标 Agent 实例 ID。
//
// 返回：新建切片，包含所有未读消息，按优先级降序、时间升序排序。
// 副作用：无（不改变 mailbox 内部状态）。
// 并发安全：通过 m.mu 读锁保护并发读取。
func (m *Mailbox) Peek(agentID string) []*Message {
	// 加读锁，允许多 goroutine 并发读取。
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 取出该 Agent 的全部消息引用（可能含已读）。
	src := m.inbox[agentID]
	// 预分配容量，减少扩容拷贝。
	out := make([]*Message, 0, len(src))
	for _, msg := range src {
		// 仅保留未读消息。
		if msg.Status == StatusUnread {
			out = append(out, msg)
		}
	}
	// 按优先级排序后返回。
	sortByPriority(out)
	return out
}

// Drain 拉取目标 Agent 的所有未读消息并将其标记为已读。
//
// 职责：消费式拉取，确保每条消息只被注入上下文一次。
// 参数：
//   - agentID：目标 Agent 实例 ID。
//
// 返回：本次从 unread 翻转为 read 的消息列表，按优先级排序。
// 副作用：
//   - 修改消息的 Status 为 StatusRead。
//   - 设置 ReadAt 为当前时间。
//
// 并发安全：通过 m.mu 写锁保护状态变更。
func (m *Mailbox) Drain(agentID string) []*Message {
	// 加写锁，因为要修改消息状态。
	m.mu.Lock()
	defer m.mu.Unlock()
	return drainMessages(m.inbox[agentID], true)
}

// DrainBroadcast 主 Agent 专用：拉取广播桶中的全部未读消息并标记为已读。
//
// 职责：消费广播桶，交由主 Agent 决议后续 Forward 或丢弃。
// 参数：无。
// 返回：本次从 unread 翻转为 read 的广播消息列表，按优先级排序。
// 副作用：
//   - 修改广播桶中消息的 Status 为 StatusRead。
//   - 设置 ReadAt 为当前时间。
//
// 并发安全：通过 m.mu 写锁保护状态变更。
func (m *Mailbox) DrainBroadcast() []*Message {
	// 加写锁，因为要修改消息状态。
	m.mu.Lock()
	defer m.mu.Unlock()
	return drainMessages(m.bcast, true)
}

// drainMessages 从消息切片中筛选未读消息，可选标记为已读并返回按优先级排序的副本。
//
// 职责：被 Drain / DrainBroadcast 共用，消除重复的状态翻转与排序逻辑。
// 参数：
//   - msgs：待扫描消息切片。
//   - markRead：true 时将未读消息翻转为已读并记录 ReadAt。
//
// 返回：未读消息列表（已排序）。
//
// 并发安全：调用方必须已持有 m.mu 写锁。
func drainMessages(msgs []*Message, markRead bool) []*Message {
	now := time.Now()
	out := make([]*Message, 0, len(msgs))
	for _, msg := range msgs {
		// 只处理未读消息，已读消息跳过。
		if msg.Status != StatusUnread {
			continue
		}
		if markRead {
			msg.Status = StatusRead
			msg.ReadAt = &now
		}
		out = append(out, msg)
	}
	sortByPriority(out)
	return out
}

// Count 返回指定 Agent 当前未读邮件数。
//
// 职责：供 Watchdog 等组件判断是否有积压待处理消息。
// 参数：
//   - agentID：目标 Agent 实例 ID。
//
// 返回：未读消息条数。
// 副作用：无。
// 并发安全：通过 m.mu 读锁保护并发读取。
func (m *Mailbox) Count(agentID string) int {
	// 加读锁，允许并发读取。
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 计数器初值为 0。
	count := 0
	for _, msg := range m.inbox[agentID] {
		// 仅统计未读消息。
		if msg.Status == StatusUnread {
			count++
		}
	}
	return count
}

// Forward 把广播桶中的某条消息转交到具体 Agent 的收件箱。
//
// 职责：主 Agent 对广播消息做决议后，调用此方法投递到真正目标。
// 参数：
//   - msgID：待转发的广播消息 ID。
//   - targetAgent：最终收件 Agent 实例 ID。
//
// 返回：true 表示找到并转发成功；false 表示广播桶中无此 ID。
// 副作用：
//   - 修改消息的 To/Status/ReadAt 字段。
//   - 将消息从 bcast 移除并追加到 inbox[targetAgent]。
//
// 并发安全：通过 m.mu 写锁保护 map 读写。
func (m *Mailbox) Forward(msgID, targetAgent string) bool {
	// 加写锁，因为要同时修改 bcast 与 inbox。
	m.mu.Lock()
	defer m.mu.Unlock()
	// 线性扫描广播桶，定位目标消息。
	for i, msg := range m.bcast {
		if msg.ID == msgID {
			// 更新收件人为最终目标。
			msg.To = targetAgent
			// 重置为未读，等待目标 Agent 拉取。
			msg.Status = StatusUnread
			// 清空历史读取时间。
			msg.ReadAt = nil
			// 追加到目标 Agent 收件箱。
			m.inbox[targetAgent] = append(m.inbox[targetAgent], msg)
			// 从广播桶移除：用切片拼接保持顺序。
			m.bcast = append(m.bcast[:i], m.bcast[i+1:]...)
			return true
		}
	}
	// 未找到对应 ID 的广播消息。
	return false
}

// Purge 清空指定 Agent 的全部邮件（含未读与已读）。
//
// 职责：Agent 销毁或会话结束时回收其收件箱，避免内存泄漏。
// 参数：
//   - agentID：待清空的 Agent 实例 ID。
//
// 返回：无。
// 副作用：从 inbox map 中删除该 key 对应的全部消息。
// 并发安全：通过 m.mu 写锁保护 map 删除。
func (m *Mailbox) Purge(agentID string) {
	// 加写锁，执行 map 删除。
	m.mu.Lock()
	defer m.mu.Unlock()
	// 直接删除 key，消息切片随之被 GC 回收。
	delete(m.inbox, agentID)
}

// sortByPriority 按优先级降序、创建时间升序稳定排序消息。
//
// 职责：让高优先级且更早到达的消息排在前面，供接收方优先处理。
// 参数：
//   - msgs：待排序消息切片（原地修改）。
//
// 返回：无。
// 副作用：改变入参切片元素顺序。
// 并发安全：纯函数式操作，调用方负责加锁。
func sortByPriority(msgs []*Message) {
	// 使用稳定排序，避免同优先级同时间消息乱序。
	sort.SliceStable(msgs, func(i, j int) bool {
		// 优先级不同时，高优先级靠前。
		if msgs[i].Priority != msgs[j].Priority {
			return msgs[i].Priority > msgs[j].Priority
		}
		// 优先级相同时，创建时间早的靠前。
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
}

// formatID 根据序号生成形如 "msg_HHMMSS_seq" 的消息 ID。
//
// 职责：在没有 strconv 依赖的情况下，拼出可读的唯一 ID。
// 参数：
//   - n：全局递增序号。
//
// 返回：消息 ID 字符串。
// 副作用：无。
// 并发安全：无状态，纯函数。
func formatID(n int64) string {
	// 时间部分取 HHMMSS，便于人工识别；序号部分由 strconv.FormatInt 转换。
	return "msg_" + time.Now().Format("150405") + "_" + strconv.FormatInt(n, 10)
}
