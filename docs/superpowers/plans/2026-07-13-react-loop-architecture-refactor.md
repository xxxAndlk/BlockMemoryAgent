# BlockMemoryAgent ReAct 主循环架构重构计划

> **对执行者**：本计划是架构级重构，不是渐进清理。当前 `refactor/architecture-cleanup` 分支上的解耦工作（提取基类、拆神文件、反转依赖）**暂停**，先做这次架构层级的重构，否则解耦工作会在即将被删除的代码上浪费。本计划完成后，原 roadmap 中关于 graph 节点解耦、记忆管线配置化的任务大部分自动失效。

**Goal:** 把当前 `ThreeLayerGraph` 4 节点状态机（Meta/Domain/SubDomain/Assistant + CallStack/SessionBlock/NextAction 路由）重写为单一 ReAct 主循环 + 异步子 Agent 工具分发。三层架构不再由图拓扑实现，而是由 `call_sub_agent` 工具递归涌现。记忆从 4 级管线（write/compress/search/assembler/snapshot，2.4k 行）压缩为两段事件流（主 Agent 事件 + 领域 Agent 事件）。graph 包从 10.8k 行压到 ~3k。

**核心判断：** 现架构把"哪个角色哪个层"做成编译期节点类型 + 运行期状态机路由，导致每个节点各自实现 LLM 调用 + 工具调用 + 记忆调用 + 角色路由，逻辑散在 6 个节点文件 + 5 个 meta_*.go 拆分 + router + role_factory 中。`roles.yaml` 已经按"角色 = 可调用工具"设计（`can_be_called` / `parents` / `tools` 字段），4 节点状态机在跟配置设计打架。正确做法是把角色调用统一为工具分发，让 Go 调用栈承担层级关系。

**Tech Stack:** Go 1.25, go-kratos Blades, PostgreSQL + pgvector, Redis, bubbletea。不引入新依赖。

---

## 当前架构问题诊断

### 1. 主循环被节点分发掩盖

`graph_loop.go:29` `Invoke` 是状态机循环，每 tick = `查节点 -> node.Invoke -> determineNext`。循环本身干净（175 行），但只做 dispatch，业务全在节点里。结果：主循环看不见，看到的是 6 个节点类型各自的状态转换。

### 2. 4 层节点重复实现编排逻辑

- `MetaAgentNode.Invoke` -> `handleInitial` / `handleBlockEvents` / `switchToNextBlock` / `handleCrossDomainRequest`，拆到 `meta_handle.go` / `meta_utils.go` / `meta_domain.go` / `meta_archive.go` 5 个文件
- `DomainAgentNode.Invoke` 220+ 行（snapshot 加载 + skill 装配 + 任务拆解 + Plan + 派发 + 自测 + 归档）
- `SubDomainAgentNode` / `AssistantNode` 各自再实现一遍 LLM 调用 + 工具执行 + 记忆回调

每层都重新发明"调 LLM -> 解析工具调用 -> 执行工具 -> 收集结果"循环，但用不同的 `NextAction` 信号驱动。

### 3. 状态机模拟函数调用栈

`CallStack` / `SessionBlock` / `NextAction{Continue,Switch,Escalate,Finish,Wait}` / `determineNext` / `resolveInstanceNode` 这一整套机制（`graph_routes.go` + `graph_resolve.go` + `router.go` + `router_score.go` + `role_registry.go` + `role_factory.go`，~2.4k 行）本质上是在重新实现 Go 原生的函数调用栈 + 工具分发。`Switch` = 函数调用，`Finish` = return，`Escalate` = 异常上抛，`Wait` = future 等待。

### 4. 记忆管线过度工程

`internal/memory/` 2.4k 行，5 阶段：
- `write.go`（381 行）：4 子步流水线 Summarizer -> FactExtractor -> ImportanceScorer -> TopicDetector
- `compress.go`（106 行）：CLAUDE.md 称 4 级 Raw->Standard->Compact->Marker，**代码只实现 2 级**（Raw/Standard）
- `search.go`（262 行）：4 信号评分（语义 0.4 + 实体 0.25 + 时间 0.15 + 因果 0.2）
- `assembler.go`（350 行）：4 段上下文 + 7 项 Token 预算
- `snapshot.go`（249 行）：Redis 热存 + PG 冷存 2 级

实际项目用不到 RAG（单会话内的事件流足够），4 段预算装配对 LLM 决策帮助有限。压缩只做了 2 级，Compact/Marker 是空头支票。

### 5. 工具与记忆调用边界模糊

- 工具逻辑分散在 `tool_executor.go` / `tool_files.go` / `tool_command.go` / `blades_tools.go`（1.6k 行），没有统一"工具模块"入口
- 记忆有 `internal/memory/` 包但节点不直调，走 `MemoryCallbackHandler` 回调注入，调用链断裂
- 7 套 `SetX` / `injectTo[X]Receiver` 注入机制（`three_layer_graph.go`）传播依赖

---

## 目标架构

### DDD Bounded Contexts

```
agent/              应用层 - ReAct 主循环，只依赖接口
domain/memory/      记忆域 - 两段事件流写入 + 上下文装配
domain/tool/        工具域 - registry + dispatch，builtin(file/cmd) + MCP 桥
domain/subagent/    子Agent域 - call_sub_agent 工具，异步 mailbox + goroutine
domain/role/        角色域 - 角色定义 + system prompt 装配 + skills
domain/skill/       技能域 - skill pool（现有，几乎不动）
infra/store/        postgres/redis（现有，不动）
infra/model/        LLM client（现有，不动）
infra/board/        任务看板（现有，从外围变核心）
infra/mailbox/      邮箱（现有，从外围变核心）
```

### 主循环

```go
// agent/agent.go
type Agent struct {
    llm     model.LLMClient
    tools   ToolRegistry
    memory  MemoryPipeline   // 可空，nil 时跑无记忆工具循环
    role    RoleDefinition
    mailbox Mailbox          // 异步子 Agent 通信
}

func (a *Agent) Run(ctx context.Context, input string) (Result, error) {
    history := []Message{{Role: "user", Content: input}}
    for {
        ctxMsgs := a.memory.Assemble(a.role, history)   // 自动，LLM 不参与决策
        resp, err := a.llm.Generate(ctx, a.role.SystemPrompt, ctxMsgs, a.tools.Schema())
        if err != nil { return Result{}, err }
        history = append(history, resp.Message)

        if !resp.HasToolCalls {
            a.memory.Write(a.role.ID, Event{Type: "answer", Content: resp.Text})
            return Result{Text: resp.Text, History: history}, nil
        }

        for _, tc := range resp.ToolCalls {
            result := a.tools.Dispatch(ctx, tc)          // 可能异步起 sub-agent
            a.memory.Write(a.role.ID, Event{Type: "tool_call", Name: tc.Name, Input: tc.Input, Output: result.Output})
            history = append(history, result.Message)
        }
    }
}
```

### 异步子 Agent 分发

`call_sub_agent(role_id, task)` 工具行为：

1. 主 Agent LLM emit `call_sub_agent` tool_call
2. `tools.Dispatch` 命中 `domain/subagent` 处理器
3. 处理器创建子 Agent 实例（同 `Agent` struct，不同 role config）
4. 起 goroutine 跑子 Agent.Run，立即返回 `sub_agent_id` 给主 Agent
5. 子 Agent 完成后把 `task_goal_summary` 推 mailbox，通知主 Agent
6. 主循环每 tick `mailbox.Poll()`，收到的消息作为 ctx 注入下一轮 LLM
7. 主 Agent LLM 判定"所需消息齐了" -> emit 终止 tool_call 或纯文本回复 -> 返回用户

**长任务支持：**
- 主 Agent emit `defer_background` tool_call -> HTTP 早返回给用户，session 标记后台运行
- 子 Agent 继续跑，结果落 postgres
- 用户后续可查询 session 状态 + 结果

**Go 调用栈即层级：** MetaAgent 调 `call_sub_agent(domain_agent, ...)` -> DomainAgent 调 `call_sub_agent(assistant, ...)` -> Assistant 跑工具循环。三层 = 3 层递归，无 CallStack/SessionBlock。

**固定角色 = 工具：** `code_assistant` / `ui_assistant` 等 `fixed_roles` 注册为 `call_code_assistant(task)` / `call_ui_assistant(task)` 工具（或一个 `call_role(role_id, task)` 工具 + 每 role 白名单）。`roles.yaml` 的 `can_be_called` / `parents` 字段直接映射工具可用性。

### 两段记忆

砍掉 compress / search / assembler / snapshot 4 个文件（~970 行），保留 `write.go` 简化版。

**主 Agent 记忆** = 事件流：
```go
Event{Type: "call_sub_agent", Role: "domain_agent", Task: "...", SubAgentID: "..."}
Event{Type: "sub_agent_summary", SubAgentID: "...", Summary: "..."}
Event{Type: "answer", Content: "..."}
```

**领域 Agent 记忆** = 事件流：
```go
Event{Type: "tool_call", Name: "ReadFile", Input: "{...}", Output: "..."}
Event{Type: "fixed_assistant_summary", Role: "code_assistant", Summary: "..."}
Event{Type: "task_goal_summary", Summary: "..."}   // 完成时推 mailbox 给主 Agent
```

**MemoryPipeline 接口：**
```go
type MemoryPipeline interface {
    Assemble(role RoleDefinition, history []Message) []Message  // 注入 ctx，可空实现
    Write(agentID string, event Event) error                    // 追加事件
}
```

无 RAG，无压缩，无向量检索。pgvector 表保留不删，未来需要 RAG 时再接。

**一行注入：**
```go
agent.New(llm, tools, role, mailbox).WithMemory(memPipe).Run(ctx, input)
```

---

## 代码清单：死 / 留 / 新

### 死（删除，~7.3k 行）

| 文件/目录 | 行数 | 死因 |
|---|---|---|
| `internal/graph/meta_agent.go` + `meta_handle.go` + `meta_utils.go` + `meta_domain.go` + `meta_archive.go` | ~1.1k | MetaAgent 逻辑折叠进主循环 + 工具 |
| `internal/graph/domain_agent.go` + `domain_subdomain.go` | ~0.7k | Domain/SubDomain 节点 -> `call_sub_agent` 工具递归 |
| `internal/graph/subdomain_agent.go` + `assistant.go` | ~0.5k | 同上 |
| `internal/graph/escalation.go` + `sinker.go` | ~0.1k | Escalation/Sinker 节点不需要 |
| `internal/graph/three_layer_graph.go` + `graph_loop.go` + `graph_routes.go` + `graph_resolve.go` | ~0.8k | 状态机循环 + 路由 + 解析全删 |
| `internal/graph/router.go` + `router_score.go` | ~0.6k | LLM 用 tool_call 选子 Agent，不打分 |
| `internal/graph/role_registry.go` + `role_factory.go` | ~0.8k | 动态解析 -> 工具分发 |
| `internal/graph/blades_agent_runner.go` | 540 | 折进主循环 |
| `internal/graph/agent_common.go` + `llm_tools.go` + `tool_executor.go` + `tool_files.go` + `tool_command.go` + `blades_tools.go` | ~2.4k | 折进 `domain/tool/` |
| `internal/memory/compress.go` + `search.go` + `assembler.go` + `snapshot.go` | ~0.97k | 4 级管线砍 4 文件 |
| `internal/memory/callback.go` + `block_vector.go` + `workspace_adapter.go` | ~0.4k | 回调机制 + 适配器不再需要 |
| `pkg/types/` 中 `ThreeLayerState` / `CallStack` / `SessionBlock` / `NextAction` / `TokenBudget` / `RelevanceScore` / `AgentSnapshot` 等 | ~0.3k | 类型不再需要 |

### 留（保留，部分简化）

| 文件/目录 | 行数 | 处理 |
|---|---|---|
| `internal/memory/write.go` | 381 | 简化：砍 FactExtractor / ImportanceScorer / TopicDetector，留 Action + Summary + ToolCalls 事件追加 |
| `internal/skill/` | 620 | 几乎不动，system prompt 装配入口 |
| `internal/board/` | 473 | 不动，从外围变核心（任务跟踪） |
| `internal/mailbox/` | 353 | 不动，从外围变核心（异步通信） |
| `internal/store/` | 2.2k | 不动，pgvector 表保留 |
| `internal/model/` | - | 不动，LLM client |
| `internal/runtime/runtime.go` | 119 | 简化，不再传播到节点，Agent struct 直持依赖 |
| `internal/config/` | 594 | 简化 `MemoryPolicyConfig` / `TokenBudget` 相关字段 |
| `internal/bootstrap/` | - | 重写 `Build`，从 `BuildHandler` 改名 |
| `config/roles.yaml` | 228 | 几乎不动，`can_be_called` / `parents` / `tools` 已是工具图谱 |
| `internal/server/` | 2.4k | HTTP 层适配新 `agent.Agent` 接口，session 管理 + SSE 流对接 mailbox 事件 |
| `internal/tui/` | 4.2k | 适配新事件流，砍 ThreeLayer 相关视图 |

### 新（创建，~3k 行）

| 文件/目录 | 估计行数 | 职责 |
|---|---|---|
| `internal/agent/agent.go` | ~200 | ReAct 主循环 |
| `internal/agent/types.go` | ~100 | Agent / Result / Event / Message DTO |
| `internal/domain/tool/registry.go` | ~150 | 工具注册 + dispatch |
| `internal/domain/tool/builtin.go` | ~400 | file/cmd 工具（从 tool_files/tool_command 折过来） |
| `internal/domain/tool/mcp.go` | ~200 | MCP 桥（预留） |
| `internal/domain/subagent/dispatcher.go` | ~250 | `call_sub_agent` 工具 + goroutine + mailbox 集成 |
| `internal/domain/memory/pipeline.go` | ~150 | MemoryPipeline 接口 + 事件流实现 |
| `internal/domain/memory/store.go` | ~100 | 事件流 postgres 持久化（复用 store 包） |
| `internal/domain/role/registry.go` | ~200 | 角色注册 + system prompt 装配 |
| `internal/domain/role/loader.go` | ~150 | 从 roles.yaml 加载 |

**总行数变化：** 40k -> ~33k（净减 ~7k），graph 包 10.8k -> 0（合并进 agent + domain），memory 2.4k -> ~0.5k。

---

## 迁移阶段

### Phase 0: 准备（不破现状）

- [ ] 0.1 冻结 `refactor/architecture-cleanup` 分支当前进度，记录已完成的解耦工作（部分可能在 Phase 3 后失效）
- [ ] 0.2 跑全量测试基线：`GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1`，记录通过/失败
- [ ] 0.3 标记 `internal/graph/` / `internal/memory/` 中即将删除的文件，加 `// Deprecated: will be removed in ReAct refactor` 注释
- [ ] 0.4 创建新分支 `refactor/react-loop` 基于 `refactor/architecture-cleanup`

### Phase 1: 新代码骨架（不改现有代码）

- [ ] 1.1 创建 `internal/agent/` 目录，写 `agent.go` 主循环骨架（无记忆，无 sub-agent，纯工具循环）
- [ ] 1.2 写 `agent.Agent` 接口 + DTO，与 server/TUI 后续对接
- [ ] 1.3 创建 `internal/domain/tool/` 目录，写 `registry.go` + 迁移 `tool_executor.go` 的 switch 分发为注册表
- [ ] 1.4 迁移 `tool_files.go` / `tool_command.go` 到 `domain/tool/builtin.go`，保留行为
- [ ] 1.5 创建 `internal/domain/role/`，从 `graph/role_registry.go` 抽出角色定义 + 加载逻辑
- [ ] 1.6 单测：`agent.Run` 工具循环 + 内置工具分发

### Phase 2: 记忆 + 异步分发（不改现有代码）

- [ ] 2.1 创建 `internal/domain/memory/`，写 `MemoryPipeline` 接口 + 事件流实现
- [ ] 2.2 简化版 `write.go`：留 Action + Summary + ToolCalls，砍 FactExtractor/ImportanceScorer/TopicDetector
- [ ] 2.3 事件流 postgres 持久化（复用 `internal/store/`，新表 `agent_events`）
- [ ] 2.4 创建 `internal/domain/subagent/`，写 `call_sub_agent` 工具 dispatcher
- [ ] 2.5 goroutine 起子 Agent + mailbox 通知 + 主循环 poll 集成
- [ ] 2.6 `defer_background` 长任务支持
- [ ] 2.7 单测：异步分发 + mailbox 通信 + 事件流写入

### Phase 3: 切换适配层（破现状，保 HTTP/TUI 兼容）

- [ ] 3.1 `internal/server/` 适配新 `agent.Agent` 接口，session 管理重写
- [ ] 3.2 SSE 流对接 mailbox 事件 + agent 事件流，保持 `/api/sessions/{id}/stream` 兼容
- [ ] 3.3 `internal/tui/` 适配新事件流，砍 ThreeLayer 相关视图（agent_tree_panel 等）
- [ ] 3.4 `internal/bootstrap/` 重写 `Build`，从 `BuildHandler` 改名
- [ ] 3.5 `cmd/` 入口适配（main.go / demo / tui / memory-console）
- [ ] 3.6 集成测试：`test/api/*` HTTP API 兼容性验证
- [ ] 3.7 集成测试：`test/tui/*` keystream 兼容性验证
- [ ] 3.8 集成测试：`test/coding/*` end-to-end 验证（snake / css-refactor / bug-fix）

### Phase 4: 删除死代码

- [ ] 4.1 删除 `internal/graph/` 全部（10.8k 行）
- [ ] 4.2 删除 `internal/memory/` 中 compress/search/assembler/snapshot/callback/block_vector/workspace_adapter（~1.4k 行）
- [ ] 4.3 删除 `pkg/types/` 中 ThreeLayer 相关类型
- [ ] 4.4 删除 `internal/runtime/` 中 graph 节点传播相关代码
- [ ] 4.5 清理 `config/` 中失效字段（MemoryPolicyConfig / TokenBudget 配置项）
- [ ] 4.6 全量测试 + lint

### Phase 5: 文档与配置

- [ ] 5.1 更新 `CLAUDE.md`（架构章节重写）
- [ ] 5.2 更新 `doc/项目说明.md`（目录布局 + 代码阅读顺序）
- [ ] 5.3 更新 `doc/TODO.md`（标记完成项 + 新增未来项）
- [ ] 5.4 更新 `docs/superpowers/plans/dependency-rules.md`（包依赖规则更新）
- [ ] 5.5 标记 `2026-07-08-blockmemoryagent-refactoring-roadmap.md` 中失效任务

---

## 待定边界

### 1. SSE 流式对接异步 mailbox

**问题：** 现架构 SSE 流推 `graph_step` / `wait` / `tool_call` 等事件，前端依赖这些事件类型。新架构主循环 + 异步子 Agent 产生的事件流如何映射？

**方案候选：**
- (a) 主 Agent 事件流直接映射 SSE，子 Agent 事件通过 mailbox 转发到主 Agent 再推 SSE
- (b) 子 Agent 事件独立推 SSE（前端需区分 agent_id）
- (c) 只推主 Agent 事件，子 Agent 事件落库不推流

**倾向：** (a)，保前端兼容，子 Agent 事件经主 Agent 汇总。

### 2. TUI agent_tree_panel 视图

**问题：** 现 TUI 显示三层 Agent 树（MetaAgent -> DomainAgent -> Assistant），新架构无显式层级。

**方案候选：**
- (a) 砍 agent_tree_panel，只显示当前 Agent + 工具调用流
- (b) 从 mailbox 事件动态构建 Agent 调用树（运行时层级，非编译期）
- (c) 保留树视图但改为"主 Agent + 子 Agent 列表"扁平结构

**倾向：** (b)，运行时从事件流构建，保留多 Agent 可视化。

### 3. 任务看板（board）的角色

**问题：** 现 `internal/board/` 跟踪 DomainAgent 任务块（TaskBlock），新架构无 SessionBlock。看板是否还需要？

**方案候选：**
- (a) 砍 board，任务跟踪完全靠 mailbox 事件流
- (b) 保留 board，任务块改为 `call_sub_agent` 创建的子 Agent 跟踪
- (c) 保留 board 但简化，只跟踪后台长任务状态

**倾向：** (c)，board 用于 `defer_background` 长任务状态查询，前台同步任务靠 mailbox。

### 4. Watchdog 的去留

**问题：** 现 `internal/watchdog/` 监控上下文长度，触发记忆压缩。新架构砍压缩，Watchdog 是否还需要？

**方案候选：**
- (a) 砍 watchdog，上下文超限直接报错
- (b) 保留 watchdog，超限时触发事件流截断（保留最近 N 条 + 总结）
- (c) 保留 watchdog，超限时触发 `defer_background` 让主 Agent 早返回

**倾向：** (b)，事件流截断是简单可行的兜底。

### 5. 历史会话兼容

**问题：** postgres 中已有 `session_history` / `session_events` / `session_logs` 表，存的是 ThreeLayer 状态机格式。新架构事件流格式不同。

**方案候选：**
- (a) 新建 `agent_events` 表，旧表保留只读，新会话写新表
- (b) 复用 `session_events` 表，schema 迁移加新字段
- (c) 全新 schema，旧数据归档不读

**倾向：** (a)，新表新格式，旧表保留供历史会话查询。

---

## 验证

### 单元测试

- `agent.Run` 主循环：无记忆 / 有记忆 / 工具循环 / 子 Agent 递归
- `domain/tool/registry`：注册 + dispatch + 未注册错误
- `domain/subagent/dispatcher`：异步起 goroutine + mailbox 通知 + 主循环 poll
- `domain/memory/pipeline`：事件流写入 + 读取 + Assemble 注入

### 集成测试

- `test/api/*`：HTTP API 兼容性（`/api/sessions` CRUD + SSE 流）
- `test/tui/*`：keystream 兼容性
- `test/coding/*`：snake-game / css-refactor / bug-fix end-to-end（验证三层涌现 + 工具分发 + 异步通信）

### 性能基线

- 单会话 token 消耗对比（新架构 Assemble 简化，应该更低）
- 子 Agent 异步分发延迟（mailbox 投递 + goroutine 启动）
- 长任务后台运行 + 前端查询延迟

### 回归

- 全量 `GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1` 通过
- `make lint` 通过
- `make run` 启动 + HTTP API 冒烟
- `make backend-test` + `make test-test` 通过

---

## 风险

1. **异步 mailbox 顺序性：** 子 Agent 完成顺序不固定，主 Agent LLM 必须能处理乱序到达的 summary。Prompt 设计需明确"等待所有 N 个子 Agent 完成"的语义。
2. **goroutine 泄漏：** 子 Agent 跑飞或卡住，主 Agent 永远 poll 不到。需要 session 级超时 + goroutine 取消机制。
3. **事件流膨胀：** 无压缩，长会话事件流可能很大。需要截断策略（Watchdog 触发）或定期归档。
4. **测试 fixture 重写：** `test/fixtures/mock_llm.go` 基于 ThreeLayer 状态机设计，新架构需要新的 mock 序列。`test/coding/*` 三个端到端用例的 mock 脚本要重写。
5. **配置兼容：** `roles.yaml` 字段保留，但 `MemoryPolicyConfig` / `TokenBudget` 配置项失效。需要迁移指南。

---

## 不做

- 不引入新外部依赖（MCP SDK 等预留接口，不实现）
- 不重写 store / model / board / mailbox / skill 包内部
- 不改 postgres schema（只加新表，不删旧表）
- 不改 HTTP API 路径（保兼容）
- 不改 `config/roles.yaml` 字段（保兼容）
- 不做 RAG / 向量检索（pgvector 表保留，未来用）
- 不做记忆压缩（事件流原样保留，靠 Watchdog 截断）
