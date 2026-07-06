## 已完成
1.**风险审计修复（2026-07-05）**：基于 `workspace/risk_report.md` 的 P0/P1/P2 风险逐项核实并修复真实缺陷：
  - P0-01：`backend/main.go` 为 `http.Server` 配置 `ReadTimeout`/`WriteTimeout`/`IdleTimeout`。
  - P0-02：`NewPostgresStore`/`NewRedisStore` 接收 `context.Context`，内部用 5s 超时包装 `PingContext`/`Ping`，避免启动挂死。
  - P0-03：`backend/internal/dag/dag.go` 的 `Trigger`/`SaveDAG` 错误改为 `log.Printf` 记录，不再静默丢弃。
  - P0-04：`backend/internal/logger/logger.go` 异步写 `session_logs` 失败时记录结构化 `slog.Error`。
  - P0-05：`backend/internal/memory/block_vector.go` 反序列化 `facts` 失败时记录日志。
  - P1-01：`agent_common.go` / `domain_skills.go` 的 `log.Printf` 替换为结构化 `logger.Error`/`logger.Event`。
  - P1-02：块记忆归档与 `session_logs` 异步写入增加 3 次指数退避重试。
  - P1-04：新增 `/api/metrics` Prometheus 端点（goroutine/内存/会话/LLM 调用/超时）。
  - P2-04：`provider_ollama.go` URL 解析失败返回错误，不再静默回退。
  - P2-05：`store/postgres.go` / `store/dag.go` / `store/session_logs.go` 多处忽略的 `json.Unmarshal` 错误增加日志。
  - 已核实不成立/已修复：P0-06（TUI HTTP body 已关闭）、P1-03（`evictCompletedSessions` 确实从 map 删除）。
  - 未改动（架构债务）：P2-01/P2-02（大文件拆分）、P2-03（SubDomainAgent 保留但运行时禁用）。
2.添加基于DAG（有向无环图）的定时任务逻辑流程
2.把每个Agent的上下文窗口不写死，根据配置变化、每个子Agent的skill库大小、工具调用轮数等写死的数据都修改为配置动态参数
3.在domainAgent执行后，块记忆存储后，增加向量检索，在需要检索到这些上下文时，从向量库中检索出最相似的块，并返回给domainAgent。
5.添加"人机对话"处理，agent遇到需要确定的问题，以提问的方式给到用户，用户回答添加到上下文中，agent根据用户回答，进行下一步处理。
6.添加抢占中断与队列注入处理。抢占中断：一个任务执行中，用户点击暂停、终止任务并重新输入指令时，上一轮的输入上下文全部清除，重新根据当前指令进行上下文注入。队列注入：一个任务执行中，用户添加指令时，使用队列注入当前指令进入上下文，再继续任务。
7.添加用户输入信息的智能路由，智能判断是否简单问题，简单问题由主agent直接回答，复杂问题派发子agent处理，子agent处理完成后，返回给主agent。
8.**5路径智能路由与编排瘦身**：新增 `router.go` 实现 5 路径精确路由（direct_tool / direct_assistant / create_domain / multi_domain / full_four_layer），规则层（零 LLM）+ LLM 兜底 + 安全兜底（create_domain，零回归）。MetaAgent 重写 `handleInitial` 按路由分派：纯 QA / 单工具请求（读文件、跑命令、查天气）走 0 层 `RouteDirectTool`，MetaAgent 自跑 blades 工具循环；单领域简单任务（修 CSS、改文案）走 1 层 `RouteDirectAssistant`，MetaAgent 直接建助手执行——**80% 任务不再进入四层编排**。复杂任务才拆领域，仅 `RouteFullFourLayer` 启用 SubDomain。
9.**MetaAgent/DomainAgent 可直接执行工具与调用助手**：提取公共执行入口 `agent_common.go`（CommonExecuteAssistantTask / CommonCreateAssistantForTask / CommonMatchFixedAssistant / CommonCollectTaskSummaries），消除 DomainAgent 与 SubDomainAgent 的逐行重复代码。MetaAgent 新增 toolCallback + `executeDirect`，能直接跑工具循环与创建助手；DomainAgent/SubDomainAgent/AssistantNode 统一走公共入口。**修复 AssistantNode 无工具执行的潜在 bug**（SubDomain→Assistant 路径原本只做单次 LLM 调用，现已具备完整 ReAct 工具循环）。
10.**SubDomain 自适应启用**：`shouldSplitToSubDomains` 从恒 `return false` 改为读 `state.EnableSubdomain` + 跨子领域边界检测（`detectSubdomainBoundaries` 规则 + 可选 LLM 兜底），仅复杂跨层任务才进入第四层，避免单领域任务不必要的 overhead。
11.**Plan-and-Execute + Self-Reflection**（原待完成 #1）：新增 `plan.go`，DomainAgent 在多任务时调重量模型生成结构化 `ExecutionPlan`（步骤列表），按步骤顺序派发助手并支持断点续行（`PendingGoals`/`MarkDone`）。`CommonExecuteAssistantTask` 内置 `reflectOnResult`（轻量模型评估结果），不达标时带反馈重试一次（仅一次防死循环）。Feature flag：`agent.plan_enabled` / `agent.reflection_enabled`，默认关闭，按需开启。
12.**前端会话复用修复**（原待完成 #3）：修正 `web/src/views/chat/index.vue` `handleSubmit`——追加消息的判定从仅 `status==='running'` 扩展到 `running/completed/error`（非 `awaiting_clarify` 即追加），后端 `POST /api/sessions/{id}/message` 已支持向已完成会话追加并 `resumeSession`；已完成会话续话时重新建立 SSE 流。新增 `localStorage.lastSessionID`，刷新页面后无 URL id 时优先恢复上次会话。**每条消息不再新开会话栏**。
13.**块记忆持久化可靠性**（原待完成 #4）：(a) 异步归档失败日志从 `fmt.Printf` 改为结构化 `log.Printf`（含 sessionID/domain/error）；(b) `SessionBlock.archived`（atomic.Bool）标记异步归档是否成功，`switchToNextBlock` 在块切换前做**幂等兜底归档**（未确认成功则同步补写，短超时，失败仅日志不阻塞）；(c) 启动期 `ValidateEmbeddingDimension` 校验 `global_knowledge.embedding` 列维度与配置一致，不一致 `log.Fatalf`（避免维度不匹配导致 SaveKnowledge 静默失败、零落库）。
14.**私有 Episode 记忆与快照主线接入**：(a) 修复 `session_events` 写入/读取 SQL 语法错误；(b) 新增 `EnsureInitialMemorySchema`，启动时自动创建 `agent_private_memory` / `agent_snapshots` / `topics` / `global_knowledge` 等 `001_init.sql` 表；(c) `main.go` 初始化 `WriteProcessor` / `CallbackHandler` / `ContextAssembler` / `Compressor` / `SnapshotManager`，并注入三层图；(d) `DomainAgent`/`SubDomainAgent` 生命周期调用记忆回调，加载/保存快照；(e) `AssistantNode` 调用 LLM 前通过 `ContextAssembler` 注入私有记忆与全局知识；(f) `MetaAgent` 的 `Watchdog` 触发压缩时调用 `Compressor`。


**P0-1. 块记忆检索领域过滤**（原待完成 #3，违背"话题隔离"核心承诺）
- 现状：`SearchBlockMemory(ctx, query, topK)` 底层 `SearchKnowledgeByType` 仅按 `knowledge_type='block_memory'` 过滤，**无 domain 过滤**，存在跨领域串扰。
- 修复：
  - `SearchBlockMemory` 增加 `domain` 参数，先领域过滤再语义匹配（参照已实现的 `SearchDomainArchive(ctx, domain, goal, topK)`）。
  - `meta->>'domain'` 加 btree 表达式索引，避免 pgvector + jsonb 全表扫。
  - 返回结构化摘要（含关键事实、涉及文件），控制总长度不超过 TokenBudget 的 20%。
  - 检索时间 < 200ms（10 领域 × 10 记录测试集）。

**P0-2. 回调写入优化**（数据可靠性根基）
- 现状：`memory/callback.go` `OnEnd` 起两个 goroutine，独立 ctx，失败静默；Episode 写失败时快照仍写，数据不一致；无重试无幂等。
- 方案（显式同步写入）：
  - `OnEnd` 直接同步调用 `WriteProcessor.ProcessWithStepCount` 写入 Episode
  - 直接同步调用 `SnapshotManager.SaveFromState` 保存快照
  - 写入失败仅记录日志，不阻塞主路径
  - 删除内存队列、后台 worker、指数退避重试、死信表 `memory_write_failures`、启动回放等复杂机制
  - 保留幂等键 `(agentID, topicID, stepCount)` 唯一索引，避免重复写入
- 失败可见：结构化日志含 sessionID/agentID/step/action/error。
**P0-3. 大文件拆分 + 技术债清理**（其他改动的前置）
- 现状：`meta_agent.go` 1545行、`domain_agent.go` 1218行、`subdomain_agent.go` 576行、`llm_tracker.go` 476行，违反 TODO#4 自定"80行内"规则。
- 拆分目标：
  - `meta_agent.go` → `meta_agent.go`(核心 Invoke) + `meta_routing.go`(ClassifyTask/5路径) + `meta_mailbox.go`(processMailbox) + `meta_watchdog.go`(runWatchdog) + `meta_handle.go`(handleInitial/switchToNextBlock)
  - `domain_agent.go` → `domain_agent.go`(Invoke) + `domain_tasks.go`(analyzeTasks/dispatch) + `domain_archive.go`(归档复用) + `domain_plan.go`(Plan-Execute 集成)
  - 进一步下沉重复代码到 `agent_common.go`
- 目标：单函数 ≤80 行，单文件 ≤400 行。
- 同步清理：
  - 删 `pkg/types/types.go` 中 4 个 enums 别名（EventType/EventStatus/ActionType/CompressionLevel），全局替换为 `enums.X`（postgres.go 4 处）
  - 删 `assistant.go` 中 `callLLM`/`formatContextPack` 死代码
  - 删 `meta_agent.go` 中 `classifyComplexityLLM` 死代码
  - `go vet`/`gofmt` 零告警（gofmt 单独一次格式化提交）

**P1-1. 删 DomainArchive 召回 + 块记忆键值化**（合并重叠机制）
- 现状：DomainArchive（特性4）与 BlockMemory（特性3）同表存两份近同内容，生命周期割裂，场景C（Archive 过期 BlockMemory 仍在）导致 Skill 装配与 prompt 注入割裂。
- 方案：
  - 删 DomainArchive 相关代码（`store/domain_archive.go` + `domain_agent.go` 中 `ensureSkillSet` 归档复用路径）
  - `ensureSkillSet` 改为只走 LLM AssembleSet
  - BlockMemory 键值化：`Meta.facts` 改为结构化数组
    ```
    facts: [
      {key:"vue_version", value:"3.4", scope:"global"},
      {key:"css_framework", value:"tailwind", scope:"domain"},
      {key:"error_pattern", value:"padding不一致", scope:"task"}
    ]
    ```
  - 检索精细化：当前领域任务只取 `scope=domain`+`scope=task`，全局共享取 `scope=global`
  - `write.go` 现有 `Facts` 三元组可复用，改格式即可
- 收益：注入 prompt 按需取键，token 省、精准度高。

**P1-2. 完整日志 + Agent IO 可视化**（调试与可观测基础）
- 现状：8 处 log.Printf 散落，`llm_tracker.go` 476 行已有追踪基础但未串联。
- 目标日志链路：
  ```
  MetaAgent.Invoke
    ├─ [routing] goal=xxx → path=create_domain
    ├─ [llm_call] prompt=xxx → response=xxx (tokens=1234, latency=800ms)
    ├─ [task_split] tasks=[a,b,c]
    ├─ [domain_create] domain=css skills=[vue,css]
    │
    ├─ DomainAgent.Invoke
    │   ├─ [memory_recall] block_memory query=xxx → hits=2
    │   ├─ [memory_inject] context=xxx (truncated to 2000 tokens)
    │   ├─ [llm_call] prompt=xxx → response=xxx
    │   ├─ [tool_call] WriteFile path=xxx size=1.2KB
    │   └─ [result] summary=xxx
    │
    └─ [meta_summary] session done, total_tokens=12345
  ```
- 实现：
  - 结构化 JSON 日志（zap/slog），按 sessionID 串联
  - 日志写入 `session_logs` 表，Web API `/api/sessions/{id}/logs` 查询
  - Web 端日志分析页：按 session/agent/level 过滤，Prompt/Response 全文可展开（脱敏 API Key）
  - Token 消耗面板：按 Agent/模型/任务类型聚合

**P1-3. 特性加 TODO 暂不做**（聚焦主路径）
- 以下特性保留代码但 feature flag 默认关，写入此 TODO 待完善，默认关的 feature 需有单测覆盖防代码腐烂：
  - Plan-and-Execute + Self-Reflection（`agent.plan_enabled` / `agent.reflection_enabled`）
  - SubDomain 自适应启用（`state.EnableSubdomain`）
  - DAG 定时任务流程（特性1）
  - 抢占中断与队列注入（特性6）
  - 人机对话处理（特性5）
  - DomainAgent 持久化归档（特性4，P1-1 删除后此条作废）

### P0 — 核心承诺修复，必须先做

**P0-1. 轻量级总结模型优化** ✅ 本轮已完成
- 现状：超时后无重试机制，需要3次重试机制。每个Agent在启动时添加一个测试，测试Agent是否成功访问LLM，若无响应则重试，重试三次失败则启动失败，报错哪些Agent没有成功连接，主要是配置中与固定助手检验。
- 本轮实现：`LLMCallTracker.CallWithTimeout` 内置 3 次指数退避重试（共享 `retryGenerate` helper，保留单次逻辑调用→单次 RecordCall 契约）；新增 `ModelFactory.VerifyConnectivity` 启动期按 (provider,model,key,baseURL) 去重探测每个已配置角色，失败聚合报错；轻量直连 callers（`reflectOnResult`/`summarizeHistoryForGoal`）改走 `CallLightweightWithRetry`；接入 `testserver.BuildHandler` 与 TUI 入口，失败 `log.Fatalf` 报告未连通角色。

**P0-2. 回调写入优化**（数据可靠性根基）✅ 已完成
- 已实现：`memory/callback.go` 改为显式同步写入，`OnEnd` 直接调用 `WriteProcessor` 写 Episode、`SnapshotManager` 保存快照；删除队列、后台 worker、指数退避、死信表 `memory_write_failures`、`ReplayDeadLetters`；保留幂等键与 `callback_test.go` 覆盖。

**P0-3. 大文件拆分 + 技术债清理**（其他改动的前置）✅ 本轮已完成
- 还有部分大文件，可后期优化
- 现状实测（2026-07-02）：`three_layer_graph.go` 814行 / `tool_executor.go` 714行 / `llm_tools.go` 449行 / `llm_tracker.go` 499行，4 个文件超 400 行限制
- 继续拆分目标：
  - `three_layer_graph.go` → 抽 `graph_resolve.go`(节点解析) + `graph_loop.go`(Invoke 循环) + `graph_routes.go`(determineNext 路由)
  - `tool_executor.go` → 按工具类别拆分（文件类/命令类/HTTP类）
  - `llm_tools.go` → 抽 `blades_agent_runner.go`(agent.Run 循环) + `blades_tools_build.go`(工具构造)
  - `llm_tracker.go` → 抽 `tracker_stats.go`(Stats/Report) + `tracker_records.go`(RecordCall/Records)
- 本轮实现：三个大文件均已按目标拆分（`graph_core/loop/resolve/routes`、`tool_executor core + files/command/http`、`blades_agent_runner/tools_build/completion`），单文件均 ≤407 行；`executeWithTools` 抽出 `buildAssistantPrompts`/`executeMockAssistant`/`runBladesAgentLoop`/`recordBladesCall` 内部 helper。`go vet`/`gofmt`（新文件）零告警。

**P0-4. blade 路径 token 计量接入**（测试报告 High 级真实问题）✅ 已修复
- 现状：`LLMCallTracker.CallWithTimeout` 仅在 `meta_llm.go`/`domain_llm.go`/`subdomain_llm.go` 的 `callLLMAs` 路径调用；`executeWithTools`（`blades_agent_runner.go`）走 blades `agent.Run` 生成器循环，**已调用 `llmTracker.RecordCall`**，但 `emitDetail(ctx, "token_usage", ...)` 的 message 仅含 `dur`，**不含 `in=/out=`**，导致 `server.parseTokenUsage` 解析为 0，Web 端 Token 消耗面板 Input/Output/Calls 全零。
- 根因：
  - blades `agent.Run` 不暴露 per-call token usage，`BladesClient.GenerateWithUsage` 提取的 `resp.Message.TokenUsage` 也常为 0，原代码未在事件中展示回退后的 `EstimateTokens` 数值。
  - blade 路径 emit 的 `token_usage` 消息格式为 `"blades agent 完成 dur=..."`，与 `meta_llm.go`/`domain_llm.go` 的 `"Token 消耗: in=N out=M dur=X"` 不一致，`parseTokenUsage` 无法提取。
- 修复：
  - `recordBladesCall` 改为返回最终采用的 `input/output tokens`（real 为 0 时回退 `EstimateTokens`，tracker 为 nil 时仍返回估算值）。
  - `executeWithTools` 在成功/错误两条路径 emit `token_usage` 时统一使用 `"[agent] Token 消耗: in=N out=M dur=X"` 格式。
  - `executeMockAssistant` 在 mock 路径同样 emit 带 `in=/out=` 的 `token_usage` 事件（此前 mock 分支直接 return，未触发外层统一 emit）。
  - 新增 `TestExecuteWithToolsMockPathEmitsTokenUsage` 验证事件格式与数值非零；新增 `TestParseTokenUsage` 验证后端解析逻辑兼容 blade 路径消息。
- 本轮加固（防数值为 0）：
  - `recordBladesCall` 增加双重兜底：input 在 `EstimateTokens(prompt)` 为 0 时置 1；output 在 response 为空时用 `err.Error()` 或 `"[no response]"` 估算，仍 0 则置 1。确保任何情况下 token_usage 事件不会出现 `in=0 out=0`。
  - `agent_common.go` 工具循环回退到普通 LLM 的路径：原先只写 tracker 不推事件，现同步 emit `token_usage` 事件，并保证 in/out ≥ 1。
  - 新增单测 `TestExecuteWithToolsMockPathEmptyResponseStillNonZeroTokens` / `TestExecuteWithToolsMockPathErrorStillNonZeroTokens` 覆盖空响应与错误场景的 token 非零。
- 验证：`go test ./...` 通过，`go vet ./...` 通过，`gofmt` 已格式化。

**P0-5. DomainArchive 死代码清理**（P1-1 删除 DomainArchive 召回后的残留）✅ 本轮已完成
- 现状：P1-1 计划删除 DomainArchive 召回机制（`store/domain_archive.go` + `domain_agent.go` 中 `ensureSkillSet` 归档复用路径），删除后需清理残留：
  - `store/domain_archive.go` 整文件删除（若 P1-1 已删则跳过）
  - `graph/util.go` 中 `SaveDomainArchive` / `SearchDomainArchive` / `DomainArchiveRecord` 接口定义删除
  - `domain_agent.go` 中 `archiveStore` 字段及相关注入删除
  - `enums.KnowledgeTypeDomainArchive` 常量保留（历史数据兼容）或删除（激进）
- 验证：`go vet`/`staticcheck` 扫描无未使用符号
- 本轮实现：`graph/domain_archive.go` 重命名为 `domain_skills.go`（现内容为 `ensureSkillSet`/`summarizeResults`/`collectBlockFacts`，与归档无关）；更新 `enums.go`/`memory.go` 注释；`KnowledgeTypeDomainArchive` 常量保留作历史数据兼容。`go vet` 无未使用符号。

### P2 — 体验与验证

**P2-1. TUI 按最新文档重写** ✅ 本轮已完成（定向修复，非重写）
- 文档：`doc/TUI设计文档.md` v2.0，参考 Claude Code。
- 核心要素：主对话占 80% 高度、底部状态栏（plan 进度+工具状态）、Tab 切换右侧 Agent 列表面板、记忆召回指示 `🧠 recalled: ...`、话题切换提示。
- 现状偏离：commit `83f3b1a`/`6f0d90b`/`2cb102b` 多次返工未对齐文档。
- 方案：先按文档重写布局，再迁现有 HTTP 驱动逻辑，不在旧实现上打补丁。
- 同步修复：TUI alt-screen 与自动化工具/overlay 焦点冲突（测试报告 Low 级真实问题）
  - 加 `--no-alt-screen` CLI flag，bubbletea `WithoutAltScreen` 选项
  - 检测 CI/自动化环境（`CI` env / 非 TTY）自动禁用 alt-screen
  - Computer Use 测试场景可用此 flag
- 本轮实现：经核对现有 TUI 已是 v2.0 对齐布局（顶栏/plan栏/Tab agent 面板/`🧠 recalled`/话题分隔线/alt-screen CI 修复均已在），无需重写。补齐验收清单缺口：命令模式新增 `/help`/`/agents`/`/status`/`/clear`/`/topic <name>`/`/topics`/`/memory <query>`（后端补 `/topic` 路由）；plan 栏无 plan 时展示 `[tool] <name>: <path> [●]` 工具状态；内联 `🧠 recalled` 限 2 条；Agent 面板展开时主对话 60%/面板 40%。同步修复 bubbletea v1.3.10 编译错误（`msg.Shift`→`msg.Alt`，`renderAgentPanel(&m)`），文档 Shift+Enter→Alt+Enter、Ctrl+L=完整记录面板。

**P2-2. 集成测试模块整理** ✅ 本轮已完成
- 现状：集成测试零散，编程主场景（写贪吃蛇/重构 CSS/修 bug 端到端）缺失。
- 目标结构：
  ```
  test/
  ├── coding/           # 编程主场景（写贪吃蛇/重构 CSS/修 bug 端到端）
  ├── api/              # HTTP API 全覆盖
  ├── tui/              # TUI 模拟按键流
  └── fixtures/         # 共享 fixture
  ```
- 完成开发后需有详细测试模块：模拟开发处接口的各种情况进行模型浏览器操作测试、API 调用测试等。
- 本轮实现：`test/api/*` 全部为可运行 HTTP 断言（含本轮新增 `session_error_test.go` 覆盖 404/空 body/不存在会话错误路径）；`test/coding/*` 三个场景（贪吃蛇/CSS 重构/bug 修复）用 mock LLM 驱动会话走通完整 graph+memory 栈，断言会话到达终态 + goal 到达 LLM（共享 `helpers_test.go`）；mock LLM 增强 `RegisterSequence`（确定性多轮工具调用）+ `RequestPrompts`（prompt 内容断言）；修复 `tui_keystream_test` 的 `Graph.Registry()`/`DAGHandler`/`Update` 类型断言（新增 `Registry()` accessor 与 `Deps.DAGHandler`）。`go vet -tags integration ./...` 零告警，三套件均实测通过。

**P0-0. 恢复 MetaAgent 直接调用工具能力并升级智能路由**

- **背景**：P0-1 记忆层优化中临时禁用了 MetaAgent 直接调用工具的能力，所有"做事"需求通过助手/领域 Agent 完成。当前 `meta_handle.go` 把 `RouteDirectTool` 与 `RouteDirectAssistant` 统一收敛到 `executeDirectAssistant`，失去了 0 层直接工具执行的低延迟路径，也与原 5 路径设计（`修改文档.md` §4.2/4.3）不一致。
- **目标**：
  1. 恢复 MetaAgent 直接调用工具的能力（RouteDirectTool，0 层，不创建任何 Agent 节点）。
  2. 保留 MetaAgent 直接调用助手的能力（RouteDirectAssistant，1 层）。
  3. 升级智能路由：规则层未命中时，使用轻量模型（`LightweightModel`）组装提示词与用户问题，先判断"简单问题 / 复杂问题"，再根据复杂度输出对应路由路径；简单问题由 MetaAgent 直接处理，复杂问题派发子 Agent。
- **实现要点**：
  - `backend/internal/graph/router.go`：
    - `classifyRouteLLM` 改为调用 `modelFactory.CallLightweightWithRetry`，不再走 `callLLMAs`（Meta 模型）。
    - Prompt 要求模型先做二分类（`simple` / `complex`），再映射到 5 条路径之一；输出格式严格为 `complexity: simple|complex\npath: <path>`，便于解析。
    - 保留非法路径回退到 `RouteCreateDomain` 的安全兜底。
  - `backend/internal/graph/meta_handle.go`：
    - `handleInitialClassify` 的 switch 区分 `RouteDirectTool` 与 `RouteDirectAssistant`。
    - `RouteDirectTool` 调用新增的 `executeDirectTool`；`RouteDirectAssistant` 保持调用 `executeDirectAssistant`。
    - `handleInitial` 判断简单问题时优先取最近一条 `ChatRoleUser` 消息，避免 `resumeSession` 将历史总结为新 goal 后，原简单问句（如"你是什么模型"）被误判为复杂任务。
  - `backend/internal/graph/meta_utils.go`：
    - 新增 `executeDirectTool`：通过 `registry.GetMetaRoleDef()` 获取 MetaAgent 自身角色定义，直接走 `CommonExecuteAssistantTask`（使用 MetaAgent 自身模型跑 blades 工具循环，不创建 Assistant 实例）。
    - 若未找到 meta 角色定义或无模型，回退到 `executeDirectAssistant`。
    - 删除 `metaDirectRoleDef` 与自跑工具逻辑的历史残留注释（P0-1 已删）。
  - `backend/internal/graph/role_registry.go`：
    - 新增 `GetMetaRoleDef()`：根据 `cfg.MetaAgent.SystemPrompt` 构造 MetaAgent 的 `RoleDefinition`（roles.yaml 中 `meta_agent` 不属于 `fixed_roles`，原本 registry 中无此定义）。
  - `backend/internal/tui/view.go` / `model.go`：
    - `renderChat` 增加空会话占位提示 `renderEmptyChat`，会话已创建但暂无消息时显示引导信息，避免首屏空白。
    - `selectSession` 重建内容后：内容未撑满视口则 `GotoTop()`，确保首条用户消息可见；仅当内容超出视口才 `GotoBottom()`。
  - `backend/internal/graph/router_test.go`：补充测试：
    - 轻量模型路由路径解析（mock 返回 `complex/simple` + path）。
    - `RouteDirectTool` 直接执行不创建 DomainAgent/Assistant 实例。
  - 验证：`go test ./...`、`go vet ./...`、`go build ./...` 通过。

**P0-1. 记忆层优化** ✅ 本轮已完成

- **目标**：取消 MetaAgent 直接调用工具的能力，所有"做事"需求通过助手/领域 Agent 完成；MetaAgent 只维护轻量级调度记忆，不记录繁琐上下文，同时保证记忆完整性不会导致调度失忆。

- **实现要点**：
  - 新增 `backend/pkg/types/agent_result.go`：`AgentResult`（SummaryForUser / MemoryForMeta / Facts / ToolResults / Error）与 `MetaMemoryEntry`（Timestamp / Source / Content / Tags）。
  - 扩展 `pkg/types/role.go`：`SessionBlock.MetaMemory []MetaMemoryEntry`、`SessionBlock.Result`、`ThreeLayerState.MetaMemory`；移除已弃用的 `CallResponse`。

- **Agent 返回与记忆组合**：
  - `graph/agent_common.go`：`CommonExecuteAssistantTask` 返回 `(*AgentResult, error)`。
  - `graph/assistant.go`：`executeTask` / `Invoke` 写入 `AgentResult` 与块级 `MetaMemory`。
  - `graph/domain_agent.go`、`domain_assistant.go`、`domain_utils.go`、`domain_skills.go`：领域执行链路返回 `AgentResult`。
  - `graph/subdomain_agent.go`、`subdomain_tasks.go`、`subdomain_utils.go`：子领域执行链路返回 `AgentResult`。
  - `graph/meta_archive.go`：`collectBlockResult` 把 `AgentResult` 聚合进 `state.SessionSummary` 与 `state.MetaMemory`。

- **MetaAgent 移除直接工具路径**：
  - `graph/meta_handle.go`：`RouteDirectTool` / `RouteDirectAssistant` 统一走 `executeDirectAssistant`（创建 Assistant 执行）。
  - `graph/meta_utils.go`：删除 `metaDirectRoleDef` 与自跑工具逻辑；`executeDirect` 改走 `CommonExecuteAssistantTask`。
  - `graph/meta_utils.go`：`loadHistorySection` 注入历史 `MetaMemory`。

- **持久化**：
  - `migrations/006_session_history_meta_memory.sql` 新增 `meta_memory` JSONB 列。
  - `backend/internal/store/postgres.go`：`SessionHistoryRecord` 读写 `MetaMemory`。
  - `backend/internal/server/session.go`：`persistHistory` / 恢复路径适配。
  - `backend/internal/testserver/testserver.go` 与 `backend/cmd/tui/main.go` 历史适配器同步更新。
- **测试**：`backend/internal/graph/agent_common_test.go`、`llm_tools_test.go` 已更新；`go test ./...`、`go vet ./...`、`go build ./...` 通过。

**P2-3. TUI 页面优化：工具输出默认折叠** ✅ 已完成
- 目标：减少 TUI 主对话区中低价值工具输出的视觉噪声。
- 默认隐藏 output 的工具（5 个）：`ReadFile`、`SearchInFiles`、`ListDir`、`HTTPGet`、`HTTPPost`。
  - 这些工具的 output 只显示工具名和路径（`[✓] ReadFile: path/to/file`），不展开 output 内容。
  - LLM 已在调用时拿到完整结果，UI 无需重复展示全文。
- 保留 output 展示的工具（2 个）：`WriteFile`、`RunCommand`。
  - `WriteFile` 简短确认有价值；`RunCommand` 的 stdout/stderr 是用户最关心的执行结果。
- 错误处理：所有工具的 `ToolError` 仍默认展示，避免隐藏失败信息。
- 实现位置：`backend/internal/tui/helpers.go` 的 `eventChatItem`。
- 实现方式：新增 `verboseTools` 集合；`tool_exec` / `tool_call` 分支中，仅 verbose 工具把 `ToolOutput` 拼入 detail。
- 验证：新增 `backend/internal/tui/helpers_test.go`：`TestEventChatItemToolOutputCollapsed` 覆盖隐藏/保留两类工具与错误展示；`go test ./internal/tui/` 通过。
**P3-2. 编程工具扩展 + 自定义工具 + Skill 装配**（原待完成 #2 扩展）✅ 本轮已完成（Git 工具）
- 现状：`tool_executor.go` 原有 7 个基础工具。
- 本轮实现：
  - 新增 Git 工具：`GitDiff` / `GitStatus` / `GitLog` / `GitBlame`，实现于 `backend/internal/graph/tool_git.go`。
  - 工具在 `tool_executor.go` 中分发，`blades_tools.go` 中注册到 blades function-calling，`config/skills.yaml` 中补充 skill 定义。
  - `buildBladesTools` 由 7 个工具扩展为 11 个。
- 仍待后续：测试运行器、浏览器自动化（chromedp）、自定义工具 YAML 注册、MCP 协议集成、Skill 与工具绑定机制。

**P3-3. 真实 Embedding 接入 + Embed 配置迁移到 roles.yaml** ✅ 本轮已完成
- 现状：`embed.PseudoEmbed` 字符哈希伪向量，cosine 不可靠；此前 embed 配置位于 `config.yaml`。
- 本轮实现：
  - `internal/embed/` 定义统一 `Embedder` 接口与 `NewEmbedder(roleCfg, dim)` 工厂。
  - 保留 `PseudoEmbedder` 作为默认零依赖实现（`provider=pseudo`）。
  - 新增 `OpenAIEmbedder`：调用 OpenAI 兼容 `/v1/embeddings`，支持 `provider=openai` 与 `provider=local`（ollama/xinference 等）。
  - **Embed 配置从 `config.yaml` 迁移到 `config/roles.yaml`**：`EmbedConfig` 类型下沉到 `backend/pkg/types/embed.go`，`RoleConfigFile` 负责 `embed` 段默认值与环境变量解析；`internal/config.Config` 移除 `Embed` 字段。
  - `PostgresStore` 增加 `SetEmbedder` / `Embed`，`memory.BlockMemorySearcher` 接口增加 `Embed`，`SearchBlockMemory` 统一走接口向量化。
  - `testserver.BuildHandler` 创建 embedder 并注入 pgStore 与 global KB adapter；`Deps` 暴露 `Embedder`。
  - 新增 `backend/internal/embed/embed_test.go` 覆盖 pseudo / openai 基础路径。
- 注意：伪向量下"召回质量"评测无意义，P3-1 记忆层简化评测**必须等真实 embedding 接入并跑通后**做。

**P3-4. 各级别 Agent 模型单独配置**（原待完成 #1）✅ 本轮已完成
- 现状：`config/roles.yaml` 已支持 MetaAgent / DomainAgent / LightweightModel / 每个 FixedRole 各自配置 `ModelConfig`，`ModelFactory` 按角色缓存模型实例。
- 本轮实现：
  - `dynamic_templates`（`domain_template` / `assistant_template`）新增独立 `model_config` 支持（`backend/pkg/config/role_config.go` + `backend/internal/graph/role_factory.go` + `backend/internal/model/factory.go`）。
  - 动态角色生成后向 `ModelFactory` 注册动态配置；`resolveConfig` 按 `动态角色 → fixed_roles → domain_agent` 优先级回退。
  - 若 LLM 未在生成的角色定义中返回模型配置，自动回退到对应模板配置；模板未配置则回退到 `domain_agent.model_config`。
- 仍待后续：
  - 前端 Web UI 暴露各角色模型配置页（当前只能改 yaml）。
  - 任务拆分（`analyzeTasks`）与代码生成（`CommonExecuteAssistantTask`）可进一步细分轻量/重量映射并统计 token 成本下降比例。

**P3-5. MCP / Skill / Computer Use / RAG / LLM Wiki 插件预留** ✅ 本轮已完成
- 预留点：
  - `plugins.ToolRegistry` / `plugins.Tool` 抽象，默认内存实现 `StaticToolRegistry`（`backend/internal/plugins/registry.go`）。
  - `plugins.KnowledgeSource` / `plugins.Chunk` 抽象（`backend/internal/plugins/knowledge.go`）。
  - `internal/computeruse/` 包占位（`doc.go`），说明后续能力范围与安全边界。
  - `config.yaml` 新增 `plugins.mcp.enabled` / `plugins.rag.enabled` / `plugins.computer_use.enabled`，默认关闭。
  - `config.go` 新增 `PluginsConfig` / `PluginToggle` 结构体。
- 关键：接口先定，实现后做，避免提前实现绑定死。

**P3-6. 原生多协议模型接入（OpenAI / Anthropic / Ollama）** ✅ 本轮已完成
- 现状：`blades_client.go` 仅显式支持 `openai` provider，anthropic/ollama 被转成 OpenAI 兼容端点，易因 base_url 拼接错误导致 401/404。
- 本轮实现：
  - `backend/internal/model/provider_openai.go`：保留 `blades/contrib/openai` 原生封装。
  - `backend/internal/model/provider_anthropic.go`：实现 `blades.ModelProvider` 接口，使用 `github.com/anthropics/anthropic-sdk-go` 原生 Messages API，独立处理 system prompt、tool-calling、usage 与流式事件。
  - `backend/internal/model/provider_ollama.go`：实现 `blades.ModelProvider` 接口，使用 `github.com/ollama/ollama/api` 原生 Chat API，默认 base_url `http://localhost:11434`，支持本地模型。
  - `backend/internal/model/blades_client.go`：`createBladesProvider` 按 `model_config.provider` 分发到对应原生实现；OpenAI 兼容后端（DeepSeek / 豆包 / 硅基流动等）统一配 `provider: openai`。
  - `config/roles.yaml` 顶部 provider 说明更新为三种原生协议。
  - `.env.example` 拆分 `OPENAI_BASE_URL`、`ANTHROPIC_BASE_URL`、`OLLAMA_BASE_URL`，避免 key 与端点错配。
- 验证：`go vet ./internal/model/...`、`go build ./internal/model/...`、`go test ./internal/model/...` 通过；关键包编译与测试通过。


**P3-7. 模型分层结果可观测性**（原待完成 #4）✅ 本轮已完成
- 实现：
  - `model.LLMCallTracker` 新增按模型层聚合的 `LayerStats`（meta/domain/lightweight/assistant/other），在 `RecordCall` 中实时累加。
  - 新增 `LLMCallTracker.LayerStatsSnapshot()` 返回分层统计快照。
  - `model.CallerToLayer(caller)` 导出，供 server 层复用。
  - `server.HandleSessionTokenMetrics` 在原有按 agent/model 聚合基础上，额外返回 `layer_stats`。
- 验证：`go test ./internal/model/...` / `go test ./internal/server/...` 通过。


---

## 待完成（按优先级分级）

### P3 — 远期优化（依赖前置项）

**P3-1. 记忆层简化**（依赖 P3-3 真实 embedding 接入后评测）
- 现状：4级压缩 Raw→Standard→Compact→Marker 阈值难调，实际触发效果未评测。
- 方案：先评测再定方案，不评测就简化=拍脑袋。
- 评测指标：①触发频次 ②各级命中率 ③压缩后召回质量（人工评分） ④token 节省量。
- 评测方法：跑 `test/coding/` + `test/api/` 全套，统计每级落库量。
- 评测后再选：可能两级够（Raw 7天 + 摘要永久），也可能保留三级但去 Marker。

**P3-2. 加强测试流程，当前测试流程不足**
- 加强测试流程，助手级Agent完成后，对自己的子任务需要进行单元测试，领域Agent完成后，需要派遣测试助手对完整模块进行全方面测试。抓跟他完成任务后需要测试完整度、是否完成任务，更徐亚哦测试各个模块间协同时的测试是否有问题，也使用测试助手进行测试。

---

## 优先级依赖关系

```
P0-3 (拆分) ──┬──→ P0-1 (domain过滤)
              ├──→ P0-2 (回调写入)
              ├──→ P1-1 (块记忆键值化)
              └──→ P1-2 (日志)

P1-2 (日志) ──→ P3-4 (模型分层可观测性)

P3-3 (真实embedding) ──→ P3-1 (记忆层简化评测)

P0-1 (domain过滤) + P1-1 (键值化) ──→ P3-1 (记忆层简化)
```

**关键约束**：
- P0-3 拆分是其他改动的前置，应最早做
- P1-2 日志是 P3-1/P3-4 的调试基础
- P3-1 记忆层简化依赖 P3-3 真实 embedding，否则评测无意义
