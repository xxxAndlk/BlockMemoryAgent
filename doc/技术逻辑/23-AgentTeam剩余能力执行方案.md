# AgentTeam 剩余能力执行方案

> 日期：2026-11-03  
> 范围：**B-11（最高优先级）**、U-02、B-03、C-03、C-05、C-06。
> 前置文档：`doc/技术逻辑/22-AgentTeam问题与修复清单.md`，尤其是第 11 节的主路线、11.5 节的 C-06 启动门槛和第 12 节的候选技术预案。  
> 文档性质：**开发与验收方案，不是实施完成报告**。本文中的目标结构体、配置项、表名和新增文件均为待实施设计；只有“现状边界”描述的是本次已核对的代码。
>
> **实施状态（2026-11-03）**：
> - **B-11（进程内 + 跨进程保守恢复）**：Domain/叶子/主 Agent 系统触限检查点与同 ID 自动续跑；主 Agent 复用 `wakeInput` 系统来源；任务级共享次数/无进展/墙钟/Team token 熔断；paused-pending 不触发父 `paused_on_child`；同任务防重派；`continuation_*` 审计。**复核修复**：二次触限复位 single-flight、保留 task/attempt 绑定、共享 root/子预算与原始 deadline、无进展按成功输出内容 hash、context_threshold 有界续跑视图、Domain 阻断通知父。跨进程：启动标 interrupted；默认不重放并显式 blocked，开启时建新 attempt_no。
> - **U-02**：Manifest v2（shadow/enforce）、来源登记、确定性裁决、optional 裁剪、结构化记忆视图、v2 审计；**运行中 required 失效停止**：enforce 下每轮比对 AGENTS.md/任务规范内容 hash，变化即停止后续执行并交上级（`context_invalid` → board blocked）。恢复专用重检仍走新派发重建。
> - **B-03**：逐项 VerificationReport、AgentResult v2、团队风险策略契约；`verification_policy_mode` 默认 shadow，enforce 仅阻断 C-03 绑定任务。**复核修复**：门禁前先生成报告；绑定任务缺报告直接阻断；报告携带 task/attempt/risk；无报告 AgentResult 默认 unverified；**恢复完成与首次完成共用同一验收/看板/attempt 终态出口**；自动任务验收清单通过 `TaskBinding.Acceptance` 作为权威输入。
> - **C-03**：精确绑定 + attempt fence + PlanVersion；`team_dispatch_attempts`（仅 PG，建表失败拒启）、`ReadyScheduler`（默认关闭、5s 扫描、固定叶角色、block_dependents、max_attempts 接入）、`DispatchBoundTask`；**人工批准门**（`POST /api/team/approve`，未批准不执行）+ 事件触发/启动 reconcile；`GET /api/team/attempts`、`POST /api/team/reconcile`；**attempt 正常终态回写**（MarkTerminal，done/unverified/failed + result_id）；Web `/team` 页面（attempt 表 + 批准 + 手动调度）；write_plan 校验唯一 integration 且必须依赖其余交付任务。开启时 Manifest/验收必须 enforce，否则 fail-fast。
> - **C-05**：团队候选/严格防复活/shadow 默认/verified 后项目人工晋升/同 key active 原子替换/撤回回滚/曝光事件与聚合 API；**真实同预算 eval**：配置 `memory.eval_tasks` 后接轻量模型 baseline/treatment + judge，报告落 events；未配置时禁止自动晋升。
> - **C-06（用户批准，默认关闭）**：`PlanEnvelope/TaskContract/Claim/ClaimBinding`；内存与 PG 三表（team_tasks/team_task_attempts/team_task_events）事务 Claim/lease/fencing；`ClaimService`、`WorkerSupervisor`（无 LLM 轮询、按角色总运行并发限制）、`DispatchClaimed` 与 `FinishClaim` 终态回写；API `/api/team/self-claim/{plan,tasks,run}`。`agent.self_claim_enabled=false` 默认关闭。
> - 实际改动与验证见 `doc/变更.md` 同日条目；文中其余目标章节仍为设计，不得当成已实现。

## 1. 执行结论

**先修 B-11 执行连续性，再推进其余能力。** 自动续跑与同任务防重派是当前真实使用阻塞点；它可基于现有 `ResumePaused` 独立落地，不应等待 U-02 或 C-03 全部完成。其余五项不能并行各做各的，正确依赖关系是：

```text
B-11 系统暂停自动续跑 + 同任务防重派（P0，独立先行）

U-02 版本化、可裁剪、可撤回的上下文契约 ─┐
                                           ├─→ B-03 分风险机器验收 ─→ C-03 依赖就绪自动调度
现有长期记忆生命周期 + 来源与证据引用 ────┘              │
                                           │              ├─→ C-05 组织学习闭环
                                           │              │
                                           └──────────────┴─→ C-06 是否立项的证据包
```

更精确地说：

1. **B-11 先保证系统触限后无需人工反复“继续”**，同一个 Agent／attempt 自动恢复，父 Agent 不因子 Agent 可恢复暂停而停住，同任务不能重派；同时必须有累计预算和无进展熔断。
2. **U-02 再统一“实际给执行者看了什么”**，否则自动调度只会更快地传播旧约束、冲突经验和无关上下文。B11-2 只需检查 required 上下文有效性，不等待完整 U-02；U-02 落地后接入同一恢复重检入口。
3. **B-03 再统一“什么状态允许完成”**，否则 C-03 会把 Agent 自述或任意一次成功命令当成依赖完成信号。B-11 先保证截断产出不能冒充完成，后续由 B-03 提供完整机器报告。
4. **C-03 先修精确任务绑定，再做自动派发**。现有按 Domain 整组 `Assign/MarkDone` 的语义不能直接用于逐任务自动执行；B-11 的 legacy 防重键在 C-03 后升级为精确 task／attempt。
5. **C-05 复用现有 `long_term_memory_items/events`、撤回和防复活机制**，只补团队候选的证据、作用域、评估、晋升和反馈，不新建第二套长期记忆。
6. **C-06 仍不是默认开发项**。先交付实验决策包；门槛不满足就停止，不为了架构一致性建设队列、Worker 和额外表。

### 1.1 六项工作的交付边界

| 编号 | 本次要交付的能力 | 明确不做 |
|---|---|---|
| B-11 | 暂停原因分类、同 Agent／attempt 自动续跑、父子等待、叶子检查点、防重派、任务级熔断与安全恢复 | 不伪造用户“继续”；不自动恢复用户暂停／审批／硬预算；不重置总预算；不让 LLM 决定续跑 |
| U-02 | 冻结 Manifest、来源优先级、冲突裁决、撤回重检、关键约束保真、全入口审计 | 不用一个更大的 Prompt 代替结构化治理；不重新打开旧块记忆自动写入 |
| B-03 | 风险档、DoD 策略、逐项 VerificationReport、AgentResult v2、旧 v1 兼容 | 不重写现有 runtime／visual／command 证据采集；不让 LLM 自评替代机器状态 |
| C-03 | 精确 task 绑定、已批准计划的确定性 ReadyScheduler、持久 attempt 防重、恢复和取消 | 不做 Agent 自由认领；不做第二条权限旁路；不默认自动重放有副作用任务 |
| C-05 | 团队经验候选、证据引用、shadow 评估、受控晋升、反馈、即时撤回和回滚 | 不把团队产出自动写用户画像；不把一次成功当成普遍经验；不自动全局推广 |
| C-06 | 对照数据、瓶颈证明、适用任务集合、停／走结论 | 未立项前不建完整三表队列、不培养空转 LLM Worker、不改成默认拓扑 |

## 2. 已核对的现状与真正的缺口

### 2.1 已有能力，必须复用

| 模块 | 当前已有实现 | 对后续方案的含义 |
|---|---|---|
| Team Spec | `backend/internal/team/spec.go` 已有版本化 `Spec/Run/RoleSpec`、角色与嵌套校验；默认深度 3、节点 32 | B-03 应扩展这里的 DoD／角色策略，不另建角色包体系；当前尚未接 Dispatcher／HTTP |
| 上下文审计 | `context_audit.go` 已有 `manifest_version=v1`、required／optional／dropped 和来源字符数 | B-08 清单已存在；U-02 是演进为可执行上下文契约，不是再补一张来源清单 |
| 机器证据 | `verify_levels.go` 已支持 command／screenshot／probe／file，已有 runtime、视觉去重和逐项计分 | B-03 组合这些证据形成完成策略；不能把“已有布尔扫描”误写成逐项充分证明 |
| 结果信封 | `agent_result.go` 已有 AgentResult v1，Mailbox JSONB 承载，无需为加字段立即改表 | v2 应保留 v1 字段兼容，新增机器报告，不再从摘要文本推导新路径状态 |
| 任务计划 | `plan.go` 已有 `write_plan`、DAG 校验、依赖门和就绪通知；`board` 有持久化快照与恢复 | C-03 复用计划、看板和 Dispatcher，不另造通用 DAG 引擎 |
| 长期记忆 | `memory_lifecycle.go`、`long_term_memory.go` 已有稳定 ID、items 投影、events 审计、candidate／active／quarantined／superseded／retracted、revision、撤回和防复活 | C-05 不重复建设存储；U-02 要读取结构化条目，而不是再解析渲染后的 Markdown |
| 写入安全姿态 | `memory.write_mode` 默认 shadow，`retraction_mode` 默认 enforce；明确否决会阻止本轮新经验 | 组织学习默认仍为 shadow；撤回能力不能随学习开关关闭 |
| 邮箱 | Mailbox 已有幂等键和持久化 | 可用于结果通知去重，但**不能替代自动派发的 attempt fencing** |

### 2.2 不能在实施中忽略的现存约束

1. `runSubAgentOnce` 的注入不只包括当前审计的五段：`WithDirectiveBlock` 还会每轮注入项目有效记忆，另有技能元数据、角色进化块、运行时名册和 sibling uptake。U-02 必须登记这些入口，不能只审计 `prefixes` 就宣称全链路无污染。
2. 当前 fork 硬顶会把共享前缀整体丢弃，但 task 仍标 included；这对 required 上下文是不安全的。Manifest enforce 模式下，required 缺失必须拒派或显式转人工，不能静默 isolated 后继续。
3. `buildAgentResult` 当前通过失败标记和“未验证”等摘要文字推导状态，成功路径默认 `completed/passed`。B-03 的新路径必须由显式 `TerminalOutcome + VerificationReport` 决定。
4. `scoreAcceptance` 对 legacy 字符串和 manual 条目不计入机器总数；command 证据也是共享布尔口径。B-03 v2 必须能说明每条验收对应哪份证据，不能把“跑过任意测试”当成所有条目通过。
5. `checkDepGate/boardAssign/boardUpdate/depWaiter` 都按 Domain 处理整组任务；`board.SetPlan` 对既有 ID 也不会完整更新标题和依赖。自动调度前必须定义精确绑定和重规划版本语义。
6. `DispatchProgrammatic` 明确把部分参数与 Spec 预检交给调用方。C-03 不能直接把数据库／计划数据送进这个弱前置入口而不补共用预检。
7. 现有 `MemoryLifecycle.CheckResurrection` 在仓储错误时 fail-open，适合旧召回降级，不适合自动晋升。C-05 的晋升路径必须增加 fail-closed 严格检查。
8. 当前有效视图的项目覆盖同 key 全局条目只是合并规则，不等于覆盖任务 Spec、最新用户指令、不同来源冲突和运行中撤回。U-02 需要独立的冲突与失效语义。

## 3. 共用实施纪律

### 3.1 统一标识

后续所有审计、结果、调度和学习反馈至少携带：

```text
session_id / plan_version / task_id / attempt_id
parent_agent_id / agent_id / role_id
spec_version / context_manifest_id / context_manifest_hash
result_id / verification_report_id
```

旧路径没有 task／attempt 时允许为空，但必须标 `execution_path=legacy_delegate`。新 `auto_dispatch` 路径不允许缺这些标识。

### 3.2 统一状态语义

- `done`：交付存在，当前风险档要求的机器证据通过，必需人工审批也已完成。
- `delivered-unverified`：有产出，但缺证据、证据无法关联或验证服务不可用；不是失败，也**默认不解锁下游**。
- `failed`：明确执行失败或机器验证失败；是否可重试由失败类型决定。
- `blocked`：依赖失败、Spec／Manifest 失效、等待审批或需要人工，不是一个新的执行终态。
- `cancelled`：用户或会话显式取消；不得自动重排。
- `interrupted`：进程恢复后发现执行状态不确定；保留证据，不冒充失败或成功。

自动调度只接受 `done` 作为依赖满足。UI 可以展示人工“强制视为满足”的操作，但必须记录操作者、理由和覆盖的缺失证据，不能由子 Agent 自己声明。

### 3.3 每个 PR 的共同交付格式

每个 PR 必须同时包含：

1. 目标行为与不变的旧行为；
2. 代码、配置、Schema 和迁移；
3. 单元测试、真实集成测试和故障注入；
4. 审计事件、排障字段和指标；
5. 默认关闭或 shadow 的回滚方式；
6. 实际执行过的命令、环境和结果，未执行项明确标注；
7. 对 21／22／23 号文档的实施状态同步，不把目标设计写成已实现。

---

## 3A. B-11：执行连续性——系统暂停自动续跑与同任务防重派

> 优先级：**P0，先于 U-02／B-03／C-03 实施**。
> 针对的真实问题：子 Agent 触限暂停后，主 Agent 重新派一个实例；或者主 Agent 也停下来，必须有人反复输入“继续”。
> 当前状态：**仅为待实施设计，尚未修改代码**。本节确认的是代码中存在的故障路径，没有在未读取问题会话原始日志的情况下断言某次事故的每一个暂停都属于同一原因。

### 3A.1 已核对的故障链与可复用原语

当前链路不是“没有恢复能力”，而是**已有恢复能力没有被系统自动、正确地驱动**：

1. `backend/internal/domain/subagent/dispatcher.go` 的 Domain `LimitReached` 路径会保存 History、执行 `tree.Pause`，但不发子 Agent 终态通知、不递减父节点 pending，随后返回 `errPaused`。
2. 父 Agent 在 `backend/internal/agent/react_agent.go` 发现 Paused 子 Domain 后，返回 `LimitReached + PausedOnChild`。
3. 顶层在 `backend/internal/agent/service_react.go` 收到 `Limit Reached` 后统一 `pauseSession`，因此子 Agent 的可恢复暂停会扩大成整个主会话等人工。
4. 叶子执行触限存在另一条路径：截取部分文本并返回 `errPartialReturn`。如果没有可恢复检查点，父 Agent 只能拿部分结果收口或重新派发，容易丢失原实例的进度。
5. `ResumePaused` 已经能按**同一个 Agent ID**恢复 Domain 和固定叶角色，加载持久 History 后继续执行。这是必须复用的原语，不重写第二套恢复引擎。
6. 现有恢复路径会再次使用 `d.timeout` 进入执行门；若直接自动循环调用，会把总墙钟也一次次重置。`maxPausedResumes<=0` 时默认 1 的现有限制也不足以表达整个任务的累计续跑预算。

因此 B-11 需要修的是**暂停分类、触发者、父子等待状态、任务级预算和防重派**，而不是简单把人工“继续”按钮改成定时自动点击。

### 3A.2 目标行为与不变约束

目标行为：

- 对可继续的系统触限，在检查点持久化且旧 run 退出后，由系统自动恢复**同一个 session／node／Agent ID／attempt**，不需要用户输入。
- Domain 子 Agent 自动续跑期间，父 Agent 保持等待该子任务，不把整个主会话置为 `paused_on_child`，也不能趁机重派。
- 叶子 token／上下文触限也保存 History 和部分工作检查点，走同一续跑控制；不得把截断文本冒充完成。
- 主 Agent 自身遇到可恢复的上下文阈值或迭代上限，也按相同原则续跑，而不是整个会话停住。
- 自动续跑有任务级次数、无进展、总墙钟和 Team token 熔断；不能把“少点一次继续”变成“无人值守地无限烧预算”。

不变约束：

- 用户显式停止／暂停、等待澄清、等待审批、权限拒绝和硬预算耗尽，绝不自动续跑。
- 不伪造真实用户消息，不写用户决策账本、项目经验或用户画像。
- 不重置派发计数、Team 累计预算、总重试上限或原始绝对截止时间。
- 旧 run 未退出、检查点未完整落盘、副作用是否提交尚不确定时，不双开执行。
- 只有真正需要人工的原因才进入现有人工暂停；普通任务仍按既定路线优先 `single_writer`，不因 B-11 扩大多 Agent 使用范围。

### 3A.3 先区分暂停原因，禁止统一 LimitReached 后 pauseSession

在中立 Team 契约中增加带原因的暂停信封；`subagent` 只依赖该中立契约，`agent` 中的控制器通过接口适配 Dispatcher，避免形成 `agent ↔ subagent` 循环依赖。

```go
// 目标契约，可放 backend/internal/team/continuation.go。
type PauseCause string

const (
    PauseContextThreshold PauseCause = "context_threshold"
    PauseIterationLimit   PauseCause = "iteration_limit"
    PauseUser             PauseCause = "user_pause"
    PauseApproval         PauseCause = "approval_required"
    PauseClarification    PauseCause = "clarification_required"
    PausePermission       PauseCause = "permission_denied"
    PauseTeamBudget       PauseCause = "team_budget_exhausted"
    PauseWallClock        PauseCause = "wall_clock_exhausted"
    PauseContextInvalid   PauseCause = "required_context_invalid"
    PauseVerification     PauseCause = "verification_failed"
    PauseUncertain        PauseCause = "uncertain_side_effect"
)

type ContinuationCheckpoint struct {
    SessionID       string
    RunID           string
    ParentAgentID   string
    AgentID         string
    NodeID          string
    TaskID          string
    AttemptID       string
    BudgetScopeID   string // 根任务/Team Run 的预算作用域，所有派生节点继承
    OwnerKind       string // root/domain/leaf
    Cause           PauseCause
    CheckpointID    string
    HistoryRef      string
    HistoryVersion  int64
    ProgressDigest  string
    OriginalDeadline time.Time
    ContinuationSeq int
    CreatedAt       time.Time
}
```

`CheckpointID` 标识一次确定的暂停，`ContinuationSeq` 标识第几次续跑。调度幂等键使用 `agent_id + checkpoint_id + continuation_seq`，不能只用 session，否则同一会话不同子 Agent 会互相吞掉续跑。

`BudgetScopeID` 从根任务／Team Run 创建并由全部派生节点继承，续跑和模型重规划都不得修改；legacy 路径可先用 `session_id + 根 Domain ID` 形成稳定作用域，C-03 后映射到精确根任务。这样 root、父 Domain 和叶子消耗的是同一套累计预算，而不是各自重新开始。

| 暂停／中断原因 | 系统自动续跑 | 必要前置与处理 |
|---|---:|---|
| 上下文／token 窗口阈值 | 是 | History 与工作检查点完整；required 上下文仍有效；只重置上下文窗口，不重置任务总预算 |
| 单次 run 迭代上限 | 是 | 当前工具调用已经安全收口；续跑的是同一 attempt，不是新建任务 |
| 子 Domain／叶子发生上述系统触限 | 是 | 父保持 `awaiting_child/continuation_pending`，不得向上转换成整会话人工暂停 |
| 用户显式暂停、停止、取消 | 否 | 只有用户之后明确恢复才可继续；取消旧定时器和唤醒 |
| 等待用户澄清或审批 | 否 | 等待对应真实输入或审批事件，不用系统“继续”替代 |
| 工具／权限拒绝 | 否 | 保留权限边界；按现有失败或人工流程处理 |
| Team token 硬预算或原始总墙钟耗尽 | 否 | 明确 blocked；不得通过续跑获得新预算 |
| required 上下文缺失、Spec／Manifest 失效 | 否 | 等 U-02 重检或人工修正；不能带着残缺约束继续 |
| 重复验证失败 | 否 | 不无限续跑同一实例；由失败策略或人工决定新 attempt |
| 进程重启后状态不确定、副作用可能已提交 | 否 | 标 `interrupted`，先对账；只有 3A.10 的安全条件满足才自动恢复 |

瞬时网络错误仍走现有有限重试策略，不与“上下文／迭代窗口续跑”混为一谈，也不能借重试绕过上述预算。

### 3A.4 ContinuationController：确定性控制，不让 LLM 决定要不要续跑

建议新增 `backend/internal/agent/continuation.go`。控制器只消费结构化检查点和预算状态，不调用模型做续跑裁决。

```go
type ContinuationController interface {
    OnCheckpoint(cp team.ContinuationCheckpoint) error
    OnRunExited(agentID, runID, checkpointID string) error
    CancelSession(sessionID, reason string) error
}

type PausedDomainResumer interface {
    ResumePausedWithOptions(/* 现有 ResumePaused 参数 + ResumeOptions */) error
}

type ResumeOptions struct {
    CheckpointID     string
    Cause            team.PauseCause
    RemainingTimeout time.Duration
    ContinuationSeq  int
    Origin           string // system_continuation/user_resume
}
```

具体接口签名按现有类型收敛，但以下语义不可删除：

1. `OnCheckpoint` 只登记候选；必须等旧 `RunID` 的 `OnRunExited` 后才允许恢复，防止旧协程和新协程同时写。
2. 以 `agent_id + checkpoint_id` 做 single-flight；定时器、恢复扫描和用户操作同时到达时也只能有一个有效续跑。
3. 续跑前再次检查 session 是否被取消、用户是否暂停、任务预算是否有剩余、检查点是否仍为最新。
4. Domain／叶子通过现有 `ResumePaused` 的适配入口恢复；保留同 Agent ID、固定叶角色和持久 History，并新增 `ResumeOptions`，不再无条件获得一份完整 `d.timeout`。
5. 主会话复用现有 `wakeInput` 的**系统唤醒通道语义**，事件携带 checkpoint／cause／origin；不得调用 `sendMessage("继续")`，也不得在用户历史中增加一条“继续”。现有恢复引擎内部的继续指令可以保留，但必须标为系统来源。
6. 续跑完成后沿用原有终态通知路径；控制器不额外构造第二份完成通知。
7. `context_threshold` 恢复不能只是原样重放全部 History 后重新给一份 token 上限，否则可能立即再次触限。必须验证实际模型输入已降到阈值内：完整 History 仍持久保留，模型侧使用有界的 continuation view，包含当前任务与最新用户约束、未决计划／工具状态、已验证进展与工件引用，以及必要的近期工作尾部。
8. continuation view 不创建新 Agent／attempt，也不改写原始 History。B11-2 先用确定性检查点和已有压缩能力实现；任何必需约束无法放入窗口时禁止自动续跑。LLM 摘要只能作为非权威辅助，不能覆盖原始约束；U-02 落地后再由版本化 Manifest 证明保真。
9. `iteration_limit` 只开启新的迭代切片，不无故压缩或重写历史；两种原因的恢复路径和审计必须可区分。

建议将运行状态显式区分为：

```text
running
  └─系统触限→ paused_system/checkpointed
                  ├─不可自动续跑→ blocked/awaiting_human
                  └─可自动续跑→ continuation_pending
                                    └─旧 run 已退出→ resuming → running（同 Agent ID、同 attempt）

任意状态 + 用户取消 → cancelled（清理待续跑事件，永不自动唤醒）
```

`paused_system` 是持久检查点状态，`continuation_pending` 是已安排系统恢复的状态；二者均不是任务完成，也不解锁下游。

### 3A.5 Domain 子 Agent 续跑时，父 Agent 不再跟着停

将现有错误传播改为显式的父子等待协议：

1. 子 Domain 保存 History、`tree.Pause` 和检查点后，发出 `continuation_checkpointed`；仍不递减父 pending、不发送完成通知。
2. 父 Agent 收到带检查点的暂停结果后记录 `awaiting_child + continuation_pending`，等待恢复或终态事件；不再仅因子 Domain 为 Paused 就返回顶层 `Limit Reached + PausedOnChild`。
3. 等子 Domain 旧 run 退出，控制器调用同 ID 恢复。父 Agent 可以继续等待，不重新规划、不创建新子 Agent。
4. 子 Domain 最终成功、明确失败或转人工时，只沿现有通知路径唤醒父 Agent 一次，pending 只递减一次。
5. 若原因不可自动续跑，才向上传 `blocked/awaiting_human`；顶层仅在确实需要用户时 `pauseSession`，错误消息必须包含真实原因和已自动续跑次数。
6. 如果父 Agent 在等待期间自身也触限，先为父保存独立检查点；父、子各自的 checkpoint 和预算分别标识，但都消耗同一个任务累计预算。恢复顺序不得造成父子双开或重复完成通知。

这一改造直接删除“子 Agent 暂停 ⇒ 主 Agent 暂停 ⇒ 用户继续 ⇒ 主 Agent 再决定重派”的默认链路。

### 3A.6 叶子触限不得直接部分假完成

为叶子执行补齐与 Domain 等价的检查点边界：

- 在达到 token／上下文阈值时，保留已持久 History、已成功的工具结果、文件 diff／工件引用和未完成的 DoD 项。
- 返回带 `OwnerKind=leaf` 的可恢复检查点，不直接走 `errPartialReturn` 的完成收口。
- 恢复时沿用同一叶子实例标识与角色，只补“从检查点继续并完成未决项”的系统指令，不重派另一个同角色 Agent。
- 自动续跑预算耗尽后，可保留 `delivered-unverified` 或 `blocked`，但必须列明缺失证据和未决项；不能标 `done`，不能默认解锁依赖。
- 只有用户明确选择“接受部分产出”时才按现有人工部分收口处理，并记录操作者与理由。

在 B-03 尚未落地前，首版至少使用现有结果字段与明确未验证标记，不能等待 B-03 才修假完成。

### 3A.7 主 Agent 自动续跑，但不制造用户输入

主 Agent 的上下文阈值／单次 run 迭代上限也生成同构检查点：

1. 当前工具副作用已经收口，保存主会话 History、待办状态与最新进度摘要。
2. 顶层识别 `PauseContextThreshold／PauseIterationLimit`，先交给控制器，而不是立即 `pauseSession`。
3. 旧 run 退出后，通过内部系统唤醒恢复原主 session；恢复入口与用户新消息入口分流。
4. 恢复时不新增用户消息、不重新执行需求理解、不重新拆队、不重置已派发子任务；先读取现有运行状态和 pending 子任务。
5. 达到续跑／无进展／预算熔断时才转人工，并输出可操作的阻塞原因，而不是只显示“已达上限，请继续”。

### 3A.8 派发前的同任务防重派守卫

自动恢复与防重派必须同时交付，否则仍可能出现“旧实例正在恢复，新实例又被派出”。

在 Dispatcher 创建新 Agent／节点之前增加确定性守卫：

- C-03 已有精确标识后，使用 `parent_agent_id + task_id + attempt_id` 查询执行句柄。
- legacy 路径暂用 `parent_agent_id + Domain ID + canonical Spec hash`；Spec hash 至少覆盖角色和任务内容，避免不同角色被误并。
- 若已有节点处于 `paused_system`、`continuation_pending`、`resuming`，或可安全恢复的 `interrupted`，返回已有句柄或 `ErrResumableTaskExists`；禁止创建新 Agent。
- 已确认不可恢复的阻塞实例，只有显式执行 `abandon/supersede` 并留下审计后，才允许建立新 attempt；普通模型重规划不能隐式完成这一步。
- 守卫与续跑 single-flight 共用同一权威状态，不能一个读内存、另一个读可能滞后的 UI 快照。
- 防重派不依赖 Mailbox 幂等键；邮箱只能保证通知去重，不能保证执行唯一。

### 3A.9 任务级熔断、预算与进展判定

建议首版默认：

```yaml
auto_resume_max_per_task: 2
auto_resume_no_progress_limit: 1
```

语义必须固定：

1. `auto_resume_max_per_task=2` 是**同一任务跨 root／Domain／leaf 的累计系统自动续跑次数**，不是每个节点各两次，防止嵌套 Agent 乘法放大。
2. 与现有 `maxPausedResumes` 统一为一套任务累计预算；迁移时保留一个权威计数，不得两套计数各自重置。配置兼容层可把旧配置映射到新预算，并在废弃期打印告警。
3. 用户手动恢复也不能无声清零计数。确实需要追加预算时，由用户显式授权并审计追加的次数与原因。
4. `auto_resume_no_progress_limit=1` 表示最多容忍一轮“续跑后零可验证进展”；下一次又需要续跑时停止自动恢复并转人工。
5. 有效进展只认：成功工具结果及其新产物、新的文件 diff、新的测试／验收证据、子任务状态实际推进。重复 LLM 文本、重复计划、重复“我将继续”和仅 token 增长均不算进展。
6. `ProgressDigest` 由上述确定性证据计算；相同输出的重复工具调用不能仅靠调用次数算进展。没有新证据时保持原 digest。
7. 自动续跑使用 `min(原剩余墙钟, 当前配置允许的单次执行时间)`；原绝对截止时间不变。`d.timeout` 只能作为单次切片上限，不能作为新的总 Deadline。
8. Team token、工具重试、节点数、深度和审批档继续累计；上下文窗口和单次 run 的迭代切片可以更新，但总任务成本不能重新计满。
9. 每次续跑前后都检查预算；恢复过程中若达到硬上限，按真实原因收口，不再安排下一次。

### 3A.10 重启恢复与副作用安全

B11-3 再开启跨进程自动恢复，且与进程内续跑分开验收：

- 启动扫描时发现可恢复检查点，先校验 `RunID／CheckpointID／HistoryVersion` 和当前 lease，旧进程仍活跃时不接管。
- 已完成持久结果但通知未发：只补通知，不重新执行。
- 已确认未开始任何有副作用工具、或所有副作用都有可核对幂等记录：可在开关允许时同 ID 自动恢复。
- 工具调用已发出但提交状态未知：保持 `interrupted`，先对账；不得盲目重放文件写入、网络写入、部署或支付等操作。
- 检查点损坏、History 缺失、required 上下文失效或恢复后状态矛盾：转人工并保留现场。
- `resume_interrupted` 默认 false；进程重启不借 B-11 把所有历史 Paused 会话批量唤醒。

### 3A.11 审计、指标与人工可见性

新增事件：

- `continuation_checkpointed`
- `continuation_scheduled`
- `continuation_resumed`
- `continuation_repaused`
- `continuation_blocked`
- `continuation_redispatch_rejected`

每条至少记录 `session_id／task_id／attempt_id／parent_agent_id／agent_id／node_id／run_id／checkpoint_id／cause／origin／continuation_seq／progress_digest／remaining_budget／remaining_deadline`。不记录完整敏感 History 正文。

至少提供以下指标：

- 自动续跑次数、成功率、平均新增墙钟与 token；
- 因无进展、总次数、Team 预算、用户暂停而阻断的次数；
- 防重派命中数、双重恢复拒绝数；
- 人工“继续”次数、重派实例数；
- 同任务从首次暂停到最终正确交付的总时长。

UI／最终结果可简洁显示“系统已自动续跑 1／2 次”；转人工时明确说明原因。审计事件不是用户消息，不进入用户决策账本或长期记忆。

### 3A.12 分批开发

#### PR B11-1：暂停原因、检查点和 shadow 对拍

改动：

- 增加中立暂停原因与检查点契约，拆解 Domain、叶子、父 Agent 和顶层的统一 `Limit Reached` 传播。
- 在现有 History／tree pause 路径补充检查点完整性与 `run_exited` 事件；尚未具备完整检查点的叶子不得开启自动恢复。
- 实现只记录、不实际唤醒的 ContinuationController shadow；记录“本会自动恢复／会因何阻断”。
- 对现有 `ResumePaused` 做适配层设计，保持人工恢复行为不变。
- 增加预算、remaining deadline 和 progress digest 采集，验证同任务累计口径。
- 同任务重派守卫先 shadow，记录会拒绝的新建实例，不改变 legacy 派发。

验收：能够从事故审计区分哪一种触限、哪个实例、是否可恢复；shadow 不发送系统唤醒、不新增用户消息、不改变完成状态。

#### PR B11-2：同 ID 自动续跑、父子等待与防重派 enforce

改动：

- 开启进程内 Domain／叶子／主 Agent 的同 ID 自动续跑；旧 run 退出后触发，带 cause／origin 的系统事件走内部入口。
- Domain 续跑期间父保持 `awaiting_child/continuation_pending`；删除默认向上扩大成 `paused_on_child` 的路径。
- 叶子触限先走检查点，不能将截断输出直接作为完成。
- 同任务防重派 enforce，旧实例可恢复时拒绝新建。
- 同 PR 必须交付累计次数、无进展、总墙钟和 Team token 熔断，不能先上无限自动循环再补限制。
- 统一 `maxPausedResumes` 与新任务预算；用户停止／取消清理所有待续跑事件。
- 增加 `off|shadow|enforce` 总开关和分原因灰度开关。B11-1 默认 shadow；B11-2 在回归矩阵通过后，对明确的系统触限默认 enforce，保留一键关闭自动行为的回滚能力。

验收：典型“子 Agent 一次上下文触限”和“主 Agent 一次迭代上限”都无需用户输入，同实例完成；父不暂停、不重派，最终完成通知只有一次。

#### PR B11-3：重启安全恢复与全链路故障注入

改动：

- 加入启动恢复扫描、lease／fencing、重复事件去重和检查点兼容性校验。
- 对状态确定且无副作用风险的任务开放受控恢复；`resume_interrupted` 默认 false。
- 故障注入覆盖保存检查点前后、旧 run 退出前后、恢复调用前后、完成通知前后崩溃。
- 用真实暂停案例做端到端回归，统计人工继续数、重派数、总 token、墙钟与最终正确性；未达到熔断和防双开要求不得扩大灰度。

### 3A.13 B-11 验收矩阵

| 场景 | 必须得到的结果 |
|---|---|
| Domain 子 Agent 上下文触限一次 | 无用户消息，同 Agent ID／attempt 自动恢复；父保持等待，不新建 Agent |
| 叶子 token 触限一次 | History 与工作检查点保留，同角色实例恢复；截断文本不标 done |
| 主 Agent 单次迭代上限 | 原主 session 自动续跑，不新增“继续”用户消息，不重新拆队 |
| 子 Agent 恢复后完成 | 父只被唤醒一次，pending 只减一次，Mailbox 只有一个逻辑完成结果 |
| 暂停期间模型或用户再次要求派同一任务 | 返回已有句柄或 `ErrResumableTaskExists`，节点数不增加 |
| 续跑后只重复文本、没有新证据 | 触发无进展熔断，明确转人工，不无限循环 |
| 达到任务累计续跑上限 | 所有嵌套节点共享计数，不再自动恢复；不能靠新 Agent 重置 |
| 原总 Deadline／Team token 耗尽 | 不自动续跑，状态与原因准确，无额外预算 |
| 用户显式停止、暂停或取消 | 待续跑定时器和系统唤醒全部失效，绝不自动复活 |
| 等待审批、澄清或权限处理 | 不自动续跑，不以系统指令冒充用户决定 |
| 定时器、恢复扫描和人工恢复同时到达 | single-flight／fencing 只允许一个 run，旧 run 迟到事件不能覆盖新状态 |
| 进程在不确定工具副作用后崩溃 | 标 interrupted 并等待对账，不自动重放 |
| 状态确定的安全检查点重启恢复 | 开关开启时同 ID 恢复；否则清楚标可人工恢复，不批量唤醒旧会话 |
| 普通小型耦合功能 | 仍走 single_writer；B-11 不引入额外拆分、讨论或组织学习开销 |

B-11 的业务验收不是“日志里出现了 resumed”，而是：**同一场可恢复触限演练中，人工继续次数为 0、同任务重派实例数为 0、最终交付证据完整，同时熔断测试证明不会无限自动执行。**

---

## 4. U-02：上下文冲突、优先级、撤回与关键约束保真

### 4.1 目标行为

对每一次新派发，生成一个不可变 `TaskContextManifest`。它既用于实际装配，也用于审计和恢复，不能出现“审计写了一套、模型实际读另一套”。

Manifest 解决四个不同问题：

- **来源**：这段内容来自哪个用户、Spec、项目文件、记忆条目、技能或工具结果。
- **版本**：冻结时是什么 revision／hash，恢复后是否仍有效。
- **裁决**：同主题冲突时谁生效，谁被替换，谁因不确定而停止注入。
- **保真**：required 是否完整进入实际模型输入，是否被预算、压缩、fork 或恢复丢掉。

### 4.2 目标契约

建议新增 `backend/internal/team/context.go`，只放中立数据结构，不 import `subagent`、`board` 或 `agent`，避免后续 Team 契约与执行器循环依赖。

```go
// 目标模型，字段名可在开发时按项目风格微调，语义不可删除。
type TaskContextManifest struct {
    Version           string         // "v2"
    ManifestID        string
    SessionID         string
    TaskID            string
    PlanVersion       int64
    SpecVersion       string
    RoleID            string
    Owner             string
    ProjectKey        string
    EffectiveRevision int64
    Required          []ContextEntry
    Optional          []ContextEntry
    Constraints       []ConstraintDecision
    ContentHash       string
    FrozenAt          time.Time
}

type ContextEntry struct {
    EntryID        string
    ConflictKey    string // 仅同 scope、同主题才允许冲突裁决
    Source         string // user_instruction/task_spec/agents_md/project_memory/user_memory/skill/block_recall/shared_fact
    SourceID       string // M-xxx、Spec key、skill name、fact id 等
    SourceRevision string
    Scope          string // task/project/user/session/role
    Trust          string // authoritative/verified/untrusted
    ContentHash    string
    Content        string // required 短文本冻结在 Manifest；大工件使用只读引用
    ArtifactRef    string
    RuneBudget     int
}

type ConstraintDecision struct {
    ConflictKey string
    WinnerID    string
    LoserIDs    []string
    Action      string // compatible/superseded/shadowed/unresolved/revoked/dropped_budget
    Reason      string
}
```

约束：

1. Manifest 中 required 的短文本直接冻结，避免 SharedMemory 同 key 覆盖后只有旧 revision 编号却找不到旧内容。
2. 大文件、diff 和参考图使用 `ArtifactRef + ContentHash`，Manifest 不复制全仓正文。
3. `EffectiveRevision` 只表示记忆视图水位，不能代替逐条目 revision 检查。
4. `ContentHash` 由规范化后的实际输入计算；渲染顺序固定，同一输入重复构建 hash 相同。
5. 工具结果、旧块记忆和自动经验保持 untrusted 围栏，不得因进入 Manifest 升格为指令。

### 4.3 来源优先级不是简单的“新的覆盖旧的”

安全、身份和工具权限由代码硬门控制，不参与 Prompt 优先级竞争。模型上下文内只对**同一 ConflictKey、同一作用域**的可比较事实做裁决：

| 优先级 | 来源 | 裁决规则 |
|---|---|---|
| 1 | 当前真实用户指令、明确纠正 | 可替换旧任务偏好和普通经验；不能借此扩大服务端工具权限 |
| 2 | 当前已批准任务 Spec 的更新版本 | 只对本任务生效；旧版本标 superseded，不覆盖用户最新纠正 |
| 3 | 人工项目约定、AGENTS.md／CLAUDE.md、显式用户长期指令 | 同主题人工约定优先于自动经验；跨作用域不直接删除另一方 |
| 4 | verified 项目记忆 | 项目事实优先于通用偏好；角色／任务过滤后才注入 |
| 5 | verified 用户画像 | 主要管沟通和通用偏好，不能捏造当前项目技术事实 |
| 6 | 已启用技能召回和角色工艺 | 是参考方法，不是任务目标；与 required 冲突时不注入并记录 |
| 7 | untrusted 旧块、工具输出、其他 Agent 文本 | 只能作为有出处的数据，永远不能覆盖上述来源 |

额外规则：

- 内容 hash 相同先做去重，不算冲突。
- 只有同一来源链的明确 successor、用户显式替换或已批准的新 Spec 可以 supersede；不能仅凭两个 Agent 的时间戳决定谁正确。
- 两个高优先级人工来源仍冲突时返回 `unresolved`。required 冲突阻止派发；optional 冲突整组不注入，保留审计，不让模型自行猜一个。
- 不做“全局高优先级文本删除所有低优先级文本”。不同 ConflictKey 的内容可以同时成立。

### 4.4 必传、选传与预算降级

首版 required：

- 当前任务文本与当前用户有效纠正；
- 已冻结 Spec 的 goal／acceptance／constraints／必要 Contract；
- 适用的项目硬约束和必要 AGENTS.md 条款；
- 本任务显式依赖的已完成 Result 引用及必要接口事实；
- 风险档要求的验证策略。

首版 optional：

- 与 role／task_type 匹配的 verified 项目经验；
- 用户通用偏好；
- 技能召回短提示；
- 相关 sibling fact、旧块召回和普通共享槽位。

预算处理顺序固定：去重 → 作用域过滤 → 冲突裁决 → optional 按相关性裁剪 → required 完整性检查。required 超预算时返回 `ErrRequiredContextOverflow`，由上级缩小任务或改用工件引用，**不能截断后半段后继续执行**。

### 4.5 全入口收敛

建议新增 `backend/internal/domain/subagent/context_manifest.go`，由 `ContextAssembler` 统一构建、校验、渲染和审计：

1. 把 `buildSharedPrefix` 改为返回“结构化条目＋渲染文本”，保留 legacy 包装以兼容现有测试。
2. 将项目有效记忆从纯 Markdown `EffectiveBlock` 扩展为结构化 `EffectiveItems` 提供器；渲染仍复用现有 active 过滤和 untrusted 围栏。
3. Manifest 模式下，项目记忆不再经 `WithDirectiveBlock` 隐式重复注入。原回调只保留失效通知或 legacy 路径。
4. 登记 `WithSkillBlock`、技能召回、`WithEvolutionBlock`、block recall、AGENTS.md、共享 Spec／槽位的版本与字符数。
5. 对 sibling uptake 和运行时事实采用独立游标记录；在增量事实协议尚未上线前，C-03 首批冷启动叶角色不得读取无关 sibling History。
6. `runSubAgentOnce` 只消费 Assembler 的输出。`emitContextAudit` 从同一 Manifest 产生，不再用另一个 map 事后拼一份近似清单。

`contextAuditEntry` v2 至少增加：

```text
entry_id / conflict_key / source_id / source_revision / trust
content_hash / required / decision / drop_reason / artifact_ref
```

审计顶层增加 `task_id/attempt_id/plan_version/spec_version/manifest_hash/actual_model_input_hash`。角色 prompt 和系统环境可只记版本／hash，不必把秘密或完整系统提示写日志。

### 4.6 撤回、热更新与恢复

在三个时点做不同强度的检查：

| 时点 | 必做行为 |
|---|---|
| 新派发前 | 重检所有 required 和已选 memory SourceID／revision；撤回条目物理缺席；Spec stale 或 required 变化则重建并重签 Manifest |
| 运行中 | 通过现有每轮 directive／安全检查点检测版本；optional 撤回只发一次失效注记，required 硬约束变化则停止后续写入并交上级重规划 |
| 重试／暂停恢复／进程重启 | 不直接回放旧渲染文本；按 SourceID 重检。旧 Manifest 可保留审计，但必须生成新恢复版本 |

运行中无法从模型 History 物理删除一段已经发送的文本，因此不得宣称“撤回后历史被擦除”。正确保证是：**后续新输入不再包含它、明确标记旧条目失效、关键变化阻止继续按旧约束执行**。

### 4.7 分批开发

#### PR U02-1：结构化来源与 v2 shadow

- 新增中立 Manifest 类型和 Assembler 接口。
- 为现有五路注入及项目 directive 增加 SourceID／revision／hash，先只 shadow 对拍，不改变模型输入。
- 记忆侧增加结构化读取适配，保留现有 RenderScope 给旧调用方。
- 建立冲突 fixtures：同 hash、同 key 新旧版本、跨项目同 key、人工与自动经验、untrusted 尝试覆盖。
- 回滚：`agent.context_manifest_mode=off`。

#### PR U02-2：裁决、保真与失效

- 实现 ConflictKey、确定性裁决、required 溢出拒绝和 optional 预算降级。
- enforce 路径禁用 fork “required 全丢仍执行”的旧降级。
- 接新派发、重试、暂停恢复和启动恢复的版本重检。
- 接运行中 required 失效停止与上级通知。
- 回滚：`agent.context_manifest_mode=shadow`，仅记录新旧差异。

#### PR U02-3：新路径收敛

- C-03 新任务只走 Manifest；legacy delegate 暂时保留。
- 对照确认没有重复项目记忆、没有未审计动态注入，再逐项迁移其他角色。
- 不为了“统一”一次性删除 legacy 代码；等真实回归覆盖后再退役。

### 4.8 验收矩阵

- 最新用户纠正与旧项目经验冲突，模型输入只保留有效裁决及必要说明。
- 项目 A 的记忆、角色和任务事实不会注入项目 B；空 projectKey 不退化为全项目查询。
- 两个高优先级来源 unresolved，required 拒派，optional 不注入且有原因。
- 已撤回 M-ID 在新派发、重试、压缩恢复、暂停恢复、进程重启后均不复活。
- required 超预算、fork 硬顶、Spec stale 均不会静默降级成“无约束执行”。
- optional 超预算可裁剪，但 required 的 acceptance／constraints 完整，实际输入 hash 与审计一致。
- 旧块、工具输出、Agent 邮件即使包含伪造指令，也不能提升优先级或覆盖 Spec。
- shadow 对拍期间 legacy 输入逐字节不变；off 可完整回退。

---

## 5. B-03：分风险 DoD 与机器可消费验收报告

### 5.1 不重做证据系统，先补策略和归因

现有证据采集继续负责“历史上是否发生过某类验证”。B-03 新增两层：

1. **策略层**：这个风险档需要哪些层级和条目，manual 是否允许，能否标 done／解锁依赖。
2. **报告层**：每条验收的状态、证据引用、缺失原因和最终裁决，直接进 AgentResult，而不是只渲染给人看。

### 5.2 风险档与默认 DoD

在 `team.Spec` 增加默认验证策略，在 `RoleSpec` 增加允许收窄的覆盖；保留现有 `DoD []string` 作为人读要求，不破坏已落地 YAML。建议新增 `team/verification.go`：

```go
type VerificationPolicy struct {
    Risk              string   // R0/R1/R2/R3
    RequiredLevels    []string // existence/static/integration/runtime/visual
    AllowManual       bool
    RequireHumanSignoff bool
    MaxRepairRounds   int
}
```

风险取任务、文件／工具副作用、角色和父团队中的最高档，子团队不能降级：

| 档 | 典型范围 | 默认完成要求 |
|---|---|---|
| R0 | 只读检索、解释、无修改 scout | 答案和来源引用；无文件写入时不要求构建；若声明了依赖自动解锁，仍需机器可读结果 |
| R1 | 文档、测试辅助、低风险单文件机械修改 | 有写入时 existence＋适用 static；声明的验收逐项有证据或允许的 manual 结论 |
| R2 | 默认代码行为修改、前后端功能、跨文件集成 | 适用 static＋相关 integration／runtime；至少一项与变更相关的有效命令证据；`file` 不能单独证明功能正确 |
| R3 | 数据迁移、权限、安全、生产配置、发布及外部副作用 | R2 全部要求＋精确关键路径证据＋必需人工审批；自动调度不得自动执行不可逆副作用 |

`approval=production` 最低 R3。工具或运行环境无法提供所需证据时落 `delivered-unverified/blocked`，不能因“任务很小”自动降档。

manual 规则：

- manual 不是“忽略”。v2 中每条 manual 都有 `pending/approved/rejected/not_applicable` 和 reviewer。
- R2／R3 的关键功能与 quality 条目不能由执行 Agent 自批 manual。
- C-03 默认只让机器通过的 done 解锁下游。纯人工任务可以完成 legacy 交付，但不自动驱动后继，除非有明确人工覆盖记录。

### 5.3 逐项 VerificationReport

建议新增 `backend/internal/domain/subagent/verification_report.go`：

```go
type VerificationReport struct {
    Version          string // "v1"，注意它与 AgentResult v2 是两个版本
    ReportID         string
    TaskID           string
    AttemptID        string
    SpecVersion      string
    Risk             string
    Status           string // passed/failed/unverified
    Items            []VerificationItem
    RequiredLevels   []LevelResult
    Unresolved       []string
    PolicyHash       string
}

type VerificationItem struct {
    AcceptanceID string
    TextHash     string
    EvidenceKind string
    Layer        string
    Status       string // passed/failed/missing/manual_pending/manual_approved
    EvidenceRefs []EvidenceRef
    Reason       string
}

type EvidenceRef struct {
    EvidenceID string
    Kind       string
    ToolCallID string
    Command    string
    ExitCode   *int
    ArtifactRef string
    ContentHash string
}
```

归因要求：

- command 条目需匹配声明的验证意图和真实工具调用，不得用一个无关 `echo` 或任意成功命令覆盖全部条目。
- screenshot／probe 继续复用现有去重、邻接和 console 检查，但报告保存具体事件／工件引用。
- file 只证明文件写入／存在，不自动证明行为通过。
- 现有 `scoreAcceptance` 保留给 legacy；新增 `buildVerificationReport` 复用其底层证据函数并输出结构化结果。
- 报告缺条目、验收文本 hash 与冻结 Spec 不一致、证据来自别的 attempt，均不能 passed。

### 5.4 AgentResult v2 与兼容

扩展现有 `backend/internal/domain/subagent/agent_result.go`，不要改 `backend/pkg/types.AgentResult` 来混入另一套旧模型。

v2 保留 v1 的 `Version/ResultID/AgentID/ParentID/Status/Summary/FilesModified/Verification` 字段，新增：

```text
task_id / attempt_id / plan_version / spec_version
context_manifest_id / context_manifest_hash
verification_report / unresolved / failure_kind / retryable
terminal_event_id / created_at
```

规则：

1. 新路径由显式 `TerminalOutcome` 构造结果，不再搜索“未验证”等摘要文字。
2. `Verification` 字符串保留给旧消费者；v2 report passed 才写 `passed`，否则保持 `unverified/unknown`。
3. v1 读取方忽略新字段仍可工作；v2 读取方收到 v1 时必须标 `legacy_unproven`，不能把 v1 的默认 passed 当成 B-03 已满足。
4. ResultID 不再只由 `subAgentID + summary hash` 生成。自动路径使用持久 attempt 的终态 ID；legacy 保留旧算法直到兼容迁移完成。
5. 同一个 result_id 重复通知返回已存结果，不再次推进 board 或解锁依赖。

### 5.5 终态收口改造

不要只在成功尾部分叉加一个 if。先在 `runSubAgent` 建立统一的内部终态对象，逐步覆盖：

- 正常完成；
- `errVerifyMissing/errUnverified`；
- 明确执行失败；
- token 部分返回；
- timeout／panic／loop guard；
- 手动暂停、软停止、硬取消；
- C-03 的启动失败和恢复中断。

每个终态只生成一次 Result，然后按统一顺序执行：验证裁决 → attempt／board 精确更新 → tree／history → Mailbox 短通知。Mailbox 失败不能反向把已完成结果改成失败。

### 5.6 分批开发

#### PR B03-1：VerificationReport 与 shadow 对拍

- 从现有 spec、history 和证据生成报告，不改变旧 board 状态。
- 对 legacy manual 保持旧计分；v2 shadow 额外显示“若 enforce 将缺哪些证据”。
- 为 command 误匹配、跨 attempt 证据、quality manual、judge 不可用建立 fixtures。

#### PR B03-2：AgentResult v2 与终态对象

- 新路径显式传状态，v1 序列化兼容测试覆盖 Mailbox JSONB。
- 将 ResultID、通知幂等键和终态 ID 对齐。
- 完成全终态映射，尤其 partial／pause／cancel 不得误标 completed。

#### PR B03-3：风险策略与 enforce

- Team Spec 增加策略校验和嵌套不可降档。
- `agent.verification_policy_mode=shadow` 默认只记录；`enforce` 用于 C-03 和显式开启的新路径。
- R2／R3 缺必需证据不能 board done；原 legacy delegate 是否逐步切 enforce 由对拍数据决定，不在本 PR 强切。

### 5.7 验收矩阵

- 无测试证据的代码任务不能因摘要写“全部通过”而 done。
- 任意成功命令不能覆盖不相关的验收条目。
- 缺 runtime／visual 证据继续落 delivered-unverified，报告能指出具体条目。
- manual pending 不解锁；人工批准后带 reviewer 和时间。
- R3 子角色不能通过 RoleSpec 降低风险或绕过审批。
- v1 结果可读取，v2 多余字段不破坏旧 Mailbox 消费方。
- 同一终态重复通知只产生一次有效状态迁移。
- 取消、暂停和部分返回不会因有文件修改而被判验证通过。

---

## 6. C-03：依赖就绪自动调度

### 6.1 默认保留通知，新增独立 opt-in

当前 `notifyDepWaiters` 只通知和唤醒，这是正确兼容基线。新增 `auto_dispatch` 后：

- `notify`：现有行为，父 Agent 决定何时、是否重派。
- `auto_dispatch`：只消费已批准且冻结的执行计划，由服务端在依赖 ready 时创建一次受控派发。

`auto_dispatch` 不是 C-06：没有 Worker 自由认领、没有 `claim_next` 模型工具、没有空转 LLM 轮询，也不要求移除 Meta／唯一集成责任。

### 6.2 先做精确 TaskBinding

建议新增中立 `team.TaskBinding`：

```text
session_id / plan_version / task_id / attempt_id / attempt_no
spec_version / context_manifest_hash / dispatch_key
```

改造顺序：

1. `call_sub_agent` 和 `ProgrammaticSpec` 增加可选 `task_id`；旧空调用保持 Domain 语义。
2. `dispatchOpts` 携带 Binding，并显式传到 `runSubAgent/runSubAgentWithAutoRetry/runSubAgentOnce` 或用兼容包装传递，不靠全局 map 猜当前任务。
3. 有 Binding 时，依赖检查、Assign、终态回写都只操作指定 task_id，并校验 plan_version／attempt。
4. 无 Binding 才走现有 `FindAllByDomain` 兼容路径。新自动路径禁止 fallback 到 Domain 整组。
5. `board.UpdateBoundTask` 使用条件状态与 attempt fencing；旧 attempt 的迟到结果不得覆盖新 attempt。

必须先通过“同 Domain 两条任务先后完成不串状态”的测试，才允许实现 Scheduler。

### 6.3 冻结可执行计划

现有 `PlanTask{id,title,domain,depends_on,acceptance}` 不足以自动执行。为 auto 计划扩展：

```text
role_id / task / responsibility / mode / verify_kind
tools_hint / skills_hint / required_tools
acceptance（带 evidence/layer 的结构化编码）
depends_on / risk / max_attempts
spec_key / spec_version / context_manifest
integration / failure_policy
```

安全边界：

- required_tools 只做资格检查，不是自我扩权清单；最终仍取父可分配、角色天花板、挂载可见性和显式授权的交集。
- 首批只允许冷启动固定叶角色，建议 `code_assistant/test_assistant/scout`；Domain、热驻、worktree、嵌套团队分别后续验收。
- 每个共享工件计划仍需唯一 integration 任务；没有集成责任的 auto 计划不予批准。
- R3 或不可逆外部副作用任务默认不能 auto_dispatch，只能等待人工。

重规划语义：

- 每次接受计划产生单调递增 `plan_version`；运行中版本不可原地改写。
- Done／running 任务及历史 Result 不随标题修改而改写。
- 新版本必须重新校验未知依赖、环、角色、工具、WriteSet、Manifest、风险和唯一 integration。
- 旧版本停止新派发；在途任务明确 drain、checkpoint 或 cancel，不能一半旧 Spec 一半新 Spec。
- 同时修正 `SetPlan` 对既有任务字段更新不完整的问题，但 legacy 重规划兼容测试必须先写清期望。

### 6.4 ReadyScheduler

建议新增 `backend/internal/team/ready_scheduler.go`，它只做确定性协调：

触发源：

- 已批准计划创建；
- 一个有效 attempt 进入终态；
- 依赖／Spec／审批状态变化；
- 启动恢复；
- 低频数据库兜底扫描，默认建议 5 秒，不产生 LLM 调用。

每个 session 串行 reconcile：

```text
读取当前已批准 plan_version
→ 找 pending 且所有依赖 done 的任务
→ 检查任务未 cancelled/blocked、attempt 未达上限
→ 重检 Spec/Manifest、权限、预算、审批、并发和写入冲突
→ 事务预留 dispatch_key + attempt
→ 调 Dispatcher 的受控入口
→ 记录 agent_id 和执行状态
→ 终态后再次 reconcile
```

事件只负责唤醒，数据库／持久快照才是真相。重复通知不能创建第二个有效 attempt。

### 6.5 最小持久防重，不借 Mailbox 假装 exactly-once

C-03 只需要一个最小 attempt 记录存储，例如 `team_dispatch_attempts`，不是 C-06 的完整三表队列。建议字段：

```text
session_id / plan_version / task_id / attempt_no / attempt_id
dispatch_key（唯一）/ status / parent_agent_id / agent_id
spec_version / context_manifest_hash
reserved_at / started_at / finished_at / result_id / last_error
```

约束：

- 唯一键 `(session_id, plan_version, task_id, attempt_no)`；`dispatch_key` 也唯一。
- 预留、状态条件更新和结果关联使用真实数据库事务。
- Board 仍作展示投影，不作为跨进程认领锁。
- 首版若只支持单 Scheduler 实例，要在启动时显式保证单 owner；多实例支持前必须有数据库 owner／lease，不允许两个进程各自扫板重派。
- 默认 `max_attempts=1`。自动重试只允许幂等、明确未产生外部副作用的任务；默认不沿用叶子的盲重试。

崩溃窗口必须诚实处理：

- 已预留但无法证明 Agent 从未启动时，恢复标 interrupted，不盲目重放写入任务。
- 已有 agent_id 和持久终态 Result 时按 result_id 幂等收口。
- 旧 attempt 恢复后迟到完成返回 fence 错误，结果可留审计但不改当前任务。
- Mailbox 未发不影响已完成业务状态，通知可补发。

### 6.6 Dispatcher 受控入口

新增 `DispatchApprovedTask`，不要直接裸调当前 `DispatchProgrammatic`：

1. 从工具 `Execute` 与 `dispatchOne` 中抽出共用预检，覆盖参数、Spec、CanCall、角色任务匹配、有效工具交集、预算、并发、取消、树和台账。
2. 入参只接受冻结 TaskContract＋Binding＋Manifest，不接受调用方临时提高工具和风险等级。
3. 继续复用现有 ReAct、工具权限、验收证据和并发池，不创建第二个 Agent 执行引擎。
4. auto 首批不走热驻、worktree、Domain 递归和旧 `dispatchRetryCount` 盲重试。
5. 完成统一进入 B-03 的 TerminalOutcome；只有 report passed 且审批满足才 `MarkDone(boundTask)`。

### 6.7 失败、取消和集成

- `failure_policy=block_dependents` 为默认：前置 failed／unverified，后继 blocked，独立分支继续。
- `wait_human`：计划进入等待人工，不自动选择替代方案。
- `skip` 只能在计划批准时显式声明，并列出被跳过任务对集成的影响；不能运行时由 Scheduler 私自跳过。
- 用户取消会话时停止新 reconcile，取消当前执行与租约，保留 attempt 和结果，不自动重排。
- integration 也是一个受控任务，读取上游 artifact hash、AgentResult 和 VerificationReport，不读长摘要猜状态。
- 根会话只消费最终集成结果与异常，普通 delegate 通知路径保持不变。

### 6.8 分批开发

#### PR C03-1：精确绑定，无自动行为

- 加 TaskBinding、精确 gate／assign／update 和兼容旧 Domain 路径。
- 修重规划字段与版本契约。
- 同 Domain 多任务、旧 attempt 迟到、重复完成测试必须通过。
- 配置无新执行行为，可独立合并和回滚。

#### PR C03-2：冻结计划与 attempt Store

- 扩展 auto 计划契约、真实 PG 表／迁移、内存测试实现和唯一键。
- 写事务预留、条件完成、恢复查询和会话硬删。
- 不接 Scheduler，不影响旧路径。

#### PR C03-3：ReadyScheduler 与 Dispatcher 接线

- 默认 `agent.auto_dispatch_enabled=false`。
- 实现单 session reconcile、事件唤醒、兜底扫描和受控 DispatchApprovedTask。
- 首批仅允许一个固定叶角色、冷启动、非 worktree，再逐步扩角色。

#### PR C03-4：恢复、取消、唯一集成和灰度

- 启动时先恢复 interrupted／未决通知，再开放新调度。
- 补计划变更、用户取消、集成任务、预算结算和 UI 下钻。
- 先只读／测试任务，再独立代码模块；小型耦合前后端功能仍走 single_writer。

### 6.9 验收矩阵

- 两个依赖同时完成只产生一个有效 attempt；重复事件、重启恢复和手动重试不重复派发。
- 同 Domain 的 A／B 两条任务不会互相标 done；旧 attempt 不覆盖新 attempt。
- delivered-unverified、failed、manual pending 均不解锁后继。
- 父 Agent 暂停、用户取消、计划版本变化时不再创建新执行。
- 权限、预算、Spec／Manifest、审批和冲突门均在启动前执行，不能先跑再补检查。
- 无 ready 任务时 Scheduler 零 LLM 调用。
- 进程在预留后、启动中、结果提交前后、Mailbox 前后崩溃，恢复行为符合 6.5，不出现静默双写。
- 关闭开关后无新 auto 派发；legacy notify 和 delegate 行为不变。

---

## 7. C-05：有反馈的组织学习闭环

### 7.1 复用现有生命周期，不新增“团队总记忆”

第一版只把**可验证的项目经验与角色／任务类型经验**纳入闭环。用户画像仍只接受真实用户明确输入；团队 Result、其他 Agent 文本和工具输出不能自动写用户画像。

现有 `SessionEvolver`、shadow candidate、长期记忆 items／events、用户否决、防复活和管理 API 继续作为底座。新增的是 TeamCandidate 适配和 PromotionService，而不是另写一套 MemoryStore。

### 7.2 候选数据与作用域

现有 `MemoryItem` 建议做加法扩展：

```text
origin 增加 team_evolver
role_id / task_type
source_result_id / source_verification_report_id
```

PG 列与索引、内存实现、查询过滤同步增加；旧行允许空值。Scope 仍使用现有 user／project，首批自动候选只允许 project，避免为了角色经验发明跨项目全局新作用域。

候选最小输入：

```text
content / conflict_key / owner / project_key
role_id / task_type
source_session / source_task / source_attempt / source_result
spec_version / context_manifest_hash
verification_report / artifact_refs
```

准入硬条件：

- source Result 是 B-03 done，报告可追溯且与冻结 Spec hash 一致；
- 没有用户明确否决，candidate 内容不含密钥、个人信息、整段代码或任务专属临时选择；
- 内容与项目／角色／任务类型匹配，untrusted 输入仍保持围栏；
- 严格防复活检查通过；仓储不可用时候选可留 shadow，但不得 active；
- 候选默认 `status=candidate + verification=unverified`，不进入正常注入。

### 7.3 状态机复用现有字段

不必为了名字增加一套平行状态：

```text
candidate + unverified
  → candidate + verified       （证据与离线 eval 通过，即“已验证待晋升”）
  → active + verified          （人工或有策略的晋升，开始可召回）
  → quarantined                （即时停止注入，等待复核）
  → superseded                 （被新经验替代）
  → retracted                  （明确撤回／否决）
```

新增 `MemoryPromotionService`，建议位于 `backend/internal/agent/memory_promotion.go`：

- `SubmitTeamCandidate`：只创建 candidate 和证据事件。
- `EvaluateCandidate`：生成同预算 baseline／treatment 报告，不写 active。
- `PromoteCandidate`：校验 expectedRevision、证据、eval、严格防复活和作用域后激活。
- `RecordExposureOutcome`：把某次实际注入与任务结果关联，只记反馈，不凭单样本自动定论。
- `Quarantine/Retract/Rollback`：复用现有生命周期，保留完整事件。

管理员的通用 Restore 仍可存在，但自动晋升不得调用它来绕过准入门。管理员强制恢复必须显式记录 override 原因。

### 7.4 同预算 shadow eval

候选不得直接污染真实用户任务。评估时构造两个上下文：

- baseline：当前有效记忆，不含候选；
- treatment：其他输入完全相同，仅增加该候选。

固定模型、工具、仓库版本、任务、验收、预算和运行次数。初始建议门槛（配置可改，实验前冻结）：

```text
min_eval_tasks=10
min_eval_repeats=2
auto_activate=false
required_machine_verification=true
```

晋升判断：

1. 所有必需安全、权限和 B-03 机器验收没有退化；
2. 完成率、首次正确交付、返工率、墙钟或成本至少一项有稳定改善，其他关键指标无不合理退化；
3. 候选不是只记住某个任务答案，而是在声明的 task_type 中有可复用收益；
4. 样本不足时保留 candidate，允许人工审核，不能自动晋升；
5. 第一版只允许项目作用域人工晋升。自动晋升即使未来开启，也先限项目＋明确 role／task_type，不自动写全局用户经验。

评估报告可存现有 evolution_log／MemoryEvent Payload 与工件引用；只有出现独立查询需求才新增 evaluation Store，不预先再造记忆表。

### 7.5 原子晋升与回滚

当前单条目 TransitionStatus 不足以独立保证“旧经验 superseded、新经验 active”跨两条目的原子性。需要在 `MemoryRepo` 增加专用事务方法：

- `PromoteCandidate`：锁候选和同 ConflictKey 当前 active，校验所有 expectedRevision，同事务迁移状态并追加事件。
- `RollbackActivation`：同事务撤回当前问题版本、恢复指定历史版本，记录 rollback_of 和操作者。
- 内存与 PG 实现语义一致；任一失败不留“两个 active”或“全部不可用”的中间状态。

撤回优先于学习：`write_mode=off/shadow` 或学习开关关闭都不能关闭 retraction。新派发和恢复由 U-02 重检，撤回后不再召回。

### 7.6 反馈指标与归因纪律

利用 U-02 Manifest 中的 memory SourceID，把曝光和结果关联：

```text
memory_id / role_id / task_type / exposure_count
verified_done / unverified / failed / rework
user_rejection / manual_override
wall_clock / token / cost
last_used_at / last_verified_at
```

规则：

- 单个成功或失败只是一条反馈，不直接证明经验普遍有效或无效。
- 真实用户明确否决立即走现有撤回／隔离，不等聚合指标。
- 普通指标退化先降候选优先级或 quarantine，复核后决定 retract／rollback。
- 不能把“注入了某经验”直接写成因果；对照报告和聚合反馈分开呈现。
- 技能包和 RoleSpec 的工艺更新后续复用同一证据与回滚门，第一版不随项目经验一起自动启用。

### 7.7 分批开发

#### PR C05-1：团队候选与证据，保持 shadow

- 扩展 MemoryItem 字段、Origin、查询和迁移。
- 接 B-03 Result／Report，新增 SubmitTeamCandidate 和严格防复活。
- 真实默认仍是 `memory.write_mode=shadow`，没有正常召回变化。

#### PR C05-2：eval 与受控晋升

- 实现 baseline／treatment runner、报告和 PromotionService。
- 加原子 PromoteCandidate、人工审核 API／UI、expectedRevision 和 override 审计。
- `auto_activate=false`，只允许项目作用域人工晋升。

#### PR C05-3：曝光反馈与回滚

- 接 U-02 SourceID、任务终态和用户反馈。
- 加聚合指标、quarantine、RollbackActivation 和恢复回归。
- 完成足够真实对照后，再讨论是否开启有限自动晋升；默认不开启。

### 7.8 验收矩阵

- 未验证、用户否决、来源跨项目、证据与 Spec 不匹配的候选均不能 active。
- 已被撤回的经验换措辞后经另一 Agent 也不能复活；严格模式仓储故障不能自动放行。
- shadow eval 除候选外输入和预算一致，报告可重放、可审计。
- 晋升和回滚在并发下不产生双 active；旧 revision 提交会失败。
- 撤回后新团队、重试和恢复均不召回；历史 events 仍可追溯。
- 候选不会改变用户画像；全局与项目作用域互不串写。
- 关闭学习开关只停止新候选／晋升，不停止用户撤回。

---

## 8. C-06：看板自认领的实验决策包

### 8.1 当前结论不变

C-06 状态保持 **未立项、默认不建设**。22 号文档第 12 节已经是立项后的详细技术预案，本节只规定如何取得“做／不做”的证据，避免把候选架构偷渡进 C-03。

### 8.2 先交付数据，不先交付队列

实验负责人需要提交一份决策记录，至少包含：

1. 任务语料：长时、独立、可验证、能提前复用中间成果的真实任务；排除小型耦合前后端功能。
2. 四路线基线：`single_writer`、`single_writer_experts`、`controlled_parallel`、轻量 self-claim 原型／trace replay。
3. 同模型、同工具、同仓库、同验收、同预算的重复运行原始数据。
4. 时间分解：planning、relay、dependency wait、capacity wait、execution、verification、integration、rework。
5. 质量与成本：首次正确交付、最终机器验收、token、LLM 轮次、人工接管和集成返工。
6. 权限、唯一集成、上下文撤回、恢复与预算的明确方案。
7. 结论：go、stop 或补充证据；未达门槛不得写“后续直接按第 12 节开发”。

### 8.3 建议的量化启动线

以下是首轮实验的默认建议，不是业界常数；开始跑数据前冻结，不能看完后调阈值：

- 至少 10 条符合条件的真实长任务 trace；
- 中继等待、重复探索或屏障等待的中位占比达到 25%，且更简单的工件引用、异步通知或 controlled_parallel 无法消除主要部分；
- 轻量原型在重复对照中，首次正确交付、端到端墙钟、总成本或完成率至少一项有稳定净收益，其他关键指标不退化；
- 所有必需机器验收和权限测试通过，集成责任与故障恢复方案完整；
- 与前三条路线比较，而不是只击败已知低效的 legacy delegate。

任一硬条件不满足：结论为 stop，保留数据，三个月内有新任务分布或新证据再重评。不得先建完整队列再补立项理由。

### 8.4 只有 go 后才执行的动作

1. 在实验分支启用 `team_execution.self_claim_enabled=false` 的代码骨架，默认仍关闭。
2. 按 22 号文档第 12 节 PR 1～8 实施事务 Claim／lease／fencing、WorkerSupervisor、DispatchClaimed、唯一 integration 和增量事实。
3. C-03 的 attempt Store 可评估复用字段或迁移，但不能为了复用把 push 调度与 pull 认领混成一个无边界服务。
4. 首批仍限固定叶角色、冷启动和受控任务，不让小功能进入 self-claim。
5. 灰度中若质量下降、上下文撤回失效、重复认领或成本明显反超，立即停止新 Claim 并回退，不用更多 Agent 弥补。

---

## 9. 总 PR 路线与人员拆分

### 9.1 推荐顺序

| 顺序 | PR | 可独立验收的产物 | 解锁条件 |
|---|---|---|---|
| 1 | B11-1 | 暂停原因、检查点、预算／进展审计、续跑与防重派 shadow | 无；最高优先级直接开始 |
| 2 | B11-2 | 同 ID 自动续跑、父子等待、叶子检查点、防重派和熔断 enforce | B11-1 对拍可信，检查点完整性通过 |
| 3 | B11-3 | 重启安全恢复、lease／fencing 和崩溃故障注入 | B11-2 进程内矩阵全通过 |
| 4 | U02-1 | Manifest v2 shadow、全来源清单、冲突 fixtures | B-11 主链路不再依赖人工反复继续 |
| 5 | U02-2 | 冲突裁决、required 保真、撤回与恢复重检 | U02-1 对拍可信 |
| 6 | B03-1／B03-2 | VerificationReport、AgentResult v2、显式终态 | 可与 U02-2 部分并行，接口先冻结 |
| 7 | B03-3 | 风险 DoD enforce，新路径状态可信 | B03-1／2 全终态回归通过 |
| 8 | C03-1 | 精确 task／attempt 绑定，无自动行为 | B03 状态语义可用 |
| 9 | C03-2／3／4 | 持久防重、ReadyScheduler、恢复和集成 | U02 enforce＋B03 enforce 均可用；复用 B-11 控制面 |
| 10 | C05-1 | 团队经验候选，保持 shadow | U02 SourceID＋B03 Report 可用 |
| 11 | C05-2／3 | eval、受控晋升、反馈和回滚 | C05-1 候选质量可审计 |
| 12 | C-06 决策包 | go／stop 结论 | C-03 与 controlled_parallel 有同条件基线 |

B11-3 与 U02-1 可由不同开发者并行，但 B11-2 未通过前不要扩大其他自动执行能力。C-05 与 C-03 可以在 U-02＋B-03 后由不同开发者并行；C-06 不应抢占 B-11、U-02、B-03 的开发资源。

### 9.2 建议默认配置

字段最终名称以 `AgentConfig/MemoryConfig` 接入为准，默认值先固定：

```yaml
agent:
  continuation_mode: enforce           # off|shadow|enforce；B11-1 先 shadow，B11-2 验收后对系统触限 enforce
  auto_resume_max_per_task: 2          # root/domain/leaf 共享的任务累计次数
  auto_resume_no_progress_limit: 1     # 容忍一轮零可验证进展，再次触限转人工
  resume_interrupted: false            # 进程重启后默认不自动恢复不确定现场

  context_manifest_mode: shadow        # off|shadow|enforce
  verification_policy_mode: shadow    # off|shadow|enforce
  auto_dispatch_enabled: false
  auto_dispatch_rescan_sec: 5
  auto_dispatch_max_attempts: 1
  auto_dispatch_failure_policy: block_dependents

memory:
  write_mode: shadow                  # 保持现有默认
  retraction_mode: enforce            # 保持现有默认
  promotion_mode: shadow              # off|shadow|enforce
  auto_activate: false
  min_eval_tasks: 10
  min_eval_repeats: 2
```

启动校验：

- continuation enforce 只对明确可恢复的系统原因生效；缺检查点、预算字段非法或旧 run 未退出时不得自动启动，转人工并报警。
- `auto_resume_max_per_task` 必须复用同一任务累计计数，不能每个 Agent 各自计数；旧 `maxPausedResumes` 只能经兼容映射保留，不能形成第二套独立预算。
- continuation shadow／off 不得伪造用户输入；用户显式暂停、取消、审批和硬预算在任何模式下均不自动恢复。
- auto_dispatch 开启时，Manifest 和 verification policy 必须 enforce，否则 fail-fast，不静默降级。
- auto_dispatch 允许角色、重试、集成责任和审批档不合法时拒绝启动新计划。
- self-claim 不属于上述默认配置，只有 C-06 go 后才加入且默认 false。
- 除 B-11 已明确验收的系统续跑外，其余新开关关闭时，legacy delegate、通知式依赖门、长期记忆撤回行为保持不变。

## 10. 全局测试与故障注入

### 10.1 测试层次

1. **契约单测**：暂停原因、续跑预算与进展判定、Manifest、冲突裁决、风险档、DoD、结果版本、计划 DAG、嵌套策略。
2. **存储集成**：真实 PostgreSQL 事务、唯一键、revision、fencing、晋升／回滚原子性、会话删除。
3. **执行集成**：Dispatcher 权限与工具交集、精确 task 回写、取消、panic、timeout、partial、pause、同 ID 恢复、防重派和完成通知去重。
4. **上下文矩阵**：跨项目、角色／任务隔离、untrusted 围栏、required 保真、撤回、压缩与恢复。
5. **端到端**：主／子／叶子触限无需人工继续、无进展熔断、依赖乱序、多依赖同时完成、独立分支、唯一集成、用户取消、缺证据阻断。
6. **性能对照**：首次正确交付、端到端墙钟、等待／执行／验证／集成分项、token、返工、人工继续次数和同任务重派实例数。

建议在 `backend` 目录按改动范围执行：

```bash
go test ./internal/team ./internal/domain/subagent ./internal/board ./internal/agent ./internal/store
go test -race ./internal/team ./internal/domain/subagent ./internal/board ./internal/agent
go test ./...
go build ./...
```

这些是必须执行的验证建议，**本文并未执行它们**。PG 集成测试使用项目现有真实测试入口，记录实际环境；不得用单连接 fake 代替并发原子性证明，也不得把用户此前回报的测试结论当成本轮复跑结果。

### 10.2 必做故障注入

| 故障点 | 期望 |
|---|---|
| 子 Domain 系统触限、旧 run 尚未退出 | 只登记 continuation_pending，不双开；退出后才同 ID 恢复，父不进入人工暂停 |
| 主 Agent／叶子系统触限 | 主 session 无伪造用户消息；叶子无部分假完成，均有累计预算与无进展熔断 |
| 可恢复暂停期间再次派发同任务 | 防重派拒绝创建新 Agent，返回已有句柄或明确错误 |
| 恢复后没有新工具结果、diff、测试或子任务进展 | 不更新 progress digest，达到阈值后转人工 |
| 用户取消与待续跑事件并发 | cancel 获胜，旧事件因 fence 失效，不自动复活 |
| Manifest 构建后、派发前撤回记忆 | 新输入不含旧条目；required 变化则重建／拒派 |
| 运行中修改关键 Spec | 停止旧约束后续写入，生成明确失效事件，交上级处理 |
| required 超预算／项目记忆服务故障 | required 不静默丢；optional 可按策略降级并留审计 |
| 验证工具不可用／证据来自旧 attempt | delivered-unverified，不解锁依赖 |
| 两个 Ready 事件同时到达 | 只有一个有效 attempt |
| 预留后、启动前进程崩溃 | 能恢复并区分未启动与状态不确定，不盲目重放写入 |
| 结果已提交、Mailbox 未发 | 业务结果不重复推进，通知可补发 |
| 旧 Worker／旧 Agent 迟到完成 | fence 拒绝覆盖当前状态 |
| 晋升与撤回并发 | revision／事务保证不产生双 active 或旧经验复活 |
| 学习开关关闭 | 停止新候选／晋升，但撤回仍生效 |

## 11. 完成定义与禁止提前事项

### 11.1 全部能力的完成定义

- [ ] 可恢复系统触限不需要人工输入“继续”，主／子／叶子均按同 session／Agent ID／attempt 恢复，且不伪造用户消息。
- [ ] Domain 自动续跑期间父不进入人工暂停；同任务重派实例数为 0，最终完成只唤醒父一次。
- [ ] 累计续跑次数、无进展、Team token 和原总 Deadline 均会熔断；用户暂停／取消、审批、澄清和硬预算绝不自动恢复。
- [ ] 叶子截断结果不冒充 done；重启后不确定副作用不自动重放。
- [ ] 实际模型输入可由 Manifest 重放，required 保真，所有动态注入有来源、版本和裁决审计。
- [ ] 撤回、冲突、跨项目、压缩恢复、暂停恢复和进程重启测试通过。
- [ ] 新路径完成状态来自显式结果与逐项机器报告，不再从摘要猜测。
- [ ] 风险档不可被子团队降低，manual 和人工覆盖有独立审计。
- [ ] auto_dispatch 只在已批准计划上运行，精确 task／attempt 防重，默认关闭，可完整回退。
- [ ] 只有机器通过的 done 解锁下游，唯一 integration 对最终交付负责。
- [ ] 团队经验经证据和 eval 后才可晋升，能即时撤回、原子回滚且不会换措辞复活。
- [ ] C-06 有明确 go／stop 决策；未 go 时没有队列和 Worker 的既成事实。
- [ ] 真实对照证明小型耦合功能仍走 single_writer，没有因新能力上线退回前后端双 Agent 默认拆分。
- [ ] 文档只记录实际落地与实际测试，不存在“目标结构体已写＝能力已完成”的误报。

### 11.2 明确禁止提前做

1. 未完成检查点、旧 run 退出确认、累计预算和无进展熔断，不开启自动续跑 enforce。
2. 不用 `sendMessage("继续")` 伪造用户恢复，不在旧实例可恢复时新建同任务 Agent，不通过续跑重置总墙钟或 Team 预算。
3. 未修精确 TaskBinding，不启动自动调度。
4. 未接 B-03，不以 Agent 文本完成状态自动解锁依赖。
5. 未接 U-02，不把冻结 Manifest 只当审计字段而继续走隐式注入。
6. 未证明命令／截图／探针与具体验收关联，不标逐项 passed。
7. 不把 Mailbox 幂等键当成执行 exactly-once；不对状态不确定的有副作用任务自动重放。
8. 不通过 C-03 偷渡 C-06 的自由认领、空转 Worker 和无集成者拓扑。
9. 不打开旧块记忆自动写入，不让团队结果直接写用户画像或全局长期经验。
10. 不为了“统一架构”一次性删除 legacy delegate；先 shadow、对照、灰度，再退役。
