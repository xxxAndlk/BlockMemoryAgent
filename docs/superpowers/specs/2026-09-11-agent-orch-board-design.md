# 任务看板 · Agent 编排页设计

日期：2026-09-11
状态：已获用户批准，待实现

## 背景与目标

现有"任务看板"（`web/src/views/session/components/panels/TaskBoardPanel.vue`）以 `el-tree` 平铺 Agent 列表，信息层级弱、无法体现编排关系，也无法查看单个 Agent 的对话或与它交互。本设计：

1. 新增全宽"编排"页：Agent 层级树图（父上子下、连接线），点击节点切换到该 Agent 的对话页。
2. 每个 Agent 有独立对话页：完整消息级历史 + Agent 间交互留痕（任务派发/结果回收/横向询问/用户注入）。
3. 用户可直发消息给指定 Agent：`waiting`（等下级返回）与 `done/failed` 状态可发；`running`（执行中）发送按钮禁用转圈，复用现有中断/终止。
4. 看板"任务目标"区块改为时间线式展示。

## 现状关键事实（调研结论）

- 树数据：`GET /api/sessions/:id/agents`（扁平 `AgentNode[]`，含 `inst_id/parent_id/status/activity_kind/last_activity_ago`）与 `GET /api/sessions/:id/tree`（权威树），前端 3s 轮询。
- 权威树：`backend/internal/domain/orchestrator/tree.go`，`Node{ID, ParentID, Role, Domain, Task, Status, Started, Finished, Summary, Err}`；状态枚举 `running/done/failed/cancelled/paused/idle/delivered-unverified`。
- 子 Agent ID：`{parentID}/{roleID}-{seq}`，MetaAgent 的 agentID == sessionID。
- 每 Agent 事件流：`GET /api/sessions/:id/agents/:aid/events`（`agent_events` 表：tool_call/answer/sub_agent_summary）。
- 完整消息历史：`agent_messages` 表（role/content/tool_calls/tool_call_id/reasoning，seq ASC），**目前仅 Paused 的 DomainAgent 写**。
- 压缩金字塔：`agent_compress_states` 表 + `backend/internal/agent` 内压缩逻辑，只影响 Agent 自身上下文。
- Mailbox：`backend/internal/mailbox/mailbox.go`，纯内存；`Message{ID, From, To, Type(milestone/request/info/escalate/reply), Subject, Body, ...}`。子→父结果经 `Dispatcher.notify()` 发 `MsgInfo`；横向有 `send_message` 工具。运行中子 Agent 每轮 `drainMailbox()`。
- 等待信号：`ReActAgent.waitForChildren()`（react_agent.go:1506）事件驱动等子完成，等子期间不烧 LLM 轮次；活动证据 `ActivityEvidenceOf(agentID)` 已有 kind `user_wait/descendant/tool:<名>` 等先例。
- 单 Agent 控制已存在：`POST /api/sessions/:id/agents/:aid/cancel`（硬取消）与 `POST /api/sessions/:id/agents/:aid/pause`（软停止，domain 可 resume）。
- Redis 已是既有基础设施：`backend/internal/store/redis_event.go/redis_output.go/redis_topic.go/redis_ttl.go`。
- 前端：Vue3 + Element Plus + Tailwind，无 pinia（composables 模块级 ref），echarts 已声明但未使用，无图库依赖。路由按 `?view=chat|monitor` 切换主区视图。

## 1. 页面结构

- 主区新增 tab `编排`（与"对话/监控"并列），`?view=orch`，全宽。
- 左右分栏：左侧约 55% 树图画布（缩放/平移/适应屏幕），右侧约 45% Agent 对话面板。
- 未选中节点：右侧显示编排概览（目标摘要 + 各状态计数）。选中节点：右侧切换为该 Agent 对话页，URL 同步 `?view=orch&agent=<id>`，刷新可恢复。
- 右侧任务看板面板保留（仍属"对话"页侧栏）；其中"Agent 编排"小节的旧 `el-tree` 移除，改为"打开编排页"入口 + 精简状态列表。树图只在编排页维护一份。

## 2. 树图（自绘 SVG + HTML 节点，零新依赖）

- 新组件：
  - `web/src/views/session/components/orch/AgentTreeCanvas.vue`：tidy-tree 布局（父上子下、同级均分）、SVG 贝塞尔连线、缩放/拖拽平移/一键适应。
  - `web/src/views/session/components/orch/AgentTreeNode.vue`：节点卡片（HTML，Vue 模板）。
- 组树：复用 `useRoleTree` 的 `parent_id` 分组逻辑，但编排页**不过滤 idle**、不做孤儿挂根，展示权威树。
- 节点卡片内容：名称/domain、状态徽章、activity 证据文本（`in ReadFile · 12s ago` / `等待下级返回`）。
- 状态视觉：`running`=黄色呼吸点；`waiting`=蓝色呼吸点；`done`=绿；`failed`=红；`paused`=灰锁。
- 交互：单击选中（右侧开对话页）；hover 浮出操作按钮（running → 中断/终止；done/failed → 发消息复活入口）。
- 数据源：复用现有 `GET /sessions/:id/agents` 3s 轮询（`usePanelRefresh`），不新增推送通道。

## 3. waiting 状态判定（后端小改）

- 节点状态枚举不动。复用活动证据机制：`ReActAgent.waitForChildren` 进入时设 `ActivityKind=child_wait`、退出清除（改动点仅此一处，先例：`user_wait`）。
- 前端把 `running + activity_kind=child_wait` 渲染为"等待下级返回"——兼作发送按钮开关信号。

## 4. 用户直发消息（新端点 + mailbox 注入）

- 新增 `POST /api/sessions/:id/agents/:aid/message` `{content}`：
  - `waiting` → `mailbox.Send(From:"user", Type:request)`；等待中的 Agent 被邮件唤醒，`drainMailbox` 注入自身历史后继续循环。
  - `done/failed` → 复活流程（第 6 节）。
  - `running` 非 waiting → 409 拒绝（前端按钮此时禁用转圈，双保险）。
- 提示词补充（`backend/pkg/prompts/meta_agent.go`、`domain_agent.go` 各一段）：等下级期间收到 user 邮件时，先自查（列出自身任务进度与各下游子节点状态/摘要），再解析用户意图：
  - 下游跑错 → `cancel_agent` + 重新派发；
  - 新增需求 → `call_sub_agent` 派新任务；
  - 纯信息补充 → 记录后继续等。
  - 分析过程作为思考事件留痕。

## 5. 对话页内容与消息持久化（Redis 热层 + PG 全量 + 现有压缩）

- 新端点 `GET /api/sessions/:id/agents/:aid/messages?before_seq&limit`：
  - 先读 Redis 热层（key `sess:{id}:agent:{aid}:msgs`，list，TTL 24h，cap 最近 500 条）；
  - 更早分页回退 PG `agent_messages`；
  - Redis 不可用时降级直读/直写 PG。
- 写路径改动：`agent_messages` 由"仅 Paused 时写"改为**所有 Agent 全程写**：每条消息先 Redis 热写，批量 flush PG 全量。flush 触发点：每累积 20 条、节点 Finish/Pause/Cancel 时强制 flush、进程退出前 best-effort flush。压缩金字塔不动——压缩只影响 Agent 自身上下文，对话页读存储层不受影响。
- mailbox 留痕：`mailbox.Send` 同步写一条 `agent_events`（type=`mailbox`，含 from/to/subject/body），不建新表。
- 对话页渲染：消息流与邮件事件按 seq/时间合并——任务派发（父→子）、结果回收（子→父）、`send_message` 横向询问、用户注入（高亮"来自用户"）全部可见，重启可回放。
- 实时性：选中 agent 的对话页 3s 轮询增量（`after_seq`），与面板轮询节奏一致。

## 6. done/failed 复活（复活原节点）

- `Tree` 新增 `Reopen(nodeID)`：节点回 `running`，保留上次 `Summary/Err` 作续跑上下文。
- dispatcher 以「原任务 + 上次结果摘要/错误 + 用户消息」为种子，用**同一 agent_id** 重新起 goroutine；热驻 slot 已销毁则走普通冷启动。
- 父节点已 done 时不回溯改父状态（v1 简化），但给父发 mailbox 通知"子任务返工中"，父若仍在等待可感知。

## 7. 中断/终止（复用现有）

- running 节点操作直接调已有端点：`POST /agents/:aid/pause`（中断/软停止，可 resume）、`POST /agents/:aid/cancel`（终止）。
- "中断并下达新指令" = pause + mailbox 注入指令（resume 时 Agent 读到）；前端做成 pause 确认框内可选输入，复用会话级 `handleInterrupt` 交互模式。

## 8. 任务目标 → 时间线（看板面板内）

- 目标文本置顶，默认折叠一行、可展开；标题行保留整体进度百分比。
- 下方时间线按时间列里程碑：派发（标题/执行者/时刻）、完成、失败、阻塞、返工记录（用户 interrupt/直发消息）。
- 数据来自现有 `session_events` + `board.tasks`，新 composable `useGoalTimeline` 聚合，纯前端改动。

## 9. API 变更汇总

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/sessions/:id/agents/:aid/messages?before_seq&limit&after_seq` | 新增，Agent 完整消息+邮件留痕分页 |
| POST | `/api/sessions/:id/agents/:aid/message` | 新增，用户直发消息（waiting 注入 / done/failed 复活 / running 409） |
| — | `GET /agents` 的 `activity_kind` 新增 `child_wait` 值 | 结构不变 |
| 复用 | `/agents/:aid/cancel`、`/agents/:aid/pause`、`/agents`、`/tree`、`/agents/:aid/events` | 不变 |

## 10. 测试与验证

- 后端 go test：
  - 消息双写（Redis 热层 + PG flush）与 Redis 不可用降级；
  - 注入端点三态校验（waiting 接受 / running 409 / done/failed 复活）；
  - `Tree.Reopen` 状态转移与续跑种子构造；
  - mailbox 留痕写入 `agent_events`。
- 前端无测试框架，以手动验证清单兜底：树图渲染与连线、点击切换对话页、三态发送按钮（waiting 可发 / running 禁用转圈 / done/failed 复活）、中断与终止、任务目标时间线。

## 明确不做（YAGNI）

- 不引入图编辑库（vue-flow/dagre），不做节点拖拽编辑。
- 不做 mailbox 独立新表（复用 `agent_events`）。
- 复活子节点不回溯改父状态。
- SSE 通道不扩展（编排页沿用 3s 轮询）。
