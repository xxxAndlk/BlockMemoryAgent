# 工作流量产流水线：开发文档

日期：2026-10-16
状态：**已实施（2026-10-16 同日落地 M1-M5；M6 指标验收与遗留见 `doc/TODO.md` #27，落地实录见 `doc/变更.md` 2026-10-16 条）**
落地偏差记录：评委为纯文本轻量通道（视觉评委留 M3）；单 unit 单产物（多帧 prompts 批量未做）；升级 Agent 产物回收留 M3；TUI 只读列表未做。其余字段/表名/流程按本文落地。
上游设计：`docs/superpowers/specs/2026-10-16-workflow-batch-production-design.md`（下称"设计报告"，字段/表名/流程以本文为准落地，冲突时回改设计报告）
关系：与 `internal/dag`（定时任务 DAG）、`map_sub_agents`（同构批量派 Agent）互补不重叠，边界见 §12。

已拍板决策（2026-10-16 与需求方确认）：

- 配方 v1 做 **workflow_freeze 轨迹提取**（打样 → 冻结 → 量产闭环完整），不做"纯手写 YAML 过渡态"；
- 人审联系表 **本期全做**（accept / reject+理由 / 重摇 + 驳回理由回灌配方版本）。

## 1. 目标与非目标

**目标**：把设计报告 §3 的"配方 / 名册 / 运行"三元组落成代码——

1. 量产引擎：有界单位流水线（件 × 阶段，2~4 次轻量调用，不派完整 ReAct 子 Agent）；
2. 主 Agent 能力面：对话内打样 → `workflow_freeze` 冻结配方 → `workflow_run` 发起量产 → `workflow_status` 查进度；
3. 真正的"工作流编排"页：替换 `web/src/views/workflow/index.vue` Beta 占位，含配方库、运行列表、运行详情（名册×阶段网格）、联系表人审与配方演化。

**非目标**（继承设计报告 §8 YAGNI）：通用 DAG 拖拽编辑器、cron/webhook 触发器（那是 `internal/dag` 的地盘）、每件一个完整子 Agent、跨角色混批、物理删除 attempt/产物。

## 2. 总体架构

```
主 Agent 对话 ──workflow_freeze / workflow_run / workflow_status ──▶ tool.Registry
                                                                          │ Dispatch（ctx 带 session/agent）
Web 前端 ──/api/workflow/*──────────────────────────────────────────▶ server.WorkflowHandler
                                                                          │
                                                                              ▼
                                                              backend/internal/workflow（新包，与 internal/dag 平级）
                                                                Engine（调度+恢复+预算熔断）
                                                                ├─ Recipe 加载/校验（工作区 YAML 为真相源）
                                                                ├─ Prompter（不变量+变量槽+锚点 → 提示词）
                                                                ├─ Executor（合成→调生成工具→程序校验→模型评委→重摇/采纳）
                                                                ├─ Judge（CallLightweightWithRetry，JSON 逐条判定）
                                                                ├─ Escalation（Dispatcher.DispatchProgrammatic 升级真子 Agent）
                                                                └─ Store（workflow_runs / workflow_units，PG）
                                                                          │
                                              依赖注入：*tool.Registry、*model.ModelFactory、embed.Embedder、
                                                       store（PG）、*subagent.Dispatcher（导出薄封装）
```

**包落点论证**：不放 `internal/domain/workflow`——引擎要同时消费 `tool.Registry`、`model.Factory`、`store`、`subagent.Dispatcher`、`embed`，属于编排基础设施而非纯领域对象，与 `internal/dag` 同定位。包内不得 import `server`/`tui`/`agent`（沿用 `internal/dag` 的纪律；需要 agent 上下文键时用 `domain/tool` 的 `agentctx.go`/`sandbox.go`，或复用 `domain/subagent` 的取法，避免依赖环）。

## 3. 配方（recipe）模型与存储语义

**真相源 = 工作区文件** `.bma/workflows/*.yaml`（随项目持久、git 可版本化），不落配方库表。理由：YAGNI；版本 diff 靠 git；run 启动时快照配方全文进 `workflow_runs.recipe_yaml`，"这批是哪个标准下做的"不依赖配方文件历史。

YAML 规范（设计报告 §3.1，落地时允许缺省段）：

```yaml
id: beast_motion                 # 文件名即 id
version: 3                       # 每次修订 +1；workflow_freeze 与"驳回回灌"都会 +1
title: 异兽/炮台 动作与特效量产线
tool: ui_design__od_image_generate   # 生成工具名（含插件前缀，按 tool.Registry 实际注册名）
deliverable:                     # 交付契约：程序校验按此判
  type: sprite_sheet             # sprite_sheet | image | video（v1 程序校验全实现到 sprite_sheet/image）
  frames: 8
  cell: 128x128
  alpha: true
  format: png
  path: "assets/units/{{id}}/{{stage}}/"   # 路径模板，{{id}}/{{stage}} 替换
stages:                          # 件内串行；anchor_out 采纳件进锚点池供后续阶段/件引用
  - { id: concept, anchor_out: true }
  - { id: walk,    anchor_in: [style, family, concept], anchor_out: true }
  - { id: attack,  anchor_in: [style, family, concept, walk] }
  - { id: vfx,     anchor_in: [style, vfx_family], frames: 4 }
anchors:                         # 锚点池（i2i reference / first_frame 的参数值，工作区相对路径）
  style:      art/style_anchor.png
  family:     art/family_{{family}}.png       # 允许引用 item 槽位
  vfx_family: art/vfx_{{element}}.png
invariants: >                    # 不变量段（逐件相同，进提示词）
  …
slots: [id, name, element, family, body_plan, palette]
params: { size: 1024, reroll: 2, seed_policy: reroll_jitter, concurrency: 2 }
check:
  program: ["exists", "path_template", "format", "dimensions"]   # v1 内置校验器关键字，见 §6.1
  judge:   ["四肢/部件交替相位正确且可循环", "轮廓与概念锚一致", …]
  human:   ["动作张力（联系表横向对比）", …]
```

**名册（roster）**：`workflow_run` 参数内联（JSON 数组）或 `.bma/workflows/roster_*.yaml` 文件路径；一行一件，`stages` 逐件裁剪。

## 4. 运行（run）模型

### 4.1 表（`migrations/014_workflow.sql`，同步 `store/schema.go` 加 `EnsureWorkflowSchema` 并注册进 `ensureSchemas`）

```sql
CREATE TABLE IF NOT EXISTS workflow_runs (
  id            TEXT PRIMARY KEY,
  owner         TEXT NOT NULL,
  session_id    TEXT NOT NULL,             -- 发起会话（工作区/事件展示锚点）
  workdir       TEXT NOT NULL,             -- 配方与产物所在工作区
  recipe_id     TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_yaml   TEXT NOT NULL,             -- 启动快照
  roster        JSONB NOT NULL,            -- [{id,name,element,family,stages:[...]}]
  status        TEXT NOT NULL,             -- pending|running|paused|done|failed|aborted
  pause_reason  TEXT,                      -- budget_exceeded | user | circuit_breaker
  budget_cap    INTEGER NOT NULL,          -- 轻量调用次数上限（估算口径见 §5.4）
  spent_estimate INTEGER NOT NULL DEFAULT 0,
  concurrency   INTEGER NOT NULL DEFAULT 2,
  fail_fast     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS workflow_units (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL REFERENCES workflow_runs(id),
  owner         TEXT NOT NULL,
  item_id       TEXT NOT NULL,
  item_name     TEXT,
  stage         TEXT NOT NULL,
  attempt_no    INTEGER NOT NULL DEFAULT 1,
  status        TEXT NOT NULL,             -- pending|running|done|needs_review|failed|skipped
  artifact_path TEXT,
  check_result  JSONB,                     -- {program:{...}, judge:{items:[{item,pass,reason}]}, drift:{...}}
  reject_reason TEXT,
  idem_key      TEXT NOT NULL UNIQUE,      -- run_id+item_id+stage+attempt_no
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workflow_units_run ON workflow_units(run_id);
```

被否 attempt **只标不删**；重摇 = 新 attempt 行。owner 列硬约定（对齐 011 起的表纪律）。

### 4.2 状态机

- run：`pending → running ⇄ paused → done | failed | aborted`。done 要求全部 unit 终态且无 running/pending。
- unit：`pending → running → done | needs_review | failed`；`skipped`（名册裁剪/人审跳过）；reroll 生成新 pending attempt。
- 每次状态迁移写 `session_events`（kind=`workflow_unit`，detail_json 带 run_id/unit_id/from/to），监控页可见；后端 kind 常量进 `server/eventkind/kind.go`（顺带把 `sub_agent_dispatch` 等历史字面量收编），前端 `eventStyles.ts` 四处映射（`kindTagType/kindIcon/kindLabel/kindPhrases`）补 `workflow_unit`。

## 5. 引擎设计（`backend/internal/workflow/`）

### 5.1 文件组织

```
internal/workflow/
  recipe.go     # YAML 加载/校验（id/version/deliverable/stages/anchors/slots/check 完整性）
  roster.go     # 名册解析（内联或文件）
  prompt.go     # Prompter：invariants + slots[item] + anchors → 生成工具 args（prompts 数组多帧、reference 锚点参数）
  engine.go     # Engine：Start/Stop、Run 创建、调度、恢复、pause/resume/abort、预算/熔断
  executor.go   # 单位执行序列（§5.2）
  check.go      # 程序校验器（按 deliverable.type 分派，关键字见 §6.1）
  judge.go      # 模型评委（复用 CallLightweightWithRetry + evolver JSON 解析套路）
  escalate.go   # 升级通道（Dispatcher 导出薄封装）
  store.go      # workflowStore：CRUD + 状态迁移（行内乐观更新：UPDATE ... WHERE status=旧值）
  events.go     # session_events / 事件账本写入
```

### 5.2 单位执行序列（executor.go）

```
prompt/args ← Prompter.Synth(recipe, item, stage, anchors)      # 合成提示词+锚点参数
result ← tools.Dispatch(ctx, recipe.tool, args)                  # ctx 带 sessionID/agentID（tool 包 agentctx/sandbox 键）
art ← result 中登记的产物路径（工具输出约定：artifact 路径）
if !Check.Program(art, recipe.deliverable, stage):                # §6.1，免费
    reroll（attempt+1，seed/提示词微抖动，上限 recipe.params.reroll）→ 仍不过 → needs_review 或 failed
elif !Judge.Eval(art, recipe.check.judge, anchors):              # §6.2，便宜
    reroll（同上）→ 仍不过 → needs_review
else:
    采纳：artifact_path 落 unit；stage.anchor_out → anchors[stage] = art（写回运行期锚点池，后续 unit 可读）
    漂移检测（best-effort）：embed 余弦距离离群 → check_result.drift.flag（不阻断，仅标记）
    下一 stage（件内串行）
```

生成工具输出约定：v1 要求生成工具在 `tool.Result` 中给出产物路径（ui_design 插件现状即登记 artifact，需在实现期确认字段名，必要时给插件加"结构化产物路径"返回）。多帧一次调用（prompts 数组）由 Prompter 负责组装。

### 5.3 调度与并发

- 引擎一个 run 一个调度循环（goroutine + 信号量），件间并发 = `run.concurrency`（默认 2，配方 params.concurrency 可覆写）；**件内阶段串行**（锚点传播依赖）。
- 不占用 `max_concurrent_sub_agents` 池；该池只为升级出来的例外服务。
- 全局活跃 run 数默认 4（config 可配，`config.yaml` 新段 `workflow.max_active_runs`），超出入队 pending。
- `Engine.Start(ctx)` / `Stop()` 照 `dag.Scheduler` 先例（bootstrap.go:1261-1266 装配、:1392-1393 cleanup）。

### 5.4 预算与熔断

- `workflow_run` 启动前估算：`units 数 × (1 生成 + ≤reroll×2 + 1 评委) ≈ 轻量调用数`，连同 budget_cap 走 **ask_user 审批门**（复用 ask_user 审批 hook），确认才建 run。
- 执行中 `spent_estimate` 每 unit 完成累计；超 cap → run `paused`（pause_reason=`budget_exceeded`），resume 需用户提额。
- 早期熔断（fail_fast=true）：前 5 个到达终态的 unit 中失败 ≥3 → run `failed`（pause_reason=`circuit_breaker`），结论"配方疑似有问题，回打样期"，不把整批烧完（设计报告 §5.4 的制度化）。

### 5.5 重启恢复（硬要求：重启不丢批量）

- 启动扫描 `status='running'` 的 run 重新入调度；`running` 的 unit 标回 `pending`（幂等键 `idem_key` 不变，重跑生成新 attempt 行，旧行保留）。
- 权威全在表 + 事件账本，无内存权威。

### 5.6 升级通道（escalate.go）

- `unit` 连续失败（program+judge 重摇全耗尽）或遇到未知输入 → 默认标 `needs_review`；用户或策略可触发升级：派**真子 Agent**。
- 实现：给 `subagent.Dispatcher` 加导出薄封装 `DispatchProgrammatic(ctx, spec ProgrammaticSpec) (string, error)`（内部调 `dispatchOne`，dispatcher.go:3470；ctx 必须带父 agentID，权限/spec 门沿用）。升级 Agent 自然进 Agent 树/编排页/邮箱语义，占并发池。
- 升级 Agent 的产物回收：轮询其终态后把 artifact_path 写回 unit（v1 可只标 `needs_review` 由人审，Agent 升级产物回收放 M3）。

### 5.7 工具权限门

`roleToolGate` 默认关闭，引擎经 `tools.Dispatch` 的程序化路径默认放行（同 `verifyloop.ExecuteChild` 先例）。对 gate 开启的部署：bootstrap 对引擎 scope 调 `MountPreApprovedForScope(scope, roleID, 生成工具名)`（mount.go:75），语义 = 配置人工声明放行。不在引擎里绕门、不复制门逻辑。

## 6. 校验体系（v1 范围）

### 6.1 程序校验（免费，check.go）

按 `deliverable.type` 分派，关键字（recipe.check.program 声明式列出）：

- `exists` 文件存在；`path_template` 路径符合 `deliverable.path` 模板；`format` 扩展名/魔数匹配。
- `dimensions`（png：Go stdlib `image/png` decode 判宽高/alpha，无新依赖）；sprite_sheet 按 `cell × frames` 网格推整张尺寸。
- `video` 类型 v1 只做 exists/path/format，帧数校验待"交付物形态"拍板后补（设计报告开放问题 #1；校验器留接口）。
- 能程序判的绝不叫模型判。

### 6.2 模型评委（便宜，judge.go）

- `model.Factory.CallLightweightWithRetry(ctx, prompt)`（factory.go:871）+ evolver.go:87-115 的 ```json 围栏解析套路。
- 提示词含：judge 逐条清单 + 锚点图路径（评委读图走工作区文件路径，prompt 里给路径与判定标准）+ 输出契约 `{items:[{item, pass, reason}]}`，聚合 pass = 全部 pass。
- 视频类 v1 不 judge 或抽帧后按图 judge（M3 定）。

### 6.3 人审（批量级，联系表，见 §9.4）

### 6.4 漂移检测（best-effort，可降级）

`embed.Embedder.Embed`（App.Embedder 已导出）对采纳产物与锚点/组质心算余弦距离，离群标 `check_result.drift.flag=true`，不阻断流程；embed 失败静默降级（对齐既有 best-effort 风格）。

## 7. 主 Agent 能力面（3 个工具）

注册方式：仿 `subagent.Dispatcher.RegisterCallTool`，workflow 包提供 `RegisterWorkflowTools(r *tool.Registry, eng *Engine)`，bootstrap 在 `RegisterCallTool` 之后调用（bootstrap.go:602 附近）；meta 角色白名单在 `role/registry.go:188` 的 meta Tools 列表补三个名字（roles.yaml 覆盖处同步）。

### 7.1 `workflow_freeze` —— 打样 → 配方（设计报告 §4.2 落地）

- 入参：`title`、`recipe_id`（默认从 title slug）、`source`（`recent_adopted`，默认：从当前会话轨迹提取）、可选覆盖段（deliverable/anchors/invariants/judge 的人工修正）。
- 流程：抓本会话轨迹中**成功且被采纳**的生成调用（数据源：session_events 的 tool 事件 + 采纳产物登记）→ 登记锚点（采纳产物路径进 anchors）→ `CallLightweightWithRetry` 参数化（把提示词切"逐件变动段/不变段"，产出 slots 草案）→ 生成 YAML 草案 → **ask_user 过目确认（可编辑）** → 落盘 `.bma/workflows/<id>.yaml`（已存在则 version+1）。
- 不让模型凭空写配方，只切分/参数化已有轨迹（设计报告 §4.2 的根因约束）。

### 7.2 `workflow_run` —— 发起量产

- 入参：`recipe_id`、`roster`（内联 JSON 或 roster 文件路径）、`budget_cap`、`concurrency?`、`fail_fast?`。
- 流程：加载配方 → 展开名册 × 阶段 = units → 估算预算 → **ask_user 审批（估算 + cap + 件数）** → 建 run（快照 recipe_yaml）→ 返回 `run_id` 与 unit 数；引擎异步执行，工具立即返回。
- 幂等：同一 recipe+roster 重复调用产生不同 run（不合并）；中断恢复由 §5.5 保证。

### 7.3 `workflow_status` —— 查进度

- 入参：`run_id` 或 `latest`。
- 出参：run 状态、unit 总数/完成/待审/失败、失败原因聚合 topN、spent_estimate/budget_cap。
- run 终态（done/failed/aborted）时对主 Agent 可见一次摘要（事件账本驱动，v1 靠 status 查询即可，不新做推送）。

## 8. HTTP API（server.WorkflowHandler，照 server/dag.go 模式）

挂 `/api/workflow/*`（页面是一级入口而非会话子资源；产物经 `GET /api/sessions/:id/workspace/*path` 已有端点服务，详情页复用）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /workflow/recipes?workdir= | 扫 `.bma/workflows/*.yaml`，返回 id/title/version/交付契约摘要 |
| GET | /workflow/recipes/:id?workdir= | 配方 YAML 全文（含 version） |
| POST | /workflow/runs | 建 run（session_id/recipe_id/roster/budget_cap），返回 run_id |
| GET | /workflow/runs?session_id=&status= | 运行列表 |
| GET | /workflow/runs/:id | run 详情：run 行 + 全部 units（名册×阶段网格数据源）+ 配方快照 |
| POST | /workflow/runs/:id/pause · /resume · /abort | 运行控制 |
| POST | /workflow/units/:uid/accept · /reject · /reroll · /skip | 人审操作（reject 必带 reason） |
| POST | /workflow/runs/:id/feedback | 聚合本 run 全部 reject_reason → 轻量模型生成配方 v+1 草案 → 返回草案（确认落盘走 recipes 保存或复用 freeze 确认流） |

bootstrap：`app.WorkflowHandler.RegisterRoutes(api)`（router.go 仿 :69 的 DAGHandler 挂法）；nil 守卫返 503（照 dag.go:50）。run 终态 webhook：notifier.go 加 `NotifyRunFinal(runID, status, ...)`，payload `Type="workflow_run_final"`（复用 post 通道，不影响现有接收方）。

## 9. 前端（web/src/views/workflow/）

### 9.1 信息架构

- `index.vue` 改真页面：顶部 `el-tabs` = **配方库 | 运行列表**；运行行点进 `RunDetail`（子路由 `/workflow/runs/:id`）。
- `layout/index.vue:58-67` 的底部"工作流编排 Beta"硬编码入口：挪进 `menuGroups`（:117-145）或保留底部但去 Beta 标——实施时定，不再叫 Beta。

### 9.2 组件拆分

```
web/src/api/workflow.ts                 # 仿 api/dag.ts：Recipe/WorkflowRun/RunUnit 接口 + 端点函数
web/src/views/workflow/index.vue        # tabs 容器
web/src/views/workflow/RecipeList.vue   # el-table（仿 dag/index.vue:363）：id/title/version/tool/deliverable 摘要 + "查看 YAML"
web/src/views/workflow/RunList.vue      # el-table + statusType el-tag（口径复用 dag/index.vue:325）+ 进度（done/total）
web/src/views/workflow/RunDetail.vue    # 头部控制条（暂停/续跑/中止、进度条、预算 spent/cap）+ RosterGrid + 审查区
web/src/views/workflow/RosterGrid.vue   # 行=件、列=阶段；cell=状态点（口径复用 OrchMiniCanvas.vue:61 dotClass + STATUS_TEXT:78）
                                        #   点击 cell → 该 unit attempts 时间线 + 产物预览 + 操作按钮
web/src/views/workflow/ContactSheet.vue # 联系表：按 stage 列选批 → 该批全部采纳产物 css grid 拼贴（零后端）
                                        #   标准件/锚点置顶；逐格 accept / reject+理由 / reroll
```

### 9.3 状态刷新

无 run 级 SSE 先例，v1 用 `usePanelRefresh` 3s 轮询 `getWorkflowRun`（会话页同款 epoch 防串）；轮询仅在 run 非终态时开启。

### 9.4 审查流（本期全做）

1. 联系表初筛：模型评委已标 `needs_review` 与漂移离群的格高亮；人按 stage 列看整批拼贴做横向对比（设计报告 §6 的核心 UX 论点：并排看才暴露漂移）。
2. 逐格操作 → `/workflow/units/:uid/*`；reject 必填理由。
3. "回灌配方"按钮（详情页头部）：聚合理由 → 后端生成 v+1 草案 → 对话框确认 → 落盘（后续新 run 绑新版本；本 run 绑定版本不变）。

### 9.5 产物渲染注意

`ArtifactCard` 依赖会话页 provide 的 `useFileOpener`（脱离会话页降级）；联系表与网格里**自渲染** `<img :src="workspaceUrl(sessionId, path)">`（workspaceUrl 出自 `@/api/session`，即拿即用），不硬套 ArtifactCard。

## 10. 配置（config.yaml 新段）

```yaml
workflow:
  max_active_runs: 4        # 全局活跃 run 闸
  default_concurrency: 2    # 件间并发默认
  budget_cap_default: 500   # 轻量调用次数默认上限
  judge_timeout_ms: 30000
```

接入 `backend/internal/config/config.go`（仿 :312 的 max_concurrent_sub_agents 模式）。

## 11. 里程碑与验收

| 里程碑 | 内容 | 验收 |
|---|---|---|
| M1 存储+引擎骨架 | 014 migration、EnsureWorkflowSchema、recipe/roster 加载、run 创建、executor 单件全链路（合成→Dispatch→程序校验→评委→采纳/重摇）、unit 状态机 | 单测：程序校验各关键字、评委 JSON 解析、状态迁移；集成：手工建 run 跑 1 件 1 阶段采纳落盘 |
| M2 调度与硬化 | 并发调度、重启恢复、预算/熔断、升级薄封装、事件 kind、webhook、config 段 | 断进程重启续跑不重不漏（done 不重跑、running 回 pending）；超 cap 暂停；5 件 3 败熔断；webhook 收到 workflow_run_final |
| M3 主 Agent 工具 | workflow_freeze（轨迹提取+确认落盘）、workflow_run（估算+审批门）、workflow_status、meta 白名单、装配 | 对话打样 → "存成工作流" → 确认 → YAML 落盘 version 正确；发起 run 返回 run_id；status 摘要准确 |
| M4 API 全量 | WorkflowHandler 全端点 + bootstrap 挂线 | curl 走通 §8 表全部端点；unit 四操作状态正确 |
| M5 前端页面 | 配方库/运行列表/详情/网格/联系表/回灌配方 + 入口调整 | 3s 轮询状态正确；联系表横批 → reject 带理由 → 回灌生成 v+1 草案确认落盘 |
| M6 指标验收 | 埋点：单件调用数/token、一次通过率、重摇收敛率、人审时间/件 | 50 件批次单件成本显著低于 map_sub_agents 同批（设计报告 §9 指标） |

依赖顺序：M1→M2→M3 可并行 M4；M5 依赖 M4；M6 最后。每里程碑独立可提交。

## 12. 与既有能力的边界（防止再混淆）

| 能力 | 形态 | 场景 | 本期动它吗 |
|---|---|---|---|
| `internal/dag` 定时任务 | cron 触发，task=完整 ReAct 会话，depends_on 依赖 | 异构、低频、定时驱动（每日简报链路、周期巡检） | 不动 |
| `map_sub_agents` | N 个完整子 Agent，`{{item}}` 模板，聚合回传 | 同构中批量、每件需开放探索（侦察、评审） | 不动；它就是升级通道与对照基线 |
| 本流水线 | 有界单位执行 + 三层校验 + 联系表 | 同构大批量创作资产量产（游戏美术 18+ 件 × 3 阶段） | 新建 |

组合关系：DAG 节点 goal 可以是"发起某 run"；本流水线例外升级走 Dispatcher 进 Agent 树。反向不成立（拿 DAG/map 量产就是设计报告 §2 的反模式）。

## 13. 风险与默认值（开放问题收口）

| 项 | 默认决策 | 备注 |
|---|---|---|
| 交付物形态（视频 vs 关键帧） | 引擎不感知：deliverable.type 分派校验器；video 程序校验 v1 只做存在性/路径/格式 | 拍板后补帧数校验与 judge 抽帧 |
| 锚点传播链 | 按配方 anchor_in 声明，攻击/特效默认以行走采纳件为锚 | 已在 YAML 规范体现 |
| 配方归属 | 工作区 `.bma/workflows/`（git 版本化），run 快照兜底 | 全局库跨项目复用留待 Phase 2 后评估 |
| 联系表生成 | 前端 CSS grid 拼贴（零后端、可内审） | 需要可外发归档时再议后端拼图 |
| 引擎工具权限门 | 默认关（现状）；开启部署用 MountPreApprovedForScope | 实现期第一个接线点 |
| 多 run 费用闸 | max_active_runs=4 + 每 run budget_cap 硬上限 | 观测后再调 |
| TUI | v1 只读运行列表（复用 dag 页模式），审查归 Web | — |

## 14. 实施提醒清单

- migration 双轨：`migrations/014_workflow.sql` + `store/schema.go EnsureWorkflowSchema` + `ensureSchemas` 注册，两边都加（bootstrap.go:1455-1481）。
- 事件 kind 三处：`server/eventkind/kind.go` 常量、`eventStyles.ts` 四处映射、ExecutionLog.vue 过滤列表。
- run 不是 Agent：不挂任何 Agent 对话状态，生命周期全在表（设计报告 §3.3）。
- 引擎 goroutine 生命周期照 dag.Scheduler（Start(ctx) + cleanup Stop）。
- `dispatchOne` 未导出：不要 hack 包外调用，加 `DispatchProgrammatic` 薄封装。
- 生成工具产物路径：实现期第一个对齐点（M1 开工先确认 ui_design 插件工具 Result 的产物字段，缺则补）。
