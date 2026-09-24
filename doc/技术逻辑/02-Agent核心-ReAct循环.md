# 02 Agent 核心：ReAct 循环

> 文档性质：只从代码还原的技术逻辑说明（不引用其他文档）。行号引用基于 2026-09-16 工作区。
> 涉及包：`backend/internal/agent`（核心文件 `react_agent.go` / `react_types.go` / `engine.go` / `tool_adapter.go` / `image_capability.go`）。

## 目录

- [2.1 包职责与文件清单](#21-包职责与文件清单)
- [2.2 核心数据结构](#22-核心数据结构)
- [2.3 RunWithHistory 完整流程（逐点）](#23-runwithhistory-完整流程逐点)
- [2.4 消息格式与 blades 转换](#24-消息格式与-blades-转换)
- [2.5 提示词三层与缓存纪律](#25-提示词三层与缓存纪律)
- [2.6 守卫与保护机制](#26-守卫与保护机制)
- [2.7 mailbox 交互](#27-mailbox-交互)
- [2.8 视觉能力缺失降级](#28-视觉能力缺失降级)
- [2.9 Engine 层（reflection / plan_execute）](#29-engine-层reflection--plan_execute)
- [2.10 活动上报与并发](#210-活动上报与并发)
- [2.11 隐含约定与坑](#211-隐含约定与坑)

---

## 2.1 包职责与文件清单

`internal/agent` 是整个系统的核心：单 Agent 的 ReAct（推理-行动）循环 + 会话服务 + 对外 Facade。

核心文件（本章范围）：

| 文件 | 职责 |
|---|---|
| `react_agent.go` | ReActAgent 定义与主循环 `RunWithHistory`；流式生成；提示词构建；裁剪/守卫 |
| `react_types.go` | ReactMessage/ToolCall/ToolResult/ReactResult DTO；LiveEvent 事件；blades 消息互转；各 Checker 接口 |
| `agent.go` | 对外 Facade 接口 `Agent`（HTTP/TUI 只依赖它） |
| `engine.go` | 派发执行模式引擎：ReflectEngine（自检重试）、PlanExecuteEngine（计划-执行） |
| `tool_adapter.go` | `domain/tool.Registry` → `ToolRegistry` 适配；Schema 白名单∪插件可见集过滤；Dispatch 图片降采样/门控兜底 |
| `image_capability.go` | "模型不支持图片输入"的识别、进程级缓存与降级（详见 2.8） |
| `react_memory.go` | MemoryPipeline 接口（Assemble/Write）+ NopMemoryPipeline 空实现 |
| `board_context.go` / `idle_roster_context.go` / `task_ledger_context.go` | 三个"上下文尾部注入"流水线包装器（看板/空闲领域清单/任务台账，仅 MetaAgent） |
| `errors.go` | 哨兵错误（ErrSessionNotFound / ErrAgentBusy / ErrAgentNotDirectable 等） |
| `types.go` | 会话 DTO（CreateRequest/Session/Event/ClarifyRequest 等）与图片/视频线型类型 |
| `image_capability.go` 之外还有 `message_log.go`（Redis 热层消息日志）、`messages_store.go`（PG 消息全量）、`models.go`（模型目录视图）、`metrics.go`、`notifier.go`、`video.go`、`imageutil.go`、`prompt_enhance.go`、`worktree.go`、`verify_evidence.go`、`evolver.go`、`session_shared.go`、`gear_signals.go` 等（详见第 03 章） |

## 2.2 核心数据结构

### ReActAgent（react_agent.go:35-158）

关键字段（构造后经 `With*` 注入）：

- `llm ModelProvider` + `providerFn func(ctx) (ModelProvider, error)`：**每次 LLM 调用前重解析 provider**（`generateOnce` 开头，react_agent.go:1300-1306），使运行中换模型（set_agent_model/TUI）下一次调用即生效；解析失败保留旧值。
- `tools ToolRegistry`：Schema()（每次迭代实时读）与 Dispatch()。
- `memory MemoryPipeline`：每次 LLM 调用前 `Assemble`（压缩/注入），事件 `Write`。
- `mailbox *mailbox.Mailbox`：异步消息（子 Agent 回传/用户注入/跨 Agent 问答）。
- `role types.RoleDefinition`：系统提示词基底 + 角色 ID。
- `name string`：agent 实例 ID（MetaAgent=sessionID，子 Agent=`session-1/domain-3`），兼作 mailbox 路由地址。
- `maxIter`（默认 50）；`llmTimeout` / `retryCount` / `retryBackoff`（LoopConfig）。
- `historyMaxMessages`（滑动窗口硬上限）；`toolOutputMaxRunes`（工具输出入史截断）。
- `toolResultDumpRunes`/`toolResultDigestRunes`：工具结果超阈值全文落盘 `.bma/tool_outputs/`，历史留摘要+路径（react_agent.go:996-1015）。
- `staleToolEvictRounds`：陈旧只读工具结果驱逐轮数（请求视图变换，react_agent.go:1077-1130）。
- `agentsMDMaxRunes`：项目自述（AGENTS.md/CLAUDE.md）注入上限。
- `parallelTools` + `toolParallelMax` + `pathMutexes`：轮内并行工具执行与**同路径写互斥**（react_agent.go:829-845, 1162-1207）。
- `tokenBudget`：上下文 token 阈值（唯一上限，默认 150K 按角色分级）；超出即 `LimitReached` 收口（react_agent.go:649-654）。
- `liveFn func(LiveEvent)`：实时事件回调（UI 流式/工具/子 Agent 事件）。
- `pendingChecker` / `pausedChecker` / `suspendGate` / `suspendOnChildWait`：父终结保护与挂起等子语义（2.3.9）。
- `activityReporter func(kind string)` + `streamKeepalive` / `streamIdleTimeout` / `streamFirstChunkTimeout`：心跳证据与流式看门狗。
- `sysPromptOnce`/`sysPromptCache`：系统提示词**按实例冻结**（sync.Once），保证跨轮前缀缓存命中。
- `msgLogger MessageLogger`：每条入史消息热写 Redis（`appendLogged` 收口，react_agent.go:440-445）。
- `promptStatsSegs`：提示词分段计量（persona/env/role_base/discipline/skill）。

### DTO 与结果

- `ReactMessage`（react_types.go:23-41）：`Role` / `Content` / `ToolCalls` / `ToolCallID` / `ReasoningContent` / `Images`（`json:"-"` 仅内存透传，工具截图）。
- `ToolCall`：`ID` / `Name` / `Input map[string]any`。
- `ToolResult`（react_types.go:163-171）：`Tool` / `Success` / `Output` / `Error` / `Images`。
- `ReactResult`（react_types.go:174-199）：`Text` / `History`，加四个互斥的暂停语义标记 —— `LimitReached`（自身触限）/ `PausedOnChild`（子触限）/ `SuspendOnChildWait`（顶层挂起等子），以及 `Unverified`/`VerifyNote`（校验层 fail-closed）/ `MachineCheck`（dispatcher 机器校验段）。
- `LiveEvent`（react_types.go:230-247）：`Kind`（llm_delta/tool_call/tool_exec/think_delta/sub_agent_done/token_usage/peer_ask/milestone/notify）+ `Agent`/`AgentID`（emit 时自动填充，react_agent.go:456-467）+ Text/Tool/Input/Output/Error/Success + token 用量（含缓存命中/未命中）。
- 接口族（react_types.go:251-362）：`ToolRegistry`、`RoleProvider`、`PendingChildrenChecker`、`PausedChildChecker`、`PausedDomainResumer`、`DispatchCountResetter`、`SoftStopMarker`、`SuspendGate`、`IdleRosterProvider`、`IdleTTLArmer`、`SessionAgentWaker`、`ModelProvider` —— 全部由 `domain/subagent.Dispatcher` 或 ReactService 实现，agent 包只声明接口（防循环依赖）。

## 2.3 RunWithHistory 完整流程（逐点）

入口：`Run(ctx, input)` → `RunWithHistory(ctx, input, nil)`（react_agent.go:472-475, 544）。

### 2.3.1 前置 ctx 注入（544-563）

1. `ctx = WithAgentID(ctx, a.name)`（mailbox 路由身份）。
2. `ctx = WithRoleID(ctx, a.role.ID)`（工具侧角色级写沙箱）。
3. `ctx = WithAgentDisplayName(ctx, a.role.Name)`（日志/UI 展示名）。
4. `ctx = tool.WithImageInputSupported(ctx, func() bool { return !a.noImageInput() })`（图片门控闭包，实时读进程缓存）。
5. 有 logger 时 `ctx = logger.NewContext(ctx, a.log)`（轻量 LLM 调用链路取 logger）。

### 2.3.2 首条 user 消息（565-582）

- 追加 `ReactMessage{Role:"user", Content:input}`；若 ctx 带用户图片（`UserImagesFromContext`，Alt+V 粘贴）则挂到该消息 `Images`。
- 若图片存在且 `a.noImageInput()` 为真：**追加一条说明消息**（imageUnsupportedNotice），让模型知道图被省略。

### 2.3.3 系统提示词（584-585）

`system := a.systemPrompt()`（sync.Once 冻结，见 2.5）。

### 2.3.4 主循环（599-932），每轮步骤

1. **suspendGate.Park**（602-606）：热驻模式下会话挂起期间阻塞；恢复返回 nil 继续。
2. **drainMailbox（generate 前）**（607-615）：压缩用户注入消息延迟上界；有新消息重置停滞计数。history 以 tool 结果（或首条 user）收尾，追加 user 角色不破坏 tool 配对。
3. **memory.Assemble**（617-619）：压缩历史/注入记忆视图（压缩在 `domain/memory.Pipeline` 内按 token 阈值触发）。
4. **evictStaleToolResults**（620-623）：`staleToolEvictRounds` 轮前的 ReadFile/SearchInFiles 结果替换为"已驱逐需重读"占位符（仅请求视图；canonical history 不动）。
5. **append(buildTimeMessage())**（625-627）：当前时间以**尾部 system 消息**注入（放头部会打碎前缀缓存）。
6. **windowMessages**（629-633）：消息数滑动窗口硬上限（默认 400，兜底防上下文溢出）；保留 system 前缀 + 首条 user，下刀避开孤立 tool 结果。
7. **sanitizeToolPairing**（634-637）：强制修正 tool_calls ↔ tool 结果配对（缺失补合成错误结果、孤立结果丢弃），避免 API 400 拒绝整轮（实证曾白跑 31m43s）。
8. **剥图**（639-644）：已知模型不支持图片时 `stripImagesForRequest`（视图变换）。
9. **tokenBudget 检查**（646-654）：估算超阈值 → 返回 `{LimitReached:true}`（部分完成，上层暂停会话等续跑）。
10. **ToBladesMessages**（656-657）→ 构造 `blades.ModelRequest{Instruction: system, Messages, Tools: a.tools.Schema()}`（**Schema 每轮实时读**，插件热插拔下一轮生效）。
11. `logPromptStats` + `reportStableHash`（666-671）：提示词构成计量与 stable 层哈希监控（变化即 WARN）。
12. **generate**（672）：链式中间件（2.3.5）。

### 2.3.5 generate 与失败分支（672-688）

- `generate`（react_agent.go:1274-1284）构造中间件链：`RetryLLM(retryCount, backoff, shouldRetryLLMCall)` → `CallLLM(llmTimeout)` → `TerminalCall`。
- `shouldRetryLLMCall`（image_capability.go:52-63）：ctx 已取消 / DeadlineExceeded / **图片不支持** 均不重试。
- `generateOnce`（1297-1329）：`providerFn` 重解析 provider → `touchActivity("llm_start")` → provider 实现 `NewStreaming` 则走 `generateStreaming`，否则一次性 `Generate` → 记日志（`logLLMCall`）→ `llm_end`。
- **失败分支**（react_agent.go:673-688）：若 `model.IsImageInputUnsupported(err)` 且本 run 未降级过 → 标记模型、注入说明消息、emit `LiveEventNotify`、`continue`（下一轮已剥图不会复现）；同 run 第二次复现则照常报错（防 400 空转）。

### 2.3.6 流式生成（generateStreaming，1494-1620）

- 用 `sp.NewStreaming(streamCtx, req)` 迭代产出；判断块是"增量"还是"累积"（前缀关系），accumulate 到 `display`，emit `LiveEventLLMDelta`。
- Metadata["thinking"] 非空 → emit `LiveEventThinkDelta`（瞬时思考，不持久化）。
- 看门狗 goroutine（每 keepalive 周期）：首块前用 `streamFirstChunkTimeout`（默认 120s）判死；首块后用 `streamIdleTimeout`（默认 300s）判死；判死即 cancelStream，使 RetryLLM 视作瞬时故障重试。
- 每块 `touchActivity("stream")`；`keepalive` tick 不刷新 lastTS（防假活豁免）。

### 2.3.7 响应处理（690-816）

1. token usage 事件（692-707）：`LiveEventTokenUsage` + cache hit/miss（provider 经 Metadata 透传）。
2. `AssistantMessageFromBlades`（710）：解析文本 + ToolParts；**参数 JSON 损坏的 ToolCall 被丢弃并在 `dropped` 列表返回**（防 nil 入参静默写坏文件）。
3. **坏工具调用保护**（718-732）：`badToolCallStreak++`；≥3 次（maxBadToolCallResponses）显式报错；否则注入 `badToolCallNudge` 要求修正重发（防"静默收官"）。
4. **空响应保护**（738-748）：无文本无工具调用 → 连续 3 次（maxEmptyResponses）报错；否则注入 `emptyResponseNudge` continue。
5. **非空响应入史**（754）：入史副本经 `truncateToolCallInputsForHistory`（单字符串值截 12000 runes，防 WriteFile 全文在滑窗内累积烧预算）。
6. **无 tool_calls = 终答候选**（757-816）：
   - 先 `drainMailbox`：有新消息则 `continue`（让模型基于完整摘要重新生成，防"请稍候"被误当终答）。
   - `pendingChecker.PendingChildren > 0` 时的三分支：
     a. `pausedChecker.HasPausedChild` → 返回 `{LimitReached, PausedOnChild}`（上层置会话暂停态）；
     b. `suspendOnChildWait`（仅会话顶层）→ 返回 `{Text, SuspendOnChildWait}`（上层置 awaiting_child，子完成回调/用户消息唤醒续跑）；
     c. 否则 `waitForChildren` 阻塞等待（**不烧 LLM 轮次**，等子期间 `touchActivity("child_wait")` 仅展示态）；其中 paused 返回 PausedOnChild。
   - `memory.Write(answer)` 后返回 `{Text, History}`。
7. **有 tool_calls**（818-925）：
   - 先批量 emit `LiveEventToolCall`（UI 同时显示执行中）。
   - **并行段**（829-845，`parallelTools && len>1`）：信号量限 `toolParallelMax`（默认 4）；写类工具（WriteFile/EditFile/RestoreFile）按目标路径 `pathLockFor` 互斥；各自 keepalive/活动上报。
   - **串行回填段**（857-910）：逐条处理。`errors.Is(err, tool.ErrLoopExit)` → 直接带原因终止循环返回；否则结果序列化 emit `LiveEventToolExec` → `condenseToolResult`（超阈值落盘）→ 截断 → 追加 tool 消息（含 ToolCallID 配对与 Images）→ `memory.Write(tool_call)`。
   - **工具结果全部入史后才 drainMailbox**（912-917）：mailbox 消息是 user 角色，插在 tool_calls 与结果之间会触发配对 400。
   - **stagnationGuard**（919-925）：产出性工具集合（写文件/派发/消息/浏览器驱动，`productiveToolNames`，react_agent.go:952-958）或 mailbox 有新消息即清零；连续无产出 10 轮预警 / 20 轮最终通牒 / 30 轮 `ErrLoopExit` 硬杀（**meta 豁免硬杀**，react_agent.go:965-990）。

### 2.3.8 循环结束（928-931）

`maxIter` 到限（>0 时）：返回 `{History, LimitReached:true}`，非错误——上层暂停会话等用户续跑。

## 2.4 消息格式与 blades 转换

`ToBladesMessages`（react_types.go:366-497）要点：

- **tool 结果回填 Name+Request**：blades 的 openai 序列化要求 tool 消息的 ToolPart 带全字段，否则 `function` 字段缺失会 400。
- **图片挂载边界**（防 base64 反复进上下文烧前缀缓存）：
  - user 图片：**仅最后一条 user 消息**挂 DataPart；
  - 工具图片：**仅最后一批连续 tool 结果**（从尾部定位）挂 DataPart。
- assistant 消息：文本 TextPart + 每个 ToolCall 一个 ToolPart（Request=JSON(Input)）；`ReasoningContent` 经 Metadata["reasoning_content"] 回传（DeepSeek V4 要求）。
- 未知角色兜底转 user 文本。

`AssistantMessageFromBlades`（react_types.go:503-548）：ToolPart.Request 解析失败**丢弃调用**并记录；Metadata 里 reasoning_content 存入 ReasoningContent。

## 2.5 提示词三层与缓存纪律

三层易变度递增（react_agent.go:1209-1234）：

1. **stable（system instruction）**：`systemPrompt()` 按实例冻结（sync.Once）。构建顺序（`buildSystemPrompt`，1802-1866）：
   - `envBlock`（buildEnvBlock，1874-1917）：OS/时区/工作目录 + 【项目概览】（`.bma/PROJECT.md`）+【项目自述】（AGENTS.md 优先，CLAUDE.md 次之，截 `agentsMDMaxRunes`）；
   - 角色 base 提示词（role.SystemPrompt）；
   - 固定【执行纪律】块（4 条：工具节制/产出必验证/完成即停/ mailbox 消息语义）；
   - 技能元数据块（`skillBlock`，渐进披露第一层）；
   - 人格/画像前缀（PersonaInjector 在最前，`CombinePersonaInjectors` 顺序组合；CompositeInjector 见 167-230）。
   - 分段 rune 计量存 `promptStatsSegs`。
   - `reportStableHash` 每轮打哈希，变化即 WARN（冻结被破坏的第一现场）。
2. **context（压缩视图）**：memory.Pipeline 冻结视图，仅压缩触发轮变化。
3. **volatile（尾部）**：时间消息、近期事件、看板段、空闲领域清单、任务台账 —— 全部**尾部注入**，不破坏前缀。

> 关键不变量：**stable 层跨轮字节稳定**。任何每轮变化的段（时间/看板/事件）都必须走尾部注入；子 Agent 的 domain 身份头也追加在**末尾**（dispatcher 侧，react_agent.go 之外）以避免截断公共前缀。

## 2.6 守卫与保护机制

| 机制 | 位置 | 语义 |
|---|---|---|
| 空响应保护 | react_agent.go:738-748 | 连续 3 次空响应 → 报错（防无声完成） |
| 坏工具调用保护 | 718-732 | 连续 3 次参数非法被丢弃 → 报错；否则 nudge 重发 |
| 停滞守卫 | 965-990 | 10/20/30 轮无产出阶梯（预警/通牒/ErrLoopExit 硬杀；meta 豁免硬杀） |
| windowMessages | 1674-1726 | 消息数滑窗；从 assistant/user 起刀避孤立 tool；首条 user 保底 |
| sanitizeToolPairing | 1735-1785 | 发送前强制配对（最后防线） |
| evictStaleToolResults | 1077-1130 | 只读探查结果过期驱逐（视图层） |
| condenseToolResult | 996-1015 | 大输出落盘 + 摘要 |
| 流式看门狗 | 1519-1566 | 首块 120s / 块间 300s 判死重试 |
| 上下文预算 | 646-654 | token 估算 ≥ budget → LimitReached |
| 视觉降级 | 673-688 | 图片不支持 → 剥图续跑（一次性） |

## 2.7 mailbox 交互

- `drainMailbox`（1967-2007）：`mailbox.Drain(a.name)` 取全部未读 → 每条转 `ReactMessage{Role:"user", Content:"[mailbox from X] ..."}`（mailboxMessageToReact：主题/正文/Payload 整体包 `WrapUntrusted("mail:<From>")` 围栏，2026-09-20 防线延伸到 Agent 间通道——兄弟 Agent 产出对收件方是不可信内容；**From=user 用户直接指令豁免**（指令优先级最高，降格为"数据"违义）；框架信号留围栏外——[升级] 前缀、[mailbox from X] 前缀、"修改文件" 清单；MsgEscalate 加 [升级] 前缀、Payload JSON 追加、FilesModified 列表追加）→ emit 事件（**From=user / From=system 不推完成事件**（system=依赖就绪等通知，正文照常入史）；MsgRequest/MsgEscalate 推 `peer_ask`；`MsgMilestone` 或 subject 前缀「里程碑:」的 info 推 `milestone`（中途播报非完成，2026-09-17）；其余 `sub_agent_done`）→ `memory.Write(sub_agent_summary)`。
- drain 时机：主循环顶部（generate 前）、无 tool_calls 分支、工具结果全部入史后、waitForChildren 循环内。**不能在 tool_calls 与 tool 结果之间注入**（配对 400）。
- `waitForChildren`（1639-1662）：`PendingChildren>0` 时每 30s `WaitForAnyChild` + drain；drain 到新消息或 ctx 取消即返回；`HasPausedChild` 时返回 paused=true。

## 2.8 视觉能力缺失降级（image_capability.go）

- 触发：provider 报错且 `model.IsImageInputUnsupported(err)`（`internal/model/capability_err.go`：小写化后含 `"not support image"`，或含 `"support image input"` 且含 `"400"`）。
- 进程级记忆：`imageUnsupportedModels sync.Map`，键=provider 的 `ModelName()`（空名不落缓存防误伤）。
- 生效点：
  1. 主循环失败分支：首次命中 → 标记 + 注入说明 + notify 事件 + continue（react_agent.go:673-688）；
  2. 请求视图剥图：`stripImagesForRequest`（仅清 `Images`，不动 Content）；
  3. 首条 user 消息带图且已知无视觉 → 预注入说明（580-582）；
  4. 工具侧 ctx 门控：`tool.ImageInputSupportedOf(ctx)` —— ReadMedia 内自拦 + `tool_adapter.Dispatch` 收口兜底（tool_adapter.go:186-190）。
- 换模型即时恢复（每轮实时读缓存，无粘性标记）。
- 重试快速失败：`shouldRetryLLMCall` 排除该错误（避免白烧 retryCount 次调用）。

## 2.9 Engine 层（engine.go）

派发执行模式（`call_sub_agent` 的 mode 参数，engine.go:20-28）：

- `react`（默认）：裸 `ReActAgent.Run`。
- `reflection`：`ReflectEngine.Run`（120-166）——先跑完整 ReAct，再用 judge LLM 按 rubric 逐条自检（`reflect`，172-186：提取验收条目 + 附【修改文件】【验证证据】，要求 JSON verdict）；不达标带反馈 `RunWithHistory(reflectionRetryMessage, history)` 重试（默认 2 轮）。
  - **fail-closed**：judge 缺失/坏 JSON → `Unverified=true`（不静默放行）；但 TODO #54 降级：若存在 L0 可执行证据（`HasExecutableVerification`）则"降级放行"（VerifyNote 标注）。
  - `extractJSON`（204-242）容错解析（剥围栏 + 平衡花括号扫描）。
- `plan_execute`：`PlanExecuteEngine.Run`（301-333）——辅助 LLM 拆 2-8 步（每步自包含指令），`planSink` 落看板（`planTaskID` 前缀防与 meta write_plan 冲突），逐步 `RunWithHistory`（前序历史累积），`planProgress` 回写看板状态；超 `maxSteps` 截断并落 failed；最后 `planExecuteFinalizeMessage` 收口终答。规划失败整体降级裸 ReAct。
- `NewEngineLLM`（398-413）：provider → LLMComplete 适配；**流式优先**（ark coding 端点拒绝非流式长任务）。
- 步骤报告模板 `planStepReportFormat`（373-377）约束瘦身输出。

## 2.10 活动上报与并发

- `touchActivity(kind)`（449-453）：kind 约定 `llm_start`/`llm_end`/`tool:<名>`/`tool_end`/`stream`/`keepalive`/`child_wait`；由 dispatcher 的心跳巡检消费（见第 04 章）。
- 工具派发的 keepalive（`dispatchToolWithKeepalive`，1136-1160）：长工具执行期间每 30s 上报 keepalive（不刷新 lastTS，防假活）。
- 并发点：并行工具段（goroutine + 信号量 + 路径互斥）、流式看门狗 goroutine（`keepaliveDone` 关闭退出）、`emitLive` 回调可能被多 goroutine 调（下游须自保）。
- `appendLogged` 是**唯一入史收口**（热层 seq = 追加前 history 长度，与 agent_messages 下标口径一致）。

## 2.11 隐含约定与坑

1. **history 是唯一可变状态**：ReActAgent 跨 run 无状态设计，一切续跑靠传入/返回 history；暂停/续跑（dispatcher）靠持久化 history 重建实例。
2. **追加 user 角色的时机极敏感**：mailbox/nudge 注入一律在"tool 结果齐全之后"或"无 tool_calls 分支"；否则 Anthropic 配对校验 400 拒绝整轮。
3. **emitLive 的 Agent 字段填 `role.Name`**（展示名），`AgentID` 填 `a.name`（实例 ID）；mailbox 路由用 `a.name`。
4. **图片只挂"最后一轮"**：user 最后一条 + tool 最后一批；旧轮 base64 自动卸载（词表占位符仍留）。
5. **`Images json:"-"`**：不进持久化、不计 token 估算。
6. **stable 层哈希恒定是断言**：`reportStableHash` 变化即 WARN。
7. **meta 豁免停滞硬杀**：ErrLoopExit 会终止整个会话，故 meta 只收预警。
8. **`maxIter<=0` 表示不限制**（meta 用 0）；tokenBudget 才是 meta 的实际收敛闸。
9. `truncateRunes` 按 rune（中文安全）；多处截断阈值硬编码（如 `historyToolCallInputMaxRunes=12000` 无配置项）。
10. 会话级 logger 经 ctx 传递（`logger.NewContext`），轻量 LLM 调用（事实提取/打捞/judge）靠它写 session_logs。

## 2.12 记忆索引与轮末微压缩（#20③/#22①，2026-09-23）

- **记忆索引槽**（memory_index.go + bootstrap/memory_index.go）：会话启动注入【记忆索引】一行式沉淀索引（global_knowledge ListIndexEntries 按 last_accessed 降序 + 用户画像摘要首条），双配额 `memory_index_max_lines: 200` / `memory_index_max_runes: 25000`——超限带【记忆索引超限】重写指令逼模型整理旧沉淀（不静默截断，CC errors#memory-index 同款）；`<untrusted_data` 围栏行结构性丢弃（provenance 门 + 召回循环防护，filterJunkFacts 同款丢围栏事实）。注入位 skillBlock 之后（promptStatsSegs memidx 记账）。
- **轮末微压缩**（ContextEngine.AfterTurn）：Assemble 尾部每轮调用——快速档在此做旧 tool result stub 修剪（>200 rune，近保留段不动，幂等防重复改写破前缀缓存）。契约=条数不变（TailStart 下标依赖），详见 06 章引擎节。
