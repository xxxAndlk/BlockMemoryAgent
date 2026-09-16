# 13 Web 前端

> 只从代码还原。代码：`web/src/`（Vue3 + Element Plus + Tailwind + vue-tsc；48 个 vue/ts 文件）。
> 本章按「框架与管理页」与「会话视图」两块合并成文。

## 目录

- [13.1 架构总览与文件地图](#131-架构总览与文件地图)
- [13.2 路由与布局](#132-路由与布局)
- [13.3 会话页数据流](#133-会话页数据流)
- [13.4 事件分类三档（turns.ts，核心）](#134-事件分类三档turnsts核心)
- [13.5 SSE 订阅机制](#135-sse-订阅机制)
- [13.6 澄清提交韧性](#136-澄清提交韧性)
- [13.7 编排视图与监控视图](#137-编排视图与监控视图)
- [13.8 右栏面板](#138-右栏面板)
- [13.9 API client 层](#139-api-client-层)
- [13.10 composables 逐个](#1310-composables-逐个)
- [13.11 通用组件与管理页](#1311-通用组件与管理页)
- [13.12 隐含约定与坑](#1312-隐含约定与坑)

---

## 13.1 架构总览与文件地图

会话页是单页多视图（`?view=chat|monitor`，默认 chat）+ 左会话列表 + 右五面板抽屉。职责四件：会话生命周期 UI、事件流→对话回合渲染、ask_user 问答卡、编排/监控。

| 层 | 位置 | 说明 |
|---|---|---|
| 入口 | `main.ts` / `App.vue` | createApp → **64 个图标白名单注册** → router + ElementPlus → mount；App 仅 config-provider + router-view |
| 布局 | `layout/index.vue` | Header（Logo/主题/设置）+ 侧栏（首页链接 + WorkDirTree + 3 组菜单 + 工作流 Beta + 人格状态）+ main router-view（fade 过渡） |
| 路由 | `router/index.ts` | 12 子路由 + 2 旧重定向；**无守卫、无 404 兜底** |
| API | `api/`（12 模块） | 全部经 `client.ts::fetchJson` 单点收口 |
| 状态复用 | `composables/`（10 个） | useSessionStream/usePanelRefresh/useTreeLayout 等 |
| 会话视图 | `views/session/**`（~30 文件） | chat/orch/monitor 三视图 + 面板 |
| 管理页 | `views/{dashboard,history,knowledge,memory,plugins,profile,projects,settings,skills,soul,workflow}` | knowledge/workflow 为纯占位 |
| 通用 | `components/`（6） | WorkDirTree/Picker/Drawer、MarkdownRenderer、AppCard、FeaturePlaceholder |
| 工具 | `utils/` | markdown 渲染管线、status、date、dir、notifications |

## 13.2 路由与布局

| path | 组件 | 语义 |
|---|---|---|
| `/` → `/dashboard` | Layout | 首页入口 |
| `/dashboard` | dashboard/index.vue | 建会话 + 会话列表 + 统计/趋势 |
| `/session` | session/index.vue | 三视图同页（`?view&id&work_dir&agent`） |
| `/projects` | projects | 目录卡片（**不在侧栏菜单**） |
| `/skills` `/plugins` `/knowledge` | 资源库组 | knowledge 占位 |
| `/memory` `/profile` `/soul` `/history` | 记忆组 | soul 只读 |
| `/settings` | 系统组 | |
| `/workflow` | 侧栏 Beta 卡进入 | 纯占位 |
| `/chat` → `/session`、`/project-prefs` → `/projects` | 重定向 | |

- **菜单是硬编码数组**（layout/index.vue:113-135），与路由表**手工同步**（加页面要改两处；`/session`、`/projects`、`/workflow` 刻意不入菜单）。
- 密钥状态共享：`useWorkDir`（localStorage `bma:last-workdir`）、`useTheme`（`bma:theme`，默认浅色，首屏不闪；App.vue 刻意不强制挂 dark class——历史事故：默认进暗色 + 图标状态错反）。
- 首页/目录树/会话页动线：`WorkDirTree` 点目录 → `setWorkDir` + `/session?work_dir=`（**双通道**）；`watch(route.fullPath)` 每次路由变化重拉会话列表。

## 13.3 会话页数据流

**状态桶**（session/index.vue:58-92）：`activeSession`、`events`（SSE 事件缓冲，唯一权威）、`agents`、metrics/tokenMetrics/mailbox/health/board/sessionLogs、`clarifyPending`/`clarifyAck`/`clarifyDrafts`、`liveStreaming`/`liveThinking`、`replyStash`。

**openSession(id)**：close stream + 停面板定时器 + `panel.invalidate()` → 清事件/澄清/live → `getSession` 全量替换 → `startStream`（SSE + 3s 面板轮询）→ refreshPanels。

**refreshPanels**：`const ep = panel.cycle()` 共享 epoch → `Promise.allSettled([agents, board, metrics, mailbox, health])` → 拉 logs + tokenMetrics。**关键：`run` 必须共享同一 cycle epoch**——各自 ++epoch 只有最后一个能存活（面板永远空白）。

**提交流程 handleSubmit 三分支**：待澄清（批量态拒绝输入框、走问答卡；带图警告）→ 已有会话（running=邮箱即时注入；非 running 重载 openSession）→ 无会话（createSession + 路由跳转）。

**唤醒补刷**：`visibilitychange→visible` 与 `online` → reconcileAfterWake（refreshPanels + loadSessions）——后台 3s 定时器被浏览器节流、SSE 快照只补事件流。

## 13.4 事件分类三档（turns.ts，核心）

`classifyEvent(ev)`（turns.ts:134-166）**按固定顺序**判定（顺序本身是逻辑）：

| 序 | 判定 | 类别 |
|---|---|---|
| 1 | `type==='user_message'`，或 `clarify && agent==='User'`（答复回显） | user_message |
| 2 | `system` + 「会话启动/继续会话」前缀 | system_start/system_resume |
| 3 | `isCompletion`（新合同 `agent_done && MetaAgent`；旧合同 system 终态文案清单；旧旧合同 system+MetaAgent 前缀） | completion |
| 4 | `clarify_detail` → 5. `clarify` | 澄清 |
| 6 | `sub_agent_dispatch` | 子 Agent 派发 |
| 7 | `assistant_text` | 中间正文 |
| 8-9 | `isToolCallEvent` / `isToolExecEvent` | 工具 |
| 10 | `tool_result`（废弃） | legacy |
| 11 | `isErrorEvent`（**含 success===false**） | error |
| 12 | `think`/`intend` | 推理 |
| 13 | `{llm,llm_result,llm_response,message,notify}` | agent_message（发言） |
| 14 | DEBUG_KINDS 或 `type==='progress'` | debug |
| 15 | **未知 kind / 系统提示** | agent_message（正文，宁多显示不藏） |

- 三档：**推理**（think/intend → 思考链盒）；**发言**（assistant_text + SPEECH_KINDS + 全部未知 kind → 正文）；**调试**（prompt/token_usage/graph_step/wait/sub_agent_done/memory_recall/interrupt/enqueue… + type==progress → 折叠 +N）。
- 判别统一 `kind || type`（prompt/system/progress 只有 type）。
- 次序陷阱：`sub_agent_dispatch`/`assistant_text` 必须在 think 兜底前；`clarify_detail` 在通用 clarify 前；工具类判定先于 error（否则 `tool_exec success=false` 被当 error 而永久 pending）。

**回合分组 `groupEventsToTurns`**：`user_message` 开新回合（running 回合被接替时 `finalText = priorReplies[timestamp]` 收编 live 文本）；completion 用 `finalizeTurn`（**必须原地改**，返回扩散副本会丢完成态→永久转圈）；tool_exec 反向找同名 pending 组配对（组 id = call timestamp）；collectArtifacts 显式 detail_json.artifacts 优先 + inferArtifactsFromOutput 去重。

**终态兜底 `settleTurnsBySessionStatus`**：running/awaiting_clarify 原样；awaiting_child 把 running 回合改 awaiting_child；其他终态把活跃回合收成 completed（error 会话收成 cancelled）。**会话终态时不允许存在 running 回合**。

**live 行协同**：liveStreaming 渲染 markdown + `▍`；liveThinking 只显示尾部 300 字符；`tool_call`/`user_message`/`assistant_text` 到达、onDone、快照回填时清 live；**ask_user/审批 hook 需要正文快照的窗口刻意不清**（后端把快照挂到提问事件 report_text）。

## 13.5 SSE 订阅机制

`useSessionStream.startStream(id, handlers)` → `streamSession`（api/session.ts:281-373）：

- `EventSource('/api/sessions/{id}/stream')`；`isSnapshot`（鸭子类型 `'id' && 'goal' && 'events'` 三键）分流全量快照 vs 增量事件。
- `done` 帧 → `es.close()` + `onDone(status)`，**不进 onEvent**；`live` 帧 → `onLive`，**不进 onEvent**。
- `onerror` **主动 close 掐掉 EventSource 自带重连**，改自己的指数退避（1s→2s→…封顶 30s，无次数上限）；页面不可见时挂起（注册一次性 visibilitychange）；`online` 事件立即重连。
- **重连补播策略：不需要补播端点**——重连后服务端先推全量快照，断档由快照补齐。
- `onDone` 处理（index.vue:354-376）：清 live、停面板、refreshPanels + loadSessions、**对账重取 getSession 原子替换 events**（补 SSE 切片竞态丢的尾部事件）、浏览器通知。

## 13.6 澄清提交韧性

`clarifySubmit.ts` 状态机：

- `NetworkError`（timeout/offline）：退避重试 `[2s,3s,5s,8s,10s×5]`，上限 150s（覆盖本机约 60s 重启窗口）；**每次重试前查会话状态**（`alreadyApplied`：不在 awaiting_clarify 即视为已提交），避免二次答复；`aborted()` 由组件卸载置位。
- `APIError`（业务拒绝）：仅 alreadyApplied 时按已提交处理，否则原样抛（400/409 给 UI）。
- `/clarify` 超时 30s；失败时问答卡内常驻原因 + **草稿保留**。
- 单题：`other` 选项引导自由文本；多选 join(',')。批量：单选即落草稿并跳下一未答；`submitBatch` 要求全答。

## 13.7 编排视图与监控视图

**编排（无独立视图，折叠进 chat + 看板）**：

- 选中态唯一存储 = URL `?agent=<inst_id>`（ChatHeader 链条 chip / OrchMiniCanvas 点击）；`isMetaSelected` 传 null；悬挂清理 watcher 随 agents 刷新清理。
- **单 Agent 对话面板**（orch/AgentChatPanel.vue）：`getAgentMessages({limit:100})` + **3s 增量轮询**（afterSeq=最大 seq，merge 按 seq 去重）；上翻 beforeSeq 前插（**必须拦 seq<=0**——后端把 ≤0 当取尾部）；`agentTurns.ts` 把 AgentMessageItem 合成 Turn（user 开回合、reasoning→think 事件、tool_calls 配对、`[mailbox from X]` 解析发送者）；发送框状态机 `sendState`（meta 禁/running+child_wait=waiting/running/done/paused 禁/idle 可发）；`canSend`=waiting|running|done|idle；queued=true 提示"已排队"否则"已请求复活重跑"；中断/暂停仅 domain+running。
- **树布局**：`useTreeLayout` tidy-tree 两遍法（后序算单位宽、前序分配区间中点）；**idle 热驻节点不入树**；孤儿/自指升为根（"树里少节点只应归因于 idle"是刻意不变量）；`name.localeCompare` 稳定排序；`isChildWaiting` 兼容两种命名（`child_wait` 特判中文"等下级返回"，ago 缺失也出字）。

**监控（?view=monitor）三 Tab**：

1. 执行日志：数据=events；Agent/Kind/搜索过滤（`matchAgentEvent`：detail_json.agent_id 精确优先，否则归一化名字前缀）；**进度条仍认旧完成合同**（`system + 会话完成`前缀）——已漂移。
2. 日志分析：sessionLogs（3s 拉）+ 客户端 matchAgentLog（服务端是精确等值匹配，选中子 Agent 时**不下发** agent 参数）。
3. 效率审计：`getSessionEfficiency` + 支路成本表；选中 Agent 下钻逐轮回放；运行中 domain 可"暂停"；**不参与 3s 面板周期**（仅 id 变化/手动刷新）。

## 13.8 右栏面板

容器（index.vue:727-772）：收起 48px 图标条 ↔ 展开单面板（每 Tab 独立宽度 760/640/760/560/600）。

| Tab | 数据源 | 刷新 |
|---|---|---|
| board 看板 | board(3s) + agents(3s) + events(live) | 响应式；含 OrchMiniCanvas |
| tools 工具 | events | 事件流配对 |
| files 文件 | listFiles + getFileContent | watch sessionId 一次性，**不轮询** |
| memory 记忆 | listEvolutionLog + listLearnedSkills | watch sessionId 一次性 |
| metrics 指标 | metrics/mailbox/health(3s) + tokenMetrics + efficiency | 混合 |

- `useTaskBoard`：board.tasks 优先，否则由非 meta Agent 合成（`toTaskStatus`：paused/cancelled/idle 一律 blocked；delivered-unverified 保留）。
- `useGoalTimeline`：从事件提 user/dispatch/done/failed 里程碑（上限 50 截最近）；空则 board.tasks 兜底（**保证面板永不空白**）。

## 13.9 API client 层

`client.ts`（全站唯一网络出口）：

- 错误分类：`APIError{status,statusText}`（服务端明确拒绝，message = 响应体文本 detail || "413 Conflict"）；`NetworkError{reason:'timeout'|'offline'}`（请求没到/响应没回来）。读 `res.text()` 作 detail 是"三态发送框只显示 409 Conflict"的修复。
- `fetchJson`：AbortController 超时（默认 10s）；`...options` 在 headers 之后展开（传 headers 会整体替换 Content-Type）；**200 也 res.json()**（空 body 会抛 SyntaxError——所有端点必须回合法 JSON）。
- 超时定制：`/models/switch` 70s（含 60s 探测）、`/fs/pick-dir` 5min（等人操作）、`/clarify` 30s。

**无鉴权头**（不注入 Authorization、不拼 `?token=`）→ 开启 auth_enabled 后 Web 整体 401 不可用；当前只适用本机 127.0.0.1 + 关闭鉴权。

主要端点（相对 /api）：sessions CRUD/delete 批量(≤200)/board/agents/tree/agents/:aid/{cancel,pause,events,messages,message}/worktrees{/diff,/action}/message/stop/cancel/interrupt/enqueue/clarify(+batch)/trust-mode/gear/thinking/workdir/topic/stream(SSE)/logs/watchdog/mailbox/metrics/token-metrics/efficiency；models{/switch}；plugins{/enable,/disable,/reload}；skills/learned{/name,/enable,/disable}、evolution/log；profile、project/preferences、project/tester-config；fs/browse、fs/pick-dir；files、files/content。

## 13.10 composables 逐个

| composable | 要点 |
|---|---|
| `useSessionStream` | isSnapshot 判定 + 托管 SSE 生命周期 |
| `usePanelRefresh` | epoch 竞态防护：`cycle()` 发批 epoch；`run(fn, epoch?)` 过期丢弃 + 静默吞错；`invalidate()` 作废在飞 |
| `useSessionList` | 极简 SessionSummary 列表（不传 limit → 200 截断） |
| `useSessionStatus` | 状态点/文案（**与 utils/sessionStatus.ts 双份实现**，改状态要改两处） |
| `useTaskBoard` | 看板任务合成 + 进度 |
| `useGoalTimeline` | 事件→里程碑 + 兜底 |
| `useTreeLayout` | tidy-tree 布局 + isChildWaiting |
| `useTheme` | localStorage bma:theme |
| `useWorkDir` | 模块级单例 + bma:last-workdir |
| `useModelSelection` | 模块级单例（catalog/selectedRole='meta'/thinking）；ensureLoaded 并发去重；doSwitch/doAdd **不 catch**（错误交调用方） |

## 13.11 通用组件与管理页

- **WorkDirTree**（侧栏核心）：按 work_dir 分组（customDirs 补空组 + customDirs/collapsedDirs 在 localStorage）、SESSIONS_PER_DIR=5 + 显示更多、⋯ 菜单（新建/查看全部/偏好/移除）、**显式 limit 1000**（不传会被 200 截断）。
- **WorkDirPicker**：**只在"选定"时 emit 一次**（此前逐字 emit 会写半截路径）；原生目录选择优先（501/504/409 退网页版：面包屑 Windows 盘符兼容/手输/最近/子目录单击选中双击进入）。
- **WorkDirDrawer**：与 /projects 右栏**同源**（改一处要考虑另一处）；关闭时 emit `update:dir null`（否则同一目录再点打不开）。
- **MarkdownRenderer**：`marked.parse`（gfm + breaks 单换行即 `<br>`）→ `DOMPurify.sanitize`（标签白名单、禁 data: URI、剥原始 HTML）；自定义 link 渲染器（非 https 归一 # + target blank）；code 渲染器输出 DeepSeek 风格头栏（复制/下载按钮靠事件委托）。
- **管理页**：dashboard（Promise.all 三源 + 失败进 mock 模式琥珀告警；客户端分页 PAGE_SIZE=10；本页全选；自绘 SVG 折线**未归一化**）；history（`?work_dir=` 预过滤、`openSession` 默认落 monitor；**不传 limit**；**<768px 卡片化**——md 断点下 el-table 换卡片列表（`md:hidden`），桌面表格不动，仅此一处移动适配）；projects（左分组 + 右偏好/测试助手）；plugins（就地替换 + 失败回滚；transportBadge **硬编码 id 判定**）；memory（项目归属靠 source_session→work_dir 反查）；profile（dirty 才可保存；自动行带时间戳、人工行程序永不改写）；settings（外观/服务信息/连接健康/能力自检/桌面通知权限/资源目录）；skills（三 Tab：经验技能 frontmatter 剥离 + 内置 + 进化日志）；soul（只读）；knowledge/workflow（占位）。

## 13.12 隐含约定与坑

1. **分类顺序即协议**：新增发言类 kind 必须插在 think 兜底之前；未知 kind 默认发言档。
2. **一律 `kind || type` 判别**。
3. **isErrorEvent 含 success===false** → 工具判定必须先于 error。
4. **finalizeTurn 必须原地改**（扩散副本丢完成态）。
5. **终态文案清单硬编码**（turns.ts TERMINAL_SYSTEM_MESSAGES）：后端新增终态文案必须同步补，否则回合永久转圈；settleTurnsBySessionStatus 用会话状态兜底。
6. **replyStash 键必须与分组查找键同源**（user_message 的 timestamp）。
7. **live 清理双保险**：三类事件 + onDone + 快照回填；assistant_text 不清会与正文块同屏重复。
8. **批量澄清草稿上浮到 index 层以 questionId 为键**（awaiting_clarify 帧 500ms 重推会重建对象，键在对象内会丢草稿）。
9. **澄清双通道互斥**（批量态输入框被拒；awaiting_clarify 时输入框内容被当答复）。
10. **提交后不重载会话**（重载清 events 导致滚动跳变；靠 SSE 接管）。
11. **panel.run 共享同一 epoch**（否则面板永空白）；`run` 静默吞错（无法区分没数据/失败）。
12. **getSessionLogs 键名双兼容**（裸数组/信封、snake/Pascal）；后端补 json tag 后仍可用。
13. **服务端日志过滤是精确等值** → 选中子 Agent 必须客户端过滤。
14. **`before_seq<=0` 后端当取尾部** → 上翻必须自己拦。
15. **`has_more` 是前端推断值**（取满即视为可能还有；afterSeq 模式恒 false）。
16. **SSE 重连自理**：done 后不置 closed 靠 `online && es` 巧合拦住重连（改 onVisible 守卫会引出"完成后反复重连"）。
17. **ExecutionLog 进度条仍认旧完成合同**（口径已漂移）。
18. **SkillSet.vue 是死代码**；`filterTurnForConcise`/`isLLMEvent`/`esc`/`sameDir`/`fmtTime` 无引用；`fmtDateTime` 与 `fmtDate` 实现逐字相同；`healthPollInterval` 死配置。
19. **FilePreview 的"VS Code 打开/下载"是空按钮**。
20. **artifact 路径全部工作区相对**（workspaceUrl 逐段 encode 保斜杠；HTML 相对引用可解析）；inferArtifactsFromOutput 只认 `.bma/` 下五个产物目录。
21. **SubAgentList 旧事件回退匹配要求唯一**（并列宁可不显示状态）。
22. **ChatHeader 销毁倒计时本地 1s timer**；destroy_at 非空时三个控制按钮全隐藏。
23. **ChatInput 档位/思考 localStorage 只在未绑定会话时读写**；绑定后即时 POST。
24. **吸底阈值 80px**；onMounted 双帧滚底防异步撑高。
25. **ThinkChain 的 expanded 必须整体替换 Set**（ref 不追踪 Set 内部变更）。
26. **状态字典需与后端全量对齐**（SubAgentList 9 态 / OrchMiniCanvas 色点）；新增状态不补映射会显示英文原值。
27. **图标白名单制**：`MoreFilled` 不在 main.ts 注册列表 → WorkDirTree ⋯ 触发器视觉上隐形（可点但看不见）。
28. **`fetchJson` 200 也 json()**；传 headers 会替换 Content-Type。
29. **UI 无鉴权注入**：auth_enabled 后整体不可用。
30. **notifications.ts 的 tag 是固定常量**（`bma-session-done`）→ 多会话完成互相顶掉；触发四重闸门（Notification 存在 ∧ granted ∧ document.hidden ∧ 终态）；`cancelled` 不在 SessionStatus 类型里但通知层认它。
31. **监控页过滤行布局约定**：会话页监控面板实测只有 600-800px 宽，而 Element Plus 的 `.el-input/.el-select` `width:100%` 会顶掉 Tailwind `w-*`（flex 基准=整行宽，挤压时把下拉压成光杆箭头、徽标盖住标签）。约定：过滤行控件一律「定宽包装 div + `!w-full`」，分组 `shrink-0`，弹性交给搜索框（`flex-1 min-w-[160px] max-w-[256px] ml-auto`），整行 `flex-wrap` 兜底；锁定徽标 `max-w-[220px] truncate` + `title`。
32. **首页会话列表口径**：`GET /api/sessions` 返回轻量 `sessionSummary`（去 events/messages）；`?limit=` 缺省 200 上限 1000，首页显式 1000；**上限内计数真实、超过 1000 页脚提示"仅显示最近 1000 条"**——历史事故：不传 limit 被 200 截断导致"删除没生效"错觉。
