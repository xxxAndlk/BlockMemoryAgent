## 待完成 / 开放项

1. **ReAct 端到端集成测试**（已部分修复）
   - 背景：`test/coding/*` 与 `test/api/*` 曾基于旧 ThreeLayerGraph 状态机 mock。
   - 当前状态：`test/fixtures/llm.go` mockMessage.Content 改 `json.RawMessage` 兼容 blades contrib/openai 数组格式后,e2e 全部通过（commit `e1e0eef`）。`test/coding/{snake,css,bug_fix}` 与 `test/api/*` 跑通。
   - 本次补充（2026-08-08）：mock LLM 加 SSE 流式支持（`stream:true` 返 `data:` chunk + `[DONE]`），修复 agent 走 streaming provider 时 mock 非-SSE 响应被解析为空、工具调用丢失的预存缺口。新增 `test/coding/fact_extraction_test.go`（确定性：MetaAgent -> call_sub_agent -> 子 Agent 完成 -> 事实提取 prompt 命中 mock 返非 JSON -> 回退原始保存）与 `test/coding/tree_cancel_test.go`（确定性：TreeFor 注册 Running 子节点 -> HTTP POST cancel -> 断言节点翻 cancelled + cancel func 被调）。
   - 开放动作：持续补充其他新端点的 e2e；mock SSE 仅覆盖非流式累积语义，流式增量拼接未验。

2. **TUI Agent 树运行时构建**（TUI 侧已完成）
   - 当前状态：`internal/domain/orchestrator/tree.go` 权威树 struct 已落地（commit `2e97eff`）,PG 持久化已落地（commit `5a1134d`）。Dispatcher 派发时 Register/SetCancel/Finish,HTTP 暴露 `GET /api/sessions/{id}/tree` + `POST /api/sessions/{id}/agents/{aid}/cancel`。`ReactService.ListAgents` 仍返回单 MetaAgent,但 `Tree()` 方法返回权威树快照。
   - TUI 迁移（本次完成）：`agent_tree_panel.rebuild` 与 `view.go` 任务面板改读 `agent.Tree()` 替代旧事件流派生 `deriveSubAgentNodes`。新增 `orchestratorNodesToTreeNodes`（按 ParentID 链算 depth + 状态映射 Running->Active/Done->Done/Failed->Error/Cancelled->Done）。`deriveSubAgentNodes` + 其测试已删（树是单一真相源,事件流派生有竞态/遗漏;树已 PG 持久化重启 lazy 恢复,无需 fallback）。
   - 开放动作：`verifyloop.ExecuteChild` 同步路径入树（phase 2）。

13. **评估项**
    - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试,不明确具体测试方向时需要先把方案列出,给开发处方案的代码Agent是否符合代码Agent的逻辑,不符合代码Agent纠正,符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合,不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent,期间各Agent可以反复询问,纠正,需要Agent在被询问时开一个协程进行回复,需要携带关键记忆或者协程Agent常驻共享代码Agent记忆,可评估。测试流程最为重要,查看现在的测试是否严谨与多重确认。代码Agent完成开发,找到固定助手的测试助手,先进行自测试,返回结果成功后返回给上级Agent,上级Agent按更大范围模块进行统一测试,哪个部分不行打回。
    - Agent执行任务,先拆解任务,拆解为不会干涉的单元任务后,一个单元任务使用一个干净上下文的Agent编码助手执行,压缩Token成本。执行完成后暂时不销毁,一定时间不使用直接消耗,有使用则重置使用时间并加长使用时间。原因：我在使用claude时,经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉,并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆,块记忆过于抽象,可用性与传统感觉不明显,。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式,每个领域干净上下文负责自己的事。

19. **Middleware 层抽象：统一拦截链（LLM 链 + 工具链）**（✅ 已完成 2026-08-10：Phase 0 核心抽象 + LLM 链 generate 重试迁移落地；Phase 1 剩余迁移与 Tool 链列为后续，见 doc/变更.md 任务 14）
    - 背景：链路上所有拦截点以 `if name == "ReadFile"` 式硬编码散落在两处主链路——`agent/react_agent.go RunWithHistory` 主循环（:308-496）与 `tool/registry.go Dispatch`（:276）。记忆注入/窗口截断/配对修复/预算/重试/审计在 agent 侧，守卫/探索预算/连读检测/派发守卫在 tool 侧，无统一挂载点，新增横切逻辑只能继续塞 if；重试逻辑三层重复且策略不一致（`generate` 对 DeadlineExceeded 不重试 vs `retryProvider` 重试）；工具白名单仅过滤 Schema 不过滤 Dispatch（`tool_adapter.go:22-24` 注释自认可绕过）；`plugins/registry.go` 预留插件接口未接线。设计目标：**抽象为一个方法进行调用**，链式可读（`middleware.Validate().RAG().Assemble().Call(...)`），任意扩展。
    - 核心抽象（新包 `backend/internal/middleware/`，洋葱模型，泛型，纯标准库）：

```go
// Handler 链上可执行的下一步（终端 handler 或下一个中间件的包装结果）
type Handler[C any] func(ctx context.Context, c *C) error

// Middleware 标准洋葱签名：拿到 next，返回包装后的 Handler
type Middleware[C any] func(next Handler[C]) Handler[C]

// Chain 构建器；Use 顺序 = 入向执行顺序，出向逆序
type Chain[C any] struct{ mws []Middleware[C] }
func New[C any]() *Chain[C]
func (c *Chain[C]) Use(mw ...Middleware[C]) *Chain[C] // 唯一扩展点
func (c *Chain[C]) Then(final Handler[C]) Handler[C]  // 反向包裹成洋葱
```

    - 语义约定：中间件可读写 `*C`（改写请求消息/工具参数/结果）；返回 error 立即短路（哨兵错误区分校验失败/预算耗尽/熔断，上层按类型处理）；中间件实例无状态，跨调用状态（exploreBudget 计数、连读 map、token 用量）放 session/agent scope 的 store 由中间件持有引用——**保持现有 per-agent scope 隔离不变**（ReadFile 重构确立的约束，子 Agent 互不可见）。
    - 链式 API（具名方法 = `Use(标准中间件)` 的语法糖，扩展性靠 `Use()`、可读性靠具名链；每个标准中间件独立文件 + 独立单测）：

    LLM 链（挂载点：`react_agent.go` 主循环 generate 调用处）：

```go
err := middleware.NewLLM(llmCtx). // llmCtx: Session/Role/Request/Meta
    Validate().          // 输入校验（新增：长度上限/空请求/注入检测）
    Guard().             // 安全校验+脱敏（新增，可先空实现占位）
    RAG().               // RAG 知识库注入（迁入 injectRecalledMemory/injectTopicRecall）
    Memory().            // 块记忆注入+历史压缩（迁入 MemoryPipeline.Assemble）
    Assemble().          // 上下文组装（迁入 windowMessages+sanitizeToolPairing+truncateToolCallInputs）
    Budget().            // token 预算检查（迁入 tokenBudget/LimitReached）
    Audit().             // 调用审计（迁入 logLLMCall → session_logs）
    Retry(3).            // 重试/退避（收敛 generate 与 retryProvider 两套不一致策略）
    Call(a.generateOnce) // 终端：真正的模型调用（流式走回调，Audit 在流结束后记完整响应）
```

    Tool 链（挂载点：`tool/registry.go Dispatch`）：

```go
err := middleware.NewTool(toolCtx). // toolCtx: Session/Caller/Name/Args/Result/Meta
    Permit().          // 角色工具白名单（新增：落实到 Dispatch 级，修 tool_adapter.go 仅滤 Schema 缺口）
    Guard().           // 安全守卫链（GuardRegistry 适配迁入：protected-path/sandbox/角色写路径分区）
    Budget().          // 探索预算+连读检测+连续失败 LoopExit（迁入 registry.go 现有逻辑）
    PostHook().        // 出向：WriteFile 后共享记忆失效+PROJECT.md 去抖刷新（迁入 registry.go:369-373）
    Audit().           // 工具调用审计（收敛 handleToolEvent → session_events）
    Call(t.Execute)    // 终端：工具执行
```

    - 迁移映射（现拦截点 → 中间件，原则 = 行为不变搬家，先包后删）：
      - LLM 链：`memory/pipeline.go:130 Assemble` → `Memory()`；`dispatcher.go injectRecalledMemory` + `service_react.go injectTopicRecall` → `RAG()`；`react_agent.go:317 windowMessages` + `:321 sanitizeToolPairing` + `:387 truncateToolCallInputs` → `Assemble()`；`:359 tokenBudget` → `Budget()`；`:594 logLLMCall` → `Audit()`；`:506 generate` 重试 + `model/retry_provider.go` → `Retry()`（策略在此层统一定死：4xx 快速失败、DeadlineExceeded 不重试、指数退避）。
      - Tool 链：`tool/guard_registry.go` WriteGuard/CommandGuard → `Guard()`（GuardRegistry 接口不删，作为 Guard() 内部注册表）；`registry.go:296-330` 连读检测 + `:337-348` exploreBudget + `:386-399` 连续失败 → `Budget()`；`:369-373` WriteFile 事后 hook → `PostHook()`；`tool_adapter.go:55` 白名单 → `Permit()` 落实到 Dispatch。
    - 阶段拆分：
      - Phase 0 核心抽象：`middleware.go`（Handler/Middleware/Chain）+ `llmchain.go`/`toolchain.go` 具名糖。单测：执行顺序（入向 1→2→3、出向逆序）、error 短路、`Use()` 扩展、空链直调终端。
      - Phase 1 LLM 链落地：按映射表搬家，`RunWithHistory` 主循环 generate 调用点替换为链式调用；`MemoryPipeline` 接口保留（变成 Memory() 的实现细节）；Retry 策略不一致问题在此层统一。约束：现有测试不改断言只可能改注入入口，全量 `go test ./...` 绿。
      - Phase 2 Tool 链落地：同原则搬家 + `Permit()` 新增 Dispatch 级白名单校验；GuardRegistry 与现有守卫/连读/预算测试原样绿。
      - Phase 3 出站链 + 配置化（可选后置）：终答/子 Agent 摘要的输出校验链（schema/内容过滤）；`config.yaml` 加 `middleware:` 段按名开关；`plugins/registry.go` 预留接口接线为 `Use()` 的中间件来源。
    - 不做（约束）：不引入第三方中间件/AOP 库；不搞反射注册魔法；出站内容审核、人机审批闸不在本期（审批闸已由 #17 P1 destructive 确认项覆盖）；中间件不跨 Agent 共享状态。
    - 验收：新增横切逻辑只需实现 `Middleware[C]` 并 `Use()`，不改主循环/ Dispatch 本体代码；链式调用形态如上示例；全量测试绿 + TUI 塔防任务回归。


20. **"domain 一直读文件不写直到超时"事故根治：LoopExit 接线 + 失败打捞 + 重派继承**（✅ 已完成 2026-08-10，doc/变更.md 任务 6）
    - 事故还原：domain 一直阅读文件不写 → 探索预算守卫（8 次）早已触发但 LoopExit 是死代码杀不死循环 → 空转每轮白烧 LLM 调用 → 有活动心跳不杀、token 预算未耗尽，只能等 `sub_agent_timeout_min: 60` 墙钟终止 → 超时只回传最后一条 assistant 文本截断 500 runes（`dispatcher.go:1280-1288`），**已读文件与结论全丢**（事实提取只在成功路径跑，`dispatcher.go:1501-1576`）→ MetaAgent 中途无感知无手段，60 分钟后自由心证细粒度重派，新 Agent 从空白上下文重新探索，浪费翻倍。
    - 第一层：LoopExit 死代码接线（原 #20 内容，治本——空转活不过探索预算）。背景：三层循环守卫——连读同一文件 ×3（`tool/registry.go:296-330`）、探索预算 8 次（`:332-348`）、单工具连续失败 ×3（`:386-399`）——都试图经 `tools.ActionLoopExit` 终止 ReAct 循环，但**全项目无 `tools.NewContext` 调用方**，`tools.FromContext` 永远失败，信号被静默丢弃（`:319-321`、`:395-397`），守卫退化为一句错误文本。执行：
      1. 定哨兵：`tool` 包加 `var ErrLoopExit = errors.New("loop guard: force exit")`；三处守卫命中时删掉写 ctx ActionLoopExit 的旧代码，改为返回包装哨兵（`fmt.Errorf("%w: %s", ErrLoopExit, msg)`），给 LLM 看的提示文案不变。
      2. Dispatch 透传：`registry.go Dispatch` 对 `errors.Is(err, ErrLoopExit)` 的守卫错误不再吞成普通工具结果，原样上抛（Result + 非 nil error），先确认现有调用点兼容。
      3. 主循环接住：`agent/react_agent.go` 工具派发/结果收集处检测 `errors.Is(err, ErrLoopExit)` → 终止循环并带原因返回（参照 `LimitReached` 既有路径 `:498-501`；子 Agent 经 `runSubAgent` 走 Failed 语义，父 mailbox 收到"被循环守卫终止"，级联解除父等待）。
    - 第二层：失败打捞（超时/被杀/守卫终止时把探索成果捞回来，重派不再从零）。
      1. 打捞点：`runSubAgent` 的失败路径（`dispatcher.go:940-947` `formatSubAgentFailure` 调用处）与 `killStuckSubAgent`（`:283-305`）、第一层新增的守卫终止路径，与成功路径的事实提取（`:1501-1576`）并列调用。
      2. 提取：复用 `llmFactExtractor`/`ParseFactsJSON`（bootstrap 已接 `CallLightweightWithRetry`），prompt 改为产出"已读文件清单 + 已得结论 + 卡点"打捞摘要；轻量调用 5s 超时、失败回退末条文本截断，不阻塞主失败流程。
      3. 去向一：打捞摘要追加进父 mailbox 失败消息（`formatSubAgentFailure` 文本后），MetaAgent 立即可见。
      4. 去向二：写入共享记忆 slot（FileSharedMemoryStore，key = `<parentID>:salvage:<domain>`），后续同域派发经 `buildSharedPrefix`（`dispatcher.go:1343-1427`）自动注入新 Agent 上下文。
      5. 与 #17 P0 关系：打捞**不进块记忆召回**（等 #17 P0 的 `outcome` 字段落地后再沉淀，避免失败记忆与成功记忆同权召回误导）；本期只走 mailbox + shared slot 两路。
    - 第三层：同域重派自动带前序摘要（不依赖 MetaAgent 的记性，机制上保证不重复探索）。
      1. `call_sub_agent` 校验段（`:818-824` 同域去重 `findPendingDomainSibling` 旁）查权威树快照：同父同 domain 存在 Failed/Cancelled 兄弟 → 取其打捞摘要（读第二层写的 shared slot）追加到新任务文本末尾，前缀 `【前序探索摘要】`。
      2. 无前序兄弟 / 摘要为空 → 零行为变化；摘要截断（≤2000 runes，与任务文本上限同口径）。
    - 测试：
      1. 第一层：同一 ReadFile 参数 ×3 → 循环以守卫原因退出；单工具连续失败 ×3 → 退出；普通工具错误不影响循环；探索预算耗尽只拒探索类调用、不误杀 RunCommand（#16 回归保护）。
      2. 第二层：失败/超时/守卫终止三路径都触发打捞；轻量提取失败回退截断文本不阻塞；mailbox 失败消息含打捞摘要；shared slot `<parentID>:salvage:<domain>` 可读。
      3. 第三层：同域重派任务文本含 `【前序探索摘要】`；无失败兄弟时任务文本零变化。
    - 验收：`backend/` 与 `test/` 双模块 `go test ./...` 绿；TUI 重现场景——人为让 domain 空转，探索预算耗尽即分钟级终止（不再等 60min），父收到含打捞摘要的失败消息，重派 Agent 任务文本带前序摘要。
    - 不做：不调三层守卫阈值/语义（3 次/8 次/3 连败维持现状）；不引入 blades `tools.NewContext` 机制；不做 MetaAgent 中途干预（#25 的 cancel_agent 覆盖，本项落地后空转活不到需要干预）；打捞不进块记忆召回（等 #17 P0 outcome 字段）；不改 `sub_agent_timeout_min` 配置。

21. **验收闭环：verifyloop 默认启用 + 按 spec 验收标准验收**（✅ 已收口 2026-08-10：前提被 cc8dde7 移除接线推翻【A/B 实证负资产】；落地 ExecuteChild 入树 + spec 强制验收，doc/变更.md 任务 8）
    - 背景：`verify_and_fix` + verifyloop 状态机（PlanConfirm→Review→SelfTest→UnifiedTest→Fix，≤5 轮，`verifyloop/orchestrator.go:202-325`）代码完整，但 `config/config.yaml:72-73` 两个 self_test 开关 false → `bootstrap.go:290-291` 不注册工具。当前 MetaAgent 只能盲信子 Agent 自述完成，唯一证据是 FilesModified 列表。隐患：`bootstrap.go:327` `ReviewEnabled == nil || *ReviewEnabled` 在 nil 时启用 reviewer，靠 applyDefaults 兜底掩盖。
    - 执行流程：
      1. 修隐患：`bootstrap.go:327` 改显式布尔判断，补单测覆盖 nil/显式 true/显式 false 三态。
      2. 开开关：`config.yaml` `assistant_self_test_enabled`/`domain_self_test_enabled` 置 true；验证 `RegisterVerifyTool`（`dispatcher.go:557-559`）注册后 meta/domain 白名单可见（白名单已含 `verify_and_fix`，`role/registry.go:196-229`）。
      3. 验收标准进 verifyloop：WriteSpec 已强制 `acceptance` 必填（`tool/spec.go:75-80`），把 acceptance 文本拼入 verifyloop PlanConfirm/SelfTest 的任务前缀（读取点复用 dispatcher 注入【任务规范】的 `dispatcher.go:1386-1392`），验收从"模型自由心证"变"对照白纸黑字验收标准"。
      4. ExecuteChild 入树（吸收 #2 开放动作）：verifyloop 同步子 Agent 在 `Tree` Register/Finish，TUI 可见、可取消、heartbeat 覆盖（叶子语义同现有子 Agent）。
      5. MetaAgent 规程：roles.yaml meta prompt 的【自测验证时机】段升级为硬规程——编码类任务终答前必须 `verify_and_fix` 或显式给出跳过理由；"终答前端到端自验"（#16 开放动作）由第 3 步验收标准支撑。
      6. 测试：acceptance 注入断言 + ExecuteChild 入树断言；e2e（`test/coding/`）：故意失败的代码任务 → Fix 循环触发 → 修复后通过。
    - 验收：开关默认 true；塔防全流程 `verify_and_fix` 至少触发一次且终答附验证证据；双模块测试绿。
    - 不做：不引入独立 Critic Agent 角色（verifyloop 即"固定流程折叠为工具"的既有正确范式）；不强制非编码类任务走 verify。

22. **真·执行计划：board.TaskBoard 接线 + 依赖派发门 + 面板真实进度**（✅ 已完成 2026-08-10，doc/变更.md 任务 11）
    - 背景：运行时无计划——`pkg/types/plan.go:3-4` 自我宣布退役；`board/board.go` 完整实现 TaskBoard（goal/约束/subtask/`DependsOn` `board.go:57`/状态重算/Brief）但生产零写入（`runtime/runtime.go:87` 只构造）；TUI"执行计划"面板是 Agent 树临时合成的派发日志（`tui/helpers.go:69-104`），"总体进度"恒为装饰；任务排序纯靠提示词自觉（`role/registry.go:46`），并行派发无依赖门（#16 实证：MetaAgent 未等回传重复派发渲染引擎 ×3 互相覆盖，当时同域去重治标，依赖门治本）。
    - 执行流程：
      1. Phase 0 写入侧：新增 MetaAgent 工具 `write_plan`（meta 白名单，`role/registry.go:196-204`），入参 = 子任务数组 `{id, title, domain, depends_on[], acceptance}`，实现调 `board.Manager.GetOrCreate` + `AddSubTask` 现有 API；校验失败（环依赖/未知依赖 id）返错误。meta prompt 加"拆任务先 write_plan 再派发"。
      2. Phase 1 派发依赖门：`dispatcher.go` `call_sub_agent` 校验段（`:771-838` 现有校验群同款位置）加依赖检查——depends_on 对应 subtask 非 Done → 拒绝派发并提示等谁（复用 `findPendingDomainSibling` 拒绝模式，拒绝不烧派发配额）。subtask id 与树节点关联：dispatch 时写入 node，Finish 时回写 board 状态。
      3. Phase 2 面板读真相源：TUI `renderPlanPanel`（`tui/view.go:169-172`）+ `helpers.go:69-104` 改读 `board.Snapshot`（出口参照 Tree() 快照同款）；行 = 子任务（标注依赖），进度 = Done/总数；board 为空（旧会话/未写计划）回退现有树合成逻辑。
      4. Phase 3 重规划（LLM 驱动，不自动）：`write_plan` 支持改/删非 Done 子任务（板内 version 或全量覆盖，取简）；失败子任务由 #23 结构化失败驱动 MetaAgent 决定重派/改计划。
      5. 测试：board 写入校验（环依赖/未知依赖）；依赖门拒绝+放行；面板快照映射；write_plan → dispatch → board Done 全链路。
    - 验收：塔防任务 TUI 面板显示真实子任务与进度百分比；依赖未满足时派发被拒；双模块绿。
    - 不做：不动 `pkg/types/plan.go`；DAG 不跨 session（`dag/` 包是会话级 cron 调度，不碰）；不做关键路径/甘特图等可视化增强。

23. **结构化失败 + 自动重试 + 升级路径**（✅ 已完成 2026-08-10，doc/变更.md 任务 7）
    - 背景：子 Agent 失败回传是一段自然语言（`formatSubAgentFailure`，`dispatcher.go:1280-1288`），无类型无错误码；全库无自动 retry/fallback/escalation，失败后怎么办全靠 LLM 当场发挥（`react_agent.go:948` 仅提示词）；`MsgEscalate`/`MsgMilestone`/`MsgDependency` 定义了从不发送；`errLimitReached` 哨兵（`dispatcher.go:1266`）有匹配无产出，死代码；发给已死 Agent 的消息静默消失。
    - 执行流程：
      1. 失败类型化：`dispatcher` 加 `FailureKind`（timeout/error/budget_partial/killed/loop_guard）；`treeFinish` Failed 时 mailbox body 头部加一行机读标记（`[failure kind=timeout retryable=false]`）+ 原有人读文本（父 LLM 消费习惯不变）。
      2. 自动重试（最小一档）：`kind=error 且 retryable` 的**叶子助手**失败由 dispatcher 自动重派 1 次（同任务同前缀）；复用 `config.yaml agent.retry_count: 1` 现存字段（先确认无消费方再接线）。domain 层与 timeout/killed 不自动重试（交 MetaAgent 决策，避免放大故障）。
      3. 升级路径：叶子/domain 无法自治时经 `send_message` 发 `MsgEscalate` 给 parentID（类型已有，补发送点 + 父侧 drain 展示 `[升级]` 前缀）；meta prompt 加升级处置规程（重派/自己接手/回报用户三选一）。
      4. 死信可见：`mailbox.Send` 对不存在/已完成收件人返回错误，发送方工具结果带"消息未送达"，消除静默消失。
      5. 死代码清理：`errLimitReached` 接线或删除（倾向删除，budget/maxIter 路径已覆盖语义）。
      6. 测试：每种 kind 的机读标记断言；error 重派一次后仍失败 → 父收到结构化失败；死信返错；escalate 父侧可见。
    - 验收：人为制造一次叶子失败，日志可见自动重派 1 次 + 父收到结构化失败；双模块绿。
    - 不做：不做指数退避/多档重试（只此一档一次）；不做熔断器（轻量模型 402 熔断 `pipeline.go` 已有范式，需要时再说）；MsgMilestone/MsgDependency 维持不用不删（见 #26）。

24. **人在回路：ask_user 工具 + 工具审批门通用机制**（✅ 已完成 2026-08-10，doc/变更.md 任务 10）
    - 背景：Dispatch 无审批钩子（`tool/registry.go:276-405` 守卫直连执行）；无 ask-user 工具；`PendingClarify` 恒 nil（`service_react.go:1542` 硬编码），`awaiting_clarify` 被挪作"暂停等继续"（`pauseSession :1199-1216`）；TUI `/clarify`（`tui/input.go:273-278`）dormant。自动写文件跑命令的系统全程无人类检查点。与 #17 P1 关系：本项提供通用提问/确认通道，#17 P1（destructive 工具生产边界确认）作为其消费者落地，不重复建设；与 #19 中间件兼容（审批即 Tool 链一环）。
    - 执行流程：
      1. 会话态正名：`pauseSession` 的 PauseKind（#15 已有 IterationLimit/TokenBudget/OnChild）新增 `Clarify`，复用 `awaiting_clarify` 状态但真实填充 `PendingClarify{Question, Context}`（替换 `:1542` 硬编码 nil）；用户任意回复走 #15 既有恢复路由。
      2. `ask_user` 工具：注册进 meta/domain 白名单；Execute 置 PendingClarify + 暂停会话 + 经 liveFn/session 事件推送问题；用户答复作为该工具 result 返回发问 Agent（恢复后工具结果入史，ReAct 继续）。超时（配置，默认 0=不限）未答 → 工具返回"用户未答复，自行决策"。
      3. 审批门：`Registry.Dispatch` 加可选 `approvalHook(ctx, toolName, args) (bool, error)` 字段（nil = 全放行，零行为变化）；命中审批规则的调用 → 走 ask_user 同款通道等确认，拒绝 → 工具返回"用户拒绝"。本期审批规则只接 #17 P1 的 destructive+生产边界一条，不起规则引擎。
      4. TUI：渲染 PendingClarify（问题 + 答复输入），答复走现有 input → sendMessage 路径；`/clarify` 命令视实现保留或删。
      5. 测试：ask_user 暂停→答复→工具结果闭环；approvalHook nil 零行为变化；destructive 命中暂停等确认、拒绝返错；TUI 手测。
    - 不做：不做多步表单/多选问题（一问一答文本即可）；不做权限角色矩阵；web 前端同步改造列开放动作。

25. **MetaAgent 控制面 + 全局硬终止**（✅ 已完成 2026-08-10，doc/变更.md 任务 9）
    - 背景：MetaAgent 无取消子 Agent 的工具（`CancelAgent` 仅 HTTP `server/session_http.go:307`，TUI 不调用）；子 Agent `context.Background()` 脱离会话（`dispatcher.go:852-859`），用户取消会话杀不死在跑子 Agent（残留 goroutine 烧 token 直到 60min 超时）；心跳巡检只覆盖叶子（`:883-890` 注释：domain 有 wait loop 注入会误杀）；`tool_call_max_rounds: -1` 迭代无上限 + 无会话墙钟，唯一兜底是 token 预算→暂停，系统永远不会主动止损。
    - 执行流程：
      1. `cancel_agent` 工具（meta/domain 白名单）：入参 agent_id → `Tree.Cancel`（`orchestrator/tree.go:225-249`）+ `doneOnce.Do(trackChildDone)` 计数兜底（参照 `killStuckSubAgent` `:283-305` 模式）+ mailbox 通知父"已被上级取消"。
      2. 会话取消级联：`service_react.go cancel`（`:1467-1499`）除取消 MetaAgent ctx 外，遍历树 Running 节点逐个 Cancel（detach 语义对 pause/resume 的好处保留，仅"会话终止"这一刻级联）；迟到结果被既有 purge 覆盖（`:1057-1061`）。
      3. domain 层心跳（防误杀版）：domain 的"活动"= 自身 LLM/工具活动 **或** 任一后代活动/回信（子活动沿 parentID 链向上冒泡刷新时间戳，subMeta 已有 parentID）；domain 阈值单独配置（默认 2× 叶子）。先写测试证明"domain 合法等子不被杀"。
      4. 全局硬终止：`config.yaml` 加 `session_max_wall_clock_min`（默认 0=关闭，保持现状语义）；到期 → 级联取消全部节点 + 会话置 error"超全局时限" + 树/日志已落库可复盘。`tool_call_max_rounds` 维持 -1（token 预算已兜底迭代失控）。
      5. 测试：cancel_agent 闭环（树状态 + cancel func，参照 `tree_cancel_test.go`）；会话取消后子 goroutine 退出可观测；domain 等子 60min 不误杀 vs domain 假死被杀两例；全局时限到期级联。
    - 验收：TUI 塔防回归 + 人为挂起一个 domain 验证心跳与止损；双模块绿。
    - 不做：不做抢占式优先级/调度器；不改 detach ctx 基本设计（pause/resume 依赖它）。

26. **一致性收尾：文案/开关/legacy 清理**（✅ 已完成 2026-08-10，doc/变更.md 任务 5）
    - 背景：一批"开关与文案/配置不一致"的小项，单独立项不值，放着会持续误导（LLM 读工具描述、人读注释）。
    - 执行流程：
      1. spec 强制对齐：`call_sub_agent` 工具描述声称 WriteSpec 强制（`dispatcher.go:735`）与 `spec_enforcement_enabled: false`（`config.yaml:85`）矛盾——决策：塔防实证 spec 有用，倾向开 true 跑回归；若过度阻塞则改工具描述为"建议"。同步修 `bootstrap.go:255-256` 陈旧注释"默认 true"。
      2. "任意深度的递归调用"注释（`dispatcher.go:537-538`）改为实际三层语义（Meta→Domain→叶子，`CanCall` `role/registry.go:245-287` 拒绝 domain→domain）。
      3. mailbox 未用类型（MsgMilestone/MsgDependency/广播桶）：加注释"预留无消费方"；#23 落地 escalate 后其余仍无场景则下次删。
      4. 角色写沙箱样板：Layer 4 已接线但 dormant（#12 归档：无角色配 `allowed_write_paths`）——用 `code_assistant` 验证 `enforceRoleWritePath`（`sandbox.go:192-219`）真实生效后，把样例写进 roles.yaml 注释；默认配置维持空=不限制，行为不变。
      5. 语义召回决策：embed `pseudo`（`roles.yaml:281` + `embed/pseudo.go:5-25` 字符哈希假向量）使块记忆召回非语义——决策项：配真实 embed provider（roles.yaml embed 段）或显式接受 pseudo 并在文档标注"召回为字符哈希近似"；不改代码。
      6. legacy 清理：`agent_private_memory`/`agent_snapshots` 疑似 legacy 表（`schema.go:190-211`）、`max_blocks`/`summary_interval` 字段（`pkg/config/role_config.go:40-46`）——确认无消费方后删除或标注。
    - 验收：roles.yaml 改动跑 `test/` 的 role_config_sanity_test.go；双模块测试绿；无行为回归。
    - 不做：本项不加任何新功能。

27. **外部知识库检索层：唤醒 retriever 抽象 + 混合检索 + 接编排热路径**（✅ 已完成 2026-08-10：摄入/tsvector+pgvector RRF 混合/search_knowledge 工具；RAG() 第二数据源列开放动作，doc/变更.md 任务 13）
    - 背景：接口抽象已存在但 dormant——`internal/retriever/global_kb.go`（VectorDB/Embedder/MetaStore 三接口 + `GlobalKnowledgeRetriever`）与 `store.KnowledgeStore`（PG+pgvector 全局知识表）是现成底座，当前只服务块记忆（内部记忆）写读；扩展设计 §11 的 LLM wiki 决策「不上向量库」指 wiki 文件层本身，且 `embed`/`retriever` 本就留作 §11.1 触发器储备。缺的是**面向外部预置知识（只读为主、与块记忆分层）的检索通路**：无摄入 pipeline、无全文/混合检索（纯向量 + pseudo embed，见 #26-5）、编排热路径无消费方（#19 的 `RAG()` 中间件只有块记忆数据源，无 search_knowledge 工具）。
    - 执行流程：
      1. 存储隔离：复用 knowledge 表加 `namespace`/`source` 字段（或独立 external chunks 表，取简）区分外部知识与块记忆；migrations 加迁移。
      2. 摄入：离线/管理态 ingestion（文档切块 → embed → 写库），先支持本地 Markdown/文本目录批量导入（`wiki/` 页可作来源之一），不做文档解析平台。
      3. 检索升级：真实 embed provider（OpenAI 兼容 API，复用 roles.yaml embed 段配置模式，知识库路径弃用 pseudo）；PG `tsvector` 全文检索（中文 zhparser/pg_jieba 分词）+ pgvector（索引换 hnsw，`config.yaml pgvector.index_type` 已预留）RRF 混合；rerank（bge 类）列可选后置。
      4. 热路径接线（先落地一条，另一条列开放动作）：a 注册 `search_knowledge` 工具进 meta/domain 白名单（显式按需检索，与 ReadFile 读 wiki 页互补）；b 作为 #19 LLM 链 `RAG()` 中间件的第二数据源（自动注入，带 token 上限与截断）。编排层只依赖 `retriever` 接口，不碰 PG 实现。
    - 测试：`retriever` 接口 mock 注入编排层不依赖 PG；ingestion → 检索命中集成测试（沿用现有真 PG 测试范式）；RRF 融合排序纯函数单测。
    - 验收：导入一批领域文档后，Agent 经工具或 RAG() 链路命中外部知识片段并在终答体现；双模块 `go test ./...` 绿。
    - 不做：不做知识图谱/实体抽取（需要显式结构时评估 LightRAG 作为 retriever 另一实现，列开放动作）；不推翻 §11 wiki 决策（wiki 渐进披露保留，向量检索是补充不是替代）；不接 RAGFlow/Dify 等平台（需要时同为 retriever 实现选项）；不动块记忆 pseudo 召回（归 #26-5）。
    - 开放动作：LightRAG 评估；rerank 接线；web 端知识库管理页；`search_knowledge` 与 `RAG()` 双通路中未先落地的一条。

28. **用户画像层：记忆体系第四层（外部知识库/块记忆/wiki 之外补「人」）**（✅ 已完成 2026-08-10，doc/变更.md 任务 12）
    - 背景：现有三层记忆的主体都是「事/知识」——外部知识库（#27，预置参考）、块记忆（任务经验流）、LLM wiki（§11 策展沉淀）——缺主体为「人」的画像层：MetaAgent 对用户偏好（沟通风格/技术栈/确认频率/任务拆解粒度）零感知，每轮会话从零对待用户。伏笔已有：§11.2 引 TencentDB Agent Memory 四层管道 L3 即用户画像；`soul.md` 的 `agent.PersonaInjector`（#7）已验证 system prompt 注入机制可复用。
    - 与既有层关系（防重合）：画像不进向量库、不作检索语料（小体量结构化偏好，非知识条目）；不写 wiki（非项目知识）；不写块记忆（非任务经验）。四层各司其职：图书馆 / 工作日志 / 策展笔记 / 用户档案。
    - 执行流程：
      1. 存储：单文件 `config/user_profile.md`（与 `soul.md` 对称），结构化小节（偏好/技术栈/沟通风格/反馈记录），人可直接编辑、git 可追踪。
      2. 注入：复用 `PersonaInjector` 模式加 UserProfileInjector（或扩展 soul 注入为「人格 + 画像」两段），只注入 MetaAgent system prompt（带 token 上限截断）；**不下发子 Agent**——干活 Agent 无需感知用户，防上下文膨胀与偏好泄露。
      3. 写入双路：a 用户显式陈述（「记住我偏好 X」）立即写入，不等管道；b 话题结束/会话完成时轻量模型扫对话提取偏好增量（复用 `llmFactExtractor`/`ParseFactsJSON` 模式），自动写入带可审计记录（追加 log 段或 log.md）。
      4. 可纠正：HTTP API + TUI `/profile` 查看编辑；画像错误必须可由用户一键修正，防 LLM 猜错自我固化。
      5. 测试：注入点断言（meta prompt 含画像段、子 Agent prompt 不含）；提取失败回退零副作用；显式写入立即生效。
    - 验收：两轮会话实证——第一轮用户表达偏好（如「直接改别问」），第二轮 MetaAgent 行为体现（减少确认）；双模块 `go test ./...` 绿。
    - 不做：不做多用户/ACL（当前单用户，§11.1 触发器命中再议）；不做隐式行为追踪统计（只从对话内容提取）；画像不参与向量召回排序。
    - 开放动作：web 端画像页；与 #24 ask_user 联动（画像不确定时主动问）。


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
- **Agent 树显式化**（commit `2e97eff`，2026-07-30）：`internal/domain/orchestrator/tree.go` 权威树 struct,Dispatcher 派发时 Register/SetCancel/Finish,HTTP `/api/sessions/{id}/tree` + `/api/sessions/{id}/agents/{aid}/cancel`。agent.Agent 接口加 Tree/CancelAgent。
- **块记忆 LLM 事实提取**（commit `c57f22b`，2026-07-30）：`saveBlockMemory` 先调轻量模型提取 1-5 条关键事实,每条单独落 KnowledgeRecord。新增 `subagent.FactExtractor` 接口 + `ParseFactsJSON` 解析器,bootstrap `llmFactExtractor` 复用 `ModelFactory.CallLightweightWithRetry`。提取失败回退原始保存。
- **#4 记忆 hot/cold 2 阶段分层**（已完成,commit `ced2e07` + part C）：`summarizeWindow` 从 `react_agent.go` 迁入 `domain/memory/pipeline.go` 的 `compressHistory`。Pipeline 加 `WithCompression(every, keepRecent)` + 按 agentID 步频计数器,Assemble 在步频命中时调 `compressHistory`。ReActAgent 主循环简化为仅 `windowMessages` 硬上限。`config.go` 的 `SummarizeEvery`/`SummarizeKeepRecent` 改由 Pipeline 消费。part C：MetaAgent 根 recall 注入,`SwitchTopic` 摘要 key 改 `topic:{sessionID}:{topicID}:summary`(session 前缀隔离),新增 `recallTopicSummaries`(按 session 前缀过滤)+ `injectTopicRecall`(`recalledTopicID` 去重),`runSession`/`resumeSession` 调 `agent.Run` 前注入旧话题摘要。
- **#5 配置清理**（已完成）：`config.go` 707 -> ~370 行,死字段已删（`GraphPolicyConfig`/`MemoryPolicyConfig`/`PluginsConfig` 等）。见 #10 与 cleanup commit。
- **#8 话题隔离轻量版**（已完成,commit `2b68d51` + recall 注入）：`SwitchTopic` 重写为轻量话题隔离:`Tree.EndCurrentTopic` 取消 Running 节点 + 快照 + 清内存 + best-effort 删 PG(`DeleteNodesBySession`);旧树快照压缩为摘要写入 sharedKV `topic:{sessionID}:{topicID}:summary`(session 前缀隔离);生成本会话单调递增 topicID,新话题从空树开始。无状态机,纯 KV 摘要 + 树切换。`Session`/`server.Session` 加 `ActiveTopicID` 字段透传前端。依赖 Agent 树持久化(commit `5a1134d`)已满足。recall 注入见 #4 part C。
- **#9 动态角色注册中心**（已完成,commit `f27b166`）：`domain/role/registry.go` 加 RWMutex + dynamic map + `Register`/`Unregister`/`List` 运行时 API。`Get` 优先查 dynamic 层再回退 cfg;`CallableFixedRoles` 含动态角色;`CanCall` 覆盖 `RoleTypeDynamic` 分支。新增 `role/tools.go`:`create_role`/`list_roles` 工具,`Registry.RegisterTools` 注入 `tool.Registry`,Schema 暴露两工具。MetaAgent Tools 白名单加 `create_role`/`list_roles`。Register 校验:ID 非空、不撞内置、Type 必须为 Dynamic、SystemPrompt 非空;ID 冲突拒绝。进程重启不保留（roles.yaml 才持久化）。
- **#10 verifyloop 折叠进 ReAct**（已完成,2026-07-30）：删 `Dispatcher.onSubAgentDone` 字段 + `SetOnSubAgentDone` 方法 + `SubAgentDoneHandler` 类型 + 异步完成钩子触发点。新增 `verify_and_fix` 工具(`Dispatcher.RegisterVerifyTool`),按 `code_role` 索引 `verifyloop.Orchestrator` map,Execute 同步调 `o.Run`。白名单限 MetaAgent/DomainAgent(`registry.go` Tools 列加 `verify_and_fix`)。`bootstrap` 由旧钩子自动触发改为 `RegisterVerifyTool(toolRegistry, orchestrators)`。`verifyloop` 包保留(接口 + 默认实现),仅入口从钩子改为工具。`roles.yaml` meta_agent system_prompt 加 `verify_and_fix` 说明 + 【自测验证时机】段。新增单元测试(空 orchestrators / 缺 caller context)。
- **#12 硬 Token 预算（每用户目标上限）**（已完成）：`config.go LLMRuntimeConfig` 加 `TokenBudgetPerGoal int`。`agent.ReactRuntimeConfig` + `LoopConfig` + `ReActAgent.tokenBudget` 透传。`RunWithHistory` 每轮累加 `resp.Message.TokenUsage`(优先 TotalTokens,否则 input+output),超 `tokenBudget` break 返回 `LimitReached`(部分完成,上层暂停会话等用户续跑,与 maxIter 正交)。`config.yaml` 加 `token_budget_per_goal: 100000`。默认 0 不限制。新增两单测(超预算 LimitReached / budget=0 不限制)。

### 本次归档（状态 = 已完成，主工作落地）

3. **记忆事件持久化**（已完成）
   - 当前状态：新增 `domain/memory/pg_store.go` `PostgresEventStore` 实现 `memory.Store`（SaveEvent/LoadEvents）。`Pipeline.Write` 仍维护内存 events map 供热路径 Assemble 读;PG 仅写侧持久化。`agent_events` 表（session_id 派生自 agentID、agent_id、type、role、content、tool_name、input、output、occurred + 两索引）由 `EnsureAgentEventsSchema` 建。`bootstrap` 把 `memory.NewInMemoryStore()` 换为 `memory.NewPostgresEventStore(pgStore.DB())`;`ensureSchemas` 注册 `agent_events`。旧 `session_events` 保留只读（UI 进度事件,不同事件流）。LoadEvents 按 occurred DESC 取最近 limit 条,当前未接热路径（Assemble 读内存）,供未来按需重载。
   - 开放动作：会话恢复时按 session_id 从 `agent_events` 重载事件到内存 map（当前仅写不读回）。

7. **Soul.Inject 接 ReAct 热路径**（已完成）
   - 当前状态：`agent` 包新增 `PersonaInjector` 接口（`Inject(systemPrompt) string`）,`soul.Loader` 实现该接口。`ReActAgent.WithPersonaInjector` + `systemPrompt()` 末尾把人格内容拼到完整 prompt 最前（envBlock 之前）。`ReactService.SetPersonaInjector` + `Dispatcher.WithPersonaInjector` 分别在构造 MetaAgent/子 Agent 时注入,`bootstrap` 传 `rt.Soul` 给两者。人格为空时 `Inject` 原样返回无副作用。`config/soul.md` 30 行真实人格内容,注入生效。
   - 开放动作：`Skill` 仍未接 ReAct 热路径（从未把 SkillSet 注入 prompt）。后续若确认不需要可继续删。

11. **删 Runtime 死重**（已完成,2026-07-30,步骤 6）
    - 当前状态：`internal/watchdog` + `internal/cmdqueue` 包整体删除,`runtime.Runtime` struct 去 `Watchdog`/`CmdQueue` 字段 + `WithWatchdog`/`WithCmdQueue` 选项 + `SetAgentConfig` 中 `Watchdog.SetConfig` 调用。`bootstrap` 去 `rt.CmdQueue.SetLogger`。死重判定依据:`Watchdog` 无 `.Check()` 热路径调用方(仅 runtime 自建自配置,`meta_watchdog.go` 注释为陈旧引用);`CmdQueue` 无消费方(仅 `SetLogger` 写日志,无 `Enqueue`/`Dequeue` 调用)。保留 `Board`/`Skill`/`Soul`:`Board` 被 TUI 作类型消费(`Snapshot`/`TaskStatus`),`Skill`/`Soul` 被 server API 作字典消费(`/api/skills`、`/api/agent/info`)。
    - 开放动作：`Skill` 未接 ReAct 热路径(从未把 SkillSet 注入 prompt);`Soul.Inject` 从未被调用(仅 `Soul.Name()` 作 API 元数据)。两者仍未接热路径,但保留 API 字典消费。后续若确认 Skill/Soul 不再需要可继续删。

14. **已删除的投机性泛化代码**（见 #10 归档）
    - assembly 包 / verifiers.go 扩展占位 / computeruse 包 / 实例池 / KV 三套抽象 / 强制门默认 / 配置死字段 已删。详见 git 历史。

15. **DomainAgent 暂停/恢复 + 助手部分回灌 + 单 Agent Token 预算分级**（已完成,2026-07-31）
    - 背景：旧实现子 Agent 触达 token 上限 = Failed + history 丢弃；"继续"从 MetaAgent 重派而非从暂停处续跑；budget 单份共享不分角色；`pauseSession` 误报"0 轮"(`max(-1,0)=0`)。
    - 预算分级：`config.go` 加 `TokenBudgetPerRole map[string]int`;`ReactRuntimeConfig.LoopConfigByRole(roleID)` 覆盖 `TokenBudget`(domain 50000 / meta 200000 安全网 / 叶子助手 20000,显式配置优先)。meta 不给 0:config `tool_call_max_rounds=-1` 已使 maxIter 无界,budget 再无界则模型不收敛时死循环(实证:TUI 重复思考不前进)。Dispatcher `WithLoopConfigByRole(fn)` 按角色派发;MetaAgent 路径用 `LoopConfigByRole("meta")`。resume 重置 `usedTokens` 局部变量即各 Agent 独立预算。
    - DomainAgent 暂停：触达上限 -> `errPaused` 哨兵 + `msgStore.SaveMessages` 存完整 history + `tree.Pause`(新增 `StatusPaused`)+ 不 notify 父 + 不 trackChildDone(父 PendingChildren 保持 >0)。叶子助手触达 -> `errPartialReturn` + `treeFinish Done("部分完成")` + notify 父部分产出 + trackChildDone 照常。`trackChildDone` defer bug 修为 `runSubAgent` 返 paused bool + wrapper 条件递减。
    - 持久化：新表 `agent_messages`(session_id/agent_id/seq/role/content/tool_call_id/tool_calls JSONB/reasoning)。`agent.MessagesStore` 接口 + `PostgresMessagesStore`(agent 包,复用 *sql.DB)tx 内 delete-then-insert。`EnsureAgentMessagesSchema` 建 DDL。仅 DomainAgent 持久化,叶子助手不存。
    - MetaAgent 主动暂停：`ReactResult.PausedOnChild` + `PausedChildChecker` 接口(`HasPausedChild`)。`react_agent.go` 父终结保护 wait loop 检测 Paused 子 domain -> 跳出返 `PausedOnChild`。`dispatcher.HasPausedChild` 扫树 Snapshot。MetaAgent 无限 budget 靠此跳出。
    - 恢复路由：`SessionStatusPausedOnChild` 枚举。`pauseSession` 重写为 `PauseKind`(IterationLimit/TokenBudget/OnChild)区分文案 + 修"0 轮"bug(maxIter<=0 不显示轮数)。`sendMessage` 在 PausedOnChild 态优先 `findEarliestPausedDomain`(min Started) + `resumePausedDomain` -> `dispatcher.ResumePaused`(LoadMessages 重建 domain Agent fresh budget 续跑,不强制压缩靠 Assemble 步频)。完成 -> tree.Finish + notify + trackChildDone + go resumeSession 让 MetaAgent drain mailbox 整合;再触限 -> re-pause;出错 -> 回退暂停。`Tree.Resume` 翻 Paused->Running + 绑新 cancel。D1:任意消息(含"继续"与新任务)都恢复 earliest paused domain,不强制压缩。D2:进程重启不恢复 paused session(agent_messages 数据留 PG 死数据)。
    - 开放动作：真 PG 集成验证(触发 50K goal 验 agent_tree_nodes.status=paused + agent_messages 行 + "继续"恢复);多 Paused domain 一次恢复一个(用户多次"继续");responsibility header resume 时丢失(Node 未存,接受弱化)。

16. **TUI 实战验收（塔防游戏任务）驱动的稳定性修复**（已完成,2026-08-02）
    - 背景：连续多轮 TUI 实战（在 `workspace/` 从零生成塔防小游戏）暴露一批长任务卡顿/死锁缺陷,逐项修复后最终轮一次交付通过验收（Agent 自产完整游戏,无人工干预）。
    - 入史工具入参截断：`react_agent.go` 新增 `truncateToolCallInputsForHistory`（单字符串值 2000 runes 上限,附原始长度标记）,assistant 消息入史/持久化/续跑回发用截断副本,派发执行仍用原始入参。破"WriteFile 全文逐轮重发 -> 单轮 input 100K+ -> 续跑预算一次耗尽 -> pause/resume 零进展"死锁。
    - 滑动窗口下刀条件：`windowMessages`（react_agent.go）与 `compressHistory`（memory/pipeline.go）由"锚 user 边界"改为"只避开孤立 tool 结果"。工作型历史窗口内常无 user,锚 user 会走空窗口、上下文塌缩成 2 条消息（实证：配置 Agent 每轮失忆重写 config.js 不收敛）。
    - 暂停态可取消：`service_react.go` `cancel()` 放开 `awaiting_clarify`/`paused_on_child` 状态（原仅 running 可取消,暂停死锁无逃生通道）。
    - RunCommand 移出探索预算：`tool/registry.go` exploreBudget 只计 ReadFile/ListDir（上限 8）。RunCommand 是验证/动作工具,计入预算会让 Agent 写完文件后无法验证,在"必须验证"与"工具被拒"间死循环（实证：配置 Agent 被拒 8 轮空转 4 分钟）。
    - 重复派发去重：`dispatcher.go` 新增 `findPendingDomainSibling`,`call_sub_agent` 派 domain 角色时查权威树快照,同父 Agent 下同 domain 子 Agent Running/Paused 即拒绝并提示等 mailbox 回传（实证：MetaAgent 未等回传重复派发渲染引擎×3,并发写同一批文件互相覆盖、接口漂移）。拒绝发生在限额计数之前,不烧派发配额;domain 为空不去重。
    - 子 Agent 递归派发终结保护：`runSubAgentOnce`/`ResumePaused` 注入 `WithPendingChildrenChecker(d)`,子 Agent 派发自己的子 Agent 后须等 mailbox 回传再终答（实证：domain-1 拆两个子任务后直接 DONE,MetaAgent 把中间状态误当终答,会话 completed 但产出缺失）。bootstrap 同步注入 `WithMessagesStore`（暂停恢复前置,缺失则 ResumePaused 直接失败）。
    - 预算/超时配置：`token_budget_per_role` domain/meta 400K、叶子助手 100K（原 120K/40K,扛不住塔防级多文件 codegen 一次交付）;`sub_agent_timeout_min` 30->60（预算抬高后单 domain 需在慢推理下跑完多文件生成）。
    - TUI 工作目录展示：`tui/model.go` 加 `workDir`（`currentWorkDir()`）,顶栏与欢迎页 Workspace 显示真实路径（原硬编码 `~/demo`）。
    - 回归测试：`TestReActAgent_TruncatesToolCallInputsInHistory` / `TestWindowMessages_WorkHistoryNotCollapsed` / `TestCancelPausedOnChildSession` / `TestCancelAwaitingClarifySession` / `TestDispatchDuplicateDomainRejected`。全量 `go test ./...` 绿。
    - 开放动作：验收中暴露但未修的次要项--共享记忆契约文件十六进制命名漂移;MetaAgent 终答前不做端到端自验（本次靠用户反馈闭环补上）。
    - verifyloop 自动接线【已落地 2026-08-07】：`Dispatcher.RegisterVerifyTool` 把编排器集合存到 `d.orchs`，`runSubAgent` 成功完成路径新增 `autoVerify`——命中 code_role（code_assistant/domain，随两个 self_test 开关启停）的子 Agent 在 notify 父前同步驱动"自测->修正->上级统一测试"，结论以【验证闭环:通过/未通过】前缀并入回灌摘要，未通过含失败原因。验证/修正轮走 `ExecuteChild->runSubAgentOnce`，绕开 `runSubAgent` 包装器，无递归。`verify_and_fix` 工具保留为显式复检。`config.yaml` 两开关均开；roles.yaml meta 提示词【自测验证时机】段改写为【验证闭环（已自动接线）】。回归测试 `TestDispatcher_AutoVerify_PassPrefixesSummary` / `TestDispatcher_AutoVerify_FailPrefixesSummary` / `TestDispatcher_AutoVerify_SkipsUnpairedRole`。

18. **PROJECT.md 领域按职责 LLM 分区 + 文件变更去抖刷新 + shared 清理**（已完成,2026-08-07,迭代 v2）
    - v1 问题（本次修）：v1 按依赖图连通分量聚类 + LLM 仅给预聚类簇命名。tower-defense 实证 hub 合并病：`index.html` 用 `<script src>` 连 6 js + css，CC 把全图并为 1 领域；但 6 js 零 import 边（纯 browser global），静态分析拆不开 -> 只出 1 领域，违反"越细越好"。且输出只列文件数未列影响目录/文件；文件增删改不自动刷新 PROJECT.md；`.bma/shared` 跨 session 堆积（agentID session 级，跨 session 不覆盖不清理）。
    - 聚类改 LLM 按职责分区（project 包）：`DomainClassifier` 接口改 `Partition(ctx, root, files) ([]DomainPartition, error)`（替旧 `Classify`）。`llmDomainClassifier.Partition`（bootstrap/domain_classifier.go）读各文件首 4096 字节样本，调 `ModelFactory.CallLightweightWithRetry` + `ParsePartitionJSON`，要求按职责/实体分组返 `[{name,purpose,files}]`（同职责跨文件夹亦同域，越细越好，每文件恰好归一域）。文件数超 `maxPartitionFiles=200` 返 nil 走启发式兜底。`scanDomains(ctx,root,cls)`：cls 非 nil 走 LLM 分区，未覆盖文件 + LLM 失败/空走 `clusterByDeps`（v1 依赖图）+ `heuristicName` 兜底补齐，不丢文件；cls nil 走启发式。**永不留空标注**。
    - 输出列影响目录+文件：`domainInfo` 加 `Files`/`Dirs`；`scanProject` 每域输出 `影响目录: ...` + `影响文件 (N):` 列表（>50 显前 20 + `...(+N more)`）+ 语言/入口。`EnsureProjectDoc` 签名改 `(ctx, workDir, cls)`；boot 首生成用 LLM（cls 经 `ReactService.SetDomainClassifier` 注入 session store），后续幂等跳过。
    - 文件变更去抖刷新（tool 包）：新 `project_refresh.go` `projectRefresher`（`sync.Mutex`+`time.Timer`，delay 3s）。`Registry.refresher` 在 `NewBuiltinRegistry` 初始化，refresh 闭包调 `project.RefreshProjectDoc(ctx, wd, exec.DomainClassifier())`。`Dispatch` WriteFile 成功 + RunCommand 命中删改类命令（`commandAffectsFiles`：rm/mv/mkdir/touch/cp + git rm/git mv）触发 `scheduleProjectRefresh`。去抖：连续写仅安静期触发一次 LLM 重分区；背景 ctx 不阻 WriteFile，错误仅 log。
    - shared session 清理：`FileSharedMemoryStore.Clear(ctx)` 删 root 下所有 .md。`ReactService.SetSharedMemoryStore` 鸭子断言 `interface{ Clear(ctx) error }`，桥接闭包到 `reactSessionStore.setSharedMemoryReset`；`createSession` 启动时先清 .bma/shared 旧 session 残留（spec/file_tree 不跨 session 复用，丢历史无损失）。
    - 验证：`go build/vet/test ./...` 全绿。新增 `TestDomainClassifier_PartitionByResponsibility`（5 域拆分+文件列表）/`TestEnsureProjectDoc_LLMClassifierUsed`/`TestProjectRefresher_Debounce`/`TestProjectRefresher_LastWorkDirWins`/`TestProjectRefresher_EmptyWorkDirNoop`/`TestCommandAffectsFiles`/`TestFileSharedMemoryStore_Clear`。v1 测试改 fakeClassifier 实现 Partition + 断言 `影响文件 (N):`。
    - 开放动作：真 PG+模型端到端验收 tower-defense（删 .bma/PROJECT.md + shared，跑 session 期多域 + 各域影响文件 + WriteFile 后自动刷新）；并发 session shared 互清风险（TUI 单 session 主路径，server 多并发需按 sessionID 前缀保留）；更多语言样本解析（Java/C#/Ruby）；LLM 分区 prompt 调参（簇粒度/命名质量）。
    - v1 残留（保留作 nil-cls 兜底）：`clusterByDeps`/`heuristicName`/`makeCluster`/`enumerateNodes`/`extractEdges`/`domainPurpose`/`entryCandidates` 全保留，dep 解析器（HTML/JS/Go/Python/CSS）沿用。

6. **文档漂移清理**（已完成,2026-08-08）
   - 当前状态：`doc/设计文档_v3.md` 裁剪重写（628 -> ~210 行）。删 stale §3.4 Router/§4 旧系统架构/§6 SessionBlock/§8 技术选型/§9 Phase 1-3/§10.2 场景；保留 §1-3 痛点与哲学（§3.3 话题即边界改为设计理念，去掉 SessionBlock struct）/§4.1 TokenBudget 分段表（标注「未接入 ReAct 热路径，未来目标」）/§5 提示词体系/§6 质量指标/§7 定位结语。顶部声明压一行。`项目说明.md:471/507` 引用仍准确（v3 文件名不变，保留的就是理念）。`扩展设计_Agent工作流平台.md` 顶部加声明（架构叙述 Router/Agent Core/SessionBlock 基于 v3 历史框架，当前实现以 `项目说明.md` 为准）。
   - 开放动作：无（重写完成）。

12. **共享记忆一致性**（Layer 4/5 已落地,2026-08-08）
    - 当前状态：详见 `doc/计划_共享记忆一致性.md`。Layer 1-3 已落地（2026-07-28）。Layer 4 角色写目录分区 dormant opt-in：`types.RoleDefinition` 加 `Sandbox *RoleSandbox`（`AllowedWritePaths []string`）；`tool/sandbox.go` 加 `WithRoleID`/`RoleIDFromContext` + `Executor.enforceRoleWritePath`（roleID 空/resolver nil/角色无 Sandbox -> 返 nil 跳过，零开销）；`builtin.go` writeFile 非临时分支 `sanitizeWritePath` 后调用；`react_agent.go` RunWithHistory 注入 `WithRoleID(ctx, a.role.ID)`（经 Dispatch 流到 WriteFile）；`bootstrap` 注入 resolver（roleID -> `roleRegistry.Get(roleID).Sandbox.AllowedWritePaths`）。**空=不限制**，现有 tower-defense 写根目录流不变。Layer 5 `mailbox.Message` 加 `FilesModified []string`；`agent.FilesModifiedFromHistory` 从 history 扫 assistant WriteFile ToolCalls 收集 path 去重；`dispatcher.notify` 签名加 `filesModified []string` + 6 处调用点补参（runSubAgent/ResumePaused 从 result.History 算，killStuckSubAgent 传 nil）；`react_agent.go` mailboxMessageToReact 展示 "修改文件: ..." 给父 LLM。单测 `TestEnforceRoleWritePath_*`（4）+ `TestFilesModifiedFromHistory_*`（3）。
    - 开放动作：Layer 4 当前 dormant（无角色配 `allowed_write_paths`），需按场景配 `roles.yaml` 才生效；Layer 5 仅展示给父 LLM 未做 KV 失效（Layer 2 写时 per-path 失效已覆盖）。

17. **编排加固：阿里 AICP 对比可落地项**（已完成,2026-08-10；来源：`doc/编排对比_阿里AICP军团.md`，对照 AICP「Agent 军团 + 共享黑板」方案筛出的增量加固点，不动主架构）
    - 背景：AICP 三层骨架（上层编排控制 / 中层能力三角 / 下层基础设施）与本仓库已同构（MetaAgent+dispatcher+tree / 角色+工具+skills+块记忆 / store+memory+model）。固定 9 层、纯状态机、黑板唯一媒介、三道闸门、权重飞轮均不 adopt（开放任务灵活性 / 编码样本稀疏）。仅取 4 个高 ROI 增量。
    - P0 块记忆价值反馈闭环【已落地 2026-08-10】：`saveBlockMemory`/`saveRawBlockMemory`/`saveFacts` 写 KnowledgeRecord 时 `Meta` 加 `outcome`（success/partial/fail）+ `reuse_count`（0 起）；成功路径 outcome=success，叶子部分返回（errPartialReturn）新增沉淀 outcome=partial；`injectRecalledMemory` 召回后按 `outcome=success` 优先、`reuse_count` 降序、新近优先排序，成功/避坑两段渲染（失败记忆不丢只降权）；召回命中 best-effort 递增 reuse_count（新可选接口 `reuseBumper`，PG jsonb_set 实现，PostgresStore.BumpReuse）。顺带修复潜在 bug：`runSubAgentOnce` 此前用空串作向量检索目标（`injectRecalledMemory(ctx, "")`），召回命中实为垃圾向量排序，现按 `origTask` 检索（query/task 参数分离）并去掉冗余二次检索。失败（error）路径不沉淀原始文本（质量差，待 #20 打捞落地后再沉淀）。测试：`TestInjectRecalledMemory_OutcomeSections` / `_ReuseCountSort` / `_BumpsReuse`。回路二（权重飞轮）编码样本稀疏难收敛，不 adopt。
    - P0 子 Agent 心跳检活【已落地 2026-08-06】：ReActAgent 加 `activityReporter` 回调（`generateOnce` + 工具派发触发）+ `WithActivityReporter`；Dispatcher 加 `activity sync.Map`（叶子 Agent 最后活动时间戳）+ `subMeta`（cancel/parentID/sessionID/doneOnce）+ `heartbeatTimeout`（默认 5min，`sub_agent_heartbeat_timeout_min` 配置）+ `patrol` 巡检 goroutine（首次 Dispatch 经 `ensurePatrol` 幂等启动，`heartbeatTimeout/2` 间隔，下限 10ms）。超阈值无活动 -> `killStuckSubAgent`：cancel ctx + `doneOnce.Do(trackChildDone)` 兜底递减（防流式挂起不尊重 ctx 时父永久空等）+ `notify` 父「子 Agent 疑似卡死」+ 树节点 Failed。**仅叶子 Agent 注入 reporter**：DomainAgent/MetaAgent 有自身 wait loop，注入会误杀合法等待；叶子被 kill 后父 PendingChildren 递减级联解除 MetaAgent 阻塞。解决「第二次 session 卡很久」：胖上下文推高 LLM 端点假死率，旧实现父空等 60min `sub_agent_timeout`，现 5min 心跳早暴露。回归测试 `TestDispatcher_HeartbeatKillsStuckLeaf` / `TestDispatcher_HeartbeatNoPatrolWhenDisabled` / `TestReActAgent_ActivityReporter`。
    - P1 共享记忆 slot 写入方校验 + version 乐观锁【已落地 2026-08-10】：`MDFrontmatter` 加 `version`；`versionedSharedMemoryStore` 可选接口（`SetIfVersion` CAS，冲突返 `ErrVersionConflict`），`FileSharedMemoryStore` 实现（读版本→校验→注入新版本落盘）；`WriteSharedMemory` 工具：`file_tree` 单写多读写入方校验（frontmatter owner 与写入者不一致拒绝覆盖）+ CAS 冲突重读重试一次（仍冲突返错交 LLM）；非版本后端退化普通 Set。**关键发现**：当前 key = `<ownAgentID>:<slot>`（ReActAgent 每实例唯一 agentID），兄弟 Agent 键天然不同，"任何子 Agent 拿到 parentID 都能写任意 key"的前提在当前 keying 下结构上不成立；CAS 保护同键并发写，owner 校验为同键不匹配（脏数据/未来 parentID 命名空间）的防御网。测试：`TestFileSharedMemoryStore_SetIfVersion` / `TestWriteSharedMemory_SingleWriterSlotOwnership` / `TestWriteSharedMemory_CASFallbackNonVersionedStore`。横向兄弟 file_tree 复用缺口列开放项。
    - P1 破坏性工具分级 + 生产环境命令用户确认【已落地 2026-08-10】：工具元数据 `DestructiveTool` 可选接口（WriteFile 标记 destructive）；危险命令模式表（git push/merge/reset、rm -rf/rmdir /s、drop/truncate table、npm publish、shutdown/format、kill -9 等，子串匹配 fail-safe）；`Registry.approvalHook`（nil=全放行零变化）+ `needsApproval`——WriteFile 仅 `agent.production_workdir`（新配置，默认空）下触发，RunCommand 危险模式恒触发/生产目录写类命令触发；Dispatch 执行前插入确认门，拒绝返工具错误不执行。会话层：`ReactService.ApprovalHook()` 命中边界时置 PendingClarify + awaiting_clarify 暂停 + 推 clarify 事件，阻塞等答复（Agent goroutine 存活不重建会话）；`sendMessage`/`answerClarify` 审批待定时答复路由进通道（`parseApproval` 明确同意才放行，其余 fail-closed）；取消经 ctx 解除阻塞；bootstrap 接线。TUI 无需改（awaiting_clarify + PendingClarify 占位 + 普通输入路由已有）。比 AICP 双签轻：只在边界触发，不阻塞常规编码流。测试：tool 包 5 个（生产放行/拒绝/非生产 noop/nil hook/危险命令判定/目录判定）+ agent 包 3 个（确认拒绝闭环、取消不挂起、答复解析）。**与 #24 关系**：本项做了最小一问一答审批通道；#24 通用 ask_user 工具/规则引擎仍未建，后续可把审批通道升级为 ask_user 消费者。
    - 不 adopt（归档决策）：9 层固定分层（开放任务僵化）/ 纯状态机主循环（verifyloop 已示范「固定流程折叠为工具」正确用法，不推广为主循环）/ 黑板替代 mailbox（杀紧耦合协作）/ 三道安全闸门（混沌工程特有，编码无爆炸半径语义）/ 完整权重飞轮（样本稀疏）/ 无外部需求时的 A2A + AgentCard（P2 按需，仅当开放外部 Agent 接入）。
    - 开放动作：横向兄弟 file_tree 共享（buildSharedPrefix 只读 parent 自身槽位，兄弟写不同 key 互不可见）；危险命令表规则化/可配置化；web 前端渲染 PendingClarify（待 #24）。
