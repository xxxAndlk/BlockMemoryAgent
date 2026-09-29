# 多 Agent 可靠性改造设计：mailbox 持久化/配对 + 子 Agent 并发池 + 看板持久化

日期：2026-09-28
状态：已获用户批准（2026-09-28 逐节确认）
范围：多 agent 调研发现的 P0（消息可靠性）与 P1（资源治理）五项修复。

## 背景

现状问题（调研结论，证据见各节）：

- **P0-1** mailbox 纯内存：`Drain` 破坏性读无重投，崩溃丢在途邮件；子 Agent 退出 `Purge` 连未读一起删，发送方无感知。
- **P0-2** request/reply 无配对机制：`ReplyTo` 被填成发送方 agentID（`dispatcher.go:2220`，与 `mailbox.go:83-85` 文档矛盾），`thread_id` 无校验无索引，提问方不能等待、无超时告警。
- **P0-3** 广播桶死代码：`DrainBroadcast`/`Forward` 仅测试调用，`To="*"` 消息永久泄漏在内存。
- **P1-4** 无子 Agent 全局并发上限：约束只有单 session 派发总数 30、批量波 ≤6、map worker pool 6；6 个 domain 各自派叶子时 goroutine 与 LLM QPS 无硬顶。
- **P1-5** 看板纯内存（`board.go` 无持久化），重启丢依赖门状态（`doc/变更.md` 自认欠账）。

## 已确认的决策

| 决策点 | 结论 |
|---|---|
| 队列实现 | 自研轻量池（chan 信号量 + FIFO 票据队列 + ctx 取消，零新依赖） |
| 排队期间墙钟 | 出队才开始计时（`WithTimeout` 移到 Acquire 成功后） |
| mailbox 持久化 | 新表 `mailbox_messages`，Send 双写 / Drain 标 read / 恢复重投 unread |
| 池与生命周期 | 池只做准入控制（acquire/release/排队/统计）；Tree 仍是唯一生命周期权威（注册/取消/巡检/落 PG），不做双真相源 |

## ① mailbox 持久化 + 死信

### 新表 `mailbox_messages`

```sql
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
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_session ON mailbox_messages (session_id, status);
```

- 落 `store.EnsureMailboxSchema`（`backend/internal/store/schema.go` 惯例：幂等 DDL + owner 列），注册进 `bootstrap.go:1161-1184` 的 `ensureSchemas` map；`migrations/010_mailbox_messages.sql` + `migrations/011_session_boards.sql` 写文档性副本（运行时不读 migrations 目录）。
- `store.MailboxStore`（新文件 `backend/internal/store/mailbox_store.go`，纯 `database/sql` + `lib/pq` 惯例）：
  - `Save(msg)`：`INSERT ... ON CONFLICT (id) DO NOTHING`（幂等，恢复重投安全）。
  - `MarkRead(ids, readAt)`、`MarkDead(ids)`：批量状态翻转。
  - `LoadUnread(sessionID)`：恢复用，返回 `status='unread'` 的全部消息按 created_at 升序。

### 接线

- mailbox 包沿用 hook 风格：现有 `WithTrace`（展示留痕，写 `agent_events`）不动；新增 `WithPersist(func(*Message))` 与 `WithReadMarker(func(ids []string, read bool))` 两个回调。
- `Send` 成功入箱后**同步**调 persist（best-effort，5s 超时，失败仅 log）。同步是为把"内存有、PG 没有"的丢失窗口压到最小；PG 不可用时内存邮箱照常工作。
- `Drain` 后对本次翻转的消息异步批量 `MarkRead`。
- `bootstrap.go` 接线处注入这两个回调（紧邻 `:360-389` 的 WithTrace 接线）。

### Purge 死信

- `Purge(agentID)` 签名改为返回被丢弃的未读消息 `[]*Message`（5 处调用点全改：`dispatcher.go:4409` defer、`killStuckSubAgent :1055`、`idle_pool.go:698/:1867`、`purge.go:57`、`service_react.go:4680` finalizeSession）。
- 子 Agent 退出路径（dispatcher）：对被丢弃的未读 `request`/`escalate`，给发送方回投一条 `MsgInfo` 死信通知（"你发给 X 的消息未送达：X 已终结"），同时 PG 标 `dead`；发送方为 user/系统或已不存在的跳过。
- `finalizeSession` 会话收尾路径：只标 `dead`，不回投通知（会话已终结，通知无意义）。

### 崩溃恢复重投

- 会话恢复链路（`buildRestoredSession`，`session_react.go:1084`）补：`LoadUnread(sessionID)` → 逐条 `Send`（保留原 ID，ON CONFLICT 幂等）。
- `mailbox.Reopen`（复活路径，`dispatcher.go:1283` 用）顺带重载该 agent 的未读邮件——堵住"复活后旧未读消失"缺口。
- `RestoreSessionDomains`（`idle_pool.go:1285`）已有 `mailbox.Reopen` 调用，自然受益。

## ② request/reply 真配对

- **注入侧**：`mailboxMessageToReact`（`react_agent.go:1999` 一带）注入文本带消息 ID：`[mailbox from X id=msg_… thread=…]`，让被问方模型可引用。
- **工具侧**：`send_message`（`dispatcher.go:2133-2285`）加可选参数 `reply_to`；type=reply 且 `reply_to` 为空时**自动配对**：按 (from,to,thread_id) 找最近未应答 request 回填；都落空则软提示（不硬拒，避免拒绝循环）。
- **pending 注册表**：Dispatcher 新增 `pendingRequests`（`map[msgID]pendingRequest{From,To,ThreadID,Subject,CreatedAt,Deadline}` + mutex）。request/escalate 投递成功即登记；reply 到达（ReplyTo 命中，或自动配对命中）即销账。会话结束/进程退出随 Dispatcher 内存态回收（PG 里消息行仍在，恢复后可重建——重建逻辑：恢复重投时对 unread 的 request 重新登记，deadline 顺延一个超时周期）。
- **超时升级**：复用现有 patrol goroutine（`dispatcher.go:981`）加一轮 sweep（每 30s）：超 `peer_request_timeout_min` 未答 → 给提问方的父 Agent 投 `MsgEscalate` 通知（"X 询问 Y 超时未答"），销账。
- **文案同步**：`send_message` 工具描述（`dispatcher.go:2151-2163`）、meta/domain 提示词的协作问答段（`pkg/prompts/meta_agent.go:236-242`、`domain_agent.go:247-261`）、热驻唤醒任务文本（`idle_pool.go:1636` 指示"reply 必须带 reply_to=原消息 ID，thread_id 沿用"）。`pkg/prompts.Version` bump 并记变更。

### 新配置

`agent.peer_request_timeout_min`：默认 15。0=未配置走默认（config.go:659-661 约定）。

## ③ 删广播死代码

- mailbox 包删 `bcast` 字段、`DrainBroadcast`、`Forward`；`Send` 对 `To==""` 或 `"*"` 返回校验错误。
- 实施第一步先 grep 确认生产代码无广播调用方（调研结论：仅测试用），测试同步删/改。
- `MsgMilestone`/`MsgDependency` 闲置类型**不动**（有事件映射牵连，超出本次范围）。

## ④ 子 Agent 并发池

### Pool（新文件 `backend/internal/domain/subagent/pool.go`，约 150 行）

```go
type Pool struct { /* chan 信号量 + container/list FIFO 票据队列 */ }
func NewPool(limit int) *Pool            // limit<=0 表示不限（acquire 直通）
func (p *Pool) Acquire(ctx context.Context) error  // 排队等名额；ctx 取消则摘除票据返回 ctx.Err()
func (p *Pool) Release()
func (p *Pool) Stats() (running, queued int64)
```

- FIFO：名额释放时授予队首票据；Acquire 内 select 票据授予与 ctx.Done。
- 排队超 2 分钟打 warn 日志（可观测性最低限度，不加 HTTP 端点——YAGNI）。

### 集成点（调研已确认的坑）

1. **Acquire 位置**：`runSubAgent`（`dispatcher.go:4281` 的调用入口，dispatchOne goroutine `:3427`）开头 Acquire、`defer Release`。覆盖单派发/批量波/`map_sub_agents` 全部路径。`ResumePaused`（`:4903`）与 `ReviveWithMessage`（`:1283`）若绕过 `runSubAgent` 则各自补 Acquire/Release（实施时核实调用链）。
2. **墙钟起点**：`context.WithTimeout`（现 `:3353-3355` 派发点创建）**移到 Acquire 成功之后**；`tree.SetCancel`（`:3371`）改绑基础 ctx（`context.WithCancel`）的 cancel——排队期取消无空窗，cancel 后 Acquire 立即返回错误走正常清理（trackChildDone + notify）。
3. **巡检豁免**：activity evidence（`activity.go:46-92`）加 `queued` 标记，Acquire 等待期间置位、成功后清除；`scanStuck`（`:1018-1049`）跳过 queued 中的 agent——否则排队超 5 分钟必被误判假死 kill。
4. **热驻 idle 槽**：驻车（idle parked）不占池名额，只有真正跑任务（`runDomainSupervisor` 的 opNewTask/opResume 执行段）才 Acquire/Release。

### 新配置

`agent.max_concurrent_sub_agents`：默认 8，挂 `AgentConfig`（`config.go:270`），默认值放 `applyAgentStandaloneDefaults`（`:854`），经新 Option `WithConcurrencyLimit(n)` 注入 Dispatcher。

## ⑤ 看板持久化

### 新表 `session_boards`

```sql
CREATE TABLE IF NOT EXISTS session_boards (
    owner      VARCHAR(64) NOT NULL DEFAULT '',
    session_id VARCHAR(64) PRIMARY KEY,
    goal       TEXT,
    snapshot   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

整板快照写穿：看板变更低频（写计划/派发回写/终态翻转），每次 mutator 后同步 best-effort 落库（失败仅 log，内存看板照常）。

### 接线

- `board.Manager` 加可选 store（`NewManagerWithStore`，nil 时纯内存——测试与现有行为不变）。
- `TaskBoard` 全部 mutator（`SetPlan/Assign/transition/SetConstraint/TakeoverFrom/AddSubTask`）收口一个 `persist()` 切面：快照序列化（`Snapshot()` :568 已有，需补 JSON 可序列化导出/导入对）→ UPSERT。
- `GetOrCreate`（`board.go:718`）内存 miss 时先查 PG 回填重建（依赖门状态随之恢复）。
- 用户显式删会话的 7 表级联（`CLAUDE.md` append-only 纪律的唯一物理删除例外）补 `session_boards` 与 `mailbox_messages` 两张表。

## 测试计划

- **单测**（backend 内就近 `_test.go`）：
  - pool：上限阻塞、FIFO 顺序、ctx 取消摘票据、Release 唤醒队首、Stats 计数。
  - pending 配对：reply_to 显式命中、自动配对命中、超时升级产出 escalate、销账幂等。
  - mailbox：Purge 返回未读列表、Send 拒绝空/`*` 收件人。
  - board：快照导出/导入往返（含依赖与状态）。
- **集成测试**（`test/` 模块，`//go:build integration`，PG+Redis 共享容器惯例）：
  - mailbox：Send→PG→模拟重启→LoadUnread 重投→Drain 后 PG 标 read；Purge 死信通知到发送方。
  - board：SetPlan/Assign→落库→新 Manager GetOrCreate 恢复→DependsDone 语义不变。
- 回归：`make backend-test`（`GOTOOLCHAIN=local`）+ 触包既有测试（mailbox/dispatcher/board/store）。

## 杂项

- 修 CLAUDE.md 漂移：「叶子角色无 send_message」→ 实际 8 个叶子角色都有，受可见性矩阵限制。
- `config/config.yaml` 与 `.env.example` 注释补两个新 key。
- `doc/变更.md` 记本次变更（含 prompts.Version bump）。

## 范围外（Non-goals）

- 共享记忆/看板的推式变更通知（P2）；`send_message` 同步 wait 模式（P2）。
- 块记忆 outcome 价值反馈（P2，AICP 对比文档 3.1）。
- dispatcher.go 拆分与 `MsgMilestone`/`MsgDependency` 清理（P3）。
- 并发池的 HTTP 观测端点（仅日志）。
- A2A 协议（维持不引入结论）。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| Send 同步落库增加投递延迟 | PG 本地同机 ~1ms；失败 fail-open 不阻塞主流程 |
| 池上限过小导致长排队 | 默认 8 可调；排队 warn 日志暴露；排队不计墙钟 |
| 恢复重投与运行中 Drain 竞态 | 恢复只在会话物化时做一次；Send 幂等（ON CONFLICT）；Drain 标 read 后 LoadUnread 不再返回 |
| pending 注册表内存膨胀 | 销账/超时双出口；会话结束随 Dispatcher 回收；PG 行留底可查 |
