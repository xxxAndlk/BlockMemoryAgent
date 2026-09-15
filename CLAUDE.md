# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent - Go multi-agent orchestration system built on go-kratos Blades. ReAct-loop core (single MetaAgent -> `call_sub_agent` recursive dispatch) with event-stream memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`; root `go.work` includes only `./backend`). Go 1.25.

## Hard Conventions

- **凡新建表/新字段统一带 owner 列**：会话/Agent 级数据必须能归属到 owner（user_id），多租户隔离靠它预留（TODO #18-8）；新增持久化结构时 owner 列与业务列同期建，不给后续迁移留死角。
- **`GOTOOLCHAIN=local` required** on machines whose default Go is older. All `go test` / `go run` commands in this repo use this flag. Do not omit.
- **Windows bash shell**: use Unix syntax (`/dev/null` not `NUL`, forward slashes in paths).
- **Strict startup**: `bootstrap.Build` fails fast - missing config files, unreachable PostgreSQL/Redis, or failed LLM warmup/connectivity check all abort boot. No degraded no-DB/no-key mode for the server binary.
- **Flags for `main.go`**: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`.

## Commands

See `Makefile` (`make help` lists all targets). Key targets: `make run` (build web + run server), `make backend-test`, `make test-test`, `make lint`, `make up`/`down` (docker compose), `make migrate`. The Makefile recipes are Unix-shell (Git Bash) syntax — `make` is not installed on some Windows hosts; there run `.\dist.ps1` (= `make dist`) in PowerShell, then `.\install.ps1` to copy `dist/` into a chosen install dir and set user env `BMA_HOME`.

## Documentation Map

- **`doc/项目说明.md`** - authoritative implementation guide (directory layout, code reading order, key design decisions, architecture). Defer to this on conflict.
- **`doc/设计文档_插件范式.md`** - plugin system design (hot-pluggable MCP/bundle plugins), implemented per `backend/internal/plugins/`.
- **`doc/TODO.md`** - progress tracking and open items.
- **`doc/设计文档_v3.md`** - vision doc, top carries status note acknowledging divergence from code.
- **`doc/扩展设计_Agent工作流平台.md`** - future-vision doc (workflow platform), not current code.
- **`docs/superpowers/plans/dependency-rules.md`** - full package dependency rules and verification commands.
- **`评测文档真伪核查报告.md`** + **`上下文工程改进点.md`** - prompt engineering audit reports.

## Package Layering (quick reference)

Bottom up: `pkg/*` -> infrastructure (`store`/`memory`/`model`/`logger`/`embed`/`config`/...) -> runtime components (`dag`/`skill`/`soul`/`watchdog`/`board`/`mailbox`/`cmdqueue`) -> `runtime` aggregator -> `domain/*` (`tool`/`role`/`memory`/`subagent`/`verifyloop`/`assembly`) -> `plugins/*` (hot-pluggable plugin system: `plugins` manager + `plugins/mcpbridge` + `plugins/bundle`) -> `agent` facade -> `bootstrap` -> adapters (`server`/`tui`/`testserver`/`cmd/*`/`main.go`/`test/*`).

Key rules: `pkg/*` must not import `internal/*`; infrastructure must not import `domain/*`/`runtime`/`server`/`tui`/`agent`; `domain/*` must not import `server`/`tui`/`agent`; adapters consume via `agent.Agent` + `bootstrap`. Full rules in `docs/superpowers/plans/dependency-rules.md`.

## Tests

- Per-package `_test.go` exist for `agent`, `board`, `mailbox`, `skill`, `soul`, `watchdog`, `config`, `model`, `logger`, `domain/tool`, `domain/role`, `domain/memory`, `domain/subagent`, `server`, `tui` - run when touching those packages.
- **Integration/eval Go suites removed** (commit b35f87e): the `test/` module currently has **zero Go test files** — remaining dirs (`ab`/`benchmark`/`duel`/`swe`/`eval`) hold shell/JS scripts and run artifacts only. Rebuild is an open TODO; `.github/workflows/ci.yml` defers its nightly PG+Redis job until then. Conventions to restore with it: fixtures used `docker/docker-compose.test.yml` (isolated ports PG 55432 / Redis 56380 — never the dev instance 5432/6380, VECTOR(1024)); containers shared and left running, one PG database + Redis logical DB per test; eval suite ran with build tag `eval` against real LLM (costs credits; knobs `EVAL_FILTER`/`EVAL_RUNS`/`JUDGE_*`).

## Agent 编排页

会话页第三个主视图 `?view=orch`（选中态 `?agent=<inst_id>`，可分享/刷新恢复）：左树图（自绘 SVG tidy 布局，零图库）+ 右单 Agent 对话面板。设计/计划：`docs/superpowers/specs/2026-09-11-agent-orch-board-design.md` + `docs/superpowers/plans/2026-09-11-agent-orch-board.md`。

- 新端点：`GET /api/sessions/{id}/agents/{aid}/messages?before_seq&after_seq&limit`（完整消息历史 + mailbox 留痕）、`POST /api/sessions/{id}/agents/{aid}/message`（用户直连）。
- 用户直连状态机（`ReactService.MessageAgent` → `subagent.Dispatcher`）：`running + activity_kind=child_wait` 经 `InjectUserMessage` 投邮件并 poke 唤醒；终态（done/failed/cancelled/delivered-unverified）经 `Tree.Reopen` + `ReviveWithMessage` 同 ID 重跑（种子=原任务+上轮结果+用户消息，父收「复活返工」通知）；`running` 其他（TODO #14 T8 排队语义）→ 同样走 `InjectUserMessage` 邮箱注入并回 200 {queued}（主循环每次 generate 前 drain，下一 LLM 轮次生效；注入失败才回 409 `ErrAgentBusy`）；paused/idle/meta/未接线 → 409 `ErrAgentNotDirectable`。
- 观测写入一律 best-effort：消息逐条热写 Redis（`sess:{sid}:agent:{aid}:msgs`，TTL 24h、cap 500）+ 子 Agent 终态全量落 PG `agent_messages`；mailbox 发送留痕双写 `agent_events`（type=mailbox）；`child_wait` 是纯展示态活动 kind（只换 lastKind，不刷 lastTS、不冒泡）。Redis 不可用时写侧跳过、读侧回退 PG，端点不报错。

## 媒体产出与预览（对话栏）

Agent 产出的图/视频/音频/HTML 原型可直接在对话栏渲染（效果图、录屏、UI 原型预览）。

- **读侧**：`GET /api/sessions/:id/workspace/*path` 流式服务该会话工作目录内的文件（`http.ServeContent`，带 Range/Content-Type；越界/目录 404，nosniff）。path 型路由是为了让 HTML 产物里的相对引用（`assets/x.png`）自然可用。鉴权组的中间件额外接受 `?token=`（`<img>/<video>/<iframe>` 带不了请求头）。
- **写侧**：`ShowArtifact` 工具（`internal/domain/tool/show_artifact.go`）登记 `Result.Artifacts`（只带工作区相对路径，零二进制）；`kind=html` 可传 `content`，自动落 `<workdir>/.bma/artifacts/`。成果随 `ProgressEvent.Detail` → `session_events.detail_json`（既有持久列，无需迁移）到前端。
- **前端**：`ArtifactCard.vue`（图/视频/音频/`sandbox="allow-scripts"` iframe）；兜底识别 `inferArtifactsFromOutput` 会扫工具输出里的 `.bma/{images,od-artifacts,ui-artifacts,videos,artifacts}/` 媒体路径，Agent 未调工具也能出卡。
- 生图/生视频沿用 `ui_design` 插件（`od_image_generate`/`od_video_generate`）；`ReadMedia` 是喂模型的，别与 ShowArtifact 混用。

## 对话栏子 Agent 列表与运行中干预

- **子 Agent 列表**：`kind=sub_agent_dispatch` 事件（后端 `handleLiveEvent`，单派发/批量逐项落事件）→ `Turn.subAgents` → `SubAgentList.vue`（一人一行：领域名 + 任务摘要 + 实时状态）。状态来自 `agents` 接口实例（每 3s 刷新），旧事件无领域名时按"任务 ↔ 实例 goal 最长公共前缀"匹配且要求唯一。**派发事件必须走 `sub_agent_dispatch` 分类**——未知 kind 会跌进思考链（`turns.ts` 陷阱）。
- **domain 一律中文领域名**：`domain` 是子 Agent 对用户可见的展示名（breadcrumb/编排树/列表/事件流都用它），提示词硬约束 + `validateDispatchArgs` 软警告（放行+提示，不硬拒——会与批量派发叠加成拒绝循环）。它随派发事件 `detail_json.domain` 下发（事件 Tool 字段只放得下角色 ID）。
- **运行中用户输入 = 邮箱注入**：运行中的 ReAct 主循环**不读 `session.Messages`**，唯一触达通道是邮箱（`waitForChildren` 每周期 drain / 主循环顶部 drain）。`sendMessage` 对 running 会话经 `injectUserMessageToRunningSession`（AgentMessenger→`InjectUserMessage`）投递；等子 Agent 时立即生效，其他阶段下一步生效。`drainMailbox` 对 From=user 不冒泡 `sub_agent_done`。
- ThinkChain 无可视步骤（只剩被折叠的 prompt/token_usage）时不渲染，避免"思考链路 0 步"空壳。

## 对话栏过程留存（思考与中间正文）

- **三档分类（2026-09-13 用户三轮实证定型，市面对齐：只有推理算思考）**：`turns.ts` 把事件分三档，档位决定展示位置——
  - **推理**（`think`/`intend`）→「思考链路」盒：12px 灰、`.chain-body` 定高 280px 内部滚动、可折叠，**盒里只放推理**（此前是 llm_result/agent_done 混装的杂物盒）。
  - **Agent 发言**（`assistant_text` 中间正文、`llm_result` 子 Agent 结果摘要、`message` Meta 中继文本、`llm`/`llm_response`/`notify`、**未知 kind**、非终态系统提示）→ 按**正文**展示（`.md-article` 15px，与最终答复同档；多 Agent 时上方标出说话者）。它们是"需要展示的东西"，不是思考。
  - **调试/活动**（`prompt`/`token_usage`/`graph_step`/`wait`/`sub_agent_done`/`memory_recall`… 、`type=progress`）→ 折叠进思考盒计「已折叠 +N」，完整事件流看监控页（`?view=monitor`）。
  - 判别一律用 **`kind||type`**（`prompt`/`system` 只有 type，只认 kind 会把「输入补全: gate_skip」当发言展示）。
  - **教训**：这些发言块既不该压成灰字、也不该装箱（156/157 两轮都错在把模型说给用户的话当成了思考）；改分类时用 `groupEventsToTurns` 离线跑一遍真实会话事件（esbuild 打包 + 事件 JSON）核对每档数量，比肉眼看页面可靠。
- **思考链累计**：`AssistantTurn.thinkEvents` 渲染**全部**推理段落（曾只留"最后一段连续 think"，一到工具调用前一段推理就整段消失）；段内去重——思考文本是累积快照（LLMDelta 与 ToolCall 各落一次），后一条是前一条超集时前一条不渲染。
- **中间正文（`kind=assistant_text`）**：模型「口播一句→调工具」的那段正文，后端在 `LiveEventToolCall` 边界经 `persistInterimText` 落事件（一次 LLM 轮次一条；`session.lastInterimText` 去重并行工具调用；子 Agent 剥【展示名】前缀）。**ask_user 跳过**——正文由提问事件 `detail_json.report_text` 承载（任务 152），再落会重复展示。前端 `classifyEvent` 显式认这个 kind（属上方"Agent 发言"档），收进 `Turn.narrations` 渲染；`isAssistantTextEvent` 到达即清 live 行，防与正文块同屏重复。
- **刻意不清 `StreamingText`**：ask_user / 审批 hook 还要用它做提问正文快照，提前清会让问答卡上方的正文丢失。`trimDebugEvents` 不裁 `assistant_text`（只裁 think/prompt/token_usage/graph_step）。
- TUI 侧暂无 `assistant_text` 渲染分支（不显示、也无从消失），仍只有 `StreamingText` live 行。

## 问答卡（ask_user）正文留存与提交韧性

- **正文留存**：模型「先输出正文、再调 ask_user」时正文只在瞬时 `StreamingText`（流式增量按设计不落事件）。三个 hook（单题/批量/审批）在**持锁区**取快照，经 `clarifyReportJSON` 挂到提问事件 `detail_json.report_text`（零迁移；批量只挂第一条）。前端 `clarifyReportFromDetail` → `Turn.clarifyReport` → `AssistantTurn` 在问答卡上方按正文渲染，刷新/回放后仍在。空正文不写字段。
- **提交韧性**：`chat/utils/clarifySubmit.ts` 把「网络层失败」（`client.ts` 的 `NetworkError`：timeout/offline）与「业务拒绝」（`APIError` 400/409）分开——前者退避重试（2s→10s，上限 150s，覆盖本机约 1 分钟的部署重启窗口），且**每次重试前先查会话状态**确认答复是否已落地，避免二次答复；后者仅在"会话已不在待澄清"时按已提交处理。`/clarify` 超时 30s。失败时问答卡内常驻显示原因、**草稿保留**，可直接再点提交。
- 排查提示：`Failed to fetch` 是浏览器网络层错误（请求根本没到服务端），先看 `D:\WebApp\bma\logs\backend_restart_*.log` 是否正逢重启。

## 监控页（?view=monitor）过滤行布局

会话页右侧监控面板实测只有 **600~800px 宽**（1440 窗口下 806px），而 Element Plus 给 `.el-input`/`.el-select` 的 `width:100%` 会顶掉 Tailwind 的 `w-*`——它的 flex **基准尺寸=整行宽**，挤压时按基准比例分摊，把同行的下拉压成光杆箭头、溢出的徽标盖住邻座标签（2026-09-12 用户实证）。约定：

- 过滤行控件一律「**定宽包装 div + `!w-full`**」（包装层是普通 div，宽度不被 EP 抢），分组补 `shrink-0`，弹性全部交给搜索框（`flex-1 min-w-[160px] max-w-[256px] ml-auto`），整行 `flex-wrap` 兜底；锁定徽标 `max-w-[220px] truncate` + `title` 全名。`SessionLogsPanel.vue` 同理。
- 事件类型展示统一走 `eventStyles.kindLabel()`（中文），原始 kind 放 `title`；**新增 kind 记得补映射**，否则回退成英文 id 直出（`sub_agent_dispatch`/`sub_agent_done` 曾如此）。
- 改监控页 UI 后的验证姿势：headless Chrome（`--remote-debugging-port`）+ Node 22 内置 WebSocket 驱动 CDP（`Emulation.setDeviceMetricsOverride` 定宽 → `Runtime.evaluate` 读 rect → `Page.captureScreenshot`），比开浏览器快且可复现；**注意 SSE 长连接会让 `--virtual-time-budget` 截图挂死**，走 CDP 就没有这个问题。

## History

Codebase migrated from CloudWeGo Eino to go-kratos Blades (pre-v3 Eino docs no longer in tree).

## Plugin System

Hot-pluggable plugins (design: `doc/设计文档_插件范式.md`): plugins register into the shared `domain/tool.Registry`; ReAct reads `Schema()` fresh every iteration, so enable/disable takes effect next iteration. Plugin manager lives in `backend/internal/plugins/` (manager + `mcpbridge/` + `bundle/`).

- Config: `config/plugins.yaml` (alongside `config.yaml`); external plugin packages go in `config/plugins.d/`. Manage via API: `GET/POST /api/plugins[/{id}][/enable|/disable]`, `POST /api/plugins/reload`. All endpoints pass `AuthMiddleware`.
- Initial plugins: `web_search` (mcp, http transport → firecrawl 官方云端 `https://mcp.firecrawl.dev/v2/mcp`，免 key 会话，enabled by default；2026-09-15 由本地自托管栈切云端——本地栈实测 search 返回空结果（CN 网络到搜索源不可达）且常驻 ~3.5GB 内存。免 key 仅暴露 search/scrape/parse 三工具；全工具需 firecrawl.dev key，自托管备选见 `config/plugins.yaml` 注释与 `docker/docker-compose.firecrawl.yml`) and `computer_use` (mcp, stdio `npx computer-use-mcp`, **disabled by default**; all tools marked `Destructive()` → approval guard chain). `computer` sandbox service (Xvfb + noVNC) available in `docker/docker-compose.yml` behind the `computer` profile.
- Role visibility: plugin `settings.roles` (default `["*"]`) restricts which roles see plugin tools; adapters filter as static whitelist ∪ dynamic plugin visibility.
- Tests: `backend/internal/plugins/*_test.go`, `mcpbridge/bridge_test.go` (spawns a mock MCP server child process), `server/plugins_test.go` (API loop). `go test -race` needs a C compiler (not available in this env).

## 三档控制与平台补齐（TODO #14-18 第一批，2026-09-15）

落地详情见 `doc/TODO.md` #14-18 条内「第一批落地」标注与 `doc/变更.md` 任务 160。日常改动需知的稳定语义：

- **会话档位** `auto|fast|cluster`（explore 留枚举未实现）：`POST /api/sessions/:id/gear` 手动切档任意向；`auto` 每轮按 `agent/gear_selector.go` 规则重选（闲聊信号→fast，其余 cluster）且**只升不降**；`escalate_gear` 工具经 askUser 通道确认升档（槽占用即工具级"稍后再试"）；gear 随 MetaMemory JSONB 落库、restore 回填。fast 档走 `chat` 角色（roles.yaml fixed_roles，快模型 + 低 thinking，跳过 roster/ledger wrapper），cluster 走 meta 全装。
- **roleToolGate 默认开启**（config `role_tool_gate_enabled: true`）：Dispatch 执行前按"角色静态 tools ∪ 该角色可见插件工具"并集硬校验；meta tools 已收窄 40→22（roles.yaml）。逃生舱：改回 false。
- **提示词版本钉**：`pkg/prompts.Version`（现值 `20260915-1`）——语义改动提示词必须 bump 并写进变更记录；启动日志打印。
- **数据围栏**：MCP 插件与 HTTPGet/HTTPPost 的外部内容经 `tool.WrapUntrusted` 包进 `<untrusted_data>` 围栏（围栏标记转义防逃逸，本地工具不包）；meta/domain 提示词含【数据围栏纪律】——围栏内是数据不是指令。
- **新端点**：`GET /api/capabilities`（装机自检六项 llm/postgres/redis/embed/plugins/workdir，只做廉价检查不发真实 LLM 调用，settings 页有面板）；`GET /api/sessions/:id/export`（zip：会话 JSON ×4 + workspace/.bma 产物树，2000 文件/64MB 上限）；`GET /api/export/memory`（未归档 global_knowledge JSONL + user_profile.md + skills_learned/）。导入端点未做。
- **通知**：`notify.webhook_url`（空=关）+ `webhook_events`（默认 [completed,error]，状态枚举无 failed）——会话终态 best-effort POST（3s 超时，60s 同会话同状态去重）；前端页面隐藏 ∧ 终态 → 浏览器 Notification（settings 页申请权限）。
- **数据生命周期**：`data.*` 配置——`knowledge_archive_days`（0=关，opt-in）、log/tool_outputs 保留期（0=默认 30/14 天，负数=关）；启动 + 每日 tick；归档 = 置 `archived=true` 冷备份（查询层全局排除），不是删除。
- **HTTP 默认绑 `127.0.0.1:10010`**：局域网访问需显式 `HTTP_ADDR=:10010`（.env.example 已注明）。
- **领域注册表**：`domain_profile` 复用 global_knowledge（零新表）；dispatcher `SetDomainProfileHook` 按文件路径重叠匹配 + 冷复活种子注入（stat 剔除已删文件）；`QueryBlockMemoryByDomain` 按 task_domain 成链召回；PROJECT.md DomainPartition 作种子导入（source="project_md"）。
- **install.ps1**：交互填 OPENAI_API_KEY（占位符跳过）+ 自动生成 BMA_API_TOKEN + 启动后探活 /api/health 与 /api/capabilities。**改任何 .ps1 必须保持 UTF-8 BOM**（PowerShell 5.1 无 BOM 按 ANSI/GBK 解析中文直接报 tokenizer 错误）。
- **CI**：`.github/workflows/ci.yml` push/PR 即跑 backend vet+test 与 web 构建；nightly PG+Redis integration job 待 test/ 模块重建 Go 套件后再补（现零 Go 测试文件，见 Tests 节）。
