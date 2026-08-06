## 待完成 / 开放项

1. **ReAct 端到端集成测试**（已部分修复）
   - 背景：`test/coding/*` 与 `test/api/*` 曾基于旧 ThreeLayerGraph 状态机 mock。
   - 当前状态：`test/fixtures/llm.go` mockMessage.Content 改 `json.RawMessage` 兼容 blades contrib/openai 数组格式后,e2e 全部通过（commit `e1e0eef`）。`test/coding/{snake,css,bug_fix}` 与 `test/api/*` 跑通。
   - 开放动作：持续补充覆盖 Agent 树 cancel、事实提取回退路径等新端点的 e2e。

2. **TUI Agent 树运行时构建**（TUI 侧已完成）
   - 当前状态：`internal/domain/orchestrator/tree.go` 权威树 struct 已落地（commit `2e97eff`）,PG 持久化已落地（commit `5a1134d`）。Dispatcher 派发时 Register/SetCancel/Finish,HTTP 暴露 `GET /api/sessions/{id}/tree` + `POST /api/sessions/{id}/agents/{aid}/cancel`。`ReactService.ListAgents` 仍返回单 MetaAgent,但 `Tree()` 方法返回权威树快照。
   - TUI 迁移（本次完成）：`agent_tree_panel.rebuild` 与 `view.go` 任务面板改读 `agent.Tree()` 替代旧事件流派生 `deriveSubAgentNodes`。新增 `orchestratorNodesToTreeNodes`（按 ParentID 链算 depth + 状态映射 Running->Active/Done->Done/Failed->Error/Cancelled->Done）。`deriveSubAgentNodes` + 其测试已删（树是单一真相源,事件流派生有竞态/遗漏;树已 PG 持久化重启 lazy 恢复,无需 fallback）。
   - 开放动作：`verifyloop.ExecuteChild` 同步路径入树（phase 2）。

3. **记忆事件持久化**（已完成）
   - 当前状态：新增 `domain/memory/pg_store.go` `PostgresEventStore` 实现 `memory.Store`（SaveEvent/LoadEvents）。`Pipeline.Write` 仍维护内存 events map 供热路径 Assemble 读;PG 仅写侧持久化。`agent_events` 表（session_id 派生自 agentID、agent_id、type、role、content、tool_name、input、output、occurred + 两索引）由 `EnsureAgentEventsSchema` 建。`bootstrap` 把 `memory.NewInMemoryStore()` 换为 `memory.NewPostgresEventStore(pgStore.DB())`;`ensureSchemas` 注册 `agent_events`。旧 `session_events` 保留只读（UI 进度事件,不同事件流）。LoadEvents 按 occurred DESC 取最近 limit 条,当前未接热路径（Assemble 读内存）,供未来按需重载。
   - 开放动作：会话恢复时按 session_id 从 `agent_events` 重载事件到内存 map（当前仅写不读回）。

6. **文档漂移清理**（部分完成,本次更新）
   - 当前状态：`doc/项目说明.md` / `README.md` / `doc/流程图.md` 本次已同步 Agent 树 + 事实提取两步改造。`doc/设计文档_v3.md` 仍保留旧 ThreeLayerGraph 描述。
   - 开放动作：`设计文档_v3.md` 顶部已有状态声明,适当时机重写或归档。

7. **Soul.Inject 接 ReAct 热路径**（已完成）
   - 当前状态：`agent` 包新增 `PersonaInjector` 接口（`Inject(systemPrompt) string`）,`soul.Loader` 实现该接口。`ReActAgent.WithPersonaInjector` + `systemPrompt()` 末尾把人格内容拼到完整 prompt 最前（envBlock 之前）。`ReactService.SetPersonaInjector` + `Dispatcher.WithPersonaInjector` 分别在构造 MetaAgent/子 Agent 时注入,`bootstrap` 传 `rt.Soul` 给两者。人格为空时 `Inject` 原样返回无副作用。`config/soul.md` 30 行真实人格内容,注入生效。
   - 开放动作：`Skill` 仍未接 ReAct 热路径（从未把 SkillSet 注入 prompt）。后续若确认不需要可继续删。

11. **删 Runtime 死重**（已完成,2026-07-30,步骤 6）
    - 当前状态：`internal/watchdog` + `internal/cmdqueue` 包整体删除,`runtime.Runtime` struct 去 `Watchdog`/`CmdQueue` 字段 + `WithWatchdog`/`WithCmdQueue` 选项 + `SetAgentConfig` 中 `Watchdog.SetConfig` 调用。`bootstrap` 去 `rt.CmdQueue.SetLogger`。死重判定依据:`Watchdog` 无 `.Check()` 热路径调用方(仅 runtime 自建自配置,`meta_watchdog.go` 注释为陈旧引用);`CmdQueue` 无消费方(仅 `SetLogger` 写日志,无 `Enqueue`/`Dequeue` 调用)。保留 `Board`/`Skill`/`Soul`:`Board` 被 TUI 作类型消费(`Snapshot`/`TaskStatus`),`Skill`/`Soul` 被 server API 作字典消费(`/api/skills`、`/api/agent/info`)。
    - 开放动作：`Skill` 未接 ReAct 热路径(从未把 SkillSet 注入 prompt);`Soul.Inject` 从未被调用(仅 `Soul.Name()` 作 API 元数据)。两者仍未接热路径,但保留 API 字典消费。后续若确认 Skill/Soul 不再需要可继续删。

12. **共享记忆一致性**【Layer 1-3 已落地 2026-07-28】
    - 当前状态：详见 `doc/计划_共享记忆一致性.md`。Layer 1-3 已完成（`writeSharedMemoryInput` 加 `Files []string` + `WriteFile` hook 失效 KV + `Dispatcher.buildSharedPrefix` mtime 校验）。KVMemory 已合并为单一 `SharedMemoryStore`,三路 inject 合并为 `buildSharedPrefix`。
    - 开放动作（P1/P2,未做）：Layer 4 角色写目录分区（`roles.yaml` sandbox.allowed_write_paths）；Layer 5 `mailbox.Message` 增 `FilesModified []string`。

13. **评估项**
    - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试,不明确具体测试方向时需要先把方案列出,给开发处方案的代码Agent是否符合代码Agent的逻辑,不符合代码Agent纠正,符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合,不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent,期间各Agent可以反复询问,纠正,需要Agent在被询问时开一个协程进行回复,需要携带关键记忆或者协程Agent常驻共享代码Agent记忆,可评估。测试流程最为重要,查看现在的测试是否严谨与多重确认。代码Agent完成开发,找到固定助手的测试助手,先进行自测试,返回结果成功后返回给上级Agent,上级Agent按更大范围模块进行统一测试,哪个部分不行打回。
    - Agent执行任务,先拆解任务,拆解为不会干涉的单元任务后,一个单元任务使用一个干净上下文的Agent编码助手执行,压缩Token成本。执行完成后暂时不销毁,一定时间不使用直接消耗,有使用则重置使用时间并加长使用时间。原因：我在使用claude时,经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉,并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆,块记忆过于抽象,可用性与传统感觉不明显,。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式,每个领域干净上下文负责自己的事。

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
    - 开放动作：验收中暴露但未修的次要项--共享记忆契约文件十六进制命名漂移;MetaAgent 终答前不做端到端自验（本次靠用户反馈闭环补上）;verifyloop `domain_self_test_enabled` 仍待接线。


17. **编排加固：阿里 AICP 对比可落地项**（来源：`doc/编排对比_阿里AICP军团.md`，对照 AICP「Agent 军团 + 共享黑板」方案筛出的增量加固点，不动主架构）
    - 背景：AICP 三层骨架（上层编排控制 / 中层能力三角 / 下层基础设施）与本仓库已同构（MetaAgent+dispatcher+tree / 角色+工具+skills+块记忆 / store+memory+model）。固定 9 层、纯状态机、黑板唯一媒介、三道闸门、权重飞轮均不 adopt（开放任务灵活性 / 编码样本稀疏）。仅取 4 个高 ROI 增量。
    - P0 块记忆价值反馈闭环（补 AICP 回路一最后一环）：`dispatcher.go:1373 saveBlockMemory` 写 KnowledgeRecord 时 `Meta` 加 `outcome`（success/fail/partial）+ `reuse_count` 字段；`dispatcher.go:1450 injectRecalledMemory` 召回后按 `outcome=success` 优先、`reuse_count` 降序排序。当前成功/失败记忆同等权重召回，失败记忆与成功记忆并列注入会误导子 Agent。失败记忆不丢，降权或单列「避坑」段。回路二（权重飞轮）编码样本稀疏难收敛，不 adopt。
    - P0 子 Agent 心跳检活【已落地 2026-08-06】：ReActAgent 加 `activityReporter` 回调（`generateOnce` + 工具派发触发）+ `WithActivityReporter`；Dispatcher 加 `activity sync.Map`（叶子 Agent 最后活动时间戳）+ `subMeta`（cancel/parentID/sessionID/doneOnce）+ `heartbeatTimeout`（默认 5min，`sub_agent_heartbeat_timeout_min` 配置）+ `patrol` 巡检 goroutine（首次 Dispatch 经 `ensurePatrol` 幂等启动，`heartbeatTimeout/2` 间隔，下限 10ms）。超阈值无活动 -> `killStuckSubAgent`：cancel ctx + `doneOnce.Do(trackChildDone)` 兜底递减（防流式挂起不尊重 ctx 时父永久空等）+ `notify` 父「子 Agent 疑似卡死」+ 树节点 Failed。**仅叶子 Agent 注入 reporter**：DomainAgent/MetaAgent 有自身 wait loop，注入会误杀合法等待；叶子被 kill 后父 PendingChildren 递减级联解除 MetaAgent 阻塞。解决「第二次 session 卡很久」：胖上下文推高 LLM 端点假死率，旧实现父空等 60min `sub_agent_timeout`，现 5min 心跳早暴露。回归测试 `TestDispatcher_HeartbeatKillsStuckLeaf` / `TestDispatcher_HeartbeatNoPatrolWhenDisabled` / `TestReActAgent_ActivityReporter`。
    - P1 共享记忆 slot 写入方校验 + version 乐观锁（偷黑板机制，非替代 mailbox）：当前 SharedMemory 槽位 `parentID:key` 任何子 Agent 拿到 parentID 都能写任意 key，兄弟 Agent 并发写 `file_tree` 等共享 slot 会互相覆盖；`verifyFileMtimes`（dispatcher.go:1477）只防 stale 文件不防并发覆盖。落地：slot 写入加 caller 校验（`file_tree` 单写多读）+ SharedMemory entry 加 version 字段，CAS 写入冲突重试（等价 AICP 黑板 Lua 原子写 + version 乐观锁）。不引入「唯一媒介」语义，保留 mailbox 点对点 request/reply（code↔测试紧耦合协作依赖）。
    - P1 破坏性工具分级 + 生产环境命令用户确认（偷 AICP「破坏性操作权限隔离」轻量版，不 adopt 双签）：当前叶子助手（code/test/ui）都持 `WriteFile`/`RunCommand`，破坏性操作分散持有。工具元数据加 `destructive` 标记（WriteFile/RunCommand 写类 = true）；派发到生产环境工作目录或命中危险命令模式时，destructive 工具调用经 `liveFn` 推「需确认」事件，上层暂停会话等用户确认，非生产环境照常自主。比 AICP 双签轻：只在边界触发，不阻塞常规编码流。
    - 不 adopt（归档决策）：9 层固定分层（开放任务僵化）/ 纯状态机主循环（verifyloop 已示范「固定流程折叠为工具」正确用法，不推广为主循环）/ 黑板替代 mailbox（杀紧耦合协作）/ 三道安全闸门（混沌工程特有，编码无爆炸半径语义）/ 完整权重飞轮（样本稀疏）/ 无外部需求时的 A2A + AgentCard（P2 按需，仅当开放外部 Agent 接入）。
    - 开放动作：按 P0 -> P1 顺序落地；每项配单测（outcome 排序 / 心跳巡检 / slot CAS / destructive 确认事件）。


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
