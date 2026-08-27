## 待完成 / 开放项

1. **ReAct 端到端集成测试**（已部分修复）
   - 背景：`test/coding/*` 与 `test/api/*` 曾基于旧 ThreeLayerGraph 状态机 mock。
   - 当前状态：`test/fixtures/llm.go` mockMessage.Content 改 `json.RawMessage` 兼容 blades contrib/openai 数组格式后,e2e 全部通过（commit `e1e0eef`）。`test/coding/{snake,css,bug_fix}` 与 `test/api/*` 跑通。
   - 本次补充（2026-08-08）：mock LLM 加 SSE 流式支持（`stream:true` 返 `data:` chunk + `[DONE]`），修复 agent 走 streaming provider 时 mock 非-SSE 响应被解析为空、工具调用丢失的预存缺口。新增 `test/coding/fact_extraction_test.go`（确定性：MetaAgent -> call_sub_agent -> 子 Agent 完成 -> 事实提取 prompt 命中 mock 返非 JSON -> 回退原始保存）与 `test/coding/tree_cancel_test.go`（确定性：TreeFor 注册 Running 子节点 -> HTTP POST cancel -> 断言节点翻 cancelled + cancel func 被调）。
   - 开放动作：持续补充其他新端点的 e2e；mock SSE 仅覆盖非流式累积语义，流式增量拼接未验。

13. **评估项**
    - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试,不明确具体测试方向时需要先把方案列出,给开发处方案的代码Agent是否符合代码Agent的逻辑,不符合代码Agent纠正,符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合,不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent,期间各Agent可以反复询问,纠正,需要Agent在被询问时开一个协程进行回复,需要携带关键记忆或者协程Agent常驻共享代码Agent记忆,可评估。测试流程最为重要,查看现在的测试是否严谨与多重确认。代码Agent完成开发,找到固定助手的测试助手,先进行自测试,返回结果成功后返回给上级Agent,上级Agent按更大范围模块进行统一测试,哪个部分不行打回。
    - Agent执行任务,先拆解任务,拆解为不会干涉的单元任务后,一个单元任务使用一个干净上下文的Agent编码助手执行,压缩Token成本。执行完成后暂时不销毁,一定时间不使用直接消耗,有使用则重置使用时间并加长使用时间。原因：我在使用claude时,经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉,并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆,块记忆过于抽象,可用性与传统感觉不明显,。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式,每个领域干净上下文负责自己的事。

42. **通信架构重构：黑板模式（共享底座 + 选择性摄取）**  ← 来源：2026-08-13 CrewAI / MS Agent Framework 对比讨论
    - 背景：当前 agent 间通信 = mailbox（显式点对点消息，隔离强）+ 父独占 history（子 Agent 间无共享态势）。MS Agent Framework GroupChat 全量重广播 transcript（共享强但上下文互染、token 爆）。两者都不理想。本项目 event 流 + block memory(pgvector) + agent tree + task board 已是 append-only 黑板底座，缺"选择性摄取"检索层。此重构是去硬化件（#44）与可信校验（#43 证据核对取兄弟产物）的前置依赖。
    - 设计（blackboard architecture：共享底座 + 选择性摄取）：
      1. 写侧统一：所有 agent 产出（工具结果/think/子 Agent 完成摘要/fact）落 append-only 黑板（复用 event 流 + block memory + shared slot），不点对点发完整内容。mailbox 降级为"有新产出"信号 + 产物指针，不再承载内容。
      2. 读侧选择性摄取：agent 组上下文时不收全量，按 **scope + 相关性** 检索切片--自己 task 正文 + 相关 fact（块记忆 embedding 搜，复用 injectRecalledMemory）+ 触及本 scope 的兄弟产出。兄弟完成摘要作 fact 入库（outcome 已有），派发前检"有没有兄弟已做相关"。
      3. scope 显式声明：task 带 scope 标签（domain/文件路径/模块），兄弟产出按 scope 索引，确定性匹配优先于语义（embedding 兜底）。防"不知道该查"漏项。
      4. 父不再注入全量 global spec（08-13 身份混淆根因）：子 Agent 只取自己 scope 的 spec 切片（renderSpecPrefix【范围】锚定已部分做，扩展为按 scope 切片注入）。
    - 收益（去硬化件根因）：杀同域重派（兄弟产出黑板可见，替 findPendingDomainSibling 去重件）+ DomainAgent 等兄弟时读黑板决"继续等 vs 收口"（替 08-13 等待叙事空转门）+ 子 Agent 第一身份=角色提示词（替 persona 去注入 bandaid）。
    - 执行流程：
      1. 黑板读 API：`blackboard.Query(ctx, agentID, scope, query)` 统一入口，内部聚合 event 流 + block memory + shared slot 按 scope/相关性检索返切片。先建接口 + 内存实现，PG 持久化后置。
      2. 兄弟产出入库：dispatcher notify 父时同步把子 Agent 完成摘要（含 FilesModified + 结论）作 fact 写块记忆（outcome=success/partial），scope = task domain/路径。
      3. 派发前摄取：call_sub_agent 派发前经黑板查同父同 scope 兄弟产出，命中追加【前序探索摘要】到 task（#20 第三层 salvage slot 已部分做，统一收口到黑板）。
      4. 子 Agent 上下文摄取：ReActAgent Assemble 经黑板按本 task scope 取相关兄弟产出 + fact 切片注入，替代父注入全量 spec。
      5. mailbox 降级：仅传"有新产出"信号 + 黑板 key 指针，父 drain 后按指针取黑板。
    - 测试：兄弟完成摘要入黑板可按 scope 检索命中；派发前摄取命中【前序探索摘要】；子 Agent 上下文含本 scope 兄弟产出、不含无关 scope；mailbox 信号 + 指针取黑板闭环。
    - 验收：塔防多域任务，同域重派不再发生（兄弟产出黑板可见模型主动复用）；DomainAgent 等兄弟时读黑板决收口而非空转叙事；TUI 可观测黑板检索命中。
    - 不做：不做全量共享 transcript（MS GroupChat 模式，上下文互染 token 爆）；不做跨 session 黑板复用（会话级，#35 Phase 2 跨重启恢复另管）；不删 mailbox（降级为信号层保留）；不立即删去重件/空转门（#44 黑板稳定后再删）。
    - 开放风险：检索质量依赖 retriever（embedding 可能漏"不知道该查"项）--scope 显式声明 + 确定性匹配优先缓解。

43. **校验分层重构：可执行 > 证据核对 > 交叉模型 rubric，fail-closed**（✅ 已完成 2026-08-13，doc/变更.md 任务 41；来源：CrewAI/MS Agent Framework 对比讨论"当前校验很有问题"）
    - **前提修正（重要）**：原设计"复用 verifyloop 作 L0/L1 壳"失效——verifyloop 接线已于 2026-08-08 整体移除（A/B 实证自动派验证 Agent 闭环=负资产）。落地**不碰 verifyloop、不派任何验证 Agent、不加开关**：L0=确定性证据扫描（零 LLM 零 spawn），L1 证据段并入 L2 rubric judge prompt（【修改文件】+【验证证据】段），verify_kind 枚举简化为 auto/executable/rubric/none。
    - **落地**：L0 executable=扫历史 RunCommand 验证类命令（IsVerificationCommand 同口径）Success 证据，缺证据 1 轮反馈重试→verify_missing 报父（附产出全文）；L2=ReflectEngine judge 交叉模型（config judge_role 默认 prompt_reviewer）+ fail-closed（judge 错/坏 JSON→Unverified→kind=unverified 报父，绝不静默 pass）+ rubric 分项 checks；路由 verify_kind 默认 auto（reflection→rubric、code/test/reviewer→executable、其余 none）；成功摘要前缀【校验:通过(L0 证据/L2 rubric)】。
    - 遗留：塔防回归观察 auto-executable 误报率（无测试基建任务会 verify_missing，噪音大可收紧 auto 推断）。08-14 实证一处误报已修：IsVerificationCommand 认不出 `node -c`（领域提示词规定的 JS 语法检查写法，塔防验收唯一证据来源），补 node -c/go build/go vet/py_compile/pytest 标记（变更.md 任务 42）；另修 verify_kind 描述诱导（模型抄 mode 值）+ verify_missing 通知正文翻倍（打捞摘要与产出全文重复）。继续观察。

44. **去调度硬化件：根因修复后退役补偿控制**  ← 来源：同上对比讨论（"调度硬化需经苦难去除"）
    - 背景：当前调度硬化件（心跳 watchdog kill、派发预算 MaxTotalDispatches、同域去重 findPendingDomainSibling、等待叙事空转门、spec 强制门）= 系统不成熟期防 Token 爆炸/误杀的补偿控制，每条掩一个根因 bug。根因修了就该删，不是"生产成熟度特性"。目标：控制项数随版本递减到零（除 spec 门 + 终止条件）。前置依赖 #42 黑板（替去重/空转门）+ #43 校验（替预算 backstop）+ 08-13 流式活动上报（已替心跳假死判）。
    - 退役映射（每条 = 根因修法 + 退役条件）：
      1. **心跳 watchdog idle>5min kill**：根因=LLM 调用看起来假死。08-13 已修流式 delta 持续上报活动；08-14 补零 chunk 盲区（首 token 前长考静默，流存活期间 30s 保活定时器上报，真实挂死由 sub_agent_timeout 墙钟兜底）。退役条件=N 个塔防任务无 HEARTBEAT KILL 误杀 -> 删 patrol/killStuckSubAgent，仅留 provider 网络调用超时（归 LLM 调用层，非"假死"概念）。
      2. **派发预算 MaxTotalDispatches**：根因=模型过度派发。退役条件=#42 黑板兄弟可见 + #43 校验派发纪律使派发数收敛到任务所需 -> 删预算计数，留终止条件（轮/token/墙钟）兜底。
      3. **同域去重 findPendingDomainSibling**：根因=模型重派同域。退役条件=#42 黑板派发前摄取兄弟产出（同 scope 可见模型主动复用不重派）-> 删去重件。
      4. **等待叙事空转门（waitForChildren/isWaitNarration/readOnlyToolNames）**：根因=等待叙事+只读重读烧 LLM。退役条件=#42 黑板 DomainAgent 等兄弟时读黑板决"继续等 vs 收口"（事件驱动阻塞替叙事空转）-> 删空转门，留事件驱动等待。
      5. **spec 强制门（WriteSpec 前置）**：留。算流程卫生非 bandaid（派发欠规格是流程问题非系统误判）。
      6. **轮/token/墙钟预算**：留但重定位=框架级终止条件（对齐 CrewAI max_iter / MS TerminationCondition），非"防爆炸"。
    - 执行流程（按退役条件逐条，非一刀切）：
      1. 观察期：#42 + #43 落地后跑 N 个塔防任务，日志核查--无 HEARTBEAT KILL、无同域重派、无等待叙事空转、派发数收敛。
         - **首次观察数据（08-13 塔防 18:14 会话，变更.md 任务 42）**：HEARTBEAT KILL 仍发生（code_assistant-2 被杀，5m58s 零 chunk 长考流——08-13 流式 delta 上报救不了"首 token 前静默"）。已修根因（流存活期间 30s 保活定时器上报，真实挂死由 sub_agent_timeout 墙钟兜底），观察期重开。同域重派/空转/派发收敛三项该会话未见复发（domain-1 被杀子 Agent 后自愈接管未重派）。
      2. 逐条退役：每条满足退役条件后单独删（含测试），跑回归确认根因未复发。先退役风险低项（去重件、空转门），后退役 watchdog/预算。
      3. 文档：变更.md 记录每条退役 + 退役前观察数据。
    - 验收：硬化件逐条删除后，塔防任务行为不退化（无 HEARTBEAT KILL、无重派、无空转、派发收敛）；控制项数递减。
    - 不做：不一刀切全删（按退役条件逐条）；不删 spec 门 + 终止条件（保留）；不删 mailbox（#42 降级为信号层保留）；退役前必须有观察数据证根因未复发。
    - 依赖：#42 黑板 + #43 校验落地 + 观察期数据。

45. **子 Agent 收尾事实提取异步化**（同步阻塞 DONE 通知，实证最长 69s）  ← 来源：2026-08-15 评测耗时归因（test/eval/runs/20260815-135300）
    - 现象：`dispatcher.go:2364` `factExtractor.Extract`（轻量模型）在子 Agent DONE 日志前同步执行，父 Agent 收到完成通知被拖住——route-code-test-doc 的 domain-4 收尾 35s、sharedmem-long-spec 的 domain-1 收尾 69s（常规值 4-7s）。
    - 复杂原因：黑板模式（#42）的兄弟产出摄取依赖这些事实的落库顺序，异步化会引入"子 Agent 已 DONE 但事实未落库"的可见性竞态；失败打捞/校验分层消费同一结果，需要专门设计。
    - 方向：事实提取挪异步 goroutine + 落库完成事件，摄取侧按事件等待而非假设 DONE 即可见。
    - 附带记账缺口：事实提取/事件摘要走 `CallLightweightWithRetry` 不写 session_logs（llm_input/llm_output），耗时账算不平；`react_agent.go:624` `llmModelName()` 取不到值导致 logs/server.log 的 model= 全空。两者是小修，随下一轮评测基建一起落。

46. **embedding 调用挂起阻塞派发主路径（需独立短超时 + 熔断）**  ← 来源：同上
    - 现象：`injectScopedRecall`（dispatcher.go:1571）→ `knowledge_store.go:216` `pg.Embed` 走 60s HTTP 超时 × 重试（client.go:51），且 fallback `SearchBlockMemoryByGoal` 用派发 ctx（30 分钟超时）。embedding 端点间歇性挂起时阻塞派发 goroutine：实证 chain-dependency 出现 158s、longctx-multifile 出现 61s 的"派发→首次 LLM"沉默延迟（占两场景墙钟 ~12%）；pseudo embed 配置下同位置为 0s。
    - 复杂原因：要给召回链路独立短超时 + 熔断，需权衡召回质量降级策略（熔断期间静默跳过召回 vs 降级注入），涉及 `PostgresStore.Embed`/Recall 链路多处调用点。
    - 方向：embed/Recall 调用统一包独立 timeout（如 5-10s）+ 熔断器（连续失败 N 次直接跳过召回并记 WARN），降级策略显式化。

47. ~~**TUI 新会话自动选中竞态（pendingSelectID 写入已废弃的 Model 副本）**~~ ✅ 已修复（2026-08-15）
    - 现象：`input.go:395` `createSession` 后台 goroutine 写 `m.pendingSelectID`，但 bubbletea Update 是值语义——`agent.CreateSession` 阻塞期间（实证 ~15s：创建会话到图启动）tick 每 100ms 拷贝一次 Model，goroutine 完成时写入的是早已被丢弃的旧副本，`refreshView` 永远消费不到，新会话不自动选中（光标停在 -1，聊天区空白）。CreateSession 快时（<一次 tick 间隔）碰巧正常，慢时必现。
    - 实证：TUI 多轮驱动 turn01 等 60s 未见选中会话，而后端会话 11s 已跑完；与 live_harness_test.go 注释的"首条问题不可见"调试历史同源。
    - 修复：新增 `sharedState`（model.go）——`flash`/`flashUntil`/`pendingSelectID` 收进指针共享结构体（NewModel 构造一次，所有 Model 拷贝共享），配互斥锁 + nil 安全访问器 + `ensureShared` 惰性初始化；`flashMsg`/`renderInput`/`createSession`/`refreshView`/tick 全部改走该结构。flash 过期判断移到读取侧（`getFlash`）。同根因的后台 flash 丢失（post 报错不显示）一并修复。`go vet` + tui 包测试全绿（race 因本机无 gcc 未跑）。

48. **TUI Agent 编排展示优化：依赖关系可见 + 树结构按数量分叉 + 顶部任务栏只显主任务**（✅ 已完成 2026-08-15，doc/变更.md 任务 47）  ← 来源：2026-08-15 塔防三合一改造实战（workspace/logs/tui/2026-08-15.log，session-1786798928512925400-9a242b85-1）——游戏主控领域 plan_execute 长跑 20+ 分钟期间，TUI 只显示"等待子 Agent 执行"，用户完全无法判断是在干活还是卡死
    - 实证背景：MetaAgent 21:31 并行派发 3 领域（domain-2 游戏主控 / domain-3 怪物 / domain-4 路径），怪物与路径 21:35 前已完成，游戏主控因自身 6 步 plan_execute（每步整文件重写 23-25KB + 逐项验收，单次 LLM 调用 14s-3m28s、输入 ~49k tokens）一直跑到 21:52+。期间 MetaAgent 空等，聊天区实时状态行恒为 `⏳ 等待子 Agent 执行: domain-2`（`helpers.go:514-521` `waitingSubAgent`），主任务目标不可见，观感等同"卡死"。
    - 子项 1（依赖关系展示）：编排面板/状态行增加 Agent 间依赖与等待关系——谁在等谁（如"MetaAgent ⏳ 等待 游戏主控领域Agent"、"游戏主控领域Agent 依赖 路径实体领域Agent 的 GameGrid 交付"）。数据源：orchestrator 树父子关系 + `waitingSubAgent` 已算的"已派发未回传"集合 + spec/契约中的跨域依赖（可从 WriteSpec 依赖字段或 mailbox 等待状态提取）；等待中的节点卡片标注"等待 XX 完成"。
    - 子项 2（编排树结构重构）：`agent_tree_panel.go` 当前把所有 depth≥1 节点扁平进同一个网格（`renderAgentsPanel:344-357` children 不分层级），连接线是一根对齐网格列的横杠（`agentConnectorLines:492-522`），领域与领域派发的助手混排无法区分归属。改为真树形：meta 下按领域数量动态分叉——1 个领域一条竖线、2 个分两叉、3 个分三叉（连接线按实际领域数生成而非按网格列数）；每个领域节点下再挂该领域自己派发的助手子分支，归属清晰。名称/描述过长时缩小字号或竖向换行展示，避免卡片被 truncate 后看不出谁是谁。
    - 子项 3（顶部任务栏只显主任务）：聊天区实时状态行的"⏳ 等待子 Agent 执行： xxx"不应顶替主任务展示——顶部固定展示当前用户目标（任务看板 goal 首行），等待信息降级为次要行或并进编排面板。涉及 `helpers.go:486-528` `appendLiveItems` 的条目优先级与标题来源。
    - 验收：重放同类三领域并行任务，(a) 任一时刻能从 TUI 直接读出主任务目标；(b) 能读出"谁在等谁"；(c) 1/2/3 领域场景下分叉形态正确且助手挂在所属领域下；(d) 长跑任务期间状态行有进度感（如当前步骤/最近工具调用），不再像卡死。

49. **增量编辑工具（Edit/patch）：根治整文件重写导致的输出 token 黑洞**（✅ 已完成 2026-08-15，doc/变更.md 任务 47）  ← 来源：同 #48，2026-08-15 塔防三合一改造实战复盘——执行侧提速的第一优先级，收益大于执行模式调整
    - 现象：当前 `WriteFile` 工具描述明确"整文件覆盖，禁止只发修改片段"，无增量编辑能力。实证：游戏主控领域改 `js/game.js` 每步都要全文重读（646 行分 3 页）+ 全文重写（单次 WriteFile 输出 23-25KB），改一处也要生成整个文件；单次 LLM 调用因此拖到 1-3.5 分钟，6 步 plan_execute 累计 20+ 分钟。react 模式同样受此制约——这是比 plan_execute ceremony 更大的耗时根因。
    - 方向：新增 `EditFile`（或 `PatchFile`）工具，语义对齐主流 Agent 工具的精确替换：old_string/new_string 精确匹配替换（old_string 须唯一或带 replace_all），匹配失败返回就近上下文提示而非静默；与 WriteFile 的 mtime 失效机制（WriteSpec/WriteSharedMemory 的 stale 判定）对齐——EditFile 同样触发文件变更失效。工具描述引导：小改（<20% 文件）优先 EditFile，新建/大改才 WriteFile。
    - 注意点：old_string 匹配的健壮性（缩进/空白差异）、与 `.bma/snapshots` 自动备份机制兼容（EditFile 也要先备份）、并发多 Agent 改同一文件的冲突语义（可后置，先单写者场景）。
    - 验收：(a) 单测覆盖精确替换/多处匹配拒绝/replace_all/无匹配报错；(b) 重放塔防改造同类任务，同等改动量下输出 token 与墙钟时间显著下降（对照 #48 实证基线：单步 23-25KB 输出 → 预期降到 KB 级）；(c) EditFile 触发 WriteSpec stale 失效与 WriteFile 行为一致；(d) 双模块 `go test ./...` 绿。

50. **plan_execute 步骤报告瘦身：验收输出约束为结论+证据行号**（✅ 已完成 2026-08-15，doc/变更.md 任务 47）  ← 来源：同 #48 复盘——步骤报告是 plan_execute 模式第二大输出开销
    - 现象：实证游戏主控领域每步结束强制输出 2000+ token 的交付报告（改动清单表 + 验收证据表 + 备注，整段复述改动内容），6 步累计上万 token，单次 LLM 调用 output 最高 2499 tokens。报告详细度超出父 Agent 整合所需——父 Agent 只需要结论、验收结果、关键位置（文件+行号）。
    - 方向：plan_execute 的步骤完成报告模板改为三段式瘦身格式：①结论（通过/未通过，一行）②关键改动位置（函数名+行号清单，不复述代码）③验收证据（逐项 验收标准→PASS/FAIL→证据引用，一行一条）。禁止整段复述 diff 与代码原文（父 Agent 需要细节可自行 ReadFile）。模板落 plan_execute 执行器的步骤提示词；验收标准逐条判的要求保留（这是步骤 3-5 抓到 PowerShell 引号吞字、CSS 类 MISS 等真实问题的纪律来源，不能瘦身掉）。
    - 验收：重放同类多步骤任务，步骤报告 output token 下降 50%+，且验收纪律不退化（逐项 PASS/FAIL 仍在、仍能拦截不达标步骤）；父 Agent 整品验收所需信息不缺失。

51. **插件系统 Agent 自安装闭环（对标 Claude Code marketplace 体验）**（✅ 已完成 2026-08-16，doc/变更.md 任务 58）  ← 来源：2026-08-16 用户提问"告诉系统 Agent 想装某插件，它自动搜索、安装并立即可用（Claude Code 可支持），当前系统是否也可以"（评估结论见 doc/变更.md 任务 57）
    - 现状结论：MCP 热插拔已具备（mcp/service/bundle 三种形态 + enable/disable/reload API，启用后下一轮 ReAct 迭代即见工具）；缺 Agent 自驱动安装链路——插件管理仅暴露 HTTP API，Agent 无对应工具；无"安装"概念（API 只能启停 plugins.yaml/plugins.d 已声明条目，无动态新增 manifest 入口，无插件市场/注册表搜索源）。
    - 补齐项：
      1. **Manager.Install(ctx, manifest)**：校验 → 写入 `plugins.yaml` 或落 `plugins.d/` → 复用现有 Load/Enable 路径立即生效；含失败回滚、并发安全、配置文件原子写（防写坏丢配置）。
      2. **Agent 工具组**：`plugin_search`（查目录源：MCP 官方 registry API + 内置精选目录）、`plugin_install`、`plugin_enable`、`plugin_disable`、`plugin_list`（包装现有 Manager 方法，接入既有审批/确认链）。
      3. **安全门**：安装动作走敏感操作二次确认；registry 来源白名单，防 Agent 拉任意代码执行（镜像/包来源校验可后置）。
    - 落地（任务 58）：
      - **Manager.Install**（`plugins/install.go`）：校验（ID 字符集/可装形态 mcp|service/传输最小契约）→ 原子写 `plugins.installed.yaml`（临时文件+rename，不覆盖用户手写 plugins.yaml，loadDesired 合并时手工条目优先）→ createInstance + Enable 立即生效；失败回滚（摘实例+移除已写条目），`installMu` 串行化。enabled=false 只注册不启用，重启后保持。
      - **目录检索**（`plugins/catalog.go`）：内置精选目录（sequential_thinking/filesystem/fetch/memory/time/git/github/gitlab/slack/firecrawl/everything 11 条，settings 完整可直接装）+ 远程 registry（MCP 官方 `registry.modelcontextprotocol.io/v0/servers`，契约 `{servers:[{server:{name,title,description,version,remotes,packages},_meta:{...isLatest}}]}`，isLatest 去重、streamable-http 优先、npm stdio 兑底 npx -y，8s 短超时 fail-soft）+ 已配置实例（带状态）；来源白名单 = `plugins.yaml` `registry_sources`（缺省官方端点），只查白名单内 URL。
      - **工具组**（`tool/plugin_tools.go` + `plugins/tooladapter.go`）：`plugin_search` / `plugin_install`（目录条目按 id 解析或手工 manifest，env/roles/destructive 可覆盖）/ `plugin_enable` / `plugin_disable` / `plugin_list`，经 `tool.PluginManager` 接口注入（tool 包不反向依赖 plugins）。白名单：meta 全五件，domain 仅 plugin_list。
      - **安全门**：`plugin_install` 自标 `Destructive()=true` → 自动接入既有审批守卫链（approval hook 未接线零变化）；registry 来源白名单如上。
    - 验证：`plugins`/`tool`/`config` 三包新增 20 个单测（安装/回滚/冲突/校验/跨 Reload 持久化/检索聚合/registry 契约 httptest/工具格式化/审批门/Schema 暴露）；`go build && go test ./backend/...` 29 包全绿；真实 LLM 会话 E2E（session-...-06bf5b4f-1）："装 Sequential Thinking" → plugin_search 命中目录 → plugin_install 触发审批门（awaiting_clarify）→ 用户批准 → 安装成功 running + 工具 sequentialthinking 注册 + plugins.installed.yaml 落盘 + plugin_list 确认，当轮即可调用。既有热插拔闭环（web_search 26 工具 running 等）不退化。
    - 验收：对话中说"装 XX 插件"→ Agent 搜索 registry → 安装 → 当轮即可调用新工具；全量 `go test ./backend/...` 绿；既有热插拔 E2E 闭环（任务 52/55/56 验证项）不退化。

52. **插件分配给 Agent 的机制：静态权限天花板 + Agent 天花板内自选（分层混合）**（✅ 已完成 2026-08-16，doc/变更.md 任务 59）  ← 来源：2026-08-16 用户问"给 Agent 分配插件使用（如 UI 助手用画图插件），写死好还是 Agent 自己分配"——分析结论：写死管"能不能用"（权限），Agent 管"用不用/何时用"（选择），不开放 Agent 修改分配关系本身
    - 分析结论：
      - 纯写死（现状）：安全边界确定、可审计可复现、零运行时成本；但新组合要改配置+reload，长尾覆盖差。
      - 纯 Agent 自分配：灵活但让 LLM 决定权限是安全红线（prompt injection 可诱导把 computer_use 等 destructive 工具分给不可信子 Agent），且不可审计、增 token 开销。否决。
      - 分层混合（采纳，对齐 Claude Code 权限模型）：`plugins.yaml` 的 `roles` 白名单 = 权限天花板（Agent 不可改）；天花板内 Agent 通过工具目录自主选用/按需挂载，解决全量 schema 注入的上下文膨胀。
    - 现状基础（已具备，不动）：`roles.yaml` 角色基础工具白名单（owned）∪ `plugins.yaml` 插件 `roles` 白名单（`plugins/manager.go` ToolVisibility），ReAct 每轮现取可见集。
    - 执行项：
      1. **天花板层（保留强化，无新代码或少量）**：插件 `roles` 白名单语义固化为权限红线——destructive 插件（computer_use）必须显式授权角色；#51 的 plugin_install 装好后必须落 roles 声明（默认 `["*"]` 需确认门，敏感插件强制显式列表）。文档明确"分配=权限声明，仅人改配置"。
      2. **工具目录工具 `tool_catalog`**：新增只读 Agent 工具，返回该角色天花板内全部插件工具的名称+一句话描述（不含 schema），按插件分组——Agent 借此"知道自已能用什么"，替代全量 schema 注入。
      3. **按需挂载/收窄**：可见集默认收窄为角色基础工具 + 任务相关插件工具；Agent 经 `tool_catalog` 发现后，下一轮 Schema 纳入目标工具（挂载请求仅在天花板内生效，越界直接拒绝并说明）。实现上扩展 ToolVisibility 回调为 (ceiling ∩ 已挂载集)。
      4. **派发侧 `tools_hint`**：`call_sub_agent` 支持 MetaAgent 在 task 中声明建议工具集，dispatcher 校验 ∩ 子 Agent 角色天花板后收窄其可见集——实现"派 UI 任务时提示用画图插件"而不放权。
    - 验收：(a) 越界挂载/委派被拒绝且日志可查；(b) 全量工具场景下注入 schema 数显著下降（对照当前全量基线）；(c) MetaAgent 派 UI 任务带 tools_hint，子 Agent 当轮可见对应插件工具；(d) 全量 `go test ./backend/...` 绿，任务 52/55/56 热插拔 E2E 不退化。
    - 依赖：与 #51（Agent 自安装闭环）衔接——安装入口与挂载入口共用天花板校验。
    - 落地（任务 59，2026-08-16）：
      - **天花板层**：`validateInstall` 强制 destructive 插件（如 computer_use）显式声明可见角色（缺省/["*"] 直接拒绝安装）；`Manager.Install` 落 roles 声明（settings["roles"] 归一化：manifest.Roles → settings.Roles → 缺省显式 ["*"]，重启后可见性语义一致）；`config/plugins.yaml` computer_use 显式 `roles: ["meta"]` + 头注声明"分配=权限声明，仅人改配置"。
      - **挂载机制**（`tool/mount.go` + `tool/mount_tools.go`）：Registry 按 scope（agentID）存挂载集（`MountForScope` 天花板校验：工具已注册 + 插件 owned + 角色 visible，越界拒绝并记日志；`MountedTools`/`UnmountTools`）；新工具组 `tool_catalog`（只读枚举天花板内插件工具，按插件分组 + 一句话描述 + 已挂载 ✓ 标注，不含 schema）/ `tool_mount`（越界直接拒绝并说明）/ `tool_unmount`。meta/domain 白名单加入三工具。
      - **可见集收窄**：`agent/tool_adapter.go` `NewToolRegistryAdapterForRole` 加 scope 参数，`Schema()` = 静态白名单 ∪（插件可见集 ∩ 已挂载集）——默认收窄为角色基础工具，插件工具按需挂载，缓解全量 schema 注入上下文膨胀。meta scope=sessionID、子 Agent scope=subAgentID（resume 用 pausedNodeID，挂载集天然跨 resume 保留）。
      - **plugin_install 自动挂载**：安装成功后把新工具天花板内部分自动挂载进调用者 scope（"装完当轮即可调用"闭环不破）；越界项（roles 限定他角色）回告不挂载。
      - **派发侧 `tools_hint`**：call_sub_agent / call_sub_agents 新增可选 `tools_hint` 字段，dispatcher 校验 ∩ 子 Agent 角色天花板后预挂载进子 scope（派 UI 任务提示用画图插件而不放权）；越界/未注册项忽略并随派发结果回告父 Agent（日志可查）。
      - **bootstrap**：`toolRegistry.SetPluginVisibility(plugins.Manager.ToolVisibility)`（与 agent/dispatcher 同源回调，tool 包不反向依赖 plugins）。
      - 验收对照：(a) 越界挂载/委派拒绝且日志可查 ✓（MountForScope 拒绝 + dispatch 回告 + log）；(b) schema 数下降 ✓（默认收窄，挂载后才注入插件工具 schema）；(c) tools_hint 子 Agent 当轮可见 ✓（TestDispatch_ToolsHint 断言子 scope 预挂载）；(d) 全量 `go test ./backend/...` 29 包绿，任务 52/55/56 热插拔 E2E（plugins/server 包内）不退化 ✓。

53. **Agent 问答系统补全：TUI 提问可见可答 + 结构化选项（单选/多选/是否确认）**（✅ 已完成 2026-08-16，doc/变更.md 任务 60；后续交互迭代见任务 61——领域进度面板撤除、原位置改为问答面板（问题全文+选项）、选项 ↑/↓ 高亮选择 + 空格多选 + 回车提交对齐 Kimi Code；来源：2026-08-16 用户反馈——Agent 发起提问/破坏性操作确认时，TUI 只在 Agent 树面板显示"待答复：【需确认】该工具为破坏性操作…"40 字截断占位，用户看不到问题全文、没有选项可点、不知道该如何回答，流程看似卡死）
    - 背景（链路勘察结论，2026-08-16）：后端问答链路**完整无断点**——`ApprovalHook`/`AskUserHook`（`service_react.go:836-951`）置 `pendingClarify` → 状态 `awaiting_clarify` → 发 `clarify` 事件 → 阻塞等 `approval`/`askUser` channel；答复入口已有两条（`sendMessage` L1731 普通消息直答、`answerClarify` L1886）。Web 端**全链路可用**（SSE 推 `awaiting_clarify` 帧 → `AssistantTurn.vue:123-130` 渲染问题卡片 → 输入框 POST `/clarify`）。断点全在 TUI：
      1. `eventChatItem`（`tui/helpers.go:836-978`）的 switch **没有 `clarify` 事件分支**，落到 L978 丢弃——问题全文永远进不了对话区，用户只能看到树面板 40 字截断（`agent_tree_panel.go:117-131`）。
      2. TUI 无任何"正在等待你的答复，直接输入回复即可"的提示；`inputClarify` 输入模式（`keys.go:36`、`input_bar.go:157`）是**死代码**——全库没有地方把 mode 设成它；`/clarify <id> <ans>` 命令（`input.go:294-300`）要求的 question_id（`approve-xxx`/`ask-xxx`）在 TUI 任何界面都不显示，用户无从得知。
      3. 实际"直接输入普通文本回车"就能答复（sendMessage → 写 channel），但 UI 零引导，用户看到 Waiting 就以为卡死。
    - 现状的结构性缺口（问答能力本身）：`ClarifyRequest`（`agent/types.go:95-103`）**无 Options 字段**；`ask_user` 工具 schema（`domain/tool/ask_user.go:75-79`）只有 `question` + `timeout_sec`，纯自由文本；破坏性确认靠 `parseApproval`（`service_react.go:889-895`）关键词匹配（"确认/yes/ok…"放行，其余 fail-closed）——没有结构化选项，模型只能让用户自由打字，确认语义靠猜词。
    - 目标：① TUI 上提问/确认**可见**（问题全文进对话区）且**可答**（明确引导 + 快捷操作）；② 问答支持**结构化选项**——单选、多选、是/否确认三类，覆盖两个典型场景：破坏性操作确认（选项=确认/拒绝）与方向不明确时的方向选择（选项=若干方向，单选或多选）；③ Web 端同步支持选项按钮。
    - 设计（三块，块 1 是止血 P0，块 2/3 是能力扩展 P1）：
      1. **块 1：TUI 问答可见可答（P0，不动协议，纯 TUI 渲染+交互）**：
         a. `eventChatItem` 增加 `clarify` 分支：问题全文（`ev.Message`）渲染进对话区，样式区分发问（Agent=System，醒目卡片/高亮）与答复（Agent=User）。用户在主对话区直接看到完整问题。
         b. `awaiting_clarify` 状态下输入栏提示引导：输入栏 placeholder/状态栏显示"⏳ Agent 等待答复，直接输入回复回车（或输入「确认」/「拒绝」）"。复用现有 sendMessage 路径，零协议改动。
         c. 接线 `inputClarify` 死代码或删除：若接线——session 快照 `awaiting_clarify` 时输入栏自动切 `inputClarify` 模式（提示符 `/clarify>`），回车走 `/clarify` 路由（question_id 从 `PendingClarify.ID` 自动取，不要求用户手输）；若判断普通消息路径已够，则删除死代码避免误导。二选一，倾向接线（语义明确、事件记录走 answerClarify）。
         d. 树面板占位节点 label 不再截断关键信息：保持 40 字截断但 prepend 类型图标（❓提问 / ⚠️确认），详情已在对话区可见。
      2. **块 2：结构化选项协议（P1，DTO + 工具 schema + 答复解析）**：
         a. DTO：`ClarifyRequest` 加 `Options []ClarifyOption` + `MultiSelect bool` + `Kind string`（`confirm`/`choice`/`text`，缺省 `text` 向后兼容）；`ClarifyOption{ ID, Label, Description }`。
         b. `ask_user` 工具 schema 扩展：加 `options`（数组，每项 `{id, label, description}`）与 `multi_select`（bool，默认 false）。模型调用约定：纯确认场景可不传 options（Kind=confirm 由 hook 自动生成 确认/拒绝 两选项）；方向选择场景传 2-N 个 options。工具描述中写明"方向不明确、需要用户拍板时给选项，不要开放式提问让用户打字"。
         c. 破坏性确认对齐选项化：`ApprovalHook` 构造 `pendingClarify` 时填 `Kind=confirm` + `Options=[{confirm,确认执行},{reject,拒绝取消}]`，文案不变；`parseApproval` 保留作自由文本兜底（Web/旧客户端仍可打字答复），选项 ID（`confirm`/`reject`）直接映射放行/拒绝，优先于关键词匹配。
         d. 答复解析：`answerClarify`/`sendMessage` 收到答复后——若 `Options` 非空，先精确匹配选项 ID 或 Label（含数字序号 "1"/"2" 映射第 N 项，TUI/Web 快捷选择都走这里）；多选时逗号/空格分隔多个 ID；匹配失败回退自由文本原文（多选未选满不报错，Agent 自行判断）。答复结果写回 `ClarifyRequest.Answer` 时同时记录选中的 option IDs。
         e. 事件与 SSE：clarify 事件 payload 或 session 快照携带 Options（Web SSE `awaiting_clarify` 帧加 `options`/`multi_select` 字段，`stream_http.go:86-102`）。
      3. **块 3：选项交互 UI（P1，TUI + Web）**：
         a. TUI 对话区渲染选项列表：clarify 卡片内编号列出选项（`1. 确认执行 — 将执行 xxx`），多选标注"可多选，逗号分隔"。
         b. TUI 快捷键：awaiting_clarify 且 Options 非空时，数字键 `1-9` 直接选中对应选项并发送（单选即选即答；多选切换勾选、回车提交）；`y`/`n` 快捷映射 confirm/reject。Esc 不答复、退回普通输入（保持 Waiting）。
         c. Web：`AssistantTurn.vue` 的待澄清卡片渲染选项按钮（单选=点击即提交；多选=checkbox + 提交按钮），点击 POST `/clarify` answer=选项 ID。
    - 执行流程：
      1. 块 1（TUI 止血）：`helpers.go` clarify 分支 + 输入栏引导 + inputClarify 接线/删除 + 树面板图标。手动验证：跑一个触发 ask_user 的会话，TUI 对话区看到问题全文，直接输入答复流程走通。
      2. 块 2（协议）：`types.go` DTO → `ask_user.go` schema → `service_react.go` ApprovalHook/AskUserHook 填选项 + 答复解析 → SSE 帧扩展。单测覆盖解析分支。
      3. 块 3（UI）：TUI 数字键交互 + Web 选项按钮。
    - 测试：
      1. 单测——选项答复解析：选项 ID/Label/数字序号三种命中、多选分隔解析、无匹配回退自由文本、confirm/reject 映射优先于关键词。
      2. 单测——ApprovalHook 构造的 pendingClarify 含 Kind=confirm 与两选项；ask_user hook 透传 options/multi_select 到 pendingClarify。
      3. 单测——ask_user 工具 schema 含 options/multi_select 参数且 Execute 正确透传。
      4. 回归——无 options 的旧行为不变：自由文本答复、parseApproval 关键词、超时自行决策（`ask_user.go:93-98`）全部不退化（复用现有 `approval_test.go`/`ask_user_test.go` 并扩充）。
      5. 集成/TUI 手动——破坏性操作确认场景：TUI 对话区显示全文 + 1.确认/2.拒绝，按 2 拒绝后工具返回"已被用户拒绝"；方向选择场景：ask_user 带 3 选项，按数字键答复，Agent 收到选中项。
    - 验收：(a) 复现 2026-08-16 场景——Agent 发起破坏性确认，TUI 对话区可见问题全文与选项，用户按数字键或输入文字均可答复，不再"看着 Waiting 干瞪眼"；(b) 方向不明确时 Agent 用 ask_user 给 2-N 个方向选项，单选/多选均可正确回传；(c) Web 端选项按钮可用；(d) 旧自由文本答复路径全兼容；(e) 双模块 `go test ./...` 绿。
    - 不做：不做多轮向导式问答（一次一问及答，多轮由 Agent 多次调用 ask_user 实现）；不做选项的动态生成 UI 表单（仅限预定义选项列表）；不做 question_id 多并发问答（同一 session 同时刻仅一个 pendingClarify，沿用现状）；不改 fail-closed 安全语义（无法识别的确认答复仍判拒绝）；不做 Web 端以外的第三方客户端适配。
    - 落地（任务 60，2026-08-16）：
      - **协议层**：`agent/types.go` + `pkg/types/graph.go` 的 `ClarifyRequest` 加 `Kind`（confirm/choice/text，缺省 text 向后兼容）/`MultiSelect`/`Options []ClarifyOption{ID,Label,Description}`/`AnswerOptionIDs`；`server/session.go` 快照映射同步；`domain/tool/ask_user.go` 实现 `SchemaSource`（schema 含 options/multi_select，LLM 可见结构化选项）+ `AskUserHookFunc` 签名加 `AskUserOptions`（旧两参调用点全量迁移）；`ApprovalHook` 构造 `Kind=confirm` + 确认/拒绝两选项；`parseClarifyAnswer`（选项 ID/Label/数字序号命中、多选逗号顿号空格分隔、全命中回传 Label 连接、任一未命中回退自由文本）+ `resolveApproval`（选项裁决优先于关键词，fail-closed）+ `recordClarifyAnswer`（Answer/AnswerOptionIDs/AnsweredAt 写回）；sendMessage/answerClarify 四分支统一走解析；`stream_http.go` awaiting_clarify 帧加 options/multi_select。
      - **TUI（块 1 + 块 3a/3b）**：`eventChatItem` 加 clarify 分支（提问 ❓ 剥离"Agent 提问: "前缀 / 答复 ✅ 剥离"提问答复: "/"审批答复: "前缀，空消息不展示）；对话区只给**最后一条**澄清提问卡片注入当前选项列表（多轮 ask_user 旧卡片不串选项）；buildContent 加 ❓（LogWarn）/✅（LogSuccess）样式；`syncInputMode` 按会话状态自动切 `inputClarify` 模式（awaiting_clarify ↔ inputClarify，离开后清空多选集）；submitInput 澄清模式非命令输入自动带 question_id 走 `/clarify`（id 缺失回退 sendMessage）；输入栏第二行引导"⏳ Agent 等待答复：直接输入文字回车提交（或按数字键 1-9 选择，y=确认 / n=拒绝）"；数字键 1-9 单选即答/多选 toggle+Enter 提交、y/n 映射 confirm/reject；树面板占位节点 ⚠️ 待确认/❓ 待答复。
      - **Web（块 3c）**：SSE awaiting_clarify 帧由 index.vue 拦截存入 clarifyPending（不 push 进 events，避免 turns 当思考步骤；onSnapshot/切换会话时复位）；MessageList/AssistantTurn 透传 props；待澄清卡片渲染单选按钮（点击即提交选项 ID）/多选 checkbox+提交（ID 逗号分隔），底部提示"也可直接输入文字答复"；提交成功 emit submit-clarify → 父级置 running + 重开会话重建事件流。自由文本答复路径（handleSubmit → /clarify）不动。
      - 测试：agent 包新增 `clarify_options_test.go`（parseClarifyAnswer 8 命中用例 + 回退、resolveApproval 12 用例、recordClarifyAnswer、ApprovalHook 选项结构 + 序号 1/2 裁决、ask_user hook 选项透传 + 序号回传 Label 入下一轮）；tool 包新增选项透传/垃圾选项降级/schema 断言 3 用例 + 旧 hook 签名迁移；TUI 包新增 `clarify_test.go` 5 用例（eventChatItem 提问/答复/空、选项注入、syncInputMode）；既有测试回归（approval/ask_user 自由文本路径、schema 计数 23→24 因 ask_user 实现 SchemaSource）。
      - 验证：`go build && go vet && go test ./backend/...` 29 包全绿；web `vue-tsc -b && vite build` 绿（顺带修复 package-lock.json 预存漂移：dompurify/@types 缺失，npm install 同步）。
      - 开放动作：TUI/Web 真机人工验收（破坏性确认按 2 拒绝、方向选择按数字键、Web 点按钮）；真实 LLM 会话观察模型是否按描述主动传 options（schema 已暴露，行为依赖模型）；answerClarify 答复后旧卡片选项随 PendingClarify 清空自动消失（已按"最后一条"注入规避串卡片）。

54. **judge LLM 解析健壮化 + judge 不可用降级策略（unverified 误判放大重派）**  ← 来源：2026-08-17 塔防 UI 增强任务 90 分钟未结耗时归因（`workspace/tower-defense/logs/tui/2026-08-17.log`，session-1786949168171235200-4861bcec-1）
    - 现象：code_assistant-3 跑 23m42s 完成 Tower._paint 重写（node --check 通过、验收证据齐全），15:32:39 被判 `[failure kind=unverified retryable=false]`，唯一原因 = judge LLM 返 `invalid JSON response`（日志 10816 行）；炮塔领域 Agent 随后自证 + 重派 code_assistant-4 再跑 ~13 分钟，同一文件重复劳动放大 ~25 分钟。
    - 根因（系统性非偶发）：`backend/internal/agent/engine.go:159` `reflect()` 对 judge 原始响应直接 `json.Unmarshal`——不剥 ```json markdown 围栏、不提取首个 JSON 对象、不重试。judge = prompt_reviewer（`config.yaml:60 judge_role`，deepseek-v4-flash）返回带围栏或夹带文字即必然失败；该模型组合下每次 reflection rubric 校验都会失败，#43 fail-closed 从"防放水"退化为"必然误判"。
    - 设计张力：`roles.yaml:222` 明确记载 verifyloop 自动验证 2026-08-08 因 A/B 实证负资产下线；#43 fail-closed judge 实质把同类机制请回。修复须解析健壮化与降级策略一起给——只修解析仍留"judge 真不可用（端点挂）时必然误杀 + 重派"的放大通道。
    - 方向（两步，1 必做 2 推荐）：
      1. 解析健壮化：剥 markdown 围栏 + 提取首个平衡 `{...}` 块再 Unmarshal；解析失败/LLM 报错重试 1 次（换更严"只输出 JSON"措辞）后才判 `ErrJudgeUnavailable`。
      2. 降级策略：judge 重试后仍不可用时，若 L0 可执行证据（node --check 等 Success，`RecentVerificationOutputs` 非空）存在 → 降级 pass-with-warning（`VerifyNote` 标注"L0 通过，L2 judge 不可用"，成功摘要前缀如实体现），不再 unverified-FAIL 触发重派；无客观证据时维持 fail-closed（#43"绝不静默放行"的边界 = 无证据场景，而非无条件）。
    - 执行流程：`engine.go` 加 extractJSON 辅助（剥围栏/找平衡块）+ reflect 内 1 次重试；`ReflectEngine.Run` 的 err 分支查证据非空则置 VerifyNote 降级放行；dispatcher 不动（unverified 仅剩真正无证据场景）。
    - 测试：extractJSON 变体单测（裸 JSON / ```json 围栏 / 前后夹带文字 / 截断坏 JSON）；reflect 重试后成功；重试仍败 + 有 L0 证据 → 降级 pass 带标注；无证据 → 仍 unverified。
    - 验收：mock judge 返围栏 JSON 不再误判；judge 真不可用且有 node --check 证据时子 Agent 判通过带标注、不触发重派；双模块 `go test ./...` 绿。
    - 不做：不恢复 verifyloop 派验证 Agent（负资产结论不变）；不动 #43 L0/L2 分层与 verify_kind 路由；不删无证据场景的 fail-closed。
    - 落地（任务 65，2026-08-17，详见 `doc/变更.md`）：
      - `engine.go` 新增 `extractJSON`（剥围栏 + 首个平衡 `{...}` 块提取，字符串内花括号感知）；`reflect()` 拆出 `judgeOnce`，失败重试 1 次（更严"只输出 JSON"措辞）后才判 `ErrJudgeUnavailable`。
      - `Run` err 分支降级：`HasExecutableVerification` 有 L0 证据 -> `VerifyNote="L0 通过，L2 judge 不可用（降级放行）"` 放行，不触发重派；无证据维持 fail-closed。dispatcher 零改动。
      - 测试：`TestExtractJSON` 8 变体、围栏 JSON 首答即过、重试恢复、L0 证据降级放行（新 verifyThenAnswerProvider 构造 RunCommand 证据历史）；`LLMErrorFailClosed` 适配两次报错语义。`agent`/`subagent` 全量绿。

55. **UI 增强类任务工具路由：图像生成可见性 + ui_preview 视觉验证闭环 + MetaAgent 美术路线决策**  ← 来源：同上归因（日志 8.1MB / LLM 调用 136 次 / tool_mount 实际调用 0 次 / `od_image_*` 全日志 0 次）
    - 现象（三重失配）：
      1. 权限天花板挡路：`plugins.yaml:125` ui_design roles=["ui_assistant"]，MetaAgent 派 role=domain，domain 的 tool_catalog 只有 ui_preview（日志 2542 行），od_image_generate 连目录都进不去。
      2. 可见插件零使用：ui_preview 对 meta/domain/ui_assistant 全可见（`plugins.yaml:158`），但 tool_mount 实际调用 0 次（296 次出现全是 catalog 文本；8 次 browser_* 同为目录文本）——近千行 canvas 绘制代码全程盲写、零视觉验证。
      3. 现状描述被硬化为禁令：用户任务文本"这是一个纯 Canvas 绘制的塔防游戏…都是程序化绘制，不是贴图"（日志 608 行）被 MetaAgent 写成契约"纯 canvas 程序化绘制，禁止引入外部图片/字体/base64 资源"（1576 行）——即使插件全放开，按此契约仍只能手绘。
    - 耗时机理：glm-5.3 实测 ~43 tok/s（8371 tokens / 194.8s），手绘路线每个渐变/描边/阴影逐 token 生成，单轮 5-20K 输出 = 2-13 分钟；任务 14:57 派发，至分析时（16:27）仍在跑（90 分钟未结）。Seedream 出图 10-60s/张，混合路线（AI 贴图 + canvas 动画/叠加）从根上避开 token 黑洞。
    - 方向（三块）：
      1. 可见性：plugins.yaml ui_design roles 加 meta（MetaAgent 看得见才会纳入派发考量）与 domain（领域 Agent 可直接出图）；同步钉死 od-artifacts 落盘目录与 HTML 引用路径约定。
      2. 视觉验证闭环：roles.yaml domain_agent system_prompt【工作模式】加纪律——UI/canvas/游戏绘制类改动后挂载 ui_preview 截图回显验证（截图不传 filename 才回 image content，见 plugins.yaml 注释），看图迭代，禁止全程盲写；MetaAgent 整品验收对可运行页面同样先截图再判。
      3. MetaAgent 路线决策：meta_agent system_prompt 加一条——UI 增强/游戏美术类任务派发前在 spec 显式钉死美术路线（纯程序化 canvas / AI 贴图 + canvas 混合）；禁止把"项目现状无图片资源"自动推断为"禁止引入图片资源"（现状 ≠ 约束）；贴图路线须声明资源加载层改动范围（接受 game.js 不再零改动）。
    - 执行流程：plugins.yaml roles + 注释更新；roles.yaml meta/domain prompt 各加 1 条；重启后端或 POST /api/plugins/reload 生效。
    - 测试：tool_catalog 对 meta/domain 角色可见 od_image_generate/od_image_list（单测或真机）；重跑塔防怪物/炮塔 UI 增强任务验证闭环。
    - 验收：同一塔防 UI 增强任务重跑——spec 显式钉死美术路线；日志可见 browser_take_screenshot 实际调用（≥1 次截图回显）；混合路线总墙钟对比 90 分钟基线显著下降（预期 < 30 分钟，待实证）。
    - 不做：不强制所有 UI 任务走贴图路线（纯程序化仍合法，由 spec 显式决策）；不改 tool_mount 按需挂载机制（#52 结论不变）；不对 ui_assistant 之外角色开放 computer_use（破坏性红线不变）。
    - 落地（任务 66，2026-08-17，详见 `doc/变更.md`）：
      - `plugins.yaml`：ui_design roles -> `["meta","domain","ui_assistant"]`；注释钉死落盘约定（`workspace/od-artifacts`，页面引用 `../od-artifacts/<file>`，ui_preview 挂 workspace 可达）。
      - `roles.yaml`：meta_agent【派发铁律】加美术路线决策条（spec 显式钉死纯程序化/混合；现状 ≠ 约束；贴图路线声明资源加载层改动）+【整品验收】加先截图再判 UI；domain_agent【工作模式】加第 6 条绘制类改动禁止盲写（ui_preview 截图回显看图迭代 + 素材优先 od_image_generate）。
      - 验证：`config`/`plugins`/`mcpbridge` 全量测试绿（纯配置变更零代码路径）。开放验收待真机：重启/`POST /api/plugins/reload` 后重跑塔防 UI 增强任务，看 spec 美术路线 + browser_take_screenshot 实调 + 墙钟对比 90 分钟基线。

56. **域完成机器校验（冒烟层）：dispatcher 自动跑语法/编译检查 + 【机器校验】证据段入完成摘要**（P0，校验三层方案第一层，先做）  ← 来源：2026-08-24 讨论——commit `95e024e` 已把整品验收切出默认流程（仅改 `config/roles.yaml` meta_agent 文案："不派验证类任务"），但当日 Fruit Fury 任务（`workspace/logs/tui/2026-08-24.log` ~17:13）meta 仍派发了只读集成验证 domain（goal="验证 Fruit Fury 三域代码的跨文件集成点一致性（只读检查，不修改）"）。根因：prompt 纪律是软约束——meta 自身被禁止读文件，要核对跨文件集成点只能派领域；dispatcher 侧无代码闸拦截（`isVerificationTask` 已在 ce43288 删除，仅 `dispatcher.go:1821` 残留注释）。困境：单独派验证领域白烧时间/token；领域自报"已 node --check 全绿"又无法证实是否真执行。
    - 设计（机器代跑校验，零 LLM 调用，证据可归因）：
      1. dispatcher 在子 Agent 完成收尾路径（`runSubAgent` 成功分支，`backend/internal/domain/subagent/dispatcher.go` ~1816-1821 附近）对 `spec.files` 中可校验文件自动派生并执行冒烟命令：`.js` → `node --check`；可扩展 `.go` → `gofmt -l`、`ts` → `tsc --noEmit`（按 workspace 可得工具链探测降级，无工具则跳过并标注）。毫秒级，零 meta 参与。
      2. 结果以【机器校验】段（命令 + 退出码 + 输出尾部 + "dispatcher 执行，非 agent 自述"标注）追加进完成摘要/mailbox/boardUpdate——meta 与上级 domain 拿到的证据天然可信，解决"不知道是否执行了"。
      3. 失败走既有 `verify_kind` 反馈重试通道打回责任 domain（复用 #43 校验分层路由，不新建通路）；重试耗尽按现有失败分级上抛。
    - 执行流程：dispatcher 完成路径加 `runSmokeChecks(spec.files)`（命令派生表 + 超时 + 输出截断）→ 结果拼进完成摘要 → 失败接 verify_kind 反馈 → `config/roles.yaml` domain_agent 文案同步（自证要求降级为"补充说明"，机器校验为准）。
    - 测试：单测——多文件派生命令正确、node 不存在时降级跳过、失败输出截断、失败触发 verify_kind 打回而非直接判死；集成测——mock 域写两 js（一语法错）→ 完成摘要含【机器校验】两段、错文件触发打回。
    - 验收：重跑 Fruit Fury/塔防类多域任务，meta 交付验收全程不派验证类 domain；完成摘要每条带 dispatcher 执行的【机器校验】段；故意植入语法错误时域被打回自修。
    - 不做：不做运行时/浏览器级验证（归 #58 例外通道）；不做语义正确性判断；不恢复 verifyloop 派验证 Agent（负资产结论不变）；不动 #43 L0/L2 分层与 #54 judge 降级。
    - 落地（任务 86，2026-08-24，详见 `doc/变更.md`）：
      - `subagent/smoke_check.go` 新增：扩展名→冒烟命令派生表（.js→`node --check`、.go→`gofmt -l`（输出非空=未格式化判失败）、.ts→`tsc --noEmit`），工具链 LookPath 探测降级（缺失跳过并标注），30s 超时 + 输出尾部 2000 rune 截断；命令执行器可注入（测试不依赖机器工具链）。
      - 校验对象收敛为 spec.files ∩ 本子 Agent 实际写入文件（偏离原文"spec.files 全量"：并行兄弟域中途写入共享文件时先完成方不被误打回，责任归属精确）。spec.files 全量清单经 WriteSpec frontmatter 新字段 `file_list` 保留（stat 失败的待创建文件也在列，冒烟检查在文件创建后仍能定位）。
      - `runSubAgentOnce` 成功路径挂冒烟：失败反馈重试 1 轮（复用 L0 反馈通道）→ 仍失败 `errSmokeFailed` → 新 FailureKind `smoke_failed` 打回父（不判死，复用 #43 路由）；通过时 `result.MachineCheck` 段追加进完成摘要（标注"dispatcher 自动执行（非 agent 自述）"）。
      - 测试：`smoke_check_test.go` 6 单测（派生/降级/截断/gofmt 语义/目标收敛/消息格式）+ `dispatcher_smoke_test.go` 3 集成测（错文件打回 smoke_failed / 全绿摘要含【机器校验】段 / 范围外写入跳过）。`agent`/`subagent`/`tool` 全量绿。
      - 观察点：真机多域任务完成摘要【机器校验】段生成率；部署环境 node/gofmt 工具链存在性（缺失时静默跳过，段内标注）。

57. **跨域契约静态校验：WriteSpec 加 contract 字段 + 兄弟域全完成后 dispatcher 跑契约检查器**（P1，校验三层方案第二层）  ← 来源：同上 2026-08-24 讨论——解决"所有领域完成后怎么校验各领域之间是否正确引用、是否协调"，且不再靠派一个验证 domain 去读所有文件。
    - 设计（契约前置显式化 + 静态检查，零 LLM）：
      1. WriteSpec 加结构化 `contract` JSON 字段（meta 派发时显式钉死）：跨域符号映射（symbol → 所在 file，如 `GameEngine.init` → `js/engine.js`）、DOM id 清单、script 加载顺序、跨域函数签名。契约即派发时的"引用协议"，从隐含约定变显式数据。
      2. dispatcher 在父节点下全部兄弟 domain 完成时跑静态契约检查器（regex/文本解析，不调 LLM）：契约符号在声明文件中存在、引用方文件含引用点、DOM id 在 HTML 中声明、script 顺序与契约一致。
      3. 失败按契约条目归属批量打回责任 domain（一次消息列全部违例，避免逐条往返）；通过结果同样以【机器校验】段入摘要供 meta 纸面对照。
    - 执行流程：WriteSpec 工具 schema 加 `contract` 字段 + 校验 → dispatcher 记录兄弟域完成集合 → 全完成触发 `runContractChecks(contracts, files)` → 违例按域分组走 verify_kind 打回。
    - 测试：单测——符号缺失/DOM id 未声明/script 顺序颠倒三类违例检出、契约为空时跳过、违例正确归属责任域；集成测——三域 mock 任务（HTML/引擎/逻辑）契约全过与缺符号打回两分支。
    - 验收：多域前端任务全完成后 dispatcher 输出跨域契约【机器校验】段；人为制造跨域引用错误（改一域函数名不改引用方）被检出并打回正确责任域。
    - 不做：不做真运行时集成测试（契约只保证静态一致性，语义协调仍靠例外通道）；不强制所有 spec 填 contract（单域/无跨域引用任务可空）；不做 AST 级精确解析（regex 够用，漏报优于复杂化）。
    - 落地（任务 87，2026-08-24，详见 `doc/变更.md`）：
      - `tool/contract.go` 新增四类契约条目（Symbols/DOMIDs/Scripts/Signatures）；`WriteSpec` 加 contract 字段（schema + map 入参 JSON 往返解析 + frontmatter yaml + body 人读段），空契约合法。
      - `subagent/contract_check.go` 静态检查器：符号匹配支持声明形态（`var GameEngine = { init: ... }` 逐段词边界匹配，非 AST）；DOM id 正则；script 顺序按 HTML 实际出现序核对；签名字面包含；文件缺失直接违例。违例按文件归属分组，一次消息列全部违例。
      - 触发点 `trackChildDone`：父 pending 归零（全部兄弟完成）→ 异步跑契约检查（归零后新一波派发已开始时跳过）→ mailbox 通知父：违例=`contract_violation` marker + 分组清单，通过=【机器校验】段。
      - spec 捕获：派发时 `recordParentSpec` 缓存 files+contract 进 `parentSpecs`（spec 会被 Layer 2 失效删除，完成时读不到——缓存是前提，会话级内存不持久化）。
      - 测试：`contract_check_test.go` 8 单测（五类违例/空契约跳过/违例分组/全过）+ 集成测（pending 归零触发打回与通过两分支）；`tool` 侧 contract 往返 + file_list 保留 2 测。全量绿。

58. **meta 验收口径改纸面信任【机器校验】+ 集成验证收敛为显式例外通道**（P1，校验三层方案第三层，prompt 侧）  ← 来源：同上——`95e024e` 只改文案未改信任结构，meta 手上无客观证据时"要验收只能派验证域"的结构性动机仍在（8-24 日志实证）。
    - 设计：
      1. `config/roles.yaml` meta_agent【交付验收】口径改：验收只信两类证据——【机器校验】段（dispatcher 执行）与 spec 验收标准纸面对照；子 Agent 自述"已测试通过/已 node --check"不计分。
      2. 重度验证（浏览器截图/ui_preview/真跑页面）保留为显式例外：meta 须在派发时声明理由（如 UI 视觉类 #55 场景），走保留的【集成验证任务模式】（`config/roles.yaml:165`）派发；默认不走。例外通道是"接入口"语义的正身，与 `95e024e` 决策不冲突。
      3. 兜底口径：spec 有可执行验收标准但摘要缺【机器校验】段 → 标"未验证"打回责任域补跑（走 #56 通道），而非派验证域代劳。
    - 执行流程：roles.yaml meta_agent 交付验收段改写（信任清单 + 例外声明格式 + 兜底口径）；domain_agent 侧同步"自述不计分"预期。
    - 测试：配置加载测试绿（纯 prompt 变更）；复盘点——重放 8-24 Fruit Fury 会话上下文，新 prompt 下 meta 不再生成验证类 call_sub_agent。
    - 验收：多域任务全程零验证类派发（日志 grep 佐证）；UI 视觉类任务 meta 仍能显式走例外通道截图验收。
    - 依赖：#56 落地后才有【机器校验】段可信，本项的信任清单才完整；#56 之前先落"自述不计分"半量口径亦可。
    - 不做：不删【集成验证任务模式】保留段（例外通道）；不引入代码闸硬拦验证类派发（除非 prompt 层再失效，届时单列项）。
    - 落地（任务 88，2026-08-24，详见 `doc/变更.md`）：
      - `roles.yaml` meta_agent【交付验收】改写：验收只信两类证据（【机器校验】段 + spec acceptance 纸面对照），子 Agent 自述"已测试通过/已 node --check"不计分；兜底口径（spec 有验收标准但摘要缺【机器校验】段 → 判"未验证"打回责任域补跑，不派验证域代劳）；重度验证收敛为显式例外通道（派发时声明理由走 domain【集成验证任务模式】，默认不走）。
      - meta【派发铁律】加 WriteSpec contract 字段纪律（与 WriteSharedMemory 契约同一协议两种载体：前者机器校验、后者注入人读）。
      - domain_agent 同步：收尾验收自述降级为"建设期自查"，交付证据以 dispatcher【机器校验】为准；【集成验证任务模式】标注为例外通道入口（meta 派发时显式声明理由才启用）。
      - 测试：`config`/`role` 加载测试绿（纯 prompt 变更零代码路径）。真机验收：重跑多域任务看零验证类派发 + 摘要含【机器校验】段（待观察）。
77. 测试工作流添加自动桌面点击测试，分多个测试，如第一个测试点击某个按钮查看反应，第二个测试滑动某个桌面，再点击某个按钮的一套业务流程。每次测试流程启动前会询问是否开始演示点击后则操控电脑进行演示。添加上下文自动压缩模式与手动管理，当前为自动压缩即到达某个上下文值就自动压缩，保持在这个值附近，手动管理则和其他工具一样自己手动管理上下文

## 已完成（已归档到 git 历史）

- **2026-08-25 落地 #67-76（任务 97-106，详见 `doc/变更.md`）——2026-08-25 水果忍者复原任务失败复盘十项**：
  - #67 runtime 实装：`Spec.Probes` + `agent.HasRuntimeProbeEvidence`（navigate+evaluate 成功 + console 回读无 error/severe）+ dispatcher runtime 分支（缺证据重试 1 轮 → delivered-unverified 黄态）；roles.yaml UI/游戏/交互类 runtime 从建议改必须。
  - #68 acceptance 结构化：`AcceptanceItem{text, evidence, layer}` 行内标记串编码（`Acceptance []string` 类型不变零破坏兼容）+ dispatcher `scoreAcceptance` 逐项计分（command/screenshot/probe/file 分派证据扫描，N/M 由 dispatcher 计算写入【机器校验】段，meta 只读不自算）。
  - #69 场景化截图：`Spec.Scenes` + `HasSceneEvidence`（shotFingerprint 内容去重，同图连拍计 1；navigate/evaluate→screenshot 时序邻接）+ `visualEvidenceCheck` 场景清单驱动判定。
  - #70 契约健壮化：`ValidateContractShape` 写时拦截全角/CJK 散文签名 + `signatureMatched` 行注释剥离重试 + `violationFingerprint` 会话级去重（首报打回/复验升级文案/全已知零打回）。
  - #71 JS 引用完整性：冒烟层第三档 `runJSRefChecks`——>300 行 .js/.html（含内联 script 提取）首选 `tsc --allowJs --checkJs` 硬判，无 tsc 回退正则轻量扫描只标存疑。
  - #72 循环守卫：WriteFile/EditFile 成功清零该路径连读计数（确认性复读放行）+ 长文件（>500 行）阈值 3→6 + 失败路径【遗产清单】段（成功写入清单 cap 30 + 看板在办步骤 + "可直接作为续建 spec 骨架素材"）。
  - #73 看板接力：`TakeoverFrom` 失败条目迁移留痕（"曾失败，由 X 接力完成"，Done 不迁）+ `call_sub_agent` takeover 参数 + Brief【接管提示】。
  - #74 spec 诊断：spec 槽位失效改 tombstone（`invalidated: ` 前缀，区分"被失效"与"从未写"）+ missing 报错列已有 spec key 清单 + 唯一 keyed spec 候选回退。
  - #75 baseline 强制：`Spec.Baseline` + `HasFidelityKeyword`（还原/复刻/仿制/对标等）→ 还原类需求无基线 WriteSpec 拒收 + 基线文件存在性校验 + acceptance layer: functional/quality 分层计分（quality 缺失不标绿）。
  - #76 meta 硬约束：domain 完成摘要缺【未验证项】段追加警告入【机器校验】（可观测不硬拒）+ `bumpDispatchGeneration` 派发代数（takeover 链贯通）：第 3 代注入【重写评估】强制段、第 4 代无【接力理由】拒派。
  - 观察点：runtime 探针证据覆盖率、同图连拍黄态出现频率、假违例推送复发（应趋零）、确认性复读误杀消失、接力熔断第 3 代出现率。
- **2026-08-25 落地 #59-66（任务 89-96，详见 `doc/变更.md`）——2026-08-24 高防植物大战僵尸复盘八项**：
  - #59 验收分层模板：`Spec.VerifyLevels`（existence/static/integration/runtime/visual），WriteSpec schema + frontmatter；dispatcher 集成层探针（`subagent/integration_check.go`：HTML script src 解析 + 入口 import 引用图 + 空壳提示）与视觉层证据强制（`agent.HasScreenshotEvidence`，缺截图 → `errVisualEvidenceMissing` → delivered-unverified 黄态）；roles.yaml 派发铁律/交付验收绑定任务类型强制层级。
  - #60 状态三态化：`orchestrator.StatusUnverified`（delivered-unverified）+ `Tree.FinishUnverified`；`board.TaskUnverified`/`BoardStatusDelivered` + `MarkUnverified`；dispatcher 失败路径按 kind 分流（verify_missing/unverified → 黄态，真失败仍红）；TUI `RoleStatusUnverified` 标黄。
  - #61 桩挂名：`ContractSymbol.Stub/Owner`；契约检查第 5 段孤儿桩检查（声明文件仍含占位标记即打回 owner）；派发铁律补桩纪律。
  - #62 契约语义化：`symbolFound` 末段大小写不敏感（属性访问路径）+ 非末段敏感；签名空白归一 + .ts 文件 tsc 编译仲裁（编译过 → "存疑"降级先质疑契约）；变更屏障（`recMtimesMatch`，capture 后文件变则跳过）。
  - #63 证据模板同源：`verificationCommandPatterns` 提取 + 扩词表（noemit/tsc /vite /npm run 等）；`VerificationEvidenceTemplate()` 同源注入任务尾部 + L0 重试消息。
  - #64 happy-path：`HasFallbackKeyword` + `HappyPathAcceptance` 自动追加（WriteSpec 检测降级/兜底关键词）。
  - #65 多 key spec：WriteSpec `key` 参数 → `<parentID>:spec:<domain>`；dispatcher `specKeyFor`/`parentSpecs` 按 (parent, domain) 键、`hasFreshSpec`/`buildSharedPrefix` 领域定向 + 遗留单键回退；顺手修 spec.go 预算文案 2000→3000/4000。
  - #66 meta 收尾集成验证默认化：交付验收 A 段并入集成层/视觉层机器校验 + 分层绑定；顺手修 roles.yaml isVerificationTask 漂移文案。
  - 观察点：verify_missing 复发率（应趋零）、契约误报率（tsc 仲裁后）、UI/游戏任务未验证黄态出现频率、多 key spec 隔离后兄弟互伤。

- **评测体系落地 + 首次基线**（2026-08-14）：`test/eval/`（build tag `eval`）真实 LLM 任务完成率评测——场景 YAML + checkpoint 判分（command/file/regex/tree/llm_judge）+ token/子Agent 指标聚合 + TheAgentCompany 式全量/部分分报告（`test/eval/runs/`）；环境隔离修复：`docker/docker-compose.test.yml` 独立端口（PG 55432/Redis 56380）+ 无固定容器名，fixture 维度对齐 migrations（768）。首基线 12 场景 full pass 91.7%、加权 0.979，详见 `doc/eval/baseline_2026-08-14.md`。后续加固（同日）：fixture 改"共享容器常驻 + 每测试独立 PG database/Redis 逻辑库"（Redis 开 1024 逻辑库），并行包 `go test ./...` 不再互相拆台；`TestFactExtractionFallback` 序列对齐现行后端（verify_kind 校验分层 + 派发后不阻塞）。开放项：verifyloop 场景判分口径修正（自动验证闭环已下线，改测 verify_kind 证据）、EVAL_RUNS=3 可靠性、SWE-bench 20 题切片（Phase 2）。
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

29. **派发执行模式选择：ReAct / Plan-and-Execute / Reflection Loop 三引擎**（✅ 已完成 2026-08-10，doc/变更.md 任务 15）
    - 背景：现行体制 kimi+K3 当 MetaAgent、claudecode+deepseek-v4-flash 当执行节点、deepseek 自检后再由 K3 复检——慢且成功率不高。根因之一：所有被派发 Agent 不分任务形态跑同一个裸 ReAct 循环（`agent/react_agent.go RunWithHistory`），简单任务白烧反思开销、复杂任务无规划直接上手翻车；"自检+复检"只靠提示词约定，无机制保证。目标：派发子 Agent 时可显式选择执行模式，MetaAgent 自身也可选模式。
    - 落地（与原方案差异见下）：`agent/engine.go`——`Engine` 接口 + `ReflectEngine`（产出后对照验收标准自检、不达标带反馈重试、轮数上限默认 2、fail-open）+ `PlanExecuteEngine`（先出步骤计划落 board TUI 可见 → 逐步执行 → 汇总终答、超步数截断、规划失败降级纯 ReAct）；`ReActAgent.Run` 即默认 ReAct 引擎（省略 mode 零行为变化）。dispatcher `call_sub_agent`/`call_sub_agents` 加 `mode` 枚举（非法值拒绝）穿透到引擎；配置 `reflection_max_rounds`/`plan_execute_max_steps`；roles.yaml meta【执行模式选择】规程；bootstrap `WithEngineConfig` 接线。
    - **差异一**：TODO 原写"Reflection Loop = ReAct 产出后套'对照验收标准自检'外壳，轻量调用走 `CallLightweightWithRetry`"——落地改为从模型工厂取**同角色 provider** 适配（`NewEngineLLM`），复用 dispatcher 既有 `GetBladesProvider` 通路（dispatcher 层不引入对 model 包的依赖，测试可注入 mock）；轻量模型仍可用于未来换低成本端点。
    - **差异二**：TODO 原写 Plan-and-Execute"失败时 replan"——落地为"逐步执行 + 超步数截断 + 全部完成后汇总终答"，**未做失败 replan**（单步失败即整体失败回传父 Agent，与 #23 结构化失败+自动重派机制衔接，避免重规划环路无兜底）；replan 列为后续项。
    - 未做（TODO 明确不做）：自动模式学习/路由；MetaAgent 默认不启用非 ReAct 模式（meta prompt 规程按任务复杂度选模式）。
    - 已知语义弱化（注释留痕）：token 预算按步/按轮重置（各 RunWithHistory 独立计 budget），总成本由 sub_agent_timeout 墙钟兜底。
    - 可行性：
      - 三种模式共享同一 Agent 骨架（LLM 调用、工具派发、历史/记忆管理全部复用 ReActAgent 既有件），差异只在"循环策略"一层——抽 `Engine` 接口即可插拔，不动中间件链（#19 另线）、记忆层、dispatcher 主逻辑，风险可控。
      - ReAct = 现有循环原样适配为默认引擎，省略 mode = 零行为变化。
      - Reflection Loop = ReAct 产出后套"对照验收标准自检 → 不达标带反馈重试"外壳，轻量调用走 `CallLightweightWithRetry` 既有范式，每轮成本 ≈ +1 次轻量 LLM 调用，轮数硬上限兜底。
      - Plan-and-Execute = 先出步骤计划再逐步执行、失败重规划；#22 已接线的 board.TaskBoard（write_plan / 依赖门 / 状态回写 / TUI 面板）全是现成底座，计划落板即可见。
      - MetaAgent 侧：meta 本身也是 ReActAgent，同一 Engine 接口直接适用；但 meta 的价值在派发决策质量，建议默认 ReAct，把"按任务复杂度为每次派发选模式"写成 prompt 规程，不给 meta 套重壳。
    - 执行流程：
      1. Engine 抽象（`backend/internal/agent/` 新增）：`Engine` 接口统一执行入口，现有 `RunWithHistory` 主循环适配为 ReAct 引擎（默认）；构造子 Agent 处按 mode 选引擎。
      2. ReflectEngine：包装 ReAct 循环，产出后对照任务验收标准自检，不达标带反馈重试；`config.go` 加 `reflection_max_rounds`（默认 2）。
      3. PlanExecuteEngine：LLM 生成步骤计划（落 board，TUI 可见）→ 逐步执行 → 失败时 replan；`config.go` 加 `plan_execute_max_steps`。
      4. dispatcher 穿线：`call_sub_agent` 链路加 mode 字段穿透到子 Agent 构造；`builtin.go` schema 加 `mode` 枚举（`react` / `reflection` / `plan_execute`，默认 react）。
      5. bootstrap 接线 + roles.yaml：meta prompt 加【执行模式选择】段——琐碎/单步任务 react；正确性敏感任务 reflection；多步骤长任务 plan_execute；MetaAgent 自身默认 react。
      6. 测试（TDD）：fake LLM 下三引擎行为单测（reflection 不达标带反馈重试 / plan_execute 先写计划再逐步执行 / react 零变化）；mode 穿透 dispatch 断言；schema 校验；双模块 `go test ./...` 绿。
    - 验收：TUI 塔防回归中 MetaAgent 对多文件 codegen 任务选 plan_execute 或 reflection 并成功交付；省略 mode 的旧会话行为完全不变。
    - 不做：不做自动模式学习/路由（先人工指定 + meta prompt 规程，样本够再议）；不动 #19 中间件另线与 #21 verifyloop 归档结论；MetaAgent 默认不启用非 ReAct 模式。

2. **TUI Agent 树运行时构建**（✅ 已完成 2026-08-10：TUI 侧此前完成；唯一剩余开放动作 ExecuteChild 入树由 doc/变更.md 任务 8 吸收落地）
   - 当前状态：`internal/domain/orchestrator/tree.go` 权威树 struct 已落地（commit `2e97eff`）,PG 持久化已落地（commit `5a1134d`）。Dispatcher 派发时 Register/SetCancel/Finish,HTTP 暴露 `GET /api/sessions/{id}/tree` + `POST /api/sessions/{id}/agents/{aid}/cancel`。`ReactService.ListAgents` 仍返回单 MetaAgent,但 `Tree()` 方法返回权威树快照。
   - TUI 迁移（本次完成）：`agent_tree_panel.rebuild` 与 `view.go` 任务面板改读 `agent.Tree()` 替代旧事件流派生 `deriveSubAgentNodes`。新增 `orchestratorNodesToTreeNodes`（按 ParentID 链算 depth + 状态映射 Running->Active/Done->Done/Failed->Error/Cancelled->Done）。`deriveSubAgentNodes` + 其测试已删（树是单一真相源,事件流派生有竞态/遗漏;树已 PG 持久化重启 lazy 恢复,无需 fallback）。
   - 开放动作：`verifyloop.ExecuteChild` 同步路径入树（phase 2）。


38. **TODO #30-37 实现期发现并修复的缺陷记录**（✅ 均已修复 2026-08-10，doc/变更.md 任务 21-22 附带落地）
    - **软停止收尾 SaveMessages 用已取消 ctx 致 "no persisted messages"**（e2e 实证）：#37 的软停止 Pause 分支在 `context.Canceled` 收尾处调用 `msgStore.SaveMessages(ctx, ...)`——此时子 Agent 的 subAgentCtx 已被 `Tree.StopRunning` 取消，PG 写入必然失败，domain 虽落 Paused 但 resume 时无历史可加载（"恢复暂停领域 Agent 失败: no persisted messages"）。修复：`context.WithoutCancel(ctx)` + 10s 超时。教训：**在 cancel 收尾分支做任何持久化，必须显式脱离取消传播**。
    - **父 wait loop 30s 才检测到 Paused 子节点，PausedOnChild 转换被拖慢**：软停止让 domain 落 Paused 后，MetaAgent 的终结保护 wait loop 阻塞在 `WaitForAnyChild(30s)` 内，要等满一个周期才走到 `HasPausedChild` 检查。修复：dispatcher 新增 `pokeParent`（`ps.notify <- struct{}{}`，不改计数），Pause 后立即唤醒父。教训：**wait loop 类阻塞路径的新状态转换，需显式唤醒信号，不能依赖超时周期兜底**。
    - **`retryStreamGenerate` 首轮错误残留致成功轮被跳过**：#33 流式重试实现中 `lastErr` 未在每次 attempt 开头重置，首轮失败后第二轮成功也被 `lastErr != nil` 短路判为失败（单测 `TestRetryStreamGenerate_ErrorRetries` 1.5s 三次尝试全"失败"暴露）。修复：attempt 循环内 `lastErr = nil` 逐轮重置。教训：**重试循环的成功判定必须逐轮重置错误状态变量**（`retryGenerate` 非流式版无此问题因其在成功路径直接 return）。
    - 另注（非缺陷，测试基建经验）：mock LLM FIFO 是全局队列——多 domain 的保活调用会与 meta 的下一条 FIFO 响应竞争吃掉派发序列（软停止 e2e 因此收敛为单 domain）；且 mock 永远返回工具调用时 meta 不会进入终结保护 wait loop（该分支在「无 tool_calls」时才会到达），e2e 需在 FIFO 末尾放纯文本响应。

### 本次归档（TODO #30-38：2026-08-10/11 事故修复批次，变更.md 任务 16-23 及 commit 8893b44 批次）

30. **spec 新鲜度校验对活体文件永久 stale 的陷阱修复**（✅ 已完成 2026-08-10，commit 8893b44 批次落地，doc/变更.md 未单列；来源：2026-08-10 塔防会话事故，`workspace/tower-defense/logs/tui/2026-08-10.log`）
    - 事故还原：用户要求 MetaAgent 自检"为什么首次执行总失败"。MetaAgent 派发日志分析任务三连败，第三败为 `spec missing or stale`——该 spec 的 `files` 含 `logs/tui/2026-08-10.log`（被分析对象），`hasFreshSpec`（`dispatcher.go:1972`）经 `verifyFileMtimes`（`dispatcher.go:2235`）逐文件比对 mtime，而**系统自身持续往该日志写入**（含 WriteSpec 执行本身产生的日志行），spec 写完几秒内必然 stale。结论："分析活体日志/任何持续增长文件"的任务在机制上永远派发不出去。与 #12 的 WriteFile 失效机制同根但更难：那条是"别的 Agent 改文件"，这条是"系统自己改"。
    - 执行流程：
      1. `verifyFileMtimes` 对 `logs/`、`.bma/` 目录及持续增长文件豁免 mtime 检查（降级为存在性检查），或 WriteSpec 记录这类文件时改记"读取偏移/快照"而非 mtime。
      2. stale 错误文案区分 missing / stale 并**列出失配文件路径**（当前文案不区分，LLM 只能瞎猜重写 spec，白烧一轮）。
      3. 防御纵深：dispatcher 侧检测"spec.files 含日志目录文件"时在 WriteSpec 返回里直接警告，让模型当场改 files 而不是派发时才炸。
    - 测试：spec.files 含一个被持续追加的文件 → WriteSpec 后立即 call_sub_agent 成功；含被 WriteFile 修改的普通源码文件 → 仍正确判 stale（#12 一致性语义不回归）。
    - 验收：TUI 中让 MetaAgent"分析当前会话日志找失败原因"能一次派发成功。
    - 不做：不改 spec 强制门开关决策（#26-1 已定为开启）。

31. **SearchInFiles 零命中误杀：空结果语义修正 + 字面匹配声明**（✅ 已完成 2026-08-10，commit 8893b44 批次落地，doc/变更.md 未单列；同上事故）
    - 事故还原：全日志 85 次 SearchInFiles"失败"。根因叠加：实现是 `strings.Contains` 字面子串匹配（`builtin.go:395`），模型按 grep 习惯写 `a|b` 交替模式必零命中；而零命中被标 `success=false`（`builtin.go:414-417` 仅 `len(lines)>0` 才成功）→ 计入循环守卫连杀（`registry.go:489`，阈值 3）→ domain-1（游戏配置）15:44:12 被 LoopExit 强杀，17 分钟工作只剩 500 字符截断。"查无此物"是有效信息，不是失败。
    - 执行流程：
      1. 零命中改 `Success=true` + Output 为"（无匹配：pattern 未在任何文件命中）"类提示文案。
      2. 工具描述（`registry.go:752` 注册处）明示："字面文本匹配（大小写不敏感），不支持正则与 `|` 交替；多关键词请拆多次搜索"。
      3. 可选增强：pattern 含 `|` 时按 `|` 切分逐个 Contains 合并结果（贴近模型直觉，消除最大踩坑点）。
    - 测试：不存在 pattern 返 success=true 且计数值不增；含 `|` pattern 按决策断言行为。
    - 验收：塔防回归中不再出现 SearchInFiles 三连杀；日志中 SearchInFiles success=false 清零（除沙箱拒绝等真错误）。
    - 联动：若保留任何"空结果计失败"的口径，必须先落 #32 的失败分级，否则换汤不换药。

32. **循环守卫分级：校验类拒绝不计连杀 + LoopExit 从"终止"改"可恢复暂停"**（✅ 已完成 2026-08-10，doc/变更.md 任务 18；项 4 LoopExit 落盘并入 #35）
    - 事故还原：MetaAgent 自检任务 call_sub_agent 三连败（task too long → role_id and task are required → spec stale）**全是可纠正的参数/前置校验拒绝，模型每次都在正确纠偏**，但第 3 次直接 `ErrLoopExit` 终止整个 goal（用户视角的"强行停止"）。同理 domain-8（整品验收）探索预算耗尽被杀时正在接近答案。当前 `registry.go:489` 对 `result.Success==false` 一刀切计数。
    - 执行流程：
      1. 失败分级：`tool.Result` 加机读类别（validation_rejected / execution_failed / empty_result），dispatcher 校验拒绝（task too long / spec stale / role_id required / responsibility required）打 validation_rejected。
      2. 连杀计数只计 execution_failed；validation_rejected 单独计数（阈值更高，如 5）且提示文案明确"这是校验拒绝，修正参数即可，不计入失败"。
      3. MetaAgent 的 call_sub_agent 豁免连杀终止（纠偏循环是编排者的正常工作方式）；或 LoopExit 对 meta 语义改为暂停等用户（复用 #15 `pauseSession`/`awaiting_clarify` 通道）。
      4. LoopExit 触发时把当前 goal/看板状态落盘（与 #35 检查点联动），为续跑留锚点。
    - 测试：连续 3 次校验拒绝 → 不终止 goal；连续 3 次执行失败 → 仍终止；meta 暂停后可经用户消息恢复。
    - 验收：重放自检场景，MetaAgent 纠偏后第 4 次派发能成功；domain-8 类场景被杀后可恢复而非归零。
    - 不做：不取消探索预算守卫本身（#20 实证必需）；不动三层守卫的执行失败阈值（3 次维持现状）。

33. **轻量模型链路全线失效修复（打捞/摘要/提取残废）**（✅ 已完成 2026-08-10，doc/变更.md 任务 19）
    - 事故还原：日志中轻量调用全部落到 glm-5.2（思考型模型）且走**非流式** POST，被方舟 coding 端点拒绝："streaming is required for operations that may take longer than 10 minutes"（5 组 `Generate exhausted retries`）。后果链：① 失败打捞 `salvageLLMTimeout=5s` 硬编码（`salvage.go:30`），思考型模型来不及出首 token → domain-1/2 打捞 `facts=0`，失败经验只剩末条截断文本；② 事件摘要两次失败降级 raw join（`pipeline.go:257`），MetaAgent 上下文膨胀；③ 块记忆事实提取未见成功日志，走 `saveRawBlockMemory` 原文截断降级——块记忆"能用"但只是原文片段级。**注意**：当前 `roles.yaml` 配 `lightweight_model.model=deepseek-v4-flash`，但运行时重试日志 `provider=glm-5.2`（=cfg.Model）——roles.yaml 于 17:08、tui.exe 于 17:09 被修改/重建（均在事故会话之后），会话期生效配置与磁盘现值不一致，需先查加载链。
    - 执行流程：
      1. 排查运行时配置加载：启动日志打印 lightweight 解析结果（provider/model/base_url 来源：lightweight_model 直配还是回退 domain）；确认 TUI 启动 CWD 与 `config/roles.yaml` 相对路径解析（日志落在 workspace 下，需确认配置从哪读）。
      2. 轻量调用改流式（`NewStreaming` 累积收集）或换支持非流式的快模型端点；在 `CallLightweightWithRetry`（`factory.go:412`）注释固化决策。
      3. `salvageLLMTimeout` 配置化（默认 ≥30s；思考型模型场景下限 60s），超时降级保留。
      4. 事件摘要失败的 raw join 加总长度截断（防降级路径反而吹爆上下文）。
    - 测试：mock 轻量模型验证 salvage facts>0；摘要失败降级输出有截断上限。
    - 验收：人为杀掉一个 domain，父 mailbox 失败消息的【失败打捞】段非空且含已读文件清单+卡点。
    - 不做：不动 embed pseudo 决策（#26-5 已单列）；不改打捞"不进块记忆召回"的既有结论（#20）。

34. **记忆压缩机制评估：保留首条 user + 最近 N 条 + 中段压缩 —— 结论与改进方向**（✅ 已完成 2026-08-10，doc/变更.md 任务 17；纯分析项，代码改进并入 #35）
    - 结论（对照 `pipeline.go` 源码逐条核实）：**方向合理、粒度太粗、状态未与历史分离**。
      - 保留的合理部分实证成立：保头保尾压中段（首条 user 防目标失忆，16:13:10 MetaAgent 仍记得原始任务）；冻结视图保 DeepSeek 前缀缓存（#16 变更记录已实证缓存杀手修复）；机械压缩零 LLM 依赖（#33 未修前是唯一可靠路径）。
      - 不合理的部分（均为 #35 吸收点）：① 中段 200 字符/条是纯截断不是摘要——ReadFile 几千行只留 200 字符≈全丢，形成"压缩丢内容→重读→被杀"死亡组合（15:28:36 domain 重读 config.js 实证）；② 只保第一条 user，多轮会话第二条指令（自检/"重新执行"）进中段被压成 200 字符——"重新执行"被误解的直接推手（#35 根因 1）；③ 编排状态（哪些领域完成/结论/失败原因）散落在 assistant 消息里被压扁，无结构化保护（#35 看板注入补齐）；④ 按步数而非 token 触发，单步 token 方差大时粒度太粗。
    - 改进归并 #35 落地：状态（看板/完成度/spec 指针）走结构化注入永远新鲜不可压缩；历史对话才压缩；所有 user 消息保前 500 字符（指令语义不可压）。
    - 执行流程（本项仅分析，无代码改动）：
      1. 通读 `pipeline.go` compressMiddle/compressedView/buildCompressedView/injectEvents 全部压缩路径，逐条对照 TODO 事故记录核实结论。
      2. 结论落档（本条目 + doc/变更.md 任务 17），改进点全部并入 #35 执行。
    - 原机制细节（备查，`pipeline.go compressMiddle:396-454` + `compressedView:162-191`）：每 `summarize_every=10` 步触发；system 前缀 + 首条 user 原样保留；中段每条消息压成 `[role] 前 200 字符`（tool 结果全文只留 200 字符、tool_calls 只记个数）；最近 `summarize_keep_recent=10` 条原样保留；压缩视图冻结（compressState）保前缀缓存。

35. **续跑机制：任务检查点 + 看板上下文注入 + 恢复路由扩展 + 会话级恢复**（✅ 已完成 2026-08-10：Phase 0 看板注入 + 压缩保护落地；Phase 1 由 #32/#37 吸收覆盖；Phase 2 部分落地、缺口如实列开放项，doc/变更.md 任务 22）
    - 机制分析（2026-08-10 实证）："重新执行"后全量重编排**不是失忆**——MetaAgent 上下文完好（首条 user + 近期事件都在，in=19091），它明确引用"上一轮 4 领域完成并通过验收"。重跑根因有四：
      1. **指令歧义**："重新执行"无宾语，用户指"续跑被强杀的自检"，模型锚定到上下文中**最完整的信息**=首条 user 的原始任务全文（自检任务的相关消息已被压成 200 字符片段，无结构化残留）。
      2. **无机器可读的"未完成事项"锚点**：loop guard 杀掉自检任务后 goal 直接终止，无待恢复标记；`board.TaskBoard`（#22 已落地 write_plan/依赖门/状态回写）只喂 TUI 面板，**不进 LLM 上下文**。
      3. **无续跑原语**：MetaAgent 唯一手段是派发新 Agent，"从未完成项继续"无机制承载。
      4. **进程重启清零**：16:13:47 TUI 重启 + `restore_sessions: false`（`config.yaml:77`，注释明写"启动即为全新会话"）+ #15 D2（进程重启不恢复 paused session）→ 真正彻底的失忆点。
    - 设计（四层，尽量复用既有件）：
      1. **任务检查点（状态结构化）**：复用 `board.TaskBoard`——领域派发/完成/失败时回写看板（子任务 id/标题/状态/失败原因/交付物文件清单/spec key），持久化（复用 `agent_events` 或新表；#3 的写侧已通）。
      2. **看板注入 LLM 上下文（消歧关键）**：Assemble（或 #19 的 `Memory()` 中间件）在【近期事件】旁注入【任务看板】段——每轮最新、压缩不可达。"重新执行"时模型看到机器可读的未完成项，歧义自然消解。
      3. **恢复路由扩展**：#15 已有"PausedOnChild 态任意消息恢复 earliest paused domain"——扩展覆盖：loop guard/探索预算/连杀从 ErrLoopExit 终止改 Pause（与 #32 联动）使被杀任务进可恢复集合；"重新执行/继续"类消息先匹配"最近被杀/暂停任务"再考虑新 goal；恢复时给 MetaAgent 注入失败原因 + 打捞摘要槽位（#20 已有 `<parentID>:salvage:<domain>`）。
      4. **会话级恢复**：TUI `restore_sessions` 打开或提供 `/resume`；落地 #3 开放动作（`agent_events` 读回内存）；解决 `.bma/shared` key 含 sessionID 前缀导致新会话看不到旧共享记忆的孤儿化（恢复时按 session 映射或重放关键槽位）。
    - 执行流程（分期）：
      1. Phase 0：看板状态注入 MetaAgent 上下文（只读、最小改动，立即消歧）。
      2. Phase 1：#32 LoopExit→Pause + 恢复路由覆盖被杀任务。
      3. Phase 2：看板持久化 + TUI `/resume` + agent_events 读回 + shared 槽位跨 session 映射。
    - 测试：模拟"领域 A 完成、B 被杀 → 用户说重新执行"→ MetaAgent 只重派 B 而非全量；进程重启后看板状态可恢复。
    - 验收：重放 2026-08-10 场景——自检任务被强杀后输入"重新执行"只续跑自检；TUI 重启后可恢复原会话上下文。
    - 不做：不做跨会话长期任务队列（dag 包另管）；不做失败自动重规划（#29 已列 replan 后续项）；不做多会话并行恢复。
36. **用户输入自动提示词补全：意图识别 + 消歧 + 结构化格式化**（✅ 已完成 2026-08-10：Phase 0 纯规则版落地；Phase 1 LLM 增强与 Phase 2 看板对齐随 #35 联动，doc/变更.md 任务 20）
    - 背景：用户输入是高频歧义源——"重新执行""继续""修一下"这类短指令没有宾语，MetaAgent 只能锚定上下文里最完整的信息（首条 user 原文），导致语义漂移甚至全量重跑。#35 的看板注入解决"模型看得见状态"，本项解决"用户说得清楚意图"，两者互补：看板是被动消歧，本项是主动消歧。
    - 设计（输入侧前置管线，三段式，全部失败可降级为原文直通）：
      1. **意图分类（规则优先，LLM 兜底）**：TUI 提交输入后先过本地规则——匹配"继续/重新执行/接着做"等续跑词、"停/取消"等控制词、"为什么/查一下"等诊断词，给出意图标签 + 置信度；规则拿不准且轻量模型可用时，用轻量模型分类（单轮、超时 ≤3s）。轻量模型挂了（#33 未修前是常态）直接走规则结果，**绝不阻塞用户输入**。
      2. **消歧绑定（核心）**：意图为"续跑/控制/诊断"时，把最近任务状态拼进提示词——数据来源是 `board.TaskBoard`（#22 已落地）+ 最近被杀/暂停任务记录（#35 Phase 1 后完备）。例：用户输入"重新执行"→ 补全为"继续执行最近被中断的任务：自检（失败原因：loop guard 三连败终止），不要重跑已完成的领域修复"。无绑定对象（无在看任务）时跳过消歧段，不编造。
      3. **结构化格式化**：把用户原话包进固定模板段——【用户原始指令】（原文逐字保留，防篡改）+【系统补全】（意图标签 + 绑定状态 + 建议解释），两段分离让 MetaAgent 能区分"用户说的"和"系统推断的"，补全错了用户能在下一轮纠正。
    - 边界与安全：
      - 只增不改：永不改写/替换用户原文，只做附加；模板固定、无自由生成，避免补全本身引入新幻觉。
      - 高歧义不猜：绑定期望对象有多个并列候选（如两个被杀任务）时，不替他选——走 #24 已落地的 ask_user 通道反问，或在【系统补全】里列出候选让 MetaAgent 自行 ask_user。
      - 可开关：config 加 `prompt_enhance: true/false`（默认开），TUI 状态栏显示本输入是否被补全过，方便排查"模型为什么这样理解"。
    - 执行流程：
      1. Phase 0（纯规则版）：TUI 提交路径加 enhancePrompt 预处理——意图规则表 + 从 session/内存看板取最近任务状态，拼模板注入；无 LLM 依赖，#33 修好前即可上线。
      2. Phase 1（LLM 增强）：规则置信度低时接轻量模型分类/改写建议（依赖 #33 修复）；超时/失败回退 Phase 0 结果。
      3. Phase 2（联动 #35）：看板注入 LLM 上下文落地后，补全段与看板段对齐同一数据源，消除"补全说的"与"模型看到的"不一致。
    - 测试：单测覆盖规则表（续跑词/控制词/普通任务输入不触发补全）；集成测"领域 A 完成、B 被杀 → 用户输入'重新执行'"→ MetaAgent 收到的消息含【系统补全】且锚定 B；轻量模型故障注入 → 输入不阻塞、直通原文+规则补全。
    - 验收：重放 2026-08-10 场景——自检被强杀后输入"重新执行"，MetaAgent 收到的提示词明确指向续跑自检；补全段不含任何用户原文之外的伪造指令；开关关闭后行为与现状一致。
    - 不做：不做自由式 LLM 改写用户提示词（幻觉风险大于收益）；不做多轮对话式澄清向导（ask_user 已够）；不做英文意图词表（当前用户群中文，规则表先中文）。
37. **任务软停止：双击 ESC 停止当前会话全部 domain/子助手 + 销毁倒计时 + 可续跑**（✅ 已完成 2026-08-10，doc/变更.md 任务 21）
    - 背景：现有停止手段全是硬销毁——Ctrl+C 退出进程、`/cancel` 走 `cascadeCancelTree`（`service_react.go:481`）把 Running/Paused 节点全标 Cancelled，history 不留恢复锚点，"停下来想想再决定"的场景无承载。而 #15 的 Pause/Resume 链路（SaveMessages 落 PG → Tree.Pause → ResumePaused 重建）已跑通"杀 goroutine、留 history、秒级重建"，软停止直接复用。
    - 已确认决策（2026-08-10 与用户逐项澄清）：
      1. **停止语义**：agent 与正常完成一样收尾退出（goroutine 释放、完整 history 落 PG），不做 goroutine 冻结；倒计时内可重建续跑，到期硬销毁。
      2. **倒计时默认 300s**，配置项 `stop_destroy_countdown_sec`。
      3. **续跑触发**：复用现有 PausedOnChild 路由——倒计时内任意消息恢复 earliest paused domain，不加新命令。
      4. **停止范围**：仅当前查看会话，不动其他 Running 会话。
    - 设计（四块，全部复用既有件，新增面集中在"收尾路径分流"）：
      1. **TUI 双击 ESC**：当前会话 Running 时，第一次按 ESC 进入 2s 武装窗（仿 `ctrlCQuit` 的 `quitArmedUntil` 模式，`model.go:677`），`flashMsg("再按一次 ESC 停止所有任务")`，**不清空输入栏**（覆盖 `input.go:30` 现有 ESC 行为仅在 Running 时）；窗内第二次 ESC → `POST /api/sessions/{id}/stop`。无 Running 会话时 ESC 行为与现状完全一致。
      2. **控制链路（核心改动）**：`ControlCommand` 新增 `ControlOpStop`；`ReactService` 给会话置"软停止中"标记 → 按 `cascadeCancelTree` 同款遍历 `Tree.Snapshot()` 取消 Running 节点 context → **分流点在 dispatcher 的 `context.Canceled` 收尾分支**：检测到停止标记则走 `SaveMessages + Tree.Pause("user stop")`（节点落 Paused），否则维持现有 Cancelled 语义。domain 全落 Paused 后 `PendingChildren>0` 触发既有父终结保护，MetaAgent 会话自然落入 `SessionStatusPausedOnChild`——恢复路由零改动生效。子助手（叶子）无 Pause 语义，走现有 errPartialReturn/salvage 路径记部分成果退出，domain 续跑后按需重派。
      3. **销毁倒计时**：会话级 `destroyAt = now + stop_destroy_countdown_sec`；backend 定时器到期 → 直接调既有 `cascadeCancelTree` 硬销毁（含 Paused 节点）；续跑触发时取消定时器。TUI 状态栏/会话行显示倒计时（100ms tick 刷新链路现成）。
      4. **续跑**：用户在倒计时内发任意消息 → 既有 `sendMessage` PausedOnChild 分支（`service_react.go:1636`）→ `findEarliestPausedDomain` → `ResumePaused` 重建 domain，同时取消销毁定时器、清除停止标记。
    - 边界与竞态（实现时必须处理）：
      - **停止中发消息**：软停止进行间（节点尚未全部落 Paused）用户发消息，消息入队等待，待会话落入 PausedOnChild 后按续跑路由处理，不得直接注入正在收尾的 MetaAgent。
      - **停止与正常完成竞态**：某 domain 在停止标记下发前自然完成→正常 trackChildDone，不参与 Pause；续跑时由 MetaAgent 按 mailbox 结果继续编排。
      - **倒计时 vs maxPausedResumes**：倒计时是时间维度销毁，与次数维度续跑上限（`dispatcher.go:1581`）独立；先触发哪个执行哪个。
      - **进程重启**：倒计时随进程消亡，PG 中 Paused 节点留存但不自动恢复（与 #15 D2 决策一致）；如需跨重启恢复走 #35 Phase 2。
    - 执行流程：
      1. Phase 0（后端核心）：`ControlOpStop` + 停止标记 + dispatcher 收尾分流 + HTTP `POST /api/sessions/{id}/stop`；单测覆盖"3 domain 运行 → stop → 全落 Paused + session PausedOnChild"。
      2. Phase 1（倒计时）：`stop_destroy_countdown_sec` 配置 + 会话级定时器 + 到期硬销毁 + 续跑取消；单测"到期全 Cancelled""续跑后定时器不再触发"。
      3. Phase 2（TUI）：双击 ESC 武装窗 + flashMsg + stop 调用 + 倒计时显示。
    - 测试：单测——停止标记下 cancel 收尾落 Paused 而非 Cancelled、叶子助手走 partial、倒计时到期/续跑取消两个定时器分支、武装窗超时 ESC 恢复清输入语义；集成测——派发 3 domain → 双击 ESC → 全 Paused → 发消息 → earliest domain 重建续跑、其余依次恢复；不发消息等 300s → 全 Cancelled。
    - 验收：塔防任务跑到一半双击 ESC，TUI 显示"已停止 N 个 Agent，MM:SS 后销毁"；输入任意消息任务从断点续跑（不重跑已完成 domain）；不操作到期后任务树全销毁且 TUI 有明确反馈。
    - 不做：不做 goroutine 真冻结（已决策假死语义）；不做全局停止（仅当前会话）；不做停止中的选择性保活（停哪留哪）；不做跨进程重启的倒计时持久化（归 #35 Phase 2）。

38. **首轮派发失败复盘（修复后重跑实证）：探索预算强杀 + RunCommand 连杀误杀 + task 超长拒绝 + 临时文件路径不可见**（✅ 已完成 2026-08-11，doc/变更.md 任务 23；来源：2026-08-10 17:22 重跑会话，`workspace/tower-defense/logs/tui/2026-08-10.log` 17:22 之后段，tui.exe 17:09 重建含 #30-#33 修复）
    - 背景（先还修复一个公道）：本轮**未再出现** `spec missing or stale`（#30 生效）、SearchInFiles 零命中误杀（#31 生效）、校验拒绝杀 goal（#32 meta 豁免生效——3 次 task too long 拒绝均未终止 MetaAgent）。修复对各自靶向失效模式有效；仍存的"第一轮失败、第二轮重派成功"由下面四个根因构成，其中根因 A 是 #32/#35 互相吸收后落空的遗留项。
    - 量化事故还原：MetaAgent 共派 9 个 domain，4 个第一轮被杀（domain-2/10/11/18，44%），死因同为"探索预算耗尽（domain 协调者上限 8 次，已调 8 次）"；叶子 code_assistant-5 死于"RunCommand 已连续失败 3 次"；另有 3 次 `task too long` 同步拒绝（3205/2201/2070 runes，上限 2000）。所有被杀领域都由 MetaAgent 第二轮以"task 内嵌精确行号+实现方案"重派才成功——**系统在用一整轮 Agent 生命换取 task 上下文精确化**。
    - 根因 A（主因，#32 落空项）：探索预算耗尽仍是一刀切 ErrLoopExit 强杀（`registry.go:434-444`）。#32 完成说明写"项 4 LoopExit 落盘并入 #35"，#35 完成说明写"Phase 1 由 #32/#37 吸收覆盖"——互相吸收，"LoopExit 终止改可恢复暂停"无人落地。两处设计矛盾：① 杀因文案"禁止再亲自探索，把剩余探索拆给叶子助手"是给**活人**看的改派指导，接收者却已被杀死，指导只能经打捞摘要间接绕到 MetaAgent；② 8 次预算与实际文件规模脱节——game.js 500+ 行、单次 ReadFile ≤300 行，读完两个文件即耗 6-7 次；4 个被杀 domain 的打捞文本均显示正在正常推进（"我来读取 buildLevelSelect 和 syncHud 部分""我现在理解了这个结构，draw(ctx) 在 L165…"），不是空转。
    - 根因 B：连杀计数器键只有工具名，不区分命令/错误内容（`registry.go:530-541`，`maxConsecutiveFailures=3`，仅同工具成功才重置）。code_assistant-5 三次失败原因各不相同且每次都在推进：① `node <工作目录>\verify-frost.js` 模块找不到（文件实际在 `.bma/tmp/<sid>/`，见根因 D）；② 改用 `$env:BMA_SESSION_TEMP_DIR` 自愈后命中脚本自身语法错误（:16:17）；③ `fix_syntax.js` 再次用工作目录相对路径找不到。三次失败间还隔着成功的 WriteFile。被杀时 tower.js 已写入 4769 bytes、正在验证——**正常的修复-验证循环被误判为无效重试死循环**。
    - 根因 C：task too long 硬拒绝（`dispatcher.go:978` maxTaskRunes=2000）。meta 豁免使其不再杀 goal，但每次拒绝白烧一整轮 MetaAgent LLM 往返（本轮首轮 llm done 耗时 2m28s）；2070/2201 这类轻微超限与"task 自包含（背景+目标+验收）"的要求天然冲突。
    - 根因 D（工具 UX，根因 B 的导火索）：WriteFile temporary=true 成功输出只有 "wrote N bytes"（`builtin.go:289`），`agent.ToolResult`（`react_types.go:89-94`）只含 tool/success/output/error——`Result.Path` 不到 LLM，Agent 不知道临时文件落在 `.bma/tmp/<sid>/`，首次运行必猜工作目录路径然后失败。ca-5 靠 RunCommand 工具描述里的 BMA_SESSION_TEMP_DIR 提示自愈，但已消耗 3 条连杀命中额度的第 1 条。（已实证：env 变量注入 `builtin.go:517` 工作正常，问题纯粹是写入结果不含落盘路径。）
    - 执行流程：
      1. ✅ **预算耗尽改软阻断**（根因 A 核心）：`checkExploreBudget` 返字符串拦截文案（不返 ErrLoopExit），Dispatch 软阻断路径返工具级错误 + `err=nil`（Agent 存活可立即 call_sub_agent 下放叶子/WriteFile 落地）；新增 `exploreBlockCount` 按 scopeKey 计数被拦调用，达 `exploreSoftBlockGrace=3` 后的下一次升级 ErrLoopExit。与 exploreCount 解耦--被拦调用不污染预算计数，WriteFile 升档/预算算术不受影响（事故根因 A ②“打捞文本均显示正在正常推进”由软阻断兜底：3 次软提示后再判死）。
      2. ✅ **连杀计数加错误指纹**（根因 B）：`failureCounter.counts` 键改为 `工具名+错误指纹` 复合键；`failureFingerprint` 对 RunCommand 取 `命令骨架 + stderr 首行`（`firstErrorLine` 优先 `[stderr]` 段），其余工具取 `name + 错误首行`，归一化（小写+空白折叠+200 字符截断）；指纹不同=新键从 1 重计。验证类命令（`isVerificationCommand`：含 `--check`/`lint`/`verify`/` test` 退出码即反馈）失败 `reset` 不计数。“完全相同调用连杀即终止”的真死循环检测保留。
      3. ✅ **task 超长软着陆**（根因 C）：`validateDispatchArgs` 返回 `(msg, warning)`；2000<n≤2600 放行附压缩警告（call_sub_agent 单/批量 Output 均拼警告）；>2600 硬拒。`WriteSpec` 成功 Output 追加“派发 task 预算 2000 字”提醒，把合规时机前移一轮。
      4. ✅ **temporary WriteFile 输出带落盘路径**（根因 D）：`writeFile` temporary=true 分支 Output 改为 `wrote N bytes to <absPath>（会话临时目录，运行用 $env:BMA_SESSION_TEMP_DIR\<文件名>）`，消除盲猜。
      5. ✅ 顺带核查：从 17:22 会话日志 27 条 call_sub_agent 调用中提取 20 个唯一 task，长度分布--19 个 ≤2000（559-1897 runes，覆盖全部“精确行号+实现方案”类第二轮成功派发）、1 个 2070（软着陆区，原本被拒白烧一整轮）、1 个 3205（全量规格转贴，>2600 仍硬拒正确）。阈值 2000/2600 与该类 task 自然长度无冲突。
    - 测试（全绿）：软阻断--`TestLoopGuard_ExploreBudget_SoftBlockThenEscalate`（超预算首次软阻断 err=nil；宽限内持续软阻断；超宽限升级 ErrLoopExit；echo 不计探索预算存活、cat 按探索计费升级）。连杀指纹--`TestLoopGuard_ConsecutiveFailures_FingerprintVarying`（10 次不同报错永不误杀）、`..._FingerprintResetOnSuccess`（同工具成功重置计数）、`..._RunCommandFingerprint`（同命令同报错×3 杀、骨架变化不杀）、`..._VerificationExempt`（`--check` 类连失败 5 次不触发）。task 软着陆--`TestValidateDispatchArgs_TaskSoftLanding`（2000 干净放行 / 2100 软着陆附警告 / 3000 硬拒）、`TestCallSubAgent_TaskSoftLandingSuccess`（Execute 端：2100 派发成功 Output 含警告，3000 校验拒绝）。temporary--`TestWriteFile_TemporaryOutputHasAbsPath`（Output 含绝对路径 + `$env:BMA_SESSION_TEMP_DIR` 提示，文件存在）。
    - 验收：阈值/守卫层修复完成；重跑实证（首轮派发存活率 5/9 -> 9/9、日志不再出现“探索预算耗尽”ErrLoopExit 与 RunCommand 误杀、task too long 拒绝数趋 0）待下次塔防任务运行验证。
    - 不做：不取消探索预算本身（#20 实证必需）；不取消 2600 字以上的全量规格转贴拦截；不改 #32 已落地的失败分级与 meta 豁免；不在本项做 LoopExit→Pause 恢复路由（若日后需要归 #35 Phase 2 一并设计）。

### 本次归档（TODO #39-41：2026-08-12 事故修复/缓存/本地向量批次，变更.md 任务 24-26）

39. **自动提示词补全误判根治：输入形态闸门 + 规则收紧 + 轻量模型仲裁（#36 Phase 1 重设计）**（✅ 已完成 2026-08-12，doc/变更.md 任务 24；来源：2026-08-11 事故——用户提交塔防新功能需求（粒子特效/怪物贴图/暂停菜单，数百字多行），暂停菜单按钮文案"1.继续游戏"被词表子串命中"继续"，意图误判为"续跑"并附加【系统补全】段）
    - 背景：#36 Phase 0 的 `classifyIntent`（`backend/internal/agent/prompt_enhance.go:62`）是纯 `strings.Contains` 子串匹配——不看命中位置、不看输入长度、不看语境、无置信度。词表 27 个词中"继续/暂停/看看/检查/分析"均为任务描述高频词，输入越长命中越必然（数百字规格书命中概率≈1）。本次事故输入是多行编号列表的新任务规格书，与"续跑"（恢复被中断任务）毫无语义关系，却被打上续跑标签并附加强引导建议（"优先继续最近失败/中断的任务"），直接带偏 MetaAgent。#33（轻量模型链路）已由 doc/变更.md 任务 19 修复，#36 预留的 Phase 1"LLM 兜底"解锁，本项一次性重设计并吸收该遗留。
    - 根因拆解：
      1. **无输入形态闸门**：续跑/控制/诊断本质是"短指令"（实证 ≤15 字："继续""重跑""停"），长文本根本不该进意图分类；现状是任意长度输入都过词表。
      2. **匹配模式过宽**：子串命中即判，"继续游戏"（名词性按钮文案）与"继续"（动词性指令）无法区分。
      3. **无置信度分级**：弱词句中命中与强词全句命中同等对待，输出同样的强引导建议。
      4. **意图与状态无交叉验证**：会话无失败/未完成任务时"续跑"意图本就存疑，现状照样输出续跑建议。
    - 设计（四层管线，保留 Phase 0 全部资产，全部失败可降级原文直通）：
      1. **L0 输入形态闸门（规则，零成本，本次核心）**：先判"像不像控制类短指令"，不像直接 IntentNone 直通、不进词表。规则：去空白后长度 > `prompt_enhance_max_input_runes`（默认 30 runes）→ 跳过；多行 / 含编号列表（`1.` `2.`）/ 含【】段标记 → 跳过（任务描述特征）。本条单独即可拦截本次事故输入（数百字多行）。
      2. **L1 规则收紧（保留词表，升级匹配模式）**：词表分级——强词（"续跑""重跑""重新执行""接着做"等会话控制专用语）允许句中命中；弱词（"继续""暂停""看看""检查"等日常高频词）仅**全句匹配**（去标点空白后整句即词）或**句首匹配**（"继续执行……"且全长 ≤30 runes）才命中。L1 高置信命中直接出结果，不调 LLM。
      3. **L2 轻量模型仲裁（仅灰区触发，#36 Phase 1 落地）**：L0 通过 + L1 未命中但句中存在弱词弱信号时，调 `ModelFactory.CallLightweightWithRetry`（`factory.go:417`）做四分类（none/resume/control/diagnose + confidence），prompt 固定模板、输出 JSON 用 `jsonutil.ExtractJSON` 容错解析。超时 `prompt_enhance_llm_timeout_sec`（默认 10s——当前轻量模型为推理系、首 token 慢，宁可短超时降级也不阻塞输入）；超时/解析失败/低置信一律 IntentNone。**宁漏判不误判**：漏判只是退回无补全的旧行为，误判是本次事故。
      4. **L3 输出层防护（误判损害兜底）**：【系统补全】模板首行追加"以下为自动分类推断，可能与原意不符；与【用户原始指令】语义无关时请整段忽略"；L2 仲裁命中的输出只给意图标签 + 状态绑定，不给"优先继续……"强引导建议；意图与状态交叉验证——resume 要求 FailedTasks/PendingTasks 非空、control 要求有在跑任务，无佐证降级为仅标签或 IntentNone。
    - 边界与安全（沿用 #36 全部承诺）：只增不改，原文逐字保留；全部失败路径（LLM 挂/超时/乱 JSON）降级原文直通，绝不阻塞用户输入（L2 最坏延迟 = 超时上限，且仅灰区短输入触发，正常输入零 LLM 调用）；模板固定无自由生成。
    - 配置：`agent.prompt_enhance`（已有总开关）下新增 `prompt_enhance_llm: true`（L2 开关）、`prompt_enhance_llm_timeout_sec: 10`、`prompt_enhance_max_input_runes: 30`；落 `config/config.yaml` + `backend/internal/config/config.go`。
    - 可观测性：判定路径打事件（gate_skip / rule_strong / rule_weak / llm_hit / llm_miss / llm_timeout / llm_error），复用 `addEvent` + `eventkind`，便于复盘误判率与 L2 调用率。
    - 执行流程：
      1. `prompt_enhance.go`：`classifyIntent` 重构为 L0 闸门 + L1 分级规则两层；新增 `IntentArbiter` 函数类型（依赖注入，便于测试 mock）；输出模板加忽略声明 + 置信度分级建议。
      2. `service_react.go`：`enhanceUserInput`（`service_react.go:173`）加 ctx 参数与 arbiter 调用；落地状态交叉验证；判定路径事件。
      3. `bootstrap.go`：装配 arbiter（复用 `CallLightweightWithRetry` + 超时，模式同 `newEventSummarizer`/`llmSalvageExtractor`）。
      4. 配置项落地 + `prompt_enhance_test.go` 扩充。
    - 测试：事故回归——完整事故输入（含"1.继续游戏"的数百字多行功能描述）→ 原文直通零附加；短指令不回归——"继续"→续跑绑定失败任务、"暂停"→控制、"为什么失败"→诊断；灰区 mock——L2 命中/超时/乱 JSON/低置信四分支；状态交叉验证——无失败任务时"继续"不给续跑强建议。
    - 验收：重放 2026-08-11 事故输入 → MetaAgent 收到原文且无【系统补全】段；"继续"类真续跑指令补全能力不退化（#36 验收场景重放通过）；L2 故障注入下输入不阻塞、直通原文。
    - 不做：不做自由式 LLM 改写用户提示词（沿用 #36 决策）；不做英文词表；不做多轮澄清向导（ask_user 已够）；不把 L0 长度闸门做成学习式分类器（规则已够用，实证调参即可）。

40. **DeepSeek 前缀缓存命中率优化：动态时间移出可缓存前缀 + provider 归一 + 命中率可观测**（✅ 已完成 2026-08-12，doc/变更.md 任务 25；来源：2026-08-11 用户反馈--DeepSeek 模型缓存命中率远低于 Claude Code 等单 Agent 工具）
    - 背景：DeepSeek V3+ 服务端自动前缀缓存（prefix cache）对"系统指令 + 历史前缀"做字节级匹配，命中省计费省延迟。单 Agent 工具（Claude Code 等）system prompt 跨轮字节稳定，命中率天然高。本系统已有压缩视图冻结（`pipeline.go compressStates:67-69`，两次压缩间前缀字节稳定）+ 近期事件/看板尾部注入（`pipeline.go:285-292` / `board_context.go:27`，尾部变化不破坏前缀）两套正确机制，但命中率仍低--根因在 system prompt 头部每轮重建 + provider 路由绕开原生缓存端点 + 命中率零可观测。
    - 根因拆解（逐条对照源码核实）：
      1. **动态时间戳注入 system Instruction 头部（主因）**：`react_agent.go:1004-1008` `now := time.Now()` 精确到秒，`:1019` 拼进 `buildEnvBlock` 返回的 envBlock，`:972` envBlock 拼进 `systemPrompt()` 返回值，`:358` `system := a.systemPrompt()` 作为 `blades.ModelRequest.Instruction`（`:370-388`）下发。`systemPrompt()` 每个 `RunWithHistory` 调一次（单轮内稳定），但**跨用户轮次**时间戳必然变化 -> system Instruction 字节级变化 -> DeepSeek 前缀缓存从时间戳位置往后全失效（system 是前缀最前段，等于整个 system + role + 执行纪律 + history 前缀全部 miss）。Claude Code 之所以命中率高，核心就是 system prompt 跨轮字节稳定。时间戳后面跟着的是 `base`（角色 system_prompt，最大稳定块）+【执行纪律】（固定文案），全被打断缓存，损失最大。
      2. **userProfileInjector 头部每次重读**：`react_agent.go:127-135` `Inject` 每次调 `p.current()` 重读画像文本并 `return "【用户画像】\n" + content + "\n\n" + systemPrompt`--画像在 system 最头部（persona.Inject 在 `:980-982` 包整个 prompt）。画像内容稳定时字节不变，但 (a) 重读有开销；(b) 画像被工具中途改写（#28 写入双路）则头部变化打碎整个前缀。当前画像在头部 = 把"可能变化的段"放在缓存前缀最前，最差位置。
      3. **PROJECT.md 每次重读**：`react_agent.go:1024` `project.LoadProjectDoc(workDir)` 每次 `systemPrompt()` 重读 .bma/PROJECT.md managed 区正文拼进 envBlock。文件稳定时字节不变，但 WriteFile 后去抖刷新（#18 `projectRefresher` 3s 去抖重写）会改内容 -> 下一个 `RunWithHistory` system 前缀变化 -> 缓存失效。与时间戳同位（envBlock 内），放大主因。
      4. **DeepSeek 角色绕开 openai-chat 原生端点**：`roles.yaml` 中 `deepseek-v4-flash` 经 `anthropic`（`:340, :471`）与 `openai-responses`（`:420, :516, :594, :641, :691`）provider 接入，无一处走 `openai-chat`。DeepSeek 自动前缀缓存在 `/chat/completions`（openai-chat）端点上最可靠且返 `prompt_cache_hit_tokens`/`prompt_cache_miss_tokens` 原生字段（`provider_openai_chat.go:252-253` 解析）；`openai-responses` 端点 DeepSeek 不支持 `prompt_cache_key`（`provider_openai_responses.go:30` 明注），缓存语义不明、`cached_tokens` 字段（`:259`）是否回填未实证；`anthropic` 端点原生需显式 `cache_control` 断点标记才缓存（本系统未设），DeepSeek-via-anthropic 网关行为不可靠。三条路都未实证命中。
      5. **命中率零可观测**：三个 provider 都解析了各自的 cache token 字段（openai-chat 的 hit/miss、openai-responses 的 cached_tokens、anthropic 的 CacheReadInputTokens/CacheCreationInputTokens），但仅还原进 `TokenUsage` 用于预算计数，**从未聚合/日志/展示命中率**。`llm_tracker.go`/`llm_tracker_stats.go` 只统计 token 总量，无 hit/miss 拆分。无法回答"当前命中率多少""哪轮 miss"，优化全靠猜，修复后也无法验证效果。
    - 设计（五块，按 ROI 排序，前两块 P0 必做）：
      1. **动态时间移出 system Instruction（P0，治本）**：`buildEnvBlock` 拆为稳定段（OS/时区名/工作目录/项目概览，跨轮不变）+ 动态段（当前时间精确到秒）。稳定段留 system Instruction（可缓存）；动态段时间作为**独立尾部 system 消息**注入--复用 `board_context.go`/`injectEvents` 既有"尾部不可缓存"范式，在 Assemble 输出末尾（看板段之后）追加 `【当前时间】<timeStr>` system 消息。system Instruction 跨轮字节稳定 -> DeepSeek 命中整个 system + role + discipline + history 前缀。时间精度不降（仍秒级），模型仍可见，只是位置从"前缀头部"挪到"尾部"。注意：时间戳不再放 Instruction 后，须确认 `systemPrompt()` 返回值跨 `RunWithHistory` 字节一致（role/soul/profile/project doc 稳定前提下）。
      2. **命中率可观测（P0，先于一切验证）**：(a) `TokenUsage` 或 `LLMCall` 记录结构加 `CacheHitTokens`/`CacheMissTokens` 字段，三个 provider 填充各自解析值（openai-chat 用 hit/miss；openai-responses 用 cached_tokens 作 hit、input-cached 作 miss；anthropic 用 CacheReadInputTokens 作 hit、CacheCreationInputTokens 作 miss）；(b) `logLLMCall`（`react_agent.go:594`）每轮日志加 `cache_hit=X cache_miss=Y hit_rate=P%`；(c) `llm_tracker_stats.go` 聚合 per-session/per-agent 累计命中率；(d) TUI 状态栏或 `/cache` 命令展示当前会话累计命中率。**没有这个就 无法证明任何修复有效**，必须先落地或与治本项同批。
      3. **DeepSeek 角色归一 openai-chat provider（P1）**：`roles.yaml` 所有 `deepseek-v4-flash` 角色的 provider 改 `openai-chat`（或别名 `openai`/`openai-deepseek`，`blades_client.go:253`），base_url 指 DeepSeek `/chat/completions` 端点。消除 anthropic 网关的 `cache_control` 依赖与 openai-responses 的缓存语义不明，回到 DeepSeek 原生自动前缀缓存 + 原生 hit/miss 字段。需验证 base_url 路由、reasoning_content 回传（`provider_openai_chat.go:9` 已兼容）、连通性启动校验不被拖死（#33 教训：deepseek 端点不稳时 strict 校验会拖死启动，用 k3 兜底或宽松校验）。若某角色必须用 responses/anthropic 端点（功能差异），保留但在该路径加显式缓存策略（openai-responses 走 `prompt_cache_key` 若端点支持；anthropic 加 `cache_control` 断点）。
      4. **头部可变段后置/冻结（P1）**：(a) userProfileInjector 内容在 session 启动时冻结一次（`current()` 改 `frozen()`），会话期内工具改画像不回写注入值（下个会话生效），消除头部可变；(b) PROJECT.md 同理，`LoadProjectDoc` 结果 session 级缓存，`projectRefresher` 刷新只更新磁盘与下个会话的缓存，不污染当前会话 system 前缀；(c) 进一步可把画像段也从 system Instruction 挪到尾部 system 消息（与时间同位），彻底消除头部可变。选 (a)+(b) 冻结方案最小改动，(c) 为激进可选。
      5. **子 Agent 同角色共享缓存（P2，治本后自然生效）**：时间戳移出后，同角色子 Agent 的 system Instruction 字节一致（role/soul/project doc 相同），DeepSeek 服务端跨子 Agent 命中同一前缀缓存。无需额外代码，作为治本项的附带收益验证项。若实证未命中，查子 Agent system 是否还有其他每次随实例变化的段（agentID/父链注入等）并后置。
    - 执行流程：
      1. **先落可观测（块 2）**：`TokenUsage` 加字段 + 三 provider 填充 + `logLLMCall` 日志 + tracker 聚合 + TUI 展示。跑一次基线会话记录当前命中率（预期很低），作为修复前后对照基线。
      2. **块 1 时间移出**：`buildEnvBlock` 拆分 + Assemble 尾部追加时间消息（MetaAgent 与子 Agent 都加，子 Agent 也受益）。跑同场景对比命中率。
      3. **块 3 provider 归一**：`roles.yaml` 改 provider + base_url，连通性校验，跑同场景对比。
      4. **块 4 头部冻结**：profile/PROJECT.md session 级冻结，跑同场景对比。
      5. **块 5 验证**：多子 Agent 同角色场景，查跨子 Agent 命中。
    - 测试：(a) 单测 `buildEnvBlock` 稳定段跨调用字节一致（同 workDir 入参返同值，无时间）；(b) 单测 Assemble 输出末尾含【当前时间】system 消息且时间在合理范围；(c) 单测三 provider cache 字段正确填充 TokenUsage（mock 响应含各字段变体）；(d) 单测 tracker 聚合命中率算术；(e) 单测 profile/PROJECT.md session 冻结（会话期内 `current()`/`LoadProjectDoc` 返稳定值）；(f) 集成测：mock LLM 记录每次请求 Instruction 字节，连续两轮 RunWithHistory Instruction 字节相等。
    - 验收：(a) TUI `/cache` 或状态栏展示当前会话累计命中率；(b) 同一 MetaAgent 会话连续 3 轮用户输入，第 2、3 轮 system Instruction 字节相等、日志 hit_rate 显著上升（基线 vs 修复后量化对比）；(c) DeepSeek 角色全部走 openai-chat，启动连通性校验通过；(d) 子 Agent 同角色派发，第 2 个起 hit_rate > 0；(e) 双模块 `go test ./...` 绿。
    - 不做：不做客户端侧缓存（DeepSeek 服务端自动缓存已够，客户端缓存引入一致性问题）；不做跨会话缓存复用（DeepSeek 缓存有 TTL，跨会话意义不大）；不引入 Anthropic 显式 `cache_control` 断点机制（归一 openai-chat 后不需要，Anthropic 路径若保留再说）；不改压缩冻结机制（已正确，本项只补 system 前缀侧）；不动 history 追加语义（尾部变化不破坏前缀，已正确）。

41. **进程内本地向量模型：ONNX Runtime 真本地 embedding（零 api_key、零外部服务）**（✅ 已完成 2026-08-12，doc/变更.md 任务 26；来源：2026-08-11 用户需求--当前 `provider=local` 实为本地 HTTP 服务（ollama/xinference），仍需起独立进程 + 端口；用户要"真本地"=模型权重进 Go 进程内存，无 api_key 无外部服务）
    - 背景：现有 embed 三 provider（`pkg/types/embed.go:6-18`）--pseudo（字符哈希伪向量，召回非语义，#26-5 已标注待修）、openai（OpenAI 兼容端点，需 api_key + 网络）、local（**语义误导**：`embedder.go:43` `case "openai", "local"` 合并处理，local 走 `OpenAIEmbedder` 打本地 HTTP `/v1/embeddings`，本质仍是 OpenAI 兼容客户端，需 ollama/xinference 等独立服务进程）。真"进程内本地"无 provider 承载。块记忆召回（#17 P0 outcome 排序）与外部知识库检索（#27 RAG）的召回质量直接受 embedding 语义能力制约，pseudo 字符哈希使召回近随机，需真实语义向量。
    - 目标：新增 `provider=onnx`，进程内加载 ONNX 格式 embedding 模型权重，cgo 调 ONNX Runtime 推理，零 api_key、零网络、零外部服务进程。模型常驻内存，Embed 调用即内存推理。
    - 选型（逐项决策）：
      1. **推理引擎 = ONNX Runtime**：业界标准，CPU/GPU 通用，Go 绑定 `github.com/yalue/onnxruntime_go`（cgo，封装官方 `onnxruntime` 共享库）。不选 TensorFlow Lite（绑定更重）、不选 llama.cpp/ggml（embedding 支持非主线、cgo 复杂）、不选纯 Go 推理（无生产级 BERT 实现）。
      2. **模型 = BGE-base-zh-v1.5 ONNX**（768 维，~400MB，中文优化，CPU 单条 ~10ms 级）：**维度 768 与现有 `config.yaml:13 pgvector.dimensions: 768` 完全一致，无需重索引、无需改 pgvector schema**。备选 BGE-small-zh-v1.5（512 维，~95MB，更轻但需改 dimensions + 重建索引）、BGE-m3（1024 维，~2.3GB，多语言最强但重且需重索引）。首版取 BGE-base-zh-v1.5 平衡质量/体积/维度兼容。
      3. **tokenizer = HuggingFace `tokenizers` Rust 库 cgo 绑定**（`github.com/ianschz/tokenizers-go` 或 `github.com/woodchuck-club/tokenizers`）：从模型目录的 `tokenizer.json` 加载，通用支持 WordPiece/BPE/SentencePiece，与 HF Python 端字节级一致。不选纯 Go WordPiece（BGE 词表与特殊 token 处理易踩坑、维护成本高）、不选 tiktoken（BPE，与 BGE 的 WordPiece 不兼容）。
      4. **provider 命名 = `onnx`**（不沿用 `local`，避免与"本地 HTTP 服务"语义混淆）：现有 `local` 保留作"本地 OpenAI 兼容服务"别名不删（向后兼容），新 `onnx` 专指进程内。`embedder.go:39` switch 加 `case "onnx"`。
    - 设计（新文件 `backend/internal/embed/onnx.go`）：
      1. **ONNXEmbedder struct**：持 `*onnxruntime_go.AdvancedSession`（模型会话）+ tokenizer 句柄 + `dim int` + `maxTokens int`（BGE 系列 512）+ `mu sync.Mutex`（ONNX Runtime 非线程安全，推理串行化或池化）。
      2. **构造 `NewONNXEmbedder(cfg, dim)`**：参数 = 模型目录路径（`cfg.BaseURL` 复用指目录，或新增 `EmbedConfig.ModelPath` 字段）、dim。启动加载 `.onnx` + `tokenizer.json` 到内存，失败 panic（`MustNewEmbedder` 语义，启动 strict 一致）。模型路径默认 `./models/bge-base-zh-v1.5/`，config 可覆盖。
      3. **Embed(ctx, text)**：空文本返全零（与 `openai.go:61-63` 一致）-> tokenizer 编码 + truncation 到 maxTokens -> 构造 ONNX input tensor（input_ids/attention_mask/token_type_ids）-> `session.Run()` -> 取 last_hidden_state 平均池化（mean pooling，BGE 不用 CLS token）-> L2 归一化 -> 返 `[]float32`。BGE 需归一化（ cosine 相似度），pgvector 余弦距离依赖。
      4. **Dim()**：返模型固有维度（768），与 cfg dim 校验不一致则启动报错（避免静默错配）。
      5. **BatchSize**：`EmbedConfig.BatchSize` 已有字段（`pkg/types/embed.go:17`），ONNX 单条推理为主，批量优化后置（ONNX Runtime 支持批量 input，但块记忆写入是单条低频，RAG 检索也是单 query，首版不批量）。
    - 配置与维度对齐：
      1. `EmbedConfig` 加 `ModelPath string`（ONNX 模型目录，onnx provider 专用；openai/local 忽略）。`role_config.go` applyDefaults 不强填，空则用默认 `./models/bge-base-zh-v1.5/`。
      2. `roles.yaml` embed 段：`provider: onnx`、`model: bge-base-zh-v1.5`、`model_path: ./models/bge-base-zh-v1.5/`、`api_key` 留空（onnx 不读）。注释说明 onnx vs local 语义差异。
      3. 维度：BGE-base-zh=768 = 现有 `pgvector.dimensions: 768`，零迁移。若后续换 BGE-small(512)/BGE-m3(1024)，需同步改 dimensions + 重建 knowledge 表索引（`store` 包迁移脚本）。
    - 模型文件管理：
      1. 不入 git（~400MB 二进制）。仓库 `models/` 目录加 `.gitkeep` + README 指引下载：HuggingFace `BAAI/bge-base-zh-v1.5` 的 `onnx/model.onnx` + `tokenizer.json` + `tokenizer_config.json` + `vocab.txt`。
      2. 启动校验：`NewONNXEmbedder` 检查 model_path 下必要文件存在，缺失报清晰错误（"模型未下载，见 models/README"），不静默回退 pseudo。
      3. 可选：首次启动自动下载（HF Hub API + 校验 SHA256），但引入网络依赖与镜像问题，首版不做，手动下载。
    - 性能与热路径：
      1. ONNX Runtime CPU 推理单条 ~10ms（BGE-base），块记忆事实提取（`saveBlockMemory`）与 RAG 检索（`search_knowledge`）均为低频单条调用，不阻塞热路径。
      2. 会话常驻：`AdvancedSession` 构造一次复用，避免每次重加载（重加载 ~秒级）。
      3. 线程安全：ONNX Runtime session Run 非线程安全，`mu` 串行化；若实测热路径争用，改 session 池（预构造 N 个 session）。
      4. 内存：模型 ~400MB 常驻 + 推理临时 tensor ~MB 级，可接受；若多 provider 实例化注意 `bootstrap` 只构造一个全局 embedder（`bootstrap.go:141` 已是单例）。
    - 部署与 cgo：
      1. `onnxruntime_go` 需 `onnxruntime` 共享库：Windows `onnxruntime.dll`、Linux `libonnxruntime.so`、macOS `libonnxruntime.dylib`。放 `models/lib/` 或系统 PATH，启动 dlopen。README 指引下载（微软 GitHub release，按平台选 CPU/GPU 版）。
      2. `tokenizers` cgo 需 Rust 编译产物（绑定库 release 提供 `.so`/`.dll`），或 `tokenizers-go` 静态链接。评估编译链复杂度，优先用预编译 release。
      3. cgo 影响：开启 cgo 后交叉编译受限（需目标平台 toolchain + 共享库）。若 CI/发布需纯静态，加 build tag `onnx` 隔离，默认构建不含 onnx（pseudo/openai 仍可用），`-tags onnx` 启用。`embedder.go` switch `case "onnx"` 在无 onnx build tag 时编译期 stub 返"需 onnx tag"错误。
      4. CLAUDE.md `GOTOOLCHAIN=local` 约束不变；cgo 与之无冲突。
    - 测试：
      1. 单测 `TestONNXEmbedder_DimAndShape`：加载模型后 Dim()=768、Embed 返 768 维 float32 且 L2 范数≈1。
      2. 单测 `TestONNXEmbedder_SemanticSimilarity`：相近语义（"塔防游戏怪物路径" vs "tower defense enemy path"）余弦 > 不相关对（"塔防" vs "数据库连接池"），证明非随机（pseudo 不过此测）。
      3. 单测 `TestONNXEmbedder_EmptyText`：空文本返全零不 panic。
      4. 单测 `TestONNXEmbedder_DimMismatch`：cfg dim 与模型固有维度不一致启动报错。
      5. 集成测：块记忆写入 -> 召回命中（复用 `injectRecalledMemory` 测试范式，换 onnx embedder 后召回质量提升）；RAG `search_knowledge` 命中（复用 #27 集成测）。
      6. build tag：默认构建 onnx case 编译期 stub；`-tags onnx` 构建真实现。
      7. 离线验证：断网环境（无 api_key 无网络）Embed 正常返回，与在线 openai 端点向量余弦 > 0.9（同模型同权重应近似一致，验证 ONNX 转换正确性）。
    - 验收：(a) `roles.yaml` 配 `provider: onnx`，无 api_key，启动加载模型成功（strict 启动校验通过）；(b) 断网跑块记忆召回，语义相近项命中、pseudo 时代的随机召回消失；(c) 召回质量对比 pseudo：相同知识库 + 相同 query，onnx top-k 语义相关性显著优于 pseudo（人工抽检或用标注集）；(d) 双模块 `go test ./...` 绿（默认构建不含 onnx 也不破坏）；(e) 单条 Embed 延迟 < 50ms（CPU BGE-base 量级）。
    - 不做：不做 GPU 加速（CPU 够用，GPU 版 onnxruntime 额外分发成本）；不做模型自动下载（首版手动，避免 HF 镜像/网络依赖）；不做批量 embedding API（`EmbedBatch` 接口，低频单条够用，后置）；不删现有 `local` provider（保留作本地 HTTP 服务别名，向后兼容）；不做 rerank（归 #27 可选后置）；不做多模型热切换（一个 embedder 一个模型，换模型改 config 重启）。
    - 联动：#26-5（pseudo 召回决策）--onnx 落地后 pseudo 正式退居"离线测试专用"，生产默认 onnx；#27（外部知识库 RAG）--onnx 为 RAG 提供真实语义召回底座，`search_knowledge` 与 `RAG()` 中间件质量直接受益；#17 P0（块记忆 outcome 排序）--真实向量使 reuse_count 飞轮与语义去重生效（pseudo 下近似随机）。
