# BlockMemoryAgent

> 一个基于 **上下文外部化 + 记忆隔离** 的 Go 多 Agent 编排系统。核心执行架构：单一 ReAct 主循环 + `call_sub_agent` 异步子 Agent 分发，配事件流记忆、三层存储（PostgreSQL + Redis + pgvector）与 HTTP Web UI / bubbletea TUI 双入口。

---

## 为什么需要这个项目

使用 Claude Code 等编程助手时，你会遇到三个无法回避的问题：

1. **上下文一膨胀，模型就"变笨"**——长上下文中有效注意力会衰减，中间信息被"稀释"。
2. **多轮对话夹杂不相干内容，污染当前任务**——所有历史挤在一个线性上下文里。
3. **对话一长就被迫开新会话，项目上下文全丢**——没有跨会话记忆持久化。

BlockMemoryAgent 的应对思路：不依赖 LLM 记住一切。子 Agent 各自持有独立的 ReAct 上下文（互不污染），执行过程写入事件流记忆，会话历史/事件持久化到 PostgreSQL。其中"跨会话知识自动召回注入 prompt"仍是演进方向（见 `doc/TODO.md`），完整设计愿景见 `doc/设计文档_v3.md`。

---

## 核心特性

- **单一 ReAct 主循环**：MetaAgent 跑 LLM → 工具 → 结果循环（默认上限 50 轮），直到输出纯文本回答
- **层级即调用栈**：`call_sub_agent(role_id, task)` 起 goroutine 跑子 Agent，子 Agent 完成后把摘要推 Mailbox，父 Agent 每轮 LLM 前 Drain 邮箱注入上下文——任意深度递归，无图状态机
- **历史压缩**：每 `summarize_every` 步（默认 5）`compressHistory` 把中段历史压成"系统前缀 + 用户目标 + 摘要 + 最近 K 条"；assistant 入史副本截断超长工具入参（单值 2000 runes），滑动窗口/压缩下刀只避开孤立 tool 结果（不锚 user 边界），防 token 爆炸、单轮 input 爆预算与上下文塌缩
- **权威 Agent 树**：`internal/domain/orchestrator/tree.go` 维护派发树快照，HTTP 暴露 `GET /api/sessions/{id}/tree` 读子 Agent 节点状态 + `POST /api/sessions/{id}/agents/{aid}/cancel` 取消子 Agent（补 ReAct 重构后丢失的 introspect/cancel 能力）；`call_sub_agent` 同父 Agent 下同领域重复派发查树快照自动去重；暂停态（paused_on_child/awaiting_clarify）会话可取消
- **块记忆事实提取**：子 Agent 完成后调轻量模型提取 1-5 条关键事实，每条单独向量化落 KnowledgeRecord，替代原始 result.Text 整段落库；提取失败自动回退原始保存
- **共享记忆**：`WriteSharedMemory` 工具让主 Agent 把关键上下文（文件路径/行号/函数签名/验收标准）写入 `sharedKV`，子 Agent 自动读取，避免重读全文件；`task` 入参 2000 runes 上限强制规格走共享记忆
- **验证闭环编排器**：`verifyloop` 原生驱动"产出 -> 自测 -> 修正 -> 上级统一测试"状态机，`Verifier`/`Fixer`/`Reporter` 三接口解耦，`PlanConfirmVerifier` 支持"测试方向不明确 -> 列方案 -> 产出方确认 -> 符合才自测"前置
- **角色工具白名单**：`NewToolRegistryAdapterWithFilter` 按角色限制可调工具集；MetaAgent 仅 `call_sub_agent` + `WriteSharedMemory` + `HTTPGet` 防越位，DomainAgent 开放完整权限承担上下文采集 + 任务拆分 + 派发执行
- **热插拔插件系统**（[设计文档](doc/设计文档_插件范式.md)）：MCP 外部插件（stdio 子进程 / streamable HTTP / docker 容器）、Claude/Codex 插件包（`.mcp.json` + `SKILL.md`）与 Docker 长驻服务统一挂进 `tool.Registry` 或生命周期管理，运行中 enable/disable 下一轮迭代即生效；三个初始插件全部容器化：`web_search`（firecrawl 自托管栈，默认开）、`computer_use`（Xvfb 虚拟桌面，默认关，全部工具接审批守卫链）、`open_design`（画图设计台 service 插件，默认关）；管理 API：`/api/plugins`（list/get/enable/disable/reload），配置 `config/plugins.yaml` + `config/plugins.d/`，部署见下文「插件（Docker 部署）」
- **14 个内置工具**：文件/命令（ReadFile/WriteFile/ListDir/RunCommand/SearchInFiles）、HTTP（HTTPGet/HTTPPost）、Git（GitDiff/GitStatus/GitLog/GitBlame）、共享内存（WriteSharedMemory）、Agent 通信（call_sub_agent/send_message），统一经沙箱守卫
- **两段事件流记忆**：`Write` 追加事件（tool_call / call_sub_agent / sub_agent_summary / answer），`Assemble` 在 LLM 调用前注入最近 N 条作为上下文；无压缩、无 RAG
- **会话持久化，默认全新启动**：会话历史（goal/summary/工具结果）与事件流写入 PostgreSQL；每次启动默认是全新会话列表，`agent.restore_sessions: true` 时才恢复最近 50 个会话到内存
- **LLM 调用全链路日志**：`sessionLogger` 把每次 LLM I/O 写 `session_logs` 表，`QueryKindLogs` 按会话回看完整调用链；`LiveEventTokenUsage` 实时推送 token 用量；DeepSeek V4 缓存模式与 `reasoning_content` 字段适配
- **结构化日志**：所有 Agent 关键事件写 `session_logs` 表并按 session/agent/level 可查；各组件注入 `*logger.Logger`，错误类日志真实输出 `[ERRO]`；文件日志按天分割（`logs/backend/`、`logs/tui/`）
- **严格启动**：config/roles/env/soul/skills 任一配置文件缺失，或 PG/Redis/LLM 后端不可达，启动即失败并明确报错
- **双入口 + 可观测**：HTTP Web UI（Vue 3 SPA）+ bubbletea TUI；SSE 实时推送事件；`/api/metrics` 输出 Prometheus 格式指标
- **可选 DAG 调度**：`dag_enabled` 开启后按简易 cron（`Ns/Nm/Nh`）定时触发任务流程（默认关闭）

---

## 架构

```
User
 │
 ▼
MetaAgent (ReAct loop, internal/agent/react_agent.go)
 │  LLM.Generate(system prompt + history + tools)
 │  ├─ 工具调用：ReadFile / WriteFile / RunCommand / GitDiff / ...
 │  └─ call_sub_agent(role_id, task) ──► goroutine (domain/subagent)
 │                                      │
 ▼                                      ▼
回答用户                        子 Agent (独立 ReAct loop)
                                    │
                                    ├─ 工具调用
                                    └─ 完成后摘要 ──► Mailbox ──► 父 Agent 下轮注入

Memory:  domain/memory 两段事件流（Write → Assemble）
Storage: PostgreSQL（会话历史/事件/日志/知识库）+ Redis（工作区/快照）+ pgvector（保留）
Wiring:  bootstrap.Build（HTTP 服务 / TUI / 集成测试共用同一份装配）
```

---

## 快速开始

### 依赖

- Go 1.25（机器默认 Go 较旧时需 `GOTOOLCHAIN=local`）
- PostgreSQL 14+（含 pgvector 插件）
- Redis 7+
- 至少一个大模型 API Key（OpenAI 兼容 / Anthropic / 本地 Ollama）

> 所有依赖必须可用：配置文件须存在，PostgreSQL、Redis、LLM 后端须可达。任一缺失或不可达，启动失败并明确报告。

### 安装与配置

```bash
cp .env.example .env  # 填 OPENAI_API_KEY / OPENAI_BASE_URL / POSTGRES_DSN / REDIS_ADDR
```

编辑 `config/roles.yaml` 配置各角色模型（meta_agent / domain_agent / lightweight_model / fixed_roles / dynamic_templates 均可独立配置 `model_config`，provider 支持 `openai-chat`（别名 `openai`）/ `openai-responses` / `anthropic` / `ollama`）。

### 运行

```bash
make up       # docker compose 启动 PostgreSQL + Redis
make migrate  # 应用 migrations/*.sql（需 POSTGRES_DSN）
make run      # 构建前端并启动 HTTP 服务（:10010）
```

等价的原始命令：

```bash
docker compose -f docker/docker-compose.yml up -d
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
cd web && pnpm install && pnpm build   # 首次构建，产物到 web/dist/
go run ./backend

# 其他入口
go run ./backend/cmd/tui              # bubbletea TUI
go run ./backend/cmd/memory-console   # 记忆检查控制台（SSE 订阅）
```

> **关于数据库迁移**：`bootstrap.Build` 启动时会幂等执行 `Ensure*Schema` 自动建表（session_history 含 meta_memory 列、session_events、session_logs、dag_jobs、001 记忆/知识表），全新数据库可直接启动；仍建议按顺序应用 `migrations/*.sql` 以保持索引等细节一致。`004_memory_write_failures.sql` 为历史遗留死信表，其 step_count 列与索引已在 001 中。

flags：`-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`

### 插件（Docker 部署）

三个初始插件全部容器化运行，新机器首次部署：

```bash
make plugins-up      # 联网搜索数据面：firecrawl 自托管栈（api :3002，免 API Key）
make plugins-build   # 构建/拉取三个插件镜像：
                     #   bma/firecrawl-mcp:local（web_search，本地构建）
                     #   bma/computer-use-mcp:local（computer_use，本地构建，含 Xvfb/noVNC 虚拟桌面）
                     #   ghcr.io/nexu-io/od:latest（open_design 画图台，直接拉取）
```

启用方式（`config/plugins.yaml`）：

- **web_search**：默认 `enabled: true`，后端启动即自动起容器可用（依赖 firecrawl 栈已起）。
- **computer_use** / **open_design**：默认 `enabled: false`，改配置为 `true` 随启动自动起，或运行时热启用 `POST /api/plugins/computer_use/enable`、`POST /api/plugins/open_design/enable`。open_design 需先在 `.env` 填 `OD_API_TOKEN`。

入口与验证：

- `GET /api/plugins` 查看插件状态；open_design UI：`http://localhost:7456`（Basic 认证 `open-design` / `OD_API_TOKEN`）；computer_use 虚拟桌面观察口：`http://localhost:6081`（noVNC，操作发生在容器内 Xvfb 虚拟桌面，不控制宿主机）。
- 同机多个 BMA 实例（如 tui.exe + headless 后端）不要同时 enable 带端口映射的同一插件（7456/6081 宿主端口冲突）。
- `docker-compose.firecrawl.yml` 中 redis/rabbitmq 走 `docker.m.daocloud.io` 加速前缀（Docker Hub 不可达环境的 workaround）；网络正常时可去掉前缀。

---

## 项目结构

```
.
├── backend/                    # Go 主模块 (github.com/blockmemory/agent/backend)
│   ├── main.go                 # HTTP 入口（flag/日志/监听，装配委托 bootstrap.Build）
│   ├── cmd/                    # tui / memory-console
│   ├── internal/
│   │   ├── agent/              # ReAct 主循环 + Agent facade + 会话生命周期
│   │   ├── domain/             # tool（注册表+内置工具）/ role / memory（事件流）/ subagent / verifyloop / orchestrator
│   │   ├── bootstrap/          # 统一依赖装配（生产/TUI/测试共用）+ fact_extractor（块记忆事实提取 LLM）
│   │   ├── model/              # Blades 模型工厂 + LLM 追踪/超时
│   │   ├── store/              # PostgreSQL（领域子存储）+ Redis
│   │   ├── logger/ logging/    # 结构化日志 + 文件按天分割
│   │   ├── runtime/            # 运行时聚合（board/mailbox/skill/soul/watchdog/cmdqueue）
│   │   ├── dag/ cmdqueue/      # DAG 调度（默认关）/ 用户指令队列
│   │   ├── server/             # HTTP API + SSE + 会话管理
│   │   ├── tui/ testserver/    # bubbletea TUI / 测试用薄封装（委托 bootstrap）
│   │   └── config/ embed/ retriever/ plugins/ computeruse/ ...
│   └── pkg/                    # types / enums / config / 工具包
├── config/                     # config.yaml / roles.yaml / skills.yaml / soul.md
├── migrations/                 # SQL schema (001-006)
├── web/                        # Vue 3 + Vite SPA
├── test/                       # 集成测试（独立模块）：api / coding / tui / fixtures
├── doc/                        # 设计文档
└── docker/docker-compose.yml   # PG + Redis 一键起
```

---

## 核心模块

### 1. ReAct 主循环（`internal/agent/react_agent.go`）

`ReActAgent.RunWithHistory()`：prepend system prompt → `model.Generate`（带工具 schema）→ 有 `ToolPart` 则经 `ToolRegistry.Dispatch` 执行并回灌结果 → 直到纯文本回答或达到上限（默认 50 轮）。每轮 LLM 前 Drain Mailbox，把子 Agent 摘要注入历史并写入记忆事件。 每 `summarize_every` 步（默认 5）`compressHistory` 压缩中段历史防 token 爆炸；assistant 消息入史时 `truncateToolCallInputsForHistory` 截断超长工具入参（单值 2000 runes，派发执行仍用原始入参），防 WriteFile 全文逐轮重发耗尽 token 预算；`windowMessages`/`compressHistory` 下刀只避开孤立 tool 结果、不锚 user 边界，防工作型历史上下文塌缩。`sessionLogger` 写 LLM I/O 到 `session_logs`，`QueryKindLogs` 按会话回看。

### 2. 异步子 Agent 分发（`internal/domain/subagent/dispatcher.go`）

`call_sub_agent` 工具起 goroutine 跑子 ReActAgent，立即返回 `sub_agent_id`；子 Agent 完成后摘要推 Mailbox。角色权限由 `roles.yaml` 的 `can_be_called` / `parents` 控制；`domain` 字段定制 DomainAgent 展示名；`task` 入参 2000 runes 上限强制规格走 `WriteSharedMemory`。`WithSharedMemory` 注入共享 KV 只读视图，`buildSharedPrefix` 自动读取主 Agent 写入的关键上下文并做 Layer 3 mtime 校验（文件改动后 KV 失效）。父会话终结保护：有未决子 Agent 时阻塞等待，防迟到 mailbox 消息丢失。`WithFactExtractor` 注入事实提取器，`saveBlockMemory` 先调轻量模型提取关键事实再落库；`WithTree` 注入权威 Agent 树，派发时 Register/SetCancel/Finish。派 domain 角色时 `findPendingDomainSibling` 查树快照去重（同父同 domain Running/Paused 即拒，不烧派发配额）；递归派发的子 Agent 注入 `WithPendingChildrenChecker`，终答前须等自己的子 Agent 回传。

### 3. 权威 Agent 树（`internal/domain/orchestrator/tree.go`）

ReAct 重构后子 Agent 由 Dispatcher 直接 goroutine 创建，不注册为 `AgentInstance`，原"层级即调用栈"丢失 introspect/cancel 能力。补 `Tree` struct 作元数据层：Dispatcher 派发前 `Register` 节点（ID/ParentID/Role/Domain/Task/Started/Status=Running）+ `SetCancel` 绑定 cancel func，完成时 `Finish` 写终态（Done/Failed/Cancelled + Summary/Err）。HTTP 暴露 `GET /api/sessions/{id}/tree` 返回快照，`POST /api/sessions/{id}/agents/{aid}/cancel` 调 cancel func 取消子 Agent。`context.CancelFunc` 幂等，与 goroutine `defer cancel()` 重复调用安全。树经 PG 持久化（`agent_tree_nodes` 表），`TreeFor` lazy init 时 `LoadNodes` 恢复节点元数据，TUI 面板直读 `Tree()` 快照（旧事件流派生 `deriveSubAgentNodes` 已删）。`call_sub_agent` 派 domain 角色时基于树快照做同领域重复派发去重（`findPendingDomainSibling`）。`verifyloop.ExecuteChild` 同步路径暂不入树（phase 2）。`agent.Agent` 接口扩展 `Tree`/`CancelAgent` 两方法。

### 3. 工具域（`internal/domain/tool/`）

`Registry.Schema()` 为每个工具生成 JSON schema，`Registry.Dispatch` 按名执行；`executor.go` + `sandbox.go` + `guards.go` 提供路径逃逸检测与命令黑名单。`NewToolRegistryAdapterWithFilter` 按角色限制可调工具集（MetaAgent 仅 `call_sub_agent` + `WriteSharedMemory` + `HTTPGet`，DomainAgent 开放完整权限）。`WriteSharedMemory` 工具写 `sharedKV`，仅 MetaAgent 白名单可用。`ReactService.SetModelProvider` 支持注入 mock provider 跑测试。

### 4. 记忆（`internal/domain/memory/`）

两段事件流：`Write` 追加 `MemoryEvent`，`Assemble` 注入最近 N 条。默认 `InMemoryStore`（进程内存），Postgres 事件持久化是开放项（`doc/TODO.md` #3）。`InMemoryKV` 提供共享 KV 记忆（主 Agent 写、子 Agent 读）。`WithMaxEventsPerAgent` 防事件流无限增长。遗留 `internal/memory/write.go` 仅负责 Episode 持久化，不在 ReAct 热路径上。

### 5. 验证闭环编排器（`internal/domain/verifyloop/`，默认关闭）

原生状态机驱动"产出 -> 自测 -> 修正 -> 上级统一测试"，不依赖主 Agent 提示词。三接口解耦：`Verifier`（SelfTest/UnifiedTest 返回结构化 `Verdict`）+ `Fixer`（修正产出）+ `Reporter`（上报结果）。`PlanConfirmVerifier` 可选接口实现"测试方向不明确 -> 列方案 -> 产出方确认 -> 符合才自测"前置。`maxRounds` 防死循环。bootstrap 按 `verification_role_pairs` 注册 `OnSubAgentDone` 钩子，`assistant_self_test_enabled` / `domain_self_test_enabled` 控制开关。`ComputerUseVerifier`/`CLIVerifier`/`MCPVerifier` 留扩展口子。

### 5. 日志（`internal/logger/` + `internal/logging/`）

zerolog 实现，console/json 两种格式；error 级别可附调用栈（`logging.stack_enabled`）。`BatchingLogStore` 批量写 `session_logs` 表。各组件通过 `SetLogger` 注入；`store` 包因循环导入约束使用窄接口 `store.Logger`。入口把标准库 `log` 输出兜底转发到统一格式。`sessionLogger` 把每次 LLM 调用 I/O 写 `session_logs`，`LiveEventTokenUsage` 实时推送 token 用量。

### 6. 运行时聚合（`internal/runtime/runtime.go`）

单一 `Runtime` 持有 Boards / Mailbox / Skills / Soul / Watchdog / CmdQueue。当前接线现状：**Mailbox 是活的**（子 Agent 摘要回灌主循环）；Board → TUI 面板与 board API；Skill 池 → `/api/skills` 查询（prompt 装配未接入 ReAct）；Soul → 加载并经 API 暴露（未注入 prompt）；Watchdog → 阈值经 Query API 暴露（未在主循环执行，历史压缩在 ReAct 层部分缓解）；CmdQueue → enqueue/interrupt 端点。见 `doc/TODO.md`。

### 8. DAG 调度（`internal/dag/`，默认关闭）

`agent.dag_enabled: true` 后，调度器按简易 cron（`Ns`/`Nm`/`Nh`）轮询 `dag_jobs` 表，按 `depends_on` 依赖把 task 派发为新会话。HTTP 端点：`GET/POST /api/dag`、`GET/DELETE /api/dag/{id}`、`POST /api/dag/{id}/trigger`、`GET /api/dag/running`。

---

## 可观测性与指标

- **健康检查**：`GET /api/health`（公开，其余端点在 `http.auth_enabled: true` 时需 Token）
- **Prometheus 指标**：`GET /api/metrics` —— `go_goroutines`、`go_memory_alloc_bytes`、`bma_sessions_total`、`bma_llm_calls_total`、`bma_llm_timeouts_total`
- **结构化日志**：`GET /api/sessions/{id}/logs` 按 session/agent/level 查询 `session_logs`；`GET /api/sessions/{id}/token-metrics` 聚合 token 消耗
- **SSE 事件流**：`GET /api/sessions/{id}/stream`，事件 Kind 含 `think`/`intend`/`tool_call`/`tool_exec`/`tool_result`/`llm`/`wait`/`error`/`prompt`/`agent_created`/`token_usage`/`graph_step` 等（全量见 `internal/server/eventkind/kind.go`）

---

## 技术栈

- **语言**：Go 1.25（需 `GOTOOLCHAIN=local`）
- **Agent leaf 框架**：go-kratos Blades v0.5.0（`ModelProvider` + 原生 function-calling）
- **上层编排**：自研 ReAct 主循环 + `call_sub_agent` 异步分发（不依赖图状态机）
- **模型适配**：`openai` / `anthropic` / `ollama` 三种 provider 原生协议，按角色配置
- **存储**：PostgreSQL 14+ + pgvector（会话历史/事件/日志/知识库）+ Redis 7+（工作区/快照/Stream）
- **Web**：Vue 3 + Vite + Tailwind + Element Plus
- **TUI**：bubbletea + lipgloss
- **HTTP**：标准库 `net/http`，默认 `:10010`
- **配置**：YAML + `.env`（`${VAR:default}` 插值）

---

## 扩展方向

近期开放项见 `doc/TODO.md`（记忆 hot/cold 2 阶段分层、话题隔离轻量版、动态角色注册中心、verifyloop 折叠进 ReAct、硬 Token 预算、`Soul.Inject` 死代码清理、记忆事件 Postgres 持久化、`设计文档_v3.md` 漂移清理）。远期"Agent 工作流平台"愿景（工作流编排 / 测试自动化 / 研究 / 团队协作）见 `doc/扩展设计_Agent工作流平台.md`。

阶段 0-9 已完成 + 2026-07-30 两步：测试基线修复 -> 块记忆闭环 -> 协作验证闭环 -> verifyloop 原生编排器 -> 评估产出 -> KVMemory 共享记忆 -> assembly 任务拆解抽象（已删）-> ReAct 历史压缩 + LLM 调用日志 + 代理展示名 + 共享内存工具 + 角色工具白名单 + DeepSeek V4 适配 -> **投机性泛化清理**（assembly/computeruse/verifiers 占位/KV 三套抽象/强制门/死配置，~1400 行净减）-> **Agent 树显式化**（`orchestrator.Tree` + `/tree` + `/cancel` 端点）-> **块记忆事实提取**（`FactExtractor` 接口 + 轻量模型提取关键事实）。详见 `doc/TODO.md` "已完成"段。

---

## 常用开发命令

```bash
make backend-test   # 运行 backend 单元/包测试
make test-test      # 运行 test 模块集成测试
make lint           # go vet backend 与 test 模块
make up             # docker compose 启动依赖服务
make down           # docker compose 停止依赖服务
make migrate        # 应用 SQL 迁移（需 POSTGRES_DSN）
make web-build      # 构建前端
make run            # 构建前端并启动后端
make help           # 显示所有目标
```

---

## 文档索引

| 文档 | 内容 |
|------|------|
| `doc/项目说明.md` | **权威实现说明**：目录结构、核心架构、代码阅读顺序、关键设计决策 |
| `doc/设计文档_v3.md` | 设计愿景稿（顶部有状态声明，与代码偏离处以项目说明.md 为准） |
| `doc/扩展设计_Agent工作流平台.md` | 未来愿景：从编程助手扩展为 Agent 工作流平台 |
| `doc/TODO.md` | 待完成 / 开放项清单 |
| `CLAUDE.md` | Claude Code 协作指引 |

---

## License

MIT
