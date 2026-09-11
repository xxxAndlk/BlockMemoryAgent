# Agent 编排页（层级树图 + 单 Agent 对话页 + 用户直连）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把任务看板的 Agent 编排升级为全宽"编排"页：层级树图（自绘 SVG）+ 点击节点查看任意 Agent 的完整对话与交互留痕 + 用户直发消息（waiting 注入唤醒 / done/failed 复活重跑 / running 禁用并复用中断终止）。

**Architecture:** 后端复用现有编排内核，只做增量：① `waitForChildren` 上报 `child_wait` 展示态（不刷 lastTS、不冒泡）；② `Mailbox.Send` 加 trace 钩子把邮件双写 `agent_events` 留痕；③ ReActAgent 每条入史消息热写 Redis（TTL 24h、cap 500），PG `agent_messages` 在子 Agent 终态/暂停全量落库；④ 新端点 `GET .../agents/:aid/messages`（Redis 热层 + PG 合并分页）与 `POST .../agents/:aid/message`（状态机路由：waiting 注入+poke 唤醒 / 终态 `Tree.Reopen`+同 ID 重跑 / running 409）；⑤ 提示词补"用户直连消息"处置规程。前端新增 `?view=orch` 主区视图：左树图（tidy-tree 布局 + SVG 连线 + HTML 节点卡）右对话面板（消息流 + 邮件留痕 + 三态发送框）；看板"任务目标"改为时间线。

**Tech Stack:** Go 1.x（Gin / go-redis/v9 / database/sql）、Vue 3.4 + TS + Element Plus 2.7 + Tailwind 3.4。零新依赖。

**设计细化（对已批准 spec 的一处修正）：** spec §5 写"PG 每 20 条批量 flush"。实现改为**运行期只写 Redis 热层，PG 在终态/暂停一次性全量落库**——`SaveMessages` 是 delete-then-insert 全量覆盖语义，每 20 条重写是 O(n²) 浪费；Redis 承担运行期热读与崩溃缓冲（其持久化由部署侧 AOF 保证），终态后 PG 为权威。功能效果与 spec 一致（对话页任意时刻可读全量）。

## Global Constraints

- 不新增任何前/后端依赖；不动 `orchestrator.Status` 枚举、SSE 通道、压缩金字塔、mailbox 投递语义。
- 一切可观测性写入（Redis 热写、trace 留痕、终态落库）必须 best-effort：失败仅记日志，绝不阻塞或失败 ReAct 主循环。
- Redis 不可用（`RedisStore.AgentMsg` 为 nil 或调用报错）时全链路降级：写侧跳过、读侧回退 PG，端点不报错。
- 代码注释用中文，遵循各文件现有注释风格（说明"为什么"）。
- 后端测试：`cd backend && go test ./...`（新增测试用例不依赖真实 PG/Redis——沿用 nil-DB no-op 与 fake 模式，参照 `messages_store_test.go`、`dispatcher_pause_test.go`）。
- 前端无测试框架：每个前端任务以 `cd web && pnpm build`（含 vue-tsc 类型检查）为验收，再加手动验证步骤。
- **git 策略（2026-09-11 用户拍板）：全程不做任何 git 提交**——各任务末尾的 "Commit" 步骤仅作变更归集参考（界定本任务改了哪些文件），执行时跳过；评审用工作区 diff，提交由用户事后自行处理。

---

### Task 1: `child_wait` 展示态（等待下级返回的可识别信号）

**Files:**
- Modify: `backend/internal/agent/react_agent.go`（`waitForChildren`，约 1503-1526 行）
- Modify: `backend/internal/domain/subagent/dispatcher.go`（`activityReporterFn`，约 624-656 行）
- Test: `backend/internal/agent/react_agent_wait_test.go`（新建）
- Test: `backend/internal/domain/subagent/activity_childwait_test.go`（新建）

**Interfaces:**
- Consumes: `ReActAgent.touchActivity(kind string)`（react_agent.go:408）、`PendingChildrenChecker` 接口（react_types.go:260）、`Dispatcher.activityReporterFn(agentID)`、`ActivityEvidenceOf(agentID) (kind string, ago time.Duration, ok bool)`。
- Produces: 活动证据新 kind 值 `"child_wait"`——经 `ListAgents` 的 `ActivityKind` 字段流出（service_react.go:1827-1831 已有管线，零改动）。语义约定：**只换展示 kind，不刷 `lastTS`（不续命）、不向上冒泡**——等子期间存活判定仍完全由后代活动冒泡决定，避免"后代全灭、父干等"被展示态掩盖。

- [ ] **Step 1: 写失败测试（agent 侧上报）**

新建 `backend/internal/agent/react_agent_wait_test.go`：

```go
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakePendingChecker 第一次 PendingChildren 返回 1（进入等待），第二次归 0 退出循环。
type fakePendingChecker struct{ calls int }

func (f *fakePendingChecker) PendingChildren(string) int {
	if f.calls == 0 {
		f.calls++
		return 1
	}
	return 0
}
func (f *fakePendingChecker) WaitForAnyChild(string, time.Duration) bool { return true }

// TestWaitForChildrenReportsChildWait 验证 waitForChildren 进入等待时上报 child_wait
// 展示态（编排页"等待下级返回"标识的数据源）。
func TestWaitForChildrenReportsChildWait(t *testing.T) {
	var kinds []string
	a := NewReActAgent("session-1/domain-1", types.RoleDefinition{ID: "domain"}, nil, nil).
		WithPendingChildrenChecker(&fakePendingChecker{}).
		WithActivityReporter(func(k string) { kinds = append(kinds, k) })
	_, paused := a.waitForChildren(context.Background(), nil)
	if paused {
		t.Fatal("无 Paused 子节点时不应返回 paused=true")
	}
	found := false
	for _, k := range kinds {
		if k == "child_wait" {
			found = true
		}
	}
	if !found {
		t.Fatalf("waitForChildren 未上报 child_wait: %v", kinds)
	}
}
```

- [ ] **Step 2: 写失败测试（dispatcher 侧特判语义）**

新建 `backend/internal/domain/subagent/activity_childwait_test.go`：

```go
package subagent

import (
	"testing"
	"time"
)

// TestActivityReporterChildWait 验证 child_wait 是"纯展示态"：
// 只换 lastKind 供 ActivityEvidenceOf 读取；不刷新 lastTS（不续命）、不冒泡——
// 等子 Agent 的存活判定不受展示态上报干扰。
func TestActivityReporterChildWait(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.activity.Store("session-1/domain-1", newEvidence())

	report := d.activityReporterFn("session-1/domain-1")
	e := d.activityEvidenceFor("session-1/domain-1")
	before := e.lastTS.Load()
	time.Sleep(time.Millisecond)

	report("child_wait")

	kind, _, ok := d.ActivityEvidenceOf("session-1/domain-1")
	if !ok || kind != "child_wait" {
		t.Fatalf("ActivityEvidenceOf kind=%q ok=%t；want child_wait", kind, ok)
	}
	if got := e.lastTS.Load(); got != before {
		t.Fatalf("child_wait 不应刷新 lastTS: before=%d after=%d", before, got)
	}

	// 普通 kind 仍走 report 主路径（回归保护）。
	report("llm_start")
	if !e.llmInFlight.Load() {
		t.Fatal("llm_start 应置 llmInFlight")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd backend && go test ./internal/agent/ -run TestWaitForChildrenReportsChildWait -v && go test ./internal/domain/subagent/ -run TestActivityReporterChildWait -v`
Expected: 两个均 FAIL（child_wait 未上报 / lastKind 未变）。

- [ ] **Step 4: 实现**

`backend/internal/agent/react_agent.go` 的 `waitForChildren` 循环体顶部（`for` 之后第一行）加：

```go
	for a.pendingChecker.PendingChildren(a.name) > 0 {
		// 展示态上报（编排页"等待下级返回"标识）：dispatcher 侧特判只换 lastKind，
		// 不刷 lastTS、不冒泡，等子存活性仍由后代活动冒泡决定。
		a.touchActivity("child_wait")
```

`backend/internal/domain/subagent/dispatcher.go` 的 `activityReporterFn` 改为：

```go
func (d *Dispatcher) activityReporterFn(agentID string) func(kind string) {
	return func(kind string) {
		now := time.Now().UnixNano()
		if kind == "child_wait" {
			// 展示态专用（编排页等待下级标识，waitForChildren 上报）：
			// 仅更新 lastKind 供 ActivityEvidenceOf 读取；不刷新 lastTS（不续命）、
			// 不向上冒泡——等子期间存活判定仍由后代活动冒泡与既有心跳阈值决定，
			// 避免"全部后代已死、父在干等"被展示态刷新误判为合法存活。
			if e := d.activityEvidenceFor(agentID); e != nil {
				e.lastKind.Store(kind)
			}
			return
		}
		if e := d.activityEvidenceFor(agentID); e != nil {
			e.report(kind, now)
		}
		d.bubbleActivity(agentID, now)
	}
}
```

- [ ] **Step 5: 跑测试确认通过 + 回归**

Run: `cd backend && go test ./internal/agent/ -run TestWaitForChildren -v && go test ./internal/domain/subagent/ -run TestActivityReporter -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 6: Commit**

```bash
git add backend/internal/agent/react_agent.go backend/internal/agent/react_agent_wait_test.go backend/internal/domain/subagent/dispatcher.go backend/internal/domain/subagent/activity_childwait_test.go
git commit -m "feat: agent 等待下级展示态 child_wait（不续命不冒泡）"
```

---

### Task 2: mailbox 留痕（邮件双写 agent_events）

**Files:**
- Modify: `backend/internal/mailbox/mailbox.go`（`Mailbox` struct 约 99-129 行、`Send` 约 137-171 行）
- Modify: `backend/internal/bootstrap/bootstrap.go`（memoryPipeline 构造之后，约 285-293 行）
- Test: `backend/internal/mailbox/trace_test.go`（新建）

**Interfaces:**
- Consumes: `memory.Pipeline.Write(agentID string, event agent.MemoryEvent) error`（pipeline.go:426）、`agent.MemoryEvent{Type, AgentID, Role, Content, ToolName, Input, Occurred}`（react_memory.go:15-33）。
- Produces: `(*mailbox.Mailbox).WithTrace(fn func(*Message))`——投递成功（含广播桶）后持锁外同步调用；死信不触发。留痕事件格式：`Type="mailbox"`, `Role=msg.From`, `Content=Subject+"\n"+Body`, `ToolName=string(msg.Type)`, `Input=msg.To`。收发双方各写一行（From 为 "user"/"dispatcher" 时只写收件方行）。查询侧（Task 5）依赖此格式。

- [ ] **Step 1: 写失败测试**

新建 `backend/internal/mailbox/trace_test.go`：

```go
package mailbox

import "testing"

// TestSendTraceHook 验证 WithTrace 钩子：定向投递与广播投递成功后各触发一次；
// 死信（收件人已 Purge）不触发；钩子收到的副本与 inbox 中消息解耦。
func TestSendTraceHook(t *testing.T) {
	m := New()
	var traced []*Message
	m.WithTrace(func(msg *Message) { traced = append(traced, msg) })

	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	if len(traced) != 1 || traced[0].To != "b" || traced[0].From != "a" {
		t.Fatalf("定向投递应留痕 1 条, got %+v", traced)
	}
	if _, err := m.Send(&Message{From: "a", To: "*", Type: MsgInfo, Subject: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(traced) != 2 {
		t.Fatalf("广播应留痕, got %d", len(traced))
	}
	m.Purge("b")
	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "x"}); err == nil {
		t.Fatal("已销毁收件人应死信")
	}
	if len(traced) != 2 {
		t.Fatalf("死信不应留痕, got %d", len(traced))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && go test ./internal/mailbox/ -run TestSendTraceHook -v`
Expected: FAIL（`WithTrace` 未定义，编译错误）。

- [ ] **Step 3: 实现**

`mailbox.go` 的 `Mailbox` struct 加字段（放在 `seq` 字段后）：

```go
	// trace 可选的发送留痕回调（编排页 Agent 间交互留痕数据源）：Send 投递成功
	//（含广播桶）后持锁外调用；nil 时零行为。实现方必须 best-effort 非阻塞。
	trace func(*Message)
```

新增方法（放在 `New()` 之后）：

```go
// WithTrace 注入发送留痕回调（编排页"Agent 间交互留痕"数据源）。重复注入后者覆盖前者。
func (m *Mailbox) WithTrace(fn func(*Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trace = fn
}
```

`Send` 的加锁段改为（把 `defer m.mu.Unlock()` 改为显式解闭锁，死信分支先解锁再返回；尾部持锁外触发 trace）：

```go
	// 加写锁保护 inbox/bcast 的写入。
	m.mu.Lock()
	if msg.To == "" || msg.To == "*" {
		// 无明确收件人或广播：进入广播桶，交主 Agent 决议。
		m.bcast = append(m.bcast, msg)
	} else {
		// 定向投递：目标已销毁则返回死信错误，不入箱。
		if _, ok := m.closed[msg.To]; ok {
			m.mu.Unlock()
			return "", fmt.Errorf("%w: %s", ErrRecipientClosed, msg.To)
		}
		// 定向投递：追加到目标 Agent 的收件箱末尾。
		m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	}
	trace := m.trace
	m.mu.Unlock()
	// 投递成功后留痕（持锁外 + 值拷贝：防回调慢/再入 mailbox 死锁，防调用方后续改 msg）。
	if trace != nil {
		cp := *msg
		trace(&cp)
	}
	return id, nil
```

`backend/internal/bootstrap/bootstrap.go`：在 `memoryPipeline := memory.NewPipeline(...)` 链式构造结束之后（约 293 行）插入：

```go
	// mailbox 留痕（编排页 Agent 间交互留痕）：每封邮件收发双方各写一条 agent_events
	//（type=mailbox），随会话删除一并清理；写失败仅记日志，不影响投递。
	sharedMailbox.WithTrace(func(msg *mailbox.Message) {
		writeRow := func(agentID string) {
			if agentID == "" || agentID == "*" {
				return
			}
			if err := memoryPipeline.Write(agentID, agent.MemoryEvent{
				Type:     "mailbox",
				AgentID:  agentID,
				Role:     msg.From,
				Content:  msg.Subject + "\n" + msg.Body,
				ToolName: string(msg.Type),
				Input:    msg.To,
				Occurred: msg.CreatedAt,
			}); err != nil {
				log.Printf("[mailbox] trace write failed: agent=%s err=%v", agentID, err)
			}
		}
		writeRow(msg.To)
		// user/dispatcher 是系统侧发送者（非 Agent），不作为发送方行落库。
		if msg.From != "user" && msg.From != "dispatcher" {
			writeRow(msg.From)
		}
	})
```

（bootstrap.go 已 import `log`、`mailbox`、`agent`；若 `agent` 未 import 则补 `"github.com/blockmemory/agent/backend/internal/agent"`。）

- [ ] **Step 4: 跑测试确认通过 + 编译**

Run: `cd backend && go test ./internal/mailbox/ -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/mailbox/mailbox.go backend/internal/mailbox/trace_test.go backend/internal/bootstrap/bootstrap.go
git commit -m "feat: mailbox 发送留痕双写 agent_events（编排页交互留痕）"
```

---

### Task 3: Agent 消息热层 Redis 存储

**Files:**
- Create: `backend/internal/store/redis_agentmsg.go`
- Modify: `backend/internal/store/redis.go`（`RedisStore` struct 约 17-66 行）
- Test: `backend/internal/store/redis_agentmsg_test.go`（新建）

**Interfaces:**
- Produces:
  - `store.AgentMsgEntry{Seq int; At string; Role string; Content string; ToolCallID string; ToolCalls string; Reasoning string}`（ToolCalls 为 JSON 字符串）
  - `(*store.AgentMsgRedisStore).AppendMsg(ctx, agentID string, e AgentMsgEntry) error`
  - `(*store.AgentMsgRedisStore).TailMsg(ctx, agentID string, limit int) ([]AgentMsgEntry, error)`（seq 升序）
  - `(*store.AgentMsgRedisStore).BeforeMsg(ctx, agentID string, beforeSeq, limit int) ([]AgentMsgEntry, error)`（seq<beforeSeq 的最近 limit 条，升序；beforeSeq<=0 等价 TailMsg）
  - `(*store.AgentMsgRedisStore).AfterMsg(ctx, agentID string, afterSeq, limit int) ([]AgentMsgEntry, error)`（seq>afterSeq 的最早 limit 条，升序，增量轮询用）
  - 全部 nil-receiver / nil-client 安全（返回 nil, nil）。key 形如 `sess:{sessionID}:agent:{agentID}:msgs`。
  - `RedisStore` 新字段 `AgentMsg *AgentMsgRedisStore`（构造函数内装配）。
- Consumes: go-redis `Pipeline/RPush/Expire/LTrim/LRange`（参照 redis_event.go 模式）。

- [ ] **Step 1: 写失败测试**

新建 `backend/internal/store/redis_agentmsg_test.go`：

```go
package store

import (
	"context"
	"testing"
)

// TestAgentMsgRedisStore_NilNoOp 验证 nil store 全部方法 no-op 不 panic（测试/未接线场景）。
func TestAgentMsgRedisStore_NilNoOp(t *testing.T) {
	var s *AgentMsgRedisStore
	if err := s.AppendMsg(context.Background(), "session-1/domain-1", AgentMsgEntry{Seq: 0}); err != nil {
		t.Fatalf("nil AppendMsg 应 no-op, got %v", err)
	}
	if got, err := s.TailMsg(context.Background(), "session-1/domain-1", 10); err != nil || got != nil {
		t.Fatalf("nil TailMsg 应 nil,nil, got %v,%v", got, err)
	}
	if got, err := s.BeforeMsg(context.Background(), "session-1/domain-1", 5, 10); err != nil || got != nil {
		t.Fatalf("nil BeforeMsg 应 nil,nil, got %v,%v", got, err)
	}
	if got, err := s.AfterMsg(context.Background(), "session-1/domain-1", 5, 10); err != nil || got != nil {
		t.Fatalf("nil AfterMsg 应 nil,nil, got %v,%v", got, err)
	}
}

// TestAgentMsgKey 验证 key 派生：MetaAgent==sessionID；子 Agent 取 "/" 前段为会话段。
func TestAgentMsgKey(t *testing.T) {
	cases := map[string]string{
		"session-1":                        "sess:session-1:agent:session-1:msgs",
		"session-1/domain-2":               "sess:session-1:agent:session-1/domain-2:msgs",
		"session-42/domain-1/code_assistant-3": "sess:session-42:agent:session-42/domain-1/code_assistant-3:msgs",
	}
	for in, want := range cases {
		if got := agentMsgKey(in); got != want {
			t.Errorf("agentMsgKey(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAgentMsgWindowSlicing 验证 Before/After 窗口切片纯逻辑（经内存切片，无需真实 Redis）。
func TestAgentMsgWindowSlicing(t *testing.T) {
	all := make([]AgentMsgEntry, 0, 10)
	for i := 0; i < 10; i++ {
		all = append(all, AgentMsgEntry{Seq: i})
	}
	if got := beforeWindow(all, 7, 2); len(got) != 2 || got[0].Seq != 5 || got[1].Seq != 6 {
		t.Fatalf("beforeWindow(7,2) 应得 seq[5,6], got %+v", got)
	}
	if got := afterWindow(all, 7, 2); len(got) != 2 || got[0].Seq != 8 || got[1].Seq != 9 {
		t.Fatalf("afterWindow(7,2) 应得 seq[8,9], got %+v", got)
	}
	if got := afterWindow(all, 9, 5); len(got) != 0 {
		t.Fatalf("afterWindow(9,5) 应空, got %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && go test ./internal/store/ -run 'TestAgentMsg' -v`
Expected: FAIL（类型/函数未定义，编译错误）。

- [ ] **Step 3: 实现**

新建 `backend/internal/store/redis_agentmsg.go`：

```go
package store

// redis_agentmsg.go Agent 消息热层（编排页对话视图数据源）：
// 运行期每条入史消息逐条 RPUSH，TTL 24h、cap 最近 500 条；PG agent_messages
// 在子 Agent 终态/暂停全量落库（权威），Redis 承担运行期热读与崩溃缓冲。
// 读侧（agent.agentMessagesQueryResult）热层 miss/不足时回退 PG 切片。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// agentMsgHotTTL 热层 key 过期时间（每次 Append 重置）。
	agentMsgHotTTL = 24 * time.Hour
	// agentMsgHotCap 热层单 Agent 容量上限（LTRIM 保留最新 N 条）。
	agentMsgHotCap = 500
)

// AgentMsgEntry 是热层中的单条 Agent 消息（字段与 agent_messages 表对齐 + at 时间戳）。
type AgentMsgEntry struct {
	Seq        int    `json:"seq"`
	At         string `json:"at"` // RFC3339Nano
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  string `json:"tool_calls,omitempty"` // JSON 字符串（[]ToolCall），空省略
	Reasoning  string `json:"reasoning,omitempty"`
}

// AgentMsgRedisStore 负责消息热层读写。client 为 nil 时全部方法 no-op（降级 PG 直读）。
type AgentMsgRedisStore struct {
	client *redis.Client
}

// agentMsgKey 生成热层 key：sess:{sessionID}:agent:{agentID}:msgs（sessionID 取 "/" 前段）。
func agentMsgKey(agentID string) string {
	return fmt.Sprintf("sess:%s:agent:%s:msgs", sessionFromAgentID(agentID), agentID)
}

// sessionFromAgentID 从 agentID 派生 sessionID（MetaAgent==sessionID，子 Agent 取 "/" 前段）。
func sessionFromAgentID(agentID string) string {
	for i, r := range agentID {
		if r == '/' {
			return agentID[:i]
		}
	}
	return agentID
}

// AppendMsg 追加一条消息（RPUSH + 重置 TTL + LTRIM 截断，同一 pipeline 三次往返合一）。
func (s *AgentMsgRedisStore) AppendMsg(ctx context.Context, agentID string, e AgentMsgEntry) error {
	if s == nil || s.client == nil || agentID == "" {
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := agentMsgKey(agentID)
	pipe := s.client.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, agentMsgHotTTL)
	pipe.LTrim(ctx, key, -agentMsgHotCap, -1)
	_, err = pipe.Exec(ctx)
	return err
}

// TailMsg 取热层尾部 limit 条（seq 升序）。
func (s *AgentMsgRedisStore) TailMsg(ctx context.Context, agentID string, limit int) ([]AgentMsgEntry, error) {
	if s == nil || s.client == nil || agentID == "" || limit <= 0 {
		return nil, nil
	}
	raw, err := s.client.LRange(ctx, agentMsgKey(agentID), int64(-limit), -1).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentMsgs(raw), nil
}

// BeforeMsg 取 seq < beforeSeq 的最近 limit 条（升序）；beforeSeq<=0 等价 TailMsg。
// 实现为读全量热层（cap 500）后内存切片——热层有界，切片成本可忽略。
func (s *AgentMsgRedisStore) BeforeMsg(ctx context.Context, agentID string, beforeSeq, limit int) ([]AgentMsgEntry, error) {
	if beforeSeq <= 0 {
		return s.TailMsg(ctx, agentID, limit)
	}
	all, err := s.TailMsg(ctx, agentID, agentMsgHotCap)
	if err != nil {
		return nil, err
	}
	return beforeWindow(all, beforeSeq, limit), nil
}

// AfterMsg 取 seq > afterSeq 的最早 limit 条（升序，对话页 3s 增量轮询用）。
func (s *AgentMsgRedisStore) AfterMsg(ctx context.Context, agentID string, afterSeq, limit int) ([]AgentMsgEntry, error) {
	all, err := s.TailMsg(ctx, agentID, agentMsgHotCap)
	if err != nil {
		return nil, err
	}
	return afterWindow(all, afterSeq, limit), nil
}

// beforeWindow 在升序切片中取 seq < beforeSeq 的最近 limit 条（保持升序返回）。
func beforeWindow(all []AgentMsgEntry, beforeSeq, limit int) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if all[i].Seq < beforeSeq {
			out = append(out, all[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// afterWindow 在升序切片中取 seq > afterSeq 的最早 limit 条。
func afterWindow(all []AgentMsgEntry, afterSeq, limit int) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, limit)
	for _, e := range all {
		if e.Seq > afterSeq {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func decodeAgentMsgs(raw []string) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, len(raw))
	for _, r := range raw {
		var e AgentMsgEntry
		if json.Unmarshal([]byte(r), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
```

`backend/internal/store/redis.go`：`RedisStore` struct 加字段（放在 `TTL` 字段后）：

```go
	AgentMsg *AgentMsgRedisStore // Agent 消息热层（编排页对话视图）
```

`NewRedisStore` 返回字面量加一行（放在 `TTL:` 行后）：

```go
		AgentMsg:     &AgentMsgRedisStore{client: client},
```

- [ ] **Step 4: 跑测试确认通过 + 编译**

Run: `cd backend && go test ./internal/store/ -run 'TestAgentMsg' -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/store/redis_agentmsg.go backend/internal/store/redis_agentmsg_test.go backend/internal/store/redis.go
git commit -m "feat: store: Agent 消息 Redis 热层（TTL 24h cap 500，nil 降级）"
```

---

### Task 4: 消息热写接入 ReActAgent + 子 Agent 终态 PG 落库

**Files:**
- Create: `backend/internal/agent/message_log.go`
- Modify: `backend/internal/agent/react_agent.go`（struct 字段 + With 方法 + 全部 `history = append(history, ...)` 站点）
- Modify: `backend/internal/domain/subagent/dispatcher.go`（msgLogger 字段 + `WithMessageLogger` + `runSubAgentOnce` 装配约 3472 行 + `runSubAgent` 五个终态站点 + `saveTerminalHistory`）
- Modify: `backend/internal/agent/service_react.go`（`msgLogger` 字段 + `SetAgentMsgCache` + runSession/resumeSession 两处 MetaAgent 构造链，约 2427 与 2454 行）
- Modify: `backend/internal/bootstrap/bootstrap.go`（约 633 行 WithMessagesStore 之后）
- Test: `backend/internal/agent/message_log_test.go`（新建）
- Test: `backend/internal/domain/subagent/dispatcher_terminal_save_test.go`（新建）

**Interfaces:**
- Produces:
  - `agent.MessageLogger` 接口：`Log(agentID string, seq int, msg ReactMessage)`（best-effort，失败内部消化）
  - `agent.NewMessageLogger(msgs *store.AgentMsgRedisStore) MessageLogger`（msgs 为 nil 返回 nil）
  - `(*ReActAgent).WithMessageLogger(l MessageLogger) *ReActAgent`
  - `(*Dispatcher).WithMessageLogger(l agent.MessageLogger) *Dispatcher`
  - `(*ReactService).SetAgentMsgCache(c *store.AgentMsgRedisStore)`（同时构建 meta 用 msgLogger；nil 安全）
  - `(*Dispatcher).saveTerminalHistory(ctx, subAgentID, sid string, history []agent.ReactMessage)`（savePausedHistory 语义复用包装）
- Consumes: `store.AgentMsgRedisStore`（Task 3）、`sanitizeUTF8`（agent 包已有）、`savePausedHistory`（dispatcher.go:3147）。
- 约定：seq = 消息入史前 `len(history)`，与 PG `agent_messages.seq`（全量覆盖时按下标写）口径一致。

- [ ] **Step 1: 写失败测试**

新建 `backend/internal/agent/message_log_test.go`：

```go
package agent

import (
	"fmt"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeMsgLogger 记录 Log 调用。
type fakeMsgLogger struct{ logs []string }

func (f *fakeMsgLogger) Log(agentID string, seq int, msg ReactMessage) {
	f.logs = append(f.logs, fmt.Sprintf("%s#%d:%s", agentID, seq, msg.Role))
}

// TestAppendLoggedHotLog 验证 appendLogged：追加同时按 seq 热写；nil logger 零行为不 panic。
func TestAppendLoggedHotLog(t *testing.T) {
	fl := &fakeMsgLogger{}
	a := NewReActAgent("session-1/domain-1", types.RoleDefinition{ID: "domain"}, nil, nil).WithMessageLogger(fl)
	h := a.appendLogged(nil, ReactMessage{Role: "user", Content: "hi"})
	h = a.appendLogged(h, ReactMessage{Role: "assistant", Content: "ok"})
	if len(h) != 2 {
		t.Fatalf("history 长度应 2, got %d", len(h))
	}
	if len(fl.logs) != 2 || fl.logs[0] != "session-1/domain-1#0:user" || fl.logs[1] != "session-1/domain-1#1:assistant" {
		t.Fatalf("热写序列不符: %v", fl.logs)
	}

	a2 := NewReActAgent("x", types.RoleDefinition{ID: "domain"}, nil, nil)
	if got := a2.appendLogged(nil, ReactMessage{Role: "user"}); len(got) != 1 {
		t.Fatal("nil logger 时 append 应正常")
	}
}

// TestNewMessageLoggerNil 验证 nil Redis store → 返回 nil（调用方判空跳过）。
func TestNewMessageLoggerNil(t *testing.T) {
	if got := NewMessageLogger(nil); got != nil {
		t.Fatalf("nil store 应返回 nil logger, got %v", got)
	}
}
```

新建 `backend/internal/domain/subagent/dispatcher_terminal_save_test.go`（env 复用 dispatcher_pause_test.go 的 `newPauseTestEnv` / `dispatchCtx` / `waitForCond` / `tokenUsageProvider`）：

```go
package subagent

// dispatcher_terminal_save_test.go 验证子 Agent 终态（成功/部分回灌）把完整 history
// 落 agent_messages（编排页对话视图 PG 全量源），复用 pause 测试的 captureMessagesStore。

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestRunSubAgent_TerminalSavesHistory 叶子助手成功收尾时应有一次 SaveMessages 落库
//（区别于既有 pause 路径——这是新增的终态落库）。
func TestRunSubAgent_TerminalSavesHistory(t *testing.T) {
	d, _, _, msgStore, tr, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "leaf done"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "write x",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	waitForCond(t, "leaf terminal", func() bool {
		n, ok := tr.Get(subID)
		return ok && (n.Status == orchestrator.StatusDone)
	})
	found := false
	for _, s := range msgStore.saved {
		if s.agentID == subID {
			found = true
		}
	}
	if !found {
		t.Fatalf("终态应落库 history, saved=%+v", msgStore.saved)
	}
}
```

（注意：该用例 tokenUsageProvider 带 200 token 触发 LimitReached 走"部分回灌"分支，同样覆盖终态落库断言；若实现后失败信息指向其他分支，按实际分支断言 SaveMessages 被调用即可——核心验收是**终态必落库**。）

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && go test ./internal/agent/ -run 'TestAppendLogged|TestNewMessageLogger' -v && go test ./internal/domain/subagent/ -run TestRunSubAgent_TerminalSavesHistory -v`
Expected: FAIL（appendLogged 未定义；终态未落库）。

- [ ] **Step 3: 实现**

新建 `backend/internal/agent/message_log.go`：

```go
package agent

// message_log.go 编排页对话视图的消息热层写入：ReActAgent 每条入史消息经
// MessageLogger 热写 Redis（best-effort，2s 超时，失败仅记日志——绝不影响
// ReAct 主循环）。读取侧见 query_react.go agentMessagesQueryResult。

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// MessageLogger 是 Agent 消息热写接口。实现必须 best-effort：错误内部消化。
type MessageLogger interface {
	Log(agentID string, seq int, msg ReactMessage)
}

// redisMessageLogger 把消息序列化后写入 store.AgentMsgRedisStore。
type redisMessageLogger struct {
	msgs *store.AgentMsgRedisStore
}

// NewMessageLogger 创建 Redis 热层消息记录器；msgs 为 nil 时返回 nil（调用方判空跳过）。
func NewMessageLogger(msgs *store.AgentMsgRedisStore) MessageLogger {
	if msgs == nil {
		return nil
	}
	return &redisMessageLogger{msgs: msgs}
}

func (l *redisMessageLogger) Log(agentID string, seq int, msg ReactMessage) {
	if l == nil || agentID == "" {
		return
	}
	calls := ""
	if len(msg.ToolCalls) > 0 {
		if b, err := json.Marshal(msg.ToolCalls); err == nil {
			calls = string(b)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.msgs.AppendMsg(ctx, agentID, store.AgentMsgEntry{
		Seq:        seq,
		At:         time.Now().Format(time.RFC3339Nano),
		Role:       msg.Role,
		Content:    sanitizeUTF8(msg.Content),
		ToolCallID: msg.ToolCallID,
		ToolCalls:  calls,
		Reasoning:  sanitizeUTF8(msg.ReasoningContent),
	})
	if err != nil {
		log.Printf("[agent] msg hot-log failed: agent=%s seq=%d err=%v", agentID, seq, err)
	}
}
```

`react_agent.go`：
1. struct 加字段（放 `suspendGate` 字段后）：

```go
	// msgLogger 可选的消息热层记录器（编排页对话视图）：每条入史消息同步热写。
	// nil 时零行为。
	msgLogger MessageLogger
```

2. 新增方法（放 `WithSuspendGate` 后）：

```go
// WithMessageLogger 注入消息热层记录器（编排页对话视图数据源）。传 nil 关闭。
func (a *ReActAgent) WithMessageLogger(l MessageLogger) *ReActAgent {
	a.msgLogger = l
	return a
}
```

3. 新增 helper（放 `touchActivity` 附近）：

```go
// appendLogged 追加一条消息到 history 并同步热写消息日志（编排页对话视图）。
// seq = 追加前 history 长度，与 agent_messages 全量落库的下标口径一致。
func (a *ReActAgent) appendLogged(history []ReactMessage, msg ReactMessage) []ReactMessage {
	if a.msgLogger != nil {
		a.msgLogger.Log(a.name, len(history), msg)
	}
	return append(history, msg)
}
```

4. 把 `react_agent.go` 内**全部** `history = append(history, ...)` 站点替换为 `history = a.appendLogged(history, ...)`（消息内容构造不变）。定位命令：

```bash
grep -n "history = append(history" backend/internal/agent/react_agent.go
```

已知站点（约 7 处）：`:533` userMsg、`:643-645` 与 `:661` nudge、`:670` assistant、`:792-802` tool 结果、`:882-886` 停滞警告、`:1825` drainMailbox 的 `mailboxMessageToReact(m)`。逐处只换 append 调用，不改消息体。（`drainMailbox` 形参叫 `history`、接收者是 `a`，同样适用。）

`dispatcher.go`：
1. struct 加字段（放 `msgStore` 字段后）：

```go
	// msgLogger 消息热层记录器（编排页对话视图）：构造子 Agent 时装配到 ReActAgent。
	msgLogger agent.MessageLogger
```

2. 新增（放 `WithMessagesStore` 后）：

```go
// WithMessageLogger 注入消息热层记录器（编排页对话视图）。nil 时跳过（测试场景）。
func (d *Dispatcher) WithMessageLogger(l agent.MessageLogger) *Dispatcher {
	d.msgLogger = l
	return d
}
```

3. `runSubAgentOnce` 中活动上报装配块（`if e := d.activityEvidenceFor(subAgentID); e != nil {...}`）之后加：

```go
	// 消息热层（编排页对话视图）：逐条热写 Redis，终态全量落 PG（runSubAgent 收尾）。
	if d.msgLogger != nil {
		sub = sub.WithMessageLogger(d.msgLogger)
	}
```

4. 新增 `saveTerminalHistory`（放 `savePausedHistory` 后）：

```go
// saveTerminalHistory 子 Agent 终态落库完整 history（编排页对话视图 PG 全量源）。
// 复用 savePausedHistory 的"脱离取消 ctx + 10s 超时"语义——取消/软停路径 ctx 已取消也能存。
func (d *Dispatcher) saveTerminalHistory(ctx context.Context, subAgentID, sid string, history []agent.ReactMessage) {
	d.savePausedHistory(ctx, subAgentID, sid, history)
}
```

5. `runSubAgent` 五个终态站点各加一行 `d.saveTerminalHistory(...)`（`sid` 在部分分支需现场取 `tool.SessionIDFromContext(ctx)`）：
   - 部分回灌分支：`d.treeFinish(ctx, subAgentID, "部分完成: "+partial, nil)` 之前
   - 硬取消分支（`log.Printf("[subagent] CANCELLED: ...")` 之后、`return false` 之前）
   - 软停止叶子分支：`d.treeFinish(ctx, subAgentID, "软停止部分完成: "+partial, nil)` 之前
   - 失败分支：`d.treeFinishStatus(ctx, subAgentID, partial, treeStatus, failText)` 之前
   - 成功分支：`d.treeFinish(ctx, subAgentID, result.Text, nil)` 之前

   统一形态：`d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)`

`service_react.go`：
1. struct 加字段（放 `skillPool` 字段后）：

```go
	// agentMsgCache/msgLogger 编排页对话视图：Redis 热层读缓存 + meta 消息热写器；
	// 均由 SetAgentMsgCache 一并装配，nil 时读侧回退 PG、写侧跳过。
	agentMsgCache *store.AgentMsgRedisStore
	msgLogger     MessageLogger
```

2. 新增 setter（放 `SetSkillCatalog` 附近）：

```go
// SetAgentMsgCache 注入 Agent 消息 Redis 热层（编排页对话视图）：读侧缓存 + meta 热写器。
// nil 安全：读回退 PG、写关闭。
func (s *ReactService) SetAgentMsgCache(c *store.AgentMsgRedisStore) {
	s.agentMsgCache = c
	s.msgLogger = NewMessageLogger(c)
}
```

3. runSession（约 2427）与 resumeSession（约 2454）两处 MetaAgent 构造链各加一节（放 `WithProviderFunc(...)` 前）：

```go
		WithMessageLogger(s.msgLogger).
```

`bootstrap.go`（约 633 行 `subAgentDispatcher.WithMessagesStore(...)` 之后）：

```go
	// 编排页（Agent 树图 + 单 Agent 对话页）接线：消息热层 + 用户直连通道。
	agentSvc.SetAgentMsgCache(redisStore.AgentMsg)
	subAgentDispatcher.WithMessageLogger(agent.NewMessageLogger(redisStore.AgentMsg))
```

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

Run: `cd backend && go test ./internal/agent/ -run 'TestAppendLogged|TestNewMessageLogger' -v && go test ./internal/domain/subagent/ -run 'TestRunSubAgent|TestRevivePaused|TestResumePaused' -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/agent/message_log.go backend/internal/agent/message_log_test.go backend/internal/agent/react_agent.go backend/internal/agent/service_react.go backend/internal/domain/subagent/dispatcher.go backend/internal/domain/subagent/dispatcher_terminal_save_test.go backend/internal/bootstrap/bootstrap.go
git commit -m "feat: agent 消息逐条热写 Redis + 子 Agent 终态全量落 PG"
```

---

### Task 5: Agent 对话查询端点（GET messages + mails）

**Files:**
- Modify: `backend/internal/agent/types.go`（QueryKind 常量区，约 310-330 行）
- Modify: `backend/internal/agent/query_react.go`（新增查询实现）
- Modify: `backend/internal/agent/service_react.go`（Query switch 加 case，约 1404 行 QueryKindAgentEvents 后）
- Create: `backend/internal/store/agent_mail_trace.go`
- Modify: `backend/internal/server/session_http.go`（HandleSessionAgentEvents 约 460-482 行后）
- Modify: `backend/internal/server/routes.go`（约 26 行 events 路由后）
- Test: `backend/internal/agent/agent_messages_query_test.go`（新建）

**Interfaces:**
- Consumes: `ReactService.agentMsgCache`（Task 4）、`NewPostgresMessagesStore(...).LoadMessages`、`s.store.pgStore`（`*store.PostgresStore`，agentEventsQueryResult 同款判空模式）。
- Produces:
  - `QueryKindAgentMessages = "agent-messages"`，Args `{agent, before_seq, after_seq, limit}`。
  - `(*store.PostgresStore).QueryAgentMailboxTrace(ctx, sessionID, agentID string, limit int) ([]map[string]any, error)`（type='mailbox' 升序，limit<=0 默认 100）。
  - HTTP `GET /api/sessions/:id/agents/:aid/messages?before_seq&after_seq&limit` → `{session_id, agent_id, messages: [{seq, at, role, content, tool_call_id?, tool_calls?, reasoning?}], mails: [{role(from), content(subject\nbody), tool_name(邮件类型), input(to), occurred}]}`。`aid="meta"` 映射为 sessionID。
  - 纯函数 `sliceMessagesWire(msgs []ReactMessage, beforeSeq, afterSeq, limit int) []map[string]any` 与 `entriesToWire(entries []store.AgentMsgEntry) []map[string]any`（agent 包内，供测试）。

- [ ] **Step 1: 写失败测试（切片纯逻辑）**

新建 `backend/internal/agent/agent_messages_query_test.go`：

```go
package agent

import (
	"fmt"
	"testing"
)

func wireSeqs(items []map[string]any) []int {
	out := make([]int, 0, len(items))
	for _, it := range items {
		out = append(out, it["seq"].(int))
	}
	return out
}

// TestSliceMessagesWire 验证 PG 全量切片的三种窗口语义（tail/before/after），
// 与 Redis 热层 beforeWindow/afterWindow 口径一致。
func TestSliceMessagesWire(t *testing.T) {
	msgs := make([]ReactMessage, 0, 10)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, ReactMessage{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 0, 0, 3)); fmt.Sprint(got) != "[7 8 9]" {
		t.Fatalf("tail 3 应 [7 8 9], got %v", got)
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 7, 0, 2)); fmt.Sprint(got) != "[5 6]" {
		t.Fatalf("before 7 limit 2 应 [5 6], got %v", got)
	}
	if got := wireSeqs(sliceMessagesWire(msgs, 0, 7, 5)); fmt.Sprint(got) != "[8 9]" {
		t.Fatalf("after 7 应 [8 9], got %v", got)
	}
	if got := sliceMessagesWire(msgs, 0, 9, 5); len(got) != 0 {
		t.Fatalf("after 9 应空, got %v", got)
	}
}

// TestEntriesToWire 验证热层条目转线型：tool_calls JSON 反序列化为数组，空串省略。
func TestEntriesToWire(t *testing.T) {
	in := []store.AgentMsgEntry{
		{Seq: 0, At: "2026-09-11T00:00:00Z", Role: "user", Content: "hi"},
		{Seq: 1, At: "2026-09-11T00:00:01Z", Role: "assistant", Content: "ok", ToolCalls: `[{"id":"c1","name":"ReadFile","input":{"path":"a.go"}}]`},
	}
	got := entriesToWire(in)
	if len(got) != 2 {
		t.Fatalf("应 2 条, got %d", len(got))
	}
	if _, has := got[0]["tool_calls"]; has {
		t.Fatal("空 tool_calls 不应出现在线型里")
	}
	calls, ok := got[1]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 应反序列化为数组, got %#v", got[1]["tool_calls"])
	}
}
```

（测试文件共两个函数 `TestSliceMessagesWire` + `TestEntriesToWire`；import 需加 `github.com/blockmemory/agent/backend/internal/store`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && go test ./internal/agent/ -run 'TestSliceMessagesWire|TestEntriesToWire' -v`
Expected: FAIL（函数未定义）。

- [ ] **Step 3: 实现**

`backend/internal/store/agent_mail_trace.go`（新建）：

```go
package store

// agent_mail_trace.go mailbox 留痕查询（编排页"交互留痕"数据源）：
// 读 agent_events 中 type='mailbox' 的行（Task 2 trace 钩子双写）。

import (
	"context"
	"fmt"
	"time"
)

// QueryAgentMailboxTrace 取某 Agent 的 mailbox 留痕事件（按时间正序）。
// 字段口径：role=发送方, content=subject\nbody, tool_name=邮件类型, input=接收方。
// limit<=0 默认 100。
func (s *PostgresStore) QueryAgentMailboxTrace(ctx context.Context, sessionID, agentID string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT role, content, tool_name, input, occurred
FROM agent_events
WHERE session_id = $1 AND agent_id = $2 AND type = 'mailbox'
ORDER BY occurred ASC
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
	return out, rows.Err()
}
```

`types.go` QueryKind 常量区加：

```go
	// QueryKindAgentMessages 编排页 Agent 对话视图：Args{"agent","before_seq","after_seq","limit"}，
	// 返回完整消息历史（Redis 热层 + PG 合并分页）+ mailbox 留痕。
	QueryKindAgentMessages = "agent-messages"
```

`query_react.go` 新增（放 `agentEventsQueryResult` 后）：

```go
// agentMessagesQueryResult 编排页 Agent 对话视图数据源：Redis 热层优先（运行中最新），
// miss/不足回退 PG agent_messages 全量切片；附 mailbox 留痕（type=mailbox 事件，双方各一行）。
// agentID 传 "meta" 或空时映射为 sessionID（MetaAgent agentID==sessionID）。
func (s *ReactService) agentMessagesQueryResult(ctx context.Context, sessionID, agentID string, beforeSeq, afterSeq, limit int) Result {
	if agentID == "" || agentID == "meta" {
		agentID = sessionID
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	mails := []map[string]any{}
	if s.store.pgStore != nil {
		if rows, err := s.store.pgStore.QueryAgentMailboxTrace(ctx, sessionID, agentID, 100); err == nil {
			mails = rows
		}
	}
	return Result{Data: map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"messages":   s.readAgentMessages(ctx, agentID, beforeSeq, afterSeq, limit),
		"mails":      mails,
	}}
}

// readAgentMessages 热层优先读消息窗口；热层空（miss/过期/未接线）回退 PG 全量切片。
func (s *ReactService) readAgentMessages(ctx context.Context, agentID string, beforeSeq, afterSeq, limit int) []map[string]any {
	if s.agentMsgCache != nil {
		var entries []store.AgentMsgEntry
		var err error
		switch {
		case afterSeq > 0:
			entries, err = s.agentMsgCache.AfterMsg(ctx, agentID, afterSeq, limit)
		case beforeSeq > 0:
			entries, err = s.agentMsgCache.BeforeMsg(ctx, agentID, beforeSeq, limit)
		default:
			entries, err = s.agentMsgCache.TailMsg(ctx, agentID, limit)
		}
		if err != nil {
			log.Printf("[query] agent msg hot-read failed, fallback PG: agent=%s err=%v", agentID, err)
		} else if len(entries) > 0 {
			return entriesToWire(entries)
		}
	}
	if s.store.pgStore == nil {
		return []map[string]any{}
	}
	all, err := NewPostgresMessagesStore(s.store.pgStore.DB()).LoadMessages(ctx, agentID)
	if err != nil {
		return []map[string]any{}
	}
	return sliceMessagesWire(all, beforeSeq, afterSeq, limit)
}

// sliceMessagesWire PG 全量切片（seq=下标）：after_seq 优先，再次 before_seq，缺省取尾部。
func sliceMessagesWire(msgs []ReactMessage, beforeSeq, afterSeq, limit int) []map[string]any {
	start, end := 0, len(msgs)
	switch {
	case afterSeq > 0:
		start = afterSeq + 1
		if start > len(msgs) {
			start = len(msgs)
		}
		if start+limit < end {
			end = start + limit
		}
	case beforeSeq > 0:
		end = beforeSeq
		if end > len(msgs) {
			end = len(msgs)
		}
		start = end - limit
		if start < 0 {
			start = 0
		}
	default:
		start = len(msgs) - limit
		if start < 0 {
			start = 0
		}
	}
	out := make([]map[string]any, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, reactMessageWire(i, msgs[i]))
	}
	return out
}

// reactMessageWire 单条 ReactMessage 转线型（PG 路径无 at 时间戳，省略该键）。
func reactMessageWire(seq int, m ReactMessage) map[string]any {
	w := map[string]any{
		"seq":     seq,
		"role":    m.Role,
		"content": m.Content,
	}
	if m.ToolCallID != "" {
		w["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		w["tool_calls"] = m.ToolCalls
	}
	if m.ReasoningContent != "" {
		w["reasoning"] = m.ReasoningContent
	}
	return w
}

// entriesToWire 热层条目转线型：tool_calls JSON 反序列化为数组（非法 JSON 降级原文字符串）。
func entriesToWire(entries []store.AgentMsgEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		w := map[string]any{
			"seq":     e.Seq,
			"at":      e.At,
			"role":    e.Role,
			"content": e.Content,
		}
		if e.ToolCallID != "" {
			w["tool_call_id"] = e.ToolCallID
		}
		if e.ToolCalls != "" {
			var calls []any
			if json.Unmarshal([]byte(e.ToolCalls), &calls) == nil {
				w["tool_calls"] = calls
			}
		}
		if e.Reasoning != "" {
			w["reasoning"] = e.Reasoning
		}
		out = append(out, w)
	}
	return out
}
```

（`query_react.go` 需补 import：`encoding/json`、`log`、`github.com/blockmemory/agent/backend/internal/store`。）

`service_react.go` Query switch（`case QueryKindAgentEvents:` 之后）加：

```go
	case QueryKindAgentMessages:
		// 编排页 Agent 对话视图：完整消息历史 + mailbox 留痕。
		agentID, _ := q.Args["agent"].(string)
		beforeSeq, _ := q.Args["before_seq"].(int)
		afterSeq, _ := q.Args["after_seq"].(int)
		limit, _ := q.Args["limit"].(int)
		return s.agentMessagesQueryResult(ctx, sessionID, agentID, beforeSeq, afterSeq, limit), nil
```

`session_http.go`（`HandleSessionAgentEvents` 之后）：

```go
// HandleSessionAgentMessages 处理 GET /api/sessions/{id}/agents/{aid}/messages（编排页对话视图）。
// 返回该实例完整消息历史（热层+PG 合并分页）与 mailbox 留痕；aid=meta 映射为会话主 Agent。
func (m *SessionManager) HandleSessionAgentMessages(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	if instID == "" {
		c.String(http.StatusBadRequest, "缺少 Agent 实例 ID")
		return
	}
	beforeSeq, _ := strconv.Atoi(c.Query("before_seq"))
	afterSeq, _ := strconv.Atoi(c.Query("after_seq"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	res, err := m.agent.Query(c.Request.Context(), id, agent.Query{
		Kind: agent.QueryKindAgentMessages,
		Args: map[string]any{"agent": instID, "before_seq": beforeSeq, "after_seq": afterSeq, "limit": limit},
	})
	if err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, res.Data)
}
```

`routes.go`（`/agents/:aid/events` 行之后）加：

```go
	rg.GET("/sessions/:id/agents/:aid/messages", m.HandleSessionAgentMessages) // 编排页：Agent 对话历史+留痕
```

- [ ] **Step 4: 跑测试确认通过 + 编译**

Run: `cd backend && go test ./internal/agent/ -run 'TestSliceMessagesWire|TestEntriesToWire' -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/store/agent_mail_trace.go backend/internal/agent/types.go backend/internal/agent/query_react.go backend/internal/agent/service_react.go backend/internal/agent/agent_messages_query_test.go backend/internal/server/session_http.go backend/internal/server/routes.go
git commit -m "feat: GET /agents/:aid/messages 编排页对话查询（热层+PG 合并分页+留痕）"
```

---

### Task 6: 复活与注入内核（`Tree.Reopen` + `Dispatcher.InjectUserMessage/ReviveWithMessage`）

**Files:**
- Modify: `backend/internal/domain/orchestrator/tree.go`（`Wake` 之后，约 293 行）
- Test: `backend/internal/domain/orchestrator/tree_test.go`
- Modify: `backend/internal/domain/subagent/dispatcher.go`（`pokeParent` :1022 附近）
- Test: `backend/internal/domain/subagent/dispatcher_revive_test.go`（新建）

**Interfaces:**
- Consumes: `Tree.Resume`(:230)/`Wake`(:276) 的"状态门控+清 Finished+persistNode"模式、`Dispatcher.mailbox`(:148)、`pokeParent`(:1022)、`trackChildStart`(:450)/`trackChildDone`、`subMeta`/`activity.Store(newEvidence())`/`ensurePatrol`(:796)、`registry.Get(roleID) *types.RoleDefinition`(:2342)、`runSubAgent` goroutine 骨架（dispatchOne :2579-2603）、`subAgentWorkDirFor`(:4709)、`tool.StopContextFrom/WithSessionID/WithWorkDir`、`ledger.RecordDispatch`(:2549)。
- Produces:
  - `Tree.Reopen(id string) bool` —— 仅 `Done/Failed/Cancelled/Unverified` → `Running`，清 `Finished`、**保留** `Summary/Err`（作为上一轮留痕与复活种子上下文），其余状态 no-op 返回 false。
  - `Dispatcher.InjectUserMessage(agentID, content string) error` —— 投 `From:"user" Type:MsgRequest` 邮件 + `pokeParent(agentID)` 唤醒。
  - `Dispatcher.ReviveWithMessage(ctx context.Context, node orchestrator.Node, userMsg string) error` —— 同 ID 复活重跑。
- 关键事实：
  - `mailbox.Send` **不唤醒** `WaitForAnyChild` 阻塞方（等待挂在 `getOrCreatePending(selfID).notify` 上），注入后必须显式 `pokeParent(agentID)`——参数即被唤醒者自己的 ID。
  - `runSubAgent` 退出路径 Purge 邮箱：终态节点 inbox 已关闭，复活**不能**走 mailbox，必须 Reopen+重跑；Paused 节点由监控页恢复通道负责，直连一律拒绝（Task 7 状态机）。

- [ ] **Step 1: 写失败测试（Tree.Reopen）**

`tree_test.go` 追加：

```go
// TestTreeReopen 验证复活门控：仅终态（Done/Failed/Cancelled/Unverified）可 Reopen
// 回 Running；Running/Paused/Idle 拒绝；Summary/Err 保留、Finished 清零。
func TestTreeReopen(t *testing.T) {
	tr := NewTree("", nil)
	tr.Register(Node{ID: "a", Role: "domain", Status: StatusRunning})
	if tr.Reopen("a") {
		t.Fatal("Running 不应可复活")
	}
	tr.Finish("a", "干完了", nil)
	if !tr.Reopen("a") {
		t.Fatal("Done 应可复活")
	}
	n, _ := tr.Get("a")
	if n.Status != StatusRunning || !n.Finished.IsZero() {
		t.Fatalf("复活后应 Running 且 Finished 清零, got %+v", n)
	}
	if n.Summary != "干完了" {
		t.Fatalf("复活应保留上轮 Summary 作留痕, got %q", n.Summary)
	}
	if tr.Reopen("missing") {
		t.Fatal("不存在的节点应返回 false")
	}
}
```

（`Tree.Get` 若不存在则用 `Snapshot()` 遍历取节点——先 grep 确认现有取节点方法名，照用。）

- [ ] **Step 2: 确认失败**

Run: `cd backend && go test ./internal/domain/orchestrator/ -run TestTreeReopen -v`
Expected: FAIL（`Reopen` 未定义，编译错误）。

- [ ] **Step 3: 实现 `Tree.Reopen`**

`tree.go`（`Wake` 之后，完全镜像 `Resume`/`Wake` 模式）：

```go
// Reopen 复活终态节点（编排页用户直连"复活重跑"）：仅 Done/Failed/Cancelled/
// Unverified 可复活回 Running；其他状态 no-op 返回 false。清 Finished 恢复运行态，
// 保留 Summary/Err 作为上一轮留痕（复活种子上下文由调用方读取后注入新一轮，
// 下一轮 Finish 时自然覆盖）。
func (t *Tree) Reopen(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	node, ok := t.nodes[id]
	if !ok {
		return false
	}
	switch node.Status {
	case StatusDone, StatusFailed, StatusCancelled, StatusUnverified:
	default:
		return false
	}
	node.Status = StatusRunning
	node.Finished = time.Time{}
	snapshot := *node
	t.persistNode(snapshot)
	return true
}
```

- [ ] **Step 4: 确认通过**

Run: `cd backend && go test ./internal/domain/orchestrator/ -run TestTreeReopen -v`
Expected: PASS。

- [ ] **Step 5: 写失败测试（注入与复活）**

新建 `backend/internal/domain/subagent/dispatcher_revive_test.go`（环境构造参照 `newPauseTestEnv`，dispatcher_pause_test.go:69；provider 用手写 plainText 桩，参照同文件现有桩）：

```go
package subagent

// TestInjectUserMessage 验证：注入即投 From=user 的 MsgRequest 邮件，
// 且 pokeParent 唤醒目标 wait loop（等子中的 Agent 立刻重检邮箱而非等满周期）。
func TestInjectUserMessage(t *testing.T) { /* 构造 dispatcher → InjectUserMessage("sess/child","看看进度") → mailbox.Tail 断言 From=="user" && Type==MsgRequest && Body 含原文；WaitForAnyChild 超时桩验证 poke 即时返回 */ }

// TestReviveWithMessage 验证：终态节点 Reopen 回 Running、种子含原任务+上轮
// Summary+用户消息、父收到"复活返工"通知、ledger 重记派发。
func TestReviveWithMessage(t *testing.T) { /* newPauseTestEnv + plainTextProvider → 直接 Register+Finish 一个终态节点 → ReviveWithMessage → 断言树状态回 Running、父邮箱含复活通知 */ }
```

（两个测试函数的完整实现按上述断言展开；复用 `newPauseTestEnv` 返回的 `*Dispatcher`/`*mailbox.Mailbox`/`*orchestrator.Tree`。）

- [ ] **Step 6: 确认失败**

Run: `cd backend && go test ./internal/domain/subagent/ -run 'TestInjectUserMessage|TestReviveWithMessage' -v`
Expected: FAIL（方法未定义）。

- [ ] **Step 7: 实现注入与复活**

`dispatcher.go`（`pokeParent` 之后）：

```go
// InjectUserMessage 用户直连注入（编排页对话面板）：向目标 Agent 邮箱投一封
// From="user" 的 MsgRequest，并 pokeParent 唤醒其 wait loop——mailbox.Send 本身
// 不唤醒 WaitForAnyChild 阻塞方，不显式 poke 要等满 wait 周期才看到消息。
func (d *Dispatcher) InjectUserMessage(agentID, content string) error {
	if d.mailbox == nil {
		return fmt.Errorf("mailbox 未初始化")
	}
	if _, err := d.mailbox.Send(&mailbox.Message{
		From:    "user",
		To:      agentID,
		Type:    mailbox.MsgRequest,
		Subject: "用户直连消息",
		Body:    content,
	}); err != nil {
		return err
	}
	d.pokeParent(agentID)
	return nil
}

// ReviveWithMessage 复活终态子 Agent 并以用户消息为增量输入同 ID 重跑。
// 种子 = 原任务 + 上轮 Summary/Err + 用户新消息；运行骨架完全镜像 dispatchOne
// （ctx 重建 → Reopen+SetCancel → subMeta/activity/ensurePatrol → goroutine
// runSubAgent → 父 poke+邮件通知 → ledger 重记）。
func (d *Dispatcher) ReviveWithMessage(ctx context.Context, node orchestrator.Node, userMsg string) error {
	roleDef := d.registry.Get(node.Role)
	if roleDef == nil {
		return fmt.Errorf("角色 %s 未注册，无法复活", node.Role)
	}
	parentID := node.ParentID
	subAgentID := node.ID

	subAgentCtx := tool.StopContextFrom(ctx)
	if subAgentCtx == nil {
		subAgentCtx = context.Background()
	}
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	subAgentCtx = tool.WithWorkDir(subAgentCtx, d.subAgentWorkDirFor(ctx))
	effectiveTimeout := d.timeout
	var cancel context.CancelFunc = func() {}
	if effectiveTimeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, effectiveTimeout)
	}

	t := d.treeFn(tool.SessionIDFromContext(subAgentCtx)) // treeFn 用法参照 HasPausedChild
	if t == nil || !t.Reopen(subAgentID) {
		cancel()
		return fmt.Errorf("节点 %s 非终态，不可复活", subAgentID)
	}
	t.SetCancel(subAgentID, cancel)

	// 复活种子：原任务 + 上轮结果留痕 + 用户新指令，让模型明确"这是返工/追加"。
	seed := node.Task + "\n\n【上一轮结果】\n" + node.Summary
	if node.Err != "" {
		seed += "\n【上轮错误】\n" + node.Err
	}
	seed += "\n\n【用户直连消息】\n" + userMsg

	if parentID != "" {
		d.trackChildStart(parentID)
	}
	meta := &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: tool.SessionIDFromContext(subAgentCtx), wallClock: effectiveTimeout}
	d.subMeta.Store(subAgentID, meta)
	if roleDef.ID != "meta" {
		d.activity.Store(subAgentID, newEvidence())
	}
	d.ensurePatrol()
	started := time.Now()
	go func() {
		defer cancel()
		defer d.subMeta.Delete(subAgentID)
		defer d.activity.Delete(subAgentID)
		paused := d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, seed, node.Domain, "", "serial", "", started)
		if !paused && parentID != "" {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()
	// 父感知（提示词 Task 8 配套）：复活返工属调度事实，邮件通知父"等重新回传，勿重复派发"。
	if parentID != "" && d.mailbox != nil {
		_, _ = d.mailbox.Send(&mailbox.Message{
			From: "dispatcher", To: parentID, Type: mailbox.MsgInfo,
			Subject: "子 Agent 复活返工",
			Body:    fmt.Sprintf("子 Agent %s 已被用户直连复活重跑，等待其重新回传，勿重复派发同领域任务。", subAgentID),
		})
		d.pokeParent(parentID)
	}
	if sid := tool.SessionIDFromContext(subAgentCtx); sid != "" {
		d.ledger.RecordDispatch(sid, parentID, subAgentID, node.Domain, truncateRunes(node.Task, 80), "")
	}
	return nil
}
```

实现注意（写码时逐一核对，勿照抄即跑）：
- `runSubAgent` 的 `mode/verifyKind/responsibility` 参数以 dispatchOne 实参为准（上文 `"serial"`/`""`/`""` 为占位，须对照 :2588 调用点修正）；`subAgentMeta` 字段名以定义为准。
- `treeFn` 的确切签名（是否吃 sessionID、返回值形态）参照 `HasPausedChild`(:1042)。
- `truncateRunes` 在 agent 包（react_agent.go:1652）——dispatcher 若无同款 helper 就用现有 taskBrief 截断写法（:2549 附近）。

- [ ] **Step 8: 确认通过 + 全量编译**

Run: `cd backend && go test ./internal/domain/subagent/ -run 'TestInjectUserMessage|TestReviveWithMessage' -v && go build ./...`
Expected: PASS + 编译通过。

- [ ] **Step 9: Commit**

```bash
git add backend/internal/domain/orchestrator/tree.go backend/internal/domain/orchestrator/tree_test.go backend/internal/domain/subagent/dispatcher.go backend/internal/domain/subagent/dispatcher_revive_test.go
git commit -m "feat: 子 Agent 复活与注入内核（Tree.Reopen + InjectUserMessage/ReviveWithMessage）"
```

---

### Task 7: 用户直连端点 `POST /sessions/:id/agents/:aid/message`（状态机路由）

**Files:**
- Modify: `backend/internal/agent/errors.go`（加 `ErrAgentBusy`）
- Modify: `backend/internal/agent/types.go`（加 `AgentMessenger` 接口）
- Modify: `backend/internal/agent/service_react.go`（messenger 字段 / `SetAgentMessenger` / `MessageAgent`）
- Modify: `backend/internal/agent/agent.go`（Agent 门面接口加 `MessageAgent`）
- Modify: `backend/internal/server/session.go`（`agentErrorStatus` :493 加映射）
- Modify: `backend/internal/server/session_http.go`（`HandleSessionAgentMessage`）
- Modify: `backend/internal/server/routes.go`
- Modify: `backend/cmd/server/bootstrap`（接线 `SetAgentMessenger`——确切路径以 Task 5 落点为准）
- Test: `backend/internal/agent/service_react_message_test.go`（新建）

**Interfaces:**
- Consumes: Task 6 的 `InjectUserMessage`/`ReviveWithMessage`；`agentErrorStatus`(:493) 现有 errno→HTTP 映射模式；`HandleSessionAgentEvents` 的 DecodeBody/参数校验模式；`truncateRunes`（react_agent.go:1652）。
- Produces:
  - `AgentMessenger` 接口：`InjectUserMessage(agentID, content string) error` + `ReviveWithMessage(ctx, node orchestrator.Node, userMsg string) error`（dispatcher 已实现，接口即抽象这两方法）。
  - `ReactService.MessageAgent(ctx, sessionID, instID, content string) error` —— 状态机：
    - `running` 且 `activity_kind=="child_wait"` → 注入唤醒；
    - `running` 否则 → `ErrAgentBusy`（前端据此禁用并引导中断/终止）；
    - `Done/Failed/Cancelled/Unverified` → 复活重跑；
    - `Paused/Idle` → `ErrInvalidSessionState`（Paused 走监控页恢复；Idle 经 MetaAgent 正常派发）；
    - meta 实例或空 content → 拒绝（`ErrInvalidSessionState` / 参数错）。
  - HTTP：成功 200；`ErrAgentBusy`/`ErrInvalidSessionState` → 409；其他沿用 `agentErrorStatus`。
- 成功时写会话事件留痕：`addEvent(sess, eventkind.System, "System", "用户直连 <aid>: "+truncateRunes(content, 200), ...)`（addEvent 实参签名以 Task 5 附近现有调用为准），主对话流可见"用户直连了某 Agent"。

- [ ] **Step 1: 写失败测试（状态机路由全分支）**

新建 `service_react_message_test.go`（与 service 同包，直接往 `s.store.sessions` 塞 `reactInternalSession`，构造参照现有 service 测试；`NewReactService(nil, nil, nil, mailbox.New(), agent.NopMemoryPipeline{}, nil)`——确切签名以 Task 4 后的构造函数为准）：

```go
package agent

// fakeMessenger 记录调用分支；fakeActivity 控制 activity_kind。
// TestMessageAgentStateRouting 逐分支断言：
//  - running+child_wait → 走 InjectUserMessage，且 content 透传
//  - running（非 child_wait）→ ErrAgentBusy
//  - Done/Failed/Cancelled/Unverified → 走 ReviveWithMessage（node 透传）
//  - Paused/Idle → ErrInvalidSessionState
//  - instID=="meta" → 拒绝；空 content → 参数错
//  - 成功分支均产生一条 eventkind.System 会话事件
```

- [ ] **Step 2: 确认失败**

Run: `cd backend && go test ./internal/agent/ -run TestMessageAgentStateRouting -v`
Expected: FAIL（`MessageAgent` 未定义）。

- [ ] **Step 3: 实现**

1. `errors.go`：`ErrAgentBusy = errors.New("agent 正在执行任务")`（注释说明：运行中不可直连，前端禁用发送并引导中断/终止）。
2. `types.go`：`AgentMessenger` 接口（两方法，注释引用 Task 6 语义）。
3. `service_react.go`：加 `messenger AgentMessenger` 字段 + `SetAgentMessenger(m AgentMessenger)`（nil 安全：未接线时 MessageAgent 返回 `ErrInvalidSessionState`）；实现 `MessageAgent`：

```go
// MessageAgent 用户直连子 Agent（编排页对话面板发送框）。状态机路由：
// 等子返回（child_wait）→ 邮箱注入+唤醒；执行中 → ErrAgentBusy；终态 → 复活重跑；
// Paused/Idle/meta → 拒绝。成功写一条 System 会话事件留痕。
func (s *ReactService) MessageAgent(ctx context.Context, sessionID, instID, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("消息内容不能为空")
	}
	if instID == "" || instID == "meta" {
		return ErrInvalidSessionState // 主 Agent 走主对话通道，不经此端点
	}
	if s.messenger == nil {
		return ErrInvalidSessionState
	}
	sess, err := s.sessionForWrite(sessionID) // 以现有取会话方法为准（参照 Message 入口）
	if err != nil {
		return err
	}
	// 目标状态：树节点优先，辅以 activity kind 区分 child_wait。
	st, kind := s.agentStatusOf(sess, instID) // 小 helper：ListAgents 同款数据源抽函数，或内联查 treeFn+ActivityEvidenceOf
	switch st {
	case "running":
		if kind == "child_wait" {
			if err := s.messenger.InjectUserMessage(instID, content); err != nil {
				return err
			}
		} else {
			return ErrAgentBusy
		}
	case "done", "failed", "cancelled", "delivered-unverified":
		node := /* 树节点 snapshot（复活需要 Task/Summary/Err/Role/Domain/ParentID） */
		if err := s.messenger.ReviveWithMessage(ctx, node, content); err != nil {
			return err
		}
	default: // paused / idle / 未知
		return ErrInvalidSessionState
	}
	s.addEvent(sess, eventkind.System, "System", "用户直连 "+instID+"："+truncateRunes(content, 200) /* 其余参数以现有调用为准 */)
	return nil
}
```

4. `agent.go` Agent 门面接口加 `MessageAgent`；`go build ./...` 揪出其他实现者（若有 mock/装饰器），逐一补委托。
5. `session.go` `agentErrorStatus`：加 `case errors.Is(err, agent.ErrAgentBusy): return "agent 正在执行任务，发送已禁用（可先中断或终止）", http.StatusConflict`；`ErrInvalidSessionState` 已有映射则复用。
6. `session_http.go`：

```go
// HandleSessionAgentMessage 处理 POST /api/sessions/{id}/agents/{aid}/message（编排页用户直连）。
func (m *SessionManager) HandleSessionAgentMessage(c *gin.Context) {
	id := c.Param("id")
	instID := c.Param("aid")
	var body struct {
		Content string `json:"content"`
	}
	if !DecodeBody(c, &body) { // DecodeBody 确切签名参照同文件现有 handler
		return
	}
	if err := m.agent.MessageAgent(c.Request.Context(), id, instID, body.Content); err != nil {
		msg, status := agentErrorStatus(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
```

7. `routes.go`（`/agents/:aid/messages` 行之后）：`rg.POST("/sessions/:id/agents/:aid/message", m.HandleSessionAgentMessage) // 编排页：用户直连子 Agent`
8. bootstrap：`agentSvc.SetAgentMessenger(subAgentDispatcher)`（变量名以 Task 5 接线处为准）。

- [ ] **Step 4: 确认通过 + 全量编译**

Run: `cd backend && go test ./internal/agent/ -run TestMessageAgentStateRouting -v && go build ./...`
Expected: PASS + 编译通过（重点看门面接口其他实现者是否漏补）。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/agent/errors.go backend/internal/agent/types.go backend/internal/agent/service_react.go backend/internal/agent/agent.go backend/internal/agent/service_react_message_test.go backend/internal/server/session.go backend/internal/server/session_http.go backend/internal/server/routes.go backend/cmd/
git commit -m "feat: POST /agents/:aid/message 用户直连（waiting 注入/终态复活/running 409）"
```

---

### Task 8: 提示词补规程（DomainAgent 用户直连处置 + MetaAgent 复活感知）

**Files:**
- Modify: `backend/pkg/prompts/domain_agent.go`（`DomainAgent` const）
- Modify: `backend/pkg/prompts/meta_agent.go`（`MetaAgent` const）
- Test: `backend/pkg/prompts/prompts_test.go`（新建，包含性断言）

**Interfaces:**
- Consumes: Task 6 的邮件形态——用户直连邮件 `From="user" Subject="用户直连消息"`；复活通知 `From="dispatcher" Subject="子 Agent 复活返工"`。
- Produces: 两段提示词规程（纯文本追加，不改任何代码行为）。
- 设计取舍：用户直连的**处置判断放在子 Agent 提示词**而非硬编码——下游跑偏重派/新需求派新任务/纯信息纳入继续，全靠模型按规程决策，后端只保证邮件必达与唤醒。

- [ ] **Step 1: 写失败测试**

新建 `prompts_test.go`：

```go
package prompts

import (
	"strings"
	"testing"
)

// TestDirectMessageGuidance 钉死两段新规程的存在性（编排页用户直连的提示词契约）。
func TestDirectMessageGuidance(t *testing.T) {
	if !strings.Contains(DomainAgent, "【用户直连消息】") {
		t.Fatal("DomainAgent 缺用户直连消息处置规程")
	}
	for _, kw := range []string{"cancel_agent", "call_sub_agent", "进度"} {
		if !strings.Contains(DomainAgent, kw) {
			t.Fatalf("DomainAgent 用户直连规程缺关键词 %q", kw)
		}
	}
	if !strings.Contains(MetaAgent, "复活返工") {
		t.Fatal("MetaAgent 缺子 Agent 复活返工感知规程")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `cd backend && go test ./pkg/prompts/ -run TestDirectMessageGuidance -v`
Expected: FAIL。

- [ ] **Step 3: 实现（追加两段规程，中文，与现有段落风格一致）**

`domain_agent.go` 在 `DomainAgent` 末尾追加：

```
【用户直连消息】
运行中可能收到 From=user、Subject=「用户直连消息」的邮件：用户绕过主 Agent 直接对你说话。处置规程：
1. 先盘点：检查自身任务进度与下游各子任务进度（必要时翻邮箱留痕与看板），再回答或行动。
2. 分类处置：
   - 下游跑偏/产物不符 → 用 cancel_agent 终止跑偏子 Agent，按用户要求重新派发；
   - 新需求/范围外追加 → 评估后自行完成或用 call_sub_agent 派发新子任务；
   - 纯信息/口径澄清 → 纳入当前任务理解，继续执行；
   - 询问进度 → 盘点后直接在终答/回传摘要中说明。
3. 处置决策写入你的终答与回传父 Agent 的摘要（说明用户指令改变了什么）；用户没有邮箱，
   不要 send_message 给 user——回复内容随你的回传与任务看板留痕呈现。
4. 用户指令优先级高于原任务描述；与原任务冲突时以用户指令为准并在摘要中显式说明变更。
```

（`cancel_agent`/`call_sub_agent`/`send_message` 工具名以 `domain_agent.go` 现有段落里的写法为准——若现有提示词用了不同名字，跟随现有名字。）

`meta_agent.go` 在 `MetaAgent` 末尾追加：

```
【子 Agent 复活返工】
可能收到 From=dispatcher、Subject=「子 Agent 复活返工」的通知：用户直连某个已终态的
子 Agent 并将其复活重跑。处置：知悉即可——该领域任务回到进行中，等其重新回传结果，
期间勿对同领域重复派发；若你正等待该子 Agent，正常等回传即可。
```

- [ ] **Step 4: 确认通过**

Run: `cd backend && go test ./pkg/prompts/ -run TestDirectMessageGuidance -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add backend/pkg/prompts/domain_agent.go backend/pkg/prompts/meta_agent.go backend/pkg/prompts/prompts_test.go
git commit -m "feat: 提示词补用户直连处置与复活返工感知规程"
```

---

### Task 9: 前端 API 层（类型 + 三个新调用）

**Files:**
- Modify: `web/src/api/session.ts`

**Interfaces:**
- Consumes: Task 5 `GET /sessions/:id/agents/:aid/messages`、Task 7 `POST .../message`、现有 `pauseSessionAgent`/`cancelSessionAgent`（api/metrics.ts，已存在——先 grep 确认名字与签名，若 cancel 缺则在此补）。
- Produces:

```ts
// 单条消息（与后端 reactMessageWire 线型对齐）
export interface AgentMessageItem {
  seq: number
  at: string
  role: 'user' | 'assistant' | 'tool'
  content: string
  reasoning?: string
  tool_calls?: { id: string; name: string; input: unknown }[]
  tool_call_id?: string
}
// 一条 mailbox 留痕（与后端 agent_mail_trace 线型对齐）
export interface AgentMailItem {
  at: string
  from: string
  to: string
  msg_type: string
  subject: string
  body: string
}
export interface AgentConversation {
  agent_id: string
  total: number
  messages: AgentMessageItem[]
  mails: AgentMailItem[]
  has_more: boolean     // 头部还有更早消息（before_seq 翻页用）
}
export function getAgentMessages(
  id: string, aid: string,
  opts?: { beforeSeq?: number; afterSeq?: number; limit?: number },
): Promise<AgentConversation>
export function sendAgentMessage(id: string, aid: string, content: string): Promise<void>
```

- 注意：`AgentConversation` 字段名以后端 `agentMessagesQueryResult` 实际返回为准（Task 5 实现后对照修正）；`aid='meta'` 后端映射主 Agent，前端树图点击 meta 节点时禁用对话面板（meta 走主对话页），故正常不传 meta。

- [ ] **Step 1: 实现**（无测试框架，直接写；函数风格跟随 `getSessionAgents` 现有写法——request 封装、URL 拼接、错误透传）

- [ ] **Step 2: 验收**

Run: `cd web && pnpm build`
Expected: vue-tsc 类型检查 + 构建通过。

- [ ] **Step 3: Commit**

```bash
git add web/src/api/session.ts
git commit -m "feat: web api 加 Agent 对话查询与用户直连发送"
```

---

### Task 10: 树图基础（tidy 布局 composable + 节点卡 + SVG 画布）

**Files:**
- Create: `web/src/composables/useTreeLayout.ts`
- Create: `web/src/views/session/orch/AgentTreeNode.vue`
- Create: `web/src/views/session/orch/AgentTreeCanvas.vue`

**Interfaces:**
- Consumes: `AgentNode`（types/index.ts:158——`inst_id/name/type/status/parent_id/activity_kind/last_activity_ago`）；状态小字格式沿用 `useRoleTree.formatActivityEvidence` 的口径（`tool:<名>` → `in <名>`，`llm_start/stream` → `thinking`）。
- Produces:
  - `useTreeLayout(agents: MaybeRef<AgentNode[]>)` → `{ roots: ComputedRef<LayoutNode[]>, edges: ComputedRef<LayoutEdge[]>, width/height: ComputedRef<number> }`
    - `LayoutNode = { id, name, type, status, activityKind, activityText, x, y, depth }`（x/y 为画布逻辑坐标，单位 px）
    - `LayoutEdge = { from: string, to: string, x1,y1,x2,y2 }`（from=父底边中点，to=子顶边中点）
    - 与 useRoleTree 一致：`idle` 节点不入树（热驻复用对用户是噪音）。
  - 常量：`NODE_W=208, NODE_H=76, H_GAP=28, V_GAP=64`。

**布局算法（tidy-tree 两遍法，自绘不引库）：**
1. 建森林：`parent_id` 映射挂子，缺父/父被过滤（idle）的节点升为根；按 `name` 稳定排序保证渲染确定性。
2. 第一遍后序：每个子树返回"单位宽度"= max(1, Σ 子子树宽度)；叶子=1。
3. 第二遍前序：按子树宽度比例分配水平区间，节点居中于自身区间，`y = depth * (NODE_H + V_GAP)`，`x = 区间中点 - NODE_W/2`；区间单位宽 = `NODE_W + H_GAP`。
4. `width/height` 取所有节点最大 x/y + 节点尺寸 + 边距，供画布 viewBox/滚动区。

- [ ] **Step 1: 实现 useTreeLayout.ts**（纯函数 + computed，无组件依赖；导出 `isChildWaiting(n) = n.status==='running' && n.activityKind==='child_wait'` 供节点卡与面板复用）

- [ ] **Step 2: 实现 AgentTreeNode.vue**（props：`node: LayoutNode`、`selected: boolean`；emit：`click`）

节点卡内容（绝对定位在画布变换层内，`style="{ left, top, width: NODE_W, height: NODE_H }"`）：
- 顶行：状态点（done=绿 / running=黄脉冲 / child_wait=蓝脉冲 / failed=红 / cancelled、paused=灰 / delivered-unverified=橙）+ `name` 加粗截断 + 状态徽章小字。
- 次行：`activityText`（`in ReadFile · 12s` / `thinking · 3s` / child_wait → `等下级返回 · <ago>`），无活动时空白占位。
- `type==='domain'` 且 `running` 时 hover 浮出右上操作组：⏸ 中断 / ⏹ 终止（emit `pause` / `cancel`，父级确认后调 API——按钮止冒泡 `@click.stop`）。
- selected 态：外框高亮（`ring-2 ring-primary`）。
- 样式：bg-card rounded border，hover 抬升阴影；全部 Tailwind，对齐现有面板风格。

- [ ] **Step 3: 实现 AgentTreeCanvas.vue**（props：`roots/edges/width/height/selectedId`；emit：`select(id)`、`pause(id)`、`cancel(id)`）

- 结构：外层 `div.canvas-wrap`（overflow hidden + 相对定位）→ 内层变换层 `div`（`transform: translate(panX,panY) scale(zoom)`，transform-origin 0 0）→ 内放一个绝对定位 SVG（width/height=布局尺寸）画全部边 + v-for 渲染 AgentTreeNode。
- 边：三次贝塞尔 `M x1 y1 C x1 (y1+V_GAP/2), x2 (y2-V_GAP/2), x2 y2`，`stroke` 按子节点状态着色（done=绿系、failed=红系、running=主色、其余 line 色），`fill=none`，1.5px。
- 交互：wheel 缩放（0.3~2，以指针位置为中心）；左键拖拽平移（pointerdown/move/up，canvas 空白处才启动——节点 click 不参与拖拽）；右上悬浮工具条：缩放比例显示、重置、"适应"（按 wrap 尺寸计算 zoom 使全树可见并居中）。
- 空态：`暂无角色实例，会话启动后自动创建`（沿用看板文案）。

- [ ] **Step 4: 验收**

Run: `cd web && pnpm build`
Expected: 类型检查 + 构建通过。（手动验证留到 Task 12 接线后一并做。）

- [ ] **Step 5: Commit**

```bash
git add web/src/composables/useTreeLayout.ts web/src/views/session/orch/AgentTreeNode.vue web/src/views/session/orch/AgentTreeCanvas.vue
git commit -m "feat: 编排树图基础（tidy 布局 + SVG 连线 + 节点卡）"
```

---

### Task 11: 对话面板 `AgentChatPanel.vue`（消息流 + 留痕 tab + 三态发送框）

**Files:**
- Create: `web/src/views/session/orch/AgentChatPanel.vue`

**Interfaces:**
- Consumes: Task 9 的 `getAgentMessages/sendAgentMessage/AgentConversation`；`pauseSessionAgent/cancelSessionAgent`（api/metrics.ts）；`AgentNode`；`isChildWaiting`（Task 10）；Markdown 渲染沿用 `MarkdownRenderer.vue`（主对话同款）。
- Produces: props `{ sessionId: string, agent: AgentNode | null }`；emit `refresh`（发送/中断/终止后请父级刷新 agents）。

**结构与行为：**
- header：`name` 加粗 + 状态徽章 + `inst_id` 等宽小字；右侧操作：⏸ 中断 / ⏹ 终止（仅 `type==='domain'` 且 running/child_wait 时可用，点击前 `ElMessageBox.confirm`，中断文案"暂停并保留进度？"、终止文案"终止该 Agent？其未完成任务将标记失败"）。
- tab：「对话」/「留痕」（`el-segmented` 或简易 button 组，跟随项目现有 tab 写法——先 grep `views/session` 下 tab 实现沿用）。
- 对话 tab：消息流渲染 `messages`：
  - `role==='user'` 且 content 以 `[mailbox from user]` 开头 → 高亮卡（主色左边框）标「来自用户 · 直连」；
  - `role==='user'` 且 content 以 `[mailbox from <X>]` 开头 → 灰色卡标「来自 <X>」；
  - 其他 `user` → 「任务/输入」中性卡；
  - `assistant` → MarkdownRenderer 渲染 content；`reasoning` 收进 `<details>`「思考过程」；`tool_calls` 渲染为工具 chips（`name` + input 摘要单行截断）；
  - `tool` → 折叠 `<pre>`，超 800 字符截断 + 「展开」。
  - 顶部 `has_more` 时「加载更早消息」按钮（`before_seq = 当前最小 seq` 翻页前插）。
- 留痕 tab：`mails` 表格流（时间 / `from → to` / msg_type 徽章 / subject 加粗 / body 折叠），最新在上。
- 发送框三态（`sendState` computed）：

| 条件 | 状态 | 表现 |
|---|---|---|
| agent 为空或 `type==='meta'` | `meta` | 禁用，提示「主 Agent 请用主对话页」 |
| running 且 child_wait | `waiting` | **可发**，placeholder「Agent 正在等下级返回，消息将立即注入并唤醒」 |
| running 非 child_wait | `running` | 禁用，按钮转圈（el-icon Loading），提示「正在执行任务——可中断或终止后再发」 |
| done/failed/cancelled/delivered-unverified | `done` | **可发**，placeholder「发送将复活该 Agent 并以消息为增量输入重跑」 |
| paused | `paused` | 禁用，提示「已暂停——请到监控页恢复」 |
| idle/其他 | `idle` | 禁用，提示「热驻待复用——新任务请经主 Agent 派发」 |

- 轮询：面板打开且 agent 非空时每 3s `getAgentMessages(sessionId, inst_id, { afterSeq: maxSeq })` 增量追加；留痕整量替换（量小）。`watch(() => [props.sessionId, props.agent?.inst_id])` 切换时清空并全量重载（`limit: 100`）。组件卸载/切换清定时器。
- 发送成功/复活成功后 emit `refresh`，并立即拉一次增量。

- [ ] **Step 1: 实现组件**（先 grep 确认： `[mailbox from ` 前缀的后端确切格式——Task 2 双写 `Content = Subject + "\n" + Body`，注入邮件 Subject 为「用户直连消息」，故对话页识别规则为 `role==='user' && content.startsWith('用户直连消息')` 或 mailbox 留痕比对；实现时对照 Task 2 落库格式修正识别条件）

- [ ] **Step 2: 验收**

Run: `cd web && pnpm build`
Expected: 通过。

- [ ] **Step 3: Commit**

```bash
git add web/src/views/session/orch/AgentChatPanel.vue
git commit -m "feat: 编排页对话面板（消息流 + 留痕 + 三态发送框）"
```

---

### Task 12: 编排主视图 `OrchView.vue` + 路由接线

**Files:**
- Create: `web/src/views/session/orch/OrchView.vue`
- Modify: `web/src/views/session/index.vue`

**Interfaces:**
- Consumes: `useTreeLayout`（Task 10）、`AgentTreeCanvas/AgentChatPanel`；props `{ session: Session, agents: AgentNode[] }`（对齐 MonitorView 的 props 形态，以 index.vue :538 实参为准）。
- Produces: `?view=orch` 视图；选中态经 `route.query.agent` 同步（刷新/分享链接可定位节点）。

**OrchView 结构：**
- 左 55%：AgentTreeCanvas；右 45%：选中时 AgentChatPanel，未选中时概览卡（总会话状态摘要：目标一句话 + 各状态计数 + 提示「点击节点查看对话」）。
- `selectedId` 与 `route.query.agent` 双向同步（watch route.query.agent → selectedId；select 时 `router.replace`）。
- `pause/cancel` emit 统一在此处理：confirm → 调 API → emit `refresh` 给父（或直接复用 index.vue 传入的刷新回调——以 MonitorView 现有刷新机制为准）。

**index.vue 接线（最小改动）：**
- `view` 联合类型加 `'orch'`（:97），初始化 `const q = route.query.view; view = q==='monitor'||q==='orch' ? q : 'chat'`；watch 里 `view==='chat' ? undefined : v` 逻辑不变（:99）。
- 切换按钮组（:508-512）加第三个：`🌳 编排`（同款 class 逻辑）。
- `MonitorView` 的 `v-else`（:538 附近）改 `v-else-if="view === 'monitor'"`，其后挂 `<OrchView v-else-if="view === 'orch'" :session="activeSession" :agents="agents" @refresh="..." />`（refresh 接线以现有 agents 刷新函数为准——grep `agents.value =` 找刷新入口）。

- [ ] **Step 1: 实现 OrchView.vue**
- [ ] **Step 2: index.vue 接线**
- [ ] **Step 3: 验收**

Run: `cd web && pnpm build`
Expected: 通过。

- [ ] **Step 4: 手动冒烟（起前后端）**——打开任一会话 `?view=orch`：树图渲染、点击节点右栏切换、URL `?agent=` 同步、缩放拖拽适应按钮、空态。

- [ ] **Step 5: Commit**

```bash
git add web/src/views/session/orch/OrchView.vue web/src/views/session/index.vue
git commit -m "feat: 编排主视图接线（?view=orch 左树右对话）"
```

---

### Task 13: 看板「任务目标」时间线化 + 移除旧编排树区块

**Files:**
- Create: `web/src/composables/useGoalTimeline.ts`
- Modify: `web/src/views/session/components/panels/TaskBoardPanel.vue`
- Modify: `web/src/views/session/index.vue`（传 `:events`）
- Delete: `web/src/composables/useRoleTree.ts`（确认无其他引用后）

**Interfaces:**
- Consumes: `SessionEvent`（types/index.ts，`type/kind/success` + `isErrorEvent` :150）、`TaskBoardData`（:185，含 `goal`/tasks）。
- Produces:
  - `useGoalTimeline(events: MaybeRef<SessionEvent[]>, board: MaybeRef<TaskBoardData | null>)` → `{ milestones: ComputedRef<Milestone[]> }`
  - `Milestone = { at: string, kind: 'user'|'dispatch'|'done'|'failed', text: string }`

**milestone 提取规则（按 events 顺序）：**
- `kind==='user_message'`（或 type==='user'，以 classifyEvent 现有口径为准——chat/utils/turns.ts）→ `{ kind:'user', text: 内容首行截 80 }`；
- `kind==='tool_call'` 且工具名为 `call_sub_agent/map_sub_agents`（名字以 classifyEvent/事件负载实际字段为准）→ `{ kind:'dispatch', text: '派发 <domain或工具参数摘要>' }`；
- `kind==='agent_done'/'sub_agent_done'` → `{ kind:'done', text: '<agent> 完成' }`；
- `isErrorEvent(ev)` → `{ kind:'failed', text: 错误摘要截 80 }`。
- **兜底**：events 为空或提取不到任何 milestone 时，从 `board.tasks` 合成（每任务一条：状态图标 + title），保证看板永不空白。
- 条数上限 50，超出保留最近 50。

**TaskBoardPanel 改造（保持其余区块不变）：**
- props 加 `events: SessionEvent[]`（index.vue :584 传 `:events="events"`）。
- 「任务目标」区：goal 文本改折叠行——默认单行截断 + 右侧「展开」；下方加时间线（竖线 + 圆点按 kind 着色：user=主色 / dispatch=蓝 / done=绿 / failed=红，时间小字 + 文本）；时间线默认可折叠（条数 >6 时收起到「显示全部 N 条」）。
- 保留现有编号子任务行与进度条不动。
- 「Agent 编排」el-tree 区块（:53-102 整段）替换为一个入口卡：`🌳 Agent 编排` 标题 + 一句说明（「层级树图查看 Agent 关系，点击节点直达对话」）+ 按钮「打开编排页 →」（`router.replace` 当前 query 加 `view: 'orch'`）。
- 删 import useRoleTree；全仓 grep `useRoleTree` 确认无残留引用后删文件。

- [ ] **Step 1: 实现 useGoalTimeline.ts**
- [ ] **Step 2: 改造 TaskBoardPanel.vue + index.vue 传 events**
- [ ] **Step 3: 删除 useRoleTree.ts**（先 `grep -r useRoleTree web/src` 确认零引用）
- [ ] **Step 4: 验收**

Run: `cd web && pnpm build`
Expected: 通过。

- [ ] **Step 5: Commit**

```bash
git add web/src/composables/useGoalTimeline.ts web/src/views/session/components/panels/TaskBoardPanel.vue web/src/views/session/index.vue web/src/composables/useRoleTree.ts
git commit -m "feat: 看板任务目标时间线化，编排树迁移至编排页"
```

---

### Task 14: 总验证与收尾

- [ ] **Step 1: 后端全量**

Run: `cd backend && go build ./... && go test ./internal/agent/... ./internal/domain/... ./internal/mailbox/... ./internal/store/... ./internal/server/... ./internal/orchestrator/... ./pkg/prompts/...`
Expected: 全 PASS。（若有历史遗留失败用例，先 `git stash` 对照基线确认非本次引入。）

- [ ] **Step 2: 前端构建**

Run: `cd web && pnpm build`
Expected: 通过。

- [ ] **Step 3: 手动验证清单**（起一个真实会话，含至少一层子 Agent）

1. 看板：目标折叠/展开正常，时间线随事件增长，「打开编排页」跳转正确。
2. 编排页：树图层级与连线正确；running 节点黄点脉冲；等子节点蓝点 + 「等下级返回」；点击节点右栏加载对话；URL `?agent=` 同步。
3. 发送三态：对 running 节点发送框禁用转圈；对等子节点发送 → Agent 邮箱立即收到（留痕 tab 可见 From=user 邮件）且 wait loop 被唤醒（日志/行为）；对 done 节点发送 → 节点回 running 重跑，父收到「复活返工」通知，种子含用户消息。
4. 中断/终止：hover 节点卡与面板 header 两处按钮均 confirm 后生效，状态与看板同步。
5. Redis 停掉后对话页仍可读（PG 回退），无报错。
6. 中断恢复（监控页）后对话页消息连续。

- [ ] **Step 4: 文档收尾**

检查 `CLAUDE.md` 与 `doc/TODO.md` 是否需补编排页说明（新端点、新视图、用户直连语义）；需要则补一行级别的最小更新。

- [ ] **Step 5: Commit**（若有文档改动）

```bash
git add CLAUDE.md doc/TODO.md
git commit -m "docs: 补编排页与用户直连说明"
```
