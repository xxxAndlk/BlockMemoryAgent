# 04 子 Agent 派发与生命周期

> 只从代码还原（不引用其他文档）。核心文件：`backend/internal/domain/subagent/dispatcher.go`（5492 行）。
> 兄弟文件（idle_pool / acceptance / salvage / task_ledger / plan_confirm / map_dispatch / worktree / contract_check / smoke_check / verify_levels / integration_check 等）见 4.16-4.18 与后续小节。

## 目录

- [4.1 职责与定位](#41-职责与定位)
- [4.2 Dispatcher 结构（字段全表）](#42-dispatcher-结构字段全表)
- [4.3 注册的工具面](#43-注册的工具面)
- [4.4 call_sub_agent 完整流程](#44-call_sub_agent-完整流程)
- [4.5 call_sub_agents 批量派发](#45-call_sub_agents-批量派发)
- [4.6 dispatchOne 逐点](#46-dispatchone-逐点)
- [4.7 runSubAgent 生命周期（结果分支全表）](#47-runsubagent-生命周期结果分支全表)
- [4.8 runSubAgentOnce（纯执行路径）](#48-runsubagentonce纯执行路径)
- [4.9 心跳巡检与假死击杀](#49-心跳巡检与假死击杀)
- [4.10 暂停 / 恢复 / 复活](#410-暂停--恢复--复活)
- [4.11 失败分类与结构化失败标记](#411-失败分类与结构化失败标记)
- [4.12 spec 体系（WriteSpec 强制门）](#412-spec-体系writespec-强制门)
- [4.13 共享记忆前缀注入](#413-共享记忆前缀注入)
- [4.14 块记忆召回与沉淀](#414-块记忆召回与沉淀)
- [4.15 墙钟体系](#415-墙钟体系)
- [4.16 看板回写 / 接力熔断 / 任务台账](#416-看板回写--接力熔断--任务台账)
- [4.17 聚合模式与批间校验](#417-聚合模式与批间校验)
- [4.18 隐含约定与坑](#418-隐含约定与坑)
- [4.19 热驻池（idle_pool.go）](#419-热驻池idle_poolgo1449-行)
- [4.20 活动证据与假死判定](#420-活动证据与假死判定activitygo)
- [4.21 复用守卫](#421-复用守卫reuse_guardgo)
- [4.22 领域档案冷复活](#422-领域档案冷复活domain_profilego)
- [4.23 接力熔断](#423-接力熔断relaygo)
- [4.24 worktree 隔离与合并门](#424-worktree-隔离派发与合并门worktreego--merge_worktreego)
- [4.25 失败打捞](#425-失败打捞salvagego)
- [4.26 任务台账](#426-任务台账task_ledgergo)
- [4.27 验收闭环](#427-验收闭环acceptancego--demogo)
- [4.28 机器校验闸门顺序](#428-机器校验闸门顺序交付质量后半程)
- [4.29 计划确认与执行计划](#429-计划确认与执行计划plan_confirmgo--plango)
- [4.30 map_sub_agents](#430-map_sub_agentsmap_dispatchgo)
- [4.31 技能工具](#431-技能工具skills_toolgo)
- [4.32 跨 Agent 协作问答（send_message 通道）](#432-跨-agent-协作问答send_message-通道)

---

## 4.1 职责与定位

`Dispatcher`（dispatcher.go:145-444）是**多 Agent 编排的唯一调度器**：

- 把 `call_sub_agent` / `call_sub_agents` / `map_sub_agents` / `send_message` / `cancel_agent` / `pause_agent` / `resume_agent` / `set_agent_model` / `merge_worktree` 注册进 `domain/tool.Registry`（4.3）；
- 派发时构造子 `ReActAgent`（角色工具白名单、memory、mailbox、心跳、日志、引擎选择）；
- 维护**权威 Agent 树**（`orchestrator.Tree`，Register/Finish/Pause/Reopen）、未决计数（pending，供父终结保护）、会话派发限额、任务台账、看板回写；
- 子 Agent 完成后把摘要经 mailbox 回传父 Agent（notify），失败走结构化失败协议（4.11）；
- 心跳巡检 goroutine 杀假死子 Agent（4.9）。

子 Agent 的 ctx **故意从 `context.Background()` 派生**（dispatcher.go:35-37）：父会话取消不波及子 Agent，避免长任务结果丢失；另经 `tool.StopContextFrom(ctx)` 继承会话级 stopCtx 以支持显式停止（2756-2765）。

## 4.2 Dispatcher 结构（字段全表）

按依赖注入来源分组（全部经 `With*` 方法注入，nil 时对应功能零行为）：

**核心依赖**：`registry *role.Registry`（角色定义/权限 CanCall）、`models ModelProviderFactory`（+可选 `AgentModelOverrider`/`AgentModelSwitcher` 实例级模型覆盖）、`tools *tool.Registry`（与父共享）、`mailbox *mailbox.Mailbox`、`memory agent.MemoryPipeline`。

**运行态**（sync.Map 族，键=subAgentID）：

- `running`：运行中的子 `*ReActAgent`（send_message 目标可达性判定）。
- `pending`：parentID → `*pendingState{count atomic.Int64, notify chan struct{} (缓冲1)}` —— 未决计数 + 完成信号。
- `sessionCounts`：sessionID → 派发总数（全局限额，用户新消息 ResetDispatchCounts 重置）。
- `subMeta`：`*subAgentMeta{cancel, parentID, sessionID, wallClock, doneOnce}`（巡检 kill 句柄 + doneOnce 防双递减）。
- `activity`：`*activityEvidence`（lastTS/lastKind/llmInFlight/toolStartTS，心跳判据）。
- `lastWrites`：近期写入文件（WriteFile/EditFile live 事件识别，kill 时回告"盘上有货"）。
- `recentActs`：杀前最近叙述片段 + 工具轨迹（机械归纳"已完成事项"）。
- `heldSkills`：agentID → 持有技能名列表（技能渐进披露权威源）。
- `parentSpecs`：`specRecKey{parentID,domain}` → `*parentSpecRecord`（spec 结构化切片：files 绝对路径/contract/verifyLevels/filesMtime 快照/probes/scenes/baseline/acceptance）。
- `pausedResumes`：nodeID → 续跑次数（上限 maxPausedResumes，默认 1）。
- `worktrees`：subAgentID → `*worktreeHandle`（worktree 隔离派发句柄）。
- `softStops`（map+mutex）：软停止中的 sessionID；`pauseRequests`：被手动暂停标记的 nodeID。
- `aggByAgent`：map_sub_agents 聚合登记；`pushedViolations`/`pushedViolationTimes`：契约违例去重；`dispatchGenerations`：per-(parent,domain) 接力代数。
- `ledger *TaskLedger`：任务台账；`pool *domainPool` + `suspendStates`：热驻槽池与会话挂起状态。

**回调族**（全部由 bootstrap/ReactService 接线）：`liveFn`（子 Agent LiveEvent 路由回会话）、`userNotifyFn`（验收进度用户可见消息）、`childDoneFn`（awaiting_child 唤醒）、`treeFn`/`boardFn`/`boardFnCreate`、`projectPrefs`、`sessionGearFn`、`domainProfilesFn`/`domainMemoriesFn`/`domainProfileSinkFn`、`skillRecall`/`skillUseCounter`、`msgStore`/`msgLogger`、`factExtractor`/`salvageExtractor`。

**配置**：`timeout`（默认 30min）、`domainReconClock`、`taskRuneSoftLimit/HardLimit`（默认 3000/4000）、`maxTotalDispatches`、`heartbeatTimeout`/`domainHeartbeatTimeout`、`dispatchRetryCount`、`reflectionMaxRounds`/`planMaxSteps`/`judgeRole`/`engineLLMTimeout`、`specEnforcementEnabled`、`planConfirmEnabled/timeout/maxRevisions`、`worktreeEnabled`、`hotCfg`。

## 4.3 注册的工具面

| 注册方法 | 工具 | 说明 |
|---|---|---|
| `RegisterCallTool`（1774-1781） | `call_sub_agent`、`call_sub_agents`、`map_sub_agents`、`merge_worktree` | 派发族；Description 动态生成（含角色清单与参数说明，2246-2264） |
| `RegisterMessagingTool`（1788） | `send_message` | 跨 Agent 邮箱消息（request/info/reply/escalate + thread_id） |
| `RegisterControlTool`（1871-1876） | `cancel_agent`、`pause_agent`、`resume_agent`、`set_agent_model` | 控制面；pause/resume/set_model 授权 = caller 必须是节点 ParentID（越级拒绝） |
| `RegisterPlanTools`（1766-1769） | `submit_plan`、`review_plan` | 计划确认（plan_confirm.go） |

递归深度约定：**MetaAgent → DomainAgent → 叶子助手**三层；`registry.CanCall(父角色, 子角色)` 拦截 domain→domain 平级派发（2627）。

## 4.4 call_sub_agent 完整流程

`callSubAgentTool.Execute`（2366-2468）：

1. 解析参数：`role_id`/`task`/`domain`/`responsibility`/`mode`/`verify_kind`/`tools_hint`/`skills`/`wall_clock_min`/`reuse_agent_id`/`takeover`/`worktree`（参数提取辅助函数 2983-3032）。
2. 取 `parentID = AgentIDFromContext(ctx)`，缺失即拒。
3. **同名热驻槽隐式复用**（2404-2408）：`roleID==domain && reuseAgentID==""` 时 `resolveIdleSiblingReuse`（2514-2549）——同父下同名且状态 Idle 的热驻槽存在则自动复用（含档位守卫：槽固化档位与会话当前档位不符则不复用）。注意这一步**先于参数校验**（空 responsibility 会被校验拒掉，复用路径不应被它挡住）。
4. **领域档案冷复活**（2413-2415）：`applyDomainProfileSeed`（domain_profile.go）——名字/别名/路径匹配档案，命中则 domain 归一化 + 种子段（摘要/存活文件/结论链）拼进 task。
5. `validateDispatchArgs`（2292-2342）：
   - 硬拒：role_id/task 空；`role==domain` 且 responsibility 空（职责边界必填）；task 超硬限（4000 runes）；mode/verify_kind 非法枚举。
   - 软警告（放行）：domain 非中文领域名；task 超软限未达硬限。
6. **WriteSpec 强制门**（2427-2441）：角色非 `SpecExempt` 时 `checkSpecBeforeDispatch` → `hasFreshSpec`（4.12）；缺失/不合法/过期拒绝派发；worktree 派发豁免 stale。
7. worktree 与 reuse 互斥校验（2443-2445）。
8. `dispatchOne`（4.6）。失败把 `errRes.Tool` 覆盖为 `call_sub_agent` 返回。
9. 成功输出 subAgentID（+ takeover 留痕 + 软警告拼接）。

## 4.5 call_sub_agents 批量派发

`callSubAgentsTool.Execute`（3061-3192）：

- `tasks` 数组 ≤6 项，每项字段同单派；逐项参数校验（空 responsibility 等），任一硬拒整批拒。
- **同波 domain 名称查重**（3124-3137）：重名整批拒（回灌摘要/树展示无法区分责任域）。
- spec 校验：有 domain 项逐领域校验；全无 domain 时按遗留单键一次校验（含全 spec_exempt 跳过）。
- 逐项 `dispatchOne`：某项失败不影响其他项；结果汇总"已并行派出 N 个 / 未派出 M 个"。
- 每条也走隐式复用解析（3106-3110）。

## 4.6 dispatchOne 逐点

`dispatchOne(ctx, roleID, domain, task, responsibility, mode, verifyKind, toolsHint, skillsHint, wallClock, reuseAgentID, takeover, opts...)`（2579-2905）：

1. **reuse 分流**（2592-2597）：`reuseAgentID != ""` → 热驻未开启报错；否则 `dispatchToIdleSlot`（idle_pool.go）——idle 唤醒注入任务 / busy 入队 / 不存在报错。
2. 角色解析 + worktree 约束（开关关闭拒；热驻 domain 拒）。**派前双校验**（dispatchOne 入口，2026-09-18/19 实测修复，拒绝发生在配额/树登记前零消耗）：
   - `checkRoleTaskFit`（2588）：任务文本的工具需求信号（powershell/get-content/grep/sed/go test/npm/pip/writefile/写入文件等直写形态）× 目标角色工具面，不匹配硬拒并给改派方向——防"派 scout 执行 PowerShell"式能力错配整波空转；
   - `checkTaskPathsFit`（2640）：任务文本提取零歧义绝对路径信号（Windows 盘符 + POSIX 绝对路径正则，带空格路径截断但根前缀足够判定），越出会话 workDir 且角色工具面无 RunCommand 时硬拒（tools_hint 预挂的也算，`hintHasTool`）——防子 Agent 跟 "path escapes sandbox" 缠斗空转（实测 3 个 code_reviewer 84 次调用零交付）；拒绝消息给三选一处置：改派 domain / ask_user 请用户切换会话 workdir / Copy-Item 摄入工作目录；`pathWithinWorkDir` 为 Windows 大小写不敏感前缀比较；
   - **决策层③派发门灰区补充**（decisionGateDispatchGap，2026-09-23 TODO #23 切入点3）：两条硬拒**保持 rules-first 不动**，决策层只补规则正则覆盖不到的需求信号（needs_browser/needs_mcp/needs_vision）；影子期只落对拍行（actual=none=现状放行）零行为变化，enforce 点命中缺口且置信达标才拦（fail-open 故障不拦派发）。
3. **scout 类缺省墙钟**：`SpecExempt` 角色无显式 wall_clock_min → 5min（light 10min）（2618-2624）。
4. **权限校验**：`CanCall(roleIDFromAgentID(parentID), roleID)`。
5. **域控守卫**（仅 domain，2634-2652）：
   - 依赖门 `checkDepGate`（看板 depends_on 未完成拒，提示等谁）；
   - `checkActiveSiblingDomain`（同父同名活跃 domain 拒——Running/Paused/Idle 算冲突，终态放行）；
   - 复用守卫 `checkIdleDomainReuse`（热驻开启时：命名包含/spec 文件重叠疑似可复用槽 → 拒一次引导 reuse_agent_id；task 含【新领域声明】放行）。
6. **前序失败打捞**（2658）：`withPriorSalvage` —— 同父同 scope 有 Failed/Cancelled 前任时把打捞摘要追加进 task（叶子按角色，domain 按领域）。
7. **接力熔断**（2660-2671）：`bumpDispatchGeneration` 代数（含 takeover 链）≥3 注入【重写评估】强制段；≥4 且 task 无【接力理由】标记 → 拒派（relayDeclineMessage）。
8. **全局派发限额**（2677-2685）：session 级计数超 maxTotalDispatches 拒（超限回滚计数）。
9. **生成 ID**：`parentID/roleID-seq`（全局原子序，2688）。
10. **worktree 创建**（2694-2700）：`createWorktreeForDispatch` 失败按 validation_rejected 拒（不泄漏 pending 计数）。
11. **tools_hint 预挂载**（2706-2710）：`tools.MountForScope(subAgentID, roleID, hint)` ∩ 角色天花板，越界项回告。
12. **技能解析**（2715-2718）：`resolveChildSkills` = 角色固定集 ∪ 父分配集（⊆ 父持有集）。
13. `trackChildStart(parentID)`；**热驻 domain 走 `dispatchHotDomain`（2748-2754）**（常驻 supervisor goroutine，ctx 不带 deadline）。
14. 否则：构造 subAgentCtx（stopCtx 基底 + SessionID + WorkDir〔worktree 时切副本路径〕+ 用户图片带外重注入）；`effectiveTimeout` = min(wallClock/domainReconClock, d.timeout)；`context.WithTimeout`。
15. **树注册**（2788-2803）：`t.Register(Node{...StatusRunning})` + `SetCancel`（goroutine 启动前同步注册）。
16. **任务台账**（2806-2808）：`ledger.RecordDispatch`（仅 Meta 直派入账）。
17. **看板回写**（2811-2820）：`boardAssign` 置 in_progress；takeover 时 `b.TakeoverFrom(old, new)` 迁移未完成条目。
18. `subMeta` + `activity` 注册（meta 角色不注册活动）；`ensurePatrol`。
19. 聚合登记（若 `opts.aggregate`）。
20. **goroutine**（2838-2865）：`runSubAgent` → 聚合兜底收口 → `paused=false` 时 `doneOnce.Do(trackChildDone)`；defer 用 `CompareAndDelete`（防复活/复用时误删新条目）+ `lastWrites/heldSkills` 清理。
21. **墙钟预警阶梯**（2871-2873）与**侦察中点预警**（2879-2897）：见 4.15。
22. 返回 ID（+ tools_hint/skills 越界说明 appendRejectNotes）。

## 4.7 runSubAgent 生命周期（结果分支全表）

`runSubAgent(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind, started) bool`（3203-3392）：

先 `runSubAgentWithAutoRetry`（3400-3413）——叶子助手 `kind=error` 失败自动重跑一次（同任务，`dispatchRetryCount`）；domain/meta/timeout/killed/loop_guard 不自动重试。

结果分支：

| 条件 | 行为 |
|---|---|
| `errPaused`（domain 触 token 上限） | 不 notify、不 treeFinish、不 trackChildDone（父 PendingChildren 保持 >0 → 父暂停链路）；返回 true |
| `errPartialReturn`（叶子触限） | `saveTerminalHistory` + treeFinish("部分完成") + notify 部分产出；返回 false |
| `context.Canceled` 且 `isPauseRequested` | **手动暂停**：`savePausedHistory`（脱离取消 ctx，10s 超时）+ `tree.Pause` + pokeParent + 邮件通知父（"已暂停，可 set_agent_model/resume_agent/cancel_agent"）；返回 true |
| `context.Canceled` 且 `isSoftStop(sid)` | **软停止**：domain → 存 history + tree.Pause + pokeParent（返回 true）；叶子 → treeFinish Done + notify "已被软停止…部分成果" + 热驻域额外"子 Agent 已取消"提示邮件 |
| `context.Canceled` 其他（硬取消/心跳杀） | 清暂停残留标记 + `saveTerminalHistory`（仅存史），通知/树收尾由取消方负责（防双通知） |
| 通用 err | `salvageFailure`（打捞摘要双路：写槽位 `<parentID>:salvage:<domain>` + 失败消息）；`renderLegacyList`（结构化遗产清单）；`failureKindOf` 分类 + retryable 判定（kind==error ∧ 非 domain/meta ∧ ctx 未取消）；`formatSubAgentFailure` 文案；**决策层②失败处置路由**（decisionGateFailureDisposition，2026-09-23 TODO #23 切入点2：Choice retry/redelegate/escalate/suspend 建议——failureKindOf/retryable **rules-first 不动**，enforce 点把【处置建议】段附进失败消息供父 LLM 裁量，影子对拍 auto-retry 实际发生与否）；**unverified/verify_missing 附产出全文**；看板/树三态（unverified→`StatusUnverified`/TaskUnverified，否则 Failed）；saveTerminalHistory + treeFinishStatus + notify |
| 成功 | 看板 Done；saveTerminalHistory；treeFinish；**domain 且终答缺【未验证项】段 → 追加机器警告行**（`appendUnverifiedWarning`）；`MachineCheck` 拼进摘要；`VerifyNote` 前缀【校验:通过(...)】；worktree 收尾 patch 附言（`worktreePatchNote`）；`notify(parentID, subAgentID, summary, files)`（files=FilesModifiedFromHistory） |

## 4.8 runSubAgentOnce（纯执行路径）

（3485-3878，供 runSubAgent 与 ExecuteChild 共用）

1. `providerForAgent`（实例级覆盖优先）；memory 空实现兜底。
2. **domain 命名**（3504-3532）：`roleDef.Name = "<domain>领域Agent"`（无 domain 用 task 首行）；responsibility 非空则**追加身份头到 SystemPrompt 末尾**（置末尾保前缀缓存）。
3. **不注入用户人格**（soul）给子 Agent（身份混淆实证，3533-3538）。
4. **黑板摄取包装**（3542-3548）：domain + 非空 domain + searcher 实现 BlackboardSearcher → `newSiblingUptakePipeline`（每轮 Assemble 末尾按 scope 注入【兄弟产出】；**【兄弟产出】逐条包 `WrapUntrusted("block-memory")` 围栏**，2026-09-20——兄弟产出是 LLM 生成文本，可能是被污染外部源的二阶转述；与【相关记忆】召回同经 `renderRecalledMemory` 一处收口，头部行留围栏外）。
5. 构造 `ReActAgent`（3549-3598）：`NewToolRegistryAdapterForRole(tools, subAgentID, roleDef.Tools, roleDef.ID, pluginVisibility)`；`WithProviderFunc`（每次调用重解析实例级 provider）；mailbox/memory/loopConfig/workDir；`WithSkillBlock`；`WithPendingChildrenChecker(d)`（子 Agent 也可能递归派发）；`WithActivityReporter(d.activityReporterFn(...))`（活动冒泡）；`WithMessageLogger`；`WithLogger`；**liveFn 包装**：先 `recordFileWrite`/`recordRecentActivity` 再转发会话。
6. `running.Store`；defer Purge mailbox。
7. **前缀拼装**（3607-3671）：`origTask` 留档 → `recordParentSpec`（抓 spec 切片）→ 依次拼：`projectBriefPrefix`（AGENTS.md）→ `buildSharedPrefix`（spec+共享记忆）→ `projectPrefsPrefix`（项目偏好）→ `skillRecallPrefix`（相关经验）→ `injectScopedRecall`（块记忆召回）→ domain 标签行 → `\n\n【当前任务】\n` + task；五路 runes 记账日志（ctx_inject）。
8. `resolveVerifyKind`（3965-3981）：显式值优先；auto → reflection=rubric / 代码类角色=executable / 其余 none。executable 时 task 尾部追加证据格式模板（`tool.VerificationEvidenceTemplate`，与识别器同源）。
9. `runEngine`（4.9 不在此，见 engine 章/第 02 章 2.9）。
10. **L0 校验**（3693-3709）：executable 且无 `HasExecutableVerification` → 1 轮反馈重试 → 仍缺 `errVerifyMissing`；通过记 `VerifyNote="L0 证据"`。
11. **LimitReached**（3711-3736）：domain → 存 history + tree.Pause + `errPaused`；叶子 → 部分产出塞 Text + saveBlockMemory(partial) + `errPartialReturn`。
12. **冒烟层**（3742-3783）：`smokeTargetsFor` = spec.files ∩ 实际写入；`runSmokeChecks`（node --check 等）失败 → 反馈 1 轮 → 仍败 `errSmokeFailed`；成功渲染【机器校验】段；**JS/HTML 引用完整性第三档**（`runJSRefChecks`）。
13. **验收分层**（3790-3866，spec verify_levels）：integration（入口引用图，失败走 errSmokeFailed 路由）；runtime（探针证据机器强制 `HasRuntimeProbeEvidence`，缺 → 重试 1 轮 → `errVisualEvidenceMissing`）；visual（场景化去重判定 `visualEvidenceCheck`）；**验收条目计分**（`scoreAcceptance` N/M + qualityBlocked）。
14. `memory.Write(task_goal_summary)` → `saveBlockMemory(success, files)`。

## 4.9 心跳巡检与假死击杀

- `patrol`（863-878）：ticker 周期 `heartbeatTimeout/2` 调 `scanStuck`。
- `scanStuck`（888-919）三判定（leaf 用 heartbeatTimeout；domain 用 domainHeartbeatTimeout=2×）：
  1. `llmInFlight` → 豁免（只展示不杀）；
  2. 工具在飞超阈值（toolStartTS）→ 杀（真挂死工具）；
  3. 步间静默超阈值（lastTS）→ 杀。
- `killStuckSubAgent`（925-995）：LoadAndDelete subMeta → cancel（nil 安全）→ doneOnce 兜底 trackChildDone → **构造 kill 消息**（含近期写入文件清单"盘上产物大概率可用" + 杀前活动摘要）→ notify 父 → tree.Finish(Failed) → clearAgentModel → salvageFailure → **`killDescendants` 级联杀后代** → 清 activity/running/mailbox。
- 活动冒泡（`bubbleActivity`，688-704）：子活动沿 parentID 链向上 stamp("descendant")，使 domain 等子期间保活（防误杀合法等待）。`child_wait` kind 只标记展示态（不刷 lastTS、不冒泡）。
- `PingActivity`（832-841）：等用户答复（审批/提问）期间由会话层周期调用，以 `user_wait` 证据防误杀。
- `ClosePatrol`（904-，2026-09-18 修复）：`patrolMu` 串行化 stop channel 的 close + 置 nil——并发/重复调用曾可 close-of-closed panic；patrol goroutine 启动即持 channel 引用（原实现 select 每轮重读字段，置 nil 后 `<-nil` 永久阻塞、ticker 照走，巡检 goroutine 泄漏）一并修掉。

## 4.10 暂停 / 恢复 / 复活

三种"停"语义区分：

| 入口 | 标记 | 收尾 | 可恢复 |
|---|---|---|---|
| 会话软停止 `ReactService.Stop` | `SetSoftStop(sessionID)` | domain→Paused(存史)；叶子→部分回灌 | domain 可 resume |
| 节点手动暂停 `pause_agent`/PauseAgent | `MarkPauseNode(nodeID)` | 同上（该节点） | 可 resume |
| `cancel_agent`/硬取消 | tree.Cancel | 仅存史 + 失败通知 | 否（可复活重跑） |

- `ResumePaused`（4080-4239）：前置（sid/tree/msgStore）→ 续跑次数检查（达 `maxPausedResumes` → `concludePaused` 强制收口）→ `msgStore.LoadMessages` 重建 → domain 名覆写 + 黑板摄取包装 → 构造 ReActAgent（同 runSubAgentOnce 接线）→ `t.Resume` + activity 注册 → `RunWithHistory(ctx,"继续",msgs)` → 三态：再触限（存史+再 Pause+notify）、出错（treeFinish+notify+trackChildDone）、完成（落史+tree.Finish+notify+trackChildDone）。
- `resumePausedNode`（4248-4272）：异步 goroutine 包装 + 失败兜底通知（节点仍 Paused 时补失败邮件，防永久挂账）。
- `ReviveWithMessage`（1102-1200）：用户直连复活终态节点——`tree.Reopen` + SetCancel；种子 = 原任务 + 检查点账本/上轮结果（按处置档，TODO #20④）+ 用户消息；trackChildStart；**mailbox.Reopen**（原 run 已 Purge，不重开则死信）；**msgLogger.Clear**（seq 重新编号）；**`msgStore.ArchiveMessages`（#20① 起；原名 DeleteMessages，2026-09-17 引入时是物理删）把旧 run 的终态消息快照移归档 `archived=true`**——底账 append-only 不物理删，读路径只取 archived=false 防旧快照续上；只归档该子 Agent，会话级消息保留供进程重启恢复；看板翻回进行中；goroutine runSubAgent(mode=react)；给父发"复活返工，勿重复派发"邮件；ledger 重记。
- `InjectUserMessage`（1081-1096）：From="user" 邮件 + pokeParent。

## 4.11 失败分类与结构化失败标记

`FailureKind`（4310-4334）：`timeout` / `error` / `budget_partial` / `killed` / `loop_guard` / `unverified` / `verify_missing` / `smoke_failed` / `contract_violation`。

- `failureMarker(kind, retryable)` → 消息头机读行 `[failure kind=X retryable=Y]`，人读文案在后（3327）。
- `failureKindOf`（4337-4362）：`isLLMCallDeadline`（ctx 存活但 err 含 "llm generate"/"Client.Timeout"）优先归 error（区分"模型层故障"与"墙钟超时"）；依次 errors.Is 匹配哨兵。
- 哨兵错误：`errPaused`/`errPartialReturn`/`errUnverified`/`errVerifyMissing`/`errVisualEvidenceMissing`（归并 verify_missing）/`errSmokeFailed`（4371-4405）。
- `formatSubAgentFailure`（4427-4453）：各 kind 独立文案；超时/循环守卫/通用失败附部分进度（`partialSuffix`）。

## 4.12 spec 体系（WriteSpec 强制门）

- 键规则 `specKeyFor`（4559-4565）：domain 非空 → `"<parentID>:spec:<domain>"`（多 key，兄弟各持各的、staleness 隔离）；空 → 遗留单键 `"<parentID>:spec"`。
- `hasFreshSpec`（4835-4869）：主键校验失败时——domain 空时"唯一 keyed spec 候选回退"（reuse 派发场景）；domain 非空时回退遗留单键，再"唯一候选回退"（附 key/domain 错配警告）。
- `checkSpecKey`（4898-4918）：墓碑值（`SpecTombstonePrefix`，Layer 2 失效留痕）报 "spec invalidated"；frontmatter 解析失败 invalid；`staleFilePaths(fm.Files)` 非空 stale（列失配文件）；缺 goal/acceptance invalid。
- `specMissingMessage`（4923-4939）：列出该 parent 全部现存 spec key（key/domain 错配诊断）。
- `specMirror`/`parentSpecRecord`（4573-4602）：本地镜像避免反向 import tool；派发时 `recordParentSpec`（4609-4660）捕获（FileList 全量 → 绝对路径；否则 Files keys）。
- 冒烟目标：`smokeTargetsFor` = spec.files ∩ 实际写入（4687-4697）。

## 4.13 共享记忆前缀注入

`buildSharedPrefix`（4713-4822）：

- 遍历 `sharedMem.Keys(ctx)` 中 `parentID:` 前缀槽：
  - 墓碑键不注入；`salvage:` 槽不通用注入（同域重派显式读回）；`spec:<domain>` 非本领域不注入。
  - 无 frontmatter（旧格式）→ spec 槽跳过、其余直接当内容。
  - spec 槽：stale 丢弃；需 goal+acceptance；渲染为【任务规范】段（`renderSpecPrefix`，含"【范围】你的职责只在本任务 task 正文"锚定）。
  - 普通槽：stale **不再丢弃**，前置"【失效警告】行号可能漂移"（`sharedStaleWarning`，4542）。
- 缺 `file_tree` 槽时注入探索提示（4803-4806）。
- 总长上限 30000 runes 截断（4813）；尾部固定【读取纪律】（`sharedPrefixDisciplineNote`，4549：注入内容视为已验证禁止重读）。

## 4.14 块记忆召回与沉淀

**召回**（`injectScopedRecall`，5229-5279）：
1. 优先 `BlackboardSearcher.Query(ctx, sid, parentID, domain, query=task, topK=3, "")`（scope 确定性：parent_id+task_domain 过滤 + 可选语义排序）；
2. 空则回退 `SearchBlockMemoryByGoal(sid, query, topK)`（纯语义）；
3. 不足 topK → `CrossSessionSearcher.SearchBlockMemoryCrossSession`（跨 session 补位，排除当前 session，更严阈值）；
4. `rankBlockMemory`（5165-5176）：outcome 排序（success=0 < partial=1 < fail=2）→ reuse_count 降序 → 新近优先；`bumpReuses` 递增命中 reuse_count；**决策层④摄取打分**（gateUptakeScore，2026-09-23 TODO #23 切入点4：现排序不动，逐候选 Score 相关性——影子对拍 actual=yes=现状全保留，enforce 点低于 score_floor 剔除；siblingUptakePipeline.Assemble 每轮摄取与本播种召回两侧同口径，单次批量往返）；
5. `renderRecalledMemory`（5196-5220）：成功经验/避坑经验两段；头部 `blockMemoryRecallHeader`（5161）防"把旧完成当待办"。

**沉淀**（`saveBlockMemory`，5024-5041）：开关关闭/空内容跳过；**决策层⑤沉淀提取预判**（decisionGateExtractWorth，2026-09-23 TODO #23 切入点5：hasSubstantiveChange 规则门保留为硬底，其后 Noul"值得提取?"预判省无效 LLM 提取调用——影子期照常提取补对拍真值 observeExtractOutcome，enforce 点判 no 且置信达标才跳过；salvageFailure 侧 decisionGateSalvageWorth 同款）；有 factExtractor 先 LLM 提取 1-5 条事实（`saveFacts` 逐条落库 `source=fact_extraction`），失败回退原始全文（`saveRawBlockMemory`，`source=sub_agent_result`）。

- Meta 标签：goal/domain(roleID)/session_id/sub_agent_id/parent_id/task_domain/files_modified/outcome/reuse_count/domain_reuse_count(SKILL域级复用权重)。
- 内容三段式"目标:/角色:/结果:"（各截断 200/500 runes）。
- `saveBlockRecord`（5062-5074）：连续失败 ≥10 次 slog.Warn 告警（链路损坏信号：embedding 端点 404 / 表缺失）。
- `bumpDomainProfile`（domain_profile.go）：files_modified 并进档案；success 时用结论刷新档案摘要。

## 4.15 墙钟体系

- `effectiveTimeout`（2735-2742）= min(显式 wallClock, d.timeout)，domain 无显式时用 domainReconClock 兜底；写进 `subAgentMeta.wallClock`，`effectiveTimeout()`（617-624）供文案报准确上限。
- `wallClockWarnLadder`（2931-2980）：50%/75%/90% 三档向子 Agent 邮箱投递递进收口警告；距派发或到期 <30s 的档位跳过。
- 侦察中点预警（2879-2897）：domain 用侦察墙钟时过半投"停止侦察开始产出"。
- 到期硬杀靠 `context.WithTimeout` → provider 读流报 DeadlineExceeded → failureKind=timeout。

## 4.16 看板回写 / 接力熔断 / 任务台账

- 看板：`boardAssign`（派发即 in_progress）、`boardUpdate`（终态 Done/Failed/Unverified）、`TakeoverFrom`（接力认领迁移旧条目）。
- 接力熔断（`bumpDispatchGeneration`/`relayDeclineMessage`，relay.go）：代数 ≥3 注入【重写评估】、≥4 需【接力理由】。
- 任务台账（`ledger`，task_ledger.go）：`RecordDispatch`（派发）+ `RecordTerminal`（notify 咽喉处一处全覆盖）+ `Render`（`TaskLedgerBrief`，4443-4454，供 MetaAgent 上下文注入；重启后从权威树播种）。

## 4.17 聚合模式与批间校验

- `map_sub_agents`（map_dispatch.go）：N 项经 `dispatchOpts.aggregate` 登记，完成不直发父邮箱而记入聚合器，全部收口后汇一条；notify 未触达路径有兜底收口（防聚合器永久悬挂）。聚合器带 `abandon(flush)` 放弃语义：放弃后已收口与后续到达的项改经 flush 逐条直发（onDone 永不触发），防消息因放弃而丢失。**聚合 record 的 ok 按失败机读标记判定**（notify 咽喉 5966：`ok = !failureMarkerRe.MatchString(summary)`，2026-09-19 修复——墙钟被杀/守卫终止的项不再在"完成 N/失败 0"聚合消息里误标完成，与派发期拒绝口径一致）。
- `call_sub_agents` 波聚合（2026-09-17，C-3a）：同波 domain 项 ≥2 且 `agent.batch_digest_enabled`（默认 true）时整波聚合（batchID=`<parentID>/wave-<seq>`）；全部完成经 `deliverWaveDigest` 汇一条【整合纪要】（`SummaryMerger` 轻量模型按领域归并/冲突单列，失败回退逐领域拼接）经 notify 单条送达父邮箱；成功 domain <2 时 `abandon` 回退逐条直发；拒派项在 okDomains≥2 时事后补记（防全败波中途触发纪要）。台账 Files 经 `LastEntryByChild` 取。
- 跨域契约检查（`maybeRunContractChecks`，513-595）：父下全部兄弟完成（count 归零）且 spec 缓存含非空契约时触发；**变更屏障**（recMtimesMatch，文件已变则跳过该份）；违例按文件归属批量打回；**指纹去重**（`violationFingerprint` = parent+file+detail）：已推过未修复的收敛为"已知违例仍未修复（首次报告于 HH:MM）"升级提示。

## 4.18 隐含约定与坑

1. **通知只有 `notify` 一个咽喉**（5371-5414）：台账终态、聚合拦截、>4000 runes 回传落盘（`.bma/returns/`，摘要留 1500）都在此收口。"未走 notify 的路径 = 台账/聚合会漏"。
2. **doneOnce 防双递减**：巡检 kill 与 goroutine 退出竞争时 PendingChildren 不得为负；且 `CompareAndDelete` 防复活/复用误删新条目。
3. **暂停路径不得 clearAgentModel**（实例级模型覆盖要跨 resume 存活）；终态漏斗才回收。
4. **mailbox 要在复活时 Reopen**，否则新 run 的下游回传全死信。
5. **ID 解析规则**：`roleIDFromAgentID` 用 **LastIndex('-')** 取角色（角色名可含连字符）；`sessionIDFromAgentID` 取首个 '/' 前段。
6. `Context.Background()` 派生语义 + stopCtx 继承，是本文件最容易误判的两条 ctx 规则。
7. 墙钟值藏在多处（wallClock / domainReconClock / timeout / slot timer），报文案必须用 `effectiveTimeout(subAgentID)`。
8. spec 失效（Layer 2）由工具侧删除时**写墓碑值**留痕，不是简单 delete。
9. **`git apply --check --3way` 不模拟三方合并冲突**（2026-09-17 实证：预检放行后真 apply 把冲突标记写进主仓工作区）——三方合并预检必须在临时索引上跑（`GIT_INDEX_FILE` + `--3way --cached`）。
10. **`WaitForAnyChild` 跨波残留信号**（2026-09-18 修复）：notify 语义是"有事发生请重查"（返回 true 只提示调用方重查计数与邮箱，waitForChildren 循环自行复核）——不可改成"消费后重查"（吞掉 poke 必现回归）；仅 count<=0 短路分支落缓冲前加非阻塞 drain，清上波完成/poke 残留信号。
11. **pending 计数不下探负数**：`trackChildDone` 裸 `Add(-1)` 改 CAS 下限循环（重复/过量补偿不得打穿）；`killStuckSubAgent` 的 doneOnce 闭包双层 recover——notify 投递 panic 时原实现会跳过 trackChildDone，父未决计数永久 +1、终结保护空等。
12. **`PurgeSession` 补 pending 前缀整批回收**（2026-09-18）：原 pending map 只增不减，每派发过的节点条目永久驻留 sync.Map；会话整体终结后无竞态，按 `parentID` 前缀清扫。

---

## 4.19 热驻池（idle_pool.go，1449 行）

**核心设计：一个槽 = 一个常驻 supervisor goroutine 的 DomainAgent 实例**。任务完成进 Idle 等复用，按加权 TTL 自然退役。

### 数据结构

- `domainHotConfig`（:44-54）：Enabled / BaseTTL / ExtendPerReuse / MaxTTL / MaxPerSession / TaskQueueLen。
- `domainOp{kind, task, wallClock, resumeMsg, images}`（:59-72）：`opNewTask`/`opResume`/`opDestroy`——supervisor 唯一指令通道（`ops chan` 缓冲 8）。
- `slotState`：`slotRunning`（含 `suspended=true` 嵌套）→ `slotIdle` → `slotDestroyed`。
- `domainSlot`（:92-140）：agent（**跨任务复用**，systemPrompt 已冻结 responsibility 头）/history（终态快照，复用续跑种子）/state/suspended/reuseCount/taskQueue/wallRemain/wallTimer/wallFired/childReported/cancelTask/**cancelPending**（ctx 未绑定窗口内的取消登记）/pendingTask/pendingWallClock/pendingImages/ttlArmed/ttlTimer/ttlDeadline/idleSince/**gear**（进 Idle 固化档位）/parentID/domain/responsibility/workDir/stopCtx。
- `domainPool`：`bySess map[sessionID]map[slotID]*domainSlot`；锁顺序恒为 `pool.mu → s.mu`；`destroySlot` 先取 s.mu、**释放后**才 pool.remove（防死锁）。
- `sessionSuspendState{suspended, wake}` + `slotSuspendGate.Park(ctx)`（select wake / ctx.Done）。

### 流程

- **dispatchHotDomain**（:1395）：buildReuseTask 拼首任务 → 建槽 → 树 Register+SetCancel(destroyFnLocked) → 台账 + **subMeta.Store（cancel=nil！）** + ensurePatrol → **LRU**（idle 数 ≥ MaxPerSession 时对最旧数个投 opDestroy）→ pool.store + `go runDomainSupervisor`。
- **supervisor 循环**（:508-577）：runDomainTask → Done/Stopped 时出队缓冲任务继续 → Suspended 保持 PendingChildren>0 → Failed/Destroyed 则 destroySlot 退出；否则 park 等 ops（opDestroy 退出 / opResume 续跑 / opNewTask 换任务）。**唯一退出路径是 opDestroy**。
- **runDomainTask**（:657-870）：建任务 ctx（stopCtx 基底）→ 绑定 taskCtx/cancelTask + 重置 childReported → **subMeta 换绑新实例（带真实 cancelTask，禁止原地改写）** → `rearmSlotActivity`（重建活动条目）→ armWallClock + tree.SetCancel → **早到指令收口**（cancelPending：暂停则暂存任务文本 + tree.Pause 返回 suspended；否则 destroyed）→ 引擎执行 → stopWallClock → 收尾分支：errPaused（存史+tree.Pause+冻结墙钟+suspended+**SuspendSession 全树挂起**）、Canceled（软停→存史+看板+notify+**enterIdle**；wallFired→wallClockWrapUp 收口；手动暂停→存史+Pause+不 SuspendSession；硬取消→destroyed）、其他 err（salvage+三态化）、成功（存史+看板+saveBlockMemory+notify+**enterIdle**）。
- **enterIdle**（:347）：Running→Idle、记 idleSince、**固化 gear**、清 TTL → `activity.Delete`（巡检豁免）→ `tree.Idle(id, summary, killFn)` → armTTL。TTL = `min(BaseTTL + reuseCount*ExtendPerReuse, MaxTTL)`，到期 AfterFunc 投 opDestroy。
- **dispatchToIdleSlot**（:1209）：槽存在性 + 跨会话校验 → **刷新 stopCtx**（会话恢复后旧基底已取消）→ 取图片 → idle 分流（reuseCount++ / 停清 TTL / buildReuseTask / tree.Wake / trackChildStart / rearmSlotActivity / 投 opNewTask（满则**回滚 trackChildDone** 并拒）/ 台账"续建#N"）/ running 分流（队列满拒；否则入 taskQueue + 台账"入队第N位"）。
- **WakeIdleWithMessage**（:1297，编排页直连）：热驻未开/槽不存在 → ErrAgentNotDirectable；running → ErrAgentBusy；idle 同复用（任务文本 = `【用户直连消息】`）。
- **挂起/恢复**：`SuspendSession`（置挂起态 + **冻结 idle TTL**）；`ResumeSessionAgents`（close(wake) 广播 + armTTL + suspended 的 running 槽投 opResume"继续"）；`DestroyAllIdle`（Shutdown 调用）。
- **destroyFnLocked 三态分流**（:387-408）：Idle → 投 opDestroy；Running 且有 ctx → **直接 cancel**（不能投 opDestroy——会排队到任务结束才生效）；Running 无 ctx → 记 cancelPending。

### 生命周期图

```
dispatchHotDomain 建槽 → slotRunning ──DONE/软停──→ slotIdle ──TTL 到期/LRU/会话删/Shutdown──→ slotDestroyed
                            │  ▲                    │  ▲
                     errPaused/手动暂停(suspended)   复用唤醒（reuseCount++ 延寿）
                            └──opResume──┘            └───────────┘
                          失败/硬取消/墙钟 → slotDestroyed
```

## 4.20 活动证据与假死判定（activity.go）

`activityEvidence`：`lastTS`（最后真实步进）/`lastKind`/`llmStartTS`/`llmInFlight`/`toolStartTS`/`toolName`/`waitingChildren`。

- `report(kind,now)` 分类记录；**`keepalive` 不刷 lastTS**（防盲报续命）；`markChildWait` 只换 kind；`stamp("descendant", now)` 祖先冒泡保活；`beginAuxLLM/endAuxLLM`（引擎 judge/plan 辅助 LLM 在飞豁免）。
- 巡检（dispatcher.patrol/scanStuck）：域用 domainHeartbeatTimeout（默认 2× 叶子）；三分支：llmInFlight 豁免 / 工具在飞超阈值杀 / 步间静默超阈值杀。
- 展示面：`ActivityEvidenceOf` 供 ListAgents 填活动字段（编排页小字）。
- `activityReporterFn` 闭包必须**按调用 Load**（捕获指针会写进被 Delete 的废弃条目）。

## 4.21 复用守卫（reuse_guard.go）

`checkIdleDomainReuse`（仅新建 domain 且热驻开启时）：早退=`【新领域声明` 标记；对每个 idle 槽算**双信号**——①命名包含（相等或全串包含，短侧 ≥2 rune；game-core vs game-ui 不命中）；②**spec 文件 ∩ 槽 24h 内写入文件**重叠（路径归一后互为目录后缀判重；归一用 `strings.ReplaceAll(p,"\\","/")` 而非 `filepath.ToSlash`——后者在非 Windows 平台不转反斜杠，2026-09-17 Linux CI 修复，与 `plugins.ExpandWorkDir` 同口径）。任一命中 → 拒绝 + `reuse_agent_id` 指引 + 逃生口说明。

## 4.22 领域档案冷复活（domain_profile.go）

`applyDomainProfileSeed`（热驻/同名复用未命中才走）：三级匹配——名字精确 → 别名 → **路径重叠**（任务文本路径 token + spec files ∩ 档案 Files，**≥2 命中**才算，并列歧义不命中）。命中后 domain 归一化为档案正名 + 种子段：历史摘要（300）+ 子项目 + **逐文件 stat 核对存活**（cap 8）+ 既有结论链（domainMemoriesFn cap 3）+ 衔接纪律行。增量写入 `bumpDomainProfile`（files 并集；success 才接管 Summary）。

## 4.23 接力熔断（relay.go）

`bumpDispatchGeneration(parentID, domain, takeover)`：键 `parentID\x00domain`；takeover 时旧键计数**累加到新键**（异名接管链不断档）。代数 ≥3 注入【重写评估】；≥4 且 task 无【接力理由】拒派。另有 `unverifiedSectionMissing/appendUnverifiedWarning`：domain 终答缺【未验证项】段时在 MachineCheck 追加机器警告（可观测不硬拒）。

## 4.24 worktree 隔离派发与合并门（worktree.go / merge_worktree.go）

- **建**（`createWorktreeForDispatch`）：git 仓库校验 → 名字 `session-domain-unix` 净化 → 路径 `<repoRoot>/.bma/worktrees/<name>` + 分支 `bma/<name>` → `git worktree add`（失败 prune+RemoveAll）→ 记 BaseCommit → 句柄登记。子 Agent workDir 切副本（**主目录零写入**）。
- **交付**：`git add -A` + `git diff --cached <base>` → patch 落 `<副本>.patch` + stat → 成功摘要追加【worktree 交付】附言。
- **review**：句柄 Removed 报错；未产出 patch 时读副本实时 diff；工具层截断 20000 runes。
- **合并门 `mergeWorktree`**：幂等 → **base 漂移检测**（主仓 HEAD ≠ BaseCommit → **3way 回退链**，2026-09-17 P1-5）→ 补产 patch → **契约静态检查**（只跑"契约文件 ∩ patch 变更清单"非空的记录）→ 未漂移 `git apply --check` + `git apply`；漂移 `git apply --3way`（预检在**临时索引**上跑：`GIT_INDEX_FILE` 指向主索引副本 + `--3way --cached`，冲突只落废弃副本）→ 移除副本 → Merged=true。真冲突才拒绝并保留副本提示人工 rebase/重派。
- **驳回**：comments 经 mailbox 回该域；槽存活 pokeParent；副本 patch 保留。
- 约束：与 reuse_agent_id 互斥、与热驻 domain 互斥；git 缺失/非仓库**明确拒绝不降级**。

## 4.25 失败打捞（salvage.go）

- 提取（`salvageFailure`，失败/被杀路径）：LLM 提取"已读文件/已得结论/卡点"（cap 2000 runes；History nil 不跑；默认 30s 超时）；双写——shared slot `<parentID>:salvage:<scope>` + 黑板 `KnowledgeRecord{outcome:fail, source:"salvage"}`。
- scope = domain 非空用 domain，否则 `role.<roleID>`（同角色跨 domain 共享打捞）。
- 注入（`withPriorSalvage`，派发时）：黑板 Query 优先（≤2 行）→ 前缀【前序探索摘要】；黑板空回落 slot。

## 4.26 任务台账（task_ledger.go）

- `RecordDispatch`（**仅 Meta 直派入账**）；`RecordTerminal` 在 **notify 唯一咽喉**（mailbox nil 检查之前）；容量 64（满则逐最旧终态，保 running）；渲染窗口 15 条。
- kind 解析：`failureMarkerRe` 解析 `[failure kind=X retryable=Y]`——unverified/verify_missing 记 `unverified` 而非 failed。
- **重启补种**：进程重启后首次 Render 从权威树 seed（树里 Running 的记 cancelled + "进程重启时仍在运行，视为中断"）。
- 纪律头：「与旧对话记忆冲突时以本表为准」「完成项禁止重派/重验收」「用户新消息只含其字面需求」。

## 4.27 验收闭环（acceptance.go + demo.go）

- 开关（`.bma/tester.yaml`）：`off|auto|on`；auto 由轻量 LLM 判（只有回答含 "YES" 才算）；**LoadTesterConfig 任何错误 → off**。
- `RunWrap`：tester 自身失败/超时/无【验收结论】→ **警告式交付不阻塞**；PASS → demo 阶段；FAIL → 解析【错误清单】`[agent:<id>]` → `reworkFailures`（**Tree.ReviveWithMessage 同 ID 复活** + cc Meta）；轮次耗尽（默认 2）→ 拼【未验证项】清单仍交付。
- 验收任务文本带【接力理由】（防接力熔断误拒）；tester 墙钟 15min；`collectAcceptanceRoster` 排除 test_assistant。
- **演示阶段（demo.go）全链 fail-open**：确认（跳过词优先）→ 派 test_assistant 录播（Playwright recordVideo → ffmpeg gdigrab → 截图序列；解析 `【演示产物】/【演示摘要】` 契约）→ 评审卡（超时/未答复 **视为通过**——与计划确认相反的默认值，有意为之）；打回走归因派发。

## 4.28 机器校验闸门顺序（交付质量后半程）

子 Agent 完成后按序过闸（先过先出），任一不过反馈重试 1 轮：

1. **L0 可执行证据**（executable 角色）：`HasExecutableVerification` 缺 → 反馈 → errVerifyMissing。
2. LimitReached 分支（domain 暂停/叶子部分返回）。
3. **冒烟层 L1**（smoke_check）：`spec.files ∩ 实写文件` → `node --check` / `tsc --noEmit` / `gofmt -l`（**空输出=通过**，唯一 outputEmptyMeansPass）；失败 → errSmokeFailed。
4. **JS 引用完整性第三档**（≥300 行 .js/.html）：有 tsc 硬判（`--allowJs --checkJs`），无 tsc 降级扫描只报"存疑"。
5. **集成层**（integration_check，spec 含 .html 才跑）：`<script src>` 可解析 + 入口 depth-1 import 图可解析。
6. **runtime 层**：`HasRuntimeProbeEvidence`（browser_navigate 成功 + browser_evaluate 断言成功 + console 回读无 error）缺 → errVisualEvidenceMissing。
7. **visual 层**：scenes 非空时按**内容指纹去重 + 数量覆盖 + 邻接证据**（同图连拍无效）；空则单截图判定。
8. **acceptance 逐项计分**（`scoreAcceptance`）：条目 `[evidence:X layer:Y]` 分项等权；`quality` 层缺证据 → **硬判 errVisualEvidenceMissing**（整体不得标绿）；报告"dispatcher 计算，非 agent 自述"。

**跨域契约静态检查**（contract_check）：兄弟域全部完成（count 归零）异步触发；检查 symbols（字面量优先/多段逐词边界/末段大小写不敏感）/DOM ids/script 顺序（basename 严格递增）/signatures（字面量→去空白→去行注释；**.ts 失配由 tsc 仲裁降级为"存疑"**）/桩标记（占位/placeholder/待实现/not implemented/…）；**变更屏障**（文件已变跳过）；违例 `retryable=false` + 指纹去重（同指纹只推一次，再推升级文案）。

## 4.29 计划确认与执行计划（plan_confirm.go / plan.go）

- `submit_plan`：未启用（planState nil）**直通**；顶层 → ask_user（approve/revise，超时/未答复 **fail-open 视为批准**）；子 Agent → mailbox MsgRequest（ThreadID=planID、Priority 10、30s PingActivity 保活、10min 超时 fail-open、ctx.Done 报错）→ 父 `review_plan`（caller 必须等于 waiter.parentID；reject 必带 feedback）。驳回次数超 maxRevisions → 升级仲裁。**死信修复（2026-09-17）**：审批请求投递后经 dispatcher `sessionWakeFn`（bootstrap 接线 `WakeSuspended`）唤醒挂起的父会话——`pokeParent` 只够 waitForChildren 轮询、够不到 SuspendOnChildWait 挂起的顶层会话（此前每子白等 600s fail-open）。
- `write_plan`：校验（id 空/重复/依赖未知/环检测）；`check_dep_gate` 仅对 domain 生效；`board_update` 终态回写（Unverified→MarkUnverified、其他→MarkFailed）；`boardAssign` 派发即 in_progress（跳过 Done）。**依赖门就绪通知（2026-09-17，P1-4 轻量版）**：拒派登记 `depWaiters`（键 parentID+domain）；boardUpdate 后 `notifyDepWaiters` 对前置全 done 的 waiter 投 From=system 的 MsgInfo（「依赖就绪…会自动通知」）+ 唤醒挂起父；**不自动派发**（用户拍板）。

## 4.30 map_sub_agents（map_dispatch.go）

worker 池 6 并发 `dispatchOne`；单项上限：默认角色 6 / **scout 32**；**spec 门整波只查一次**；`batchID = <parentID>/map-<seq>`；聚合 `mapAggregation`（按 idx 排序）——整波完成**只发一条 mailbox 汇总**（不逐项通知，防邮箱淹没）；立即派发失败直接 record(false)；notify 命中 aggByAgent 时只 record 不通知，goroutine 兜底收口防聚合器悬挂。**spec_exempt 角色经 map 路径无显式 wall_clock_min 时注入 5 分钟兜底墙钟**（3050，与单派 scout 缺省同口径；显式值优先，端到端硬杀后聚合消息带 timeout 机读标记、单项标败）。

## 4.31 技能工具（skills_tool.go）

`list_skills`（meta 全池 / 非 meta heldSkills / 回落角色固定集）与 `load_skill`（非 meta 且非 learned 必须命中 held 集合；learned 全局可载并自增 use_count）；输出附【同目录资源文件】清单（渐进披露第二层）。

## 4.32 跨 Agent 协作问答（send_message 通道）

**工具面**（`sendMessageTool`，dispatcher.go:1788-1867）：`send_message{to_agent_id, subject, body, message_type?, thread_id?}`——`message_type`：默认 `request`（期望回复）/ `info`（单向通知）/ `reply`（回复）/ `escalate`（升级求助，父侧收 `[升级]` 前缀）；发送时自动 `ReplyTo=发送方ID`；返回消息 ID；目标已销毁返回 `消息未送达: X`（死信可见，不静默消失）。**问句类型提示**（2125-2133，2026-09-17）：发送文本命中问句特征词（`inquiryMarkers`）而 message_type 标成 reply/info 时，工具结果就地附 `mislabeledInquiryHint` 提示——"对方不会按询问处置（不触发当轮必须回复纪律），你会等到沉默"，防类型错标导致沉默回复。**叶子角色无此工具**（工具面未变）。

**语义**：异步投邮箱，收件方下一轮 `drainMailbox` 才读到，**提问方不阻塞**；多轮问答靠 `thread_id` 聚合、`ReplyTo` 指向被答复消息（mailbox 本身纯透传不做配对校验）。

**提示词纪律**（2026-09-16 启用）：
- DomainAgent【跨 Agent 协作问答】：可直连**兄弟域与上级**（不必经父 Agent 转达）；**收到 request 当轮必须 reply**（带 thread_id，答复自包含）；提问要自包含。
- MetaAgent【派发铁律】："跨域问答直连，不必经你转达" + "你本人收到 request/[询问] 类消息时按【等待期纪律】只处理该事务并立即回复"。

**事件与观测**：drainMailbox 对 `MsgRequest`/`MsgEscalate` 推 `LiveEventPeerAsk`（与 sub_agent_done 区分——"需要我回答"而非"某子 Agent 完成"）→ 会话层落 Message 事件 `kind=peer_ask` + 正文 llm_result 详情；前端 `kindLabel/kindPhrase` 映射（"跨Agent询问"/"收到其他 Agent 的询问"）。mailbox 留痕异步双写 `agent_events`（type=mailbox）；编排页单 Agent 消息接口含 `mails` 留痕。

**机器通道同款**：plan_confirm 的 `submit_plan`/`review_plan` 机器问答走同一条邮箱 request/reply（ThreadID=planID、Priority 10、30s PingActivity 保活、10min 超时 fail-open）——这是此前唯一的 request/reply 系统消费者。**A2A 协议（AgentCard/JSON-RPC）未做**（决策：无跨团队接入需求前不引入）。

**唤醒语义（2026-09-17）**：request/escalate 成功投递给**挂起（awaiting_child）的顶层会话**时经 `wakeSuspendedParent` 唤醒续跑 drain（挂起会话不 drain 邮箱，不唤醒=死信）；**info 不唤醒**（中间信息不值得烧上级一轮）。里程碑播报（domain 提示词纪律：`message_type=info, subject="里程碑: …"`，每任务 ≤3 条）走 info 通道，drainMailbox 单列 `LiveEventMilestone`（kind=milestone，中途播报非完成），前端已映射中文标签。

## 4.33 逻辑检查点与失败分支剪枝（failure_prune.go，#20④）

**逻辑检查点**：派发时 + 里程碑时快照任务账本落 `agent_events`（type=checkpoint，Input=TaskLedgerBrief 快照）+ 内存回读点（`lastCheckpoints`）。落点四处相位：dispatch（dispatchOne/dispatchHotDomain 派发即记）/ reuse（热驻续建/入队/唤醒）/ user_direct（ReviveWithMessage）/ milestone（send_message 发"里程碑:"即记发送方检查点）。审计行不进上下文（`isContextHiddenEvent`）。

**失败处置三选**（`ReviveMode`，对齐 CC rewind 菜单 + /branch）：

| 档 | 触发 | 种子 | 旧轨迹 |
|---|---|---|---|
| prune 剪枝重派（**默认**） | Failed/Err 非空 | 原任务+检查点账本+用户消息 | 移出活跃上下文、底账 archived 可查 |
| continue 同支续跑 | Done/轻微（auto） | 原任务+上轮结果+上轮错误+用户消息（现状） | 留活跃上下文 |
| fork 分叉重派 | 显式 ReviveFork | 同 prune（新支 ID） | 旧支节点与消息**原样保留**可 resume 考古 |

`resolveReviveMode`：空=auto（Failed→prune，其余→continue），显式 prune/continue/fork 直通。截断点只选轮边界（新 run 起步=run 边界；sanitizeToolPairing 请求装配兜底残对）。fork ID 分配防撞（逐个试号跳过树中已存在——进程重启 seq 归零，旧支节点经 PG 恢复树仍在，Register 撞名会覆盖考古入口）。父感知邮件带处置口径（"剪枝重派/分叉重派"），meta 经既有邮件机制感知。

## 4.34 announce 边界协议（announce.go，#22②）

- **规范化回报信封**（`buildAnnounce`，notify 咽喉一处全覆盖）：summary 原文**置顶不动**（`^` 锚定的 failureMarkerRe 等机读消费方依赖标记在首行）；尾部【回报】收口块——Status（`deriveAnnounceStatus` 机械推导 done/failed/partial/delivered-unverified）、Notes（修改文件清单）、统计行（回报 runes/文件数）。
- **回灌预算公式**（Hermes 同款）：`announceBudgetRunes(子数) = clamp(父剩余 150K×0.5÷子数, floor 2K, 静态封顶 4K)`——高扇出每份回报自动收窄，防 N 子回传同灌父上下文；超预算全文落盘 `<workDir>/.bma/returns/`、邮箱留摘要头+路径。
- **派发前上下文预算**（`capSpawnPrefixes`）：前缀+任务合计超 fork 硬顶 100K runes 转 **isolated**——前缀全弃（不截断硬灌），任务尾附隔离说明引导 ReadFile 按需取。首派（runSubAgentOnce）与热驻续建（buildReuseTask）同口径。
- **minimal 提示词面**：人格/用户画像不入子上下文（bootstrap metaPersona 仅注 MetaAgent，runSubAgentOnce/buildDomainAgent 不下发）——既有装配保证，本协议不重复建设。
- **逐级上灌纪律不变**：notify 只发直接父级，跨域走黑板（与 #2 黑板正交，不动 mailbox）。

## 4.35 会话恢复中间层与展开式召回（idle_pool.go / expand.go）

**RestoreSessionDomains**（#21④）：会话 resume 时按 (session_id, agent_id) 从 agent_messages + agent_tree_nodes 重建热驻槽（未终态/终态 30min 内 domain）——ResumeSessionAgents 头部接线，重建槽同样被 armTTL/opResume 扫到（零特判）。与 #17 领域档案构成三级连续体：热驻池（分钟级）→ **会话 resume 中间层（本条）** → 领域档案（跨会话永久级）。

**expand_memory**（#22④ 展开式召回，expand.go）：压缩摘要讲不细时派**只读**展开器沿摘要链（`Pipeline.Bundles`：leaf→condensed 包链副本）定位细节所属段，再用低层工具取原文（workspace 文件 / .bma/returns 全文 / .bma/tool_outputs 落盘件）。约束：delegation grant 双闸（token 预算 8K + TTL 3min，超限截答返回已有部分）+ 答案硬顶 2000 rune + **结构性禁递归**（展开器工具面只给 ReadFile/SearchInFiles/ListDir——无 call_sub_agent/send_message/写工具，派不出下级）。与 salvage 合成完整记忆环：失败轨迹 salvage 主动重注（现状），成功细节 expand_memory 按需展开——一推一拉，压缩不再等于失忆。meta/domain 白名单放行。
