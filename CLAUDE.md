# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent - Go multi-agent orchestration system built on go-kratos Blades. ReAct-loop core (single MetaAgent -> `call_sub_agent` recursive dispatch) with event-stream memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`; root `go.work` includes both `./backend` and `./test`). Go 1.25.

## Hard Conventions

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
- Integration tests under `test/` (separate module): `test/api/*`, `test/coding/*` (e2e via mock LLM), `test/tui/*`, `test/fixtures/*`. Fixtures use `docker/docker-compose.test.yml` (isolated ports PG 55432 / Redis 56380) — never point tests at the dev instance (5432/6380, VECTOR(1024)). Test containers are shared and left running; each test gets its own PG database + Redis logical DB (do not add compose `down` to fixture setup/cleanup — parallel packages share the containers).
- Eval suite under `test/eval/` (build tag `eval`, real LLM, costs API credits): scenario YAMLs + deterministic checkpoint judges + metrics, reports to `test/eval/runs/<ts>/report.{json,md}`. Dry-run: `cd test && EVAL_DRY_RUN=1 GOTOOLCHAIN=local go test -tags=eval ./eval/ -run TestEval -v`. Knobs: `EVAL_FILTER`, `EVAL_RUNS`, `JUDGE_*` for llm_judge checks.

## Agent 编排页

会话页第三个主视图 `?view=orch`（选中态 `?agent=<inst_id>`，可分享/刷新恢复）：左树图（自绘 SVG tidy 布局，零图库）+ 右单 Agent 对话面板。设计/计划：`docs/superpowers/specs/2026-09-11-agent-orch-board-design.md` + `docs/superpowers/plans/2026-09-11-agent-orch-board.md`。

- 新端点：`GET /api/sessions/{id}/agents/{aid}/messages?before_seq&after_seq&limit`（完整消息历史 + mailbox 留痕）、`POST /api/sessions/{id}/agents/{aid}/message`（用户直连）。
- 用户直连状态机（`ReactService.MessageAgent` → `subagent.Dispatcher`）：`running + activity_kind=child_wait` 经 `InjectUserMessage` 投邮件并 poke 唤醒；终态（done/failed/cancelled/delivered-unverified）经 `Tree.Reopen` + `ReviveWithMessage` 同 ID 重跑（种子=原任务+上轮结果+用户消息，父收「复活返工」通知）；`running` 其他 → 409 `ErrAgentBusy`；paused/idle/meta/未接线 → 409 `ErrAgentNotDirectable`。
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
- Initial plugins: `web_search` (mcp, stdio `uvx free-search-mcp`, enabled by default; requires `uv/uvx` — missing binary degrades gracefully with a logged reason) and `computer_use` (mcp, stdio `npx computer-use-mcp`, **disabled by default**; all tools marked `Destructive()` → approval guard chain). `computer` sandbox service (Xvfb + noVNC) available in `docker/docker-compose.yml` behind the `computer` profile.
- Role visibility: plugin `settings.roles` (default `["*"]`) restricts which roles see plugin tools; adapters filter as static whitelist ∪ dynamic plugin visibility.
- Tests: `backend/internal/plugins/*_test.go`, `mcpbridge/bridge_test.go` (spawns a mock MCP server child process), `server/plugins_test.go` (API loop). `go test -race` needs a C compiler (not available in this env).
