# BlockMemoryAgent 执行流程图阅读指南

本文档面向刚接触 BlockMemoryAgent 的新成员，结合 `doc/image/agent_execution_flow.png` 中的流程图，逐步说明系统从用户输入到最终输出的完整执行链路、每个组件的职责以及它们之间的交互方式。

---

## 1. 项目定位

BlockMemoryAgent 是一个基于 Go + go-kratos Blades 构建的**四层 Agent 编排系统**：

- **MetaAgent**：顶层调度者，负责理解用户目标、拆分领域、创建任务看板、监控上下文长度。
- **DomainAgent**：领域级执行者，负责检索历史记忆与归档、把任务分发给下层助手。
- **SubDomainAgent**：子领域执行者，可选，用于进一步拆分复杂领域。
- **Assistant**：具体执行者，负责 LLM 调用、Skill 装配、工具调用。

系统同时包含 Runtime 组件（看板、邮箱、Watchdog、Skill 注册表）、记忆系统（块记忆、归档、快照）、DAG 调度、命令队列和 TUI/Web 界面。

---

## 2. 流程图总览

流程图从上到下描述了**一次用户请求的生命周期**：

1. 用户通过 HTTP API 或 TUI 提交目标。
2. `SessionManager` 创建会话并启动图执行。
3. `ThreeLayerGraph.Invoke` 按状态机驱动四层 Agent 依次执行。
4. 每个 Agent 在执行过程中与 Runtime、记忆系统、存储层交互。
5. 工具执行结果通过回调写回会话事件流。
6. 最终会话进入 `completed`、`error` 或 `awaiting_clarify` 状态。
7. `DAG Scheduler` 和 `CmdQueue` 可以从外部注入新会话或指令。

图中：

- **实线箭头**表示主控制流，即代码调用顺序。
- **虚线箭头**表示 Runtime / 数据交互，例如读看板、写邮箱、查数据库。
- **彩色方框**表示不同类型的节点或组件，颜色含义见右下角图例。

---

## 3. 各步骤详细说明

### 3.1 用户 / HTTP API / TUI

这是系统的入口。用户可以通过三种方式与系统交互：

- **HTTP API**：`POST /api/sessions` 传入 `goal`，由 `SessionManager.HandleCreateSession` 处理。
- **TUI**：`go run ./backend/cmd/tui`，在终端内直接发起会话、查看执行过程。
- **Web UI**：通过 `/static/` 下的前端页面与后端交互。

无论哪种方式，最终都会把用户目标交给 `SessionManager`。

相关代码：

- `backend/internal/server/session.go:557 HandleCreateSession`
- `backend/cmd/tui/main.go`

---

### 3.2 SessionManager

`SessionManager` 是会话生命周期的管理者，核心职责：

- 分配会话 ID，例如 `session-1`。
- 创建 `Session` 对象，包含目标、状态、消息历史、事件流。
- 在独立 goroutine 中启动 `runSession`，异步执行图。
- 在会话结束时调用 `persistHistory`，把工具调用结果和摘要写入 Postgres `session_history` 表。
- 提供 SSE 流 `/api/sessions/{id}/stream`，供前端实时消费事件。

`CreateSession` 会初始化一个 `ThreeLayerState`，并把 `DomainGoal` 设为用户目标，然后调用 `ThreeLayerGraph.Invoke`。

相关代码：

- `backend/internal/server/session.go:323 CreateSession`
- `backend/internal/server/session.go:393 runSession`
- `backend/internal/server/session.go:769 HandleSessionStream`

---

### 3.3 ThreeLayerGraph.Invoke

`ThreeLayerGraph` 是整个系统的中央状态机。`Invoke` 方法从 `MetaAgent` 节点开始，循环执行：

1. 根据当前节点名取出对应 `ThreeLayerNode`。
2. 调用 `node.Invoke(ctx, state)`，得到新的 `state`。
3. 根据 `state.NextAction` 决定下一个节点。
4. 重复直到 `ActionFinish`、`ActionWait`、节点返回空、或超过 200 步上限。

`NextAction` 是状态机的核心信号，有五种取值：

- `Continue`：当前节点继续推进。
- `Switch`：切换到另一个 DomainAgent / SessionBlock。
- `Escalate`：把问题升级给 `EscalationHandlerNode`。
- `Finish`：正常结束整个图。
- `Wait`：挂起图，等待用户澄清答复。

相关代码：

- `backend/internal/graph/three_layer_graph.go:248 Invoke`
- `backend/pkg/types/types.go:162 ActionType`

---

### 3.4 MetaAgentNode

`MetaAgentNode` 是第一层 Agent，也是图的入口。它承担以下职责：

#### 3.4.1 复杂度判定

MetaAgent 会调用 `classifyComplexityLLM` 判断用户目标是：

- `simple`：简单问答，直接交给 Assistant。
- `query`：查询/分析类任务，设置 `DirectExecute` 标志，跳过子任务拆解。
- `complex`：复杂任务，需要拆分成多个 DomainAgent 协作。

#### 3.4.2 创建 TaskBoard

MetaAgent 调用 `Runtime.Boards.GetOrCreate(sessionID, goal)` 创建任务看板，并推断出顶层子任务写入看板。后续 DomainAgent 会读取看板、认领任务、更新状态。

#### 3.4.3 拆分领域并创建 DomainAgent

根据目标中的关键词或 LLM 输出，MetaAgent 会创建一个或多个 `DomainAgent` 实例，注册到 `RoleRegistry`，并加入 `state.ActiveBlocks`。

#### 3.4.4 处理 Runtime 组件

每个 tick 顶部，MetaAgent 会：

- 调用 `Watchdog.Check` 监控上下文 token 数。
- 调用 `Mailbox.DrainBroadcast` 读取广播事件，转化为升级或切换信号。
- 读取 `TaskBoard` 了解全局进度。
- 使用 `Skill Registry` 查询可用 Skill。

相关代码：

- `backend/internal/graph/meta_agent.go`
- `backend/internal/graph/meta_agent.go:31 HistoryStore`

---

### 3.5 Runtime 组件

Runtime 是一个聚合对象，避免每个节点单独持有大量依赖指针。它包含四个核心组件：

#### 3.5.1 Watchdog

上下文长度看门狗。MetaAgent 每轮调用 `Watchdog.Check(agentID, contextText)`，估算当前上下文的 token 数，并按阈值返回：

- `OK`：正常。
- `Warn`：接近软阈值，建议监控。
- `Compress`：超过软阈值，建议压缩上下文。
- `Evict`：超过硬阈值，必须切换或驱逐 Agent。

默认阈值：soft=12000，hard=20000。

相关代码：`backend/internal/watchdog/watchdog.go`

#### 3.5.2 TaskBoard

任务看板，每个会话一个。结构：

- `Goal`：全局目标。
- `Tasks`：子任务列表，包含 `ID`、`Title`、`Status`、`Assignee`、`Result`。
- `Constraints`：全局约束。

MetaAgent 创建，DomainAgent 更新，Assistant 读取。

相关代码：`backend/internal/board/board.go`

#### 3.5.3 Mailbox

Agent 间异步邮箱。某 Agent 发现需要通知其他 Agent 时，调用 `Mailbox.Send` 发送 `Message`，包含 `From`、`To`、`Type`、`Subject`、`Body`。主 Agent 通过 `DrainBroadcast` 消费广播消息。

消息类型包括：`milestone`、`request`、`info`、`escalate`、`dependency`。

相关代码：`backend/internal/mailbox/mailbox.go`

#### 3.5.4 Skill Registry

Skill 装配映射表。`DomainAgent` 为某个 Assistant 选择 Skill 后，调用 `Registry.Bind(skillSet)` 记录该 Assistant 的 Skill 子集。Assistant 执行时通过 `GetForAgent` 取出，拼入 system prompt。

相关代码：`backend/internal/skill/registry.go`

---

### 3.6 DomainAgentNode

`DomainAgentNode` 是第二层 Agent，负责单个领域的具体执行。主要职责：

#### 3.6.1 读取任务看板

从 `Runtime.Boards.Get(sessionID)` 取出看板快照，认领状态为 `pending` 且匹配本领域的子任务。

#### 3.6.2 检索历史记忆

- **块记忆（特性3）**：调用 `BlockMemoryStore.SearchBlockMemory`，从 `Postgres + pgvector` 检索相似历史块记忆。
- **归档复用（特性4）**：调用 `DomainArchiveStore.SearchDomainArchive`，检索相似领域的 domainAgent 归档，复用其 Skill 列表和上下文摘要。

#### 3.6.3 分派子任务

DomainAgent 根据任务内容决定：

- 直接创建 Assistant 执行。
- 或先创建 SubDomainAgent 进一步拆分。

#### 3.6.4 更新看板

Assistant 返回结果后，DomainAgent 调用 `Board.MarkDone` 或 `Board.MarkFailed` 更新子任务状态。

相关代码：

- `backend/internal/graph/domain_agent.go`
- `backend/internal/store/domain_archive.go`
- `backend/internal/memory/block_vector.go`

---

### 3.7 SubDomainAgentNode

第三层 Agent，可选。当 DomainAgent 判断某个领域仍然过大时，会创建 SubDomainAgent 进一步拆分。逻辑与 DomainAgent 类似，但通常不会继续拆分，而是直接调用 Assistant。

相关代码：`backend/internal/graph/subdomain_agent.go`

---

### 3.8 AssistantNode

第四层 Agent，真正执行 LLM 调用。流程：

1. **加载角色定义**：从 `RoleRegistry` 读取 `RoleDefinition`。
2. **加载 Skill 子集**：从 `Skill Registry` 读取本 Assistant 的 `SkillSet`。
3. **构造 system prompt**：拼接 `soul.md`、角色定义、Skill 简要。
4. **调用 LLM**：通过 `ModelFactory` 获取对应角色的 blades 客户端。
5. **处理响应**：若 LLM 返回工具调用请求，则交给 `ToolExecutor`；否则返回结果给父 Agent。

Assistant 也可能触发澄清请求，设置 `state.PendingClarify`，让会话进入 `awaiting_clarify` 状态。

相关代码：

- `backend/internal/graph/assistant.go`
- `backend/internal/model/factory.go`

---

### 3.9 ToolExecutor

`ToolExecutor` 负责执行 Assistant 请求的工具。当前支持：

- `ReadFile`、`WriteFile`、`ListDir`
- `RunCommand`
- `SearchInFiles`
- `HTTPGet`、`HTTPPost`

执行结果封装为 `ToolResult`，通过 `ToolCallback` 回调给 `SessionManager`，写入 `session.Events`。

相关代码：

- `backend/internal/graph/tool_executor.go`
- `backend/internal/graph/three_layer_graph.go:107 SetToolCallback`

---

### 3.10 结果回调 / Board 更新 / Session 事件写入

工具执行后，结果会沿两条路径传播：

1. **业务路径**：Assistant 把结果返回给 DomainAgent，DomainAgent 更新 `TaskBoard`，并决定下一步动作。
2. **观测路径**：`SessionManager.handleToolResult` 把工具输出写入 `session.Events`，类型为 `tool_exec`。同时 `handleProgress` 会把 `think`、`intend`、`graph_step` 等调试事件也写入事件流。

这些事件最终通过 SSE 推送给 TUI / Web UI。

相关代码：

- `backend/internal/server/session.go:526 addEventDebug`
- `backend/internal/server/session.go:97 SetToolCallback`
- `backend/internal/server/session.go:102 SetProgressCallback`

---

### 3.11 EscalationHandlerNode 与 Sinker

#### EscalationHandlerNode

当某层 Agent 遇到无法自行处理的问题时，会设置 `ActionEscalate`。`EscalationHandlerNode` 负责仲裁：

- 决定是否需要切换 Domain。
- 或是否需要重新规划任务。
- 或是否需要请求用户澄清。

#### Sinker

终止节点，将 `state.NextAction` 强制设为 `ActionFinish`，结束图循环。之后 `runSession` 返回最终状态，会话进入终态。

相关代码：

- `backend/internal/graph/escalation.go`
- `backend/main.go` / `backend/cmd/tui/main.go` 中的本地 `sinkerNode`

---

### 3.12 会话状态

`runSession` 结束时会话会被设为以下状态之一：

- `running`：正在执行。
- `completed`：正常完成，`Result` 字段保存会话摘要。
- `error`：执行出错，例如节点失败或超过最大步数。
- `awaiting_clarify`：等待用户答复澄清问题，此时图挂起，不继续执行。

当用户通过 `/api/sessions/{id}/clarify` 提交答复后，`SessionManager` 清空 `PendingClarify`，调用 `resumeSession` 继续执行。

相关代码：

- `backend/internal/server/session.go:418 ActionWait 分支`
- `backend/internal/server/session.go:837 HandleSessionClarify`
- `backend/internal/server/session.go:1072 resumeSession`

---

### 3.13 DAG Scheduler

特性1：基于 DAG 的定时任务调度。

- DAG 定义持久化在 Postgres `dag_jobs` 表。
- 每个 DAG 包含多个 `Task`，Task 之间有 `depends_on` 依赖。
- `Scheduler` 按 cron 表达式或手动触发，把可执行的 task 通过 `LaunchSession` 派发为新会话。
- 依赖 task 完成后，后续 task 自动启动。

触发入口：`POST /api/dag/{id}/trigger`。

相关代码：

- `backend/internal/dag/dag.go`
- `backend/internal/server/dag.go`

---

### 3.14 CmdQueue

特性6：用户指令队列，支持两种意图：

- `IntentInterrupt`（抢占中断）：清空当前上下文，以新目标重启 MetaAgent。
- `IntentEnqueue`（队列注入）：把新指令追加到当前上下文继续执行。

MetaAgent 每个 tick 顶部会调用 `CmdQueue.Drain(sessionID)` 读取用户指令并处理。

入口：

- `POST /api/sessions/{id}/interrupt`
- `POST /api/sessions/{id}/enqueue`

相关代码：

- `backend/internal/cmdqueue/cmdqueue.go`
- `backend/internal/server/session.go:897 HandleSessionInterrupt`
- `backend/internal/server/session.go:955 HandleSessionEnqueue`

---

### 3.15 TUI / Web UI

系统的观测与交互层。

- **TUI**：`backend/cmd/tui/main.go` 启动 bubbletea 终端界面，展示对话、任务看板、Agent 拓扑，并支持鼠标滚轮、键盘滚动、指令输入。
- **Web UI**：`web/` 目录下的静态文件，通过 `/api/sessions/{id}/stream` 订阅 SSE 实时事件。

TUI 进程内直接持有 `SessionManager`、`Runtime`、`Graph`，同时启动一个本地 HTTP 端口供输入栏调用 API。

相关代码：

- `backend/cmd/tui/main.go`
- `backend/internal/tui/`
- `backend/internal/server/tui_broadcaster.go`

---

## 4. 核心数据结构

### 4.1 ThreeLayerState

贯穿整个图执行过程的共享状态，包含：

- `SessionID`：会话标识。
- `ActiveBlocks` / `CompletedBlocks`：活跃和已完成会话块。
- `CurrentDomain` / `DomainGoal`：当前领域和目标。
- `CallStack`：嵌套调用栈。
- `PendingClarify`：待处理的澄清请求。
- `NextAction`：状态机下一步动作。

代码：`backend/pkg/types/types.go:187 ThreeLayerState`

### 4.2 RoleInstance

运行时角色实例，记录：

- `ID`、`RoleDefID`、`Type`、`Status`
- `SessionID`、`Domain`、`ParentID`、`Children`

代码：`backend/pkg/types/role.go:80 RoleInstance`

### 4.3 SessionBlock

DomainAgent 的上下文容器，隔离不同领域的状态。

代码：`backend/pkg/types/role.go:158 SessionBlock`

---

## 5. 记忆系统

系统的记忆分为三层：

1. **短期 / 热记忆**：Redis 中的 `AgentSnapshot`，用于快速恢复 Agent 状态。
2. **块记忆（特性3）**：`Postgres + pgvector` 中存储的 DomainAgent 执行结果片段，支持语义检索。
3. **归档复用（特性4）**：`Postgres` 中存储的 domainAgent 归档，包含 Skill 列表、权重、过期时间，支持跨会话复用。

记忆写入时会经过重要性评分、主题绑定、四级压缩（Raw / Standard / Compact / Marker）。

相关代码：

- `backend/internal/memory/write.go`
- `backend/internal/memory/compress.go`
- `backend/internal/memory/search.go`
- `backend/internal/memory/assembler.go`
- `backend/internal/memory/snapshot.go`

---

## 6. 给新成员的建议阅读顺序

1. 先看 `doc/AGENT_FLOW_GUIDE.md` 和 `doc/image/agent_execution_flow.png`，建立整体认知。
2. 读 `backend/pkg/types/types.go` 和 `backend/pkg/types/role.go`，理解核心数据结构。
3. 读 `backend/internal/graph/three_layer_graph.go`，理解状态机如何驱动节点。
4. 读 `backend/internal/graph/meta_agent.go`，理解顶层调度逻辑。
5. 读 `backend/internal/runtime/runtime.go`，理解 Runtime 组件如何被注入节点。
6. 读 `backend/internal/server/session.go`，理解会话生命周期和事件流。
7. 读 `backend/internal/tui/` 和 `backend/cmd/tui/main.go`，理解交互层如何观测系统。

---

## 7. 总结

BlockMemoryAgent 的执行流程可以概括为：

> 用户目标 → 会话管理 → 状态机图 → 四层 Agent 分层执行 → Runtime 协作 → 工具执行 → 事件回流 → 终态或外部触发循环。

每个组件的职责边界清晰：

- `SessionManager` 管会话。
- `ThreeLayerGraph` 管调度。
- `MetaAgent` 管规划。
- `DomainAgent` / `SubDomainAgent` 管领域执行与记忆检索。
- `Assistant` 管 LLM 与工具。
- `Runtime` 管协作组件。
- `Memory` / `Store` 管持久化与检索。
- `DAG` / `CmdQueue` 管外部触发。
- `TUI` / `Web` 管交互与观测。

理解了这张图，就理解了系统 80% 的骨架。
