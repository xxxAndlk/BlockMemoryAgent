# 12 TUI 终端界面

> 只从代码还原。代码：`backend/internal/tui/`（13 个非测试文件）+ `backend/cmd/tui/main.go`。
> 技术栈：bubbletea v1 单模型应用 + lipgloss 渲染 + bubbles/viewport。

## 目录

- [12.1 职责与接线方式](#121-职责与接线方式)
- [12.2 文件清单](#122-文件清单)
- [12.3 核心数据结构](#123-核心数据结构)
- [12.4 启动与主循环](#124-启动与主循环)
- [12.5 键位与输入分发](#125-键位与输入分发)
- [12.6 面板体系](#126-面板体系)
- [12.7 与后端的完整接线](#127-与后端的完整接线)
- [12.8 配置项/常量/环境变量](#128-配置项常量环境变量)
- [12.9 隐含约定与坑](#129-隐含约定与坑)

---

## 12.1 职责与接线方式

TUI 是 BlockMemoryAgent 的终端前端（bubbletea v1 单 `Model`）：

1. 会话视图（列表/对话流/流式输出/实时状态条目）；
2. 观测面板（右侧执行计划 + Agent 编排树；弹窗：帮助/计划/Agent/完整记录/详情/模型切换/新增模型表单）；
3. 输入与命令（多行输入、历史、Alt+V 图片/视频附件、斜杠命令、澄清问答卡单题+批量分页）；
4. **混合接线**（最易误判的一点）：`cmd/tui/main.go` 复用 `bootstrap.Build` 装配同一套后端，然后进程内起一个只绑 `127.0.0.1:0` 的本地 Gin 服务暴露与生产同构的 `/api/sessions*` 路由 ——**读路径走进程内 `agent.Agent` facade，写路径走本地 HTTP**（`cmd/tui/main.go:176-217`、`input.go:922-967`）。澄清答复、消息发送、停止/取消等全走 HTTP；`List/Get/Stream/Tree/ListAgents/Board/CreateSession/Shutdown/SummarizeTaskTitle` 走进程内。

## 12.2 文件清单

| 文件 | 职责 |
|---|---|
| `cmd/tui/main.go` | 入口：runewidth 修正 → flag/BMA_HOME → 严格校验 → 文件日志 → `bootstrap.Build` → 本地 Gin → `tui.NewModel` → `tea.NewProgram` → 退出清理 |
| `model.go` | 顶层 Model、Init/Update/View、tick 节流刷新、键/鼠标分发、布局预算、sharedState |
| `view.go` | 顶层布局、顶栏、右侧双面板外壳、Token 栏、通用文本工具（wrapToWidth/hardClipLine/fitFrameLines） |
| `chat_panel.go` | 对话 viewport：item 收集裁剪（`maxChatItems=100`）、内容构建（前缀着色）、滚动/锚定/滚动条拖拽 |
| `helpers.go` | `chatItem` 与事件→展示行映射（`eventChatItem`）、去重/合并/聚合、轻量 Markdown、看板快照合成 |
| `agent_tree_panel.go` | 权威 Tree() → 扁平节点 → 卡片式编排面板渲染（分叉连接线/子卡列/滚动开窗） |
| `overlay_panel.go` | 弹窗状态机（help/plan/agents/log/detail/model）+ 模型切换全流程 |
| `clarify_panel.go` | 问答面板单题/批量 + 光标/草稿计算 |
| `input.go` | 输入栏按键、澄清快捷键、斜杠命令路由 `submitInput`、HTTP postJSON/getJSON、createSession |
| `input_bar.go` | InputBar 数据结构与 render（runes/cursor/history/附件/多行折叠） |
| `keys.go` | 面板/弹窗/输入模式常量 + `fullHelpText` 帮助全文 |
| `clipboard.go` | Alt+V：Windows powershell 读剪贴板位图（PNG）与 FileDropList（视频路径） |
| `task_brief_cache.go` | 长任务标题 LLM 摘要缓存（信号量 5、3s 超时、失败截断兜底） |
| `styles.go` | Tokyo Night 色板 + Styles 聚合 |

## 12.3 核心数据结构

### Model（model.go:24-145）

- 依赖：`agent agent.Agent`（facade）、`dagHandler *server.DAGHandler`（**构造注入但从未读取**，预留）、`httpAddr`（本地 Gin，空回退 `localhost:10010`）、`modelName`、`workDir`。
- UI：`styles`、`focus`（panelChat/panelInput）、`sessions []*server.Session` + `sessionsCursor`（**初始 -1 = 不选中**）、四个面板结构（`chatPanel/inputBar/overlayPanel/agentTreePanel/taskBriefCache`）、token 累计四元组、`rightPanelForced`（0/1/-1）、`planScroll`/`agentScroll`、`startedAt`。
- 二次确认武装：`quitArmedUntil`（Ctrl+C 3s）、`stopArmedUntil`（ESC 2s）。
- 澄清态：`clarifySel/clarifyCursor/clarifyID/clarifyPage/clarifyDrafts`。
- 流：`streamEvents chan agent.Event`（cap 16）+ `streamCancel`。
- 刷新：`tickCount`/`dirty`（高频事件只置脏、tick 合并重绘）。
- 模型弹窗：`modelStage`（0 角色/1 模型/2 思考档/3 新增表单）+ 各选择态 + `modelForm [4]textinput`。

### sharedState（model.go:152-247）—— 值语义陷阱的解药

bubbletea `Update` 是值语义：后台 goroutine 直接写 Model 字段会落到废弃副本。故"后台写、主循环读"的状态放指针共享结构：`flash`（2s/5s）、`pendingSelectID`（createSession goroutine → 主循环消费后 refreshSessions+selectSession）、`pendingOverlayTitle/Lines`（/worktree 异步结果 → 主循环开弹窗）。

### 其他

- `ChatPanel`：`vp viewport.Model` + `cursor`（item 级，`itemOffsets` 反查）+ `followBottom/anchorUser/pendingScrollToUser/pendingFirstMessage` + 重建判据（`lastItems/lastWidth/lastLiveTitle`）+ `goalBarH`（目标栏行数）+ 滚动条拖拽态。
- `AgentTreePanel{nodes []agentTreeNode}`：`agentTreeNode`=depth/parentID/instID/name/domain/roleType/status/goal/summary/err/isClarify/createdAt/activity。
- `OverlayPanel{mode/title/lines []string/cursor/kind}`：通用行级滚动容器。
- `InputBar`：`mode`（normal/clarify/interrupt/enqueue）+ `runes []rune`+cursor（rune 级编辑）+ `history map[string][]string`（**按会话分桶**）+ `pendingImages []tool.ResultImage` + `pendingVideos []agent.WireVideo`。
- `chatItem{title/detail/rawDetail/timestamp/isEvent/role}`；`clarifyBatchDraft{sel,text}`（文本优先）；`TaskBriefCache`（mu **指针**保证值拷贝共享锁 + sem cap 5）。
- 自定义 tea.Msg：`tickMsg`/`sizePollMsg`/`streamEventMsg`/`clipImageMsg`/`clipVideoMsg`/`modelSwitchDoneMsg`/`modelAddedMsg`。

## 12.4 启动与主循环

### 启动

`NewModel`（model.go:258-288）→ `refreshSessions()`（`agent.List` → `server.ToServerSession`）→ `Init` → `tea.Batch(tickCmd(100ms), sizePollCmd(1s), streamCmd)`。

- `sizePollCmd`：1s 轮询 `term.GetSize`（**Windows 无 SIGWINCH 的补偿**），变化则合成 `WindowSizeMsg` 走统一 resize 路径（model.go:338-368）。
- `streamCancel`/`startStream`（model.go:440-474）：`agent.Stream(ctx, sid)` 取通道 → goroutine 投递到 `streamEvents`；**缓冲满静默丢弃**，靠 tick 的 `agent.Get` 兜底。

### Update 分发（model.go:576-663）

- `WindowSizeMsg`：设 vp.Width → `syncChatBodyHeight` → `rebuildChatContent` → 跟随则 GotoBottom。
- `tickMsg`（597-629）分档：`%5`（0.5s）`refreshSessions` + `syncInputMode`（**必须在 refresh 之后**，依赖最新会话状态）；`%10`（1s）`rebuildAgents` + `accumulateTokens`；`%2`（200ms）选中会话 running 时强制置脏（思考动效相位取墙钟）；`dirty || %10` → `refreshView`。
- `streamEventMsg`：只 `dirty = true`（不立即刷帧）。
- 鼠标：`handleMouse`（748-828）：弹窗滚轮 → 计划面板滚轮（步长 3）→ 编排面板滚轮 → viewport 滚轮 → 滚动条拖拽/跳转。

### refreshView（667-745）——顺序敏感

1. `syncChatBodyHeight()` 必须**先于一切滚动计算**（目标栏出现/消失改高度）；
2. 消费 `takePendingSelect` → refreshSessions + selectSession；
3. 消费 `takePendingOverlay` → openOverlay；
4. `collectChatItems()` 只算一次；
5. `pendingScrollToUser` → 找 `> ` 前缀条目滚动 → 置 `anchorUser=true`；
6. 内容重建判据（items/width/liveTitle）→ 重建后贴底则 GotoBottom；
7. 内容溢出视口**强制解除锚定**跟随底部；
8. 弹窗打开时 `refreshOverlay()`。

## 12.5 键位与输入分发

`handleKey`（831-961）优先级：

1. **全局**：`ctrl+l` 完整记录弹窗；`ctrl+b` 右栏循环（自动→强制显示→强制隐藏）。
2. **弹窗模式**：model 表单态全部按键给 form；否则 refreshOverlay；`ctrl+c` 退出；`esc/q` 关闭（model 逐段回退）；`↑/↓` 光标；`g/G` 首尾；`enter` 详情/切换；`2/3/4` 切弹窗、`1` 关闭。**弹窗内字母快捷键不生效（只认数字）**。
3. **输入栏焦点** → `handleInputKey`（input.go:28-290）。
4. **对话区**：1/esc、2/p 计划、3/a Agent、4/l 记录、s 模型、K 聚焦输入、/ 输入命令、? 帮助、enter 详情、j/k/g/G/pgup/pgdown/home/end 导航；**default：任意可打印字符直接进输入栏填充缓冲**（951-958）。

`handleInputKey` 关键分支：

- **最先拦截 `alt+v`**（36-38，必须在 KeyRunes 之前，否则 v 被插入）→ 读剪贴板：FileDropList 命中视频扩展名 → `clipVideoMsg`，否则位图 → `clipImageMsg`。
- `Esc`：running 且武装窗内 → `POST /stop`；running 未武装 → 武装 2s；否则清输入。
- `Enter`：Alt / Paste / **距上次按键 <80ms**（pasteEnterThreshold，粘贴 Enter 判定）→ 插入换行；批量澄清激活 → `clarifyBatchEnter`；澄清导航且输入空 → 提交选项（`POST /clarify`）；否则 pushHistory + `submitInput`。
- `Backspace/Delete` 多行时**一次性清空**；`↑/↓` 澄清导航优先否则历史；`←/→` 批量翻页。
- `KeyRunes`：澄清单字符快捷键优先；过滤控制字符（只留 `unicode.IsPrint`+`\n\t`，防 NUL 进 PG 报 22021）。

`submitInput`（634-840）命令路由顺序：附件收集（澄清/非 /new 命令丢弃附件并提示）→ 澄清自由文本直答（批量拦截）→ `/new` → 本地命令（/help /model /agents）→ 无会话时预置 pendingFirstMessage+createSession → 需会话命令（/status /clear /topic /topics /memory /clarify /cancel /interrupt /approval /enqueue /worktree /dag）→ **default：POST /api/sessions/{id}/message**（带 images/videos）。

传输：`postJSON` 异步 goroutine、3s 超时、网络错误重试 2 次、≥400 flash 状态码；`createSession` 后台创建成功写 `shared.pendingSelectID`。

## 12.6 面板体系

### 布局（view.go:28-75）

```
顶栏(1) / mainRow=对话[|右侧双面板] / 澄清面板(0 或 1+n) / Token 栏(1) / 输入栏(5) / [弹窗 height/3 min6]
```
`mainContentHeight = height - 1 - 1 - 5 - overlayH - clarifyH`（下限 4）。整帧逐行 `hardClipLine` + `fitFrameLines`（超高**保留第 0 行+末尾**）。右侧阈值：强制显示 或（有选中会话 ∧ width≥80）；对话区 65%，右侧按 **计划 45% / 编排 55%** 上下堆叠。

### 对话面板

- 顶部固定目标栏（`🎯` + currentGoalText：看板 Goal → 最后一条用户消息 → 会话 Goal）。
- 条目流水线 `chatItemsResolved`（helpers.go:324-486）：Messages → memory_recall 只留最后 2 条 → Events 经 `eventChatItem` 映射 → 时间排序 → **工具调用/结果合并**（`mergeToolCallPairs`）→ compact 时聚合（`[✓] Tool × N`）→ 去重（与最终答复重复的完成事件、相邻重复 assistant）→ running 时**追加一条 live 条目**（流式文本 `▍` > ThinkingText > 正在执行工具 X > 等待子 Agent X > 盲文转圈"思考中···"）。
- 裁剪：`maxChatItems=100`，超出顶部插省略提示行。
- 着色：按 title 前缀分派（`> `=用户、`[●]/[✓]/[✗]`=工具、`🧠`/`💭`=暗色、`───`=话题切换、`❓`/`✅`=澄清、`✗ Error`、`✅`、默认助手 Markdown）。
- 滚动耦合：`itemOffsets`（j/k item 级）+ 滚动条拖拽映射（`goalBarH` 参与偏移）。

`eventChatItem` 事件映射要点（helpers.go:853-1018）：

| 事件 | 展示 |
|---|---|
| `Type==tool_exec` / `Kind==tool_call` | `[✓]/[✗]/[●] Tool: path`（非 verbose 隐藏调用行；子 Agent 追加 `· 名称`） |
| `Kind==think` | `💭 …` |
| `Kind==sub_agent_dispatch` | `🚀 派发领域 Agent: <role> — <任务摘要>` |
| `Kind==sub_agent_done` | `✓ 领域 Agent执行完成` |
| `Type==clarify` | `❓ …` / `✅ 答复: …` |
| `Type==system` | 仅"已达最大轮数"/"会话完成"两类保留，其余丢弃 |
| `Type==agent_done` | 转 assistant 条目渲染 |
| `Kind∈{llm_result,llm,intend,wait}` | `Agent: message` |

### Agent 编排面板

- `rebuild`：顶层 MetaAgent 手工节点 + `Tree()`（过滤上一轮终态节点：`Started < roundStart`；**剔除热驻 Idle**；按 ParentID 算 depth）+ `ListAgents` 回填活动证据小字（`tool:X → in X · active Ns ago`；`user_wait → awaiting user`）+ 待澄清占位节点。
- 名称解析：domain → `Domain+"领域"`；固定助手查 `fixedRoleDisplayNames` 表。
- 渲染：meta 卡居中（状态色边框 + 等待标注）→ 分支连接线（`│├┴┼┌┐`）→ 分支卡 + 子卡列（限高整卡粒度裁剪 + `… 还有 N 个`）→ 列数 ≤3（`minBranchColW=14`）→ 滚动开窗（`↑/↓ 还有 N 行`）→ 图例 `✓Done ●Running ◐Waiting`。
- 拥挤过滤：depth≥2 且 goal 空且 Done/Idle 不渲染；孤儿挂最后一个分支。

### 问答卡

- 触发：`awaiting_clarify` 且 `State.PendingClarify != nil`。
- 单题：Detail 前置（截 6 行）→ 问题（截 4 行）→ 选项（`○/●/☐/☑` + `❯` 高亮）→ 按键提示。`空格`（输入栏空时）/数字 1-9/`y/n`（confirm）提交；`「其他」`（`types.ClarifyOtherOptionID=="other"`）引导自由文本。
- 批量：`←/→` 翻页（`题 i/N · 已答 n/N ●●○○`）、快捷键只写当前页草稿、Enter 全答则 `buildClarifyBatchAnswers` 整组提交（任一题空白即拒绝）、否则跳下一未答题。
- 模式同步 `syncInputMode` 在 tick `%5`；`pc.ID != clarifyID` 时重置光标/选择集/页码/草稿。

### 弹窗

- 通用 render：宽 `w*4/5`、高 `h-2` 居中、以光标为中心开窗（`start=cursor-maxLines/2`）。
- 内容源：`buildPlanLines`、`buildAgentsLines`、`buildTranscriptLines`（全量 chatItems）、`agentTreePanel.buildLines()`；`overlayLog` 光标在末尾时保持跟随。
- **模型弹窗多段式**：stage0 角色（含 `[bound: id]` 标记）→ stage1 模型候选（**`selectable_roles` 白名单过滤**，末项 `＋ 新增模型…`）→ stage2 思考档（空/off/low/medium/high）→ Enter 异步 `SwitchModel`（含 ≤60s 连通性探测）→ `modelSwitchDoneMsg`；stage3 新增表单（provider/model/api_key/base_url）→ `AddModelEntry`。
- 接线：`m.agent.(agent.ModelManager)` 类型断言（agent/models.go:12-21）。

## 12.7 与后端的完整接线

1. **进程内 facade（读）**：`List`/`Get`（内存快照优先、旧会话懒恢复 `restoreOneSession`）/`Stream`（**不走 HTTP SSE**，SSE 是给 Web 的）/`Tree`/`ListAgents`/`Board`（无则 Tree 合成）/`CreateSession`/`SummarizeTaskTitle`/`Shutdown`。注意 `selectedSession()` 被多个渲染函数频繁调用，是隐性的每帧后端调用。
2. **本地 HTTP（写 + worktree 读）**：`gin` + `server.RegisterSessionRoutes` + `DAGHandler.RegisterRoutes` + `/health`（**硬编码 fake**）+ `/metrics`；监听 `127.0.0.1:0`；裸 net/http 客户端，**不发 Authorization**（同机免鉴权）。端点：`/message`、`/clarify`、`/cancel`、`/interrupt`、`/enqueue`、`/stop`、`/topic`、`/trust-mode`、`/worktrees/:aid/{merge|reject}`、`/worktrees`(+diff)、`/api/memory/search`、`/api/dag*`。
3. **澄清**：完全经 HTTP `POST /clarify`（单题 `{question_id, answer}`；批量 `{question_id, answers: []}`）；状态回流靠 `agent.Get` 的 `State.PendingClarify`。
4. **模型管理**：进程内类型断言（ModelManager），不走 HTTP。

## 12.8 配置项/常量/环境变量

- flags：`-config -roles -env -soul -skills -no-alt-screen`；未指定落 `config.HomeDir()`（`BMA_HOME`）。
- 环境变量：`BMA_HOME`；`CI`（非空禁 alt-screen）；`runewidth.DefaultCondition.EastAsianWidth = false`（全局，独占宽度算法依赖它）。
- 节奏：tick 100ms、尺寸轮询 1s、会话刷新 0.5s、面板 1s、运行态强制重绘 200ms、流缓冲 16、flash 2s/5s、Ctrl+C 窗 3s、ESC 窗 2s、HTTP 超时 3s、粘贴 Enter 阈 80ms、剪贴板超时 10s。
- 渲染：`maxChatItems=100`、verboseTools={WriteFile,RunCommand}、compact 输出 5 行、详情折叠 8 行、澄清问题 4 行/Detail 6 行、摘要阈值 60 字符、右侧 45/55、对话区 65%、overlay 高 height/3 min6、顶栏版本号硬编码 `v0.8.0`、欢迎页 Skills/MCP/Context 硬编码 38/12/128K。
- 媒体限流：≤4 图（单 ≤4MiB，base64 存储）、≤2 视频（路径引用）。

## 12.9 隐含约定与坑

1. **值语义 + 后台写入丢失**：后台写 Model 必须走 `sharedState`（新增"后台写、主循环读"状态照此办理）。
2. **map/slice/指针字段拷贝间共享**（Go 语义）：`history`/`clarifyDrafts`/`itemOffsets`/`shared`/`taskBriefCache.mu`（故意指针共享锁）。
3. **刷新节流契约**：`streamEventMsg` 只置 dirty；thinking 动效靠 `%2` 墙钟相位；`%5` 的 `syncInputMode` 必须在 refreshSessions 之后。
4. **Windows 尺寸补偿**：无 SIGWINCH，删 `sizePollCmd` 会导致最大化后布局错乱。
5. **整帧必须等于终端高度**：`hardClipLine`+`fitFrameLines`；新增面板高度必须同步进 `mainContentHeight`（历史 bug：InputBar 预算算 3 行实为 5 行）。
6. **目标栏高度双口径**：`syncChatBodyHeight` 必须在 Update 路径维护（View 值接收者上的修正会丢），否则末行永远看不到。
7. **`selectSession` 不重置 `pendingScrollToUser`**；`pendingFirstMessage` 只在服务端 Messages 含同内容后清。
8. **事件流可丢**（cap 16）：最终一致靠 tick 的 `agent.Get`。
9. **按键与焦点互斥**：对话区快捷键（1/2/3/4/j/k/q/s/…）在输入栏焦点下全部失效；弹窗内字母不生效。
10. **Alt+V 拦截顺序**必须在 KeyRunes 之前。
11. **粘贴 Enter 是启发式**（<80ms 判定），终端快速连按会误判。
12. **多行删除=一次清空**。
13. **澄清抢键条件**：输入栏为空才消费空格/数字/y/n；模式同步有 ≤500ms 延迟窗（tick %5）。
14. **批量澄清必须全答**才能提交；批量激活时拦截自由文本。
15. **图片载荷必须 base64**（raw PNG 会被静默丢弃）；视频只传宿主路径。
16. **附件生命周期**：全量替换 runes 的路径（历史浏览/清空/模式切换/提交）都必须 `clearPendingAttachments`；澄清/斜杠命令不带附件。
17. **`boardSnapshot` 真相源优先级**：`agent.Board` > `agent.Tree` 合成（Idle→Done、Paused→Blocked、Unverified→Unverified）> 空。
18. **上一轮残留过滤靠最后一条用户消息时间戳**（`currentRoundStart`）；零值不过滤。
19. **渲染层"标题前缀即类型"**：`buildContent` 靠 `strings.HasPrefix(title, ...)` 分派样式；新增展示前缀必须同步加分支。
20. **`eventChatItem` 的 type/kind 混用**：`tool_exec/agent_done/system/clarify/error` 判 Type；`tool_call/think/sub_agent_*/llm*` 判 Kind；`inflightTool`/`waitingSubAgent` 依赖两处配对，改事件分类必须一起改。
21. **`formatMarkdown` 是自研轻量解析**（不支持嵌套/未闭合标记原样保留）。
22. **`wrapToWidth` 保留 ANSI**：编排卡"先折行再逐行着色"；对话区"先着色再折行"——两条路径次序相反，别混。
23. **弹窗是"底部追加 + 预留高度"而非真覆盖**；打开会压缩主内容区。
24. **鼠标命中依赖固定行偏移**（顶栏 1 + goalBarH）；在顶栏与主内容间插行会让滚轮错位。
25. **`TaskBriefCache` 会触发真实 LLM 调用**（异步 3s 超时、失败静默、先截断兜底）；计划面板每行都走它。
26. **退出清理不可省**：`ln.Close()` + `Shutdown`；Ctrl+C 二次确认防误杀长任务。
27. **`/health` 是 TUI 私有假实现**（redis 恒 false "not configured in TUI"），不要拿它判断依赖。
28. **死代码清单**（考古时别当活的）：`clipChat`/`renderPlanBar`/`formatTopBar`/`agentTreePrefix`/`renderAgentPanel`/`NewAgentTreePanel`/`NewOverlayPanel`/`chatItems`/`planTaskDomain`/两份同名 `deriveDomainTaskStatuses`/`Model.renderScrollbar`；死字段 `dagHandler`（只存不读）、`overlayDAG`/`overlayRuntime`（预留）、`inputInterrupt`/`inputEnqueue`（提示符分支存在但无人设置）；`model.go:1174-1176` 有孤立注释块（函数已搬走）。
29. **两处"本地兜底合成"使 `hasPlan()` 恒真**：有选中会话就有"直接执行"卡片。
30. **行内代码样式与快捷键栏**：对话区行内代码去背景改字色（终端行内高亮会碎成一格一格）；底部快捷键栏已删（帮助弹窗 `?` 承载全部键位；`fullHelpText` 是唯一权威键位清单）。
