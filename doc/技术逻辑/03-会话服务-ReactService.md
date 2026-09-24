# 03 会话服务：ReactService

> 只从代码还原。核心文件：`backend/internal/agent/service_react.go`（4613 行）、`session_react.go`（1317 行），及周边 `query_react.go` / `session_shared.go` / `messages_store.go` / `message_log.go` / `notifier.go` / `metrics.go` / `prompt_enhance.go` / `video.go` / `imageutil.go` / `verify_evidence.go` / `evolver.go` / `gear_signals.go` / `worktree.go` / `models.go` / `errors.go`。

## 目录

- [3.1 职责](#31-职责)
- [3.2 核心数据结构](#32-核心数据结构)
- [3.3 公开方法面](#33-公开方法面)
- [3.4 会话状态机](#34-会话状态机)
- [3.5 主流程逐点（创建/发消息/运行/恢复）](#35-主流程逐点创建发消息运行恢复)
- [3.6 事件管线](#36-事件管线)
- [3.7 人在回路三通道（澄清/审批/升档）](#37-人在回路三通道澄清审批升档)
- [3.8 停止/中断/取消/删除](#38-停止中断取消删除)
- [3.9 档位与思考强度](#39-档位与思考强度)
- [3.10 周边文件](#310-周边文件)
- [3.11 隐含约定与坑](#311-隐含约定与坑)

---

## 3.1 职责

ReactService 是**会话门面（facade）**：HTTP/TUI 只依赖 `agent.Agent` 接口，实现落在这里。它不实现 ReAct 循环（在 react_agent.go），只做：

- 内存会话生命周期（建/查/续/删/恢复/淘汰）；
- 把执行委托给 ReActAgent（按档位选顶层角色、装配依赖、注入上下文）；
- 把 LiveEvent/工具进度汇总成事件流并双写持久化（PG session_events/history/messages + Redis 热层）；
- 作为 HTTP/SSE/Query 端点的唯一数据源；
- 人在回路通道（澄清/审批/升档）与软停/硬取消状态机。

`ReactService` 本身不 import `subagent` 包——与 Dispatcher 全部靠接口反转（react_types.go 的 Checker 接口族，bootstrap 装配）。

## 3.2 核心数据结构

### ReactService（service_react.go:47-200）

- 基础设施：`store *reactSessionStore`、`roleRegistry`、`modelFactory`、`toolRegistry`、`mailbox`、`memory`、`runtimeCfg`。
- 注入回调族（全部 nil=零行为）：`notifier`、`pendingChecker`/`pausedChecker`/`resumeDispatcher`（父终结保护与恢复）、热驻四件套（`idleRosterProvider`/`activityEvidenceProvider`/`idleTTLArmer`/`sessionAgentWaker`/`hotResident`）、`trees sync.Map`（每会话权威树 lazy init + treeStore 恢复）、`boardFn`/`boardRemoveFn`/`ledgerFn`、`sharedMemoryStore`、`persona`、输入补全五件套（`promptEnhance`/`promptEnhanceLLM`/`promptEnhanceArbiter`/timeout/maxRunes）、`stopMarker`/`stopCountdown`、记忆/进化族（`userProfile`/`profileExtractor`/`projectPrefs`/`prefMerger`/`evolver`/`skillSink`/`evolutionLog`/`skillRecall`）、`pluginVisibility`、`activityPinger`、`skillPool`、`agentMsgCache`/`msgLogger`、`messenger`（用户直连通道，nil 时 MessageAgent 恒拒）、`acceptance`（验收闭环）、`createMu`（建会话临界区）。

### reactInternalSession（session_react.go:35-146）

ID/Goal/Status/Result/StartedAt/EndedAt/Events[]/Messages[]；`firstTurnImages`（一次性内存透传）；`workDir atomic.Value`（跨 goroutine）；`History []ReactMessage`；`StreamingText`/`ThinkingText`（瞬时）；去重锚 `lastThinkEventText`/`lastInterimText`；`ctx/cancelFn`（run 上下文）与 `stopCtx/stopCancel`（独立中断传播基底）；`runStartedAt`；`trustMode/gear/thinking atomic.Value`；`activeTopicID/recalledTopicID`；**三互斥澄清通道** `approval chan bool` / `askUser chan string` / `askUserBatch chan []string` + `pendingClarify`；`wakeInput`；`stopTimer/destroyAt`；`projectDocReady`。

常量：`maxReactInMemorySessions = 20`（:150）、会话列表默认 200（service_react.go:1121）。

## 3.3 公开方法面

继承自 `agent.Agent` 接口（agent.go）+ ModelManager（models.go:15-40）。

### 构造与注入（全部 Set*）

`NewReactService(roleRegistry, modelFactory, toolRegistry, mailbox, memory, pgStore)`（:967，内挂 `toolRegistry.SetProgressCallback(handleToolEvent)` :990）；Set 族 30+ 个：`SetRuntimeConfig/SetSkillCatalog/SetAgentMsgCache/SetActivityPinger/SetUserProfileStore/SetProfileExtractor/SetPrefMerger/SetEvolver/SetSkillSink/SetEvolutionLogger/SetSkillRecall/SetProjectPreferencesStore/SetPluginVisibility/SetBoard/SetTaskLedgerProvider/SetBoardRemover/SetPromptEnhance(+LLM)/SetTreeStore/SetSharedMemoryStore/SetDomainClassifier/SetPersonaInjector/SetPendingChildrenChecker/SetPausedChildChecker/SetPausedDomainResumer/SetIdleRosterProvider/SetIdleTTLArmer/SetActivityEvidenceProvider/SetAgentMessenger/SetAcceptanceRunner/SetNotifier/SetSessionAgentWaker/SetModelProvider/SetWorkDir/SetLogger/SetSoftStopMarker/SetStopCountdown/SetHotResident`。

### 会话与执行

| 方法 | 位置 | 语义 |
|---|---|---|
| `CreateSession(ctx, req)` | :996 | createMu 幂等闸（同 goal running 复用）+ 视频抽帧入 goal + 建内存会话 + gear/thinking 覆盖 + 首轮图片 + 墙钟 + `go runSession` |
| `Get(ctx, id)` | :1105 | 内存快照优先，未命中 `restoreOneSession` 懒恢复；失败 ErrSessionNotFound |
| `List(ctx, filter)` | :1175 | 内存过滤 + PG RecentSessionHistories 补充（3s、失败降级纯内存），合并去重排序截断 |
| `Send(ctx, id, msg)` | :1220 | → sendMessageFull |
| `ResumeSession(ctx, id, req)` | :1226 | 有 CarryOver/UserInput 走 sendMessage 后取快照 |
| `Stream(ctx, id)` | :1247 | 50ms ticker 轮询快照推新事件 + live_update 轻量帧；非 running 且非 awaiting_clarify 200ms 后关流 |
| `Query(ctx, id, q)` | :1320 | kind 全表（见下） |
| `Control(ctx, id, cmd)` | :1400 | op 全表（见下） |
| `CancelAgent(ctx, sid, instID)` | :2452 | 树 Cancel + ClearAgentModel；不存在 ErrAgentNotFound |
| `PauseAgent(ctx, sid, instID)` | :2472 | 仅 domain+Running：MarkPauseNode → StopRunning（失败回滚） |
| `MessageAgent(ctx, sid, instID, content)` | :735 | 用户直连子 Agent（状态机见 3.7） |
| `Shutdown(ctx)` | :2513 | DestroyAllIdle + worktree 清理 + store.shutdown |
| `DeleteSession(ctx, id)` | :4265 | 硬删（见 3.8） |
| `Stop(ctx, id)` | :4403 | 软停（见 3.8） |
| `SummarizeTaskTitle` / `ClearSessionChat` / `SessionCount` / `LLMStats` / `LaunchSession` / `RestoreSessions` | :2529/2569/2574/2579/2555/2564 | 杂项 |
| `SwitchTopic(ctx, sid, name, goal)` | :2595 | 旧树 EndCurrentTopic + 旧话题摘要写 KV `topic:{sid}:{topicID}:summary` + 新 topicID 递增 + 事件 |
| `ForwardLiveEvent(sid, ev)` | :820 | dispatcher 侧子 Agent 事件转投会话流（加【展示名】前缀） |
| `WakeOnChildDone(parentID)` | :3690 | 智能唤醒（2026-09-17）：pending>0 且邮箱无未读时**不**翻态（省"收到，继续等"空转轮）；末次完成或邮箱有未读必醒；pendingChecker/mailbox 未注入时保守恒唤醒 |
| `WakeSuspended(parentID, wakeInput)` | :3746 | 参数化唤醒（2026-09-17）：仅 awaiting_child 翻转 + wakeInput + persist + `go resumeSession`；`WakeOnChildDone` 与邮箱 MsgRequest 到达路径（submit_plan 审批 / send_message request·escalate，经 dispatcher `WithSessionWake` 接线）共用 |
| `NotifyUserSystemMessage` | :700 | 落 System 事件 |
| `TreeFor/Tree/ListAgents` | :2422/2441/2362 | 权威树（lazy init + LoadFromStore 3s）；ListAgents 中 parent==sessionID 归一为 "meta" |
| 配置读写 | :1567-1762 | SetDefault/SessionTrustMode、Set/Get Gear、Thinking、WorkDir |
| 画像/偏好/看板 | :342/350/320/328/498 | Profile/SaveProfile/ProjectPreferences/SaveProjectPreferences/Board |
| Hook | :1499/1916/2255/2022 | ApprovalHook/AskUserHook/AskUserBatchHook/EscalateGearHook（3.7） |

### Query Kind 全表（:1320-1393）

`session-count` / `llm-stats` / `logs` / `board` / `metrics` / `mailbox` / `mailbox-trace`（2026-09-17，会话级邮件留痕：全部 Agent 往来一行一封、收发双方重复行 store 层 DISTINCT，按时间正序；与 `mailbox` 的"未取走队列"语义不同——那是待办、这是历史）/ `watchdog`（空列表兼容）/ `token-metrics` / `efficiency` / `agent-events` / `agent-messages`（响应带 `run_id` 与 `hot_max_seq`，2026-09-17——复活重跑热层清空重编号后，前端增量游标以 run_id 单调性为权威判据、hot_max_seq 兜底）/ `worktrees` / `worktree-diff`；default 空结果。

### Control Op 全表（:1400-1470）

`message` / `clarify`（answers 兼容 []string 与 []any）/ `interrupt` / `enqueue` / `cancel` / `stop` / `topic` / `trust-mode` / `gear` / `thinking` / `work-dir`（fail-closed 缺参拒绝）/ `worktree`（merge/reject）；default 报 unknown op。

## 3.4 会话状态机

枚举（pkg/enums）：`running` / `completed` / `error` / `awaiting_clarify` / `paused_on_child` / `awaiting_child`。

迁移表：

| 迁移 | 触发点 |
|---|---|
| → running | 新建（session_react.go:339）；hook 收答复（:1553 等）；非 running 会话收消息 + `go resumeSession` |
| running → awaiting_clarify | 三 hook 占槽（:1528/:1975/:2315）；pauseSession 的 PauseUserStop/PauseIterationLimit 分支 |
| running → paused_on_child | result.PausedOnChild（:3149/:3338）或软停时树有 Running/Paused domain（:3621）；恢复走 resumePausedDomain 或 waker |
| running → awaiting_child | suspendOnChildWait → pauseSession(PauseChildWait)；恢复走 WakeOnChildDone（智能唤醒）/ WakeSuspended（邮箱 MsgRequest 到达）/ 用户消息 |
| running → completed | runSession:3174 / resumeSession:3364；**先落 agent_done 事件再翻状态**（防 SSE done 帧抢跑） |
| 任意 → error | setSessionError（首次错误胜出守卫 :3720）；cancel（"cancelled by user"）；墙钟；优雅停机 |

- 重启恢复映射 `restoredSessionStatus`（session_react.go:162）：库中 running → error + 中断提示；旧空值 → completed；其余原样。
- **重启讣告**（interrupted 分支）：restore 时 best-effort 查该会话最近 agent_events / 最后派发，拼"上次任务断在 X"System 事件（nil-DB 静默跳过）——只做讣告不做续跑，让用户知道断点在哪。
- **暂停态不动 EndedAt**（:3641），因此 `evictCompletedSessions`（只淘汰非 running 且 EndedAt!=nil，session_react.go:746）不会淘汰暂停会话。

## 3.5 主流程逐点（创建/发消息/运行/恢复）

### CreateSession（:996）

createMu 锁 → 同 goal running 查重（findRunningDuplicateSession:299）→ 视频抽帧（锁外，`context.WithoutCancel`，:1010）→ `createSession`（sessionID 格式 `session-<bootEpoch>-<bootRand>-<seq>`，session_react.go:326——解决旧 "session-N" 重启回 1 的块记忆污染）→ Messages=[system "Goal: "+goal, user goal] → runCtx/stopCtx 双上下文 → 信任模式/档位默认值原子存 → 入 map → `sharedMemoryReset` → `EnsureProjectDoc`（有 classifier 才异步）→ 返回。

### sendMessageFull（:3775）

空内容拒 → 视频抽帧 → 锁内查会话（未命中懒恢复）→ 四路分流：

1. `askUser != nil` → 选项解析 recordClarifyAnswer → 写通道 + Messages 追加 `[澄清答复]`；
2. `askUserBatch != nil` → 拒（引导面板，ErrInvalidSessionState）；
3. `approval != nil` → resolveApproval 写通道；
4. 常规 → **决策层①任务级意图分诊**（decideTaskTriage，2026-09-23 TODO #23 切入点1：Choice 任务性质 quick/single/multi + Choice 工具面 none/read/write/exec + Noul 需澄清?；低置信→建议先 ask_user 澄清而非硬猜；只做建议与澄清触发——建议前缀并入 content，**不自动改档**；影子期 GoObserve 异步零延迟税）→ `enhanceUserInput`（输入补全）→ Messages 追加 → ResetReadHistory + ResetDispatchCounts + ArmIdleTTLs → 非 running 置 running + restartSessionContext → 信号①计算 + **决策层⑥档位建议只读影子**（gearHintShadow，与 T18 信号并行落对拍行，actual=当前档位；**永不做自动选档**）→ 锁外落 Prompt/信号事件 → `cancelSoftStopState`（软停窗口内任意消息=续跑意图）→ UserMessage 事件 → **wasRunning 则邮箱注入**（主循环不读 Messages，唯一触达是邮箱）→ 非 running 则 persist + persistHistory → ResumeSessionAgents → PausedOnChild 且 waker 未接线时 resumePausedDomain → `go resumeSession`。

### runSession（:3009）

persist running → `resolveGearMetaRole` → `mountTopLevelEssentials`（插件 `top_level: true` 的顶层必备工具预挂到会话顶层 scope，**三档全挂**，2026-09-18 起——原排除 cluster 的前提"T13 收窄后 meta 仍可用 tool_catalog+tool_mount"已失效；幂等，子 Agent scope 不受影响）→ fast 档不等 PROJECT.md（其他等 ≤2s）→ provider（`GetBladesProviderWithThinking(gearRoleID, thinking, fallbackObserver)`）→ 记忆包装（meta 档：看板/roster/台账 + metaSkillBlock；daily 档：roleSkillBlock）→ `NewReActAgent` 链式注入（LiveEvents、WithSuspendOnChildWait(true)、WithProviderFunc；**非 fast 才注 pending/paused checker**）→ runCtx 注入 sessionID/workDir/stopCtx/trustMode 读取器 + 首轮图片 → goal 拼话题召回 + 技能预筛 → `agent.Run`：软停错误 → pauseSession；其他错误 → setSessionError；成功：SuspendOnChildWait → suspendOnChildWait；LimitReached → pauseSession；否则验收 RunWrap → agent_done 事件 → completed → evolveSession("success") → persistHistory/Events。

### resumeSession（:3193）

取输入（wakeInput 优先，否则倒序最后一条 user，带 Images）→ 拷 History → 同 runSession 构造（persona 用 metaPersonaLite）→ `RunWithHistory` → 同样四路分流。

### finalizeSession（defer，:3382）

清 Streaming/Thinking → notifier.NotifySessionFinal → 暂停三态保留 TempDir，其余清理 → evictCompletedSessions。

## 3.6 事件管线

路径：ReActAgent `WithLiveEvents` / 工具注册表 `SetProgressCallback(handleToolEvent)` / dispatcher `ForwardLiveEvent` → `st.addEvent/addEventDetail/addEventDebug`（session_react.go:642/649/671，锁内 append，>500 条裁到 200）→ 落库 `persistEvents`（:935）→ PG `SaveSessionEvents`；SSE 侧由 server 轮询 `Stream()` 推帧。

`handleLiveEvent` 映射（:3491-3551）：

| LiveEvent | 事件 |
|---|---|
| `llm_delta` | finalizeThinking + setStreamingText（**不落事件**）；**集群档顶层 Meta 例外**：只写轮缓冲 `pendingTopText` 不推 StreamingText（见下"集群档顶层治理"） |
| `think_delta` | setThinkingText；**集群档顶层 Meta 直接丢弃**（不进 ThinkingText、不落 think 事件，2026-09-19 定案） |
| `tool_call` | finalizeThinking + persistInterimText（口播正文落 `assistant_text`）；**集群档顶层 Meta 例外**：中间轮口播随缓冲一起丢弃（不落 assistant_text），ask_user 边界把缓冲灌回 StreamingText（提问正文快照依赖它）；`call_sub_agent(s)` 逐项落 `kind=sub_agent_dispatch`（中文领域名进 detail_json） |
| `tool_exec` | 仅补 call_sub_agent 的 tool_exec 事件（其他工具由 handleToolEvent 记） |
| `sub_agent_done` | Message `kind=sub_agent_done` + 摘要 `llm_result` |
| `peer_ask` | Message `kind=peer_ask` + 正文 `llm_result` |
| `notify` | System 事件 |
| `token_usage` | debug 事件，msg `in=… out=… cache_hit=… cache_miss=…` |

**集群档顶层治理（2026-09-18/19）**：判定 `isClusterTopEvent` = `ev.AgentID == session.ID`（顶层实例 ID 即会话 ID；初版误用 `ev.Agent==""`，实测顶层事件带展示名 "MetaAgent" 永不命中，已修正）。该档用户只看最终交付：中间轮的编排口播/编排推理绝不进用户流——LLM delta 只进轮缓冲 `pendingTopText`，ToolCall 边界丢弃缓冲且不调 persistInterimText；run 完成收尾时 `flushPendingTopText`（:3532）把终答灌进 StreamingText（与 agent_done 同 tick 快照）。日常/快速档与子 Agent（含集群档 domain/叶子）两路径零改动。

`handleToolEvent`（:2734）：从 ctx 取 AgentID/展示名 → 仅 running 会话记录 → 跳过 call_sub_agent → tool_call/tool_exec 事件（artifacts merge 进 detail_json）；失败拼 `path= err=`。

**去重/裁剪约定**：finalizeThinking 用 `lastThinkEventText` 去重；persistInterimText 用 `lastInterimText` 去重且跳过 ask_user；`trimDebugEvents` 只裁 think/prompt/token_usage/graph_step（不裁 assistant_text）；`sanitizeUTF8` 剥 NUL 防 PG jsonb 拒收。

持久化目标：session_history（MetaMemory JSONB 含 gear+thinking）、session_events（**append-only**：按 `(session_id, seq)` 唯一键 ON CONFLICT DO NOTHING 幂等纯追加，seq 永不清零）、agent_messages（主对话 agentID==sessionID，**尾差量追加**：边界行一致则只补尾段，复活重跑前旧 run 快照经 `ArchiveMessages` 移归档 archived=true 不物理删）、session_logs。

## 3.7 人在回路三通道（澄清/审批/升档）

**三通道互斥单槽**（approval/askUser/askUserBatch 至多一个非 nil），hook 用"锁内自旋 + 2s 一拍等空位"排队。

- `ApprovalHook`（:1499）：sid 空放行；已有 pending 放行避免死锁；占 approval 槽 + `pendingClarify`（Kind=confirm，confirm/reject 选项）+ 状态 AwaitingClarify + report 快照；落 clarify 事件；startUserWaitKeepalive（等用户期间保活）；ctx.Done 清槽报错；答复清槽回 Running + 清 StreamingText。
- `AskUserHook`（:1916）：槽位排队；Options→Kind=choice/text；Detail 独立 `clarify_detail` 事件；正文快照挂提问事件。
- `AskUserBatchHook`（:2255）：三槽全空才占；逐题 options；顶层字段镜像第一题；正文快照只挂第一条。
- `EscalateGearHook`（:2022）：**顶层守卫**（`AgentIDFromContext != sid` 返回提示不占槽）；确认卡 Kind=confirm；确认后 `setGear("cluster")` + 事件 + `restartWithGearSeed`（合成种子走"终态收消息→新 run"）。

`answerClarify`（:4006）：空答复拒 → 非 awaiting_clarify 拒（PausedOnChild 附引导文案）→ 批量分支逐题校验 → askUser/approval 分支 → 兜底追加 `[澄清答复]` + 置 running + `go resumeSession`。

**用户直连子 Agent `MessageAgent`（:735）状态机**：running + child_wait → InjectUserMessage 投邮件并 poke；终态（done/failed/cancelled/delivered-unverified）→ `Tree.Reopen` + `ReviveWithMessage` 同 ID 重跑；running 其他 → 邮箱注入 + 200 `{queued}`；paused/idle/meta/未接线 → 409 ErrAgentNotDirectable。

## 3.8 停止/中断/取消/删除

- `interrupt`（:4105）：非 running 先置 running + restartSessionContext；落 interrupt 事件；**仅落事件不做注入**（运行中靠循环读邮箱？实际为落事件供 UI，运行中由 stopCtx 语义之外的路径处理）。
- `enqueue`（:4136）：挂起时路由到 answerClarify；batch 拒；running → 邮箱注入；非 running → 补 Messages + `go resumeSession`。
- `cancel`（硬取消，:4197）：running/awaiting_clarify/paused_on_child/awaiting_child 可取消（**暂停态也允许，退出死锁**）；置 error+"cancelled by user"；锁外 cancelFn + stopCancel + `cascadeCancelTree`（含 Idle 节点）+ worktree 清理。
- `Stop`（软停，:4403）：仅 running；信号②计算；`stopMarker.SetSoftStop`；树内 Running 节点 `StopRunning`（不改状态，dispatcher 收尾分流）；热驻模式下停用 session 级倒计时（idle 域自身加权 TTL 治理）；非热驻 countdown>0 起 destroyAt 定时器；锁外 cancel；落软停事件；`destroyAfterSoftStop`（:4482）幂等硬销毁；`cancelSoftStopState`（:4535）续跑清理。
- `DeleteSession`（硬删，:4265）：锁内摘 map 先行（stillLive 守卫拦截迟到 finalize 落库）→ 锁外 cancel → 树快照 + EndCurrentTopic（树条目保留防迟到 lazy 重建）→ `PurgeSession`（dispatcher 侧进程内清扫）→ mailbox 兜底 → boardRemove → worktree 清理 → TempDir 清理 → topic 摘要 KV 清扫 → `pgStore.DeleteSessionData` 单事务硬删全部从属表（**块记忆 global_knowledge 不删**）。

## 3.9 档位与思考强度

**档位**（tool/gear.go:17-38：`fast|daily|cluster`，`NormalizeGear` 旧 `auto`→daily）：

- 解析 `resolveGearMetaRole`（:2965）：fast→`doc_assistant`、daily→`domain`、cluster/未设/未知→`meta`；角色缺失回退 meta，再缺 run 落 error。
- 差异落地：fast 跳过 PROJECT.md 等待与 pending/paused checker；daily 注 roleSkillBlock；cluster 注看板/roster/台账 + metaSkillBlock；顶层三者都开 `WithSuspendOnChildWait(true)`。
- 切换 `SetSessionGear`（:1623）：atomic 即时生效下一轮（在飞子 Agent 不强杀）；事件留痕。创建初值取 req.Gear 或 config `agent.default_gear`。
- 升档 `EscalateGearHook`；恢复 `gearFromMetaMemory`（session_react.go:878，旧 auto→daily）。

**思考强度**（tool/thinking.go:11-27：`off|low|medium|high`，空=跟随角色默认）：

- 存储 `sess.thinking atomic.Value`；创建/`SetSessionThinking`（空串合法=清除覆盖，事件留痕）。
- 生效：run/resume 首解析用 `GetBladesProviderWithThinking`；**每次 LLM 调用**经 `providerForRole` 闭包实时读取（运行中切换下一次即生效）；只覆盖本会话顶层 Agent。
- 落库 MetaMemory JSONB 双键 gear+thinking；恢复 `thinkingFromMetaMemory`（:894）。

**信任模式**：`suggest|auto-edit|full-auto`，同 atomic 模式；runCtx 经 `tool.WithTrustModeFunc` 实时读。

**决策层消费面（2026-09-23 TODO #23）**：`SetDecisionLayer` 注入 `domain/decision.Layer`（nil=关闭零行为）。本服务持两个切入点——①任务级意图分诊（decideTaskTriage，sendMessageFull enhanceUserInput 同位，只做建议与澄清触发不自动改档）与⑥档位建议只读影子（gearHintShadow，与 T18 隐性信号并行；永不做自动选档，auto 退役决策不破）。影子行落 `agent_events type=decision_shadow`，晋级按点切 enforce（config agent.decision_points）。

## 3.10 周边文件

| 文件 | 要点 |
|---|---|
| `query_react.go` | Query 各 kind 实现：看板快照合成（board 权威优先回退树）、指标/token 聚合、邮箱清单、效率表、agent_events/agent_messages 下钻（Redis 热层 + PG 合并分页；`after_seq>0` 且热层有数据时只用热层） |
| `session_shared.go` | internalEvent → Event DTO；`trimDebugEvents`；`sanitizeUTF8` |
| `messages_store.go` | agent_messages 读写：**尾差量追加**（边界行前缀探测；`ArchiveMessages` 归档旧 run 快照，append-only 纪律）**（位于 `internal/agent/`）**；nil db no-op |
| `message_log.go` | Redis 热写消息（best-effort 2s 超时，绝不影响主循环）；`Clear` 复活前调用（seq 重置） |
| `evolver.go` | 会话结束自进化：≥5 事件才跑，一次轻量 LLM 产出画像增量/项目偏好/技能包 → PrefMerger 合并 → skillSink 落库 → evolution_log |
| `metrics.go` | 进程级 LLM 调用累计器（次数/超时/总时长/最大） |
| `notifier.go` | 会话终态 webhook（3s 超时、60s 同会话同状态去重） |
| `video.go` | 视频附件抽帧（ffprobe/ffmpeg）+ 原生视频 MIME 直通判定 |
| `imageutil.go` | 工具结果图片降采样（长边上限、box filter）+ 原图落盘 `.bma/images/` |
| `prompt_enhance.go` | 输入补全四层管线：L0 形态闸门 / L1 规则分类 / L2 LLM 仲裁（默认 30 runes 以下、10s 超时）/ L3 输出护栏 |
| `verify_evidence.go` | 校验证据纯函数：可执行验证/截图/运行时探针判定、场景证据报告、截图指纹 |
| `gear_signals.go` | 档位使用隐性信号（信号①快速档+5min 内行动指令；信号②集群档+30s 内被停），只落事件不改行为 |
| `models.go` | ModelManager 接口（ListModels/SwitchModel/AddModelEntry）+ 视图 DTO（**不含 api_key**） |
| `worktree.go` | worktree 视图 DTO + Reader/Operator 接口（dispatcher 实现） |

## 3.11 隐含约定与坑

1. **锁不可重入**：`store.mu` 下禁调 addEvent/persist*；持锁区要落事件须先解锁（多处注释点名）。
2. **先落事件再翻终态**：completed 前必须先 addEvent(agent_done)，否则 SSE 见终态即关流，最终答复到不了前端。
3. **运行中消息必走邮箱**：主循环不读 session.Messages，只写 Messages = 静默丢失；注入需 Send+poke。
4. **澄清通道互斥单槽** + 2s 自旋排队；答复后必须清 StreamingText（否则 live 帧重放出上一轮正文），而 persistInterimText 刻意**不清**（ask_user 要快照）。
5. **stopCtx ≠ session ctx**：stopCtx 从 Background 独立建，仅 stop/cancel/destroy/shutdown 取消；restartSessionContext 每次续跑重建。
6. **软停 vs 硬取消**：Stop 只标记+StopRunning+取消调用，状态由 dispatcher 收尾自然落 Paused；cancel 先置 error 再取消（isSoftStopCancel 判据依赖首错胜出守卫）。
7. **硬删除防复活**：先摘 map 再清理；落库前 stillLive 校验 map 指针身份。
8. **恢复的会话通道为 nil**：restoredSession 无 approval/askUser，自然回落普通续跑。
9. **热驻模式改变 Stop/取消语义**：session 级倒计时停用；cancel 仍级联杀 Idle。
10. **墙钟看门狗**：SessionMaxWallClockMin>0 时独立 goroutine，只对活跃四态生效；先置 error 再取消（顺序保证文案不被覆盖）。
11. **优雅停机**：先标记 running→error+统一中断文案，再 cancel（防 "context canceled" 覆盖文案）。
12. **恢复 limit 默认 50 > 内存上限 20**：恢复后立即淘汰已完成；seq 从历史 ID 复原（防新 ID 撞车）。
13. **视频/图片内存透传不落库**：firstTurnImages 一次消费即 nil；视频帧元数据拼文本才持久化。
14. **worktree 能力经 stopMarker 变量二次断言**：同一接口变量承载多能力（SoftStop/Worktree/Purge/PauseMark），未实现走断言失败分支。
15. **evolveSession 阈值**：minEvolveEvents=5 / digest 1500 runes / 90s 超时；画像合并 90s。
16. **持久化失败只记日志**：persistEvents/persistHistory 失败不阻塞会话（存储层另有 sanitize 兜底）。

## 3.12 会话恢复中间层（#21④，2026-09-23）

resume 路径（ResumeSessionAgents）头部调 `Dispatcher.RestoreSessionDomains`：按 (session_id, agent_id) 从 agent_messages + agent_tree_nodes 重建热驻槽（未终态/终态 30min 内 domain），重建槽同样被 armTTL/opResume 扫到（零特判）。三级连续体：热驻池（分钟级 LRU）→ 本条（同会话 resume）→ 领域档案（跨会话永久，#17）。边界：只还原落库数据已够的会话历史，不新增持久化面（#14"不做协作状态持久化"决策不破）。
