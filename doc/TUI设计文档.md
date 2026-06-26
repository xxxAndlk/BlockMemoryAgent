# BlockMemoryAgent 终端 TUI 设计文档

> 目标：基于当前项目结构与已落地/规划功能，重新设计 `cmd/tui` 终端 UI，使其能在
> 一个进程内同时观测与操控四层 Agent 编排、记忆管线、Skill 库、看板/邮箱、
> DAG 调度、人机对话、抢占/队列、domainAgent 归档等全部能力。
>
> 文档同时是「重建 TUI」的施工蓝图：每一节都给出面板、键位、数据源与渲染规则，
> 落地时按章节顺序实现即可。

## 1. 背景与现状

### 1.1 当前 TUI 现状（`backend/cmd/tui/main.go`）
- 框架：`bubbletea` + `lipgloss`，Tokyo Night 配色。
- 入口：直接在进程内构造 `ThreeLayerGraph` 并 `Invoke`，不连 HTTP 服务。
- 已有面板：角色树、会话状态、事件日志、统计、调用栈、会话摘要。
- 缺口：
  - 仅展示单会话内状态，无会话列表切换。
  - 不展示 TaskBoard / Mailbox / Watchdog / Skill 装配明细。
  - 不支持人机对话答复（特性5）、抢占中断/队列注入（特性6）。
  - 不展示 DAG 调度（特性1）、domainAgent 归档与复用（特性4）、块记忆检索（特性3）。
  - 不展示 Agent 运行时动态配置（特性2）。

### 1.2 项目能力盘点（TUI 必须覆盖）
| 能力 | 来源 | 数据通道 |
| --- | --- | --- |
| 四层 Agent 编排 | `internal/graph` | 进程内 `ThreeLayerState` |
| 任务看板 | `internal/board` | `Runtime.Boards` |
| 邮箱 | `internal/mailbox` | `Runtime.Mailbox` |
| Skill 装配 | `internal/skill` | `Runtime.Skills` |
| Watchdog | `internal/watchdog` | `Runtime.Watchdog` |
| 块记忆归档/检索 | `internal/memory` + `store` | `pgBlockMemoryAdapter` |
| domainAgent 归档 | `internal/store/domain_archive.go` | `PostgresStore` |
| 人机对话 | `types.ClarifyRequest` + `/api/sessions/{id}/clarify` | `state.PendingClarify` |
| 抢占中断 / 队列注入 | `internal/cmdqueue` | `Runtime.CmdQueue` |
| 智能路由 | `MetaAgent.classifyComplexityLLM` | `state.DirectExecute` |
| DAG 调度 | `internal/dag` | `dag.Scheduler` + `dag_jobs` 表 |
| 跨会话历史 | `store.RecentSessionHistories` | `pgHistoryAdapter` |
| Agent 运行时参数 | `config.AgentConfig` | `Runtime.AgentCfg` |

## 2. 总体布局

终端视口分为 5 列 + 1 行底部输入栏 + 1 行底部帮助栏。所有面板支持焦点切换；
焦点面板用 `FocusBorder`（`#7AA2F7`），非焦点用 `BlurBorder`（`#414868`）。

```
┌────────────────────────────────────────────────────────────────────────────────────┐
│ BlockMemoryAgent TUI   session-7  ●running  step 23/200  tok in=12k out=3k         │ ← 标题栏
├──────────────┬──────────────────┬──────────────────┬──────────────────┬────────────┤
│              │                  │                  │                  │            │
│  Sessions    │  Agent Tree      │  Workspace       │  Memory          │  DAG       │
│  (会话列表)   │  (角色树+调用栈)  │  (Board/Mail/    │  (Block/Archive/ │  (调度视图) │
│              │                  │   Watchdog/Skill) │   History)       │            │
│              │                  │                  │                  │            │
├──────────────┴──────────────────┴──────────────────┴──────────────────┴────────────┤
│ > _                                                                                │ ← 输入栏
├────────────────────────────────────────────────────────────────────────────────────┤
│ Tab 切换  Enter 选中  i 中断  e 注入  c 答复  t 触发DAG  ? 帮助  q 退出              │ ← 帮助栏
└────────────────────────────────────────────────────────────────────────────────────┘
```

### 2.1 焦点模型
- 全局焦点变量 `focus panel`，取值 ∈ {`sessions`, `tree`, `workspace`, `memory`, `dag`, `input`}。
- `Tab` / `Shift+Tab` 在前 5 个面板间循环；`/` 直接跳到输入栏；`Esc` 从输入栏回到上一个面板。
- 仅焦点面板响应 `j/k` 滚动与 `Enter` 选中。

### 2.2 视口自适应
- 终端宽度 < 160 列时，DAG 面板折叠为底部抽屉（`Shift+g` 召唤）。
- 终端高度 < 30 行时，Workspace 面板的 Watchdog 子页签隐藏，仅保留 Board+Mail。
- 所有面板内部支持 `j/k` 上下滚动、`g/G` 跳首尾、`Ctrl-d/Ctrl-u` 半屏翻页。

## 3. 面板详细设计

### 3.1 Sessions 面板（会话列表）
**职责**：列出内存 + Postgres `session_history` 中的全部会话，支持切换观察对象。

**数据源**：
- 进程内：`SessionManager.ListSessions()`（已恢复的历史会话也在其中）。
- 后台任务：每 5s 调 `pgStore.RecentSessionHistories(ctx, 200)` 补齐已淘汰会话。

**渲染**：
```
▸ session-7  ●running  3 domains   2m ago
  session-6  ◐awaiting_clarify    8m ago   ← 特性5：人机对话挂起
  session-5  ◷enqueue pending     15m ago  ← 特性6：队列注入待消费
  session-4  ✓completed  2 agents 1h ago
  session-3  ✗error  timeout       2h ago
```
- 状态图标：`●running` / `◐awaiting_clarify` / `[tidle]` / `✓completed` / `✗error`。
- 当前观察的会话前缀 `▸`，其余前缀空格。
- 行宽超过面板宽度时，右侧省略号；`Enter` 在 Tree/Memory 面板加载该会话状态。

**键位**：
- `Enter`：把该会话设为观察对象，所有面板刷新。
- `i`：对该会话触发抢占中断（弹出行内输入框收集新指令，回车提交到 `/api/sessions/{id}/interrupt`）。
- `e`：对该会话触发队列注入（同样弹出行内输入框，提交到 `/enqueue`）。
- `r`：刷新列表。
- `d`：删除已完成会话（仅内存删除，DB 历史保留）。

### 3.2 Agent Tree 面板（角色树 + 调用栈）
**职责**：展示当前观察会话的四层 Agent 实例树、状态、调用栈、当前活跃块。

**数据源**：
- `Session.State.ActiveBlocks` / `CompletedBlocks` / `CallStack`。
- `registry.GetInstancesBySession(id)` 取实例列表。
- `state.PendingClarify` 非空时，在 MetaAgent 节点下挂一个 `❓ Clarify` 子节点。

**渲染**：
```
MetaAgent [session-7]                    ●active  step 23
├─ DomainAgent 商城页面                  ●active  block_商城页面_0
│  ├─ SkillSubset: ui_fix, code_write    ← 特性2：装配的 Skill 子集
│  ├─ ❓ Clarify pending                 ← 特性5：等待用户答复
│  ├─ Assistant code_assistant           ✓done    wrote snake.py
│  └─ Assistant ui_assistant             ●active  CSS inspect
├─ DomainAgent 订单模块                  [tidle]  block_订单模块_1
│  └─ (pending)
└─ DomainAgent 用户模块                  ✓done    block_用户模块_2

CallStack: Meta → Domain[商城] → Assistant[ui_assistant]
```
- 颜色：Meta 红、Domain 黄、SubDomain 蓝、Assistant 绿（沿用现有 `TreeMeta/Domain/Sub/Assist` 样式）。
- 状态徽标：`●active` / `[tidle]` / `✓done` / `✗error` / `⏳waiting`。
- 特性5 澄清节点用 `❓` 图标 + 黄色高亮，`Enter` 跳到输入栏并预填 `/clarify <question_id> `。
- 特性6 抢占/注入事件在树根节点用红色短闪提示（`Interrupted` / `Enqueued: <text>`）。

**键位**：
- `Enter`：在 Workspace 面板聚焦该 Agent 的看板任务与邮件。
- `s`：弹出该 DomainAgent 的 Skill 子集详情浮层（覆盖整屏，`Esc` 关闭）。
- `a`：查看该 Agent 的归档信息（跳到 Memory 面板的 Archive 子页签并定位）。

### 3.3 Workspace 面板（看板 / 邮箱 / Watchdog / Skill）
**职责**：聚合当前会话的运行时协作组件，4 个子页签切换。

**数据源**：
- Board：`Runtime.Boards.Get(sessionID).Snapshot()`。
- Mail：`Runtime.Mailbox.Peek(instID)` + `DrainBroadcast()`（Peek 不消费，DrainBroadcast 消费广播桶——为 TUI 单独加一个 `PeekBroadcast` 方法，避免消费）。
- Watchdog：`Runtime.Watchdog.History()` 按 blockID 过滤。
- Skill：`Runtime.Skills.GetForAgent(instID)`。

**子页签**：
1. **Board**：顶层目标 + 子任务列表（状态/负责人/优先级）。完成率进度条。
2. **Mail**：邮件列表（from/to/subject/priority/created_at），点击查看 body。
3. **Watchdog**：历史决策（agent_id/tokens/level/reason/suggested/occurred_at），按 level 着色（Evict 红 / Compress 黄 / Warn 灰）。
4. **Skill**：当前装配的 Skill 子集表格（SkillID/Domain/Description/Cost/Tags）。

**键位**：
- `1/2/3/4` 切换子页签。
- `j/k` 行内导航，`Enter` 查看详情浮层。
- `m`：手动触发 `Watchdog.Check` 评估当前块（仅展示，不写入历史）。

### 3.4 Memory 面板（块记忆 / 归档 / 跨会话历史）
**职责**：覆盖特性3（块记忆检索）、特性4（domainAgent 归档复用）、跨会话历史。

**数据源**：
- 块记忆：`pgStore.SearchKnowledgeByType(ctx, "block_memory", embed.PseudoEmbed(query, dim), topK)`。
- 归档：`pgStore.SearchDomainArchive(ctx, domain, goal, topK)`。
- 历史：`pgStore.RecentSessionHistories(ctx, limit)`。

**子页签**：
1. **BlockMemory**：
   - 顶部一个查询输入框（聚焦面板时 `/` 进入输入）。
   - 下方按相似度排序的块记忆列表，每行 `[sim=0.84] 商城页面 / 写贪吃蛇...`。
   - `Enter` 查看完整 content + meta。
2. **Archive**：
   - 列表：`[weight=5] 商城页面  expires 6d  skills=[ui_fix,code_write]`。
   - `Enter` 展开归档详情（上下文摘要 + Skill 列表 + 来源 session）。
   - `r`：手动 `BumpDomainArchiveWeight`（测试用，需二次确认）。
   - `x`：手动删除该归档。
3. **History**：
   - 最近 N 条会话历史（目标/摘要/工具调用路径）。
   - `Enter` 把该历史会话设为观察对象（若内存不在则从 DB 恢复最小记录）。

**键位**：
- `1/2/3` 切换子页签。
- `/` 进入查询输入（仅 BlockMemory / Archive）。
- `Ctrl-r`：手动触发 `CleanupExpiredDomainArchives`，弹窗显示删除条数。

### 3.5 DAG 面板（特性1 调度视图）
**职责**：展示 DAG 定义、运行中实例、任务依赖图。

**数据源**：
- 定义列表：`dag.Store.ListDAGs(ctx)`（HTTP `GET /api/dag`）。
- 运行中：`dag.Scheduler.Snapshot()`（HTTP `GET /api/dag/running`）。

**渲染**：
```
DAG: deploy_pipeline   cron=5m   enabled
  ▸ t1 build_image       ✓completed  session-12  3m ago
    t2 scan_image        ●running    session-13  1m ago
    t3 deploy_staging    [tidle]     waits: t2
    t4 deploy_prod       [tidle]     waits: t3

[+ n] create  [e] edit  [t] trigger  [x] delete
```
- 每个任务按依赖关系缩进显示（拓扑序）。
- 状态徽标同 Agent Tree。
- 依赖未满足的任务用 `waits: t2,t3` 标注。

**键位**：
- `t`：触发当前选中的 DAG（`POST /api/dag/{id}/trigger`）。
- `e`：编辑 DAG（弹出 YAML/JSON 编辑浮层，保存到 `POST /api/dag`）。
- `n`：新建 DAG（同上）。
- `x`：删除（二次确认）。
- `r`：刷新列表。

### 3.6 输入栏（底部）
**职责**：统一入口，承接用户指令、人机对话答复、抢占中断、队列注入四种动作。

**模式**（前缀决定路由）：
- 无前缀：发送到当前观察会话 `/api/sessions/{id}/message`。
- `/clarify <question_id> <answer>`：发送到 `/api/sessions/{id}/clarify`。
- `/interrupt <new goal>`：发送到 `/api/sessions/{id}/interrupt`。
- `/enqueue <text>`：发送到 `/api/sessions/{id}/enqueue`。
- `/dag trigger <id>`：发送到 `POST /api/dag/{id}/trigger`。
- `/dag new <json>`：发送到 `POST /api/dag`。
- `/config show` / `/config set <key> <value>`：查看/修改运行时 `AgentCfg`（仅内存生效，重启失效）。

**键位**：
- `Enter`：提交。
- `↑/↓`：历史输入。
- `Tab`：自动补全（会话 ID、DAG ID、Skill ID）。
- `Ctrl-c`：清空当前输入。
- `Esc`：离开输入栏回到上一个面板。

### 3.7 帮助栏（最底部）
- 一行式键位速查，根据当前焦点面板高亮该面板的键位。
- `?` 弹出全屏帮助（`Esc` 关闭）。

## 4. 全局键位表

| 键 | 作用 | 适用焦点 |
| --- | --- | --- |
| `Tab` / `Shift+Tab` | 面板间循环切换 | 全局 |
| `1`–`5` | 直接聚焦 Sessions/Tree/Workspace/Memory/DAG | 全局 |
| `/` | 聚焦输入栏（Memory 面板下进入查询输入） | 全局 |
| `Esc` | 退出输入栏 / 关闭浮层 | 全局 |
| `j` / `k` | 当前面板上下导航 | 面板 |
| `g` / `G` | 跳首/尾 | 面板 |
| `Enter` | 选中 / 展开 | 面板 |
| `r` | 刷新当前面板 | 面板 |
| `i` | 抢占中断（当前会话） | Sessions/Tree |
| `e` | 队列注入（当前会话） | Sessions/Tree |
| `c` | 答复人机对话（预填 `/clarify`） | Tree（Clarify 节点） |
| `t` | 触发 DAG | DAG |
| `s` | 查看 Skill 子集 | Tree |
| `a` | 查看归档详情 | Tree/Memory |
| `?` | 全屏帮助 | 全局 |
| `q` | 退出 TUI | 全局（输入栏聚焦时改为退出输入栏） |

## 5. 数据流与刷新策略

### 5.1 进程内模式（沿用 `cmd/tui/main.go` 当前形态）
- TUI 进程直接持有 `ThreeLayerGraph` + `SessionManager`，所有面板读进程内对象。
- `tea.Tick` 每 200ms 触发一次 `model.update()`，从 `SessionManager` 拉最新 state。
- 工具回调 / 进度回调直接 push 到 `tea.Program` 的消息通道，无需轮询。

### 5.2 远程模式（可选，便于连 HTTP 服务）
- TUI 进程不构造图，改为通过 HTTP 客户端连 `:10010`。
- Sessions/Workspace/Memory/DAG 面板分别调对应 REST 端点。
- 实时事件走 SSE：`GET /api/sessions/{id}/stream`，把 `tea.Program.Send` 作为 SSE handler 的 sink。
- 远程模式下抢占/队列/澄清/DAG 触发全部走 HTTP，不依赖进程内对象。

> 建议保留两种模式，通过 `--remote <addr>` flag 切换。本地模式适合开发自测，
> 远程模式适合运维观测生产实例。

### 5.3 刷新频率
| 面板 | 频率 | 触发方式 |
| --- | --- | --- |
| Sessions | 5s | Tick |
| Agent Tree | 200ms | Tick + 事件回调 |
| Workspace | 1s | Tick |
| Memory | 按需 | 用户 `/` 查询触发 |
| DAG | 2s | Tick |
| 输入栏 | 即时 | 用户输入 |

## 6. 与新特性的对接细节

### 6.1 特性2（动态配置）
- DAG 面板旁边新增一个 `Config` 浮层（`Shift+c` 召唤），表格展示 `AgentCfg` 全部字段。
- 浮层支持 `e` 编辑单字段，回车后调 `/config set`（仅远程模式生效；本地模式直接改 `Runtime.AgentCfg` 指针）。
- 关键字段：`MaxSteps` / `SkillSetSize` / `ToolCallMaxRounds` / `RetryCount` / `LLMSoftTimeoutSec` / `ContextWindow` / `DomainArchiveTTLHours`。

### 6.2 特性3（块记忆检索）
- DomainAgent 触发检索时，在 Agent Tree 对应节点闪烁 `🧠 recalled` 徽标 1s。
- Memory 面板的 BlockMemory 子页签默认查询=当前会话的 `state.DomainGoal`，可手动改写。

### 6.3 特性4（domainAgent 归档复用）
- Agent Tree 中复用归档的 DomainAgent 节点前缀 `♻️`，悬浮显示来源 session + weight。
- Memory/Archive 子页签用进度条展示 `weight / DomainArchiveMaxWeight`，超阈值标红。

### 6.4 特性5（人机对话）
- 会话状态 `awaiting_clarify` 时，Sessions 面板行尾追加 `❓`，按 `c` 直接跳输入栏预填 `/clarify <id> `。
- Agent Tree 中 MetaAgent 下的 Clarify 节点持续闪烁直到答复。
- 输入栏 `/clarify` 模式下，Tab 自动补全当前 `PendingClarify.ID`。

### 6.5 特性6（抢占中断 / 队列注入）
- Sessions 面板行尾图标：抢占中断 pending 显示 `⏹`，队列注入 pending 显示 `⏵`。
- 中断触发后，Tree 面板顶部显示 1s 红色横幅 `Interrupted: <old goal> → <new goal>`。
- Workspace/Board 子页签保留中断前的快照 5s（灰色），便于用户对比上下文重置效果。

### 6.6 特性1（DAG 调度）
- DAG 面板独立列；运行中任务点击 `Enter` 跳到 Sessions 面板并选中对应 session。
- DAG 触发后 5s 内若 sessions 列表未出现新会话，面板标黄提示 `trigger failed`。
- 编辑浮层支持 YAML 语法：
  ```yaml
  id: deploy_pipeline
  name: 部署流水线
  cron: 5m
  enabled: true
  tasks:
    - id: t1
      goal: 构建镜像并推到 registry
      depends_on: []
    - id: t2
      goal: 扫描镜像漏洞
      depends_on: [t1]
  ```

### 6.7 特性7（智能路由）
- Agent Tree 根节点 MetaAgent 行显示路由判定标签：`simple` / `query` / `complex`。
- `query` 标签附加 `DirectExecute` 徽标，提示跳过子任务拆解。

## 7. 渲染规则与样式

### 7.1 配色（沿用 Tokyo Night）
| 用途 | 色值 | 字段 |
| --- | --- | --- |
| 标题/聚焦边框 | `#7AA2F7` | `Title` / `FocusBorder` |
| MetaAgent | `#F7768E` | `TreeMeta` |
| DomainAgent | `#E0AF68` | `TreeDomain` |
| SubDomain | `#7AA2F7` | `TreeSub` |
| Assistant | `#9ECE6A` | `TreeAssist` |
| 活跃状态 | `#9ECE6A` 加粗 | `TreeActive` |
| 完成状态 | `#565F89` | `TreeDone` |
| 警告 | `#E0AF68` | `LogWarn` |
| 错误/抢占 | `#F7768E` | 新增 `LogError` |
| 帮助栏背景 | `#1F2335` | `HelpBar` |

### 7.2 图标集
- 状态：`●active` / `[tidle]` / `✓done` / `✗error` / `⏳waiting` / `❓clarify` / `⏹interrupt` / `⏵enqueue`
- 复用：`♻️ archive`
- 记忆：`🧠 recalled`
- 路由：`[simple]` / `[query]` / `[complex]`

### 7.3 截断与换行
- 面板宽度不足时，文本按 rune 截断并加 `…`。
- LLM prompt / tool output 等长文本不在主面板展示，统一进入浮层 `Enter` 查看，浮层内支持 `j/k` 滚动与 `y` 复制到剪贴板。

## 8. 浮层与模态

| 浮层 | 触发键 | 内容 |
| --- | --- | --- |
| 帮助 | `?` | 全局键位表 + 当前焦点面板专属键位 |
| Skill 详情 | `s`（Tree） | SkillSet 全字段表格 |
| 归档详情 | `a`（Tree/Memory） | DomainArchiveRecord 全字段 + 上下文摘要 |
| DAG 编辑 | `e`（DAG） | YAML 编辑器，支持语法校验 |
| Watchdog 决策详情 | `Enter`（Workspace/Watchdog） | 单条决策的 reason/suggested 全文 |
| 工具结果详情 | `Enter`（Tree/Assistant 叶子） | 工具调用 stdout/stderr 全文 |
| Config | `Shift+c` | AgentCfg 表格 + 编辑 |
| 抢占中断输入 | `i` | 单行输入框，回车提交 |
| 队列注入输入 | `e` | 单行输入框，回车提交 |
| 答复输入 | `c` | 预填 `/clarify <id> `，回车提交 |

所有浮层 `Esc` 关闭；浮层打开时主面板不响应键位，避免误操作。

## 9. 落地施工顺序

1. **骨架重构**：把 `cmd/tui/main.go` 拆成 `model.go` / `views/*.go` / `keys.go` / `styles.go`，每个面板一个 view 文件。
2. **Sessions 面板** + 焦点切换框架（最小可运行版本，只展示会话列表）。
3. **Agent Tree** + 进程内 state 接入（替换现有角色树）。
4. **Workspace** 4 子页签（Board/Mail/Watchdog/Skill）。
5. **Memory** 3 子页签（BlockMemory/Archive/History）+ 远程 HTTP 调用。
6. **DAG** 面板 + 编辑浮层。
7. **输入栏** 4 模式 + 自动补全。
8. **特性对接**：把 §6 列出的 7 项徽标/闪烁/横幅逐项接入。
9. **远程模式**：SSE 客户端 + HTTP 调用封装。
10. **帮助/浮层/键位一致性校验**。

每步独立提交，便于回滚。建议先跑通 1–4 步拿到可用版本，再逐步叠加 5–9 步的高级面板。

## 10. 验收清单

- [ ] 5 个面板 + 输入栏 + 帮助栏同时可见，焦点切换流畅。
- [ ] Sessions 面板能展示全部状态（含 `awaiting_clarify` / 抢占/队列 pending）。
- [ ] Agent Tree 能看到 4 层 Agent + Clarify + 路由标签 + 复用徽标。
- [ ] Workspace 4 子页签数据正确，Watchdog 着色无误。
- [ ] Memory 面板能按查询文本召回块记忆与归档。
- [ ] DAG 面板能展示拓扑序、触发、编辑、删除。
- [ ] 输入栏 4 模式路由正确，Tab 补全可用。
- [ ] 浮层 `Esc` 关闭，主面板键位不受影响。
- [ ] 终端 resize 后布局自适应（< 160 列折叠 DAG，< 30 行折叠 Watchdog）。
- [ ] 进程内模式与远程模式均能跑通上述全部交互。
