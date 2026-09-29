# 多 Agent 可靠性改造实施计划（mailbox 持久化/配对 + 并发池 + 看板持久化）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复多 agent P0（mailbox 消息丢失/无配对/广播死代码）与 P1（无全局并发上限、看板不持久化），设计依据 `docs/superpowers/specs/2026-09-28-multiagent-reliability-design.md`。

**Architecture:** mailbox 包加 persist/read/dead/restore 四类 hook（包本身不依赖 store），bootstrap 适配到 PG 新表；并发池是自研 FIFO 票据队列（chan 信号量 + container/list），只做准入控制，Tree 仍是生命周期唯一权威；看板整板快照 JSONB 写穿，Manager 挂 Store 接口惰性恢复。

**Tech Stack:** Go 1.25、`database/sql` + `lib/pq`（无 sqlx）、pgvector、GOTOOLCHAIN=local。

## Global Constraints

- 所有 `go build`/`go test` 命令必须带 `GOTOOLCHAIN=local`；工作目录 `D:/data/project/BlockMemoryAgent`，backend 命令在 `backend/` 下跑。
- Windows Git Bash：Unix 语法（`/dev/null` 非 `NUL`，路径正斜杠）。
- **不执行任何 git 变更操作**（commit/add/push 等）——仓库纪律要求用户显式批准。每个 Task 以「构建 + 相关测试通过」为完成判据，结尾向用户报告变更文件清单。
- 新建表必须带 `owner VARCHAR(64) NOT NULL DEFAULT ''` 列；append-only 纪律：物理删除只允许出现在 `sessionScopedTables` 会话级联清单。
- 包分层：`internal/store`（基础设施）不得 import `internal/board`、`internal/mailbox`、`internal/domain/*`（runtime 组件/domain）。store 层新代码只用 `[]byte`/基础类型出参，board/mailbox 类型适配器放 `bootstrap`。
- 提示词语义改动必须 bump `backend/pkg/prompts/prompts.go` 的 `Version` 并记 `doc/变更.md`。
- **命名冲突警告**：`Dispatcher.pool` 字段已被热驻 idle 池占用（idle_pool.go），并发池字段名一律用 `concPool`。
- 代码注释风格跟随现有代码（中文、密集、注明实证/出处）；乱码注释不新增。
- 每个 Task 完成后运行 `cd backend && GOTOOLCHAIN=local go build ./...` 确认编译通过。

---
### Task 1: mailbox 包改造（持久化 hook + Purge 返回未读 + 删广播 + Send 校验 + Restore）

**Files:**
- Modify: `backend/internal/mailbox/mailbox.go`（全文 409 行，逐函数改）
- Test: `backend/internal/mailbox/mailbox_test.go`（已存在，删广播相关测试，加新测试）

**Interfaces:**
- Produces（后续任务依赖的签名）：
  - `func (m *Mailbox) WithPersist(fn func(*Message))` — Send 成功入箱后持锁外同步调用
  - `func (m *Mailbox) WithReadMarker(fn func(ids []string))` — Drain 翻转已读后持锁外调用
  - `func (m *Mailbox) WithDeadMarker(fn func(ids []string))` — Purge 丢弃未读时持锁外调用
  - `func (m *Mailbox) WithRestoreHandler(fn func(*Message))` — Restore 入箱后持锁外调用（Task 4 接 dispatcher）
  - `func (m *Mailbox) Restore(msg *Message) error` — 崩溃恢复重投：保留原 ID/字段入箱，不触发 trace/persist，只触发 restoreHandler
  - `func (m *Mailbox) Purge(agentID string) []*Message` — 签名变更：返回被丢弃的未读消息（已读不返回）
  - `Send` 行为变更：`To==""` 或 `"*"` 返回校验错误；request/escalate 且 ThreadID 为空时回填 `ThreadID = 消息 ID`

- [x] **Step 1: 先删广播死代码的测试，写新行为失败测试**

先定位并删除广播相关测试（生产代码无调用方，已确认）：

Run: `grep -n "Broadcast\|Forward\|bcast" backend/internal/mailbox/mailbox_test.go`
动作：删除命中广播语义的测试函数（保留定向投递/死信/排序等其余测试）。

在 `backend/internal/mailbox/mailbox_test.go` 追加：

```go
// Send 拒绝空/广播收件人（广播桶已删除，防止消息永久堆积无人消费）。
func TestSendRejectsBroadcast(t *testing.T) {
	m := New()
	if _, err := m.Send(&Message{From: "a", To: "", Type: MsgInfo, Subject: "x"}); err == nil {
		t.Fatal("To 为空应返回错误")
	}
	if _, err := m.Send(&Message{From: "a", To: "*", Type: MsgInfo, Subject: "x"}); err == nil {
		t.Fatal("To=* 应返回错误")
	}
}

// request/escalate 且未填 thread_id 时，Send 自动回填 ThreadID=消息 ID（问答链配对锚点）。
func TestSendAutoThreadID(t *testing.T) {
	m := New()
	msg := &Message{From: "a", To: "b", Type: MsgRequest, Subject: "问"}
	id, err := m.Send(msg)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ThreadID != id {
		t.Fatalf("request 未回填 ThreadID=id：got %q want %q", msg.ThreadID, id)
	}
	// info 不回填；显式 thread_id 不被覆盖。
	info := &Message{From: "a", To: "b", Type: MsgInfo, Subject: "报"}
	if _, err := m.Send(info); err != nil {
		t.Fatal(err)
	}
	if info.ThreadID != "" {
		t.Fatalf("info 不应回填 ThreadID：got %q", info.ThreadID)
	}
	explicit := &Message{From: "a", To: "b", Type: MsgRequest, Subject: "问2", ThreadID: "t-fixed"}
	if _, err := m.Send(explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.ThreadID != "t-fixed" {
		t.Fatalf("显式 ThreadID 被覆盖：got %q", explicit.ThreadID)
	}
}

// Purge 返回被丢弃的未读消息（死信通知数据源），已读不在其列。
func TestPurgeReturnsDroppedUnread(t *testing.T) {
	m := New()
	mb := New()
	_ = mb // 防误用
	if _, err := m.Send(&Message{From: "x", To: "a", Type: MsgRequest, Subject: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(&Message{From: "x", To: "a", Type: MsgInfo, Subject: "i1"}); err != nil {
		t.Fatal(err)
	}
	// drain 掉第一条使其已读，第二条保持未读。
	m.Drain("a")
	if _, err := m.Send(&Message{From: "y", To: "a", Type: MsgRequest, Subject: "r2"}); err != nil {
		t.Fatal(err)
	}
	dropped := m.Purge("a")
	if len(dropped) != 1 || dropped[0].Subject != "r2" {
		t.Fatalf("Purge 应只返回未读 r2：got %+v", dropped)
	}
	if m.Count("a") != 0 {
		t.Fatal("Purge 后收件箱应为空")
	}
}

// 四类 hook 触发时机。
func TestMailboxHooks(t *testing.T) {
	m := New()
	var persisted, readMarked, deadMarked, restored []string
	m.WithPersist(func(msg *Message) { persisted = append(persisted, msg.ID) })
	m.WithReadMarker(func(ids []string) { readMarked = append(readMarked, ids...) })
	m.WithDeadMarker(func(ids []string) { deadMarked = append(deadMarked, ids...) })
	m.WithRestoreHandler(func(msg *Message) { restored = append(restored, msg.ID) })

	id, err := m.Send(&Message{From: "x", To: "a", Type: MsgRequest, Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0] != id {
		t.Fatalf("persist hook 未触发：%v", persisted)
	}
	m.Drain("a")
	if len(readMarked) != 1 || readMarked[0] != id {
		t.Fatalf("readMark hook 未触发：%v", readMarked)
	}
	id2, _ := m.Send(&Message{From: "x", To: "a", Type: MsgInfo, Subject: "s2"})
	m.Purge("a")
	if len(deadMarked) != 1 || deadMarked[0] != id2 {
		t.Fatalf("deadMark hook 应只含未读 id2：%v", deadMarked)
	}
	// Restore：保留原 ID 入箱、不触发 persist/trace、触发 restoreHandler、可被 Drain 读到。
	persisted = nil
	m.Reopen("a") // 清上面的 closed 标记
	err = m.Restore(&Message{ID: "msg_restore_1", From: "x", To: "a", Type: MsgRequest, Subject: "rs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 0 {
		t.Fatalf("Restore 不应触发 persist：%v", persisted)
	}
	if len(restored) != 1 || restored[0] != "msg_restore_1" {
		t.Fatalf("restoreHandler 未触发：%v", restored)
	}
	got := m.Drain("a")
	if len(got) != 1 || got[0].ID != "msg_restore_1" {
		t.Fatalf("Restore 后 Drain 应读到原 ID 消息：%+v", got)
	}
	// Restore 幂等：同 ID 重复恢复不产生第二条。
	if err := m.Restore(&Message{ID: "msg_restore_1", From: "x", To: "a", Type: MsgRequest, Subject: "rs"}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.Drain("a")); n != 0 {
		t.Fatalf("同 ID Restore 应幂等去重，Drain 应空，got %d", n)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/mailbox/ -run 'TestSendRejectsBroadcast|TestSendAutoThreadID|TestPurgeReturnsDroppedUnread|TestMailboxHooks' -v`
Expected: FAIL（编译错误：Restore/WithPersist 等不存在；Purge 无返回值）

- [x] **Step 3: 实现 mailbox.go 改造**

对 `backend/internal/mailbox/mailbox.go` 做如下修改（未列出的函数不动）：

(a) 包注释第 12-13 行（`当目标 Agent 不存在（To == "*"）时…转入广播桶`）整段删除，改为：

```go
//   - 每条消息有 Status 标记，Drain 后置为已读，不再被重复拉取。
//   - 收件人必须显式指定（To 空/"*" 一律拒收）：广播桶曾长期无消费方，
//     消息永久堆积内存，已删除（2026-09-28）。
```

(b) `Mailbox` struct（约 109-118 行）：删 `bcast` 字段，新增四个 hook 字段：

```go
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
```

(c) 新增四个 With  setter（紧跟 `WithTrace` 之后），风格同 `WithTrace`：

```go
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
```

(d) `Send` 全文替换为：

```go
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
```

(e) `Drain` 替换为（收集翻转 ID，持锁外调 readMark）：

```go
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
```

(f) 删除 `DrainBroadcast` 与 `Forward` 两个函数整体（约 237-341 行）。

(g) `Purge` 替换为：

```go
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
```

(h) 新增 `Restore`（放在 `Reopen` 之后）：

```go
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
```

(i) `ErrRecipientClosed` 上方注释不动；`Count`/`Peek`/`Reopen`/`sortByPriority`/`formatID`/`drainMessages` 不动。

- [x] **Step 4: 跑测试 + 全量构建**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/mailbox/ -v`
Expected: PASS（含既有非广播测试）

Run: `cd backend && GOTOOLCHAIN=local go build ./...`
Expected: PASS（Purge 返回值被忽略的旧调用点仍编译通过——Go 允许丢弃返回值）

- [x] **Step 5: 检查点**

向用户报告：mailbox.go 改动摘要 + 测试结果 + 变更文件清单（`internal/mailbox/mailbox.go`、`internal/mailbox/mailbox_test.go`）。

---
### Task 2: store.MailboxStore + schema + 迁移副本 + 级联清单

**Files:**
- Create: `backend/internal/store/mailbox_store.go`
- Modify: `backend/internal/store/schema.go`（尾部追加 `EnsureMailboxSchema`）
- Modify: `backend/internal/store/postgres_store.go:21-34`（`PostgresStore` 加 `Mailbox` 字段）、`:67-73`（构造装配区）
- Modify: `backend/internal/store/session_store.go:204-212`（`sessionScopedTables` 加表）
- Modify: `backend/internal/bootstrap/bootstrap.go:1163-1174`（`ensureSchemas` map 加条目）
- Create: `migrations/010_mailbox_messages.sql`
- Test: `test/api/mailbox_store_integration_test.go`（新建，`//go:build integration`）

**Interfaces:**
- Consumes: `mailbox.Message` 的字段（Task 1）；bootstrap 适配器负责 `mailbox.Message` ↔ `store.MailboxMessage` 转换（Task 3），store 包不 import mailbox。
- Produces:
  - `store.NewMailboxStore(db *sql.DB) *MailboxStore`
  - `store.MailboxMessage` struct（字段：`ID, Owner, SessionID, FromAgent, ToAgent, Type, Subject, Body string; Payload []byte; Priority int; ReplyTo, ThreadID string; CreatedAt time.Time`）
  - `(*MailboxStore) Save(ctx, m MailboxMessage) error`（`ON CONFLICT (id) DO NOTHING`）
  - `(*MailboxStore) MarkRead(ctx, ids []string, readAt time.Time) error`
  - `(*MailboxStore) MarkDead(ctx, ids []string) error`
  - `(*MailboxStore) LoadUnread(ctx, sessionID string) ([]MailboxMessage, error)`
  - `store.EnsureMailboxSchema(ctx, db) error`
  - `PostgresStore.Mailbox *MailboxStore`（NewPostgresStore 装配）

- [x] **Step 1: 写集成测试（失败先行）**

先读一个既有集成测试学 fixtures 模式：

Run: `ls test/api/ && head -60 test/api/$(ls test/api/ | grep -m1 '_test.go')`
动作：找到 fixtures 提供的 PG 建库辅助（每包独立库、docker-compose.test.yml 容器），照其签名写：

`test/api/mailbox_store_integration_test.go`：

```go
//go:build integration

package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
	// fixtures 的 PG 辅助按既有测试实际包名/函数名引入（先读同目录既有测试确认）。
)

// mailbox_messages：Save 幂等 / LoadUnread 只回未读 / MarkRead 与 MarkDead 状态翻转。
func TestMailboxStoreRoundtrip(t *testing.T) {
	db := /* 照既有集成测试取 *sql.DB（fixtures 辅助） */ nil
	if db == nil {
		t.Fatal("fixtures PG 不可用")
	}
	ctx := context.Background()
	if err := store.EnsureMailboxSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	ms := store.NewMailboxStore(db)
	now := time.Now()
	m1 := store.MailboxMessage{ID: "it_msg_1", SessionID: "it-sess", FromAgent: "it-sess/domain-1", ToAgent: "it-sess", Type: "request", Subject: "s1", ThreadID: "it_msg_1", CreatedAt: now}
	m2 := store.MailboxMessage{ID: "it_msg_2", SessionID: "it-sess", FromAgent: "user", ToAgent: "it-sess/domain-1", Type: "info", Subject: "s2", CreatedAt: now}
	if err := ms.Save(ctx, m1); err != nil {
		t.Fatal(err)
	}
	if err := ms.Save(ctx, m2); err != nil {
		t.Fatal(err)
	}
	// 幂等：同 ID 再存不报错不重复。
	if err := ms.Save(ctx, m1); err != nil {
		t.Fatal(err)
	}
	unread, err := ms.LoadUnread(ctx, "it-sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 {
		t.Fatalf("unread 应 2 条，got %d", len(unread))
	}
	if err := ms.MarkRead(ctx, []string{"it_msg_1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	unread, err = ms.LoadUnread(ctx, "it-sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || unread[0].ID != "it_msg_2" {
		t.Fatalf("MarkRead 后 unread 应只剩 it_msg_2：%+v", unread)
	}
	if err := ms.MarkDead(ctx, []string{"it_msg_2"}); err != nil {
		t.Fatal(err)
	}
	unread, err = ms.LoadUnread(ctx, "it-sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 0 {
		t.Fatalf("MarkDead 后 unread 应空：%+v", unread)
	}
	// 他会话隔离。
	other, err := ms.LoadUnread(ctx, "it-sess-other")
	if err != nil || len(other) != 0 {
		t.Fatalf("跨会话串读：%+v err=%v", other, err)
	}
}
```

（fixtures 取 DB 的两行样板以既有测试为准；若既有测试用的是 `*store.PostgresStore` 则 `pg.DB()` 取 `*sql.DB`。）

- [x] **Step 2: 跑测试确认失败**

Run: `cd test && GOTOOLCHAIN=local go test -tags integration ./api/ -run TestMailboxStoreRoundtrip -v`
Expected: FAIL（编译错误：store.NewMailboxStore / EnsureMailboxSchema 不存在）。本机若无 PG 容器可跳过到 Step 3 后统一跑（fixtures 拉起方式见 test/go.mod 旁注释或既有测试头部注释）。

- [x] **Step 3: 实现 mailbox_store.go**

新建 `backend/internal/store/mailbox_store.go`：

```go
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
```

注意：`sanitizeUTF8` 是 session_store.go 里既有的小写清洗函数（同包可直接用，先 `grep -n "func sanitizeUTF8" backend/internal/store/` 确认拼写；若实际叫别的名则改用现有那个）。

- [x] **Step 4: schema.go 追加 EnsureMailboxSchema**

`backend/internal/store/schema.go` 尾部追加（风格照 `EnsureAgentMessagesSchema`）：

```go
// EnsureMailboxMessagesSchema 自动创建 mailbox_messages 表 (幂等)。
// 对应 migrations/010_mailbox_messages.sql：mailbox 持久化（2026-09-28）——
// Send 双写 / Drain 标 read / Purge 标 dead / 重启 LoadUnread 重投。
// append-only：状态翻转只 UPDATE，不物理删行（删除仅会话级联）；owner 多租户预留。
func EnsureMailboxSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS mailbox_messages (
    id          VARCHAR(128) PRIMARY KEY,
    owner       VARCHAR(64) NOT NULL DEFAULT '',
    session_id  VARCHAR(64) NOT NULL,
    from_agent  VARCHAR(128) NOT NULL DEFAULT '',
    to_agent    VARCHAR(128) NOT NULL DEFAULT '',
    type        VARCHAR(16) NOT NULL,
    subject     TEXT,
    body        TEXT,
    payload     JSONB,
    priority    INT NOT NULL DEFAULT 0,
    status      VARCHAR(16) NOT NULL DEFAULT 'unread',
    reply_to    VARCHAR(128),
    thread_id   VARCHAR(128),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_session
    ON mailbox_messages (session_id, status);
`)
	return err
}
```

- [x] **Step 5: PostgresStore 装配 + ensureSchemas 注册 + 级联清单 + 迁移副本**

(a) `backend/internal/store/postgres_store.go`：
- struct（:21-34）`LearnedSkills` 行后加字段：`Mailbox *MailboxStore // Agent 间邮件持久化（010_mailbox_messages.sql）`
- `NewPostgresStore` 装配区（:67-73 `s.LearnedSkills = ...` 附近）加：`s.Mailbox = NewMailboxStore(db)`

(b) `backend/internal/bootstrap/bootstrap.go:1163-1174` ensureSchemas map 加一行：

```go
		"mailbox_messages":      store.EnsureMailboxSchema,
```

(c) `backend/internal/store/session_store.go:204-212` `sessionScopedTables` 追加 `"mailbox_messages"`（放 `"agent_tree_nodes"` 后）。

(d) 新建 `migrations/010_mailbox_messages.sql`（文档性副本，运行时不读该目录）：

```sql
-- 010_mailbox_messages.sql: mailbox 持久化（2026-09-28，P0 消息可靠性）
-- 运行时由 store.EnsureMailboxSchema 幂等建表，本文件为文档性副本。
CREATE TABLE IF NOT EXISTS mailbox_messages (
    id          VARCHAR(128) PRIMARY KEY,
    owner       VARCHAR(64) NOT NULL DEFAULT '',
    session_id  VARCHAR(64) NOT NULL,
    from_agent  VARCHAR(128) NOT NULL DEFAULT '',
    to_agent    VARCHAR(128) NOT NULL DEFAULT '',
    type        VARCHAR(16) NOT NULL,
    subject     TEXT,
    body        TEXT,
    payload     JSONB,
    priority    INT NOT NULL DEFAULT 0,
    status      VARCHAR(16) NOT NULL DEFAULT 'unread',  -- unread/read/dead
    reply_to    VARCHAR(128),
    thread_id   VARCHAR(128),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_session
    ON mailbox_messages (session_id, status);
```

- [x] **Step 6: 构建 + 单测 + 集成测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/store/ ./internal/mailbox/`
Expected: PASS

Run: `cd test && GOTOOLCHAIN=local go test -tags integration ./api/ -run TestMailboxStoreRoundtrip -v`
Expected: PASS（需 fixtures PG 容器；本机无 docker 时记录"未跑"并报告用户）

- [x] **Step 7: 检查点**

报告变更文件清单与测试结果。

---
### Task 3: bootstrap 持久化接线 + 崩溃恢复重投

**Files:**
- Modify: `backend/internal/bootstrap/bootstrap.go:358-389`（WithTrace 块之后追加持久化 hook 接线）
- Modify: `backend/internal/agent/session_react.go:189-219`（`reactSessionStore` 加 `mailbox` 字段）、`:1084-1153`（`buildRestoredSession` 尾部加重载调用）
- Modify: `backend/internal/agent/service_react.go`（`reactSessionStore` 构造处注入 mailbox；先 `grep -n "reactSessionStore{" backend/internal/agent/` 定位）

**Interfaces:**
- Consumes: `store.PostgresStore.Mailbox`（Task 2）、`mailbox.WithPersist/WithReadMarker/WithDeadMarker/Restore`（Task 1）
- Produces:
  - bootstrap 内 `mailboxMessageToRow(msg *mailbox.Message) store.MailboxMessage`（转换适配器）
  - `(*reactSessionStore).reloadUnreadMailbox(sessionID string)`（agent 包内 `rowToMailboxMessage(m store.MailboxMessage) *mailbox.Message` 转换器）

- [x] **Step 1: 写恢复重投的失败测试**

`backend/internal/agent/` 下找既有 session 测试文件模式：`grep -ln "reactSessionStore\|buildRestoredSession" backend/internal/agent/*_test.go`。

在合适的测试文件（或新建 `backend/internal/agent/mailbox_restore_test.go`）写：

```go
package agent

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// 恢复重投：Restore 进箱的未读消息能被 drainMailbox 正常注入历史。
func TestDrainMailboxAfterRestore(t *testing.T) {
	mb := mailbox.New()
	ag := &ReActAgent{name: "sess-1", mailbox: mb}
	if err := mb.Restore(&mailbox.Message{
		ID: "msg_r1", From: "sess-1/domain-1", To: "sess-1",
		Type: mailbox.MsgRequest, Subject: "恢复的问题", ThreadID: "msg_r1",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	history, n := ag.drainMailbox(nil)
	if n != 1 || len(history) != 1 {
		t.Fatalf("恢复消息未被 drain：n=%d history=%d", n, len(history))
	}
	if got := history[0].Content; !strings.Contains(got, "msg_r1") {
		t.Fatalf("注入文本应含消息 id：%q", got)
	}
}
```

注：`ReActAgent` 字段名（`name`/`mailbox`）以 `react_agent.go` 实际定义为准（先 `grep -n "mailbox" backend/internal/agent/react_agent.go | head -5` 确认字段名与可设置性；若为私有构造则照 `running_inject_test.go:156` 的既有构造方式）。`strings` 按需补 import。

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/agent/ -run TestDrainMailboxAfterRestore -v`
Expected: PASS——**此测试在 Task 1 完成后即应通过**（Restore 已实现）。若失败说明 Task 1 未完成，回查。本测试实际验证点在 Step 5 的注入文本含 id（Task 4 才加 id 到注入文本），此处先验证 Restore→Drain 链路；`msg_r1` 断言在 Task 4 后才过——先注释该行断言，Task 4 取消注释。

- [x] **Step 3: bootstrap 持久化 hook 接线**

`backend/internal/bootstrap/bootstrap.go` 的 `sharedMailbox.WithTrace(...)` 块（:360-389）结束之后追加：

```go
	// mailbox 持久化（2026-09-28 P0）：Send 同步双写 mailbox_messages（崩溃丢失窗口
	// 压到最小；PG 抖动 fail-open 仅日志）；Drain/Purge 状态翻转异步批量落库。
	mailboxStore := store.NewMailboxStore(pgStore.DB())
	sharedMailbox.WithPersist(func(msg *mailbox.Message) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mailboxStore.Save(ctx, mailboxMessageToRow(msg)); err != nil {
			log.Printf("[mailbox] persist failed: id=%s err=%v", msg.ID, err)
		}
	})
	sharedMailbox.WithReadMarker(func(ids []string) {
		// 已读标记异步：读侧已消费注入，落库慢不阻塞 ReAct 主循环；崩溃重投一次
		// 由注入侧幂等消化（Drain 再翻已读是空操作）。
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := mailboxStore.MarkRead(ctx, ids, time.Now()); err != nil {
				log.Printf("[mailbox] mark read failed: ids=%d err=%v", len(ids), err)
			}
		}()
	})
	sharedMailbox.WithDeadMarker(func(ids []string) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := mailboxStore.MarkDead(ctx, ids); err != nil {
				log.Printf("[mailbox] mark dead failed: ids=%d err=%v", len(ids), err)
			}
		}()
	})
```

并在 bootstrap.go 文件尾部（或紧邻接线处的包级区）加转换适配器：

```go
// mailboxMessageToRow 把 mailbox.Message 转为 store 行（分层纪律：store 不 import mailbox）。
// session_id 从收件人 ID 派生（MetaAgent agentID==sessionID；子 Agent "session/…" 取首段）。
func mailboxMessageToRow(msg *mailbox.Message) store.MailboxMessage {
	return store.MailboxMessage{
		ID:        msg.ID,
		SessionID: sessionIDOfAgentID(msg.To),
		FromAgent: msg.From,
		ToAgent:   msg.To,
		Type:      string(msg.Type),
		Subject:   msg.Subject,
		Body:      msg.Body,
		Payload:   store.MailboxPayloadToJSON(msg.Payload),
		Priority:  msg.Priority,
		ReplyTo:   msg.ReplyTo,
		ThreadID:  msg.ThreadID,
		CreatedAt: msg.CreatedAt,
	}
}

// sessionIDOfAgentID 取 agentID 的会话前缀（"sess/domain-1" → "sess"）。
func sessionIDOfAgentID(agentID string) string {
	if i := strings.IndexByte(agentID, '/'); i > 0 {
		return agentID[:i]
	}
	return agentID
}
```

（`strings`/`context`/`time`/`store` import 缺啥补啥。）

- [x] **Step 4: reactSessionStore 加 mailbox 字段 + buildRestoredSession 重载**

(a) `backend/internal/agent/session_react.go:189` `reactSessionStore` struct 加字段（放在 `pgStore` 字段后）：

```go
	// mailbox 用于崩溃恢复时把 mailbox_messages 的未读邮件重投进箱（Task：P0 持久化）。
	// 由 ReactService 构造时注入；nil（测试）时重载跳过。
	mailbox *mailbox.Mailbox
```

`grep -n "reactSessionStore{" backend/internal/agent/*.go` 找构造处， composite literal 加 `mailbox: <ReactService 的 mailbox 字段>`（ReactService 持有 `s.mailbox`，构造处通常在 `NewReactService`；把 `mailbox` 参数透传进去）。

(b) `session_react.go` `buildRestoredSession`（:1152 `return sess` 之前）加：

```go
	// mailbox 未读重投（P0 持久化）：重启前已投递未消费的邮件回箱，meta/热驻槽
	// 恢复后经 drainMailbox 自然消费；同 ID 幂等（Restore 内部去重）。
	st.reloadUnreadMailbox(rec.SessionID)
```

(c) `session_react.go` 追加：

```go
// reloadUnreadMailbox 从 mailbox_messages 读该会话全部未读邮件并重投进内存邮箱。
// best-effort：PG 故障仅记日志，不影响会话恢复主流程。
func (st *reactSessionStore) reloadUnreadMailbox(sessionID string) {
	if st.pgStore == nil || st.pgStore.Mailbox == nil || st.mailbox == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := st.pgStore.Mailbox.LoadUnread(ctx, sessionID)
	if err != nil {
		log.Printf("[mailbox] reload unread failed: session=%s err=%v", sessionID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	n := 0
	for _, row := range rows {
		if err := st.mailbox.Restore(rowToMailboxMessage(row)); err != nil {
			log.Printf("[mailbox] restore failed: id=%s err=%v", row.ID, err)
			continue
		}
		n++
	}
	log.Printf("[mailbox] unread restored: session=%s total=%d restored=%d", sessionID, len(rows), n)
}

// rowToMailboxMessage 把 store 行转回 mailbox.Message（agent 包内适配，分层纪律）。
func rowToMailboxMessage(row store.MailboxMessage) *mailbox.Message {
	m := &mailbox.Message{
		ID:        row.ID,
		From:      row.FromAgent,
		To:        row.ToAgent,
		Type:      mailbox.MessageType(row.Type),
		Subject:   row.Subject,
		Body:      row.Body,
		Priority:  row.Priority,
		ReplyTo:   row.ReplyTo,
		ThreadID:  row.ThreadID,
		Status:    mailbox.StatusUnread,
		CreatedAt: row.CreatedAt,
	}
	if len(row.Payload) > 0 {
		var payload map[string]any
		if json.Unmarshal(row.Payload, &payload) == nil {
			m.Payload = payload
		}
	}
	return m
}
```

（`encoding/json`、`log`、`context`、`time`、`store`、`mailbox` import 按需补。）

- [x] **Step 5: 构建 + 测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/agent/ ./internal/mailbox/`
Expected: PASS

人工核查点（不必自动化）：`restoreSessions`（session_react.go:1203）与 `restoreOneSession`（:1162）都经 `buildRestoredSession`，确认两条路径都会触发重载。

- [x] **Step 6: 检查点**

报告变更文件清单与测试结果。

---
### Task 4: request/reply 配对注册表 + 超时升级 + 提示词同步

**Files:**
- Create: `backend/internal/domain/subagent/pending_request.go`
- Test: `backend/internal/domain/subagent/pending_request_test.go`（新建）
- Modify: `backend/internal/domain/subagent/dispatcher.go`（struct 字段 :149 区、`NewDispatcher` :1441、`sendMessageTool` :2140-2285、`patrol` :991-1007、`purge.go` `PurgeSession` :20）
- Modify: `backend/internal/agent/react_agent.go:1999-2032`（`mailboxMessageToReact` 注入格式带 id/thread/reply_to）
- Modify: `backend/internal/config/config.go`（`AgentConfig` 加字段 + `applyAgentStandaloneDefaults` :854 加默认值）
- Modify: `backend/internal/bootstrap/bootstrap.go`（dispatcher option 链 :471 后接线 + mailbox WithRestoreHandler 接线）
- Modify: `backend/pkg/prompts/meta_agent.go:236-244`、`backend/pkg/prompts/domain_agent.go:247-261`、`backend/internal/domain/subagent/idle_pool.go:1636-1638`、`backend/pkg/prompts/prompts.go:15`（Version bump）
- Modify: `backend/internal/agent/react_agent_test.go:1013-1019`、`backend/internal/agent/mailbox_fence_test.go`（注入格式断言跟进）

**Interfaces:**
- Consumes: `mailbox.Send` 自动 ThreadID 回填（Task 1）、`mailbox.WithRestoreHandler`（Task 1）
- Produces:
  - `newPendingRequestRegistry() *pendingRequestRegistry`，方法 `add/complete/matchAuto/expire/purgeSession`（本文件内部，dispatcher 用）
  - `(*Dispatcher) WithPeerRequestTimeout(d time.Duration) *Dispatcher`
  - `(*Dispatcher) RegisterRestoredPending(msg *mailbox.Message)`（bootstrap 的 restoreHandler 接线目标；导出因 bootstrap 跨包）
  - config key `agent.peer_request_timeout_min`（默认 15）

- [x] **Step 1: 写注册表单测（失败先行）**

新建 `backend/internal/domain/subagent/pending_request_test.go`：

```go
package subagent

import (
	"testing"
	"time"
)

func TestPendingRequestCompleteByReplyTo(t *testing.T) {
	r := newPendingRequestRegistry()
	r.add(&pendingRequest{MsgID: "m1", From: "a", To: "b", ThreadID: "m1", Deadline: time.Now().Add(time.Minute)})
	if got := r.complete("m1"); got == nil || got.From != "a" {
		t.Fatalf("complete 应命中 m1：%+v", got)
	}
	if got := r.complete("m1"); got != nil {
		t.Fatal("重复销账应幂等返回 nil")
	}
}

func TestPendingRequestMatchAuto(t *testing.T) {
	r := newPendingRequestRegistry()
	old := &pendingRequest{MsgID: "m-old", From: "meta", To: "b", ThreadID: "m-old", Deadline: time.Now().Add(time.Minute)}
	r.add(old)
	time.Sleep(time.Millisecond) // 保证 createdAt 先后
	r.add(&pendingRequest{MsgID: "m-new", From: "meta", To: "b", ThreadID: "m-new", Deadline: time.Now().Add(time.Minute)})
	// b 回复 meta：自动配对最近一条未答 request。
	got := r.matchAuto("b", "meta", "")
	if got == nil || got.MsgID != "m-new" {
		t.Fatalf("自动配对应取最近一条：%+v", got)
	}
	// thread 过滤：带 thread 时不串链。
	if got := r.matchAuto("b", "meta", "m-old"); got == nil || got.MsgID != "m-old" {
		t.Fatalf("thread 过滤失效：%+v", got)
	}
	// 反向（meta 回复 b）不命中。
	if got := r.matchAuto("meta", "b", ""); got != nil {
		t.Fatalf("方向反了不该命中：%+v", got)
	}
}

func TestPendingRequestExpire(t *testing.T) {
	r := newPendingRequestRegistry()
	r.add(&pendingRequest{MsgID: "m-exp", From: "a", To: "b", Deadline: time.Now().Add(-time.Second)})
	r.add(&pendingRequest{MsgID: "m-live", From: "a", To: "b", Deadline: time.Now().Add(time.Hour)})
	expired := r.expire(time.Now())
	if len(expired) != 1 || expired[0].MsgID != "m-exp" {
		t.Fatalf("expire 应只弹超期项：%+v", expired)
	}
	// 已弹出不再重复弹。
	if again := r.expire(time.Now()); len(again) != 0 {
		t.Fatalf("expire 应销账不重复：%+v", again)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/subagent/ -run 'TestPendingRequest' -v`
Expected: FAIL（编译错误）

- [x] **Step 3: 实现 pending_request.go**

新建 `backend/internal/domain/subagent/pending_request.go`：

```go
// pending_request.go 实现 send_message 协作问答的请求-回复配对注册表
//（2026-09-28 P0：此前配对靠双方模型沿用 thread_id 自觉，ReplyTo 被误填成
// 发送方 agentID，回复永远不来也无任何告警——实测 meta 发进度询问零回复）。
//
// 机制：request/escalate 投递成功即登记（消息 ID 为键）；reply 到达时按 ReplyTo
// 显式销账，未填 ReplyTo 时按 (回复方, 被回复方, thread) 自动配对最近一条；
// patrol 每 tick 扫超期项，给提问方的父 Agent 投 escalate 告警。
package subagent

import (
	"strings" // purgeSession 的会话前缀匹配
	"sync"
	"time"
)

// pendingRequest 一条待应答的协作询问。
type pendingRequest struct {
	MsgID     string    // 请求消息 ID（配对键）
	From      string    // 提问方 agentID
	To        string    // 被问方 agentID
	ThreadID  string    // 问答链 ID（首问 ID）
	Subject   string    // 摘要（超时告警文案用）
	CreatedAt time.Time // 登记时间（自动配对取最近）
	Deadline  time.Time // 超时升级死线
}

// pendingRequestRegistry 进程内待应答注册表（按消息 ID 索引）。
// 进程重启丢失：恢复路径对未读 request 经 RegisterRestoredPending 重建（deadline 顺延）；
// 已读未答的条目随进程蒸发——重建成本（跨表配对 SQL）远高于收益，回复到达时
// 自动配对兜底不依赖注册表存在。
type pendingRequestRegistry struct {
	mu   sync.Mutex
	byID map[string]*pendingRequest
}

func newPendingRequestRegistry() *pendingRequestRegistry {
	return &pendingRequestRegistry{byID: make(map[string]*pendingRequest)}
}

// add 登记待应答请求（同 ID 覆盖：恢复重建与重投安全）。
func (r *pendingRequestRegistry) add(req *pendingRequest) {
	if req == nil || req.MsgID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[req.MsgID] = req
}

// complete 按消息 ID 销账（reply 的 ReplyTo 命中）；返回被销账项，未命中/重复返回 nil。
func (r *pendingRequestRegistry) complete(msgID string) *pendingRequest {
	if msgID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[msgID]
	if ok {
		delete(r.byID, msgID)
		return req
	}
	return nil
}

// matchAuto 自动配对：replier（回复方=reply 发送者）答复 target（被回复方=reply 收件人）
// 时，找"From==target 且 To==replier"的未答请求，threadID 非空时要求同链；取最近一条。
// 命中即销账（一答销一问）。未命中返回 nil（reply 照常投递，只是不销账）。
func (r *pendingRequestRegistry) matchAuto(replier, target, threadID string) *pendingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *pendingRequest
	for _, req := range r.byID {
		if req.From != target || req.To != replier {
			continue
		}
		if threadID != "" && req.ThreadID != threadID {
			continue
		}
		if best == nil || req.CreatedAt.After(best.CreatedAt) {
			best = req
		}
	}
	if best != nil {
		delete(r.byID, best.MsgID)
	}
	return best
}

// expire 弹出全部超期项并销账（调用方负责告警）；按 Deadline 升序返回。
func (r *pendingRequestRegistry) expire(now time.Time) []*pendingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*pendingRequest
	for id, req := range r.byID {
		if now.After(req.Deadline) {
			out = append(out, req)
			delete(r.byID, id)
		}
	}
	return out
}

// purgeSession 会话硬删除时清理会话内全部条目（From/To 任一在会话内）。
func (r *pendingRequestRegistry) purgeSession(sessionID string) {
	if sessionID == "" {
		return
	}
	inSession := func(id string) bool {
		return id == sessionID || strings.HasPrefix(id, sessionID+"/")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, req := range r.byID {
		if inSession(req.From) || inSession(req.To) {
			delete(r.byID, id)
		}
	}
}
```

- [x] **Step 4: Dispatcher 字段 + 构造初始化 + Option + config**

(a) `dispatcher.go:149` `Dispatcher` struct 字段区（找 `pausedResumes sync.Map` :356 附近）加：

```go
	// pendingReqs 协作问答待应答注册表（2026-09-28 P0 request/reply 配对）。
	pendingReqs *pendingRequestRegistry
	// peerReqTimeout 协作询问超时升级时长（patrol sweep 用）；<=0 按默认 15min。
	peerReqTimeout time.Duration
```

(b) `NewDispatcher`（:1441 composite literal）加一行：

```go
		pendingReqs:       newPendingRequestRegistry(),
```

(c) Option（紧跟 `WithMaxTotalDispatches` 定义附近，:1525-2030 区找位置）加：

```go
// WithPeerRequestTimeout 配置协作询问的超时升级时长（patrol 每 tick 扫超期项，
// 给提问方父 Agent 投 escalate）。<=0 保持默认 15min。
func (d *Dispatcher) WithPeerRequestTimeout(t time.Duration) *Dispatcher {
	if t > 0 {
		d.peerReqTimeout = t
	}
	return d
}

// peerRequestTimeoutOrDefault 读协作询问超时（未配置按 15min）。
func (d *Dispatcher) peerRequestTimeoutOrDefault() time.Duration {
	if d.peerReqTimeout > 0 {
		return d.peerReqTimeout
	}
	return 15 * time.Minute
}
```

(d) `backend/internal/config/config.go`：
- `AgentConfig`（:296 `DomainHeartbeatTimeoutMin` 字段后）加：

```go
	// PeerRequestTimeoutMin 协作问答（send_message request/escalate）超时升级（分钟）。
	// 默认 15：超期未获 reply 给提问方父 Agent 投 escalate 告警。<=0 按默认。
	PeerRequestTimeoutMin int `yaml:"peer_request_timeout_min"`
```

- `applyAgentStandaloneDefaults`（:854，放 DispatchRetryCount 默认后）加：

```go
	// 协作询问超时升级（2026-09-28 P0 配对）：默认 15 分钟。
	if c.Agent.PeerRequestTimeoutMin == 0 {
		c.Agent.PeerRequestTimeoutMin = 15
	}
```

(e) `bootstrap.go` dispatcher option 链（:471 `WithMaxTotalDispatches` 后）加：

```go
	// 协作问答配对：request/reply 机制配对 + 超时升级（patrol sweep）。
	subAgentDispatcher.WithPeerRequestTimeout(time.Duration(cfg.Agent.PeerRequestTimeoutMin) * time.Minute)
```

(f) `bootstrap.go` 恢复 handler 接线（dispatcher 构造完成后，:533 `role.RegisterModelTools` 附近）加：

```go
	// 崩溃恢复重投时重建 pending 问答注册表（未读 request 的 deadline 顺延一个周期）。
	sharedMailbox.WithRestoreHandler(func(msg *mailbox.Message) {
		subAgentDispatcher.RegisterRestoredPending(msg)
	})
```

- [x] **Step 5: sendMessageTool 改造**

`dispatcher.go` `sendMessageTool.Execute`（:2168-2257）做三处修改：

(a) 参数解析区（:2211-2212 `threadID` 行后）加 `reply_to` 解析与自动配对：

```go
	// thread_id 可选：同一问答链上的消息共享 ThreadID，便于多轮验证闭环聚合。
	threadID, _ := args["thread_id"].(string)
	// reply_to 可选（2026-09-28 配对机制）：回答别人提问时填来信的 id（注入文本
	// [mailbox from X id=… thread=…] 里可查）。未填时自动配对最近一条该方向未答请求。
	replyTo, _ := args["reply_to"].(string)
	if msgType == mailbox.MsgReply {
		if replyTo == "" && d.pendingReqs != nil {
			if pr := d.pendingReqs.matchAuto(fromID, toID, threadID); pr != nil {
				replyTo = pr.MsgID
				if threadID == "" {
					threadID = pr.ThreadID
				}
			}
		} else if replyTo != "" && d.pendingReqs != nil {
			d.pendingReqs.complete(replyTo)
		}
	}
```

（注意：`matchAuto` 命中即销账，显式 `reply_to` 走 `complete` 销账。）

(b) `Send` 调用（:2214-2222）改为带上 replyTo：

```go
	id, err := d.mailbox.Send(&mailbox.Message{
		From:     fromID,
		To:       toID,
		Type:     msgType,
		Subject:  subject,
		Body:     body,
		ReplyTo:  replyTo,
		ThreadID: threadID,
	})
```

（删除旧的 `ReplyTo: fromID` 行——那是文档与实现矛盾的历史错误。）

(c) Send 成功后（:2226 err 检查之后、里程碑检查点之前）加登记：

```go
	// 问答配对登记：request/escalate 入册等答，patrol sweep 超时升级；Send 已把
	// 缺省 ThreadID 回填为消息 ID，这里直接读。
	if (msgType == mailbox.MsgRequest || msgType == mailbox.MsgEscalate) && d.pendingReqs != nil {
		d.pendingReqs.add(&pendingRequest{
			MsgID:     id,
			From:      fromID,
			To:        toID,
			ThreadID:  threadID,
			Subject:   subject,
			CreatedAt: time.Now(),
			Deadline:  time.Now().Add(d.peerRequestTimeoutOrDefault()),
		})
	}
```

（`threadID` 变量此处可能未被 Send 回填——Send 改的是 msg 结构体。把上面 add 的 `ThreadID: threadID` 改为 `ThreadID: msg.ThreadID`，并相应把 Send 参数改为先构造 `msg := &mailbox.Message{...}` 再 `id, err := d.mailbox.Send(msg)`。）

(d) `Description()`（:2151-2163）末尾追加参数说明：

```go
		return "...（原文保留）..." +
			"reply_to：回答提问时填来信的 id（来信注入头 [mailbox from X id=…] 里有），" +
			"未填时系统自动配对最近一条该方向的未答提问；问答超时会升级给你的上级。"
```

（实现时把上面字符串并入原 return 语句，不要留占位符。）

- [x] **Step 6: patrol sweep + RegisterRestoredPending + PurgeSession 清理**

(a) `dispatcher.go` `patrol()`（:998-1005 的 recover 包裹 func 内，`d.scanStuck()` 之后）加一行：

```go
				d.sweepPendingRequests()
```

（与 scanStuck 同一个 recover 包裹即可。）

(b) `dispatcher.go` 新增（放 `wakeSuspendedParent` :1959 附近）：

```go
// sweepPendingRequests 扫超期未答的协作询问，给提问方的父 Agent 投 escalate 告警
//（提问方等不到回复不该只能沉默）。父取不到（树未接线/顶层 meta 提问）时发到会话
// 顶层（meta，agentID==sessionID）。
func (d *Dispatcher) sweepPendingRequests() {
	if d.pendingReqs == nil || d.mailbox == nil {
		return
	}
	for _, pr := range d.pendingReqs.expire(time.Now()) {
		to := ""
		if d.treeFn != nil {
			if t := d.treeFn(sessionIDFromAgentID(pr.From)); t != nil {
				if n, ok := t.Get(pr.From); ok {
					to = n.ParentID
				}
			}
		}
		if to == "" {
			to = sessionIDFromAgentID(pr.From)
		}
		if to == "" || to == pr.From {
			continue
		}
		if _, err := d.mailbox.Send(&mailbox.Message{
			From:    "dispatcher",
			To:      to,
			Type:    mailbox.MsgEscalate,
			Subject: "协作询问超时未答: " + truncateRunes(pr.Subject, 60),
			Body: fmt.Sprintf("【系统】%s 向 %s 的询问（消息 %s：%s）超过 %s 未获回复，登记已销账。"+
				"请处置：代为追问/改派/据现有信息收口。",
				pr.From, pr.To, pr.MsgID, truncateRunes(pr.Subject, 80), d.peerRequestTimeoutOrDefault()),
		}); err != nil {
			log.Printf("[subagent] pending-timeout escalate failed: to=%s err=%v", to, err)
			continue
		}
		d.wakeSuspendedParent(to, "【系统】有协作询问超时未答，请查收邮箱处置。")
		log.Printf("[subagent] PENDING-TIMEOUT: msg=%s from=%s to=%s escalated=%s", pr.MsgID, pr.From, pr.To, to)
	}
}

// RegisterRestoredPending 崩溃恢复重投时重建待应答注册（bootstrap 经 mailbox
// WithRestoreHandler 接线）：未读 request/escalate 顺延一个超时周期重新计时。
// user/dispatcher/system 等系统侧发送者不登记（它们不等回复）。
func (d *Dispatcher) RegisterRestoredPending(msg *mailbox.Message) {
	if d.pendingReqs == nil || msg == nil {
		return
	}
	if msg.Type != mailbox.MsgRequest && msg.Type != mailbox.MsgEscalate {
		return
	}
	if msg.From == "" || msg.From == "user" || msg.From == "dispatcher" || msg.From == "system" {
		return
	}
	d.pendingReqs.add(&pendingRequest{
		MsgID:     msg.ID,
		From:      msg.From,
		To:        msg.To,
		ThreadID:  msg.ThreadID,
		Subject:   msg.Subject,
		CreatedAt: time.Now(),
		Deadline:  time.Now().Add(d.peerRequestTimeoutOrDefault()),
	})
}
```

（`sessionIDFromAgentID` 是 dispatcher.go 既有函数，:2199 已用；`t.Get` 见 :5096 既有用法。）

(c) `purge.go` `PurgeSession`（:20，会话级清理段 `d.suspendStates.Delete(sessionID)` 后）加：

```go
	if d.pendingReqs != nil {
		d.pendingReqs.purgeSession(sessionID)
	}
```

- [x] **Step 7: 注入格式带 id（mailboxMessageToReact）+ 测试跟进**

`backend/internal/agent/react_agent.go:2030-2031` 的返回行改为：

```go
	// 组合成带发送者标记的 user 消息返回（2026-09-28 配对：头里带消息 id/thread，
	// 被问方回复时按 id 填 reply_to；回复消息带 reply_to 便于提问方对账）。
	prefix := fmt.Sprintf("[mailbox from %s id=%s]", m.From, m.ID)
	if m.ThreadID != "" {
		prefix += " thread=" + m.ThreadID
	}
	if m.ReplyTo != "" {
		prefix += " reply_to=" + m.ReplyTo
	}
	return ReactMessage{Role: "user", Content: prefix + " " + body}
```

跑既有断言并跟进（把 `[mailbox from X]` 类断言改为含 id 的新格式；`mailbox_fence_test.go` 的围栏断言不受影响则不动）：

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/agent/ -run 'Mailbox|mailbox|Fence' -v`
Expected: PASS（必要时更新 `react_agent_test.go:1013/1019` 的期望字符串）

同时取消 Task 3 Step 2 里 `msg_r1` 断言的注释。

- [x] **Step 8: 提示词同步 + Version bump**

(a) `backend/pkg/prompts/meta_agent.go:239` 把：

```
你本人收到 request/[询问] 类消息时按【等待期
纪律】只处理该事务并立即 send_message(message_type=reply, thread_id 沿用) 回复，
禁止沉默或代为转派。
```

改为：

```
你本人收到 request/[询问] 类消息时按【等待期
纪律】只处理该事务并立即 send_message(message_type=reply, reply_to=来信注入头
里的 id, thread_id 沿用) 回复，禁止沉默或代为转派（问答有超时升级机制：
沉默会被升级给你的上级处置）。
```

（按文件实际缩进对齐；该行跨行则整段替换。）

(b) `backend/pkg/prompts/domain_agent.go:253-256` 「答别人」条改为：

```
- 答别人：收到 From 为其他 Agent 的 request/[询问] 消息**当轮必须回复**：send_message
   (to_agent_id=提问方, message_type=reply, reply_to=来信注入头里的 id, thread_id 沿用
   原消息)，给结论+关键依据+文件路径。只答该信息本身，不接新任务、不转派；确实不知道
   也回一句「无/建议问 X」，禁止沉默（超时未答会升级给你的上级）。
```

(c) `backend/internal/domain/subagent/idle_pool.go:1636-1638` 唤醒任务文本改为：

```go
	taskText := "【邮箱请求】Agent " + askerID + " 向你提问（request 消息已在你的邮箱）。\n" +
		"本轮只处置该提问：查看邮箱，用 send_message(to_agent_id=对方id, message_type=reply, " +
		"reply_to=来信注入头里的 id, thread_id 沿用) 回复，" +
		"给结论+关键依据+文件路径；回复后若无其他待办即结束。"
```

(d) `backend/pkg/prompts/prompts.go:15` Version 改 `"20260928-1"`。

- [x] **Step 9: 构建 + 相关测试全跑**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/domain/subagent/ ./internal/agent/ ./internal/config/ ./internal/mailbox/`
Expected: PASS

- [x] **Step 10: 检查点**

报告变更文件清单与测试结果。

---
### Task 5: Purge 死信通知（dispatcher 侧）

**Files:**
- Create: `backend/internal/domain/subagent/deadletter.go`
- Modify: `backend/internal/domain/subagent/dispatcher.go:1142`（killStuckSubAgent）、`:4407-4411`（runSubAgentOnce defer）、`:5036-5040`（ResumePaused defer）、`:5132-5134`（concludePaused）
- Modify: `backend/internal/domain/subagent/idle_pool.go:697-699`（destroySlot）、`:1866-1868`（supervisor panic 清理）
- Test: `backend/internal/domain/subagent/deadletter_test.go`（新建）

**Interfaces:**
- Consumes: `mailbox.Purge` 返回 `[]*Message`（Task 1）、`pendingReqs.complete`（Task 4）
- Produces: `(*Dispatcher) purgeMailboxWithNotice(agentID, reason string)`

注：`purge.go:57`（PurgeSession）与 `service_react.go:4680-4682`（finalizeSession）两处**不改**——会话整体删除时收件方同会话陪葬，通知无意义；Purge 返回值忽略在 Go 里合法（表达式语句丢弃返回值），deadMark hook（Task 1/3）已自动把 PG 行标 dead。

- [x] **Step 1: 写失败测试**

新建 `backend/internal/domain/subagent/deadletter_test.go`：

```go
package subagent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// 子 Agent 终结被 Purge 时，未读 request 的发送方收到"未送达"死信通知，且问答销账。
func TestPurgeMailboxWithNotice(t *testing.T) {
	mb := mailbox.New()
	d := &Dispatcher{mailbox: mb, pendingReqs: newPendingRequestRegistry()}
	// a 向 victim 发出 request（登记 pending）。
	id, err := mb.Send(&mailbox.Message{From: "sess/a", To: "sess/victim", Type: mailbox.MsgRequest, Subject: "问接口"})
	if err != nil {
		t.Fatal(err)
	}
	d.pendingReqs.add(&pendingRequest{MsgID: id, From: "sess/a", To: "sess/victim", Subject: "问接口"})
	// info 类未读不产生通知。
	if _, err := mb.Send(&mailbox.Message{From: "sess/c", To: "sess/victim", Type: mailbox.MsgInfo, Subject: "纯告知"}); err != nil {
		t.Fatal(err)
	}
	d.purgeMailboxWithNotice("sess/victim", "已终结")
	// a 收到死信通知。
	got := mb.Drain("sess/a")
	if len(got) != 1 || !strings.Contains(got[0].Subject, "未送达") {
		t.Fatalf("a 应收到死信通知：%+v", got)
	}
	// pending 已销账（死信等价于"永远不会有回复"）。
	if r := d.pendingReqs.complete(id); r != nil {
		t.Fatal("死信后 pending 应已销账")
	}
	// c 的 info 不产生通知。
	if n := mb.Count("sess/c"); n != 0 {
		t.Fatalf("info 未读不应通知发送方，c 收到 %d 条", n)
	}
}

// 收件箱为空/无未读 request 时零行为。
func TestPurgeMailboxWithNoticeNoop(t *testing.T) {
	mb := mailbox.New()
	d := &Dispatcher{mailbox: mb, pendingReqs: newPendingRequestRegistry()}
	d.purgeMailboxWithNotice("sess/none", "已终结") // 不 panic 即通过
	if _, err := mb.Send(&mailbox.Message{From: "sess/x", To: "sess/none2", Type: mailbox.MsgInfo, Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	d.purgeMailboxWithNotice("sess/none2", "已终结")
	if n := mb.Count("sess/x"); n != 0 {
		t.Fatalf("info 不应触发通知：%d", n)
	}
}
```

注：`Dispatcher` struct 字面量只填这两个字段若编译报错（其它字段无零值问题则无碍），照 `dispatcher_test.go` 既有构造方式调整（先 `grep -n "Dispatcher{" backend/internal/domain/subagent/dispatcher_test.go | head -3`）。

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/subagent/ -run 'TestPurgeMailboxWithNotice' -v`
Expected: FAIL（purgeMailboxWithNotice 不存在）

- [x] **Step 3: 实现 deadletter.go + 改四处调用点**

新建 `backend/internal/domain/subagent/deadletter.go`：

```go
// deadletter.go 实现 Purge 死信通知（2026-09-28 P0）：子 Agent 终结/被杀/销毁时
// 收件箱里未读的 request/escalate 不再静默消失——给发送方回投"未送达"通知并把
// 待应答注册销账，消除"发送时成功、投递后被清掉"的假投递。
package subagent

import (
	"fmt" // 通知正文格式化
	"log" // 死信投递失败仅记日志（best-effort）

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// purgeMailboxWithNotice 清空调用方指定 Agent 的收件箱，并对被丢弃的未读
// request/escalate 逐条给发送方回投 MsgInfo 死信通知（info 单向通知不回投——
// 它没有"等回复"语义）。reason 说明终结原因（进通知正文，供发送方决策改派/收口）。
// 会话级硬删除（PurgeSession/finalizeSession）不走这里：收件方同会话陪葬，通知无意义。
func (d *Dispatcher) purgeMailboxWithNotice(agentID, reason string) {
	if d.mailbox == nil {
		return
	}
	dropped := d.mailbox.Purge(agentID)
	for _, m := range dropped {
		if m.Type != mailbox.MsgRequest && m.Type != mailbox.MsgEscalate {
			continue
		}
		// 系统侧/自发消息不回投（user 直连有 UI 反馈，dispatcher/system 无消费方）。
		if m.From == "" || m.From == "user" || m.From == "dispatcher" || m.From == "system" || m.From == agentID {
			continue
		}
		// 死信等价于"永远不会有回复"：pending 注册销账，防超时升级误报。
		if d.pendingReqs != nil {
			d.pendingReqs.complete(m.ID)
		}
		if _, err := d.mailbox.Send(&mailbox.Message{
			From:    "dispatcher",
			To:      m.From,
			Type:    mailbox.MsgInfo,
			Subject: "消息未送达: " + truncateRunes(m.Subject, 60),
			Body: fmt.Sprintf("【系统】你发给 %s 的 %s（消息 %s：%s）未能送达：对方%s。"+
				"请据现状决定改派他人/自行处置/在回传中说明。",
				agentID, m.Type, m.ID, truncateRunes(m.Subject, 80), reason),
		}); err != nil {
			log.Printf("[subagent] dead-letter notice failed: to=%s msg=%s err=%v", m.From, m.ID, err)
		}
	}
}
```

调用点替换（四处 dispatcher.go + 两处 idle_pool.go，全部是 `d.mailbox.Purge(x)` 换成 `d.purgeMailboxWithNotice(x, "<原因>")`，注意 nil 守卫原样保留）：

(a) `dispatcher.go:1142`（killStuckSubAgent 内）：
```go
		d.purgeMailboxWithNotice(subAgentID, "已因长时间无活动被判定假死终止")
```
（保留原 `if d.mailbox != nil` 包裹——helper 内部有 nil 守卫时可去外层的，保持最小改动：直接替换调用行即可。）

(b) `dispatcher.go:4407-4411`（runSubAgentOnce defer）：
```go
	defer func() {
		d.purgeMailboxWithNotice(subAgentID, "已终结")
	}()
```

(c) `dispatcher.go:5036-5040`（ResumePaused defer）：同 (b)。

(d) `dispatcher.go:5132-5134`（concludePaused）：
```go
	if d.mailbox != nil {
		d.purgeMailboxWithNotice(pausedNodeID, "已达续跑上限强制收口")
	}
```

(e) `idle_pool.go:697-699`（destroySlot）：
```go
	if d.mailbox != nil {
		d.purgeMailboxWithNotice(s.id, "热驻槽已销毁")
	}
```

(f) `idle_pool.go:1866-1868`（supervisor panic 清理）：
```go
	if d.mailbox != nil {
		d.purgeMailboxWithNotice(s.id, "异常终止")
	}
```

- [x] **Step 4: 构建 + 测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/domain/subagent/ -v -run 'DeadLetter|Purge|TestPurgeMailbox'`
Expected: PASS；再跑整包 `GOTOOLCHAIN=local go test ./internal/domain/subagent/ ./internal/agent/` 确认无回归。

- [x] **Step 5: 检查点**

报告变更文件清单与测试结果。

---

### Task 6: 子 Agent 全局并发池（FIFO 排队 + 可配置上限 + 排队期巡检豁免）

**Files:**
- Create: `backend/internal/domain/subagent/pool.go`
- Test: `backend/internal/domain/subagent/pool_test.go`（新建）
- Modify: `backend/internal/domain/subagent/activity.go`（evidence 加 `queued` 标记 + `markQueued/clearQueued`）
- Modify: `backend/internal/domain/subagent/dispatcher.go`（struct 加 `concPool`、`NewDispatcher`、`WithConcurrencyLimit` Option、`enterExecGate` 帮助函数、`scanStuck` :1018-1049 加豁免、`dispatchOne` :3305-3450 goroutine 段、`ReviveWithMessage` :1291-1380 段）
- Modify: `backend/internal/domain/subagent/failure_prune.go:160-222`（fork 路径同模式）
- Modify: `backend/internal/domain/subagent/dispatcher.go:5013-5044`（ResumePaused 墙钟移位 + gate）
- Modify: `backend/internal/domain/subagent/idle_pool.go:1018-1050`（runDomainEngine 执行段 gate）
- Modify: `backend/internal/config/config.go`（`AgentConfig` 加 `MaxConcurrentSubAgents` + 默认值 8）
- Modify: `backend/internal/bootstrap/bootstrap.go:471` 附近（`WithConcurrencyLimit` 接线）

**Interfaces:**
- Consumes: 无（与 Task 1-5 正交，可并行实施；若并行注意 dispatcher.go 同文件冲突）
- Produces:
  - `newExecPool(limit int) *execPool`（limit<=0 返回 nil=不限）、`(*execPool) Acquire(ctx, id string) error`、`Release()`、`Stats() (running, queued int)`
  - `(*Dispatcher) WithConcurrencyLimit(n int) *Dispatcher`
  - `(*Dispatcher) enterExecGate(baseCtx, ev *activityEvidence, subAgentID string, wallClock time.Duration) (runCtx context.Context, runCancel context.CancelFunc, release func(), err error)`
  - `(*activityEvidence) markQueued()` / `clearQueued(now int64)`
  - config key `agent.max_concurrent_sub_agents`（默认 8）

- [x] **Step 1: 写池单测（失败先行）**

新建 `backend/internal/domain/subagent/pool_test.go`：

```go
package subagent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// 上限阻塞 + FIFO 顺序：满池后三个等待者**顺序入队**，放行后按入队序获名额。
func TestExecPoolFIFO(t *testing.T) {
	p := newExecPool(1)
	if err := p.Acquire(context.Background(), "holder"); err != nil {
		t.Fatal(err)
	}
	grantCh := make(chan string, 3)
	// 顺序入队：每个等待者确认入队后再派下一个，消除 goroutine 调度乱序。
	for i, id := range []string{"w1", "w2", "w3"} {
		id := id
		go func() {
			if err := p.Acquire(context.Background(), id); err != nil {
				t.Error(err)
				return
			}
			grantCh <- id // 持名额后上报（limit=1，任一时刻只有一个持有者，无并发写）
			p.Release()
		}()
		// 确认本等待者已入队再派下一个，消除 goroutine 调度乱序。
		deadline := time.Now().Add(2 * time.Second)
		for p.StatsQueued() != i+1 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.StatsQueued() != 3 {
		t.Fatalf("应有 3 个排队者，got %d", p.StatsQueued())
	}
	p.Release() // holder 放行，链式移交
	var got []string
	for i := 0; i < 3; i++ {
		select {
		case id := <-grantCh:
			got = append(got, id)
		case <-time.After(2 * time.Second):
			t.Fatalf("等待者未获名额：got=%v", got)
		}
	}
	if got[0] != "w1" || got[1] != "w2" || got[2] != "w3" {
		t.Fatalf("FIFO 顺序破坏：%v", got)
	}
	if r, q := p.Stats(); r != 0 || q != 0 {
		t.Fatalf("结束后 running/queued 应归零：%d/%d", r, q)
	}
}

// 排队期 ctx 取消：票据摘除，不占名额。
func TestExecPoolCancelWhileQueued(t *testing.T) {
	p := newExecPool(1)
	_ = p.Acquire(context.Background(), "holder")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.Acquire(ctx, "cancelled") }()
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("取消应返回 ctx.Err()")
	}
	p.Release() // holder 归还
	// 名额应可直接获取（取消的票据已摘除，不挡路）。
	got := make(chan error, 1)
	go func() { got <- p.Acquire(context.Background(), "next") }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消的票据挡住了后续 Acquire")
	}
}

// 取消与授予竞态：已授予的票据不因迟到的 cancel 丢名额（调用方 defer Release 归还）。
func TestExecPoolCancelGrantRace(t *testing.T) {
	p := newExecPool(1)
	_ = p.Acquire(context.Background(), "holder")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- p.Acquire(ctx, "racer") }()
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	p.Release() // 授予 racer
	cancel()    // 迟到的取消
	if err := <-got; err != nil {
		t.Fatalf("已授予后取消应视为成功：%v", err)
	}
	p.Release() // racer 归还（不能 panic/阻塞）
}

func TestExecPoolUnlimited(t *testing.T) {
	if newExecPool(0) != nil || newExecPool(-1) != nil {
		t.Fatal("limit<=0 应返回 nil（不限）")
	}
}
```

（`StatsQueued()` 是排队数便捷方法；实现时一并定义。）

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/subagent/ -run 'TestExecPool' -v`
Expected: FAIL（编译错误）

- [x] **Step 3: 实现 pool.go**

新建 `backend/internal/domain/subagent/pool.go`：

```go
// pool.go 实现子 Agent 全局并发池（2026-09-28 P1 资源治理）：限制同时在跑的子
// Agent 总数，超额派发 FIFO 排队而非拒绝（拒绝会与模型重试叠加成拒绝循环）。
//
// 职责边界：池只做准入控制（Acquire/Release/Stats）——子 Agent 生命周期权威仍是
// orchestrator.Tree（注册/取消/巡检/落 PG），池不维护任何 Agent 状态、不参与
// 取消语义（Acquire 等待期 ctx 取消即摘票据退出，树态由取消方收口）。
//
// 不变量：waitQ 非空 ⇒ sem 满。Release 时若有排队者，名额直接移交队首（不还桶），
// 保证 FIFO 且不被新到 Acquire 的 fast path 插队。
package subagent

import (
	"container/list" // FIFO 票据队列
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// execPoolQueueWarn 排队超此时长打 warn 日志（可观测性最低限度，不加 HTTP 端点）。
const execPoolQueueWarn = 2 * time.Minute

// execTicket 一张排队票据：granted 标记与 close(ch) 构成授予信号，均在 p.mu 下读写。
type execTicket struct {
	ch      chan struct{}
	granted bool
	elem    *list.Element
}

// execPool 并发名额池。limit<=0 时构造返回 nil（调用方 nil 检查即"不限"）。
type execPool struct {
	sem     chan struct{} // 容量=limit 的名额桶
	mu      sync.Mutex    // 保护 waitQ 与 ticket.granted
	waitQ   *list.List    // *execTicket FIFO
	queued  atomic.Int64  // 当前排队数（Stats/测试用）
	running atomic.Int64  // 当前持名额数（Stats/测试用）
}

// newExecPool 创建池；limit<=0 返回 nil 表示不限并发（旧行为）。
func newExecPool(limit int) *execPool {
	if limit <= 0 {
		return nil
	}
	return &execPool{sem: make(chan struct{}, limit), waitQ: list.New()}
}

// Acquire 获取一个执行名额：有空位直通；满则 FIFO 排队等 Release 移交。
// 排队期 ctx 取消：票据摘除返回 ctx.Err()（若取消与授予竞态且授予在先，视为成功——
// 调用方必须 defer Release 归还）。
// id 仅用于排队超时 warn 日志。
func (p *execPool) Acquire(ctx context.Context, id string) error {
	// fast path：有空位直通（waitQ 非空时 sem 恒满，不会插队）。
	select {
	case p.sem <- struct{}{}:
		p.running.Add(1)
		return nil
	default:
	}
	t := &execTicket{ch: make(chan struct{})}
	p.mu.Lock()
	t.elem = p.waitQ.PushBack(t)
	p.queued.Add(1)
	p.mu.Unlock()
	warn := time.AfterFunc(execPoolQueueWarn, func() {
		log.Printf("[subagent] exec-pool QUEUE-WAIT: sub=%s 排队超 %s 未获名额", id, execPoolQueueWarn)
	})
	defer warn.Stop()
	select {
	case <-t.ch:
		p.running.Add(1)
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		if t.granted {
			// 授予在先：名额已归本调用方，照常成功（迟到的取消不丢名额）。
			p.mu.Unlock()
			p.running.Add(1)
			return nil
		}
		p.waitQ.Remove(t.elem)
		p.queued.Add(-1)
		p.mu.Unlock()
		return ctx.Err()
	}
}

// Release 归还名额：有排队者直接移交队首（FIFO），否则还桶。
func (p *execPool) Release() {
	p.running.Add(-1)
	p.mu.Lock()
	if el := p.waitQ.Front(); el != nil {
		t := el.Value.(*execTicket)
		p.waitQ.Remove(el)
		p.queued.Add(-1)
		t.granted = true
		close(t.ch) // 名额移交队首（桶内占用不归还）
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	select {
	case <-p.sem:
	default:
		// 配对失衡（Release 多于 Acquire）防呆：不阻塞、打日志。
		log.Printf("[subagent] exec-pool RELEASE-UNDERFLOW（Acquire/Release 未配对）")
	}
}

// Stats 返回当前持名额数与排队数。
func (p *execPool) Stats() (running, queued int) {
	return int(p.running.Load()), int(p.queued.Load())
}

// StatsQueued 返回排队数（测试便捷方法）。
func (p *execPool) StatsQueued() int {
	return int(p.queued.Load())
}
```

- [x] **Step 4: 跑池测试**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/subagent/ -run 'TestExecPool' -v`
Expected: PASS

- [x] **Step 5: activity 排队标记 + scanStuck 豁免**

(a) `activity.go` `activityEvidence` struct（:24-43，`waitingChildren` 字段后）加：

```go
	// queued 并发池排队标记（2026-09-28 P1 并发池）：Acquire 等待期间置位，巡检豁免——
	// 排队中的 Agent 还没开始执行，lastTS 停留在派发时刻，不豁免则排队超阈值被误杀。
	queued atomic.Bool
```

(b) `activity.go` 加方法（`markChildWait` 后）：

```go
// markQueued 标记进入并发池排队（巡检豁免 + 展示态 "queued"）。
func (e *activityEvidence) markQueued() {
	e.queued.Store(true)
	e.lastKind.Store("queued")
}

// clearQueued 标记出队开始执行：刷 lastTS（排队时长不计入静默）并落 "dequeued" 展示态。
func (e *activityEvidence) clearQueued(now int64) {
	e.queued.Store(false)
	e.lastTS.Store(now)
	e.lastKind.Store("dequeued")
}
```

(c) `dispatcher.go` `scanStuck`（:1027 Range 闭包内，阈值计算后、LLM 豁免前）加：

```go
		// 0. 并发池排队中：豁免（未开始执行，无"静默"可言）。
		if e.queued.Load() {
			return true
		}
```

- [x] **Step 6: Dispatcher 字段 + Option + enterExecGate + config + bootstrap**

(a) `Dispatcher` struct 加字段（`pausedResumes sync.Map` 附近）：

```go
	// concPool 子 Agent 全局并发池（2026-09-28 P1）：nil=不限（旧行为）。
	// 只做准入控制，生命周期权威仍是 Tree。注意与 d.pool（热驻 idle 池）命名区分。
	concPool *execPool
```

(b) Option（WithPeerRequestTimeout 旁）：

```go
// WithConcurrencyLimit 配置同时在跑的子 Agent 全局上限（2026-09-28 P1）：
// 超额派发 FIFO 排队（排队不计墙钟、巡检豁免），<=0 不限（旧行为）。
func (d *Dispatcher) WithConcurrencyLimit(n int) *Dispatcher {
	d.concPool = newExecPool(n)
	return d
}
```

(c) 新增 `enterExecGate`（放 pool.go 尾部，跨三处派发路径共用）：

```go
// enterExecGate 并发池准入 + 墙钟挂载（2026-09-28 P1"出队才计墙钟"）：
//   - concPool==nil（不限）：直通，墙钟立即挂载（旧行为）；
//   - 排队等待期间置 evidence.queued（巡检豁免），出队才挂墙钟——排队不烧预算。
// 返回 runCtx（挂好墙钟的执行 ctx）、runCancel（墙钟释放）、release（名额归还，
// 调用方 defer）、err（排队期 ctx 取消——树态与父通知由取消方收口，调用方走
// "未执行"分支直接收尾，不调 runSubAgent）。
func (d *Dispatcher) enterExecGate(baseCtx context.Context, ev *activityEvidence, subAgentID string, wallClock time.Duration) (context.Context, context.CancelFunc, func(), error) {
	runCancel := context.CancelFunc(func() {})
	release := func() {}
	if d.concPool == nil {
		runCtx := baseCtx
		if wallClock > 0 {
			runCtx, runCancel = context.WithTimeout(baseCtx, wallClock)
		}
		return runCtx, runCancel, release, nil
	}
	if ev != nil {
		ev.markQueued()
	}
	if err := d.concPool.Acquire(baseCtx, subAgentID); err != nil {
		if ev != nil {
			ev.clearQueued(time.Now().UnixNano())
		}
		return baseCtx, runCancel, release, err
	}
	if ev != nil {
		ev.clearQueued(time.Now().UnixNano())
	}
	runCtx := baseCtx
	if wallClock > 0 {
		runCtx, runCancel = context.WithTimeout(baseCtx, wallClock)
	}
	return runCtx, runCancel, d.concPool.Release, nil
}
```

(d) `config.go` `AgentConfig`（PeerRequestTimeoutMin 后）加：

```go
	// MaxConcurrentSubAgents 同时在跑的子 Agent 全局上限（2026-09-28 P1 并发池）。
	// 默认 8；超额派发 FIFO 排队（排队不计墙钟）。<=0 按默认；显式不限需注释掉默认值。
	MaxConcurrentSubAgents int `yaml:"max_concurrent_sub_agents"`
```

`applyAgentStandaloneDefaults` 加：

```go
	// 子 Agent 全局并发上限（2026-09-28 P1）：默认 8，超顶排队不拒绝。
	if c.Agent.MaxConcurrentSubAgents == 0 {
		c.Agent.MaxConcurrentSubAgents = 8
	}
```

(e) `bootstrap.go`（WithPeerRequestTimeout 接线行后）加：

```go
	// 子 Agent 全局并发池：同时在跑总数上限，超额 FIFO 排队（排队不计墙钟）。
	subAgentDispatcher.WithConcurrencyLimit(cfg.Agent.MaxConcurrentSubAgents)
```

- [x] **Step 7: dispatchOne 接入（墙钟移位 + gate）**

`dispatcher.go` dispatchOne 段做如下修改（:3305-3450 区）：

(a) ctx 构造（:3352-3355）由：

```go
	cancel := context.CancelFunc(func() {})
	if effectiveTimeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, effectiveTimeout)
	}
```

改为：

```go
	// 墙钟移位（2026-09-28 P1）：派发点只建可取消基底，WithTimeout 挪到并发池
	// 出队后（enterExecGate）——排队不烧墙钟预算；树/巡检的 cancel 句柄绑基底
	//（baseCancel），排队期取消 Acquire 立即返回，无空窗。
	baseCtx, baseCancel := context.WithCancel(subAgentCtx)
```

(b) :3371 `t.SetCancel(subAgentID, cancel)` 改 `t.SetCancel(subAgentID, baseCancel)`。

(c) :3400 `subAgentMeta{cancel: cancel, ...}` 改 `cancel: baseCancel`。

(d) goroutine（:3411-3450 区）改造——`defer cancel()` 改 `defer baseCancel()`；`paused := d.runSubAgent(subAgentCtx, ...)` 一行改为：

```go
		paused := false
		runCtx, runCancel, release, gateErr := d.enterExecGate(baseCtx, ev, subAgentID, effectiveTimeout)
		if gateErr != nil {
			// 排队期被取消（cancel_agent/会话停止/巡检）：树态与父通知由取消方收口，
			// 此处只走正常尾部清理（trackChildDone 经下方 doneOnce 兜底），不调 runSubAgent。
			log.Printf("[subagent] gate-abort: sub=%s err=%v（排队期被取消）", subAgentID, gateErr)
		} else {
			defer runCancel()
			defer release()
			paused = d.runSubAgent(runCtx, parentID, subAgentID, *roleDef, task, domain, responsibility, mode, verifyKind, started)
		}
```

（goroutine 内其余部分——recover defer、CompareAndDelete、聚合兜底、`if !paused` 的 doneOnce 尾部——原样保留。注意 `ev` 变量已在 :3402 定义，直接用。）

- [x] **Step 8: ReviveWithMessage 与 fork 同模式接入**

(a) `dispatcher.go` `ReviveWithMessage`（:1299-1305）由：

```go
	effectiveTimeout := d.effectiveWallClock(roleDef.ID, 0)
	var cancel context.CancelFunc = func() {}
	if effectiveTimeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, effectiveTimeout)
	}
```

改为：

```go
	effectiveTimeout := d.effectiveWallClock(roleDef.ID, 0)
	baseCtx, baseCancel := context.WithCancel(subAgentCtx)
```

随后 :1308/:1313 的 `cancel()` 改 `baseCancel()`；:1316 `t.SetCancel(subAgentID, cancel)` 改 `baseCancel`；:1355 subAgentMeta `cancel: cancel` 改 `cancel: baseCancel`；goroutine（:1363-1376）`defer cancel()` 改 `defer baseCancel()`，`paused := d.runSubAgent(subAgentCtx, ...)` 改：

```go
		paused := false
		runCtx, runCancel, release, gateErr := d.enterExecGate(baseCtx, ev, subAgentID, effectiveTimeout)
		if gateErr != nil {
			log.Printf("[subagent] revive gate-abort: sub=%s err=%v", subAgentID, gateErr)
		} else {
			defer runCancel()
			defer release()
			paused = d.runSubAgent(runCtx, parentID, subAgentID, *roleDef, seed, node.Domain, "", agent.ModeReact, "", started)
		}
```

（`ev` 在该函数 :1357 已定义。）

(b) `failure_prune.go:160-170`（fork 的 WithTimeout 段）与 :204（subAgentMeta）、:192（SetCancel）、:212-222（goroutine）做完全相同的模式替换（`ev` 在 :206 已定义）。先读 `failure_prune.go:140-172` 确认变量名（应为 `cancel`/`subAgentCtx`/`effectiveTimeout`）。

- [x] **Step 9: ResumePaused 接入**

`dispatcher.go:5023-5026` 由：

```go
	cancel := context.CancelFunc(func() {})
	if d.timeout > 0 {
		subCtx, cancel = context.WithTimeout(subCtx, d.timeout)
	}
```

改为：

```go
	baseCtx, baseCancel := context.WithCancel(subCtx)
```

`grep -n "cancel" backend/internal/domain/subagent/dispatcher.go | awk -F: '$1>5026 && $1<5075'` 找出 5027-5075 区间内 `cancel` 的剩余引用（`t.Resume(pausedNodeID, cancel)`、`defer cancel()`），全部改 `baseCancel`。

`:5031` 的 activity 注册改为持有引用：

```go
	ev := newEvidence()
	d.activity.Store(pausedNodeID, ev)
```

`:5044` 的 `result, err := sub.RunWithHistory(subCtx, "继续", msgs)` 前插入 gate：

```go
	// 并发池准入（P1）：resume 是完整任务执行，同样占名额；排队期 ctx 取消按
	// 续跑失败收口（树 Failed + 通知父 + 计数递减），与下方 err 分支同构。
	runCtx, runCancel, release, gateErr := d.enterExecGate(baseCtx, ev, pausedNodeID, d.timeout)
	if gateErr != nil {
		d.treeFinish(subCtx, pausedNodeID, "", gateErr)
		d.notify(parentID, pausedNodeID, formatSubAgentFailure(subCtx, gateErr, agent.ReactResult{}, d.timeout, ""), nil)
		d.trackChildDone(parentID)
		return agent.ReactResult{}, gateErr
	}
	defer runCancel()
	defer release()

	result, err := sub.RunWithHistory(runCtx, "继续", msgs)
```

- [x] **Step 10: 热驻 domain 执行段接入（驻车不占名额）**

`idle_pool.go` `runDomainEngine`（:1018-1050）：在 `d.running.Store(s.id, agentInst)`（:1035）之后、执行调用（:1044/:1046/:1049）之前插入：

```go
	// 并发池准入（P1）：热驻槽只在真正跑任务时占名额，park/idle 不占。
	// 墙钟传 0——槽任务墙钟由 slot timer 管理（dispatchOne :3317-3318 注释）。
	ev := d.activityEvidenceFor(s.id)
	runCtx, runCancel, release, gateErr := d.enterExecGate(taskCtx, ev, s.id, 0)
	if gateErr != nil {
		d.running.Delete(s.id)
		return agent.ReactResult{}, gateErr
	}
	defer runCancel()
	defer release()
```

并把 :1039-1049 的 `agentInst.RunWithHistory(taskCtx, ...)`/`d.runEngine(taskCtx, ...)` 三处 `taskCtx` 改 `runCtx`。

- [x] **Step 11: 构建 + 全量子 agent 测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/domain/subagent/ ./internal/agent/`
Expected: PASS

人工核查点：
- `grep -n "context.WithTimeout(subAgentCtx" backend/internal/domain/subagent/*.go`——dispatchOne/Revive/fork 三处应已无残留（其余 ctx 上的 WithTimeout 不在本次范围）。
- `map_sub_agents`/`call_sub_agents` 批量路径都经 dispatchOne → 自动受池约束，确认无第二条 goroutine 直跑路径（`grep -n "go func" backend/internal/domain/subagent/dispatcher.go` 扫一遍，除测试/helper 外不应有绕过 enterExecGate 的 runSubAgent 调用——已知例外：ExecuteChild :5374 是 verifyloop 未接线原型，不动）。

- [x] **Step 12: 检查点**

报告变更文件清单与测试结果。

---
### Task 7: 看板 PG 持久化（快照写穿 + 惰性恢复）

**Files:**
- Modify: `backend/internal/board/board.go`（`Store` 接口、`Manager.WithStore`、`GetOrCreate` 恢复路径、`TaskBoard.persistFn` + mutator 收口、`BoardFromSnapshot`、`Snapshot` 加 `Seq`）
- Create: `backend/internal/store/board_store.go`
- Modify: `backend/internal/store/schema.go`（追加 `EnsureBoardSchema`）
- Modify: `backend/internal/store/postgres_store.go`（加 `Board` 字段 + 装配）
- Modify: `backend/internal/store/session_store.go:204-212`（级联清单加 `session_boards`）
- Modify: `backend/internal/bootstrap/bootstrap.go:1163-1174`（ensureSchemas 注册）+ `:628` 附近（`rt.Boards.WithStore` 接线 + 适配器）
- Create: `migrations/011_session_boards.sql`
- Test: `backend/internal/board/board_persist_test.go`（新建，fake store 单测）
- Test: `test/api/board_store_integration_test.go`（新建，`//go:build integration`）

**Interfaces:**
- Produces:
  - `board.Store` 接口：`LoadBoard(topicID string) (Snapshot, bool, error)` + `SaveBoard(snap Snapshot) error`
  - `(*board.Manager) WithStore(s Store) *Manager`
  - `board.BoardFromSnapshot(s Snapshot) *TaskBoard`
  - `board.Snapshot` 新增字段 `Seq int64 \`json:"seq"\``
  - `store.NewBoardStore(db *sql.DB) *BoardStore`，方法 `SaveBoard(ctx, sessionID, goal string, snapshot []byte) error`、`LoadBoard(ctx, sessionID string) (goal string, snapshot []byte, found bool, err error)`
  - `store.EnsureBoardSchema(ctx, db) error`
  - `PostgresStore.Board *BoardStore`

分层注意：`board.Store` 接口定义在 board 包；`store.BoardStore` 只用 `[]byte`/string 出参（不 import board）；JSON 编解码适配器放 bootstrap（Global Constraints 的分层纪律）。

- [x] **Step 1: 写 board 持久化单测（失败先行）**

新建 `backend/internal/board/board_persist_test.go`：

```go
package board

import (
	"errors"
	"testing"
)

// fakeStore 内存实现 board.Store，记录调用次数。
type fakeStore struct {
	snap     Snapshot
	found    bool
	saveErr  error
	saveCnt  int
	loadCnt  int
}

func (f *fakeStore) LoadBoard(topicID string) (Snapshot, bool, error) {
	f.loadCnt++
	return f.snap, f.found, nil
}

func (f *fakeStore) SaveBoard(snap Snapshot) error {
	f.saveCnt++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.snap = snap
	f.found = true
	return nil
}

// mutator 触发写穿：SetPlan/Assign/MarkDone 后 store 收到最新快照。
func TestBoardPersistWriteThrough(t *testing.T) {
	fs := &fakeStore{}
	m := NewManager().WithStore(fs)
	b := m.GetOrCreate("sess-1", "goal")
	if err := b.SetPlan("goal", []PlanTask{{ID: "t1", Title: "任务一", Domain: "后端"}, {ID: "t2", Title: "任务二", Domain: "测试", DependsOn: []string{"t1"}}}); err != nil {
		t.Fatal(err)
	}
	if fs.saveCnt == 0 {
		t.Fatal("SetPlan 未触发持久化")
	}
	_ = b.Assign("t1", "sess-1/domain-1")
	_ = b.MarkDone("t1", "done")
	if fs.saveCnt < 3 {
		t.Fatalf("Assign/MarkDone 应各触发一次持久化：%d", fs.saveCnt)
	}
	if fs.snap.Tasks[0].Status != TaskDone {
		t.Fatalf("快照应含最新状态：%+v", fs.snap.Tasks[0])
	}
	// 持久化失败不影响内存行为（fail-open）。
	fs.saveErr = errors.New("pg down")
	if err := b.MarkBlocked("t2", "等依赖"); err != nil {
		t.Fatalf("持久化失败不应影响看板操作：%v", err)
	}
	fs.saveErr = nil
}

// 重启恢复：新 Manager（空内存）经 GetOrCreate 从 store 惰性重建，状态/依赖/seq 完整。
func TestBoardRestoreFromStore(t *testing.T) {
	fs := &fakeStore{}
	m1 := NewManager().WithStore(fs)
	b1 := m1.GetOrCreate("sess-1", "goal")
	_ = b1.SetPlan("goal", []PlanTask{{ID: "t1", Title: "任务一", Domain: "后端"}, {ID: "t2", Title: "任务二", Domain: "测试", DependsOn: []string{"t1"}}})
	_ = b1.MarkDone("t1", "ok")
	autoID := b1.AddSubTask("自动编号任务") // 占用 seq

	// 模拟重启：新 Manager，同一 store。
	m2 := NewManager().WithStore(fs)
	b2 := m2.GetOrCreate("sess-1", "ignored")
	if !b2.DependsDone("t2") {
		t.Fatal("t1 已 done，t2 依赖应就绪")
	}
	got := b2.Snapshot()
	if got.Status != BoardStatusInProgress || len(got.Tasks) != 3 {
		t.Fatalf("恢复快照不符：%+v", got)
	}
	// seq 恢复：新自动编号不与恢复的 autoID 冲突。
	newID := b2.AddSubTask("再自动编号")
	if newID == autoID {
		t.Fatalf("seq 未恢复，ID 冲突：%s", newID)
	}
}

// 无 store 时纯内存（旧行为不变）。
func TestManagerWithoutStore(t *testing.T) {
	m := NewManager()
	b := m.GetOrCreate("sess-x", "g")
	_ = b.SetPlan("g", []PlanTask{{ID: "t1", Title: "x", Domain: "d"}})
	if !b.DependsDone("t1") {
		t.Fatal("纯内存行为回归")
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/board/ -run 'TestBoardPersist|TestBoardRestore|TestManagerWithoutStore' -v`
Expected: FAIL（编译错误：WithStore/Store 不存在）

- [x] **Step 3: board.go 改造**

(a) `Snapshot` struct（:549-556）加字段：

```go
	Seq         int64             `json:"seq"`           // 自动编号计数器（AddSubTask 的 _t<n> 后缀水位）
```

`Snapshot()` 方法返回处（:583-590）加 `Seq: b.seq.Load(),`。

(b) `TaskBoard` struct（:88-99）加字段：

```go
	persistFn   func(Snapshot)    // 持久化回调（Manager.WithStore 接线时注入）；nil=纯内存
```

(c) 加持久化方法与 Store 接口（文件尾部、`Manager` 定义前）：

```go
// persist 持久化切面（2026-09-28 P1 看板持久化）：每个 mutator 成功后调用。
// 快照经 RLock 拷贝后回调；回调失败由实现方 fail-open（log），不影响内存行为。
func (b *TaskBoard) persist() {
	if b.persistFn == nil {
		return
	}
	b.persistFn(b.Snapshot())
}

// Store 看板持久化接口（bootstrap 适配到 store.BoardStore；board 包不依赖 store——
// 分层纪律）。实现必须幂等（SaveBoard 为 UPSERT）。
type Store interface {
	LoadBoard(topicID string) (Snapshot, bool, error)
	SaveBoard(snap Snapshot) error
}
```

(d) `AddSubTask`（:138-161）：`defer b.mu.Unlock()` 改为显式解锁后持久化——函数尾部 `return id` 前改为：

```go
	b.mu.Unlock()
	b.persist()
	return id
```

（删除原 `defer b.mu.Unlock()`，幂等返回已有 ID 的 early return 分支（:143-145）也改为显式 `b.mu.Unlock(); return id`——幂等命中无状态变化，不调 persist。）

(e) `SetConstraint`（:174-179）同模式：`defer` 去掉，尾部 `b.mu.Unlock(); b.persist()`。

(f) `SetPlan`（:192-288）：`defer b.mu.Unlock()` 去掉；所有校验失败的 `return err` 前显式 `b.mu.Unlock()`（不调 persist）；成功路径尾部（:287 `return nil`）改：

```go
	b.mu.Unlock()
	b.persist()
	return nil
```

(g) `TakeoverFrom`（:333-361）：尾部改为：

```go
	migrated := 0
	now := time.Now()
	for _, id := range b.Order {
		t := b.Tasks[id]
		if t == nil || t.Domain != oldDomain || t.Status == TaskDone {
			continue
		}
		// ……原有迁移逻辑不动……
		migrated++
	}
	if migrated > 0 {
		b.UpdatedAt = now
	}
	b.mu.Unlock()
	if migrated > 0 {
		b.persist()
	}
	return migrated
```

（去 `defer b.mu.Unlock()`；迁移循环体原样保留，上面省略号处照抄现有代码。）

(h) `Assign`（:395-407）：去 defer；`task not found` 分支显式 Unlock 后 return err；成功尾部 `b.mu.Unlock(); b.persist(); return nil`。

(i) `transition`（:486-499）：同 (h)——成功尾部 `b.mu.Unlock(); b.persist(); return nil`（recomputeStatusLocked 仍在持锁区内完成，顺序：`b.recomputeStatusLocked()` → Unlock → persist）。

(j) 新增 `BoardFromSnapshot`（`Snapshot()` 方法后）：

```go
// BoardFromSnapshot 从持久化快照重建看板（2026-09-28 P1：重启后依赖门状态恢复）。
// seq 取快照水位与任务 ID 后缀（_t<n>）解析的最大值，防自动编号冲突
//（write_plan 自定义 ID 无 _t 后缀，解析失败跳过即可）。
func BoardFromSnapshot(s Snapshot) *TaskBoard {
	b := NewTaskBoard(s.TopicID, s.Goal)
	b.Status = s.Status
	for k, v := range s.Constraints {
		b.Constraints[k] = v
	}
	b.UpdatedAt = s.UpdatedAt
	maxSeq := s.Seq
	for _, t := range s.Tasks {
		tc := t // 值拷贝，防切片元素复用共享
		b.Tasks[t.ID] = &tc
		b.Order = append(b.Order, t.ID)
		if i := strings.LastIndex(t.ID, "_t"); i >= 0 {
			if n, err := strconv.ParseInt(t.ID[i+2:], 10, 64); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	b.seq.Store(maxSeq)
	return b
}
```

（board.go 需补 `strconv` import。）

(k) `Manager` struct（:668-671）加字段 + WithStore + GetOrCreate 恢复：

```go
type Manager struct {
	mu     sync.RWMutex          // 读写锁保护 boards map
	boards map[string]*TaskBoard // 按 topicID 索引的看板表
	store  Store                 // 持久化（nil=纯内存，测试/未接线）
}

// WithStore 注入持久化（bootstrap 接 store.BoardStore 适配器）。重复注入后者覆盖。
func (m *Manager) WithStore(s Store) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = s
	return m
}

// persist 是注入各 TaskBoard 的持久化回调（持 Manager 读锁取 store，fail-open）。
func (m *Manager) persist(snap Snapshot) {
	m.mu.RLock()
	s := m.store
	m.mu.RUnlock()
	if s == nil {
		return
	}
	if err := s.SaveBoard(snap); err != nil {
		log.Printf("[board] persist failed: topic=%s err=%v", snap.TopicID, err)
	}
}
```

`GetOrCreate`（:718-727）替换为：

```go
// GetOrCreate 取或新建看板；内存 miss 且接了 store 时先从持久层恢复
//（重启后依赖门/任务状态随之找回），恢复失败/无记录才新建。
func (m *Manager) GetOrCreate(topicID, goal string) *TaskBoard {
	m.mu.Lock()
	defer m.mu.Unlock() // 恢复路径 PG 查询在锁内（惰性触发、低频，换取单创建语义）
	if b, ok := m.boards[topicID]; ok {
		return b
	}
	if m.store != nil {
		if snap, found, err := m.store.LoadBoard(topicID); err != nil {
			log.Printf("[board] restore failed: topic=%s err=%v（按新建处理）", topicID, err)
		} else if found {
			b := BoardFromSnapshot(snap)
			b.persistFn = m.persist
			m.boards[topicID] = b
			return b
		}
	}
	b := NewTaskBoard(topicID, goal)
	b.persistFn = m.persist
	m.boards[topicID] = b
	return b
}
```

（board.go 需补 `log` import。）

- [x] **Step 4: 跑 board 单测**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/board/ -v`
Expected: PASS（含既有看板测试全部回归）

- [x] **Step 5: store.BoardStore + schema + 装配 + 级联 + 迁移副本**

(a) 新建 `backend/internal/store/board_store.go`：

```go
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
```

(b) `schema.go` 尾部追加：

```go
// EnsureBoardSchema 自动创建 session_boards 表 (幂等)。
// 对应 migrations/011_session_boards.sql：看板整板快照持久化（2026-09-28），
// 重启后依赖门状态可恢复；owner 多租户预留；删除仅会话级联。
func EnsureBoardSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS session_boards (
    owner      VARCHAR(64) NOT NULL DEFAULT '',
    session_id VARCHAR(64) PRIMARY KEY,
    goal       TEXT,
    snapshot   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`)
	return err
}
```

(c) `postgres_store.go`：struct 加 `Board *BoardStore // 会话任务看板持久化（011_session_boards.sql）`；`NewPostgresStore` 装配区加 `s.Board = NewBoardStore(db)`。

(d) `bootstrap.go:1163-1174` ensureSchemas map 加：

```go
		"session_boards":        store.EnsureBoardSchema,
```

(e) `session_store.go:204-212` `sessionScopedTables` 追加 `"session_boards"`。

(f) 新建 `migrations/011_session_boards.sql`：

```sql
-- 011_session_boards.sql: 会话任务看板持久化（2026-09-28，P1 资源治理）
-- 运行时由 store.EnsureBoardSchema 幂等建表，本文件为文档性副本。
CREATE TABLE IF NOT EXISTS session_boards (
    owner      VARCHAR(64) NOT NULL DEFAULT '',
    session_id VARCHAR(64) PRIMARY KEY,
    goal       TEXT,
    snapshot   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- [x] **Step 6: bootstrap 接线（适配器 + WithStore）**

`bootstrap.go` `:628` `agentSvc.SetBoard(rt.Boards.Get)` 前加：

```go
	// 看板持久化（2026-09-28 P1）：整板快照写穿 session_boards，重启惰性恢复。
	rt.Boards.WithStore(boardStoreAdapter{st: store.NewBoardStore(pgStore.DB())})
```

`bootstrap.go` 文件尾部加适配器：

```go
// boardStoreAdapter 把 store.BoardStore（[]byte 出入参）适配为 board.Store
//（Snapshot 出入参）——分层纪律：store 不 import board，JSON 编解码在此收口。
type boardStoreAdapter struct {
	st *store.BoardStore
}

func (a boardStoreAdapter) LoadBoard(topicID string) (board.Snapshot, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	goal, raw, found, err := a.st.LoadBoard(ctx, topicID)
	if err != nil || !found {
		return board.Snapshot{}, found, err
	}
	var snap board.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return board.Snapshot{}, false, fmt.Errorf("decode board snapshot %s: %w", topicID, err)
	}
	if snap.Goal == "" {
		snap.Goal = goal
	}
	return snap, true, nil
}

func (a boardStoreAdapter) SaveBoard(snap board.Snapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode board snapshot %s: %w", snap.TopicID, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.st.SaveBoard(ctx, snap.TopicID, snap.Goal, raw)
}
```

（`encoding/json`/`board` import 按需补；bootstrap.go 已有 `context`/`time`/`fmt`/`store`。）

- [x] **Step 7: 写集成测试（看板 PG 往返）**

新建 `test/api/board_store_integration_test.go`（fixtures 模式同 Task 2 的 mailbox 集成测试）：

```go
//go:build integration

package api_test

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
)

// session_boards：UPSERT 覆盖 + LoadBoard 往返 + 无记录 found=false。
func TestBoardStoreRoundtrip(t *testing.T) {
	db := /* 照既有集成测试取 *sql.DB */ nil
	if db == nil {
		t.Fatal("fixtures PG 不可用")
	}
	ctx := context.Background()
	if err := store.EnsureBoardSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	bs := store.NewBoardStore(db)
	if _, _, found, err := bs.LoadBoard(ctx, "it-board-none"); err != nil || found {
		t.Fatalf("空表应 found=false：found=%v err=%v", found, err)
	}
	if err := bs.SaveBoard(ctx, "it-board-1", "goal-v1", []byte(`{"goal":"goal-v1","seq":2}`)); err != nil {
		t.Fatal(err)
	}
	// UPSERT 覆盖。
	if err := bs.SaveBoard(ctx, "it-board-1", "goal-v2", []byte(`{"goal":"goal-v2","seq":3}`)); err != nil {
		t.Fatal(err)
	}
	goal, raw, found, err := bs.LoadBoard(ctx, "it-board-1")
	if err != nil || !found {
		t.Fatalf("LoadBoard 失败：found=%v err=%v", found, err)
	}
	if goal != "goal-v2" || string(raw) != `{"goal":"goal-v2","seq":3}` {
		t.Fatalf("UPSERT 未覆盖：goal=%q raw=%s", goal, raw)
	}
}
```

- [x] **Step 8: 构建 + 测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/board/ ./internal/store/`
Expected: PASS

Run: `cd test && GOTOOLCHAIN=local go test -tags integration ./api/ -run 'TestBoardStoreRoundtrip|TestMailboxStoreRoundtrip' -v`
Expected: PASS（需 fixtures PG；无 docker 则记录"未跑"）

- [x] **Step 9: 检查点**

报告变更文件清单与测试结果。

---

### Task 8: 文档收尾 + 全量回归

**Files:**
- Modify: `CLAUDE.md`（修漂移 + 记新机制）
- Modify: `config/config.yaml`（agent 段注释补两个新 key）
- Modify: `doc/变更.md`（追加变更记录）
- Modify: `docs/superpowers/specs/2026-09-28-multiagent-reliability-design.md` 无需改；本计划文件勾选完成项

- [x] **Step 1: CLAUDE.md 修漂移 + 补新机制**

(a) 找到「跨 Agent 协作问答」行（"叶子角色无 `send_message`（工具面未变）"），改为：

```
叶子角色也有 `send_message`（roles.yaml 8 个叶子均含），受可见性矩阵限制范围（叶子仅上级 domain+同域同级）。
```

(b) 在该条目末尾追加两行新机制说明：

```
- **mailbox 持久化与配对（2026-09-28）**：邮件 Send 同步双写 `mailbox_messages`（PG），Drain 标 read、Purge 标 dead，重启经 LoadUnread+Restore 重投未读；request/escalate 自动登记 pending 注册表，reply 按 `reply_to`（来信注入头 `[mailbox from X id=…]`）显式销账或自动配对最近未答，`peer_request_timeout_min`（默认 15）超时给提问方父 Agent 投 escalate；Purge 丢弃的未读 request/escalate 给发送方回投「未送达」死信通知。广播桶已删（Send 拒收空/`*` 收件人）。
- **子 Agent 全局并发池（2026-09-28）**：`max_concurrent_sub_agents`（默认 8）限制同时在跑总数，超额 FIFO 排队（拒绝会叠重试成拒绝循环）；排队不计墙钟（WithTimeout 出队才挂）、巡检经 evidence.queued 豁免；池只做准入，生命周期权威仍是 Tree。看板整板快照写穿 `session_boards`，重启经 GetOrCreate 惰性恢复（依赖门不再丢）。
```

(c) CLAUDE.md「**新端点**」相关段不动；`包依赖` 段不动。

- [x] **Step 2: config.yaml 注释**

`config/config.yaml` agent 段找到 `sub_agent_timeout_min` 附近，补两行（带注释）：

```yaml
  # 子 Agent 全局并发上限（同时在跑总数，超额 FIFO 排队、排队不计墙钟）
  max_concurrent_sub_agents: 8
  # 协作问答超时升级（分钟）：request/escalate 超期未获 reply 升级给提问方父 Agent
  peer_request_timeout_min: 15
```

（若 config.yaml 该段无对应 key 先例则加到 agent: 段尾部，保持缩进。）

- [x] **Step 3: doc/变更.md 追加记录**

文件头部（最新条目处，照既有格式）追加：

```markdown
## 2026-09-28 多 Agent 可靠性改造（P0 消息可靠性 + P1 资源治理）

- mailbox 持久化：新表 `mailbox_messages`（store.EnsureMailboxSchema / migrations/010），Send 同步双写、Drain 标 read、Purge 标 dead、重启 LoadUnread+Restore 重投未读（reactSessionStore.buildRestoredSession 链路，含懒恢复）；广播桶（DrainBroadcast/Forward）删除，Send 拒收空/`*` 收件人。
- request/reply 机制配对：ReplyTo 改指原消息 ID（旧实现误填发送方 agentID）；request/escalate 登记 pendingRequests 注册表，reply 显式 reply_to 销账或按 (回复方,被回复方,thread) 自动配对最近未答；patrol 每 tick sweep，超 `peer_request_timeout_min`（默认 15）未答给提问方父 Agent 投 escalate；恢复重投经 mailbox.Restore → RegisterRestoredPending 重建（deadline 顺延）。注入头带 id/thread/reply_to（mailboxMessageToReact）。prompts.Version 20260921-2 → 20260928-1。
- Purge 死信通知：Purge 返回被丢弃未读，dispatcher.purgeMailboxWithNotice 给 request/escalate 发送方回投「未送达」+ pending 销账；会话级删除（PurgeSession/finalizeSession）只标 dead 不通知。
- 子 Agent 全局并发池：`max_concurrent_sub_agents`（默认 8），自研 FIFO 票据队列（subagent/pool.go，零新依赖）；排队不计墙钟（WithTimeout 移出队后）、evidence.queued 巡检豁免；接入 dispatchOne/ReviveWithMessage/fork/ResumePaused/runDomainEngine 五条执行路径。池只做准入，生命周期权威仍是 Tree。
- 看板持久化：新表 `session_boards`（migrations/011）整板快照 JSONB 写穿，Manager.WithStore + GetOrCreate 惰性恢复（含 seq 水位）；Snapshot 加 Seq 字段。
- 级联删除清单（sessionScopedTables）补 mailbox_messages、session_boards。
- CLAUDE.md 漂移修正：叶子角色实际有 send_message（可见性矩阵限范围）。
```

- [x] **Step 4: 全量回归**

Run: `cd backend && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test ./...`
Expected: PASS（既有全部测试 + 新增）

Run: `cd test && GOTOOLCHAIN=local go test ./...`
Expected: PASS（纯单测；integration 套件需 PG+Redis 容器，能跑则 `GOTOOLCHAIN=local go test -tags integration ./...`，不能跑如实记录）

Run: `cd web && pnpm build`（前端零改动，仅确认未被波及；若本机无 pnpm 则跳过并注明）
Expected: PASS 或注明跳过

- [x] **Step 5: 终检报告**

向用户报告：全部变更文件清单、测试结果、两个新配置 key、两张新表、未跑项（如 integration/docker/web）。

---

## 附：任务依赖图

```
T1 mailbox 包 ──→ T2 store+schema ──→ T3 bootstrap 接线+恢复重投
     │                                    │
     └──→ T4 pending 配对（依赖 T1 的 Restore handler/自动 ThreadID；接线在 T3 后）
              └──→ T5 Purge 死信通知（依赖 T1 的 Purge 返回 + T4 的 pendingReqs）
T6 并发池（与 T1-T5 正交，可插队先做；注意与 T4/T5 同改 dispatcher.go 的文件冲突）
T7 看板持久化（与 T1-T6 正交，除 bootstrap.go/schema.go/session_store.go 文件级冲突外无依赖）
T8 文档收尾（最后做，汇总全部）
```

建议执行顺序：T1 → T2 → T3 → T4 → T5 → T6 → T7 → T8（单线最稳，dispatcher.go/bootstrap.go 集中改动不并行）。

## 附：自审记录（2026-09-28）

- **对规格①的偏离（有意）**：规格写「`mailbox.Reopen`（复活路径）顺带重载该 agent 的未读邮件」。实施中发现矛盾：Purge 现在会把丢弃的未读经 deadMark 标 `dead` 且发送方已获死信通知（T5）——被 Purge 的 agent 在 PG 里已**没有** unread 行可重载，Reopen 重载是空操作。故该条不做，以「Purge 标 dead + 死信通知」为准（语义更闭环：消息有明确归宿而非复活后幽灵重现）。
- 规格其余条目（①新表/双写/恢复重投、②配对/超时升级、③删广播、④并发池/墙钟移位/巡检豁免/可配置、⑤看板写穿/惰性恢复/级联）均有对应 Task 覆盖。
- 命名一致性已核：`enterExecGate`、`purgeMailboxWithNotice`、`RegisterRestoredPending`、`pendingRequestRegistry`（add/complete/matchAuto/expire/purgeSession）、`execPool`（Acquire/Release/Stats/StatsQueued）、`board.Store`/`BoardFromSnapshot`、`store.MailboxStore`（Save/MarkRead/MarkDead/LoadUnread）、`store.BoardStore`（SaveBoard/LoadBoard）、`MaxConcurrentSubAgents`、`PeerRequestTimeoutMin` 跨 Task 一致。
