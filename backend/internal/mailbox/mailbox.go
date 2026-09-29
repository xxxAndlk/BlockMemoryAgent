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
//   - 收件人必须显式指定（To 空/"*" 一律拒收）：广播桶曾长期无消费方，
//     消息永久堆积内存，已删除（2026-09-28）。
package mailbox

import (
	"sort"        // 用于按优先级排序消息
	"strconv"     // int64 → string 转换
	"sync"        // 提供 RWMutex 保护并发访问
	"sync/atomic" // 提供原子计数器生成消息 ID
	"time"        // 用于时间戳与 ID 格式化

	"errors" // errors 定义死信哨兵 ErrRecipientClosed
	"fmt"    // fmt 包装死信错误
)

// MessageType 邮件类型，区分 Agent 间事件语义。
type MessageType string

const (
	// MsgMilestone 里程碑达成事件，表示某 Agent 完成关键节点。
	// 预留类型：当前无发送方/消费方（#23 escalate 落地后仍无场景则删除）。
	MsgMilestone MessageType = "milestone" // 完成里程碑
	// MsgRequest 协助请求，要求目标 Agent 提供支持。
	MsgRequest MessageType = "request" // 请求协助
	// MsgInfo 普通通知，仅作信息同步，无需响应。
	MsgInfo MessageType = "info" // 普通通知
	// MsgEscalate 升级请求，需要上层 Agent 介入裁决。
	// 消费方：#21 verifyloop 验证未通过时经此通知父 Agent（verifiers.go）。
	MsgEscalate MessageType = "escalate" // 升级请求
	// MsgDependency 依赖完成事件，通知等待方前置任务已就绪。
	// 预留类型：当前无发送方/消费方（#22 board 依赖门不依赖 mailbox 事件，靠轮询树状态）。
	MsgDependency MessageType = "dependency" // 依赖完成事件
	// MsgReply 是对先前 MsgRequest 的回复：ReplyTo 字段指向原请求消息 ID，
	// 用于多 Agent 验证闭环中"被询问方回复"的请求-响应配对。
	MsgReply MessageType = "reply" // 对 request 的回复
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
	// To 收件者 Agent 实例 ID；必须显式指定，空/"*" 一律拒收（广播已删除）。
	To string `json:"to"` // 收件者 agent 实例 ID
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
	// ReplyTo 被回复消息的 ID：用于 MsgReply 类型，指向先前 MsgRequest 的 ID，
	// 使请求-响应可配对跟踪。空表示该消息不是回复。
	ReplyTo string `json:"reply_to,omitempty"`
	// ThreadID 会话线程标识：同一问答链上的消息共享 ThreadID，
	// 便于多轮验证闭环中按线程聚合请求与回复。
	ThreadID string `json:"thread_id,omitempty"`
	// FilesModified 子 Agent 本次运行修改的文件路径列表（Layer 5）。
	// 由 dispatcher.notify 从子 Agent result.History 扫 WriteFile 工具调用收集，
	// 父 drainMailbox 时展示给父 LLM，使其知晓子改了哪些文件（非 KV 失效--Layer 2 已在写时 per-path 失效）。
	FilesModified []string `json:"files_modified,omitempty"`
}

// ErrRecipientClosed 是投递给已销毁（Purge 过）收件人的死信错误。
// 发送方（send_message 工具）据此返回"消息未送达"，消除发给已死 Agent 的消息静默消失。
var ErrRecipientClosed = errors.New("mailbox: recipient closed")

// Mailbox 多 Agent 邮箱管理器，维护每个 Agent 的收件箱。
//
// 字段说明：
//   - mu：读写锁，保护 inbox 的并发访问。
//   - inbox：按 agentID 索引的消息队列，存放定向投递的消息。
//   - closed：已销毁（Purge 过）的收件人集合，向其 Send 返回 ErrRecipientClosed（死信可见）。
//   - seq：原子计数器，用于生成全局唯一的消息 ID。
//
// 并发安全：所有公开方法均自行加锁，可被多 goroutine 同时调用。
type Mailbox struct {
	mu     sync.RWMutex          // 读写锁：保护 inbox 的并发访问
	inbox  map[string][]*Message // agentID -> messages
	closed map[string]struct{}   // 已销毁收件人（Purge 过），Send 死信
	seq    atomic.Int64          // 全局递增序号，用于生成消息 ID
	// trace 可选的发送留痕回调（编排页 Agent 间交互留痕数据源）：Send 投递成功后持锁外调用；
	// nil 时零行为。实现方必须 best-effort 非阻塞。
	trace func(*Message)
	// persist 可选的持久化回调（mailbox_messages 表双写）：Send 入箱后持锁外同步调用——
	// 同步是为把"内存有、PG 没有"的崩溃丢失窗口压到最小；实现方 fail-open（失败仅日志）。
	persist func(*Message)
	// readMark 可选的已读标记回调：Drain 翻转后持锁外调用（异步批量落库即可）。
	readMark func(ids []string)
	// deadMark 可选的死信标记回调：Purge 丢弃未读消息时持锁外调用。
	deadMark func(ids []string)
	// restoreHook 可选的恢复回调：Restore 重投后持锁外调用（Task 4 接 pending 重建）。
	restoreHook func(*Message)
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
		inbox:  make(map[string][]*Message),
		closed: make(map[string]struct{}),
	}
}

// WithTrace 注入发送留痕回调（编排页"Agent 间交互留痕"数据源）。重复注入后者覆盖前者。
func (m *Mailbox) WithTrace(fn func(*Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trace = fn
}

// WithPersist 注入持久化回调（bootstrap 接 store.MailboxStore.Save）。重复注入后者覆盖。
func (m *Mailbox) WithPersist(fn func(*Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persist = fn
}

// WithReadMarker 注入已读标记回调（bootstrap 接 store.MailboxStore.MarkRead）。
func (m *Mailbox) WithReadMarker(fn func(ids []string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readMark = fn
}

// WithDeadMarker 注入死信标记回调（bootstrap 接 store.MailboxStore.MarkDead）。
func (m *Mailbox) WithDeadMarker(fn func(ids []string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadMark = fn
}

// WithRestoreHandler 注入恢复重投回调（bootstrap 接 Dispatcher.RegisterRestoredPending）。
func (m *Mailbox) WithRestoreHandler(fn func(*Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreHook = fn
}

// Send 投递一条邮件到目标 Agent 的收件箱。
//
// 职责：填充 ID/CreatedAt/Status，并定向投递。收件人必须显式指定：
// To 空/"*" 返回校验错误（广播桶已无消费方，2026-09-28 删除，防消息永久堆积）。
// request/escalate 且未填 ThreadID 时自动回填为消息 ID（问答链配对锚点：
// 回复方凭来信注入里的 id/thread 填 reply_to 即可配对，见 mailboxMessageToReact）。
// 死信可见：定向收件人已销毁（Purge 过）时返回 ErrRecipientClosed。
// 持久化：入箱成功后持锁外同步调 persist hook（fail-open，见字段注释）。
// 返回：消息 ID 与投递错误（nil 表示已入箱；msg 为 nil 返回错误）。
func (m *Mailbox) Send(msg *Message) (string, error) {
	// 防御 nil 入参，避免后续解引用 panic。
	if msg == nil {
		return "", errors.New("mailbox: nil message")
	}
	// 收件人必须显式指定：广播语义无消费方（DrainBroadcast/Forward 已删），空收件人等同丢失。
	if msg.To == "" || msg.To == "*" {
		return "", fmt.Errorf("mailbox: recipient required (broadcast removed): from=%s subject=%q", msg.From, msg.Subject)
	}
	// 优先复用调用方传入的 ID（Restore 重投保留原 ID，ON CONFLICT 幂等）。
	id := msg.ID
	if id == "" {
		id = formatID(m.seq.Add(1))
	}
	msg.ID = id
	// 创建时间为零值时，以当前时间填充。
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	// 投递时统一标记为未读，等待接收方拉取。
	msg.Status = StatusUnread
	// request/escalate 缺省 ThreadID 回填为消息 ID：问答链以首问 ID 为根，
	// 回复方只填 reply_to 即可隐式入链（thread_id 沿用纪律不变）。
	if (msg.Type == MsgRequest || msg.Type == MsgEscalate) && msg.ThreadID == "" {
		msg.ThreadID = id
	}

	// 加写锁保护 inbox 的写入。
	m.mu.Lock()
	// 定向投递：目标已销毁则返回死信错误，不入箱。
	if _, ok := m.closed[msg.To]; ok {
		m.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrRecipientClosed, msg.To)
	}
	m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	trace := m.trace
	persist := m.persist
	m.mu.Unlock()
	// 投递成功后留痕/落库（持锁外 + 值拷贝：防回调慢/再入 mailbox 死锁，防调用方后续改 msg）。
	if trace != nil {
		cp := *msg
		trace(&cp)
	}
	if persist != nil {
		cp := *msg
		persist(&cp)
	}
	return id, nil
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
// 持久化：本次翻转的消息 ID 持锁外回调 readMark（批量落库标记，崩溃最晚丢到下次
// 翻转前——恢复侧 LoadUnread 会把它们当未读重投一次，注入侧幂等消化）。
// 其余语义不变（优先级降序+时间升序）。
func (m *Mailbox) Drain(agentID string) []*Message {
	m.mu.Lock()
	out := drainMessages(m.inbox[agentID], true)
	readMark := m.readMark
	m.mu.Unlock()
	if readMark != nil && len(out) > 0 {
		ids := make([]string, 0, len(out))
		for _, msg := range out {
			ids = append(ids, msg.ID)
		}
		readMark(ids)
	}
	return out
}

// drainMessages 从消息切片中筛选未读消息，可选标记为已读并返回按优先级排序的副本。
//
// 职责：Drain 的状态翻转与排序逻辑。
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

// Purge 清空指定 Agent 的全部邮件（含未读与已读），返回被丢弃的**未读**消息。
//
// 职责：Agent 销毁或会话结束时回收其收件箱，避免内存泄漏。
// 死信可见（2026-09-28）：未读消息随 Purge 丢弃前，先回调 deadMark（PG 标 dead），
// 并把清单返回给调用方——dispatcher 据此给 request/escalate 的发送方回投"未送达"
// 通知，消除"发送方拿到成功、消息却被静默清掉"的假投递。
// 并发安全：通过 m.mu 写锁保护 map 删除。
func (m *Mailbox) Purge(agentID string) []*Message {
	m.mu.Lock()
	var dropped []*Message
	var deadMark func([]string)
	for _, msg := range m.inbox[agentID] {
		if msg.Status == StatusUnread {
			dropped = append(dropped, msg)
		}
	}
	delete(m.inbox, agentID)
	// 标记收件人已销毁：此后 Send 至该 ID 返回死信错误。
	m.closed[agentID] = struct{}{}
	if len(dropped) > 0 {
		deadMark = m.deadMark
	}
	m.mu.Unlock()
	if deadMark != nil {
		ids := make([]string, 0, len(dropped))
		for _, msg := range dropped {
			ids = append(ids, msg.ID)
		}
		deadMark(ids)
	}
	return dropped
}

// Reopen 重新打开一个已被 Purge 的收件人（编排页"复活重跑"）：撤销 closed 标记。
//
// Purge 的语义是"该实例已终结、后续投递为死信"；复活沿用**同一个实例 ID** 重跑，
// 若不撤销，新起子 Agent 的回传（notify→Send）与用户直连注入会全部命中死信而被丢弃
// （子任务结果静默消失、wait loop 空手退出）。只清标记，历史消息不恢复。
func (m *Mailbox) Reopen(agentID string) {
	if agentID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.closed, agentID)
}

// Restore 崩溃恢复重投（2026-09-28 mailbox_messages 持久化）：把 PG LoadUnread 读回的
// 未读消息按原 ID 重新入箱，等接收方（复活的 meta/热驻槽/用户续跑）自然 Drain。
//
// 与 Send 的差异：不触发 trace/persist（PG 行已存在，重投只是重建内存态）；
// 触发 restoreHook（dispatcher 据此重建 pending 问答注册表）；
// 同 ID 幂等（已在箱不重复入箱，恢复路径可安全重入）。
// 收件人已 closed（Purge 过）时先撤销标记再入箱（恢复语义等价 Reopen）。
func (m *Mailbox) Restore(msg *Message) error {
	if msg == nil || msg.ID == "" {
		return errors.New("mailbox: restore requires message with ID")
	}
	if msg.To == "" || msg.To == "*" {
		return fmt.Errorf("mailbox: restore recipient required: id=%s", msg.ID)
	}
	msg.Status = StatusUnread
	msg.ReadAt = nil
	m.mu.Lock()
	delete(m.closed, msg.To)
	dup := false
	for _, ex := range m.inbox[msg.To] {
		if ex.ID == msg.ID {
			dup = true
			break
		}
	}
	if !dup {
		m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	}
	hook := m.restoreHook
	m.mu.Unlock()
	if hook != nil && !dup {
		cp := *msg
		hook(&cp)
	}
	return nil
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
