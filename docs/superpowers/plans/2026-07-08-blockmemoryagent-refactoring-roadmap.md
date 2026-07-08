# BlockMemoryAgent 架构解耦与代码清晰化重构计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 通过提取通用基类/接口、拆分神文件、反转错误依赖、策略配置化、参数对象化，将当前臃肿的 backend/frontend 代码重构为边界清晰、职责单一、可测试、可演进的架构。**核心交付物为 `internal/agent` 模块**：把 Agent 编排能力（graph + runtime + session 生命周期）封装为稳定 Go 接口，对话模块（HTTP）、TUI 模块（in-process）通过该接口调用 Agent，不再直接依赖 `internal/graph` / `internal/runtime` 内部类型。测试模块作为后期需求延期，本次不涉及测试基础设施改造。

**Architecture:** 以"基础设施 ↔ Agent 模块 ↔ 适配层 ↔ 业务前端"四层重新切分：底层 store/memory/model/logger 只暴露接口；中间 `agent` 模块封装 graph/runtime/skill/soul/watchdog，对外暴露 `agent.Agent` 接口与 DTO；适配层 `server`（HTTP）、`tui`（in-process）、`test`（mock 注入）依赖 `agent.Agent` 接口，不直接依赖 graph；业务规则（路由关键词、工具策略、prompt 模板、温度表）全部外置到配置。frontend 按 API/类型/composables/组件/视图分层。

**Tech Stack:** Go 1.25, go-kratos Blades, PostgreSQL + pgvector, Redis, Vue 3 + Vite + Tailwind, bubbletea.

## Global Constraints

- **不改动现有业务逻辑与思维链路**：路由策略、任务拆解、记忆召回、工具执行结果、Agent 层级调用关系保持原行为；重构只改变代码组织方式。
- **行为修复必须显式标注**：部分 Task 修复已知 bug（错误吞掉、UTF-8 截断、死锁、计数语义错误等），属于行为变更而非纯重构。此类 Task 在标题后标注 `(行为修复)`，必须在 Step 1 先写回归测试固化旧行为，再在 Step 2 修复，确保修复前后行为差异可观测、可回滚。
- **不删除未来优化项**：`SubDomainAgent`、MCP 相关预留接口、DAG 特性等当前未启用但规划内代码只进行封装/隔离，不删除、不移除类型常量、不破坏存储兼容性。
- **不改变现有外部行为**：HTTP API、TUI 交互、CLI 入口、配置文件路径保持兼容。
- **所有改动必须保留并补充测试**；Go 侧使用 `GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1` 验证。
- **配置 YAML 路径保持兼容**，新字段提供默认值，旧字段通过嵌入保持向上兼容。
- **不引入新的外部依赖**（优先使用标准库或已有依赖）。
- **每个 track 独立可交付、可测试**；每次提交聚焦一个任务。
- **工程化优先**：通过接口抽象、DTO、Options 模式、worker pool、常量/配置外置、包边界清晰化等手段提升代码可读性、可测试性、可维护性。

## 工程化原则

本次重构的核心目标是**在不改变行为的前提下，让代码更像一个可维护的工程产物**。遵循以下原则：

1. **单一职责**：每个文件/结构体/函数只做一件事；神文件按领域拆分。
2. **显式依赖**：通过构造函数和接口注入依赖，避免全局状态和隐式构造。
3. **配置外置**：业务阈值、超时、prompt 模板、温度表、路由关键词等全部抽到配置，代码只保留保守默认值。
4. **DTO 化**：减少长参数列表，用小型 DTO / Options 对象封装调用上下文。
5. **错误显式传播**：不忽略可恢复错误；使用 `errors.As` 处理包装错误；不吞掉关键异常。
6. **可测试性**：每个组件可独立 mock；避免在 View/渲染路径中触发 IO/LLM。
7. **可复用抽象**：同一类功能（文本截断、JSON 提取、日期格式化、HTTP helper）只保留一个实现。
8. **向后兼容**：旧配置路径、旧 HTTP 路由、旧类型名称尽量保留，新能力通过新增字段/文件实现。

---

## 当前技术债全景（按域）

### 1. Graph 编排层（`backend/internal/graph/`）

- **三层节点重复代码**：`MetaAgentNode` / `DomainAgentNode` / `SubDomainAgentNode` 都有相同的 `Set*` 注入、`emit` / `emitDetail` / `sessionLogger`、LLM tracker 绑定。
- **LLM 调用包装重复**：`meta_llm.go`、`domain_llm.go`、`subdomain_llm.go` 中 `callLLMAs` / `callLightweightAs` 结构高度重复。
- **God method**：`DomainAgentNode.Invoke` 超过 220 行，涵盖快照加载、Skill 装配、任务拆解、Plan、派发、自测、归档。
- **工具层混入业务规则**：`tool_files.go` 嵌入邮箱文件化拦截、临时脚本反模式拦截；`tool_command.go` 嵌入长运行服务器检测。
- **工具分发巨型 switch**：`tool_executor.go` 用 11 个 case 分发，新增工具需改两处。
- **SubDomain 代码未封装**：运行时默认禁用，但实现散落在多文件中，缺少统一门面与启用开关，理解成本高。
- **依赖注入重复**：`three_layer_graph.go` 中多个 `Set*` 方法重复遍历 nodes 做类型断言。
- **Magic number 遍布**：截断长度、token 上限、重试次数、循环检测阈值均未集中配置。
- **JSON 提取逻辑重复**：`role_factory.go` 与 `agent_common.go` 各有一份，行为不一致。

### 2. Memory & Storage 层（`backend/internal/memory/`、`store/`、`embed/`）

- **`PostgresStore` 神文件**：1,059 行，混合私有记忆、快照、全局知识、话题、Agent 注册表、决策日志、session 历史/事件、schema 管理。
- **分层倒置**：`memory` 包导入 `graph`；`store` 包导入 `dag`。
- **`RedisStore` 职责混杂**：同时管理 topic meta、agent outputs、events、decisions、deps graph、snapshot、archive、TTL。
- **硬编码业务策略**：上下文分段比例、默认 200 tokens/条、向量维度 768、ivfflat lists=100 等写死。
- **Embedding 调用不统一**：`BlockMemoryRecord.ToKnowledgeRecord` 直接调用 `embed.PseudoEmbed`，无视配置的真实 embedder。
- **Callback 大函数**：`memory/callback.go:OnEnd` 顺序执行写 Episode、广播、拉 Episode、保存快照、广播状态。
- **参数冗余**：`RegisterAgent` 使用 7 个位置参数；`SearchAndScore` 传入未使用的 `agentID`/`topicID`。
- **错误处理缺失**：`SearchAndScore` embedding 失败静默忽略；`SaveKnowledge` 序列化错误忽略；`IsDuplicateError` 不使用 `errors.As`。

### 3. Runtime & 跨组件层（`runtime/`、`config/`、`board/`、`mailbox/`、`skill/`、`soul/`、`watchdog/`、`logger/`、`dag/`、`cmdqueue/`）

- **`config.AgentConfig` 臃肿**：30+ 字段混合基础设施、Agent 运行时、安全策略、记忆管线、特性开关。
- **`runtime.New` 硬编码构造且 panic**：无法替换 board/mailbox/watchdog；soul 加载失败直接 panic。
- **`dag.Scheduler` 职责过多**：同时做 cron 触发、DAG 状态机、session 派发、持久化回调、HTTP 快照；用 JSON 序列化做深拷贝；goroutine 生命周期管理缺失。
- **业务规则侵入通用层**：`skill` prompt 硬编码、`soul` 温度表硬编码、`watchdog` token 估算硬编码 `bytes/4+1`。
- **`logger` goroutine 风暴**：每条日志起一个 goroutine + 3 次重试，无背压；`LLMCall` 9 个位置参数；按字节截断 UTF-8；修改入参 record。
- **`mailbox` 自定义 `itoa`**：手写 `itoa` 替代 `strconv`；`Drain`/`DrainBroadcast` 重复逻辑。
- **`board` 裸字符串状态**：`"NEW"/"IN_PROGRESS"/"DONE"/"FAILED"` 散落；ID 生成依赖 `len(Order)+1`。
- **`cmdqueue` 无容量上限**：单个 session 指令队列可能无限增长。

### 4. Server / API / TUI 层（`server/`、`model/`、`tui/`、`cmd/`、`main.go`）

- **`SessionManager` 神类/神文件**：`session.go` 2,236 行，同时承担会话生命周期、HTTP handler、事件追加/裁剪、持久化、统计聚合、看门狗/邮箱适配、`graph.Invoke` 调用。
- **`runSession` / `resumeSession` 高度重复**：终态处理、clarify/queue 逻辑、临时目录清理均重复。
- **硬编码事件 kind / 状态字符串**：`"think"`、`"tool_exec"`、`"running"` 等遍布 server 与 tui。
- **HTTP handler 手工解析路径**：大量 `TrimPrefix` / `TrimSuffix` / `r.URL.Path[len(...):]`。
- **TUI `Model` 过度膨胀**：`model.go` 30+ 字段；`view.go` 在渲染路径中触发 LLM 调用创建无界 goroutine。
- **TUI 与生产 wiring 分叉**：`cmd/tui/main.go` 与 `testserver.go` 重复实现 `sessionRouter`、`pgBlockMemoryAdapter`、`pgHistoryAdapter`、`sinkerNode`。
- **`APIHandler` 职责混杂**：直接写 SQL 查询 `agent_private_memory`；`paused map` 无锁访问；`SnapshotHandler` 与 `SnapshotInspectHandler` 重复。
- **`LLMCallTracker` 错误计数语义错误**：任何错误都计入 `timeoutCount` 并触发 slow mode；取消也被计入。
- **`BladesClient` 4 个 Generate 方法重复构造请求与校验**。
- **`memory-console` SSE 每条事件重建连接**。

### 5. Frontend 层（`web/src/`）

- **API 层集中**：`api/session.ts` 一个文件承载所有领域 API，并在内部重复定义 `HealthStatus` / `Skill` 类型。
- **核心视图职责过重**：`chat/index.vue` 479 行、`session/index.vue` 633 行，同时处理路由、SSE、轮询、状态机、业务计算、模板。
- **业务规则重复**：Agent 树、任务看板、状态颜色/标签在 chat/session/dashboard/ExecutionLog 中重复实现。
- **类型冲突/弱类型**：`Skill`、`HealthStatus` 在 `types/index.ts` 与 `api/session.ts` 各有一份；多处 `any[]`。
- **占位/mock 数据**：`history/index.vue` 全部硬编码假数据；`knowledge/settings/skills/soul` 多个页面为 placeholder。
- **事件分组大函数**：`turns.ts` 237 行，依赖中文前缀与魔法值。
- **全局注册所有图标**：`main.ts` 无法 tree-shaking。
- **时间格式化/ markdown 样式/卡片样式重复**。

### 6. Tests / Config / Migrations / Docs

- **`test/fixtures/llm.go` 嵌套锁死锁**：`handle` 已加锁后调用 `pickResponse` 再次加锁，`sync.Mutex` 非可重入。
- **测试配置手写完整 YAML**：`test/fixtures/config.go` 用字符串拼接生成整套 roles/skills/soul，与生产配置 schema 强耦合。
- **API 测试重复样板**：几乎每个测试都重复"创建会话 → JSON 解码 → 取 id"。
- **迁移文件残留死信表**：`004_memory_write_failures.sql` 与 TODO P0-2 已完成项冲突；`004` 重复创建 `step_count` 列与索引。
- **配置混合**：`config/config.yaml` 基础设施与 Agent 策略/特性开关混排；`config/roles.yaml` 大量重复 `model_config`。
- **文档漂移**：`项目说明.md` 迁移文件描述与实际不符；`设计文档_v3.md` 与实现偏离。

---

## Refactor Tracks（按依赖顺序执行）

| Track | 名称 | 核心目标 | 依赖 |
|---|---|---|---|
| 1 | Graph 编排层解耦 | 提取 `BaseAgentNode` / `LLMCaller` / `ToolRegistry` / `Guard` / `LoopConfig`；封装 SubDomain 代码（不删除） | 无 |
| 2 | Memory & Storage 解耦 | 拆分 `PostgresStore` / `RedisStore`；解除 `memory→graph`、`store→dag` 依赖；配置化记忆策略 | Track 1（部分类型下移） |
| 3 | Runtime & 组件治理 | `Runtime` Options 模式；`config.AgentConfig` 拆分；`logger` worker pool；策略接口化 | Track 2（配置结构） |
| 4 | Agent 模块 API 封装与 Server/TUI 瘦身 | 定义 `internal/agent.Agent` 接口，封装 graph+runtime 为模块；HTTP/TUI/test 通过接口调用；拆分 `SessionManager`；统一事件类型；TUI 子模型；统一 wiring | Track 1, 3 |
| 5 | Frontend 结构化 | 拆分 API / 类型 / composables / 组件；统一状态与样式 | Track 4（事件协议稳定后） |
| 6 | Tests / Config / Docs 清理 | 修复 LLM mock 死锁；提取测试 helper；模板化测试配置；清理迁移重复；拆分配置；更新文档 | Track 3, 4 |
| 7 | 工程化能力补齐 | 统一错误处理、日志包边界、通用 HTTP 工具、Makefile、包依赖方向文档 | Track 1-4 |

---

## Track 1: Graph 编排层解耦

**目标：** 消除 Meta/Domain/SubDomain 三层节点的重复代码，将工具执行、业务策略、循环配置、JSON 提取等抽象为独立接口/包，使 `internal/graph` 从“万能编排包”回归为状态机与节点调度框架。

### Task 1.1: 提取 `BaseAgentNode` 统一三层节点公共行为

**Files:**
- Create: `backend/internal/graph/base_node.go`
- Modify: `backend/internal/graph/meta_agent.go`, `domain_agent.go`, `subdomain_agent.go`
- Test: `backend/internal/graph/agent_common_test.go`, `meta_sync_test.go`

**Interfaces:**
- Consumes: `*model.ModelFactory`, `ProgressCallback`, `*logger.Logger`, `*runtime.Runtime`, `ToolCallback`
- Produces: `BaseAgentNode` 提供 `Emit/EmitDetail/SessionLogger/TrackLLMCall/SetProgressCallback/SetModelFactory/SetRuntime/SetLogger`

- [ ] **Step 1: 在 `base_node.go` 定义基类**

```go
package graph

import (
    "context"
    "github.com/blockmemory/agent/backend/internal/logger"
    "github.com/blockmemory/agent/backend/internal/model"
)

type BaseAgentNode struct {
    registry     *RoleRegistry
    factory      *RoleFactory
    modelFactory *model.ModelFactory
    progress     ProgressCallback
    logger       *logger.Logger
    llmTracker   *model.LLMCallTracker
    runtime      *runtime.Runtime
    toolCallback ToolCallback
}

func (b *BaseAgentNode) SetRegistry(r *RoleRegistry)        { b.registry = r }
func (b *BaseAgentNode) SetFactory(f *RoleFactory)          { b.factory = f }
func (b *BaseAgentNode) SetModelFactory(mf *model.ModelFactory) { b.modelFactory = mf }
func (b *BaseAgentNode) SetProgressCallback(pc ProgressCallback) { b.progress = pc }
func (b *BaseAgentNode) SetLogger(l *logger.Logger) {
    b.logger = l
    if l != nil {
        b.llmTracker = model.NewLLMCallTracker(func(agent, model, prompt, response string, inTokens, outTokens, latencyMs int, extra ...slog.Attr) {
            l.LLMCall(context.Background(), agent, model, prompt, response, inTokens, outTokens, latencyMs, extra...)
        })
    }
}
func (b *BaseAgentNode) SetRuntime(rt *runtime.Runtime) { b.runtime = rt }
func (b *BaseAgentNode) SetToolCallback(tc ToolCallback) { b.toolCallback = tc }

func (b *BaseAgentNode) Emit(inst *types.AgentInstance, evt types.Event) { ... }
func (b *BaseAgentNode) EmitDetail(inst *types.AgentInstance, kind, detail string) { ... }
func (b *BaseAgentNode) SessionLogger(ctx context.Context, sessionID, agentName, phase string) context.Context { ... }
```

- [ ] **Step 2: 让 `MetaAgentNode` / `DomainAgentNode` / `SubDomainAgentNode` 嵌入 `BaseAgentNode`**

删除各自重复的 `Set*` / `emit` / `emitDetail` / `sessionLogger` 字段与方法，改为：

```go
type MetaAgentNode struct {
    BaseAgentNode
    // Meta 专有字段
}
```

- [ ] **Step 3: 运行 graph 包测试**

```bash
cd D:/data/project/BlockMemoryAgent
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add backend/internal/graph/base_node.go backend/internal/graph/meta_agent.go backend/internal/graph/domain_agent.go backend/internal/graph/subdomain_agent.go
git commit -m "refactor(graph): extract BaseAgentNode to unify Meta/Domain/SubDomain common behavior"
```

---

### Task 1.2: 统一 LLM 调用层

**Files:**
- Create: `backend/internal/graph/llm_caller.go`
- Modify: `backend/internal/graph/meta_llm.go`, `domain_llm.go`, `subdomain_llm.go`
- Test: `backend/internal/graph/llm_tools_test.go`, `meta_watchdog_test.go`

> **行为保真要求**：三层现有 `callLLMAs` / `callLightweightAs` 的超时配置**不对称**，必须逐字迁移：
> - `meta_llm.go` `callLLMAs` + `callLightweightAs`：soft=30s / hard=90s，均有 `AgentCfg` override
> - `domain_llm.go` `callLLMAs`：读 `AgentCfg.LLMSoftTimeoutSec/LLMHardTimeoutSec`（配置驱动）
> - `domain_llm.go` `callLightweightAs`：**硬编码 soft=15s / hard=25s**，无 AgentCfg override（`domain_llm.go:36-37`）
> - `subdomain_llm.go`：核对后补充
>
> 统一 `CallLLM` 时，`Lightweight=true` 的 domain 调用必须保持 15s/25s，不能套用 meta 的 30s/90s，否则 `analyzeTasks` 快速失败路径丢失。soul 注入条件、emitDetail 事件 kind（`prompt` / `token_usage` / `llm_response`）、llmTracker 调用顺序必须保持原序。任何超时值偏差即行为变化。

**Interfaces:**
- Consumes: `BaseAgentNode`
- Produces: `LLMCallOptions`, `func (b *BaseAgentNode) CallLLM(ctx, prompt string, opts LLMCallOptions) (string, error)`

- [ ] **Step 0: 盘点三层现有超时与 soul 注入配置**

```bash
# 列出所有超时常量
grep -n "SoftTimeout\|HardTimeout\|30\*\|90\*\|15\*\|25\*\|time\.Second" backend/internal/graph/meta_llm.go backend/internal/graph/domain_llm.go backend/internal/graph/subdomain_llm.go
# 列出 soul 注入条件
grep -n "InjectSoul\|soul\." backend/internal/graph/meta_llm.go backend/internal/graph/domain_llm.go backend/internal/graph/subdomain_llm.go
# 确认 domain_llm.go:36-37 的 15s/25s 是否仍存在（RISK_7/8）
grep -n "15\|25" backend/internal/graph/domain_llm.go
```

输出写入 `docs/superpowers/notes/llm-caller-baseline.md`，作为 Step 1 默认值依据。**特别注意 domain_llm.go callLightweightAs 的 15s/25s 与 meta 30s/90s 不同**。

- [ ] **Step 1: 定义 `LLMCallOptions` 与统一调用方法**

```go
package graph

import "time"

type LLMCallOptions struct {
    Caller       string
    Lightweight  bool        // 是否使用 lightweight 模型
    InjectSoul   bool
    Temperature  *float64
    SoftTimeout  time.Duration
    HardTimeout  time.Duration
}

func (b *BaseAgentNode) CallLLM(ctx context.Context, prompt string, opts LLMCallOptions) (string, error) {
    // 1. 取模型（根据 Lightweight）
    // 2. 注入 soul（若 InjectSoul）
    // 3. emitDetail prompt
    // 4. 解析超时（使用 opts.SoftTimeout / opts.HardTimeout，未设置则用 Step 0 盘点的默认值）
    // 5. llmTracker.CallWithTimeout
    // 6. emitDetail token_usage / llm_response
    // 返回 content, err
}
```

- [ ] **Step 2: 替换三层 `callLLMAs` / `callLightweightAs` 调用，逐个核对 Step 0 盘点值**

例如 `meta_llm.go` 中：

```go
// 替换前
content, err := n.callLLMAs(ctx, prompt)
// 替换后（SoftTimeout/HardTimeout 必须匹配 Step 0 盘点值）
content, err := n.CallLLM(ctx, prompt, LLMCallOptions{Caller: "MetaAgent", InjectSoul: true, SoftTimeout: 30*time.Second, HardTimeout: 90*time.Second})
```

- [ ] **Step 3: 删除旧的 `callLLMAs` / `callLightweightAs` 私有方法**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/llm_caller.go backend/internal/graph/meta_llm.go backend/internal/graph/domain_llm.go backend/internal/graph/subdomain_llm.go
git commit -m "refactor(graph): unify LLM calling via BaseAgentNode.CallLLM"
```

---

### Task 1.3: 拆分 `DomainAgentNode.Invoke` 为四阶段方法

**Files:**
- Modify: `backend/internal/graph/domain_agent.go`
- Test: `backend/internal/graph/domain_tasks_test.go`, `agent_common_test.go`

- [ ] **Step 1: 将 `Invoke` 拆分为私有阶段方法**

```go
func (n *DomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState, inst *types.AgentInstance) (*types.ThreeLayerState, error) {
    if err := n.prepareContext(ctx, state, inst); err != nil {
        return nil, err
    }
    tasks, err := n.analyzeAndPlan(ctx, state, inst)
    if err != nil {
        return nil, err
    }
    results, err := n.dispatchAndCollect(ctx, state, inst, tasks)
    if err != nil {
        return nil, err
    }
    return n.finalizeBlock(ctx, state, inst, results)
}
```

- [ ] **Step 2: 把原 220+ 行代码按职责填入四个方法，每个方法 30-60 行**

- [ ] **Step 3: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/domain_agent.go
git commit -m "refactor(graph): split DomainAgentNode.Invoke into prepare/analyze/dispatch/finalize phases"
```

---

### Task 1.4: 工具策略 Guard 化（WriteGuard / CommandGuard）

**Files:**
- Create: `backend/internal/graph/guards.go`, `guard_registry.go`
- Modify: `backend/internal/graph/tool_files.go`, `tool_command.go`, `tool_executor.go`
- Test: `backend/internal/graph/tool_files_test.go`, `tool_command_test.go`, `tool_sandbox_test.go`

**Interfaces:**
- Produces:
  ```go
  type WriteGuard interface { Check(path, content string) error }
  type CommandGuard interface { Check(cmd string) (blocked bool, reason string) }
  type GuardRegistry struct { writeGuards []WriteGuard; commandGuards []CommandGuard }
  ```

- [ ] **Step 1: 定义 Guard 接口与注册表**

```go
package graph

type WriteGuard interface { Name() string; Check(path, content string) error }
type CommandGuard interface { Name() string; Check(cmd string) (blocked bool, reason string) }

type GuardRegistry struct {
    writeGuards   []WriteGuard
    commandGuards []CommandGuard
}

func (g *GuardRegistry) RegisterWriteGuard(wg WriteGuard) { g.writeGuards = append(g.writeGuards, wg) }
func (g *GuardRegistry) RegisterCommandGuard(cg CommandGuard) { g.commandGuards = append(g.commandGuards, cg) }
func (g *GuardRegistry) CheckWrite(path, content string) error { ... }
func (g *GuardRegistry) CheckCommand(cmd string) (bool, string) { ... }
```

- [ ] **Step 2: 把 `tool_files.go` 中业务规则提取为 Guard 实现**

```go
type protectedPathGuard struct{ protected []string }
func (g *protectedPathGuard) Name() string { return "protected-path" }
func (g *protectedPathGuard) Check(path, content string) error { ... }

type mailboxFileGuard struct{}
func (g *mailboxFileGuard) Name() string { return "mailbox-file" }
func (g *mailboxFileGuard) Check(path, content string) error { ... }

// 类似地：temporaryScriptGuard, mailboxGoProgramGuard, pathWhitespaceGuard
```

- [ ] **Step 3: 把 `tool_command.go` 中业务规则提取为 Guard 实现**

```go
type longRunningServerGuard struct{}
type portConflictGuard struct{}
```

- [ ] **Step 4: 在 `ToolExecutor` 构造时注入 `GuardRegistry` 并替换特判**

```go
type ToolExecutor struct {
    // ...
    guards *GuardRegistry
}
```

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/guards.go backend/internal/graph/guard_registry.go backend/internal/graph/tool_files.go backend/internal/graph/tool_command.go backend/internal/graph/tool_executor.go
git commit -m "refactor(graph): decouple tool business rules into WriteGuard/CommandGuard registry"
```

---

### Task 1.5: 工具注册表重构

**Files:**
- Create: `backend/internal/graph/tool_registry.go`
- Modify: `backend/internal/graph/tool_executor.go`, `tool_git.go`, `tool_http.go`, `tool_files.go`, `tool_command.go`, `tool_sandbox.go`
- Test: `backend/internal/graph/tool_executor.go` 相关测试

**Interfaces:**
- Produces:
  ```go
  type Tool interface {
      Name() string
      Aliases() []string
      Execute(ctx context.Context, args map[string]any) *ToolResult
  }
  type ToolRegistry struct { tools map[string]Tool; aliases map[string]string }
  ```

- [ ] **Step 1: 定义 `Tool` 接口与 `ToolRegistry`**

```go
package graph

import "context"

type ToolResult struct {
    Output string
    Error  string
}

type Tool interface {
    Name() string
    Aliases() []string
    Execute(ctx context.Context, args map[string]any) *ToolResult
}

type ToolRegistry struct {
    tools   map[string]Tool
    aliases map[string]string
}

func NewToolRegistry() *ToolRegistry { ... }
func (r *ToolRegistry) Register(t Tool) { ... }
func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) (*ToolResult, error) { ... }
```

- [ ] **Step 2: 让每个具体工具实现 `Tool` 接口**

例如：

```go
type readFileTool struct{ exec *ToolExecutor }
func (t *readFileTool) Name() string { return "ReadFile" }
func (t *readFileTool) Aliases() []string { return []string{"read_file", "readFile"} }
func (t *readFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult { ... }
```

- [ ] **Step 3: 在 `ToolExecutor` 中替换 switch 分发**

```go
func (e *ToolExecutor) Execute(ctx context.Context, name string, args map[string]any) *ToolResult {
    return e.registry.Execute(ctx, name, args)
}
```

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/tool_registry.go backend/internal/graph/tool_executor.go backend/internal/graph/tool_*.go
git commit -m "refactor(graph): introduce ToolRegistry to replace giant switch dispatch"
```

---

### Task 1.6: 封装隔离 SubDomainAgent 代码（不删除）

**Files:**
- Create: `backend/internal/graph/subdomain_package.go`（可选门面）
- Modify: `backend/internal/graph/subdomain_agent.go`, `subdomain_llm.go`, `subdomain_tasks.go`, `subdomain_utils.go`, `domain_subdomain.go`, `graph_routes.go`, `graph_resolve.go`, `three_layer_graph.go`
- Test: `backend/internal/graph/self_test_test.go`, `runtime_wiring_test.go`

- [ ] **Step 1: 在 SubDomain 相关文件头部添加清晰注释，说明当前状态**

```go
// SubDomainAgent 当前处于预留/未启用状态（shouldSplitToSubDomains 默认返回 false）。
// 本文件仅做技术封装，不改动业务逻辑，未来可通过 FeatureToggle 重新启用。
```

- [ ] **Step 2: 将 SubDomain 相关公共函数/结构体收敛到最小公开接口**

例如把 `subdomain_utils.go` 中仅内部使用的方法改为未导出，减少外部依赖点。

- [ ] **Step 3: 在 `three_layer_graph.go` 的依赖注入中，将 SubDomain 节点统一通过接收者接口处理（见 Task 1.7），不再单独写特殊分支**

- [ ] **Step 4: 保持 `types.RoleTypeSubDomain` 常量与存储层兼容，不删除任何类型或路由常量**

- [ ] **Step 5: 运行全量 graph 测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/subdomain_*.go backend/internal/graph/domain_subdomain.go backend/internal/graph/graph_routes.go backend/internal/graph/graph_resolve.go backend/internal/graph/three_layer_graph.go
git commit -m "refactor(graph): encapsulate SubDomainAgent code without removing future capability"
```

---

### Task 1.7: 统一依赖注入接口

**Files:**
- Create: `backend/internal/graph/receivers.go`
- Modify: `backend/internal/graph/three_layer_graph.go`
- Test: `backend/internal/graph/runtime_wiring_test.go`

- [ ] **Step 1: 定义接收者接口**

```go
package graph

type ModelFactoryReceiver interface { SetModelFactory(*model.ModelFactory) }
type ProgressReceiver interface { SetProgressCallback(ProgressCallback) }
type LoggerReceiver interface { SetLogger(*logger.Logger) }
type RuntimeReceiver interface { SetRuntime(*runtime.Runtime) }
// ...
```

- [ ] **Step 2: 在 `ThreeLayerGraph` 中用泛型辅助统一注入**

```go
func propagate[R any](nodes []ThreeLayerNode, fn func(R)) {
    for _, node := range nodes {
        if r, ok := node.(R); ok {
            fn(r)
        }
    }
}
```

- [ ] **Step 3: 替换 `three_layer_graph.go` 中重复的 switch 注入**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/graph/... -count=1
git add backend/internal/graph/receivers.go backend/internal/graph/three_layer_graph.go
git commit -m "refactor(graph): unify dependency injection via receiver interfaces"
```

---

### Task 1.8: 统一 JSON 提取与文本截断工具（含行为保真风险）

**Files:**
- Create: `backend/pkg/jsonutil/extract.go`, `backend/pkg/textutil/truncate.go`
- Modify: `backend/internal/graph/role_factory.go`, `agent_common.go`, `meta_domain.go`, `plan.go`, `domain_agent.go`, `meta_utils.go`, `meta_archive.go`
- Test: 新增 `backend/pkg/jsonutil/extract_test.go`, `backend/pkg/textutil/truncate_test.go`

> **行为保真风险**：JSON 提取有 **4 个 callsite**，行为各异，必须逐点核对选项：
> - `role_factory.go:408` `extractJSON`：剥离 ``` 围栏 + 去注释 + 修尾逗号 + 单引号转双引号 + 处理 array OR object（全套）
> - `agent_common.go:446` `extractJSONBlock`：仅剥离 ``` + 首个 `{` 到末尾 `}`（最小集）
> - `meta_domain.go:95`：调用 `extractJSON`（全套，**原 plan Files 漏列**）
> - `plan.go:52,105`：调用 `extractJSON`（全套，**原 plan Step 3 漏列**）
>
> 统一为 `jsonutil.ExtractJSON` 时，选项必须按 callsite 对应：
> - `role_factory.go` / `meta_domain.go` / `plan.go`：`ExtractOptions{StripComments:true, FixSingleQuotes:true, FixTrailingCommas:true, AllowArray:true}`
> - `agent_common.go`：`ExtractOptions{}`（空选项，保持原 `extractJSONBlock` 行为）
>
> **不得统一开启全部选项**，否则 `agent_common.go` 调用点处理范围扩大，LLM 响应解析结果变化。

- [ ] **Step 1: 实现 `jsonutil.ExtractJSON`，选项可独立开关（含 `FixTrailingCommas` / `AllowArray`）**

```go
package jsonutil

type ExtractOptions struct {
    StripComments      bool
    FixSingleQuotes    bool
    FixTrailingCommas  bool
    AllowArray         bool
}

func ExtractJSON(input string, opts ExtractOptions) string { ... }
```

- [ ] **Step 2: 实现 `textutil.TruncateRunes` / `TruncateBytes`**

```go
package textutil

func TruncateRunes(s string, n int, suffix string) string { ... }
func TruncateBytes(s string, n int, suffix string) string { ... }
```

- [ ] **Step 3: 替换 4 个 callsite，逐个核对选项**

```go
// role_factory.go / meta_domain.go / plan.go
jsonutil.ExtractJSON(s, jsonutil.ExtractOptions{StripComments:true, FixSingleQuotes:true, FixTrailingCommas:true, AllowArray:true})
// agent_common.go
jsonutil.ExtractJSON(s, jsonutil.ExtractOptions{})
```

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/pkg/jsonutil/... ./backend/pkg/textutil/... ./backend/internal/graph/... -count=1
git add backend/pkg/jsonutil backend/pkg/textutil backend/internal/graph/role_factory.go backend/internal/graph/agent_common.go backend/internal/graph/meta_domain.go backend/internal/graph/plan.go
git commit -m "refactor(graph): unify JSON extraction and text truncation utilities"
```

---

### Task 1.9: 将 Magic Number / 阈值集中到 `runtime.AgentCfg`

**Files:**
- Modify: `backend/internal/runtime/runtime.go`, `backend/internal/config/config.go`, `backend/internal/graph/tool_files.go`, `tool_command.go`, `blades_agent_runner.go`, `domain_agent.go`, `agent_common.go`, `meta_archive.go`
- Test: `backend/internal/config/config_test.go`, `backend/internal/graph/router_test.go`

- [ ] **Step 1: 在 `runtime.AgentCfg`（或拆分后的子配置）中补充字段**

```go
type AgentConfig struct {
    // ... 已有字段
    ReadFileMaxChars        int
    RunCommandMaxOutput     int
    ContextExplodeHardLimit int
    ContextExplodeSoftLimit int
    ReadFilePerTaskLimit    int
    SummaryBaseLimit        int
    SummaryExtendedLimit    int
    LoopDetectorWindowSize  int
    LoopDetectorMaxRepeat   int
    LoopDetectorMaxEmpty    int
}
```

- [ ] **Step 2: 在 `config.DefaultAgentCfg` 中给出默认值，必须逐字匹配当前硬编码值**

默认值清单（经代码核对，执行时再 grep 确认）：

| 字段 | 默认值 | 来源 | 备注 |
|---|---|---|---|
| `ReadFileMaxChars` | 4000 | `tool_files.go:47` | 同时核对 `tool_files.go:504`（SearchInFiles）是否同值，若不同需拆字段 |
| `RunCommandMaxOutput` | 10000 | `tool_command.go:141` | |
| `RunCommandTimeoutSec` | 60 | `tool_command.go:94` (60s) | |
| `ToolExecMaxBytes` | 300 | `tool_executor.go:206` | |
| `LLMPromptMaxChars` | 500 | `meta_llm.go:35,74,110,152` + `domain_llm.go:32,61,91,122` | **8 处重复**，全部需替换，漏一处即不一致 |
| `LLMTrackerSlowModeThreshold` | 3 | `llm_tracker.go:248` | **与 Task 4.10 冲突**：4.10 修改语义（cancel 不再计入），本 Task 先集中化旧值，4.10 再改语义。两 Task 顺序不可颠倒。 |
| `LoggerRetryCount` | 3 | `logger.go:161` | |
| `ContextExplodeSoftLimit` | 50000 | `blades_agent_runner.go:377` (`softWarnThreshold`) | **非** `agent_common.go:348` 的 100（那是 summary 截断长度，单独字段） |
| `ContextExplodeHardLimit` | 80000 | `blades_agent_runner.go:376` (`maxInputTokensBudget`) | |
| `SummaryTruncateChars` | 100 | `agent_common.go:348` | summary 截断长度，与 ContextExplodeSoftLimit 不同字段 |
| `SearchBlockMemoryMaxTokens` | 800 | `block_vector.go:104` (`maxTokens`) | **非**向量维度。向量维度在 `cfg.PgVector.Dimensions`（默认 768），属配置项非 magic number |
| `VectorDim` | 768 | `config.yaml` + `block_vector_test.go:40,60,86` | 已在配置，本 Task 确认未硬编码 |
| `ReadFilePerTaskLimit` | （grep `tool_files.go` 任务级限制） | | 执行时核对 |
| `SummaryBaseLimit` / `SummaryExtendedLimit` | （grep `meta_archive.go`） | | 执行时核对 |
| `LoopDetectorWindowSize` / `MaxRepeat` / `MaxEmpty` | （grep 循环检测器） | | 执行时核对 |

> **强制核对**：执行此 Step 前，对每个字段 `grep -n "<硬编码值>" backend/internal/` 确认当前值与所有出现位置，写入上表。`500`/`4000` 等多出现值必须全部替换。默认值偏差即行为变化。
>
> **字段重命名注意**：原 plan 误标 `BlockVectorDim=800`（实为 maxTokens）与 `ContextExplodeSoftLimit=100`（实为 summary 截断）。本表已修正，执行时以代码 grep 为准，勿照搬旧表。

- [ ] **Step 3: 替换各文件中的 magic number 为 `rt.AgentCfg.Xxx`，逐项核对 Step 2 清单**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/config/... ./backend/internal/graph/... ./backend/internal/runtime/... -count=1
git add backend/internal/config/config.go backend/internal/runtime/runtime.go backend/internal/graph/tool_*.go backend/internal/graph/blades_agent_runner.go backend/internal/graph/domain_agent.go backend/internal/graph/agent_common.go backend/internal/graph/meta_archive.go
git commit -m "refactor(graph): centralize thresholds into AgentConfig defaults"
```

---

## Track 2: Memory & Storage 层解耦

**目标：** 拆分 `PostgresStore` / `RedisStore` 神文件，解除 `memory→graph`、`store→dag` 的倒置依赖，将记忆管线中的业务策略（分段比例、向量模型、prompt 模板）配置化。

### Task 2.1: 解除 `memory` 对 `graph` 的依赖

**Files:**
- Create: `backend/internal/memory/types.go`, `backend/pkg/types/context.go`（若不存在则下移）
- Modify: `backend/internal/memory/assembler.go`, `backend/internal/graph/util.go` 或 `backend/pkg/types`
- Test: `backend/internal/memory/assembler_test.go`, `backend/internal/graph/agent_common_test.go`

> **现状澄清（RISK_27, RISK_28）**：
> - `Episode` **已在** `pkg/types/memory.go:11`，全代码用 `types.Episode`，**无 `memory.Episode`**。原 plan "memory.Episode 改为类型别名"是 no-op，删除该步骤。
> - `graph/util.go:102` `Message.Role` 类型是 `enums.ChatRole`（**非 string**）。下移到 `pkg/types.Message` 时 `Role` 字段类型必须用 `enums.ChatRole`，否则类型断裂。
> - `mailbox/mailbox.go:50` 有第三个 `Message` 类型。若 `memory` / `graph` 同时 import `mailbox` 与 `pkg/types`，同 scope 内 `Message` 冲突，需用别名区分。

**Interfaces:**
- Consumes: `graph.BuildRequest`, `graph.ContextPack`, `graph.Message`
- Produces: `pkg/types.ContextPack`, `pkg/types.Message`（`Role` 用 `enums.ChatRole`）

- [ ] **Step 0: 确认 `Episode` 已在 `pkg/types`，无需下移**

```bash
grep -n "type Episode struct" backend/pkg/types/memory.go
grep -rn "memory\.Episode" backend/  # 应无输出
```

- [ ] **Step 1: 在 `pkg/types` 定义 `ContextPack` / `Message` / `BuildRequest`，`Message.Role` 用 `enums.ChatRole`**

```go
package types

import "github.com/blockmemory/agent/backend/pkg/enums"

type Message struct {
    Role    enums.ChatRole  // RISK_28: 保持与 graph/util.go:102 一致，非 string
    Content string
}

type ContextPack struct {
    System        string
    TopicGlobal   string
    SharedState   string
    PrivateMemory string
    Task          string
    Messages      []Message
}

// Episode 已存在 pkg/types/memory.go，无需重复定义。

type BuildRequest struct {
    SessionID string
    TopicID   string
    AgentID   string
    Goal      string
    History   []*Episode
    // ...
}
```

> **循环依赖已规避**：`Episode` 已在 `pkg/types/memory.go:11`，`BuildRequest.History []*Episode` 直接复用，无新循环。原 plan "memory.Episode 改为别名"步骤删除（无 memory.Episode）。
>
> **Message 类型冲突规避（RISK_28）**：`pkg/types.Message`、`mailbox.Message`、`graph.Message` 三者并存。迁移时：
> - `memory/assembler.go` 删除 `graph.Message` 引用，改用 `types.Message`
> - 若同文件需 import `mailbox`，用别名 `mailmsg "github.com/.../mailbox"` 区分
> - `graph/util.go:102` `Message` 保留为 `types.Message` 别名（`type Message = types.Message`）或直接替换，保持调用点兼容

- [ ] **Step 2: 修改 `memory/assembler.go` 使用 `pkg/types` 类型**

删除 `import "github.com/blockmemory/agent/backend/internal/graph"`，改为 `pkg/types`。

- [ ] **Step 3: 修改 `graph` 中引用点，确保其仍使用同一份类型**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/memory/... ./backend/internal/graph/... -count=1
git add backend/pkg/types/context.go backend/internal/memory/types.go backend/internal/memory/assembler.go
git commit -m "refactor(memory): move ContextPack/Message to pkg/types to break memory→graph dependency"
```

---

### Task 2.2: `dag.Scheduler` 依赖注入化（`dag.JobStore` 接口）

**Files:**
- Create: `backend/internal/dag/store.go`（接口定义）
- Modify: `backend/internal/store/dag.go`
- Test: `backend/internal/store/...`, `backend/internal/dag/...`

> **范围澄清**：当前 `store/dag.go` 导入 `internal/dag` 以使用 `DAGJob` 类型，这是单向依赖（store -> dag），**不存在循环**。所谓"反转"实为依赖注入：让 `dag.Scheduler` 依赖 `dag.JobStore` 接口而非 `*store.PostgresStore` 具体类型，使 `dag` 包可在无 `store` 的情况下独立测试。`store -> dag` 的 import 方向**不变**（store 仍需 `dag.DAGJob` 类型实现接口方法）。若要彻底消除 store->dag import，需把 `DAGJob` 下移到 `pkg/types`，本 Task 不做。

**Interfaces:**
- Produces:
  ```go
  package dag
  type JobStore interface {
      Save(ctx context.Context, job *DAGJob) error
      Load(ctx context.Context, id string) (*DAGJob, error)
      List(ctx context.Context) ([]*DAGJob, error)
      Delete(ctx context.Context, id string) error
  }
  ```

- [ ] **Step 1: 在 `dag` 包定义 `JobStore` 接口**

```go
package dag

import "context"

type JobStore interface {
    Save(ctx context.Context, job *DAGJob) error
    Load(ctx context.Context, id string) (*DAGJob, error)
    List(ctx context.Context) ([]*DAGJob, error)
    Delete(ctx context.Context, id string) error
}
```

- [ ] **Step 2: 确认 `store.PostgresStore` 现有方法签名满足 `dag.JobStore` 接口（隐式实现）**

若签名不匹配，调整 `store/dag.go` 方法名/参数，保持行为不变。

- [ ] **Step 3: `dag.Scheduler` 构造函数改为接收 `dag.JobStore` 接口而非具体类型**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/dag/... ./backend/internal/store/... -count=1
git add backend/internal/dag/store.go backend/internal/store/dag.go
git commit -m "refactor(dag): inject JobStore interface into Scheduler for testability"
```

---

### Task 2.3: 拆分 `PostgresStore` 为按域子 store

**Files:**
- Create: `backend/internal/store/postgres_store.go`（组合器）、`episode_store.go`、`snapshot_store.go`、`knowledge_store.go`、`topic_store.go`、`agent_registry_store.go`、`session_store.go`、`schema.go`
- Delete/Move: 将 `backend/internal/store/postgres.go` 内容拆分到上述文件
- Test: `backend/internal/store/...`, `backend/internal/memory/...`, `test/api/...`

**Interfaces:**
- Produces:
  ```go
  type PostgresStore struct {
      db *sql.DB
      Episode      *EpisodeStore
      Snapshot     *SnapshotStore
      Knowledge    *KnowledgeStore
      Topic        *TopicStore
      AgentRegistry *AgentRegistryStore
      Session      *SessionStore
  }
  ```

- [ ] **Step 0: 调用面盘点（前置）**

拆分前先盘点 `store.PostgresStore` 所有公开方法及调用点，确认测试覆盖：

```bash
cd D:/data/project/BlockMemoryAgent
# 列出所有公开方法
grep -n "^func (s \*PostgresStore)" backend/internal/store/postgres.go
# 盘点调用点
grep -rn "store\.\(SaveEpisode\|GetEpisodes\|SearchEpisodes\|SaveSnapshot\|LoadSnapshot\|SaveKnowledge\|RegisterAgent\|ListAgents\|SaveSessionHistory\|SaveSessionEvents\)" backend/ test/
```

输出写入 `docs/superpowers/notes/store-callers.md`。若某方法无测试覆盖，先补测试再拆分，避免静默破坏。

- [ ] **Step 1: 创建 `postgres_store.go` 组合器**

```go
package store

import "database/sql"

type PostgresStore struct {
    db *sql.DB
    Episode       *EpisodeStore
    Snapshot      *SnapshotStore
    Knowledge     *KnowledgeStore
    Topic         *TopicStore
    AgentRegistry *AgentRegistryStore
    Session       *SessionStore
}

func NewPostgresStore(db *sql.DB, cfg config.PostgresConfig) *PostgresStore {
    return &PostgresStore{
        db: db,
        Episode:       &EpisodeStore{db: db},
        Snapshot:      &SnapshotStore{db: db},
        Knowledge:     &KnowledgeStore{db: db},
        Topic:         &TopicStore{db: db},
        AgentRegistry: &AgentRegistryStore{db: db},
        Session:       &SessionStore{db: db},
    }
}
```

- [ ] **Step 2: 按表拆分方法到各子 store**

例如 `episode_store.go` 包含 `SaveEpisode`, `GetEpisodes`, `SearchEpisodes` 等。

- [ ] **Step 3: 更新所有调用点从 `store.SaveEpisode(...)` 到 `store.Episode.Save(...)`，按 Step 0 盘点清单逐项核对**

- [ ] **Step 4: 运行全量测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1
git add backend/internal/store/*.go
git commit -m "refactor(store): split PostgresStore monolith into domain-specific stores"
```

---

### Task 2.4: 拆分 `RedisStore`

**Files:**
- Create: `backend/internal/store/redis_topic.go`, `redis_output.go`, `redis_event.go`, `redis_snapshot.go`, `redis_ttl.go`
- Modify: `backend/internal/store/redis.go`（保留组合器）
- Test: `backend/internal/store/...`, `backend/internal/memory/...`

- [ ] **Step 1: 在 `redis.go` 中保留 `RedisStore` 组合器**

```go
type RedisStore struct {
    rdb *redis.Client
    Topic   *TopicRedisStore
    Output  *OutputRedisStore
    Event   *EventRedisStore
    Snapshot *SnapshotRedisStore
}
```

- [ ] **Step 2: 将各 Redis 操作方法迁移到对应子文件**

- [ ] **Step 3: 更新调用点**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... -count=1
git add backend/internal/store/redis*.go
git commit -m "refactor(store): split RedisStore into domain-specific redis stores"
```

---

### Task 2.5: 统一 Embedding 调用

**Files:**
- Modify: `backend/internal/memory/block_vector.go`, `backend/internal/store/postgres.go`（拆分后的 `knowledge_store.go`）
- Modify: `backend/internal/testserver/testserver.go`（注入 embedder）
- Modify: `backend/cmd/tui/main.go`（**RISK_22**：`cmd/tui/main.go:322` 调用 `ToKnowledgeRecord(a.dim)`，签名变更必须同步）
- Test: `backend/internal/memory/block_vector_test.go`（**RISK_23**：3 处 `ToKnowledgeRecord(768)` 调用 + 断言需更新）, `test/api/memory_test.go`

> **调用点盘点**：`ToKnowledgeRecord` 当前签名 `(dim int) *types.KnowledgeRecord`（无 error）。本 Task 改为 `(embedder embed.Embedder, dim int) (*types.KnowledgeRecord, error)`。所有调用点必须同步：
> - `cmd/tui/main.go:322`（原 plan 漏列）
> - `block_vector_test.go:40,60,86`（3 处测试，断言需加 error 处理）
> - 生产路径调用点（grep 确认）
>
> 签名变更 + 新增 error 返回属于**编译期行为变更**，调用方必须处理 error，否则静默吞错。

**Interfaces:**
- Consumes: `embed.Embedder`

- [ ] **Step 0: 盘点所有 `ToKnowledgeRecord` 调用点**

```bash
grep -rn "ToKnowledgeRecord" backend/ test/
```

- [ ] **Step 1: 在 `BlockMemoryRecord.ToKnowledgeRecord` 中注入 `embed.Embedder`，返回 error**

```go
func (r *BlockMemoryRecord) ToKnowledgeRecord(ctx context.Context, embedder embed.Embedder, dim int) (*types.KnowledgeRecord, error) {
    vec, err := embedder.Embed(ctx, r.Content)
    if err != nil { return nil, fmt.Errorf("embed content: %w", err) }
    // ...
}
```

- [ ] **Step 2: 在 `ContextAssembler` / `WriteProcessor` 构造时保存 embedder 并传递，更新所有 Step 0 盘点调用点**

- [ ] **Step 3: 更新 `block_vector_test.go` 断言处理 error**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/memory/... ./backend/internal/store/... ./backend/cmd/tui/... ./test/api/... -count=1
git add backend/internal/memory/block_vector.go backend/internal/store/knowledge_store.go backend/internal/testserver/testserver.go backend/cmd/tui/main.go backend/internal/memory/block_vector_test.go
git commit -m "refactor(memory): inject embed.Embedder for all vector generation"
```

---

### Task 2.6: 配置化记忆管线策略

**Files:**
- Modify: `backend/internal/config/config.go`, `config/config.yaml`
- Modify: `backend/internal/memory/assembler.go`, `memory/write.go`, `memory/compress.go`, `memory/search.go`, `memory/snapshot.go`
- Test: `backend/internal/memory/assembler_test.go`, `config_test.go`

- [ ] **Step 1: 在 `config.AgentConfig` 中新增记忆策略字段**

```go
type MemoryPolicyConfig struct {
    ContextWindow        int
    SystemSegmentRatio   int
    TopicGlobalRatio     int
    SharedStateRatio     int
    PrivateMemoryRatio   int
    TaskRatio            int
    DefaultTokensPerItem int
    SnapshotSummaryCount int
    SnapshotIssueThreshold float64
    VectorDim            int
    VectorIndexType      string // ivfflat / hnsw
}
```

- [ ] **Step 2: 在 `config.yaml` 中提供默认值**

- [ ] **Step 3: 替换 `ContextAssembler` 中硬编码比例与 `avgTokens=200`**

引入 `TokenEstimator` 接口（见 Track 3.3）作为后续增强；当前先用配置字段替换 magic number。

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/memory/... ./backend/internal/config/... -count=1
git add backend/internal/config/config.go config/config.yaml backend/internal/memory/assembler.go backend/internal/memory/write.go backend/internal/memory/compress.go backend/internal/memory/search.go backend/internal/memory/snapshot.go
git commit -m "refactor(memory): externalize memory pipeline thresholds to config"
```

---

### Task 2.7: 修复搜索与存储异常处理（行为修复）

**Files:**
- Modify: `backend/internal/memory/search.go`, `backend/internal/store/postgres.go`（拆分后）
- Test: `backend/internal/memory/search_test.go`, `backend/internal/store/...`

> **行为修复声明**：本 Task 修复 4 处错误处理缺陷：(1) `SearchAndScore` embedding 失败静默置 nil 改为返回 error；(2) `cosineSimilarity` 未 clamp 到 [0,1]；(3) `IsDuplicateError` 用类型断言而非 `errors.As`，包装错误无法识别；(4) `SaveKnowledge` 忽略 JSON 序列化错误。修复 (1) 会改变搜索失败时的返回行为（原返回空结果，改为返回 error），调用方需处理。先写回归测试固化旧行为，再修复。

- [ ] **Step 1: `SearchAndScore` embedding 失败时返回错误**

```go
queryEmbedding, err := e.Embed(ctx, query)
if err != nil { return nil, fmt.Errorf("embed query: %w", err) }
```

- [ ] **Step 2: 修复 `cosineSimilarity` 返回值到 `[0,1]`**

```go
func cosineSimilarity(a, b []float32) float64 {
    // ...
    cos := dot / (normA * normB)
    if cos < 0 { return 0 }
    if cos > 1 { return 1 }
    return cos
}
```

- [ ] **Step 3: 修复 `IsDuplicateError` 使用 `errors.As`**

```go
func IsDuplicateError(err error) bool {
    var pgErr *pq.Error
    if errors.As(err, &pgErr) {
        return pgErr.Code == "23505"
    }
    return false
}
```

- [ ] **Step 4: 对 `SaveKnowledge` 等忽略错误的点返回错误**

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/memory/... ./backend/internal/store/... -count=1
git add backend/internal/memory/search.go backend/internal/store/*.go
git commit -m "fix(memory,store): propagate embedding/duplicate/json errors correctly"
```

---

## Track 3: Runtime & 跨组件治理

**目标：** 让 `Runtime` 可配置、可测试；拆分臃肿的 `config.AgentConfig`；将业务策略（skill 选择、soul 温度、watchdog token 估算）抽象为策略接口；治理 logger goroutine 风暴与 dag scheduler 生命周期。

### Task 3.1: 拆分 `config.AgentConfig` 为子配置结构

**Files:**
- Modify: `backend/internal/config/config.go`, `config/config.yaml`
- Modify: 所有引用 `AgentConfig` 字段的文件（graph/runtime/memory/watchdog/skill）
- Test: `backend/internal/config/config_test.go`, 全量测试

**Interfaces:**
- Produces:
  ```go
  type AgentConfig struct {
      GraphPolicyConfig   `yaml:",inline"`
      LLMRuntimeConfig    `yaml:",inline"`
      MemoryPolicyConfig  `yaml:",inline"`
      SafetyConfig        `yaml:",inline"`
      FeatureTogglesConfig `yaml:",inline"`
  }
  ```

- [ ] **Step 1: 定义子配置结构**

```go
package config

type GraphPolicyConfig struct {
    StallSteps            int    `yaml:"stall_steps"`
    MaxRepeatFingerprint  int    `yaml:"max_repeat_fingerprint"`
    MaxSteps              int    `yaml:"max_steps"`
    SessionTimeoutMin     int    `yaml:"session_timeout_min"`
}

type LLMRuntimeConfig struct {
    ToolCallMaxRounds int           `yaml:"tool_call_max_rounds"`
    RetryCount        int           `yaml:"retry_count"`
    RetryBackoff      time.Duration `yaml:"retry_backoff"`
    SoftTimeout       time.Duration `yaml:"soft_timeout"`
    HardTimeout       time.Duration `yaml:"hard_timeout"`
}

type SafetyConfig struct {
    ToolSandboxPath        string   `yaml:"tool_sandbox_path"`
    ToolSandboxAllowlist   []string `yaml:"tool_sandbox_allowlist"`
}

type FeatureTogglesConfig struct {
    InterruptEnabled bool `yaml:"interrupt_enabled"`
    QueueEnabled     bool `yaml:"queue_enabled"`
    HumanEnabled     bool `yaml:"human_enabled"`
    DAGEnabled       bool `yaml:"dag_enabled"`
    PlanEnabled      bool `yaml:"plan_enabled"`
    ReflectionEnabled bool `yaml:"reflection_enabled"`
    SelfTestEnabled  bool `yaml:"self_test_enabled"`
}
```

- [ ] **Step 2: 让 `AgentConfig` 嵌入上述结构体，保持 YAML 路径不变**

- [ ] **Step 3: 拆分 `applyDefaults` 为多个小函数**

```go
func (c *Config) applyDefaults() {
    c.applyPostgresDefaults()
    c.applyRedisDefaults()
    c.applyAgentDefaults()
}
```

- [ ] **Step 4: 更新所有引用点从 `cfg.StallSteps` 到 `cfg.GraphPolicyConfig.StallSteps`（或通过嵌入保持旧访问方式）**

保持嵌入后旧访问方式仍可用，但新代码按子结构访问。

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... -count=1
git add backend/internal/config/config.go config/config.yaml
git commit -m "refactor(config): split AgentConfig into focused sub-configs"
```

---

### Task 3.2: `runtime.New` 改为 Options 模式并返回 error（含 panic->error 行为变更）

**Files:**
- Modify: `backend/internal/runtime/runtime.go`, `backend/internal/testserver/testserver.go`, `backend/main.go`, `backend/cmd/tui/main.go`, `backend/cmd/demo/main.go`, `backend/cmd/memory-console/main.go`
- Test: `backend/internal/graph/runtime_wiring_test.go`, `test/api/...`

> **行为修复声明**：当前 `runtime/runtime.go:57` soul 加载失败直接 `panic`。本 Task 改为返回 `error`。属于行为变更：原 panic 路径（soul.md 损坏或缺失且非空路径）会使进程崩溃，修复后由调用方决定（通常是拒绝启动并打印错误）。CLAUDE.md 已说明 "missing file is non-fatal"（`soulPath` 为空时 `loader.Load` 应返回 nil loader 而非 error），需区分"空路径"与"加载失败"两种情况。

**Interfaces:**
- Produces:
  ```go
  type RuntimeOption func(*Runtime)
  func New(soulPath string, skillPool *skill.Pool, opts ...RuntimeOption) (*Runtime, error)
  ```

- [ ] **Step 1: 定义 Options**

```go
package runtime

import (
    "github.com/blockmemory/agent/backend/internal/board"
    "github.com/blockmemory/agent/backend/internal/cmdqueue"
    "github.com/blockmemory/agent/backend/internal/mailbox"
    "github.com/blockmemory/agent/backend/internal/skill"
    "github.com/blockmemory/agent/backend/internal/soul"
    "github.com/blockmemory/agent/backend/internal/watchdog"
)

type RuntimeOption func(*Runtime)

func WithBoards(bm *board.Manager) RuntimeOption      { return func(r *Runtime) { r.Boards = bm } }
func WithMailbox(mb *mailbox.Mailbox) RuntimeOption    { return func(r *Runtime) { r.Mailbox = mb } }
func WithWatchdog(w *watchdog.Watchdog) RuntimeOption  { return func(r *Runtime) { r.Watchdog = w } }
func WithCmdQueue(cq *cmdqueue.Manager) RuntimeOption  { return func(r *Runtime) { r.CmdQueue = cq } }

func New(soulPath string, skillPool *skill.Pool, opts ...RuntimeOption) (*Runtime, error) {
    if skillPool == nil {
        skillPool = skill.BuiltinPool()
    }
    loader := soul.NewLoader(soulPath)
    if err := loader.Load(); err != nil {
        return nil, fmt.Errorf("load soul.md: %w", err)
    }
    rt := &Runtime{
        Boards:   board.NewManager(),
        Mailbox:  mailbox.New(),
        Skills:   skill.NewRegistry(skillPool),
        Soul:     loader,
        Watchdog: watchdog.New(watchdog.DefaultConfig()),
        CmdQueue: cmdqueue.NewManager(),
    }
    for _, opt := range opts {
        opt(rt)
    }
    return rt, nil
}
```

- [ ] **Step 2: 更新所有调用点处理 error**

- [ ] **Step 3: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1
git add backend/internal/runtime/runtime.go backend/internal/testserver/testserver.go backend/main.go backend/cmd/tui/main.go backend/cmd/demo/main.go backend/cmd/memory-console/main.go
git commit -m "refactor(runtime): make Runtime constructible via options and return errors"
```

---

### Task 3.3: 策略接口化（Skill / Soul / Watchdog）

**Files:**
- Create: `backend/internal/skill/policy.go`, `backend/internal/soul/classifier.go`, `backend/internal/soul/temperature.go`, `backend/internal/watchdog/estimator.go`
- Modify: `backend/internal/skill/pool.go`, `backend/internal/soul/soul.go`, `backend/internal/watchdog/watchdog.go`
- Test: 对应包测试

> **行为保真风险**：
> - **RISK_24**：`skill/pool.go` 有两个不同操作 - `AssembleSet`（pool.go:248，从 pool 选 N）与 `SelectOne`（pool.go:303-315，从已组装 SkillSet 选 1）。`SkillSelectionPolicy.Select` 只覆盖 AssembleSet，SelectOne 需单独接口或保留原方法。
> - **RISK_25**：`soul/soul.go:170-191` `Temperature` switch 对 `KindGeneric`/default 分支返回**调用方传入的 base**。`DefaultTemperaturePolicy{table map[TaskKind]float64}` 无法表达"fall through to caller base"语义。policy 实现需保留 fallback：map 未命中时返回 base。
> - **RISK_26**：`watchdog` 当前函数名 `Estimator`（`watchdog.go:34-43`），plan rename 为 `TokenEstimator.Estimate` 会破坏所有 `watchdog.Estimator` 调用点。需先 grep 调用点，采用"新增接口 + 保留旧函数名作适配"策略，或同步 rename 所有调用点。

**Interfaces:**
- Produces:
  ```go
  // AssembleSet 策略（从 pool 选 N）
  type SkillSelectionPolicy interface { Select(ctx, candidates, domain, goal string, maxKeep int) ([]*types.Skill, error) }
  // SelectOne 策略（从已组装 SkillSet 选 1）- RISK_24
  type SkillPickOnePolicy interface { PickOne(ctx, set []*types.Skill, goal string) (*types.Skill, error) }
  type TaskKindClassifier interface { Classify(text string) soul.TaskKind }
  // Temperature 保留 base fallback 语义 - RISK_25
  type TemperaturePolicy interface { Temperature(kind soul.TaskKind, base float64) float64 }
  type TokenEstimator interface { Estimate(text string) int }
  ```

- [ ] **Step 0: 盘点 `watchdog.Estimator` 调用点**

```bash
grep -rn "watchdog\.Estimator\|Estimator(" backend/ test/
```

- [ ] **Step 1: 提取 `SkillSelectionPolicy` + `SkillPickOnePolicy`（覆盖 AssembleSet 与 SelectOne）**

```go
package skill

type SkillSelectionPolicy interface {
    Select(ctx context.Context, candidates []*types.Skill, domain, goal string, maxKeep int) ([]*types.Skill, error)
}

type SkillPickOnePolicy interface {
    PickOne(ctx context.Context, set []*types.Skill, goal string) (*types.Skill, error)
}

type DefaultLLMPolicy struct { pool *Pool; promptTemplate string }
func (p *DefaultLLMPolicy) Select(...) ([]*types.Skill, error) { ... }
func (p *DefaultLLMPolicy) PickOne(...) (*types.Skill, error) { ... }
```

- [ ] **Step 2: 提取 `TaskKindClassifier` 与 `TemperaturePolicy`，保留 base fallback**

```go
package soul

type TaskKindClassifier interface { Classify(text string) TaskKind }
type KeywordClassifier struct { rules map[TaskKind][]string }
func (c *KeywordClassifier) Classify(text string) TaskKind { ... }

// TemperaturePolicy 保留 base fallback - RISK_25
// map 未命中（KindGeneric/default）时返回 caller base，不能返回 0。
type TemperaturePolicy interface { Temperature(kind TaskKind, base float64) float64 }
type DefaultTemperaturePolicy struct { table map[TaskKind]float64 }
func (p *DefaultTemperaturePolicy) Temperature(kind TaskKind, base float64) float64 {
    if v, ok := p.table[kind]; ok {
        return v
    }
    return base  // fallback to caller-provided base，匹配 soul.go:170-191 行为
}
```

- [ ] **Step 3: 提取 `TokenEstimator`，保留旧 `Estimator` 函数名作适配（RISK_26）**

```go
package watchdog

type TokenEstimator interface { Estimate(text string) int }

type ByteRatioEstimator struct { Ratio float64 }
func (e *ByteRatioEstimator) Estimate(text string) int {
    // 匹配 watchdog.go:43 当前公式：bytes/4 + 1（注意是 bytes 非 runes）
    return len(text)/4 + 1
}

// Estimator 旧函数名保留作适配，内部委托 TokenEstimator，避免破坏现有调用点。
// 待所有调用点迁移到 TokenEstimator 后，可在后续 Task 删除。
func Estimator(text string) int {
    return defaultEstimator.Estimate(text)
}
```

> **RISK_26 注意**：当前 `watchdog.go:43` 公式是 `bytes/4 + 1`（`len(text)` 字节数），**非** `len([]rune(text)) * Ratio`。原 plan 示例 `int(math.Ceil(float64(len([]rune(text))) * e.Ratio))` 用 runes，行为变化。默认实现必须用 bytes 匹配当前行为。

- [ ] **Step 4: 在 `Pool` / `Loader` / `Watchdog` 构造时注入策略，默认保留当前行为（逐项核对 soul.go:175-184 温度表、watchdog.go:43 公式）**

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/skill/... ./backend/internal/soul/... ./backend/internal/watchdog/... -count=1
git add backend/internal/skill/policy.go backend/internal/soul/classifier.go backend/internal/soul/temperature.go backend/internal/watchdog/estimator.go backend/internal/skill/pool.go backend/internal/soul/soul.go backend/internal/watchdog/watchdog.go
git commit -m "refactor(skill,soul,watchdog): introduce policy interfaces for business rules"
```

---

### Task 3.4: `logger` 持久化改为 Worker Pool + DTO（含 UTF-8 截断行为修复）

**Files:**
- Create: `backend/internal/logger/record.go`, `backend/internal/logger/batch_store.go`
- Modify: `backend/internal/logger/logger.go`
- Test: `backend/internal/logger/logger_test.go`, `backend/internal/server/session_logs_test.go`

> **行为修复声明**：本 Task 含两处行为变更：(1) `truncate` 从按 byte 截断（`s[:n]`，UTF-8 多字节字符会截断产生乱码）改为按 rune 截断；(2) 用有界 worker pool 替代每条日志一 goroutine，新增背压（队列满时阻塞或丢弃，需在 Step 1 测试中明确策略）。UTF-8 截断修复会改变落库的 prompt/response 字符串末尾内容，需回归测试。

**Interfaces:**
- Produces:
  ```go
  type LLMCallRecord struct { Agent, Model, Prompt, Response string; InputTokens, OutputTokens, LatencyMs int }
  type LogStore interface { SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error }
  type BatchingLogStore struct { store LogStore; workers int; queue chan *store.SessionLogRecord }
  ```

- [ ] **Step 1: 定义 DTO 与批量 store**

```go
package logger

type LLMCallRecord struct {
    Agent       string
    Model       string
    Prompt      string
    Response    string
    InputTokens int
    OutputTokens int
    LatencyMs   int
}

type BatchingLogStore struct {
    store   LogStore
    workers int
    queue   chan *store.SessionLogRecord
    wg      sync.WaitGroup
}

func NewBatchingLogStore(s LogStore, workers int, queueSize int) *BatchingLogStore { ... }
func (b *BatchingLogStore) Enqueue(ctx context.Context, rec *store.SessionLogRecord) error { ... }
func (b *BatchingLogStore) Stop() { ... }
```

- [ ] **Step 2: 修改 `Logger.LLMCall` 使用 DTO**

```go
func (l *Logger) LLMCall(ctx context.Context, rec LLMCallRecord, extra ...slog.Attr) {
    // 拷贝 record，按 rune 截断 prompt/response
}
```

- [ ] **Step 3: 用 `BatchingLogStore` 替代每条日志一 goroutine**

- [ ] **Step 4: 修复 `truncate` 按 rune 截断**

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/logger/... ./backend/internal/server/... -count=1
git add backend/internal/logger/record.go backend/internal/logger/batch_store.go backend/internal/logger/logger.go
git commit -m "refactor(logger): replace per-log goroutines with bounded worker pool and DTO"
```

---

### Task 3.5: 修复 `dag.Scheduler` 生命周期与深拷贝（含 cloneTask 语义保真）

**Files:**
- Create: `backend/internal/dag/runner.go`, `backend/internal/dag/toposort.go`
- Modify: `backend/internal/dag/dag.go`
- Test: `test/api/dag_test.go`, 全量测试

> **行为保真风险**：当前 `dag.go:343-345` 用 `json.Marshal`/`Unmarshal` 深拷贝 DAG。改用显式 `cloneTask`/`cloneDAG` 时，必须保持以下语义：(1) nil 切片深拷贝后仍为 nil（JSON 会把 nil 切片变成 `[]`，反序列化得到非 nil 空切片，这是**原行为**，cloneTask 可选择保留原 JSON 语义或修正为 nil-保留-需明确）；(2) 指针字段是否共享；(3) map 字段深拷贝；(4) time.Time 字段保留时区。**执行前先写测试固化 JSON 深拷贝的当前输出**（序列化后再反序列化的字段值），再用 cloneTask 跑同样测试对比，差异即行为变化。goroutine 生命周期修复（Step 4）属于行为修复（原 `Stop` 不等待 `loop` 退出，修复后等待）。

**Interfaces:**
- Produces:
  ```go
  type DAGRunner struct { dag *DAG; launcher SessionLauncher }
  func TopoSort(tasks []*Task) ([]*Task, error)
  ```

- [ ] **Step 1: 提取纯函数 `TopoSort` 并优化到 O(V+E)**

```go
package dag

func TopoSort(tasks []*Task) ([]*Task, error) {
    // 构建邻接表与入度表
    // Kahn 算法
}
```

- [ ] **Step 2: 写测试固化 JSON 深拷贝当前输出（nil 切片、指针、map、time.Time 字段值），作为 cloneTask 对比基线**

- [ ] **Step 3: 拆分 `Scheduler` 为 `CronTrigger` / `DAGRunner` / `Scheduler`**

- [ ] **Step 4: 用显式 `cloneTask` / `cloneDAG` 替代 JSON 深拷贝，跑 Step 2 测试对比**

若差异仅为 nil 切片语义修正（nil 保留 nil），可接受并更新测试断言；若其他字段差异，需调整 cloneTask 实现。

- [ ] **Step 5: 修复 goroutine 生命周期：使用 `sync.WaitGroup`，`Stop` 等待 `loop` 返回**

- [ ] **Step 6: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/dag/... ./test/api/dag_test.go -count=1
git add backend/internal/dag/runner.go backend/internal/dag/toposort.go backend/internal/dag/dag.go
git commit -m "refactor(dag): split scheduler, fix lifecycle, replace JSON deep copy"
```

---

### Task 3.6: 清理 `mailbox` / `board` 小债务 + `cmdqueue` 容量上限（行为修复）

**Files:**
- Modify: `backend/internal/mailbox/mailbox.go`, `backend/internal/board/board.go`, `backend/internal/cmdqueue/cmdqueue.go`
- Test: 对应包测试

> **行为修复声明**：`cmdqueue` 当前 `Push` 无上限，本 Task 新增 `defaultQueueCap = 100` 与 `ErrQueueFull` 返回值。原调用方 `Push` 无 error 返回，修复后调用方需处理 `ErrQueueFull`。这是新增的拒绝路径，属于行为变更。`mailbox` 的 `itoa` 替换与 `board` 常量化属于纯重构，无行为变化。

- [ ] **Step 1: `mailbox` 删除手写 `itoa`，使用 `strconv.FormatInt`**

- [ ] **Step 2: `mailbox` 提取 `drainMessages` 减少 `Drain`/`DrainBroadcast` 重复**

- [ ] **Step 3: `board` 定义 `BoardStatus` 常量与原子 ID 生成**

```go
type BoardStatus string
const BoardStatusNew BoardStatus = "NEW"
const BoardStatusInProgress BoardStatus = "IN_PROGRESS"
const BoardStatusDone BoardStatus = "DONE"
const BoardStatusFailed BoardStatus = "FAILED"
```

- [ ] **Step 4: 写回归测试固化 `cmdqueue.Push` 当前无界行为（可无限 append），再修改为有界 + `ErrQueueFull`**

```go
const defaultQueueCap = 100
func (q *Queue) Enqueue(cmd UserCommand) error {
    if len(q.items) >= q.cap { return ErrQueueFull }
    q.items = append(q.items, cmd)
    return nil
}
```

- [ ] **Step 5: 更新所有 `cmdqueue.Push` 调用点处理 `ErrQueueFull`（server 层 queue 模式）**
- [ ] **Step 6: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/mailbox/... ./backend/internal/board/... ./backend/internal/cmdqueue/... -count=1
git add backend/internal/mailbox/mailbox.go backend/internal/board/board.go backend/internal/cmdqueue/cmdqueue.go
git commit -m "refactor(mailbox,board,cmdqueue): remove custom itoa, add status enum, queue capacity"
```

---

## Track 4: Agent 模块 API 封装与 Server/TUI 瘦身

**目标：** 把 Agent 编排能力（`internal/graph` + `internal/runtime` + `internal/server` 中的 session 生命周期管理）封装为 `internal/agent` 模块，对外暴露稳定 Go 接口 `agent.Agent` 与 DTO。HTTP（对话模块）、TUI（in-process）通过该接口调用 Agent，不再直接依赖 `internal/graph` / `internal/runtime` 内部类型。测试模块作为后期需求延期，本次不涉及。在此基础上拆分 `SessionManager`、统一事件类型、TUI 子模型化、统一 wiring。

**封装原则：**
- `internal/agent` 是唯一对外的 Agent 门面，依赖 `graph` + `runtime` + `store`，不依赖 `server` / `tui` / `test`。
- `agent.Agent` 接口只暴露 DTO（`Session` / `Event` / `Message` / `Query` / `ControlCommand`），不泄漏 `types.ThreeLayerState` 等内部状态机类型。
- HTTP handler、TUI 通过构造函数注入 `agent.Agent`，可替换为 mock 实现（测试模块延期，mock 注入后续补齐）。
- 适配层不做业务决策，只做协议转换（HTTP <-> DTO、tea.Msg <-> DTO）。

### Task 4.1: 定义 `agent.Agent` 接口与 DTO

**Files:**
- Create: `backend/internal/agent/agent.go`, `backend/internal/agent/types.go`, `backend/internal/agent/errors.go`
- Test: `backend/internal/agent/agent_test.go`

> **DTO 字段保真要求**：`Session`/`Event`/`Message` 等 DTO 必须**逐字段对齐** `server.Session`（10 字段）与 `server.SessionEvent`（13 字段），避免 HTTP 响应/TUI 渲染字段丢失。
>
> **`server.Session` 字段清单**（`session.go:43-55`）：`ID`, `Goal`, `Status`, `Result`, `State`, `StartedAt`, `EndedAt`, `Events`, `Messages`, `TempDir`。
>
> **`server.SessionEvent` 字段清单**（`session.go:59-76`）：`Type`, `Agent`, `Message`, `Kind`, `Tool`, `ToolPath`, `ToolOutput`, `ToolError`, `Success`, `Timestamp`, `Prompt`, `InputTokens`, `OutputTokens`, `DetailJSON`。
>
> 执行前先 grep 核对上述清单未变：
> ```bash
> grep -n "type Session struct" -A 20 backend/internal/server/session.go
> grep -n "type SessionEvent struct" -A 20 backend/internal/server/session.go
> ```
> 盘点结果写入 `docs/superpowers/notes/agent-dto-inventory.md`，作为 `types.go` 字段清单依据。任何字段遗漏会导致 HTTP 响应缺字段或 TUI 工具执行渲染断裂，属于行为变化。

**Interfaces:**
- Produces:
  ```go
  package agent

  type Agent interface {
      CreateSession(ctx context.Context, req CreateRequest) (*Session, error)
      ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error)
      Send(ctx context.Context, sessionID string, msg Message) error
      Stream(ctx context.Context, sessionID string) (<-chan Event, error)
      Query(ctx context.Context, sessionID string, q Query) (Result, error)
      Control(ctx context.Context, sessionID string, cmd ControlCommand) error
      List(ctx context.Context, filter Filter) ([]*Session, error)
      Get(ctx context.Context, sessionID string) (*Session, error)
      // ListAgents 返回会话内 Agent 实例清单（供 TUI agent 面板渲染），
      // 解决 RISK_20：TUI 原直接调用 registry.GetInstancesBySession。
      ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error)
      Shutdown(ctx context.Context) error
  }

  type CreateRequest struct { Goal string; Meta map[string]any }
  type ResumeRequest struct { CarryOver string; UserInput string }
  type Message struct { Role string; Content string }
  type Query struct { Kind string; Args map[string]any }
  type Result struct { Data any }
  type ControlCommand struct { Op string; Args map[string]any }
  type Filter struct { Status string; Limit int }

  // Session 字段必须与 server.Session 1:1 对齐（RISK_29）。
  type Session struct {
      ID        string
      Goal      string
      Status    string
      Result    string
      State     string        // 序列化后的 ThreeLayerState 摘要，不暴露内部类型
      StartedAt time.Time
      EndedAt   time.Time
      Events    []Event
      Messages  []Message
      TempDir   string
  }

  // Event 字段必须与 server.SessionEvent 1:1 对齐（RISK_30）。
  type Event struct {
      Type        string
      Agent       string
      Message     string
      Kind        string
      Tool        string
      ToolPath    string
      ToolOutput  string
      ToolError   string
      Success     bool
      Timestamp   time.Time
      Prompt      string
      InputTokens int
      OutputTokens int
      DetailJSON  string
  }

  // AgentInstance 供 TUI 渲染 agent 面板（RISK_20）。
  type AgentInstance struct {
      Name      string
      Role      string
      ModuleID  string
      Status    string
      // ... 与 types.RoleInstance 公开字段对齐
  }

  var (
      ErrSessionNotFound = errors.New("agent: session not found")
      ErrSessionFinished = errors.New("agent: session already finished")
      ErrQueueFull       = errors.New("agent: command queue full")
  )
  ```

- [ ] **Step 1: 在 `backend/internal/agent/agent.go` 定义 `Agent` 接口（含 `ListAgents` 解决 TUI roster 依赖）**
- [ ] **Step 2: 在 `types.go` 定义全部 DTO，逐字段对齐 `server.Session`（10 字段）/ `server.SessionEvent`（13 字段）/ `types.RoleInstance`，编写映射函数 `toAgentSession` / `toAgentEvent` / `toAgentInstance`**
- [ ] **Step 3: 在 `errors.go` 定义 sentinel error，与现有 `server` 包错误语义对齐**
- [ ] **Step 4: 编写 `agent_test.go` 验证 DTO 与 server 类型互转无丢失（字段逐一断言）**
- [ ] **Step 5: Commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/agent/... -count=1
git add backend/internal/agent
git commit -m "refactor(agent): define Agent interface and DTO for module encapsulation"
```

---

### Task 4.2: 实现 `agent.Service`（从 SessionManager 抽取核心逻辑）

**Files:**
- Create: `backend/internal/agent/service.go`, `backend/internal/agent/service_runner.go`
- Modify: `backend/internal/server/session.go`（保留 HTTP handler 壳，逻辑下沉到 `agent.Service`）
- Test: `backend/internal/agent/service_test.go`, `test/api/...`

**Interfaces:**
- Consumes: `*graph.ThreeLayerGraph`, `*runtime.Runtime`, `store.SessionStore`, `store.SnapshotStore`
- Produces: `agent.Service` 实现 `agent.Agent` 接口

- [ ] **Step 1: 定义 `Service` 结构体持有 graph/runtime/stores/eventbus**

```go
package agent

type Service struct {
    graph   *graph.ThreeLayerGraph
    runtime *runtime.Runtime
    store   Store
    bus     *EventBus
    // ...
}

func New(graph *graph.ThreeLayerGraph, rt *runtime.Runtime, store Store, opts ...Option) *Service { ... }
```

- [ ] **Step 2: 把 `SessionManager` 的 `CreateSession` / `runSession` / `resumeSession` / `processClarify` / `processQueue` 逻辑迁移到 `agent.Service` 对应方法**

迁移时必须**逐项核对**以下行为等价（每项写回归测试）：

| 行为 | 当前位置 | 迁移要点 |
|---|---|---|
| 终态判定 gate on `CmdQueue.HasPending` | `session.go:625-627,1912` | 会话完成前必须检查 `rt.CmdQueue.HasPending(sessionID)`，有 pending interrupt/enqueue 时不标记 complete。`agent.Service` 需持有 `CmdQueue` 引用。 |
| 事件 append 绕锁路径 | `session.go:1278,1342,1412,1471,1558,1641` | 6 处用 `session.Events = append(...)` 而非 `addEvent`，注释明确"避免 addEvent 再次取锁自死锁"。`agent.Service` 必须保留此 lock-held append 路径，不能强制走 `addEvent`。 |
| clarify 条件（非数值阈值） | `session.go:608-614,1895-1901` | 条件为 `NextAction==ActionWait && PendingClarify!=nil`，**无数值阈值**。原 plan "clarify 阈值"表述误导，按条件表达式迁移。 |
| cmdqueue 队列（无 priority 字段） | `session.go:1327-1332,1397-1402` | `cmdqueue.Item{Content, Intent}` **无 priority**。原 plan "队列优先级"误读，mailbox `msg.Priority`（`session.go:2081,2100`）是 mailbox 消息非 cmdqueue。迁移时不要新增 priority。 |
| tempDir 清理双 callsite | `session.go:578-582,1835-1839` + helper `:241` | `runSession` 结束 + `resumeSession` 结束两处都需清理，`agent.Service.serviceRunner` 统一在 `run` defer 中处理。 |
| 快照两种含义区分 | `session.go:524-557` (SSE 内存 copy) vs `memory/callback.go` (持久化) | `snapshotSession` 是 SSE 内存深拷贝，**非** `store.SaveSnapshot`。`agent.Service` 需保留 SSE 内存快照路径，持久化由 `memory.Callback` 触发不变。 |
| 事件 append 顺序 | 全文 | `Events` 切片顺序即 SSE 推送顺序，迁移后顺序不变。 |
| 持久化快照保存时机 | `memory/callback.go` | 由 `memory.Callback.OnEnd` 触发，`agent.Service` 不接管，保持 `memory` 包内部时序。 |

- [ ] **Step 3: `SessionManager` 持有 `agent.Agent`，方法瘦身为 HTTP 协议适配**

```go
type SessionManager struct { agent agent.Agent; bus *EventBus; /* HTTP 相关字段 */ }
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
    var req agent.CreateRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { /* ... */ }
    sess, err := m.agent.CreateSession(r.Context(), req)
    if err != nil { /* ... */ }
    json.NewEncoder(w).Encode(sess)
}
```

- [ ] **Step 4: `EventBus` 迁移到 `agent` 包，`Stream` 返回 `<-chan agent.Event`**
- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/agent/... ./backend/internal/server/... ./test/api/... -count=1
git add backend/internal/agent/service.go backend/internal/agent/service_runner.go backend/internal/server/session.go
git commit -m "refactor(agent): implement Service as Agent facade, slim SessionManager to HTTP adapter"
```

---

### Task 4.3: ~~测试模块通过 `agent.Agent` 接口注入~~（延期至后续测试模块需求）

> **延期声明**：测试模块作为独立需求后期交付，本次重构不涉及 `MockAgent` 注入与 test fixture 改造。现有 `test/fixtures/llm.go`（HTTP 层 MockLLM）与 `test/api`/`test/coding`/`test/tui` 集成测试保持现状，仅作为重构回归验证基线运行，不在本计划范围内修改。后续测试模块需求启动时，再基于已稳定的 `agent.Agent` 接口实现 `MockAgent`。

---

### Task 4.4: TUI 模块通过 `agent.Agent` 接口调用（in-process）

**Files:**
- Modify: `backend/internal/tui/model.go`, `view.go`, `helpers.go`
- Modify: `backend/cmd/tui/main.go`
- Test: `backend/internal/tui/...`, `test/tui/...`

> **封装完整性风险（RISK_20, RISK_21）**：
> - `tui/model.go:28-34` 当前持有 `*graph.RoleRegistry`、`*runtime.Runtime`、`*store.PostgresStore`、`*model.ModelFactory`。`agent.Agent` 接口已新增 `ListAgents`（Task 4.1）解决 roster 依赖，但 `Runtime`/`PostgresStore`/`ModelFactory` 的 TUI 直连用途需逐项核对：
>   - `ModelFactory`：TUI `warmTaskBriefAsync`（`view.go:1063`）需调用 LLM。`agent.Agent` 需新增 `WarmTaskBrief(ctx, title) (string, error)` 或 TUI 保留 `ModelFactory` 引用（接受部分封装）。
>   - `PostgresStore`：TUI 知识库/快照查询直连 SQL。需在 `agent.Agent` 新增查询方法或 TUI 保留 store 引用。
>   - `Runtime`：TUI 看 task board / mailbox 状态。需新增 `agent.Agent.GetBoard(ctx, sessionID)` 等只读方法。
> - `cmd/tui/main.go:198` `server.NewDAGHandler(pgStore, dagScheduler)` 暴露给 TUI。`agent.Agent` 无 DAG surface，DAGHandler 仍为直接 server 包依赖。**本 Task 决策**：DAG 相关功能 TUI 暂保留 `server.DAGHandler` 直连（属适配层可接受的依赖），仅 Agent 编排走 `agent.Agent`。

**Interfaces:**
- Consumes: `agent.Agent`, `server.DAGHandler`（DAG 保留直连）, 可能保留 `*model.ModelFactory`（task brief LLM）

- [ ] **Step 1: `tui.Model` 持有 `agent.Agent` 字段，移除 `*graph.RoleRegistry` 直连（用 `agent.ListAgents` 替代）**

```go
type Model struct {
    agent     agent.Agent
    dagHandler *server.DAGHandler  // RISK_21: DAG 保留直连
    // ModelFactory / PostgresStore 按需保留，逐项核对后决策
    sessionID string
    events    <-chan agent.Event
    // ...
}
```

- [ ] **Step 2: TUI 启动会话改用 `agent.CreateSession`，事件流改用 `agent.Stream`，agent 面板改用 `agent.ListAgents`**
- [ ] **Step 3: `cmd/tui/main.go` 通过 `bootstrap.Build` 获取 `agent.Agent` 与 `DAGHandler`，移除重复 wiring（`sessionRouter` / `pgBlockMemoryAdapter` / `pgHistoryAdapter` / `sinkerNode` 全部下沉到 `bootstrap`）**
- [ ] **Step 4: 核对 `warmTaskBriefAsync` 等仍依赖 `ModelFactory` 的点，决定封装到 `agent.Agent` 还是保留引用，写入 `docs/superpowers/notes/tui-encapsulation-decisions.md`**
- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/tui/... ./test/tui/... -count=1
git add backend/internal/tui backend/cmd/tui/main.go
git commit -m "refactor(tui): call agent via Agent interface in-process, remove duplicate wiring"
```

---

### Task 4.5: 拆分 `SessionManager` HTTP 层（按路由分文件 + `MetricsCollector`）

**Files:**
- Create: `backend/internal/server/metrics.go`, `backend/internal/server/session_http.go`, `backend/internal/server/stream_http.go`, `backend/internal/server/control_http.go`
- Modify: `backend/internal/server/session.go`（瘦身到 400 行以内，仅保留 struct 定义与路由注册）
- Test: `backend/internal/server/session_test.go`, `session_logs_test.go`, `test/api/...`

> **范围调整**：Task 4.2 已把会话生命周期（orchestrator）、事件总线（EventBus）、持久化（Store）迁移到 `agent` 包。本 Task 仅处理 HTTP 层剩余职责：`MetricsCollector`（LLM 指标聚合，服务端独立关注点）与 `session.go`（2236 行）按路由组分文件。不再创建 `server/orchestrator.go` / `server/eventbus.go` / `server/session_store.go`（已在 `agent` 包）。

**Interfaces:**
- Produces:
  ```go
  type MetricsCollector struct { /* LLM 调用统计聚合 */ }
  func (m *MetricsCollector) Record(agent, model string, in, out, latency int)
  func (m *MetricsCollector) Snapshot() MetricsSnapshot
  ```

- [ ] **Step 1: 实现 `MetricsCollector`，从 `SessionManager` 抽取 LLM 指标聚合逻辑**

- [ ] **Step 2: `session.go` 按路由组拆分 HTTP handler**
  - `session_http.go`：`HandleCreateSession` / `HandleGetSession` / `HandleListSessions` / `HandleDeleteSession`
  - `stream_http.go`：`HandleStreamSession`（SSE）
  - `control_http.go`：`HandlePause` / `HandleResume` / `HandleInterrupt` / `HandleQueue`

- [ ] **Step 3: `SessionManager` struct 仅保留 `agent.Agent`、`MetricsCollector`、路由注册字段**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/server/... ./test/api/... -count=1
git add backend/internal/server/metrics.go backend/internal/server/session_http.go backend/internal/server/stream_http.go backend/internal/server/control_http.go backend/internal/server/session.go
git commit -m "refactor(server): split SessionManager HTTP layer by route group, extract MetricsCollector"
```

---

### Task 4.6: ~~合并 `runSession` / `resumeSession` 为统一 runner~~（已并入 Task 4.2）

> **合并声明**：Task 4.2 Step 2 已将 `runSession` / `resumeSession` 逻辑统一迁移到 `agent.Service.service_runner.go`（`serviceRunner.run`），`SessionManager` 仅构造 runner 并调用。本 Task 不再独立存在，避免与 4.2 重复。

---

### Task 4.7: 统一事件 kind 常量与 HTTP 路由参数

**Files:**
- Create: `backend/internal/server/eventkind/kind.go`, `backend/internal/server/request.go`
- Modify: `backend/internal/server/session.go`, `backend/internal/server/api.go`, `backend/internal/server/dag.go`
- Test: `backend/internal/server/session_test.go`, `test/api/...`

- [ ] **Step 1: 定义事件 kind 常量**

```go
package eventkind

const (
    Think       = "think"
    Prompt      = "prompt"
    TokenUsage  = "token_usage"
    GraphStep   = "graph_step"
    ToolExec    = "tool_exec"
    ToolCall    = "tool_call"
    MemoryRecall = "memory_recall"
    TopicSwitch = "topic_switch"
    LLMResult   = "llm_result"
    AgentDone   = "agent_done"
)
```

- [ ] **Step 2: 替换 `session.go` / `api.go` 中所有硬编码字符串**

- [ ] **Step 3: 使用 Go 1.22 `http.ServeMux` 路径变量，简化 handler 签名**

```go
mux.HandleFunc("GET /api/sessions/{id}", m.HandleGetSession)

func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    // ...
}
```

- [ ] **Step 4: 提取 `request.DecodeBody[T]`**

```go
func DecodeBody[T any](r *http.Request) (T, error) { ... }
```

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/server/... ./test/api/... -count=1
git add backend/internal/server/eventkind/kind.go backend/internal/server/request.go backend/internal/server/session.go backend/internal/server/api.go backend/internal/server/dag.go
git commit -m "refactor(server): centralize event kind constants and use path variables"
```

---

### Task 4.8: 统一后端 wiring（bootstrap 包，产出 `agent.Agent`）

**Files:**
- Create: `backend/internal/bootstrap/bootstrap.go`
- Modify: `backend/internal/testserver/testserver.go`, `backend/cmd/tui/main.go`, `backend/main.go`
- Test: `test/api/...`, `test/tui/...`

**Interfaces:**
- Produces:
  ```go
  // App 公开字段仅暴露对外接口，内部 graph/runtime/registry 用小写不导出（RISK_32），
  // 避免上层反向 import bootstrap 形成循环。
  type App struct {
      Agent   agent.Agent             // 主交付物：所有模块通过此字段访问 Agent
      Server  *server.SessionManager  // HTTP adapter
      DAGHandler *server.DAGHandler   // DAG HTTP handler（TUI 也需）
      // 内部依赖不导出，避免 bootstrap 被反向 import
      runtime  *runtime.Runtime
      graph    *graph.ThreeLayerGraph
      registry *graph.RoleRegistry
  }
  func Build(ctx context.Context, cfgPaths ConfigPaths) (*App, error)
  ```

- [ ] **Step 1: 将 `testserver.BuildHandler` 中的装配逻辑提取到 `bootstrap.Build`，产出 `agent.Agent`**

```go
package bootstrap

type ConfigPaths struct {
    ConfigPath string
    RolePath   string
    EnvPath    string
    SoulPath   string
    SkillPath  string
}

func Build(ctx context.Context, paths ConfigPaths) (*App, error) {
    // config.Load -> store -> model factory -> runtime -> graph
    // -> agent.New(graph, runtime, store, opts...) -> agent.Agent
    // -> server.NewSessionManager(agent, ...) -> HTTP adapter
}
```

- [ ] **Step 2: 将 `pgBlockMemoryAdapter`、`pgHistoryAdapter`、`sessionRouter`、`sinkerNode` 统一放到 `bootstrap` 或 `graph` 包**

- [ ] **Step 3: 让 `main.go` / `cmd/tui/main.go` / `testserver` 都调用 `bootstrap.Build`，三方均通过 `App.Agent` 访问 Agent 能力**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... ./test/... -count=1
git add backend/internal/bootstrap/bootstrap.go backend/internal/testserver/testserver.go backend/cmd/tui/main.go backend/main.go
git commit -m "refactor(bootstrap): unify backend wiring, expose agent.Agent as primary deliverable"
```

---

### Task 4.9: 拆分 TUI Model 为子组件

**Files:**
- Create: `backend/internal/tui/chat_panel.go`, `backend/internal/tui/input_bar.go`, `backend/internal/tui/overlay_panel.go`, `backend/internal/tui/agent_tree_panel.go`, `backend/internal/tui/task_brief_cache.go`
- Modify: `backend/internal/tui/model.go`, `view.go`, `helpers.go`
- Test: `backend/internal/tui/model_test.go`, `view_test.go`, `test/tui/...`

**Interfaces:**
- Produces:
  ```go
  type tea.Model 子模型
  type TaskBriefCache struct { ... }
  ```

- [ ] **Step 1: 定义 `ChatPanel` 子模型**

```go
type ChatPanel struct {
    items    []chatItem
    viewport viewport.Model
    // ...
}
func (p *ChatPanel) Init() tea.Cmd { return nil }
func (p *ChatPanel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { ... }
func (p *ChatPanel) View() string { ... }
```

- [ ] **Step 2: 定义 `InputBar`、`OverlayPanel`、`AgentTreePanel` 子模型**

- [ ] **Step 3: 在顶层 `Model` 中持有子模型，消息分发到对应组件**

- [ ] **Step 4: 提取 `TaskBriefCache` 并限制并发**

```go
type TaskBriefCache struct {
    mu     sync.Mutex
    items  map[string]string
    sema   chan struct{}
}

func (c *TaskBriefCache) Warm(ctx context.Context, mf *model.ModelFactory, title string) { ... }
```

- [ ] **Step 5: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/tui/... ./test/tui/... -count=1
git add backend/internal/tui/chat_panel.go backend/internal/tui/input_bar.go backend/internal/tui/overlay_panel.go backend/internal/tui/agent_tree_panel.go backend/internal/tui/task_brief_cache.go backend/internal/tui/model.go backend/internal/tui/view.go backend/internal/tui/helpers.go
git commit -m "refactor(tui): split Model into sub-models and limit task brief goroutines"
```

---

### Task 4.10: 修复 `APIHandler` 与 `LLMCallTracker` 缺陷（行为修复）

**Files:**
- Modify: `backend/internal/server/api.go`, `backend/internal/model/llm_tracker.go`, `llm_tracker_stats.go`, `blades_client.go`
- Test: `backend/internal/model/llm_tracker_test.go`, `backend/internal/server/auth_test.go`

> **行为修复声明**：本 Task 修复 `LLMCallTracker` 错误计数语义（原任何 err 都计入 `timeoutCount` 并触发 slowMode，cancel 也被误计）。修复前 slowMode 触发条件包含 cancel/网络错误，修复后仅 `DeadlineExceeded` 累计。需先写回归测试固化旧错误行为，再修复，确保差异可观测。

- [ ] **Step 1: 写回归测试固化 `LLMCallTracker` 当前错误计数行为**

```go
// 旧行为：context.Canceled 也计入 timeoutCount
// 测试断言：连续 3 次 Canceled 后 slowMode == true
// 此测试在 Step 4 修复后需更新断言为 false
```

- [ ] **Step 2: `APIHandler.paused` 加 `sync.RWMutex` 保护**

- [ ] **Step 3: 合并 `SnapshotHandler` 与 `SnapshotInspectHandler`**

- [ ] **Step 4: 将 SQL/评分逻辑下沉到 `MemoryService` / `StatsService`**

- [ ] **Step 5: `LLMCallTracker` 区分 timeout / cancel / other error，更新 Step 1 测试断言**

```go
type LLMCallTracker struct {
    // ...
    consecutiveTimeout int
    consecutiveError   int
    consecutiveCancel  int
}

func (t *LLMCallTracker) RecordCall(ctx context.Context, err error, latency time.Duration) {
    switch {
    case errors.Is(err, context.Canceled):
        t.consecutiveCancel++
    case errors.Is(err, context.DeadlineExceeded):
        t.consecutiveTimeout++
        t.slowMode = t.consecutiveTimeout >= 3
    case err != nil:
        t.consecutiveError++
    }
}
```

- [ ] **Step 6: `BladesClient` 提取 `doGenerate`**

```go
func (c *BladesClient) doGenerate(ctx context.Context, req *blades.ModelRequest) (*blades.Message, error) { ... }
```

- [ ] **Step 7: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/server/... ./backend/internal/model/... -count=1
git add backend/internal/server/api.go backend/internal/model/llm_tracker.go backend/internal/model/llm_tracker_stats.go backend/internal/model/blades_client.go
git commit -m "fix(server,model): protect paused map, merge snapshot handlers, classify LLM errors, dedupe generate"
```

---

## Track 5: Frontend 结构化

**目标：** 拆分 API / 类型 / composables / 组件 / 视图，统一状态/日期/事件样式，消除 mock 数据，提升 TypeScript 类型安全。

### Task 5.1: 拆分 API 层与统一类型

**Files:**
- Create: `web/src/api/client.ts`, `web/src/api/health.ts`, `web/src/api/metrics.ts`, `web/src/api/skills.ts`, `web/src/api/memory.ts`, `web/src/api/files.ts`
- Modify: `web/src/api/session.ts`（仅保留会话相关）
- Modify: `web/src/types/index.ts`（合并 Skill / HealthStatus）
- Test: 运行 `cd web && npm run type-check`（若存在）

**Interfaces:**
- Produces:
  ```ts
  // api/client.ts
  export async function fetchJson<T>(url: string, options?: RequestInit, config?: { timeout?: number }): Promise<T>
  
  // api/session.ts
  export function createSession(goal: string): Promise<Session>
  export function streamSession(id: string, handlers: StreamHandlers): () => void
  ```

- [ ] **Step 1: 创建 `api/client.ts` 统一 `fetchJson` 与超时**

```ts
import { APP_CONFIG } from '@/config/app'

export class APIError extends Error { ... }

export async function fetchJson<T>(
  url: string,
  options: RequestInit = {},
  config: { timeout?: number } = {}
): Promise<T> {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), config.timeout ?? APP_CONFIG.apiTimeout)
  try {
    const res = await fetch(`${APP_CONFIG.apiBase}${url}`, { ...options, signal: controller.signal })
    if (!res.ok) throw new APIError(res.statusText, res.status)
    return await res.json()
  } finally {
    clearTimeout(timeout)
  }
}
```

- [ ] **Step 2: 按领域拆分 API 文件**

- [ ] **Step 3: 删除 `api/session.ts` 中重复的类型定义，统一引用 `types/index.ts`**

- [ ] **Step 4: 创建 `web/src/config/app.ts` 集中配置**

```ts
export const APP_CONFIG = {
  apiBase: import.meta.env.VITE_API_BASE ?? '',
  apiTimeout: 10_000,
  sseRetry: { max: 5, baseMs: 1000, capMs: 16_000 },
  healthPollInterval: 10_000,
  panelRefreshInterval: 3_000,
  tokenContextLimit: 128_000,
  warningThreshold: 0.8,
  errorThreshold: 0.95,
}
```

- [ ] **Step 5: 运行类型检查并 commit**

```bash
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
git add web/src/api web/src/types/index.ts web/src/config/app.ts
git commit -m "refactor(web): split API layer by domain and centralize app config"
```

---

### Task 5.2: 提取核心 Composables

**Files:**
- Create: `web/src/composables/useSessionStream.ts`, `usePanelRefresh.ts`, `useRoleTree.ts`, `useTaskBoard.ts`, `useSessionList.ts`, `useSessionStatus.ts`
- Modify: `web/src/views/chat/index.vue`, `web/src/views/session/index.vue`
- Test: 手动验证页面行为

**Interfaces:**
- Produces:
  ```ts
  export function useSessionStream(id: Ref<string>, handlers: { onEvent, onDone, onError }): { close, reconnect }
  export function usePanelRefresh(id: Ref<string>): { agents, board, metrics, refresh, loading }
  export function useRoleTree(agents: Ref<AgentNode[]>): ComputedRef<AgentTreeNode[]>
  export function useTaskBoard(board: Ref<TaskBoardData | null>): { tasks, progress, constraints }
  ```

- [ ] **Step 1: 实现 `useSessionStream`**

```ts
import { ref, watch, type Ref } from 'vue'
import { streamSession } from '@/api/session'

export function useSessionStream(
  id: Ref<string>,
  handlers: { onEvent: (ev: SessionEvent) => void; onDone: () => void; onError: (err: Error) => void }
) {
  const close = ref<() => void>(() => {})
  watch(id, (newId) => {
    close.value()
    close.value = streamSession(newId, handlers)
  }, { immediate: true })
  return { close: () => close.value() }
}
```

- [ ] **Step 2: 实现 `usePanelRefresh` 使用 `AbortController` + epoch**

- [ ] **Step 3: 实现 `useRoleTree`、`useTaskBoard`、`useSessionStatus`**

- [ ] **Step 4: 在 `chat/index.vue` 与 `session/index.vue` 中使用 composables，删除重复逻辑**

- [ ] **Step 5: 运行类型检查并 commit**

```bash
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
git add web/src/composables web/src/views/chat/index.vue web/src/views/session/index.vue
git commit -m "refactor(web): extract core composables for stream/refresh/role-tree/task-board"
```

---

### Task 5.3: 组件化 `session/index.vue` 与统一工具函数

**Files:**
- Create: `web/src/views/session/components/MetricsCard.vue`, `TokenMetricsCard.vue`, `WatchdogCard.vue`, `MailboxCard.vue`, `HealthCard.vue`, `SessionLogsPanel.vue`
- Create: `web/src/utils/date.ts`, `web/src/utils/sessionStatus.ts`
- Modify: `web/src/views/session/index.vue`, `web/src/views/dashboard/index.vue`, `web/src/views/chat/utils/eventStyles.ts`, `ExecutionLog.vue`
- Test: 手动验证

- [ ] **Step 1: 并行化 `loadSessionPanels` 使用 `Promise.allSettled`**

```ts
const [agentsRes, boardRes, metricsRes, watchdogRes, mailboxRes, healthRes] = await Promise.allSettled([
  fetchAgents(id), fetchBoard(id), fetchMetrics(id), fetchWatchdog(id), fetchMailbox(id), fetchHealth()
])
```

- [ ] **Step 2: 将右侧各卡片拆为独立组件**

- [ ] **Step 3: 提取 `utils/date.ts` 与 `utils/sessionStatus.ts`**

```ts
// utils/date.ts
export function formatTime(iso?: string): string { ... }
export function formatDateTime(iso?: string): string { ... }

// utils/sessionStatus.ts
export function statusColor(status: string): string { ... }
export function statusLabel(status: string): string { ... }
```

- [ ] **Step 4: 统一 `ExecutionLog` 复用 `eventStyles.ts`**

- [ ] **Step 5: 运行类型检查并 commit**

```bash
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
git add web/src/views/session/components web/src/utils web/src/views/session/index.vue web/src/views/dashboard/index.vue web/src/views/chat/utils/eventStyles.ts web/src/views/session/components/ExecutionLog.vue
git commit -m "refactor(web): componentize session view and unify status/date utilities"
```

---

### Task 5.4: 结构化事件类型与分组器拆分

**Files:**
- Modify: `web/src/types/index.ts`
- Modify: `web/src/views/chat/utils/turns.ts`
- Test: 手动验证聊天渲染

**Interfaces:**
- Produces:
  ```ts
  export type SessionEvent =
    | UserMessageEvent
    | ToolCallEvent
    | ToolExecEvent
    | LLMEvent
    | ErrorEvent
    | TokenUsageEvent
  ```

- [ ] **Step 1: 定义 discriminated union 事件类型**

```ts
export interface BaseEvent {
  id: string
  timestamp: string
}

export interface UserMessageEvent extends BaseEvent {
  type: 'user_message'
  content: string
}

export interface ToolCallEvent extends BaseEvent {
  type: 'tool_call'
  tool: string
  args: Record<string, unknown>
}

export interface ToolExecEvent extends BaseEvent {
  type: 'tool_exec'
  tool: string
  output: string
  success: boolean
}

export interface LLMEvent extends BaseEvent {
  type: 'llm'
  content: string
}

export interface ErrorEvent extends BaseEvent {
  type: 'error'
  message: string
}

export interface TokenUsageEvent extends BaseEvent {
  type: 'token_usage'
  input_tokens: number
  output_tokens: number
}
```

- [ ] **Step 2: 编写类型守卫 `isToolCallEvent`、`isLLMEvent` 等**

- [ ] **Step 3: 拆分 `groupEventsToTurns` 为纯函数**

```ts
export function classifyEvent(ev: SessionEvent): EventClass { ... }
export function isCompletion(ev: SessionEvent): boolean { ... }
export function isError(ev: SessionEvent): boolean { ... }
export function createToolGroup(events: SessionEvent[]): ToolGroup { ... }
export function finalizeTurn(turn: Turn): Turn { ... }
```

- [ ] **Step 4: 在 `turns.ts` 中组合上述纯函数**

- [ ] **Step 5: 运行类型检查并 commit**

```bash
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
git add web/src/types/index.ts web/src/views/chat/utils/turns.ts
git commit -m "refactor(web): use discriminated union for SessionEvent and split turn grouping"
```

---

### Task 5.5: 清理 placeholder 与全局图标注册

**Files:**
- Create: `web/src/components/FeaturePlaceholder.vue`, `MarkdownRenderer.vue`, `AppCard.vue`
- Modify: `web/src/main.ts`, `web/src/views/history/index.vue`, `knowledge/index.vue`, `skills/index.vue`, `settings/index.vue`, `soul/index.vue`
- Modify: `web/src/App.vue` 或 `layout/index.vue` 中卡片样式
- Test: 手动验证

- [ ] **Step 1: 创建 `FeaturePlaceholder.vue`**

```vue
<template>
  <div class="text-center text-gray-400 py-20">
    <el-icon><Tools /></el-icon>
    <p>{{ title }} 开发中</p>
    <p class="text-sm">{{ description }}</p>
  </div>
</template>
```

- [ ] **Step 2: 将 history / knowledge / skills / settings / soul 中的 mock/死按钮替换为 `FeaturePlaceholder` 或接入真实 API**

- [ ] **Step 3: `main.ts` 改为按需引入图标**

```ts
import { ChatLineRound, DataLine, Setting, Tools } from '@element-plus/icons-vue'
const app = createApp(App)
app.component('ChatLineRound', ChatLineRound)
// ...
```

- [ ] **Step 4: 创建 `MarkdownRenderer.vue` 统一 `.markdown-body` 样式**

- [ ] **Step 5: 创建 `AppCard.vue` 统一卡片壳**

- [ ] **Step 6: 运行类型检查并 commit**

```bash
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
git add web/src/components web/src/main.ts web/src/views/history/index.vue web/src/views/knowledge/index.vue web/src/views/skills/index.vue web/src/views/settings/index.vue web/src/views/soul/index.vue
git commit -m "refactor(web): replace placeholders, register icons on demand, unify card/markdown components"
```

---

## Track 7: 工程化能力补齐（跨域）

**目标：** 在业务逻辑不变的前提下，补齐错误处理、日志、通用工具、开发脚本、包依赖方向等工程化短板，使项目具备长期可维护性。

### Task 7.1: 统一错误处理模式

**Files:**
- Modify: `backend/internal/store/postgres.go`（拆分后）、`backend/internal/memory/search.go` / `write.go` / `callback.go`、`backend/internal/graph/*.go`
- Test: 全量测试

- [ ] **Step 1: 扫描所有 `_ = ...` 忽略错误点，分类处理**

```go
// 可恢复错误：返回或记录 warning
meta, err := json.Marshal(rec.Meta)
if err != nil { return fmt.Errorf("marshal meta: %w", err) }

// 非关键路径：记录 debug 但不阻断
_ = r.metaDB.IncrementAccessCount(ctx, rec.ID) // 改为 if err := ...; err != nil { log.Debug(...) }
```

- [ ] **Step 2: 将所有 `err.(*pq.Error)` 改为 `errors.As`**

- [ ] **Step 3: 对包装错误统一使用 `fmt.Errorf("...: %w", err)`**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/... -count=1
git add backend/internal/store backend/internal/memory backend/internal/graph
git commit -m "refactor: unify error handling with errors.As and explicit propagation"
```

---

### Task 7.2: 统一日志包边界（logger vs logging）

**Files:**
- Create: `backend/internal/logoutput/writer.go`（由 `logging` 重命名/迁移）
- Modify: `backend/internal/logger/logger.go`、`backend/internal/logging/logging.go`、`backend/main.go`
- Test: `backend/internal/logger/logger_test.go`

- [ ] **Step 1: 将 `logging` 包重命名为 `logoutput`，职责限定为"按天/入口切分文件输出"**

- [ ] **Step 2: `logger` 包可选注入 `logoutput.Writer` 作为文件目标，统一文件输出路径**

```go
func New(store LogStore, w io.Writer, opts ...LoggerOption) *Logger { ... }
```

- [ ] **Step 3: 消除 `logging.Init` 的全局副作用，改为返回 `io.WriteCloser`**

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/logger/... ./backend/internal/logoutput/... -count=1
git add backend/internal/logoutput backend/internal/logger backend/internal/logging backend/main.go
git commit -m "refactor(logger): clarify logger vs logoutput responsibilities and remove global side effects"
```

---

### Task 7.3: 补齐跨包通用工具

**Files:**
- Create: `backend/pkg/httputil/client.go`
- Modify: `backend/internal/embed/openai.go`、`backend/internal/server/api.go` 等使用 HTTP client 的位置
- Test: 新增测试

- [ ] **Step 1: 实现 `RetryableHTTPClient`**

```go
package httputil

type RetryConfig struct {
    MaxRetries int
    BaseDelay  time.Duration
    MaxDelay   time.Duration
}

type RetryableClient struct {
    client *http.Client
    cfg    RetryConfig
}

func (c *RetryableClient) Do(req *http.Request) (*http.Response, error) { ... }
```

- [ ] **Step 2: 替换 `embed/openai.go` 等手写重试逻辑**

- [ ] **Step 3: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/pkg/httputil/... ./backend/internal/embed/... -count=1
git add backend/pkg/httputil backend/internal/embed/openai.go
git commit -m "refactor: introduce RetryableHTTPClient and unify retry logic"
```

---

### Task 7.4: 引入 Makefile 与开发脚本

**Files:**
- Create: `Makefile`
- Modify: `CLAUDE.md`、`README.md`

- [ ] **Step 1: 创建 Makefile 封装常用命令**

```makefile
.PHONY: test backend-test test-test web-build lint up down migrate

backend-test:
	cd backend && GOTOOLCHAIN=local go test ./... -count=1

test-test:
	cd test && GOTOOLCHAIN=local go test ./... -count=1

web-build:
	cd web && pnpm install && pnpm build

lint:
	cd backend && GOTOOLCHAIN=local go vet ./...
	cd test && GOTOOLCHAIN=local go vet ./...

up:
	docker compose -f docker/docker-compose.yml up -d

down:
	docker compose -f docker/docker-compose.yml down -v

migrate:
	for f in migrations/*.sql; do psql "$$POSTGRES_DSN" -f "$$f"; done

run: web-build
	cd backend && GOTOOLCHAIN=local go run .
```

- [ ] **Step 2: 更新 CLAUDE.md / README.md 使用 `make` 命令**

- [ ] **Step 3: Commit**

```bash
git add Makefile CLAUDE.md README.md
git commit -m "chore: add Makefile for common dev/test/run commands"
```

---

### Task 7.5: 包依赖方向检查与文档化

**Files:**
- Create: `docs/superpowers/plans/dependency-rules.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: 使用 `go list -deps` 输出各包依赖，检查并修复以下方向违规**

```bash
cd backend
GOTOOLCHAIN=local go list -deps ./internal/memory/... | grep internal/graph
GOTOOLCHAIN=local go list -deps ./internal/store/... | grep internal/dag
```

执行 Track 2 后上述命令应无输出。

- [ ] **Step 2: 在 `CLAUDE.md` 中补充包依赖规则**

```markdown
### 包依赖规则

- `pkg/types`、`pkg/enums`、`pkg/textutil`、`pkg/jsonutil`、`pkg/httputil`：最底层，不依赖任何 internal 包。
- `internal/store`、`internal/memory`、`internal/model`、`internal/logoutput`：基础设施层，可依赖 `pkg/*`，不可依赖 `internal/graph`、`internal/runtime`、`internal/agent`、`internal/server`。
- `internal/graph`、`internal/dag`、`internal/skill`、`internal/soul`、`internal/watchdog`、`internal/board`、`internal/mailbox`、`internal/cmdqueue`：编排/运行时组件层，可依赖基础设施层与 `pkg/*`，不可依赖 `internal/agent`、`internal/server`、`internal/tui`。
- `internal/runtime`：聚合层，可依赖运行时组件层与基础设施层，不可依赖 `internal/agent`、`internal/server`、`internal/tui`。
- `internal/agent`：Agent 模块门面层，可依赖 `internal/graph`、`internal/runtime`、`internal/store`、`internal/memory`、`pkg/*`，不可依赖 `internal/server`、`internal/tui`、`internal/bootstrap`、`test/*`。对外暴露 `agent.Agent` 接口与 DTO，是所有调用方（server/tui/test）的唯一入口。
- `internal/server`、`internal/tui`、`internal/bootstrap`：入口/适配层，可依赖所有下层包（含 `internal/agent`），但不得直接依赖 `internal/graph`/`internal/runtime` 内部类型——必须通过 `agent.Agent` 接口。
```

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/plans/dependency-rules.md CLAUDE.md
git commit -m "docs: document package dependency direction rules"
```

---

## Track 6: Tests / Config / Migrations / Docs 清理

**目标：** 清理迁移重复，拆分配置，更新文档。测试模块改造（MockLLM 死锁修复、API helper 提取、测试配置模板化、Fixture 拆分）**延期至后续测试模块需求**，本次不涉及。

> **测试模块延期声明**：Task 6.1-6.4 属于测试基础设施改造，与"测试模块作为后期需求"冲突，全部延期。现有 `test/` 目录下的集成测试保持现状，仅作为重构回归验证基线运行（`GOTOOLCHAIN=local go test ./test/... -count=1`），不在本计划范围内修改。后续测试模块需求启动时，基于已稳定的 `agent.Agent` 接口与 `bootstrap.Build` 重新设计测试架构。

### Task 6.1: ~~修复 `MockLLM` 嵌套锁死锁~~（延期）

> **延期**：测试模块后期需求。现有测试若触发该路径会 hang，需调用方避免并发调用 `handle`。后续测试模块重构时修复。

---

### Task 6.2: ~~提取 API 测试公共 Helper~~（延期）

> **延期**：测试模块后期需求。

---

### Task 6.3: ~~模板化测试配置~~（延期）

> **延期**：测试模块后期需求。

---

### Task 6.4: ~~Fixture 职责拆分~~（延期）

> **延期**：测试模块后期需求。

---

### Task 6.5: 迁移文件清理（不删除已存在表，仅去重与归档说明）

**Files:**
- Modify: `migrations/004_memory_write_failures.sql`
- Modify: `doc/项目说明.md`, `README.md`, `CLAUDE.md`
- Test: 手动执行 `for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done`

- [ ] **Step 1: 从 `004_memory_write_failures.sql` 中删除重复创建 `step_count` 列与索引的语句**

`001_init.sql` 已创建，无需在 `004` 中重复。

- [ ] **Step 2: 保留 `memory_write_failures` 表定义（不删除）**

该表为历史机制残留，但当前不启用；删除表属于破坏性变更，本次重构不动。

- [ ] **Step 3: 在 `004` 文件头部添加注释说明当前状态**

```sql
-- memory_write_failures 表为历史机制残留，当前业务代码已不再写入。
-- 本文件仅保留建表语句以兼容旧环境；step_count 列/索引已在 001_init.sql 中创建，此处不再重复。
```

- [ ] **Step 4: 更新 README/CLAUDE/项目说明.md 中的迁移说明为"按编号顺序全部应用"，并补充 `memory_write_failures` 状态说明**

- [ ] **Step 5: 运行迁移脚本验证幂等性**

```bash
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
```

Expected: 全部成功，无报错。

- [ ] **Step 6: Commit**

```bash
git add migrations/004_memory_write_failures.sql README.md CLAUDE.md doc/项目说明.md
git commit -m "chore(migrations): remove duplicate step_count/index in 004 and document legacy table"
```

---

### Task 6.6: 配置分层拆分

**Files:**
- Create: `config/infrastructure.yaml`, `config/agent-policy.yaml`
- Modify: `config/config.yaml`（作为入口合并）、`backend/internal/config/config.go` 加载逻辑
- Modify: `config/roles.yaml`（使用 YAML 锚点减少重复 model_config）
- Test: `backend/internal/config/config_test.go`, `test/...`

- [ ] **Step 1: 将 `config/config.yaml` 拆分为基础设施与策略**

`config/infrastructure.yaml`：postgres / redis / http / logging。
`config/agent-policy.yaml`：agent 行为参数、阈值、特性开关。

- [ ] **Step 2: `config.yaml` 保持兼容，通过程序合并两个文件**

```go
func Load(configPath string) (*Config, error) {
    // 先加载 config.yaml
    // 若存在同目录 infrastructure.yaml / agent-policy.yaml，则合并
}
```

- [ ] **Step 3: `roles.yaml` 使用锚点**

```yaml
model_default: &model_default
  provider: openai
  model: gpt-4o-mini
  api_key: ${OPENAI_API_KEY}
  base_url: ${OPENAI_BASE_URL}

fixed_roles:
  - name: code
    model_config: *model_default
```

- [ ] **Step 4: 运行测试并 commit**

```bash
GOTOOLCHAIN=local go test ./backend/internal/config/... ./test/... -count=1
git add config/infrastructure.yaml config/agent-policy.yaml config/config.yaml config/roles.yaml backend/internal/config/config.go
git commit -m "config: split infrastructure and agent policy configs, use YAML anchors in roles"
```

---

### Task 6.7: 文档权威化与漂移修复

**Files:**
- Modify: `doc/项目说明.md`, `doc/设计文档_v3.md`, `doc/TODO.md`, `doc/test/测试demo.md`

- [ ] **Step 1: 修复 `doc/项目说明.md` 中的迁移文件列表**

按实际文件名列出 `001_init.sql` ~ `006_session_history_meta_memory.sql`，并说明新增 `007_drop_memory_write_failures.sql`。

- [ ] **Step 2: 删除 `设计文档_v3.md` 中已偏离的 §4/§8/§9，或归档为"历史设计"**

- [ ] **Step 3: 清理 `doc/TODO.md` 中已落地条目，补充死信表残留等未闭环项**

- [ ] **Step 4: 删除或重写 `doc/test/测试demo.md`**

- [ ] **Step 5: Commit**

```bash
git add doc/项目说明.md doc/设计文档_v3.md doc/TODO.md doc/test/测试demo.md
git commit -m "docs: update migration list, archive drifted design sections, clean up TODO"
```

---

## 执行顺序与依赖

按 Track 编号顺序执行，原因：

1. **Track 1（Graph）** 无外部依赖，且为后续提供 `pkg/types.ContextPack` 等基础类型。
2. **Track 2（Memory/Storage）** 需要 `pkg/types` 下移后的类型，并依赖 `config.AgentConfig` 结构（可与 Track 3 前半部分并行）。
3. **Track 3（Runtime/Components）** 拆分 `AgentConfig` 后，Track 4/5/6 才能引用新的子配置路径。
4. **Track 4（Agent 模块 API 封装与 Server/TUI 瘦身）** 依赖 Graph 与 Runtime 的稳定接口。本 Track 是本次重构的核心交付：Task 4.1-4.4 定义并实现 `agent.Agent` 接口，Task 4.5+ 在此基础上瘦身。务必先完成 4.1-4.4 再做 4.5+，否则 Server/TUI 仍会直接依赖 graph 内部类型。
5. **Track 5（Frontend）** 依赖 Server 端事件协议稳定（Task 4.7 完成后）。
6. **Track 6（Config/Migrations/Docs）** 仅含配置拆分、迁移清理、文档更新，依赖前面代码结构基本稳定。测试模块相关 Task 6.1-6.4 已延期，不阻塞本 Track。
7. **Track 7（工程化能力补齐）** 跨域收尾，依赖 Track 1-4 的包边界基本清晰。

每个 Track 内部按 Task 编号顺序执行；每个 Task 内部按 Step 顺序执行并运行对应测试。

> **Agent 模块封装的依赖链**：Track 1 (graph 稳定) → Track 3.1-3.2 (config/runtime 稳定) → Track 4.1 (agent.Agent 接口定义) → Track 4.2 (agent.Service 实现) → Track 4.4 (TUI 通过接口调用) → Track 4.5+ (Server 瘦身、wiring 统一)。Task 4.3（测试模块注入）已延期，不阻塞依赖链。
>
> **测试验证基线**：测试模块虽延期改造，但现有 `test/` 集成测试仍作为回归基线运行（`GOTOOLCHAIN=local go test ./test/... -count=1`），验证重构行为保真。若现有测试因重构失败，需修复重构代码而非测试。

---

## 全局验证命令

每个 Task 完成后至少运行对应包测试。每个 Track 完成后运行以下命令：

```bash
# 后端单元 + 集成测试（需 PG/Redis/LLM mock 就绪）
cd D:/data/project/BlockMemoryAgent
GOTOOLCHAIN=local go test ./backend/... -count=1

cd test
GOTOOLCHAIN=local go test ./... -count=1

# 前端类型检查
cd D:/data/project/BlockMemoryAgent/web
npm run type-check || npx vue-tsc --noEmit
npm run build
```

全部 Tracks 完成后运行一次端到端冒烟：

```bash
cd D:/data/project/BlockMemoryAgent
docker compose -f docker/docker-compose.yml up -d
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
cd web && pnpm install && pnpm build && cd ..
GOTOOLCHAIN=local go run ./backend
# 另开终端：访问 http://localhost:10010 并创建会话验证流程
```

---

## 风险与回退策略

| 风险 | 缓解 |
|---|---|
| 大规模重构导致测试长时间失败 | 每个 Task 独立可测试，失败时只回滚该 Task 的 commit。 |
| 配置拆分后 YAML 不兼容 | 保持 `AgentConfig` 嵌入子结构体，旧字段访问方式不变。 |
| 前端事件类型改动影响 SSE 解析 | 先在后端增加 `event_type` 字段并保持旧字段，前端逐步迁移。 |
| 存储层拆分导致 SQL 查询回归 | 每个子 store 保留原 SQL，拆分后再优化；用现有测试覆盖。 |
| SubDomain 封装影响路由理解 | 不删除实现，仅增加注释与统一门面；保留 `types.RoleTypeSubDomain` 常量。 |
| `agent.Agent` 接口设计偏差导致后续 Server/TUI 无法平滑迁移 | Task 4.1 先定义接口，Task 4.2 实现后立即跑全量 `test/api` 与 `test/coding` 验证 DTO 无损映射；若发现接口缺失字段，回滚 4.1 而非在 4.5+ 打补丁。 |
| `agent.Service` 从 SessionManager 抽取逻辑时漏迁 clarify/queue/tempDir 清理 | Task 4.2 Step 2 强制逐项核对：clarify 阈值、队列优先级、终态判定、临时目录清理、事件 append 顺序、快照保存时机。每项写回归测试。 |
| Track 4.1-4.4 未完成就启动 4.5+ 导致 Server 仍依赖 graph 内部类型 | 执行顺序硬约束：4.5+ 必须在 4.1-4.4 全部 merge 后启动。 |
| 行为修复 Task (2.7/3.2/3.4/3.6/4.10) 静默改变基线 | 每个 (行为修复) Task Step 1 先写回归测试固化旧行为，再修复，差异可观测。 |

---

## 交付物清单

- [ ] `docs/superpowers/plans/2026-07-08-blockmemoryagent-refactoring-roadmap.md`（本计划）
- [ ] Track 1 完成后：`internal/graph/base_node.go`、`llm_caller.go`、`guards.go`、`tool_registry.go`、`receivers.go`
- [ ] Track 2 完成后：`internal/store/*_store.go`、`pkg/types/context.go`（`ContextPack` / `Message` / `BuildRequest` 下移；`Episode` 已在 `pkg/types` 无需移动）
- [ ] Track 3 完成后：`internal/config` 子配置、`internal/runtime` Options、`internal/skill/policy.go`、`internal/soul/classifier.go`、`internal/watchdog/estimator.go`、`internal/logger/batch_store.go`、`internal/dag/runner.go`
- [ ] Track 4 完成后（**核心交付**）：`internal/agent/agent.go`、`types.go`、`errors.go`、`service.go`、`service_runner.go`、`internal/server/metrics.go`、`session_http.go`、`stream_http.go`、`control_http.go`、`eventkind/kind.go`、`bootstrap/bootstrap.go`、`internal/tui/*_panel.go`
- [ ] Track 5 完成后：`web/src/api/*.ts`、`web/src/composables/*.ts`、`web/src/utils/date.ts`、`web/src/utils/sessionStatus.ts`、`web/src/components/*.vue`
- [ ] Track 6 完成后（测试模块延期，仅含配置/迁移/文档）：更新后的 `migrations/004_memory_write_failures.sql`、`config/infrastructure.yaml`、`config/agent-policy.yaml`、更新后的文档。测试 helper / mock_agent / 模板化配置 / fixture 拆分均延期至后续测试模块需求。
- [ ] Track 7 完成后：统一错误处理、`internal/logoutput`、通用 `httputil`、根目录 `Makefile`、`docs/superpowers/plans/dependency-rules.md`、更新后的 `CLAUDE.md`

---

## 执行方式选择

**Plan complete and saved to `docs/superpowers/plans/2026-07-08-blockmemoryagent-refactoring-roadmap.md`.**

**Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per Track/Task, review between tasks, fast iteration. REQUIRED SUB-SKILL: superpowers:subagent-driven-development.

**2. Inline Execution** — Execute tasks in this session using superpowers:executing-plans, batch execution with checkpoints for review.

**Which approach would you like?** 若选择 Subagent-Driven，建议从 **Track 1 Task 1.1** 开始逐个交付，每完成一个 Task 汇报并确认后再继续。
