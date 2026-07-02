## 已完成
1.添加基于DAG（有向无环图）的定时任务逻辑流程
2.把每个Agent的上下文窗口不写死，根据配置变化、每个子Agent的skill库大小、工具调用轮数等写死的数据都修改为配置动态参数
3.在domainAgent执行后，块记忆存储后，增加向量检索，在需要检索到这些上下文时，从向量库中检索出最相似的块，并返回给domainAgent。
4.domainAgent在执行任务结束后不直接删除，把domainAgent的信息，上下文skill库等信息以轻量级（如JSON、向量）保存起来，并且能够索引到。在跨会话或者相隔很久的会话中，如果主Agent分配的任务与之前的domainAgent领域重合，如第一个会话是开发商城页面任务，指派给商城domainAgent，在很久以后商城有一个优化，会检索以往Agent列表，找到商城Agent重新给他指派任务。Agent保存需要遵循权重与定期清理的清理方式。在配置中设置一个默认的定期清理时间，超过这个时间，Agent信息会自动删除，但是如果在某个时间段内，有新的任务被分配给这个Agent，那这个Agent权重增加，在执行完成后，自动清理时长增加。
5.添加“人机对话”处理，agent遇到需要确定的问题，以提问的方式给到用户，用户回答添加到上下文中，agent根据用户回答，进行下一步处理。
6.添加抢占中断与队列注入处理。抢占中断：一个任务执行中，用户点击暂停、终止任务并重新输入指令时，上一轮的输入上下文全部清除，重新根据当前指令进行上下文注入。队列注入：一个任务执行中，用户添加指令时，使用队列注入当前指令进入上下文，再继续任务。
7.添加用户输入信息的智能路由，智能判断是否简单问题，简单问题由主agent直接回答，复杂问题派发子agent处理，子agent处理完成后，返回给主agent。
8.**5路径智能路由与编排瘦身**：新增 `router.go` 实现 5 路径精确路由（direct_tool / direct_assistant / create_domain / multi_domain / full_four_layer），规则层（零 LLM）+ LLM 兜底 + 安全兜底（create_domain，零回归）。MetaAgent 重写 `handleInitial` 按路由分派：纯 QA / 单工具请求（读文件、跑命令、查天气）走 0 层 `RouteDirectTool`，MetaAgent 自跑 blades 工具循环；单领域简单任务（修 CSS、改文案）走 1 层 `RouteDirectAssistant`，MetaAgent 直接建助手执行——**80% 任务不再进入四层编排**。复杂任务才拆领域，仅 `RouteFullFourLayer` 启用 SubDomain。
9.**MetaAgent/DomainAgent 可直接执行工具与调用助手**：提取公共执行入口 `agent_common.go`（CommonExecuteAssistantTask / CommonCreateAssistantForTask / CommonMatchFixedAssistant / CommonCollectTaskSummaries），消除 DomainAgent 与 SubDomainAgent 的逐行重复代码。MetaAgent 新增 toolCallback + `executeDirect`，能直接跑工具循环与创建助手；DomainAgent/SubDomainAgent/AssistantNode 统一走公共入口。**修复 AssistantNode 无工具执行的潜在 bug**（SubDomain→Assistant 路径原本只做单次 LLM 调用，现已具备完整 ReAct 工具循环）。
10.**SubDomain 自适应启用**：`shouldSplitToSubDomains` 从恒 `return false` 改为读 `state.EnableSubdomain` + 跨子领域边界检测（`detectSubdomainBoundaries` 规则 + 可选 LLM 兜底），仅复杂跨层任务才进入第四层，避免单领域任务不必要的 overhead。
11.**Plan-and-Execute + Self-Reflection**（原待完成 #1）：新增 `plan.go`，DomainAgent 在多任务时调重量模型生成结构化 `ExecutionPlan`（步骤列表），按步骤顺序派发助手并支持断点续行（`PendingGoals`/`MarkDone`）。`CommonExecuteAssistantTask` 内置 `reflectOnResult`（轻量模型评估结果），不达标时带反馈重试一次（仅一次防死循环）。Feature flag：`agent.plan_enabled` / `agent.reflection_enabled`，默认关闭，按需开启。
12.**前端会话复用修复**（原待完成 #3）：修正 `web/src/views/chat/index.vue` `handleSubmit`——追加消息的判定从仅 `status==='running'` 扩展到 `running/completed/error`（非 `awaiting_clarify` 即追加），后端 `POST /api/sessions/{id}/message` 已支持向已完成会话追加并 `resumeSession`；已完成会话续话时重新建立 SSE 流。新增 `localStorage.lastSessionID`，刷新页面后无 URL id 时优先恢复上次会话。**每条消息不再新开会话栏**。
13.**块记忆持久化可靠性**（原待完成 #4）：(a) 异步归档失败日志从 `fmt.Printf` 改为结构化 `log.Printf`（含 sessionID/domain/error）；(b) `SessionBlock.archived`（atomic.Bool）标记异步归档是否成功，`switchToNextBlock` 在块切换前做**幂等兜底归档**（未确认成功则同步补写，短超时，失败仅日志不阻塞）；(c) 启动期 `ValidateEmbeddingDimension` 校验 `global_knowledge.embedding` 列维度与配置一致，不一致 `log.Fatalf`（避免维度不匹配导致 SaveKnowledge 静默失败、零落库）。

## 待完成
1.各级别Agent配置页的模型单独设置，节省成本
    当前现状：`config/roles.yaml` 已支持 MetaAgent / DomainAgent / LightweightModel / 每个 FixedRole 各自配置 `ModelConfig`（provider/model/api_key/temperature），`ModelFactory` 按角色缓存模型实例。`internal/model/factory.go` 提供 `GetMetaModel`/`GetDomainModel`/`GetLightweightModel`/`GetModel(roleDefID)`。
    待完善：
    - 前端 Web UI 暴露各角色模型配置页（当前只能改 yaml）。
    - 路由判定 / 块记忆摘要 / 反思 已用轻量模型；任务拆分（`analyzeTasks`）与代码生成（`CommonExecuteAssistantTask`）仍用角色配置的模型，可进一步细分轻量/重量映射并统计 token 成本下降比例。
    - 模型分层结果的可观测性：日志/面板展示轻量 vs 重量调用分布与 token 消耗。

2.工具集扩展（编程助手定位）
    当前现状：`tool_executor.go` 仅有 7 个基础工具（ReadFile/WriteFile/ListDir/RunCommand/SearchInFiles/HTTPGet/HTTPPost）。
    待完善：
    - 新增 Git 工具（diff/blame/log）、测试运行器（多框架检测 Go/Python/Node）、浏览器自动化（chromedp 截图/导航）。
    - MCP 协议作为可选工具扩展（默认关闭，feature flag 控制），本地核心工具保持原生实现。
    - 新工具注册到 `blades_tools.go` 的 `buildBladesTools` 与 `llm_tools.go` 的 `defaultTools` 描述。

3.块记忆检索领域过滤
    当前现状：`SearchBlockMemory(ctx, query, topK)` 底层 `SearchKnowledgeByType` 仅按 `knowledge_type='block_memory'` 过滤，**无 domain 过滤**，存在跨领域串扰。
    待完善：
    - `SearchBlockMemory` 增加 `domain` 参数，先领域过滤再语义匹配（参照已实现的 `SearchDomainArchive(ctx, domain, goal, topK)`）。
    - 返回结构化摘要（含关键事实、涉及文件），控制总长度不超过 TokenBudget 的 20%。
    - 检索时间 < 200ms（10 领域 × 10 记录测试集）。

4.代码工程化清理
    - 删除 `pkg/types/types.go` 中 4 个 enums 别名（EventType/EventStatus/ActionType/CompressionLevel），全局替换为 `enums.X`（实际引用仅 postgres.go 4 处）。
    - 清理 `assistant.go` 中已被 `CommonExecuteAssistantTask` 取代的 `callLLM`/`formatContextPack` 死代码。
    - 清理 `meta_agent.go` 中已被 `classifyRouteLLM` 取代的 `classifyComplexityLLM` 死代码。
    - 单函数不超过 80 行，`go vet`/`gofmt` 零告警（注：仓库现存大量 gofmt 历史格式，建议单独一次格式化提交）。
