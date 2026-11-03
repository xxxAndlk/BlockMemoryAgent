# BlockMemoryAgent

> 一个基于 **上下文外部化 + 记忆隔离** 的 Go 多 Agent 编排系统。核心执行架构：单一 ReAct 主循环 + `call_sub_agent` 异步子 Agent 分发，配事件流记忆、PostgreSQL／pgvector + Redis 存储，以及 HTTP Web UI、bubbletea TUI、Windows 桌面入口。
>
> **文档入口**：当前模块与实现全景见 [项目模块全景](doc/项目模块全景.md)，多 Agent 完整主链见 [21 多 Agent 设计详解](doc/技术逻辑/21-多Agent设计详解.md)，新旧资料边界见 [文档地图](doc/文档地图.md)。本文保留快速上手和演进背景；具体默认开关、端点与状态语义冲突时，以当前代码、配置和项目模块全景为准。

---

## 为什么需要这个项目

使用 Claude Code 等编程助手时，你会遇到三个无法回避的问题：

1. **上下文一膨胀，模型就"变笨"**——长上下文中有效注意力会衰减，中间信息被"稀释"。
2. **多轮对话夹杂不相干内容，污染当前任务**——所有历史挤在一个线性上下文里。
3. **对话一长就被迫开新会话，项目上下文全丢**——没有跨会话记忆持久化。

BlockMemoryAgent 的应对思路：不依赖 LLM 记住一切。子 Agent 各自持有独立的 ReAct 上下文（互不污染），执行过程写入事件流记忆，会话历史/事件持久化到 PostgreSQL。其中"跨会话知识自动召回注入 prompt"仍是演进方向（见 `doc/TODO.md`），完整设计愿景见 `doc/设计文档_v3.md`。

---

## 核心特性

- **单一 ReAct 主循环**：角色 Agent 执行 LLM → 工具 → 结果回填，直到纯文本回答、暂停或达到配置上限；`tool_call_max_rounds` 当前为 `-1`，不再默认限制 50 轮
- **层级即调用栈**：`call_sub_agent(role_id, task)` 异步运行子 Agent，完成后经 Mailbox 回传父 Agent；支持递归派发、权威 Agent 树、取消、计划确认、验收、热驻复用与可选 worktree 隔离
- **层级历史压缩**：由角色 token 阈值驱动，`summarize_every` 已退役；旧历史形成可合并的结构化压缩包，保留近期消息，压缩状态落 `agent_compress_states`，完整主对话落 `agent_messages`
- **长期记忆与多轴决策**：`long_term_memory_items/events` 提供候选、激活、隔离、替换、撤回、恢复和审计；自动写入默认 `shadow`，明确撤回默认 `enforce`；`session_decisions` 按执行模式、素材、视角、审批、验收等轴保留用户指令
- **共享记忆与规格**：`WriteSharedMemory`／Spec 在父子 Agent 间传递文件路径、契约与验收标准，派发参数有软／硬长度限制，当前配置以 `task_max_runes` 和 `task_max_runes_hard` 为准
- **分层验收**：当前主链以叶子自检、领域验收、Meta 整品验收和 spec verify levels 为主；`verifyloop` 包保留验证闭环接口，不应写成默认启用所有任务的独立状态机
- **角色工具白名单**：工具经 Registry、角色白名单、插件可见性、沙箱与审批守卫共同约束；当前 `role_tool_gate_enabled: true`
- **经验技能治理**：SessionEvolver 可生成偏好、项目经验与技能候选，但实际写入受 `memory.write_mode` 控制；技能库具有选择、使用统计、整理与淘汰机制
- **工作区文件树与全屏预览**：会话侧栏工作区目录树、在线编辑、消息文件卡片和全屏 FileViewer，支持图片、PDF、视频、Markdown、代码、CSV、xlsx、ipynb 等格式
- **热插拔插件系统**（[设计文档](doc/设计文档_插件范式.md)）：MCP stdio／HTTP／Docker、目录式插件包与长驻服务统一接入工具或生命周期管理；管理 API 为 `/api/plugins`，配置为 `config/plugins.yaml` 与 `config/plugins.d/`
- **可扩展工具系统**：不止早期的 14 个工具；当前覆盖文件、命令、HTTP、Git、共享记忆／Spec、提问、定时、外部 Agent、插件、Widget、Workflow、工具锻造等，实际集合由装配与角色权限决定
- **三类后续编排**：Goal／Loop 负责会话续跑与提醒，Flow 执行 start／llm／tool／branch／approval／answer 节点图，Workflow 执行带人审和预算的“件 × 阶段”量产
- **会话持久化与恢复**：会话、消息、事件、树、看板、决策和各类 run 按各自机制落库；当前 `agent.restore_sessions: true`，启动预热最近会话，其他数据按查询或懒加载恢复
- **LLM 调用全链路日志**：`sessionLogger` 写 `session_logs`，`QueryKindLogs` 回看调用链，`LiveEventTokenUsage` 推送 token 用量
- **结构化日志与可观测性**：会话日志、文件按日分割、SSE 实时事件与 `/api/metrics` Prometheus 指标
- **基础设施严格启动，模型不阻断**：配置与 PG／Redis 等基础设施按启动校验处理；`agent.verify_llm_on_startup: false` 默认关闭，开启探测也只告警不阻断，模型可在应用内切换
- **Web／TUI／桌面入口**：Vue 3 SPA、Bubble Tea TUI 与 Windows WebView2 桌面壳；桌面支持托盘后台、版本化更新、健康门禁与回滚
- **DAG 定时调度**：`dag_enabled` 开启后支持标准 5 段 cron、简易间隔和依赖任务，Web 端 `/dag` 管理

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

## 初次使用指引（从零跑通）

按以下顺序执行，每步都有验证方法，卡在哪一步先看末节「常见坑」。

### 第 0 步：准备环境

| 依赖 | 版本 | 用途 |
|------|------|------|
| Go | 1.25+（机器默认 Go 较旧时所有 go 命令前缀 `GOTOOLCHAIN=local`） | 构建/运行后端 |
| Node.js + npm | Node 18+ | 构建前端 SPA（`web/dist`） |
| Docker + docker compose | 任意近期版本 | 一键起 PostgreSQL + Redis；也可自备实例 |
| LLM API Key | 至少一个 | OpenAI 兼容 / Anthropic / 本地 Ollama 均可 |

Windows 下命令在 Git Bash 中执行；没有 `make` 时用下文给出的等价原始命令。

### 第 1 步：最小配置（三个文件）

```bash
cp .env.example .env
```

1. **`.env`（必填）**：按你的模型供应商填一组 Key 与端点，例如 DeepSeek 填 `OPENAI_API_KEY` + `OPENAI_BASE_URL=https://api.deepseek.com/v1`；Claude 系填 `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL`。`POSTGRES_DSN` / `REDIS_ADDR` 用 compose 默认值即可。
   - 注意：自带 compose 的 Redis 设了密码，`.env` 里需填 `REDIS_PASSWORD=blockmemory_dev`（`.env.example` 默认为空，不配会在启动探活时认证失败）。
   - 块记忆向量化需要 Embedding：填 `EMBEDDING_BASE_URL` / `EMBEDDING_API_KEY`（OpenAI 兼容 `/v1/embeddings`，roles.yaml 的 `embed` 段选模型）；不填则 `embed` 能力不可用（其余功能不受影响）。
2. **`config/models.json`（必填）**：模型连接参数清单（provider/model/api_key/base_url，`${VAR}` 从 `.env` 插值）。照模板加一条你自己的模型条目即可，`api_key`/`base_url` 引用 `.env` 变量，不要把密钥直接写进 json。
3. **`config/roles.yaml`（必填）**：把 `meta_agent.model_config.model_ref`、`domain_agent.model_config.model_ref`、`lightweight_model.model_ref` 指向 models.json 里可用的条目 id。这三个角色是最小可跑集合；meta 建议非推理或低思考档模型（路由/汇总要快），domain 建议留思考档（编码执行要正确率），lightweight 选最便宜快的（做摘要/压缩）。

其余配置文件（`config/config.yaml` / `skills.yaml` / `soul.md` / `user_profile.md`）保持仓库默认值即可，但**必须存在**——严格启动，任一缺失或 PG/Redis 不可达都会启动失败并报出具体原因。**模型不参与启动校验**：`models.json` 里某个条目 key 过期/端点不可用不影响启动，服务起来后在模型页切换角色绑定即可（`agent.verify_llm_on_startup: false` 默认关闭探测，开启也只记告警）。

### 第 2 步：启动 PostgreSQL + Redis

```bash
make up        # 等价：docker compose -f docker/docker-compose.yml up -d
```

核心数据面是 PostgreSQL（:5432，含 pgvector）与 Redis（:6380）；当前 Compose 还包含 `open_design` 和 `minimax-media` 媒体服务。已有自备 PG／Redis 时可调整或跳过相应服务，并修改 `.env` 的 DSN／地址；媒体服务是否启用按实际需求与配置处理。

### 第 3 步：建表

```bash
make migrate   # 需 POSTGRES_DSN 环境变量；等价：for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
```

全新数据库其实可以跳过：`bootstrap.Build` 启动时会幂等自动建表。仍建议跑一遍，保证索引等细节与 migrations 一致。

### 第 4 步：构建前端 + 启动后端

```bash
make run       # 等价：cd web && npm install && npm run build，然后 go run ./backend
```

前端只需首次（或前端改动后）构建；日常改后端代码直接 `go run ./backend`。启动日志出现监听 `127.0.0.1:10010` 即成功。

### 第 5 步：自检（六类能力探活）

```bash
curl http://127.0.0.1:10010/api/health          # {"status":"ok"} 即进程就绪
curl http://127.0.0.1:10010/api/capabilities    # 逐项核对 llm/postgres/redis/embed/plugins/workdir
```

`/api/capabilities` 对每项给出 `ok` + 失败原因 + 修复提示（hint），是首次部署排障的固定入口。全绿后浏览器打开 `http://127.0.0.1:10010` 进 Web UI。

### 第 6 步：第一次对话

1. Web UI 新建会话：填目标（goal），选执行档位——**fast**（文档助手直达，轻任务）/ **daily**（DomainAgent 直接执行，默认）/ **cluster**（MetaAgent 全装编排多领域子 Agent，大任务）。
2. 工作目录不选则落到 `agent.default_workdir`（默认安装目录下 `workspace/`）；建议指向一个测试目录，Agent 的文件读写都在该目录沙箱内。
3. 发送后右侧 SSE 流实时展示思考、工具调用与子 Agent 树；子 Agent 状态可经 `GET /api/sessions/{id}/tree` 查看。
4. 偏爱终端可用 TUI：`go run ./backend/cmd/tui`。

### 第 7 步（可选）：Windows 装机部署

不想守着仓库目录跑，可打包安装到固定目录：

```powershell
.\dist.ps1        # 构建前端+后端二进制，生成 dist/ 布局（PowerShell 版 make dist）
.\install.ps1     # 复制到安装目录（默认 D:\WebApp\bma），写入 BMA_HOME，向导式补填 LLM Key、自动生成 BMA_API_TOKEN
```

install.ps1 幂等可重复跑（即升级流程）；服务已在跑时会自动调 `/api/capabilities` 做六类能力自检。

### 常见坑

- **`go: toolchain ...` 报错**：所有 go 命令加前缀 `GOTOOLCHAIN=local`（仓库 go.work 锁定工具链）。
- **启动报 Redis 认证失败**：`.env` 补 `REDIS_PASSWORD=blockmemory_dev`（compose 容器设了 `requirepass`）。
- **端口冲突**：5432 / 6380 / 10010 被占用时，改 compose 端口映射或 `.env` 的 `POSTGRES_DSN` / `REDIS_ADDR` / `HTTP_ADDR`。
- **开启鉴权后 401**：`config.yaml` 的 `http.auth_enabled: true` 时，前端登录与 API 调用都需 `Authorization: Bearer <BMA_API_TOKEN>`。
- **局域网访问不到**：默认只绑回环，`.env` 设 `HTTP_ADDR=0.0.0.0:10010`（请同时开启鉴权）。
- **embed 能力 FAIL**：只影响块记忆向量化检索，补 `EMBEDDING_*` 后重启即可；其余功能不受影响。

---

## 快速开始

### 依赖

- Go 1.25（机器默认 Go 较旧时需 `GOTOOLCHAIN=local`）
- PostgreSQL 14+（含 pgvector 插件）
- Redis 7+
- 至少一个大模型 API Key（OpenAI 兼容 / Anthropic / 本地 Ollama）

> 配置文件与 PostgreSQL、Redis 等启动必需基础设施必须可用。LLM 后端不是启动闸门：默认不做连通性探测，即使开启 `agent.verify_llm_on_startup` 也只告警、不阻断；运行对话前仍需为所用角色配置可用模型。

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

web_search 走 firecrawl 官方云端免 key 端点，无本地数据面；其余插件的桥镜像容器化运行，open_design 设计台并入统一 compose。新机器首次部署：

```bash
make plugins-build   # 构建本地插件镜像：
                     #   bma/computer-use-mcp:local（computer_use，本地构建，含 Xvfb/noVNC 虚拟桌面）
                     #   bma/open-design-mcp:local / bma/ui-preview-mcp:local（ui_design / ui_preview 桥）
make up              # 起 PostgreSQL + Redis + open_design 设计台（compose project bma，
                     # 容器 bma-open-design，127.0.0.1:7456；需先在 .env 填 OD_API_TOKEN）
```

启用方式（`config/plugins.yaml`）：

- **web_search**：默认 `enabled: true`，http transport 直连官方云端 `https://mcp.firecrawl.dev/v2/mcp`（免 key，仅 search/scrape/parse 三工具）；配 `FIRECRAWL_API_KEY` 后可按 plugins.yaml 注释改走备选 2（stdio `npx firecrawl-mcp`）解锁全工具与更高配额。
- **computer_use** 等其余插件：默认 `enabled: false`，改配置为 `true` 随启动自动起，或运行时热启用 `POST /api/plugins/<id>/enable`。
- **open_design**：不再是后端拉起的插件，随 `docker/docker-compose.yml` 统一启停（`make up`），UI：`http://localhost:7456`（Basic 认证 `open-design` / `OD_API_TOKEN`）。

入口与验证：

- `GET /api/plugins` 查看插件状态；computer_use 虚拟桌面观察口：`http://localhost:6081`（noVNC，操作发生在容器内 Xvfb 虚拟桌面，不控制宿主机）。
- 同机多个 BMA 实例（如 tui.exe + headless 后端）不要同时 enable 带端口映射的同一插件（6081 宿主端口冲突）。

---

## 项目结构

```
.
├── backend/                    # Go 主模块（50 个包完整索引见项目模块全景）
│   ├── main.go                 # HTTP 入口，装配委托 bootstrap.Build
│   ├── cmd/                    # desktop / tui / memory-console / skill-cleanup
│   ├── internal/
│   │   ├── agent/              # ReAct、会话、长期记忆、决策、附件与上下文
│   │   ├── domain/             # subagent / tool / memory / role / orchestrator / decision / verifyloop
│   │   ├── workflow/           # 配方、名册、件 × 阶段量产与人审
│   │   ├── flow/ goal/ dag/    # 节点流程 / 会话指令 / 定时与依赖调度
│   │   ├── bootstrap/          # 生产、TUI、桌面后端与测试共用的统一装配
│   │   ├── model/ store/       # 模型适配 / PostgreSQL、Redis 与 Schema
│   │   ├── server/             # Gin HTTP API + SSE + 管理端点
│   │   ├── tui/                # Bubble Tea TUI
│   │   ├── widgets/            # 界面部件管理
│   │   ├── plugins/            # 插件包、MCP 桥与生命周期
│   │   └── ...                 # board / mailbox / project / skill / logger 等
│   └── pkg/                    # types / enums / config / prompts / 工具包
├── config/                     # config / roles / models / plugins / skills / soul 等
├── migrations/                 # SQL schema（当前 001-019）
├── web/                        # Vue 3 + TypeScript + Vite SPA（18 个子路由）
├── test/                       # 独立集成测试模块
├── doc/                        # 当前全景、文档地图、技术逻辑、历史设计与评测
├── docker/                     # Compose、open_design、minimax-media 与插件镜像
└── bma-app.ps1                 # Windows 打包、安装、更新、启停与回滚
```

---

## 核心模块

### 1. ReAct 与会话（`internal/agent`）

`react_agent.go` 执行模型、工具与结果回填；`service_react.go`、`session_react.go` 管理会话创建、发送、控制和恢复。历史由角色 token 阈值、滑动窗口和结构化压缩共同治理，`summarize_every` 已退役。决策账本、项目上下文、画像、技能和 Mailbox 在各自约束下进入请求视图。当前轮数上限、预算与超时以 `config/config.yaml` 为准。

### 2. 子 Agent 与权威树（`internal/domain/subagent`、`orchestrator`）

`dispatcher.go` 负责异步派发、权限与 Spec、并发、计划确认、验收、暂停恢复、热驻复用、失败处理和 worktree。子 Agent 结果经 Mailbox 回传；`orchestrator.Tree` 维护持久化层级元数据，HTTP 提供树查询和取消。共享 KV 传递上下文，旧块记忆自动写入当前默认停用。

### 3. 工具域（`internal/domain/tool`）

`Registry` 负责 Schema、Dispatch、角色可见性和授权；内置工具覆盖文件、命令、HTTP、Git、共享记忆、协作、定时、插件、外部 Agent、Widget 与 Workflow 等。`sandbox.go`、`guards.go`、`destructive.go` 提供路径边界、命令黑名单和破坏性操作控制。当前角色工具硬门开启，不能再按“14 个工具”理解系统。

### 4. 记忆与决策（`domain/memory`、`agent/memory_*`、`store`）

事件流与 Pipeline 负责执行历史和压缩；共享 KV／Spec 服务会话内协作；`long_term_memory_items/events` 负责跨会话条目生命周期与审计，`session_decisions` 按多轴保留当前会话指令。长期记忆自动写入默认 `shadow`，撤回默认 `enforce`，详见 [17 长期记忆与决策轴](doc/技术逻辑/17-长期记忆与决策轴.md)。

### 5. Goal、Flow 与 Workflow

Goal／Loop 在会话完成后评估或排程续跑；Flow 串行执行六类节点并支持审批；Workflow 按配方对名册执行件间并发、件内阶段串行的量产，支持 attempt、人审、预算、冻结和反馈。三者不是同一状态机，详见 [18](doc/技术逻辑/18-Goal指令与Flow流程.md) 与 [19](doc/技术逻辑/19-工作流量产流水线.md)。

### 6. 模型与存储（`internal/model`、`internal/store`）

模型层基于 Blades，适配 OpenAI 兼容、Anthropic、Ollama 等 provider，处理角色绑定、超时、重试、思考和 token。存储层按领域拆分会话、消息、事件、树、决策、长期记忆、看板、Flow、Workflow 等子存储；PostgreSQL 是持久权威，Redis 承担热数据，pgvector 支持向量检索。

### 7. 插件与扩展（`internal/plugins`、`mcpbridge`）

支持 MCP stdio／HTTP／Docker、目录式插件包和长驻服务，统一接入工具注册表与生命周期。`computer_use` 当前为按需 Docker transport 插件；`open_design` 和 `minimax-media` 随 Compose 提供媒体相关服务。

### 8. 运行时与 DAG（`internal/runtime`、`board`、`mailbox`、`dag`）

Runtime 聚合 Board、Mailbox、Skill、Soul 等组件。Board 当前通过 `session_boards` 保存整板 JSONB 快照，Mailbox 负责进程内异步回传。DAG 支持 cron、简易间隔与依赖调度；它和 Goal 提醒、Flow 运行、Workflow 量产分别使用自己的状态与入口。

### 9. 日志与可观测性（`internal/logger`、`server`）

结构化日志、会话 LLM I/O、SSE 事件、token 指标、Prometheus 指标和健康／能力端点共同提供诊断面。桌面更新还读取 `/api/health` 的 build 版本，防止旧进程占端口造成假更新成功。

### 10. Windows 桌面与发布（`cmd/desktop`、`bma-app.ps1`）

桌面壳使用 WebView2，关窗默认隐藏托盘；发布脚本提供版本化安装、更新、启停和回滚，用户数据与不可变版本目录分离。详见 [20 桌面端与发布运维](doc/技术逻辑/20-桌面端与发布运维.md)。

---

## 可观测性与指标

- **健康检查**：`GET /api/health`（公开，其余端点在 `http.auth_enabled: true` 时需 Token）
- **Prometheus 指标**：`GET /api/metrics` —— `go_goroutines`、`go_memory_alloc_bytes`、`bma_sessions_total`、`bma_llm_calls_total`、`bma_llm_timeouts_total`
- **结构化日志**：`GET /api/sessions/{id}/logs` 按 session/agent/level 查询 `session_logs`；`GET /api/sessions/{id}/token-metrics` 聚合 token 消耗
- **SSE 事件流**：`GET /api/sessions/{id}/stream`，事件 Kind 含 `think`/`intend`/`tool_call`/`tool_exec`/`tool_result`/`llm`/`wait`/`error`/`prompt`/`agent_created`/`token_usage`/`graph_step` 等（全量见 `internal/server/eventkind/kind.go`）

---

## 技术栈

- **语言与 Agent 框架**：Go 1.25，go-kratos/blades；自研 ReAct 与 `call_sub_agent` 异步编排
- **模型适配**：OpenAI Chat／Responses、Anthropic、Ollama，按角色配置并支持会话级覆盖
- **存储**：PostgreSQL 14+、pgvector、Redis 7+
- **HTTP**：Gin，默认 `127.0.0.1:10010`，SSE 实时事件
- **Web**：Vue 3、Vite、TypeScript、Tailwind、Element Plus
- **终端与桌面**：Bubble Tea／Lipgloss；Windows WebView2 与托盘
- **配置**：YAML、JSON、`.env` 环境变量插值
- **部署**：Docker Compose、PowerShell 版本化安装／更新／回滚

---

## 扩展方向与历史

开放项以 [TODO](doc/TODO.md) 的当前有效条目为准，远期愿景见 [Agent 工作流平台扩展设计](doc/扩展设计_Agent工作流平台.md)。TODO 同时保留历史完成记录，不能仅凭“已完成”标题推断当前默认开关或主链行为。

项目经历 ReAct 重构、子 Agent 生命周期、共享记忆、验证机制、插件、历史压缩、长期记忆治理、Goal／Flow／Workflow 和桌面发布等多轮迭代。逐次背景与当时验证见 [变更记录](doc/变更.md)，当前模块状态以 [项目模块全景](doc/项目模块全景.md) 为准。

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
| [项目模块全景](doc/项目模块全景.md) | **当前实现总入口**：50 个 Go 包、18 个 Web 路由、数据、配置、部署与测试地图 |
| [文档地图](doc/文档地图.md) | 现行说明、历史快照、设计、计划、变更与评测的可信边界 |
| [技术逻辑 00](doc/技术逻辑/00-总览与阅读指引.md) | 00–22 章阅读导航与代码入口 |
| [17 长期记忆](doc/技术逻辑/17-长期记忆与决策轴.md) · [18 Goal／Flow](doc/技术逻辑/18-Goal指令与Flow流程.md) · [19 Workflow](doc/技术逻辑/19-工作流量产流水线.md) · [20 桌面发布](doc/技术逻辑/20-桌面端与发布运维.md) | 四篇当前实现专题 |
| [21 多 Agent 设计详解](doc/技术逻辑/21-多Agent设计详解.md) | 当前多 Agent 主链：角色、ReAct、派发、权限、记忆、审批、验收与恢复 |
| [22 AgentTeam 问题与修复清单](doc/技术逻辑/22-AgentTeam问题与修复清单.md) | 真实使用 P0（效率与上下文）、身份权限、安全可靠性、Team 能力缺口及分阶段修复路线 |
| [项目说明](doc/项目说明.md) | 2026-09-08 历史实现快照，不再作为权威现状说明 |
| [设计文档 v3](doc/设计文档_v3.md) | 设计愿景；与代码偏离处以当前实现为准 |
| [Agent 工作流平台扩展设计](doc/扩展设计_Agent工作流平台.md) | 未来愿景与扩展方向 |
| [TODO](doc/TODO.md) · [变更](doc/变更.md) | 开放项／历史完成记录 · 逐次变更与当时验证 |
| [CLAUDE](CLAUDE.md) | Claude Code 协作指引 |

---

## License

MIT

# 更新命令
cd D:\WebData\github\BlockMemoryAgent
.\bma-app.ps1 deploy -Target D:\WebApp\BMA-Desktop -SkipWeb
