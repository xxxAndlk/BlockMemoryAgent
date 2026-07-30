## 待完成 / 开放项

1. **ReAct 端到端集成测试**
   - 背景：`test/coding/*` 与 `test/api/*` 仍基于旧 ThreeLayerGraph 状态机 mock，需要重写为 ReAct 事件流 mock。
   - 开放动作：更新 `test/fixtures/mock_llm.go`，为 ReAct 循环提供序列化响应；重写 snake / css-refactor / bug-fix 三个端到端用例。

2. **TUI Agent 树运行时构建**
   - 背景：当前 `ReactService.ListAgents` 只返回单一 MetaAgent 节点，TUI 右侧 Agent 编排栏无法展示子 Agent 调用树。
   - 开放动作：从会话事件流中动态构建 Agent 调用树（主 Agent → call_sub_agent → 子 Agent），替代旧编译期层级。

3. **记忆事件持久化**
   - 背景：`domain/memory.Pipeline` 当前使用 `InMemoryStore`，会话结束后事件丢失。
   - 开放动作：实现 `domain/memory.Store` 的 Postgres 适配器，把 agent 事件流写入新表 `agent_events`（旧 `session_events` 保留只读）。

4. **Watchdog 事件流截断**
   - 背景：新架构无记忆压缩，长会话事件流可能膨胀。
   - 开放动作：在 `watchdog` 超限时触发 `domain/memory.Pipeline` 截断（保留最近 N 条 + 生成总结事件）。

5. **配置清理**
   - 背景：`config.AgentConfig` 仍保留 `GraphPolicyConfig`、`MemoryPolicyConfig`、snapshot/compress 等旧字段。
   - 开放动作：确认无引用后删除失效字段，或标记为 deprecated。

6. **文档漂移清理**
   - 背景：`doc/设计文档_v3.md` 仍有大量 ThreeLayerGraph 描述。
   - 开放动作：标记历史章节，或在适当时机重写。
  
7. **评估项**
   - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试，不明确具体测试方向时需要先把方案列出，给开发处方案的代码Agent是否符合代码Agent的逻辑，不符合代码Agent纠正，符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合，不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent，期间各Agent可以反复询问，纠正，需要Agent在被询问时开一个协程进行回复，需要携带关键记忆或者协程Agent常驻共享代码Agent记忆，可评估。测试流程最为重要，查看现在的测试是否严谨与多重确认。代码Agent完成开发，找到固定助手的测试助手，先进行自测试，返回结果成功后返回给上级Agent，上级Agent按更大范围模块进行统一测试，哪个部分不行打回。
   - Agent执行任务，先拆解任务，拆解为不会干涉的单元任务后，一个单元任务使用一个干净上下文的Agent编码助手执行，压缩Token成本。执行完成后暂时不销毁，一定时间不使用直接消耗，有使用则重置使用时间并加长使用时间。原因：我在使用claude时，经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉，并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆，块记忆过于抽象，可用性与传统感觉不明显，。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式，每个领域干净上下文负责自己的事。

8. **硬 Token 预算（每用户目标上限）**【已规划落地，待编码】
   - 背景：`maxIter=50` 仅防死循环，无累计 token 上限。`react_agent.go:240-246` 每次 LLM 调用后 emit `LiveEventTokenUsage` **仅 emit 不累加**；`service_react.go:846-848` 仅写前端展示，无 cap 检查。`config.go:156` `ContextExplodeHardLimit` 仅限单次输入 token，总消耗 ≤ 50 * (input_cap + output_cap) 仍可能很高。用户目标"优化整个项目"可能烧百万 token 才被 `maxIter` 截断。
   - 开放动作（约 15 行代码）：
     1. `internal/config/config.go` `AgentConfig` 加字段 `TokenBudgetPerGoal int `yaml:"token_budget_per_goal"` // 0 = 不限`。
     2. `config/config.yaml` `agent:` 段加 `token_budget_per_goal: 100000  # 单用户目标累计 token 上限，0 = 不限`。
     3. `internal/agent/react_agent.go` `ReActAgent` 加 `tokenBudget int` 字段，构造时从 config 注入。
     4. `RunWithHistory`（`react_agent.go:182-369`）加 `cumulativeTokens int` 变量，每轮 LLM 调用后累加 `resp.Message.TokenUsage.TotalTokens`。
     5. 累加后检查 `a.tokenBudget > 0 && cumulativeTokens > a.tokenBudget`，超限则 break 循环，返回 `[系统] Token 预算耗尽（已用 {cumulativeTokens} / {budget}）。已完成部分：{lastAnswer}`。
     6. **不**改 `maxIter=50`、**不**改 `pendingChecker`、**不**引入其他硬阈值。
   - 验证：`GOTOOLCHAIN=local go test ./backend/internal/agent/... -count=1`；mock LLM 注入大 `TotalTokens` 验证超限 break。
   - 收益：防"无限烧钱"运维噩梦，生产级部署硬性要求。配合 `LiveEventTokenUsage` 事件前端可实时显示预算条。

   **已完成（阶段 0-3）：**
   - **阶段 0 - 测试基线修复**：`test/api/memory_test.go` 重写为 ReAct 现状断言；`test/role_config_sanity_test.go` 去硬编码模型名；`test/coding/{snake,css,bug_fix}` 三个 e2e 用例接入 `RegisterSequence` 精细化断言（`helpers_test.go` 新增 `toolCall`/`writeFileCall`/`readFileCall`/`toolResultEchoed`/`toolResultSucceeded`）。
   - **阶段 1 - 块记忆闭环**：`dispatcher.saveBlockMemory` 子 Agent 成功完成时沉淀结果摘要；`blockMemorySaver` 适配 `PostgresStore.Embed + SaveKnowledge`；`config.block_memory_write_enabled`（默认 true）控制开关；`roleIDFromAgentID` 修复连字符角色 ID bug；`pipeline.WithMaxEventsPerAgent` + `InMemoryStore.WithMaxEventsPerAgent` 防事件流无限增长；`runSubAgent` defer `mailbox.Purge` 修子 Agent 收件箱泄漏。
   - **阶段 2 - 协作验证闭环**：`mailbox.Message` 增加 `ReplyTo`/`ThreadID` 字段 + `MsgReply` 类型 + `WaitForMessage` 阻塞等待；`send_message` 工具支持任意 Agent 向另一个 Agent 实例邮箱投递消息（请求/通知/回复）；`roles.yaml` `code_assistant <-> test_assistant` 通过 `parents` 白名单平级互调；`config.verification_max_rounds`（默认 5）+ `Dispatcher.WithVerificationMaxRounds` 防循环死锁；`agent.PendingChildrenChecker` 接口 + `Dispatcher.PendingChildren`/`WaitForAnyChild` + `ReActAgent` 终结保护分支（有未决子 Agent 时阻塞等待而非立即终结，防迟到 mailbox 消息丢失）。
   - **阶段 3 - 子 Agent 实例池**：`Dispatcher.WithReuse(enabled, idleTimeout)` + `config.sub_agent_reuse_enabled`（默认 false）+ `config.sub_agent_idle_timeout_sec`（默认 300）；`servePooled` 子 Agent 完成初始任务后转入服务态，阻塞等待 mailbox 询问，收到消息时 `RunWithHistory` 处理并把回复投递给 `ReplyTo`，闲置超时退出；`trimPooledHistory` 防长生命周期历史无限增长；`Dispatcher.Stop` 释放所有池化子 Agent，`bootstrap.Close` 注册为 cleanup 回调。
   - **阶段 4 - verifyloop 原生编排器**：`internal/domain/verifyloop` 包实现"代码->自测->修正->上级统一测试"状态机，原生驱动不依赖主 Agent 提示词；`Dispatcher.ExecuteChild` 同步执行接口 + `runSubAgentOnce` 拆分（纯执行/异步包装分离）+ `OnSubAgentDone` 钩子；`[VERIFY:PASS/FAIL]` 标记确定性解析；`maxRounds` 往返上限 + ctx 取消快速返回；`AssistantSelfTestEnabled` 开启时 bootstrap 自动注入钩子，`code_assistant` 异步完成后 goroutine 触发编排器，`NotifyParent` 投递最终结果到父邮箱（通过发 MsgInfo/未通过发 MsgEscalate）。`ExecuteChild` 不触发钩子，防编排器修正轮递归自测。`config.yaml` 显式 `assistant_self_test_enabled: false`（默认关闭，实测稳定后可开）。
   - **阶段 5 - 编排器抽象层**：编排器引擎与验证方式解耦，支持多场景复用与后续扩展。三接口：`Verifier`（SelfTest/UnifiedTest 返回结构化 `Verdict`，替代裸字符串标记）+ `Fixer`（修正产出）+ `Reporter`（上报结果）。`Orchestrator` 降级为纯引擎，`New` 默认装配 `AgentVerifier`+`AgentFixer`+`MailboxReporter` 兼容现有调用，`NewWith` 支持自定义三件套注入。`verifiers.go` 留扩展验证器口子：`ComputerUseVerifier`（待 computeruse 包接入，截图对比/UI 交互）、`CLIVerifier`（待接入 exec/Executor，跑 go test/build 等）、`MCPVerifier`（待 MCP 客户端接入，调 MCP 服务器测试工具），均实现 Verifier 接口 + 编译期断言 + TODO 标注接入点，当前返回"not implemented"占位。测试覆盖默认路径 + 自定义接口注入（customVerifier/customFixer/customReporter）+ 三类扩展验证器口子存在性。
   - **阶段 6 - 评估产出**：`doc/评估_块记忆vs实例池.md` 对比两机制（持久性/Token成本/召回准确性/跨Agent共享等10维度），结论为不可替代应融合，给出3个融合点建议（实例池退出时沉淀块记忆、服务态期间召回块记忆、召回命中率观测）；`doc/评估_测试严谨性.md` 定义10项量化指标对比修改前后（确认层数0-1->2、修正闭环否->是、结论确定性裸字符串->结构化Verdict等），测试覆盖30个用例（verifyloop 14+subagent 13+coding 3），严谨性评分3/10->7/10，列出5项短板与8条改进建议。
   - **阶段 7 - 实例池移除 + 抽象补全**：评估结论"实例池引入上下文污染与Token激增（原文痛点），收益已被verifyloop覆盖"，移除实例池全链路（`WithReuse`/`servePooled`/`trimPooledHistory`/`pooledAgent`/`pool`/`IsPooled`/`Stop`、`mailbox.WaitForMessage`、`config.sub_agent_reuse_enabled`/`sub_agent_idle_timeout_sec`、bootstrap `WithReuse`调用与cleanup `Stop`、`TestDispatcher_ReusePool_ServiceMode`）；新增 `KVMemory` 键值对记忆（`internal/domain/memory/kv.go`）支持创建时控可写（`NewInMemoryKV(writable, store)`），主线程 Agent 独占写、协程 Agent 只读，锁防 panic（defer unlock + 不持锁调外部代码）；verifyloop `PlanConfirmVerifier` 可选接口 + `AgentVerifier.PlanConfirm` 实现"测试方向不明确->列方案->产出方确认->符合才自测"前置步骤，引擎 `Run` type assert 检查实现，未实现跳过（向后兼容）；`config.VerificationRolePairs` 角色对配置化，bootstrap 按列表注册多个 OnSubAgentDone 钩子，默认 `[{code_assistant, test_assistant}]`，支持扩展 `ui_assistant->test_assistant` 等多角色对。
   - **阶段 8 - 批2 接线与抽象**：`DomainSelfTestEnabled` 接线（bootstrap 合并 Assistant/Domain 开关，DomainSelfTestEnabled 为 true 时追加 `{domain, test_assistant}` 角色对）；KVMemory 接入 dispatcher（`Dispatcher.WithKVMemory` 注入只读视图，`injectKVMemory` 按 `parentID:shared` 键读取主 Agent 写入的关键记忆注入任务前缀，实现"协程 Agent 共享主 Agent 记忆"语义）；领域任务拆解抽象（新建 `internal/domain/assembly` 包：`Splitter`/`Executor`/`Aggregator` 三接口 + `Assembly` 编排器，按 DAG 拓扑序执行小任务，依赖完成后才执行下游，`RunnerExecutor` 默认实现经 `Dispatcher.ExecuteChild` 派发干净上下文子 Agent，对应"拆解为不会干涉的单元任务+干净上下文执行"）；e2e 测试用例 `TestAssistantSelfTest_VerifyLoop`（skip，因最新代码 mock LLM 连通性校验回归，预先存在非本批引入，verifyloop 逻辑由单测覆盖）。
   - **阶段 9 - ReAct 运行时增强与 DomainAgent 任务分配**（commits `bc3b2b2`/`3392595`/`0f20283`/`0969335`，2026-07-25 ~ 07-27）：
     - **历史压缩**（`react_agent.go`）：新增 `summarizeEvery`/`summarizeKeepRecent` 配置，每 N 轮 ReAct 迭代把中段历史压缩为 system 前缀 + 用户任务目标 + 中段摘要 + 最近 K 条消息；`summarizeWindow` 函数落地。`config.yaml` 显式 `summarize_every: 5`、`summarize_keep_recent: 10`、`history_max_messages: 30`（滑动窗口上限）。部分缓解 TODO 第 4 项"长会话事件流膨胀"（在 ReAct 层而非 watchdog 层）。
     - **LLM 调用日志**：`ReactService.sessionLogger` 把每次 LLM 调用 I/O 写入 `session_logs` 表；`QueryKindLogs` 查询类型支持按会话回看完整调用链。`LiveEventTokenUsage` 事件类型实时推送 token 用量到 SSE/TUI。
     - **代理展示名**：`WithAgentDisplayName` + `AgentDisplayNameFromContext`，工具事件日志优先展示角色名而非 session-ID；`agentIDKey` 迁至 `domain/tool/agentctx.go` 统一管理。`call_sub_agent` 工具新增 `domain` 字段，DomainAgent 派生展示名（如"金融领域Agent"）。
     - **共享内存工具**：新增 `WriteSharedMemory` 工具 + `SharedMemoryStore` 接口（`domain/tool/shared_memory.go`），仅 MetaAgent 白名单可用；bootstrap 注入同一 `sharedKV`（`memory.NewInMemoryKV(true, nil)`）到工具注册表与 dispatcher，主 Agent 写入关键上下文，子 Agent 经 `injectKVMemory` 自动读取，避免重读全文件。`workspace/` 目录启动时自动创建。
     - **工具白名单**：`NewToolRegistryAdapterWithFilter` 按角色限制可调工具集；DomainAgent 开放 `ReadFile`/`ListDir`/`SearchInFiles`/`HTTPGet`/`WriteSharedMemory`/`WriteFile`/`RunCommand` 等完整权限，承担上下文采集+任务拆分+派发执行；MetaAgent 移除 `ReadFile` 类工具防越位读全文。`tool_call_id -> ToolCall` 索引修复 OpenAI tool_call 缺 function 字段问题。Windows 路径大小写统一比较。
     - **任务规格约束**：`call_sub_agent` 入参 `task` 长度上限 2000 runes，超限报错并提示"把规格/原文写入 WriteSharedMemory，task 只写目标+验收标准"。任务路由规则推荐复杂任务走 `domain` 角色二次拆分。
     - **ReadFile 重复读限制重构**：已读记录从任务级改为 session 级；`ResetReadHistory` 在新用户消息时清空，确保重复读限制为单任务级而非整个 session 级。移除 `LoopExit` 机制，改为返回错误提示让模型自主调整。
     - **DeepSeek V4 适配**：`provider_deepseek.go` 新增 `inputTokens`/`outputTokens` 方法兼容 `prompt_cache_hit_tokens`/`prompt_cache_miss_tokens`；处理 `reasoning_content` 字段（思考模型）。`blades_client.go` 增加缓存模式 token 调试日志。
     - **受保护目录扩展**：`guards.go` 新增 `web/node_modules`/`dist`/`build`/`bin`/`pkg`/`logs` 等构建产物目录防误写。
     - **配置整合**：移除 `config/agent-policy.yaml` 与 `config/infrastructure.yaml`，全部整合进 `config/config.yaml`。`tool_call_max_rounds: -1`（不限制）、`react_llm_timeout_sec: 600`。
   - **待办**：e2e mock LLM 连通性校验回归（`POST .../v1/chat/completions: 400`，最新代码引入，阻塞所有 coding e2e，需单独排查）；`AssistantSelfTestEnabled`/`DomainSelfTestEnabled` 默认仍关闭（e2e 未实测通过前不开）；`ComputerUseVerifier`/`CLIVerifier`/`MCPVerifier` 仅留口子，待对应基础设施就绪后填入实现；**assembly 包已实现但未接入**——commit `0969335` 改走 DomainAgent 原生工具权限 + `call_sub_agent` domain 字段方案（DomainAgent 在 ReAct 循环内直接做上下文采集+拆分+派发），未走 `assembly.Run` 编排器，assembly 包当前无任何引用，待清理或重新接入；无指标埋点（平均往返轮数/自测通过率/修正成功率），严谨性无法持续监控；watchdog 与 ReAct 历史压缩未联动（item 4 仍部分开放）；`domain/memory.Pipeline` 仍用 `InMemoryStore`（item 3 未动）。

9. **共享记忆一致性（文件读写缓存失效）**【Layer 1-3 已落地 2026-07-28，详见 `doc/计划_共享记忆一致性.md`】
   - 背景：多 Agent 共享 workspace ≈ CPU 多核共享内存。当前 `sharedKV` 存自由文本摘要，子 Agent 改文件后 KV 仍持有旧摘要 -> 父 Agent 下次派发把旧摘要注入新子 Agent task -> 幻觉。本质是缓存一致性问题，不是要不要缓存而是失效策略。
   - 选定方案：Scheme 2（File Blackboard）为主 + Scheme 3 lite（版本戳 + 写后失效）。不做完整 MESI 状态机，不做 CRDT。
   - **已完成 Layer 1-3（2026-07-28）**：
     - **Layer 1**: `WriteSharedMemory` 入参结构化 ── `writeSharedMemoryInput` 加 `Files []string` 字段；KV value 改 `SharedEntry` JSON `{files: map[path]mtime, content: string}`；`statFile` 规范化绝对路径并记录 mtime；files 缺省时退化为空 Files map 兼容。代码：`domain/tool/shared_memory.go`。
     - **Layer 2**: `WriteFile` hook 失效 KV ── `SharedMemoryStore` 接口扩 `Get`/`Delete`/`Keys`；`Registry.invalidateSharedMemoryForPath` 在 `Dispatch` WriteFile 成功路径调用，遍历 KV 解析 JSON，引用同 path 的 entry 删除；旧格式 value 保留不删由 Layer 3 兜底。`InMemoryKV.Keys` 新增。代码：`domain/tool/registry.go`、`domain/memory/kv.go`。
     - **Layer 3**: `Dispatcher.injectKVMemory` 版本戳校验 ── 解析 KV value 为 SharedEntry JSON（旧格式直接用），stat 各 path 对比 mtime，任一不匹配丢弃 KV 降级 fresh read；Files 为空跳过 stat 校验。`sharedEntryMirror` 本地镜像避免 subagent 反向 import tool 包。代码：`domain/subagent/dispatcher.go`。
   - **测试**：`registry_test.go` 加 `TestWriteSharedMemory_StructuredAndFileTracking`/`TestWriteFile_InvalidatesSharedMemory`/`TestWriteFile_DoesNotInvalidateUnrelatedEntry`；`dispatcher_test.go` 加 `TestInjectKVMemory_Layer3StaleDetection`/`TestInjectKVMemory_NoFilesSkipsStatCheck`。全部通过。
   - **待办（P1/P2，未做）**：
     - **Layer 4 (P1, 独立立项)** 角色写目录分区：`roles.yaml` 给每个角色配 `sandbox.allowed_write_paths`，从源头消灭写冲突。需配套改 e2e fixture。工程量 2-3 天。
     - **Layer 5 (P2)** `mailbox.Message` 增 `FilesModified []string` 字段，子 Agent 完成时从 `result.History` 提取 WriteFile 调用填入，父 Agent Drain 时定向失效 KV。工程量 1 天。
   - 涉及代码：`domain/memory/kv.go`、`domain/tool/shared_memory.go`、`domain/tool/registry.go`（WriteFile hook）、`domain/subagent/dispatcher.go:757`（injectKVMemory）、`mailbox.Message`。
   - 不要做的事：(1) 不做完整 MESI 状态机；(2) 不 KV 存文件全文；(3) 不依赖 LLM 自觉失效（提示词"文件改了请重读"不可靠）；(4) 不 CRDT 代码文件。
   - 回退策略：每层独立开关 `config.shared_memory_invalidation_enabled`（默认 true，当前未加配置开关，行为默认开启；如需关闭可后续加）。Layer 1 入参改 schema 已向后兼容（`files` 可选，缺省退化旧逻辑）。
   - 验证：`GOTOOLCHAIN=local go test ./backend/internal/domain/memory/... ./backend/internal/domain/tool/... ./backend/internal/domain/subagent/... -count=1` 全通过。

10. **已删除的投机性泛化代码（待真正需要时重写）**
   - 背景：阶段 0-9 累积多处"建了拆、拆了建"的投机性抽象，零引用占位与重叠机制叠加，导致 LLM 派发链路长到不可达、基础任务跑不通。本次清理把已删项的设计思路留档，避免未来重蹈覆辙。
   - **assembly 包**（`internal/domain/assembly/`，已删）：`Splitter`/`Executor`/`Aggregator` 三接口 + `Assembly` 编排器，按 DAG 拓扑序执行小任务。删除理由：阶段 8 实现后零引用，commit `0969335` 改走 DomainAgent 原生工具权限 + `call_sub_agent` domain 字段方案（DomainAgent 在 ReAct 循环内直接做上下文采集+拆分+派发），未走 `assembly.Run`。重写前提：DomainAgent 方案被证明不足以覆盖跨多 Domain 的并行编排需求。
   - **verifiers.go 扩展占位**（`internal/domain/verifyloop/verifiers.go` 中 `ComputerUseVerifier`/`CLIVerifier`/`MCPVerifier`，已删）：三占位返回 "not implemented"。删除理由：未实现的接口 = 没需求的抽象。重写前提：`computeruse` 包 / `RunCommand` 工具 / MCP 客户端对应基础设施就绪后，按 `Verifier` 接口契约填入实现（`SelfTest(ctx, produced) -> Verdict` / `UnifiedTest(ctx, produced) -> Verdict`）。
   - **computeruse 包**（`internal/computeruse/doc.go`，已删）：仅 doc.go 占位。重写前提：浏览器/GUI 自动化需求明确后，按 `Verifier` 接口契约实现。
   - **实例池**（阶段 7 已删并归档）：`WithReuse`/`servePooled`/`pool`/`IsPooled`/`Stop`/`mailbox.WaitForMessage`/`config.sub_agent_reuse_enabled`/`sub_agent_idle_timeout_sec`。评估结论"实例池引入上下文污染与 Token 激增（原文痛点），收益已被 verifyloop 覆盖"，不可与 verifyloop 共存。重写前提：明确"协程 Agent 常驻共享代码 Agent 记忆"需求且 verifyloop 无法覆盖。
   - **KV 缓存三套抽象合并为一套**（已合并）：原 `KVMemory`（`domain/memory/kv.go`）+ `SharedMemoryStore`（`domain/tool/shared_memory.go`）+ `Spec`（`domain/tool/spec.go`）三套并存，功能重叠。合并为单一 `SharedMemoryStore` 接口，`Spec` 退化为命名 key `<agentID>:spec`。dispatcher 三路 inject（`injectSpec`/`injectKVMemory`/`injectRecalledMemory`）合并为单路 `buildTaskPrefix`。重写前提：无；合并后抽象足够。
   - **强制门默认关闭**（已改默认值）：`SpecEnforcementEnabled` 原 default true 改 false；`ReviewEnabled` 原 default true 改 false；`PlanSkipEnabled` 原 default false 改 true（PlanConfirm 阶段整体移除）。理由：默认开启的强制门叠加使 LLM 完成基础任务链路长到不可达（WriteSpec -> call_sub_agent -> 子自测 -> reviewer 审查 -> 上级 unified test -> 终答，任一环节失误即任务失败），且 e2e mock LLM 400 回归阻塞验证。Pipeline 重构后 `SpecEnforcementEnabled` 与 `PlanSkipEnabled` 配置项整体删除（PlanStage 替代 WriteSpec 工具）。
   - **配置死字段**（已删）：`GraphPolicyConfig`（graph 包已删）/`MemoryPolicyConfig`（assembler 已删）/`LLMRuntimeConfig.RetryCount`/`RetryBackoffMs`/`LLMSoftTimeoutSec`/`LLMHardTimeoutSec`（旧 graph 重试，ReAct 用 `ReactLLMTimeoutSec`）/`FeatureTogglesConfig.PlanEnabled`/`ReflectionEnabled`/`InterruptEnabled`/`QueueInjectEnabled`/`HumanClarify*`（零引用）/`PluginsConfig`（MCP/RAG/ComputerUse 预留未实现）。
   - 涉及代码：见 git 历史 `feat(cleanup): 删除投机性泛化与重叠抽象` 系列 commit。

## 已完成（已归档到 git 历史）

- ReAct 主循环骨架（`internal/agent/react_agent.go`）
- 工具域（`internal/domain/tool/`）注册表 + 11 个内置工具
- 角色域（`internal/domain/role/`）加载与权限
- 记忆域（`internal/domain/memory/`）两段事件流
- 子 Agent 域（`internal/domain/subagent/`）异步分发 + Mailbox 集成
- `bootstrap.Build` 切换到 `agent.NewReactService`
- 删除 `internal/graph/` 全部代码
- 删除 `internal/memory/` 中 compress/search/assembler/snapshot/callback/block_vector/workspace_adapter
- 删除旧 `agent.Service` / `agent.sessionStore` 实现
- HTTP/TUI 入口适配新 `agent.Agent` 接口
- 包依赖规则文档更新
