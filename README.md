# BlockMemoryAgent

> 一个基于 **上下文外部化 + 记忆隔离** 的 Go 多 Agent 编排系统。解决你在使用 Claude Code 等工具时遇到的真实问题：上下文膨胀导致模型变笨、无关对话污染当前任务、每次新对话从零开始。

---

## 为什么需要这个项目

使用 Claude Code 等编程助手时，你会遇到三个无法回避的问题：

### 1. 上下文一膨胀，模型就"变笨"

Claude 3.5 Sonnet 标称 200K 上下文，但实际操作中，当对话历史、工具输出、文件内容堆积超过一定阈值后，模型对中间信息的提取准确率急剧下降。不是模型不支持长上下文，而是**长上下文中的有效注意力会衰减**。这导致：
- 明明刚才讨论过的方案，模型突然"忘了"
- 中间穿插的代码审查、文件读取结果，在后期被忽略
- 代码修改时遗漏之前已经确认过的约束

**BlockMemoryAgent 的解法**：不依赖 LLM 记住一切。所有历史对话、工具输出、文件快照都写入外部记忆库（PostgreSQL + Redis + pgvector），LLM 的上下文永远只包含"当前任务 + 按需召回的 relevant 记忆"。无论运行多久，输入 LLM 的上下文总量保持稳定。

### 2. 多轮对话中夹杂不相干内容，污染当前任务

你在修复 CSS 问题时，中途问了一句"帮我查一下这个函数的文档"，然后又回到 CSS 修复。但 LLM 的上下文里有 50% 是函数文档查询的内容，这些无关信息会干扰后续的 CSS 修复决策。

**BlockMemoryAgent 的解法**：会话块隔离（SessionBlock）。每个 DomainAgent 拥有独立上下文块，互不污染。切换话题时，旧话题的记忆被归档（压缩为摘要 + 写入向量库），新话题只加载自己的上下文 + 从归档中按需召回相关片段。

### 3. 使用期限很短，需要频繁开新对话重置上下文

Claude Code 的对话一旦超过几百轮，质量明显下降。你被迫开启新对话，但新对话里项目的所有上下文、代码风格、你的偏好都丢失了。

**BlockMemoryAgent 的解法**：跨会话记忆持久化。每次对话结束后，系统自动提取关键知识写入长期记忆库。新对话启动时，自动检索并注入相关历史知识。项目约定、代码风格、你的偏好——这些只会在第一次讨论时建立，之后永久可用。

**一句话总结**：BlockMemoryAgent 不是 Claude Code 的替代品，而是**在 Claude Code 的痛点上，做它做不到的事情——长时稳定运行、话题隔离、跨会话记忆**。

---

## 核心特性

- **四层 Agent 层级**：MetaAgent → DomainAgent → SubDomainAgent → Assistant（SubDomain 自适应触发，仅复杂跨层任务启用）
- **5 路径智能路由**：规则层（零 LLM）+ LLM 兜底 + 安全兜底，80% 简单任务不进入四层编排（`router.go`）
- **上下文恒定控制**：无论运行多久、多少轮对话，输入 LLM 的上下文始终控制在配置阈值内，通过外部化记忆 + 按需召回实现
- **会话块隔离（SessionBlock）**：每个 DomainAgent 拥有独立上下文块，切换话题时旧话题归档，新话题只加载 relevant 记忆，互不污染
- **4 阶段记忆管线**：Write（重要性评分 + 话题绑定）→ Compress（两级：Raw → Standard）→ Search（多信号：pgvector 语义 + 实体重叠 + 因果链 + 时间衰减）→ Assemble（4 段 TokenBudget 组装）
- **跨会话记忆**：关键知识在会话结束后持久化，新会话自动召回；MetaMemory 轻量调度记忆跨会话保留
- **Leaf 执行用 go-kratos Blades**：Assistant 真正干活走 Blades `Agent` + 原生 function-calling（ReAct 工具循环）；上层四层图状态机自研
- **严格启动**：config/roles/env/soul/skills 任一配置文件缺失，或 PG/Redis/LLM 后端不可达，启动即失败并明确报错
- **双入口 + 可观测**：HTTP Web UI（Vue 3 SPA）+ bubbletea TUI；SSE 实时推送思考/工具/token 事件；后端暴露 `/api/metrics` 输出 Prometheus 格式运行时指标
- **工程防护**：完成门控（LLM 声称写文件时必须见到成功 WriteFile）、死循环渐进告警、LLM 调用追踪 + 超时追踪、同步记忆写入

---

## 架构

```
┌─────────────────────────────────────────────────────────────┐
│  MetaAgent (Layer 1, 调度中枢)                                │
│    ClassifyTask 5 路径路由 → 按路径分派 → 监控 → 汇总          │
│    路径: direct_tool(0层) / direct_assistant(1层) /           │
│          create_domain(2层) / multi_domain(2层×N) /           │
│          full_four_layer(4层, 启用 SubDomain)                 │
├─────────────────────────────────────────────────────────────┤
│  DomainAgent (Layer 2, 领域管理)                              │
│    管理单领域上下文 → (可选)生成 ExecutionPlan → 派发助手 → 汇总│
│    (复杂跨层任务触发 SubDomainAgent)                          │
├─────────────────────────────────────────────────────────────┤
│  SubDomainAgent (Layer 2.5, 自适应启用)                       │
│    DomainAgent 的并行子分支（跨子领域边界检测）                │
├─────────────────────────────────────────────────────────────┤
│  Assistant (Layer 3, 任务执行)                                │
│    固定角色(code/ui/test/doc...) 或 动态创建                   │
│    Blades Agent + function-calling 真正干活（7 个内置工具）    │
│    执行后 Self-Reflection 评估，不达标带反馈重试一次           │
├─────────────────────────────────────────────────────────────┤
│  Memory Layer（4 阶段管线，独立于 Blades）                    │
│    Write → Compress(4级) → Search(多信号) → Assemble(TokenBudget)│
│    Snapshot: Redis 热加载 + Postgres 持久化                   │
├─────────────────────────────────────────────────────────────┤
│  Storage Layer（三层存储）                                    │
│    PostgreSQL（持久记忆/快照/知识库/会话历史）                 │
│    Redis（工作区热缓存/Stream/快照 TTL）                      │
│    pgvector（向量检索）                                       │
└─────────────────────────────────────────────────────────────┘
```

### 记忆管线详解

Blades `Session` 仅管叶子 ReAct 短上下文，长期记忆全自研，4 阶段独立于 Blades：

| 阶段 | 文件 | 职责 |
|---|---|---|
| Write | `memory/write.go` | Episode 重要性评分 + 话题绑定 |
| Compress | `memory/compress.go` | 2 级压缩：Raw → Standard |
| Search | `memory/search.go` | 多信号相关性：pgvector 语义 + 实体重叠 + 因果链 + 时间衰减 |
| Assemble | `memory/assembler.go` | 4 段上下文组装（System/TopicGlobal/SharedState/PrivateMemory）+ TokenBudget |
| Snapshot | `memory/snapshot.go` | Redis 热加载 + Postgres 持久化，归档幂等兜底（`archived` atomic 标记 + 切块前补写） |

**记忆写入机制**：Agent 执行后通过 `CallbackHandler` 显式同步写入 Episode 与 Snapshot。`memory/callback.go` 不再使用队列、后台 worker、重试或死信表，写入失败直接记录日志。

**压缩层级**：
- `LevelRaw` — 完整原始记录
- `LevelStandard` — 摘要（丢弃 FullObservation）

> 向量检索默认使用 `embed.PseudoEmbed`（sha256 hashed bag-of-tokens 伪向量，零外部依赖），已在 `config/roles.yaml` 的 `embed` 段支持接入真实 Embedding 模型（OpenAI 兼容端点，如 text-embedding-3 / bge-m3 / 豆包 Embedding）。详见 `doc/项目说明.md` §6 与 `doc/TODO.md` P3-3。

---

## 快速开始

### 依赖

- Go 1.25（机器默认 Go 较旧时需 `GOTOOLCHAIN=local`）
- PostgreSQL 14+（含 pgvector 插件，记忆持久化）
- Redis 7+（短期记忆热缓存）
- 至少一个大模型 API Key（OpenAI 兼容 / Anthropic / 本地 Ollama）

> 所有依赖必须可用：config/roles/env/soul/skills 配置文件须存在，PostgreSQL、Redis、LLM 后端须可达。任一缺失或不可达，服务器启动失败并明确报告。

### 安装与配置

```bash
go mod download
cp .env.example .env  # 填 OPENAI_API_KEY / OPENAI_BASE_URL / POSTGRES_DSN / REDIS_ADDR
```

编辑 `config/roles.yaml` 配置各角色模型（MetaAgent / DomainAgent / LightweightModel / FixedRole / dynamic_templates 均可独立配置 `model_config`）。

### 运行

```bash
# 1. 启动 PostgreSQL + Redis
docker compose -f docker/docker-compose.yml up -d

# 2. 应用数据库迁移（按顺序应用 migrations/*.sql 中所有文件）
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
# 注：004_memory_write_failures.sql 为历史遗留表，step_count 列与幂等索引
#     已在 001_init.sql 中创建，应用 004 时不会重复创建或删除现有数据。

# 3. （首次）构建 Web UI
cd web && pnpm install && pnpm build   # 产物到 web/dist/

# 4. 启动 HTTP 服务（主入口）
go run ./backend

# 其他入口
go run ./backend/cmd/tui              # bubbletea TUI
go run ./backend/cmd/demo             # CLI 三层流演示
go run ./backend/cmd/memory-console   # 记忆检查控制台
```

> **关于自动迁移**：`main.go` 把所有装配委托给 `testserver.BuildHandler`，后者启动时会幂等 `Ensure*` 自动建 `session_history` / `session_events` / 001 记忆表。但 `005_session_logs` 与 `006_session_history_meta_memory`（`meta_memory` 列）不在自动迁移覆盖范围内，**必须按顺序应用 `migrations/*.sql` 中所有文件**，否则 `persistHistory` 写入会失败。`004_memory_write_failures.sql` 为历史遗留死信表，应用时不会创建/删除任何业务数据。

flags：`-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`

---

## 项目结构

```
.
├── backend/                    # Go 主模块 (github.com/blockmemory/agent/backend)
│   ├── main.go                 # HTTP 入口，委托 testserver.BuildHandler 装配
│   ├── cmd/                    # demo / tui / memory-console
│   ├── internal/
│   │   ├── graph/              # 四层图编排（核心，5 路径路由 + 状态机）
│   │   ├── memory/             # 4 阶段记忆管线
│   │   ├── model/              # Blades 模型工厂 + LLM 追踪/超时
│   │   ├── store/              # PostgreSQL + Redis 存储
│   │   ├── runtime/            # 运行时聚合（board/mailbox/skill/soul/watchdog）
│   │   ├── board/ mailbox/ skill/ soul/ watchdog/  # 各运行时组件
│   │   ├── server/             # HTTP API + SSE + 会话管理
│   │   ├── testserver/         # 可复用 wiring（生产与测试共用）
│   │   └── config/ tui/ retriever/ embed/ ...
│   └── pkg/types/              # 核心类型（Role/State/SessionBlock/Episode/Skill...）
├── config/                     # config.yaml / roles.yaml / skills.yaml / soul.md
├── migrations/                 # SQL schema (001-006)
├── web/                        # Vue 3 + Vite SPA
├── test/                       # 集成测试（独立模块）：api / coding / tui / fixtures
├── doc/                        # 设计文档（v3 为准，项目说明.md 为权威实现说明）
└── docker/docker-compose.yml   # PG + Redis 一键起
```

---

## 核心模块

### 1. 智能路由（`internal/graph/router.go`）

5 路径精确路由，决策顺序：规则层（零 LLM）→ LLM 兜底（轻量模型）→ 安全兜底（`create_domain`，零回归）：

| 路径 | 层级 | 场景 |
|---|---|---|
| `direct_tool` | 0 层 | 纯 QA / 单工具请求（读文件、跑命令、查天气），MetaAgent 自跑工具循环 |
| `direct_assistant` | 1 层 | 单领域简单任务（修 CSS、改文案），MetaAgent 直接建助手执行 |
| `create_domain` | 2 层 | 单领域复杂任务，拆领域不启用 SubDomain |
| `multi_domain` | 2 层 × N | 多领域并行 |
| `full_four_layer` | 4 层 | 完整编排，启用 SubDomain |

**效果**：80% 请求不走四层编排；规则层零 LLM 成本覆盖多数简单任务。

### 2. 图执行引擎（`internal/graph/three_layer_graph.go`）

`ThreeLayerGraph.Invoke()` 是状态机循环（max 200 步），节点返回 `NextAction ∈ {Continue, Switch, Escalate, Finish}` 控制流转，`CallStack` 支持嵌套调用，`SessionBlock` 做领域级上下文隔离。

### 3. Leaf 执行：Blades 工具循环（`internal/graph/llm_tools.go` + `blades_tools.go`）

Assistant 走 Blades 原生 function-calling：`blades.NewAgent` + `WithTools` + `WithMaxIterations(tool_call_max_rounds)`，`Agent.Run` 内部跑 ReAct（`model.Generate` → 检测 ToolPart → `tool.Handle` → 回灌 → 直到无 tool call 或达上限）。默认 40 轮，并在 `blades_agent_runner.go` 中用 `loopDetector` 检测重复调用/空转，提前优雅退出，避免无限循环。

**7 个内置工具**：`ReadFile` / `WriteFile` / `ListDir` / `RunCommand` / `SearchInFiles` / `HTTPGet` / `HTTPPost`，用 `tools.NewFunc` 自动生成 JSON schema，复用 `ToolExecutor` 沙箱实现。

**完成门控**：写文件类任务必须见到成功的 `WriteFile` 才算完成，防 LLM 幻觉"声称已写"。

### 4. 记忆模块（`internal/memory/`）

4 阶段管线（见上文"记忆管线详解"）。`ContextAssembler` 在 Assistant 调用 LLM 前注入私有记忆与全局知识，`SnapshotManager` 做 Redis 热加载 + PG 持久化，`Compressor` 在 Watchdog 触发时压缩。

### 5. 运行时聚合（`internal/runtime/runtime.go`）

单一 `Runtime` 结构注入：`board.Manager`（任务看板）/ `mailbox.Mailbox`（跨 Agent 消息）/ `skill.Registry`（按领域装配 Skill 子集）/ `soul.Loader`（人格 + 温度策略）/ `watchdog.Watchdog`（上下文看门狗）。

---

## TUI 界面

参考 Claude Code 的简洁终端交互，编程场景优化：主对话区 + 底部状态栏（plan 进度 + 工具状态）+ Tab 切换右侧 Agent 面板 + 记忆召回指示 `🧠 recalled: ...`。工具输出默认折叠（ReadFile/SearchInFiles/ListDir/HTTPGet/HTTPPost 仅显示路径，WriteFile/RunCommand 保留 output，错误一律展示）。

---

## 可观测性与指标

- **健康检查**：`GET /api/health` 返回 Postgres / Redis / LLM 连通性 JSON。
- **Prometheus 指标**：`GET /api/metrics` 暴露以下运行时指标（`text/plain`）：
  - `go_goroutines`：当前 goroutine 数
  - `go_memory_alloc_bytes`：已分配内存字节数
  - `bma_sessions_total`：内存中会话总数
  - `bma_llm_calls_total`：LLM 调用总次数
  - `bma_llm_timeouts_total`：LLM 超时总次数
- **结构化日志**：所有 Agent 关键事件写入 `session_logs` 表，可通过 `GET /api/sessions/{id}/logs` 按 session/agent/level 查询；stderr 同时输出 JSON 格式日志。

## 与 Claude Code 的对比

| 能力 | Claude Code | BlockMemoryAgent |
|------|-------------|------------------|
| 终端编程界面 | ✅ 优秀 | ✅ 参考 Claude Code 设计 |
| 代码编辑 / 命令执行 | ✅ 成熟 | ✅ 工具调用实现（7 个内置工具） |
| **长时稳定运行** | ❌ 几百轮后质量下降 | ✅ 上下文恒定，理论上无限运行 |
| **话题隔离** | ❌ 所有对话在一个上下文 | ✅ SessionBlock 隔离，互不污染 |
| **跨会话记忆** | ❌ 新对话从零开始 | ✅ 自动提取关键知识，新会话召回 |
| **记忆可观测** | ❌ 黑盒 | ✅ Web/TUI 均可查看记忆检索过程 |
| **自托管 / 模型选择** | ❌ 闭源，固定 Anthropic | ✅ 开源本地运行，按角色分层选型 |

**定位**：BlockMemoryAgent 不是 Claude Code 的替代品，而是**在 Claude Code 做不到的事情上补强**。可以配合使用：Claude Code 负责日常快速编辑，BlockMemoryAgent 负责需要持续多天、多话题并行、跨会话记忆保持的长任务。

---

## 技术栈

- **语言**：Go 1.25（需 `GOTOOLCHAIN=local`）
- **Agent leaf 框架**：go-kratos Blades v0.5.0（`ModelProvider`/`Agent`/`tools.Tool`）
- **模型适配**：原生多 Provider 分发（`openai` / `anthropic` / `ollama`），`config/roles.yaml` 每个角色独立配置 `model_config.provider`；OpenAI 兼容后端（DeepSeek / 豆包 / 硅基流动等）统一用 `provider: openai`
- **上层编排**：自研 `ThreeLayerGraph` 状态机（不依赖任何框架的图抽象）
- **存储**：PostgreSQL 14+ + pgvector（私有记忆/快照/知识库/会话历史/会话事件/会话日志）+ Redis 7+（工作区/快照热加载/Stream）
- **Web**：Vue 3 + Vite + Tailwind
- **TUI**：bubbletea + lipgloss
- **HTTP**：标准库 `net/http`，`:10010`
- **配置**：YAML + `.env`（`${VAR:default}` 插值）

---

## 扩展方向

见 `doc/TODO.md` P3 远期项：
- **真实 Embedding**：已接入 OpenAI 兼容端点，配置从 `config.yaml` 迁移到 `roles.yaml`（P3-3）；伪向量仍保留为默认零依赖方案
- **编程工具扩展**：Git 工具已落地（P3-2），测试运行器、浏览器自动化、自定义工具注册待后续
- **MCP / Computer Use / RAG / LLM Wiki 插件**：接口预留，实现后做（P3-5）
- **模型分层可观测性**：已支持按 meta/domain/lightweight/assistant/other 聚合的 token 统计（P3-7），前端配置页待后续（P3-4）
- **多协议模型接入**：原生 OpenAI / Anthropic / Ollama 协议已落地（P3-6），通过 `provider` 字段区分

---

## 文档索引

| 文档 | 内容 |
|------|------|
| `doc/项目说明.md` | **权威实现说明**：目录结构、核心架构、代码阅读顺序、关键设计决策 |
| `doc/设计文档_v3.md` | 设计愿景稿（顶部有状态声明，与代码偏离处以项目说明.md 为准） |
| `doc/TODO.md` | 已完成 / 待完成任务清单与优先级依赖 |
| `doc/TUI设计文档.md` | TUI 界面设计与交互规范 |
| `CLAUDE.md` | Claude Code 协作指引 |

---

## License

MIT
