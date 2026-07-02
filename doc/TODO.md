## 已完成
1.添加基于DAG（有向无环图）的定时任务逻辑流程
2.把每个Agent的上下文窗口不写死，根据配置变化、每个子Agent的skill库大小、工具调用轮数等写死的数据都修改为配置动态参数
3.在domainAgent执行后，块记忆存储后，增加向量检索，在需要检索到这些上下文时，从向量库中检索出最相似的块，并返回给domainAgent。
4.domainAgent在执行任务结束后不直接删除，把domainAgent的信息，上下文skill库等信息以轻量级（如JSON、向量）保存起来，并且能够索引到。在跨会话或者相隔很久的会话中，如果主Agent分配的任务与之前的domainAgent领域重合，如第一个会话是开发商城页面任务，指派给商城domainAgent，在很久以后商城有一个优化，会检索以往Agent列表，找到商城Agent重新给他指派任务。Agent保存需要遵循权重与定期清理的清理方式。在配置中设置一个默认的定期清理时间，超过这个时间，Agent信息会自动删除，但是如果在某个时间段内，有新的任务被分配给这个Agent，那这个Agent权重增加，在执行完成后，自动清理时长增加。
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
- 方案（写队列 + 重试）：
  - `OnEnd` 不起 goroutine，推到内存队列（chan）
  - 后台 worker 消费，失败重试 3 次（指数退避）
  - 重试失败入死信表 `memory_write_failures`，启动时扫表补写
  - 队列满时降级同步写（阻塞主路径，保数据）
  - 幂等键 `(agentID, topicID, stepCount)` 唯一索引，重试不产生重复
- 失败可见：结构化日志含 sessionID/agentID/step/action/error，超阈值告警。
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

---

## 待完成（按优先级分级）

### P0 — 核心承诺修复，必须先做


**P0-2. 回调写入优化**（数据可靠性根基）
- 后期仍然需要回调写入队列+重试优化。

**P0-3. 大文件拆分 + 技术债清理**（其他改动的前置）
- 还有部分大文件，可后期优化

### P2 — 体验与验证

**P2-1. TUI 按最新文档重写**
- 文档：`doc/TUI设计文档.md` v2.0，参考 Claude Code。
- 核心要素：主对话占 80% 高度、底部状态栏（plan 进度+工具状态）、Tab 切换右侧 Agent 列表面板、记忆召回指示 `🧠 recalled: ...`、话题切换提示。
- 现状偏离：commit `83f3b1a`/`6f0d90b`/`2cb102b` 多次返工未对齐文档。
- 方案：先按文档重写布局，再迁现有 HTTP 驱动逻辑，不在旧实现上打补丁。

**P2-2. 集成测试模块整理**
- 现状：`test/aiopstest/` AIOps 场景完整，编程主场景缺失。
- 目标结构：
  ```
  test/
  ├── aiopstest/        # 现有 AIOps
  ├── coding/           # 编程主场景（写贪吃蛇/重构 CSS/修 bug 端到端）
  ├── browser/          # Web UI e2e（chromedp 驱动模拟用户操作）
  ├── api/              # HTTP API 全覆盖
  ├── tui/              # TUI 模拟按键流
  └── fixtures/         # 共享 fixture
  ```
- 完成开发后需有详细测试模块：模拟开发处接口的各种情况进行模型浏览器操作测试、API 调用测试等。

### P3 — 远期优化（依赖前置项）

**P3-1. 记忆层简化**（依赖 P3-3 真实 embedding 接入后评测）
- 现状：4级压缩 Raw→Standard→Compact→Marker 阈值难调，实际触发效果未评测。
- 方案：先评测再定方案，不评测就简化=拍脑袋。
- 评测指标：①触发频次 ②各级命中率 ③压缩后召回质量（人工评分） ④token 节省量。
- 评测方法：跑 `test/aiopstest/` 全套 + 编程主场景 10 轮，统计每级落库量。
- 评测后再选：可能两级够（Raw 7天 + 摘要永久），也可能保留三级但去 Marker。

**P3-2. 编程工具扩展 + 自定义工具 + Skill 装配**（原待完成 #2 扩展）
- 现状：`tool_executor.go` 仅有 7 个基础工具（ReadFile/WriteFile/ListDir/RunCommand/SearchInFiles/HTTPGet/HTTPPost）。
- 待完善：
  - 新增 Git 工具（diff/blame/log/status）
  - 测试运行器（多框架检测 Go/Python/Node）
  - 浏览器自动化（chromedp 截图/导航/点击）
  - 自定义工具：用户在 `config/tools.yaml` 注册，handler 走 MCP 协议或本地脚本
  - Skill 与工具关系：Skill 是"工具使用模板"，绑定工具子集 + 使用场景描述，助手装配 Skill 时自动获得对应工具
  - 新工具注册到 `blades_tools.go` 的 `buildBladesTools` 与 `llm_tools.go` 的 `defaultTools` 描述
- 预留接口：`ToolRegistry.Register(tool Tool)`，runtime 从 yaml 加载注册。

**P3-3. 真实 Embedding 接入**
- 现状：`embed.PseudoEmbed` 字符哈希伪向量，cosine 不可靠。
- 预留点：`internal/embed/` 包，接口抽象 `Embedder interface { Embed(text) []float32 }`。
- 测试期：伪向量跑通全链路，验证召回路径无 bug。
- 后期接入：text-embedding-3 / bge-m3 / 本地 m3e，配置 `embed.provider=openai|local|pseudo`。
- 注意：伪向量下"召回质量"评测无意义，P3-1 记忆层简化评测**必须等真实 embedding 接入后**做。

**P3-4. 各级别 Agent 模型单独配置**（原待完成 #1）
- 现状：`config/roles.yaml` 已支持 MetaAgent / DomainAgent / LightweightModel / 每个 FixedRole 各自配置 `ModelConfig`，`ModelFactory` 按角色缓存模型实例。
- 待完善：
  - 前端 Web UI 暴露各角色模型配置页（当前只能改 yaml）
  - 路由判定 / 块记忆摘要 / 反思 已用轻量模型；任务拆分（`analyzeTasks`）与代码生成（`CommonExecuteAssistantTask`）仍用角色配置的模型，可进一步细分轻量/重量映射并统计 token 成本下降比例
  - 模型分层结果的可观测性：日志/面板展示轻量 vs 重量调用分布与 token 消耗（依赖 P1-2 日志基础）

**P3-5. MCP / Skill / Computer Use / RAG / LLM Wiki 插件预留**
- 预留点：
  - `ToolRegistry` 抽象，MCP 工具走 `MCPTool implements Tool`
  - `KnowledgeSource` 接口，RAG/Wiki 各自实现 `Retrieve(query) []Chunk`
  - `ComputerUse` 作为特殊工具集，独立 `internal/computeruse/`
  - 配置 `plugins.mcp.enabled` / `plugins.rag.enabled` 默认关
- 关键：接口先定，实现后做，避免提前实现绑定死。

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
