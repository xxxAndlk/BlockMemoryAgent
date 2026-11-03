# 22 AgentTeam 问题与修复清单

> 本文回答三个问题：当前多 Agent 主链有哪些**已能从代码确认的问题**，哪些是有意取舍但必须显式管理的**可靠性与安全边界**，以及面向真实使用时还缺哪些 **Team 级能力**。
>
> 阅读顺序：先看 [21 多 Agent 设计详解](21-多Agent设计详解.md) 了解现行为，再用本文确定改造顺序。本文是评审与修复清单，不代表其中任何修复已经实施。
>
> **修复状态（2026-11-03）**：U-01 的拆分提示冲突、A-01（权威 Agent 身份）、A-02（Parents 目标白名单语义）、A-03（工具授权取父级有效工具面）已落地。P1 批次另落地 B-01（安全 profile）、B-02（审批 fail-closed）、B-04（会话 token 预算）、B-05（共享写路径强制乐观锁）、B-06（Mailbox 幂等键）、B-09（AgentResult 信封）、B-10（token 用量指标）。详见 `doc/变更.md` 同日两条。其余：U-02 注入审计、B-03 分风险 DoD、B-07 恢复矩阵代码化、B-08 TaskContextManifest、C-01～C-06 Team 能力尚未实施。

## 1. 评估口径

### 1.1 本文的 AgentTeam 指什么

当前系统的“团队”由一次会话、临时 Agent 树、角色注册表、Dispatcher、Mailbox、看板与任务台账共同形成。它能完成多角色协作，但代码中未检索到一等、持久化的 `AgentTeam`／`TeamSpec` 聚合对象。

因此本文区分：

- **Agent 实例**：一次具体执行，有自己的角色、上下文、工具面和生命周期；
- **会话内团队**：以 Agent 树组织的当次协作；
- **Team 级能力**：跨任务管理角色、权限、预算、交付、记忆和恢复的治理层。

### 1.2 四类结论不能混写

| 类别 | 含义 | 本文件标记 |
|---|---|---|
| 真实使用问题 | 真实任务中已观察到的现象；静态代码只用于寻找候选原因，未完成链路记录前不写成单点根因 | U |
| 已确认问题 | 代码路径、配置语义或身份来源存在可指明的不一致；仍需回归测试确定实际影响面 | A |
| 可靠性／安全边界 | 当前行为可能是有意设计，但不能向用户承诺更强的保证 | B |
| 能力缺口 | 新能力建设，不冒充已发生故障，也不冒充当前已有功能 | C |

优先级同时考虑真实使用影响与正确性风险：

- **真实使用 P0**：先处理 U-01 的严重低效与 U-02 的上下文污染，恢复日常研发任务的可用性；不能用宏大的 Team 重构替代这两个问题。
- **正确性 P0**：A-01～A-03 仍需统一身份与权限。保留其 P0 等级，共享环境或高风险上线时不能被体验优化挤占。
- **P1**：让真实任务可控、可验收、可恢复，避免并行覆盖、预算失控和错误交付。
- **P2**：在稳定执行内核之上，再建设持久 Team、嵌套子团队和组织学习。

### 1.3 证据边界与已有决策

- 结论来自当前代码、配置和 `doc/TODO.md` 的静态复核；证据统一写“仓库相对路径 + 函数／符号名”，不使用易漂移行号。
- “同类前后端联查与功能页面修改，Claude Code 约 5 分钟、BMA 前后端两个子 Agent 约 30 分钟且不一定正确”是**用户真实使用反馈**，不是本轮复测数据；本轮尚未取得该任务的分段 trace，不能把某个等待点写成已证实根因。
- “关闭块记忆、更新项目经验与用户画像写入后污染减轻”同样是用户观察，说明方向有效，不等于已验证解决。
- 本轮没有 Go 工具链，未运行 Go 构建或测试。文中的“验收”是**修复后必须执行的测试要求**，不是本轮测试结果。
- `doc/TODO.md` 第 13 项明确保留 hub-and-spoke、禁止 Domain→Domain 平级派发；第 14 项明确“依赖就绪只唤醒通知、不自动派发”；第 16 项记录“成本熔断／预算墙暂不纳入”。这些属于已有决策，本文只补充真实场景的启用条件与最小增量，不把旧方案重新包装成全新发现。
- 文件写已有 `expected_mtime` 乐观锁、同轮写路径互斥与可选 worktree，不是“毫无并发保护”；旧块记忆写入关闭是防污染的主动决策，不应简单重新打开。

## 2. 结论与修复总表

当前最需要的不是增加更多 Agent 角色，也不是再叠一层总结，而是先让小型研发任务**少拆分、少等待、上下文可解释且交付正确**；同时把身份、权限、验收和恢复做成一致的机器语义。

| 编号 | 类别／优先级 | 问题或缺口 | 最小修复／建设方向 | 主要验收 |
|---|---|---|---|---|
| U-01 | 真实使用问题 · P0 | 小型前后端垂直功能被过度拆成两个子 Agent，多层协作与等待使约 5 分钟级任务拖到约 30 分钟，且质量不稳定 | 分段计时；小型耦合功能默认单 Agent 垂直负责；消除拆分提示冲突；收敛非必要审批与摘要 | 同任务同模型同验收下，墙钟、LLM 轮次、token、测试与返工均有对照；提速不能牺牲正确性 |
| U-02 | 真实使用问题 · P0 | 项目经验、用户画像、旧块记忆、领域档案与兄弟产出多源注入，仍可能污染或丢失关键上下文 | 注入审计 + 任务作用域上下文清单 + 来源优先级、失效与撤回 | 冲突、跨项目、子 Agent 交接、压缩恢复时不串用、不复活旧约束、不丢关键约束 |
| A-01 | 已确认问题 · P0 | 顶层角色靠 ID 字符串推断，daily 顶层会被当成 meta | 建立权威 `AgentIdentity`，权限与调度不再解析 ID 猜角色 | fast／daily／cluster 三档的允许与拒绝矩阵一致 |
| A-02 | 已确认问题 · P0 | `Parents` 的检查对象与“允许哪些父级”配置直觉相反 | 明确父子白名单语义，按目标角色声明校验 | 未声明父级被拒，合法父级放行，禁止循环派发 |
| A-03 | 已确认问题 · P0 | Meta 可授工具按全注册表计算，可能超出自身 Schema 工具面 | 授权上限取父级当前有效工具面，插件另算可见性 | 父不能授出自己没有权限的工具 |
| B-01 | 边界 · P1 | full-auto 与审批禁用适合本机开发，不是生产安全 profile | 预检、显式启用、按风险审批，保留硬门 | 共享环境无法静默以全自主配置上线 |
| B-02 | 边界 · P1 | 计划确认超时／不可达 fail-open，且依赖模型主动调用 | 风险分级审批；高风险暂停而非自动放行 | 超时、父失联、反复驳回均按策略收口 |
| B-03 | 边界 · P1 | 默认独立验收关闭，不同角色的默认校验强度不同 | 按任务风险定义 DoD 与证据等级 | 缺必需证据不能显示为已验证完成 |
| B-04 | 边界 · P1 | 上下文阈值不是 Team 累计预算，缺少成本硬止损选项 | 团队预算账本、预留结算、降级与停止策略 | 达预算后不继续扩队，保留部分产出 |
| B-05 | 边界 · P1 | 跨 Agent／跨会话写同一文件没有强制所有权 | 强制版本校验、路径租约、冲突队列 | 两 Agent 写同文件不会静默覆盖 |
| B-06 | 边界 · P1 | Mailbox 崩溃重投是重复风险，不是 exactly-once | 至少一次投递 + 消费幂等 + 死信重放 | 注入前／后崩溃不产生重复副作用 |
| B-07 | 边界 · P1 | 会话预热不等于整个团队原样恢复 | 持久化最小派发规格与检查点，定义恢复矩阵 | 重启后能续跑、重派或明确转人工 |
| B-08 | 边界 · P1 | 子 Agent 不自动继承顶层完整记忆与决策组合 | 按作用域裁剪的任务上下文包，承接 U-02 | 约束可追踪，私密记忆不越权传播 |
| B-09 | 边界 · P1 | 结果回传仍依赖摘要、状态与多个旁路字段拼读 | 持久、版本化 `AgentResult` 信封 | 结果、工件、证据、未决项可机器消费 |
| B-10 | 边界 · P1 | 有观测数据，但缺 Team 级 SLO 与失败归因聚合 | 先支撑 U-01 分段计时，再聚合排队、返工、成本、未验证率 | 能回答慢在哪、贵在哪、为何返工 |
| C-01 | 能力缺口 · P2 | 没有一等、版本化、可复用的 TeamSpec | 团队配置与运行实例分离，Dispatcher 做适配 | 同一团队定义可重建并保留策略快照 |
| C-02 | 能力缺口 · P2 | 固定三层难表达大型项目中的有界子团队 | 有界嵌套 Team，继承预算与权限上限 | 不形成循环，不突破总预算与工具边界 |
| C-03 | 能力缺口 · P2 | 计划依赖完成只通知，不会自动执行就绪任务 | 可选、确定性的就绪任务调度器 | 依赖完成仅触发一次，失败可按策略阻断 |
| C-04 | 能力缺口 · P2 | 缺产品、架构、安全、发布等完整角色包 | 角色、工具、DoD、审批策略打包，非只加提示词 | 角色包不能绕过统一权限与验收 |
| C-05 | 能力缺口 · P2 | 团队经验缺少可衡量的晋升与效果反馈闭环 | 候选→验证→审核→晋升→撤回→效果评估 | 被否决经验不复活，变差不自动扩散 |
| C-06 | 能力缺口 · P2 | 候选实验：去中心化队列／看板自认领能否减少特定长任务的中继等待，尚未在 BMA 证明；现有 `Assign` 也不能安全承担认领 | 不直接建设完整队列；先比较单 Agent、单写入主线＋专家、受控并行三条基线，只有 trace 证实中继瓶颈且队列存在净收益时才启用第 12 节预案 | 默认关闭；必须优于同任务的最佳替代基线，而不只优于当前 `delegate`；质量、恢复、权限与上下文回归全部通过 |

## 3. 真实使用 P0：效率与上下文

### U-01 小型前后端任务的过度编排与串行等待

**真实反馈与问题定性**

同类前后端联查、增加功能页面并修改对应接口的任务，Claude Code 约 5 分钟可以完成；BMA 会派一个前端、一个后端子 Agent，反复协作约 30 分钟，且结果不一定正确。当前首先要修的是**小任务编排策略与交付正确性**，不是先建设更复杂的团队能力。

**静态核查发现的耗时放大点**

以下是可以从代码或配置确认的机制与候选原因，但各点实际占用多少墙钟仍需 trace：

1. **拆分提示存在直接冲突**：`backend/pkg/prompts/meta_agent.go` 一方面要求耦合任务归单 Domain，另一方面要求多文件创建类任务并行派发至少两个 Domain。前后端垂直小功能可能被后一条强行拆成两个 Agent，即使共享需求、接口约定和验收边界并不适合分工。
2. **计划链路可能层层叠加**：`backend/internal/domain/subagent/plan_confirm.go` 的 `submitPlanToUser`、`submitPlanToParent` 与逐级 `submit_plan/review_plan` 默认启用；单次等待上限为 600 秒且超时 fail-open，`plan_max_revisions: 0` 表示修订次数不设硬上限。上限不等于每次都等满，但多层计划、审批与修订会形成额外串行链路。
3. **协作前后还有额外模型与查询步骤**：`backend/internal/domain/memory/pipeline.go` 在近期事件超过阈值时可同步执行轻量摘要，配置超时为 120 秒；相同输入有 hash 缓存，因此不能写成“每轮必调”。多个 Domain 的 `call_sub_agents` 默认生成整合纪要，`deliverWaveDigest` 的轻量合并超时为 2 分钟；`blackboard_uptake.go` 的 Domain 每轮 `Assemble` 可进行黑板查询，超时 10 秒。
4. **配置不能直接靠拍脑袋降档**：`config/roles.yaml` 中 Domain 当前使用 `ark-glm-5.3-flash`、`thinking: high`，Meta 使用 `doubao-seed-evolving`、`thinking: low`。配置注释保留过高延迟及关闭思考后质量下降的历史记录，所以应做同模型、同任务对照，不能全局关闭思考后宣称完成优化。
5. **排除一个错误归因**：`waitForChildren` 当前是事件驱动等待，30 秒是唤醒检查周期；没有新邮件时不会白跑一次 LLM。不能沿用“每 30 秒一次大模型调用”的旧解释。

**最小修复顺序**

1. **先取得同任务基线**：固定一组真实前后端垂直功能任务，分别运行当前双 Agent 与单 Agent 路线，保持模型、工具、仓库版本和验收命令一致。记录端到端墙钟、首次正确交付时间与返工次数。
2. **补齐分段计时**：按 attempt 关联 LLM、工具、排队、计划提交／审批、事件摘要、整合纪要、黑板查询、Mailbox 空闲等待和返工，区分“实际执行”与“等待事件”。B-10 首先服务这个归因，不先做宏大运营平台。
3. **改成单垂直切片负责人默认路由**：小型、耦合的前后端功能默认由一个 Agent 对需求、接口、页面和测试端到端负责。只有职责与文件边界能独立、接口契约可冻结、预期并行收益大于协作成本时才拆分；不能再按“前端／后端”组织边界自动拆单。
4. **消除提示词冲突并收敛低风险附加步骤**：统一 `meta_agent.go` 的拆分规则；对低风险任务减少重复计划审批、非必要摘要和整合纪要，但保留必要测试、变更证据与高风险审批。不能为了提速关闭质量门。
5. **最后做模型与 thinking 档位对照**：同模型、同任务、同工具比较质量与延迟，依据数据调整角色配置；不把模型更换当作掩盖编排问题的第一手段。

**验收标准**

- 同一组任务至少覆盖只改前端、只改后端、前后端同改和接口契约不清四种情形；路由结果与拆分理由可审计。
- 同时比较墙钟、LLM 轮次、token、工具调用、测试通过、首次交付正确率和返工次数；只报告墙钟下降不算通过。
- 双 Agent 只有在质量不降低且净墙钟收益为正时才作为该类任务的默认路线；否则回退单 Agent。
- 计划等待、摘要、整合纪要、黑板查询与 Mailbox 空闲等待可以分别下钻，能解释主要耗时占比。
- 以重复对照的统计结果为准，不把“某一次跑进 5 分钟”作为验收标准。

**优先级／依赖**：真实使用 P0；先做最小路由与观测修复，不等 TeamSpec、嵌套团队或自动 DAG 调度。

### U-02 多源上下文污染与交接失真

**真实反馈与问题定性**

关闭旧块记忆写入、更新项目经验与用户画像写入后，污染已有所减轻，但仍需系统检查。这里必须同时检查两类问题：**无关、过期或冲突内容被注入**，以及**关键约束没有传给实际执行的子 Agent**。减少字符数本身不能证明语义正确。

**静态核查发现的上下文入口**

1. `backend/internal/agent/service_react.go` 的 `sessionDirectiveBlock`、`longTermMemoryBlock`：顶层每轮读取有效用户画像、项目记忆与决策账本；现有预算大致为用户 2600 runes、项目 2000 runes、决策块 2200 runes。预算能限制篇幅，不能判断内容是否与当前任务相关。
2. `backend/internal/domain/subagent/dispatcher.go` 的 `projectPrefsPrefix`：每个子 Agent 每轮注入项目偏好，最多 2000 runes；已剥归档并按最新小节控制预算，但仍需验证项目、任务和版本作用域。
3. `injectScopedRecall` 仍在子 Agent 启动时读取旧块记忆；`blackboard_uptake.go` 仍可能逐轮读取兄弟产出；`domain_profile.go` 还会注入领域摘要、文件清单和既有结论链。因此“旧块记忆停止写入”不等于“所有旧记忆读取消失”。
4. `runSubAgentOnce` 还组合项目自述、共享 Spec／KV、技能召回、块记忆与领域标签等前缀；`capSpawnPrefixes` 超预算会丢弃前缀。这既可能让无关内容占据预算，也可能让关键约束在裁剪时消失。
5. 子 Agent 不继承顶层完整用户决策账本与长期记忆，只获得项目偏好等裁剪通道。因此不能只在顶层检查污染，还要检查交接后的信息保真与权限边界。

**最小修复方案**

1. **建立逐次注入审计**：每次模型调用可关联 context item 的来源、项目／任务作用域、版本、状态、注入理由、目标 Agent、字符／token 预算与裁剪结果。日志记录引用与必要摘要，避免为排障再次泄露完整私密记忆。
2. **落地任务上下文清单**：复用 B-08 的 `TaskContextManifest` 思路，必传当前目标、冻结验收、相关约束、相关文件和依赖产出引用；项目经验、用户偏好、旧块记忆与兄弟产出只在相关且获授权时进入。
3. **明确来源优先级与冲突处理**：当前用户明确指令优先，其后是冻结任务 Spec／验收、项目硬约束、经授权且相关的经验与历史材料；发生冲突时暴露来源并请求裁决或暂停，不能由后注入的旧记忆静默覆盖新指令。
4. **补齐失效与撤回**：归档、撤回、过期、跨项目和任务完成后的内容不得继续注入；父级更新约束时生成新版本并通知相关子 Agent。不要重新打开旧块记忆自动写入，也不要再叠一层无来源总结。
5. **同时检查过多与不足**：审计每次注入为什么存在，也审计关键 Spec、验收与决策是否被 `capSpawnPrefixes` 或摘要裁剪掉。

**验收标准**

- 构造相互冲突的用户偏好、项目经验、旧块记忆与最新指令，最终只能采用明确的权威版本，并保留冲突处理记录。
- 两个项目交替执行、同项目不同任务切换、不同权限子 Agent 交接时，不串用私密或无关内容。
- 父级修改验收标准、撤回经验、归档记忆后，新调用不再使用旧版本；进行中的子 Agent 按版本策略更新或停止。
- 子 Agent 启动、压缩、恢复与热驻复用后，关键目标、约束和验收不丢失，过期结论不复活。
- 对照任务成功率、返工率、无关注入项和关键约束缺失数；不能只用上下文变短或记忆条数变少宣称解决。

**优先级／依赖**：真实使用 P0；先审计与最小作用域修复，再演进 B-08 的统一上下文包和 C-05 的组织学习。

## 4. P0：身份与权限正确性

### A-01 顶层 Agent 的身份来源不一致

**证据**

- `backend/internal/domain/subagent/dispatcher.go` 的 `roleIDFromAgentID`：无 `/` 的顶层 ID 固定返回 `meta`。
- `backend/internal/domain/subagent/dispatcher.go` 的 `dispatchOne`、`parentAssignableTools`、`assignableSkills`、`visibleTargets`、`scanStuck` 等路径使用该推断。
- daily 顶层实际是 `domain`，fast 顶层实际是 `doc_assistant`；工具执行适配器使用的角色身份来自另一条角色装配路径。

**真实场景影响**

同一个 DomainAgent 在 daily 顶层与 cluster 子节点中，可能在派发权限、可授工具、技能分配、消息可见性或心跳分类上得到不同判定。工具执行硬门仍可能使用真实角色，因此目前只能确认**身份真相源不一致**，不能未经测试就断言存在某个具体越权漏洞。

**修复方案**

1. 在 Agent 创建／恢复时生成权威 `AgentIdentity{AgentID, SessionID, RoleID, ParentID}`，由运行时注册表或只读上下文传递。
2. `CanCall`、工具授权、消息可见性、技能分配和角色专属超时都读取同一身份，不再从 `agentID` 形状猜权限。
3. 旧树恢复可使用字符串解析作兼容迁移，但迁移结果必须落为明确角色，不能长期保留双真相源。
4. 将“展示 ID 解析”和“安全身份判断”拆成不同函数，防止以后复用错误。

**验收标准**

- fast、daily、cluster 三档分别覆盖直接执行、下拆、拒绝角色、发消息、工具授予和技能授予。
- 同一个角色在顶层与子节点只享有策略明确允许的差异，不因 ID 是否有 `/` 改变身份。
- 使用带连字符的动态角色 ID、重启恢复后的旧 ID，权限判断仍正确。
- 工具 Schema 可见、Dispatcher 授权、最终工具硬门三者的拒绝原因可关联到同一 roleID。

**优先级／依赖**：P0；应先于 A-02、A-03 实施，否则后两者仍可能拿错调用者身份。

### A-02 `Parents` 白名单语义需要纠偏

**证据**

`backend/internal/domain/role/registry.go` 的 `CanCall`：

- caller 为 meta／domain、callee 为 fixed／dynamic 时，只检查 `callee.CanBeCalled`，没有读取 `callee.Parents`；
- 非编排者分支遍历的是 **caller 自己的 `Parents`**，再与 calleeRoleID 比较；
- `config/roles.yaml` 中 `scout`、`light` 等角色的 `parents: [meta, domain]` 表达的是允许谁调用它，与上述读取方向不一致。

**真实场景影响**

配置维护者可能以为收窄 `parents` 就能限制派发者，实际主路径并未执行这层限制。角色文档、管理界面与真实授权会漂移。

**修复方案**

1. 将目标角色的 `Parents` 明确定义为“允许的直接父角色集合”，对 fixed／dynamic 的目标执行校验。
2. `CanBeCalled` 表示是否允许作为子 Agent，`Parents` 表示允许的父级，两者是且关系；空集合的默认策略必须写入配置规范。
3. 保留 `Domain→Domain`、`Meta→Meta` 等循环／平级调用禁令，不能因白名单泛化而放开。
4. 若叶子确实需要再派发，另写明确的 `allowed_callees`，不要复用 `Parents` 表达反向关系。

**验收标准**

- 配置只允许 meta 的角色，domain 调用时被拒；允许 domain 的角色则正常。
- `CanBeCalled=false` 即使父级匹配也拒绝。
- Domain→Domain、叶子跨级调用、未知角色和动态角色均覆盖。
- Registry 单测与 Dispatcher 端到端测试使用同一张权限矩阵，避免只测配置函数。

**优先级／依赖**：P0；依赖 A-01 的真实 caller roleID。

### A-03 工具可授集合不等于父级当前有效工具面

**证据**

`backend/internal/domain/subagent/dispatcher.go` 的 `parentAssignableTools`：当推断角色为 meta、角色不存在或 tools 为空时，把 `d.tools.ToolNames()` 的全部注册内置工具放入可授集合。`resolveChildTools` 再以此校验下放；插件可见性另有独立天花板。

**真实场景影响**

Meta 自身的 YAML Schema 工具清单已经收窄，但它仍可能把自身未暴露的注册内置工具授给子角色。这是最小权限策略不一致；是否形成实际风险还取决于子角色白名单、工具硬门、挂载和参数，应通过测试确认，而不是直接命名为已验证安全漏洞。

**修复方案**

- 可授上限 = 父级当前有效内置工具面 ∩ 注册工具；运行时 `extend` 授权也只能扩展策略明确允许的能力。
- 插件工具继续独立校验“父可用 + 对子角色可见 + 已挂载”，不要混入全注册内置工具集合。
- `tools_mode=only` 是收窄，不是绕过父级上限；空 tools 的语义必须在角色配置中统一，不再被授权路径解释成特权。
- 拒绝项返回结构化原因，便于区分未注册、父无权和子不可见。

**验收标准**

- Meta／Domain／叶子均不能授出自身无权使用的工具。
- 同一角色在 daily 顶层与 cluster 子节点的授权上限符合同一策略。
- `extend`、`only`、插件未挂载、动态工具注销后的授权均覆盖。

**优先级／依赖**：P0；依赖 A-01。

## 5. P1：安全、交付与运行可靠性

### B-01 生产环境需要显式安全 Profile

**现状证据**

`config/config.yaml` 当前为 `trust_mode: full-auto`、`tool_approval_disabled: true`，破坏性操作审批默认直通；角色工具硬门、路径沙箱和命令黑名单仍然保留，不能概括为“没有任何安全门”。

**影响与修复**

本机个人开发与多人共享／真实生产不是同一信任边界。新增 `development` 与 `shared/production` profile：后者默认关闭全自主模式，按写文件、执行命令、外部 Agent、插件破坏性操作分级审批。启动／部署预检报告实际生效组合，而不是只展示某一个开关。

**验收**

- 共享环境配置为 full-auto 时启动预检明确阻断或要求显式风险确认。
- 切换 profile 不削弱角色白名单、路径沙箱和命令黑名单。
- 审批超时、用户离线、审批人身份和工具参数摘要都有留痕。

**优先级**：P1；若直接部署到共享环境，则上升为上线阻断项。

### B-02 计划审批是活性优先机制，不是高风险硬保证

**现状证据**

`backend/internal/domain/subagent/plan_confirm.go` 的 `submitPlanToUser`、`submitPlanToParent`：审批等待超时、父级不可达、顶层 `ask_user` 通道失败都会 fail-open；`config/config.yaml` 为 `plan_confirmation_enabled: true`、超时 600 秒、`plan_max_revisions: 0`。`submit_plan` 是工具机制，模型不调用时没有一个覆盖所有变更动作的“已批准计划令牌”硬门。

**修复方案**

- 按风险设置 `approval_policy=fail_open|fail_closed`；普通开发保留现状，高风险写入、发布、凭据操作必须 fail-closed。
- fail-closed 超时进入 `waiting_approval`／`blocked`，不是把任务伪造成失败，也不是自动批准。
- 记录 `plan_id`、计划版本／hash、审批人、决定、原因和有效范围；计划变化后旧批准自动失效。
- 修订次数、升级仲裁和取消都要有界；高风险任务可将已批准 plan_id 作为变更工具的前置条件。

**验收**

模拟用户不答、父 Agent 销毁、父会话挂起、计划被驳回多次、审批后计划变更和进程重启，确保每种情况都符合所选策略，且不会静默执行未批准的高风险计划。

### B-03 把“完成”与“已验证完成”分开治理

**现状证据**

- `backend/internal/domain/subagent/acceptance.go` 的 `LoadTesterConfig`：独立 `test_assistant` 验收由 `<workDir>/.bma/tester.yaml` 控制，默认 off；on 总执行，auto 还需 `auto_prompt` 命中，默认最多 2 轮。
- `config/config.yaml` 的 `reflection_max_rounds: 3` 是反馈重试上限，不是质量必过保证。
- Dispatcher 的校验路由与 `verify_kind=auto` 按角色选择强度；code_assistant／test_assistant／code_reviewer 走 executable，普通 Domain／其他叶子默认可为 none。judge 不可用时，有 L0 客观证据的路径可带警告降级，无 L0 才有 Unverified 出口。

**修复方案**

建立按任务风险的 Definition of Done：

| 风险 | 必需交付证据 | 验证不足时的状态 |
|---|---|---|
| 普通问答／草稿 | 答复与来源说明 | completed，不宣称独立验收 |
| 文件／代码修改 | 变更清单、必需命令及退出码、结果摘要 | unverified／failed，不能仅写“测试通过” |
| 研究／内容 | 来源、关键事实核验、未决假设 | partially_verified 或带未决项交付 |
| 高风险生产动作 | 独立验收、审批、回滚方案 | blocked，等待人工 |

独立验收只用于风险与收益匹配的任务，不能对所有任务强套同一个测试器。

**验收**

- 默认 auto=none 的任务在 UI 和结果中清楚标为未独立验证。
- reflection 达轮次上限、judge 不可用、缺 L0 证据都不会被渲染为“验收通过”。
- 必需命令失败时，即使模型给出完成文本也不能改变机器验证结论。
- 独立 tester 崩溃／超时按配置降级，未决项随最终交付保留。

### B-04 增加可选的 Team 累计预算与排队治理

**现状证据**

`config/config.yaml` 的 `tool_call_max_rounds: -1`、`session_max_wall_clock_min: 0`；子 Agent 默认墙钟 120 分钟、全局并发 8、单 session 派发总数 30。`token_budget_per_role`／`context_token_budget` 是每个 Agent 单轮上下文阈值，不是跨 Agent 累计成本预算；`doc/TODO.md` 第 16 项记录了预算墙暂不实施的旧决策。

**真实场景缺口**

并发数和单任务墙钟能限制形状，却不能回答“这棵树已经花了多少，还能再派几个”。超额排队也需要排队时长、取消和上限管理，不能只统计执行墙钟。

**修复方案**

1. 先以观测模式记录 session 内实际 token、模型单价（若可得）、调用次数、执行与排队时间。
2. 增加 Team 预算账本：派发前预留，结束按实际结算，失败／取消释放未用预留。
3. 阈值动作分级：告警→禁止新增派发→只用便宜模型→暂停→取消；默认不替旧决策强制打开硬熔断。
4. 记录排队入队／出队时间，增加可选排队超时和优先级，防止全局 FIFO 下长任务独占。

**验收**

- 多子 Agent 并发、重试、暂停恢复、取消后，预留总额与结算总额可对账。
- 达上限后不再扩队，已完成与部分产出仍可交付。
- 超额排队有明确状态，取消排队任务不消耗执行配额。

### B-05 文件防覆盖需要从“建议加锁”升级为共享路径协议

**现状证据**

- `backend/internal/domain/tool/registry.go` 的 WriteFile／EditFile 描述与 `backend/internal/domain/tool/builtin_mtime_test.go`：支持 `expected_mtime`，冲突时拒写；未传时保持 advisory，不强制校验。
- 同轮工具并行有同路径写互斥；Dispatcher 的 `checkActiveSiblingDomain` 解决同父同名活跃 Domain 查重，不是跨 Domain／跨会话文件所有权。
- worktree 是显式选项；`dispatchOne` 在热驻开启时拒绝热驻 Domain worktree，`reuse_agent_id` 与 worktree 也互斥。

**修复方案**

- 对声明为共享写路径的文件强制 `expected_mtime` 或内容版本，不允许模型省略后静默覆盖。
- 派发时生成文件读写集与所有权／租约，冲突任务选择排队、只读、worktree 或人工处理；不要另造全局串行锁。
- worktree 保存持久 manifest：repo、base revision、branch、任务、patch hash、merge 状态与锁。合并冲突与 base 漂移继续走已有合并门。
- 明确 `RunCommand`、外部 Agent 等可能绕开文件参数校验的路径，纳入工作区策略，不能声称只锁 WriteFile 就解决全部写入。

**验收**

- 两个 Domain 与两个不同会话同时修改同一文件，后写者收到冲突而不是覆盖。
- 不同文件仍可并行；租约超时、Agent 死亡、重启后能安全回收。
- worktree 完成、拒绝、三方合并、冲突与重启恢复后，patch 只能合入一次且 base 可追踪。

### B-06 Mailbox 采用至少一次语义，业务效果必须幂等

**现状证据**

`backend/internal/mailbox/mailbox.go` 的 `Drain` 先将内存消息翻为已读，再回调已读标记；`backend/internal/bootstrap/bootstrap.go` 的 `WithReadMarker` 异步 `MarkRead`，注释明确崩溃可能重投。`LoadUnread` 恢复保证不丢未读的方向，但消费与标读之间存在重复注入窗口。

**修复方案**

- 明确协议为 **at-least-once delivery**，不要承诺传输层 exactly-once。
- 消息增加稳定 `message_id/idempotency_key`，消费方持久记录处理阶段；同一业务动作按 key 去重。
- 将“已取出、已注入、已处理、可标读”拆开，崩溃后按阶段恢复；有副作用的工具自身也需幂等或补偿。
- 死信提供原因、保留期限、人工重放和丢弃审计。

**验收**

在 Drain 后、标读前、工具副作用后、结果回传前分别注入崩溃：消息可以重复投递，但文件写、派发、审批与通知不能重复生效。死信重放沿用原幂等键。

### B-07 恢复目标应是“明确续跑方式”，不是“运行时完整复活”

**现状证据**

- `config/config.yaml` 的 `restore_sessions: true` 只负责启动预热最近 50 个会话；其他数据按查询或懒加载恢复。
- Dispatcher 的热驻槽、执行 gate、墙钟计时器、动态角色 map、计划等待者与 worktree handle 等仍含内存状态。
- `backend/internal/domain/subagent/dispatcher.go` 的恢复派发路径注明原 `mode`、`responsibility` 未随节点完整持久化，复活按默认 ReAct 重跑。
- worktree 文件和 patch 可在磁盘，但运行 handle 在 Dispatcher 内存中，不能据此声称合并门自动恢复。

**修复方案**

先建立恢复矩阵，再选择性持久化，不做“全量保存一切”的大重写：

| 对象 | 最小需恢复内容 | 无法安全自动恢复时 |
|---|---|---|
| 子任务 | 原 DispatchSpec、attempt、状态、父 ID、验证策略 | 标记可重派／需人工，不静默换模式 |
| 计划审批 | 计划版本、审批人、等待状态、决定 | 高风险 fail-closed，重新请求审批 |
| worktree | manifest、base、patch、合并状态 | 可人工 review，不自动重复 merge |
| 墙钟／队列 | 开始、截止、入队与取消状态 | 明确过期或重新入队策略 |
| 热驻 | 可重建的会话历史与角色 | 冷启动并说明，不伪装上下文仍在 |

**验收**

在排队、LLM 调用、工具执行、审批等待、邮件回传、worktree 待合并各阶段重启。每个任务只能进入已定义的续跑、重派或人工接管路径，不能无声消失，也不能无提示改变执行模式。

### B-08 用任务上下文包替代“把全部记忆广播给团队”

**现状证据**

`backend/internal/domain/subagent/dispatcher.go` 的 `runSubAgentOnce` 使用 `WithDirectiveBlock`，当前接的是 `projectPrefsPrefix`，不是顶层“决策账本 + 用户／项目长期记忆”的完整组合。子 Agent 另有 Spec、共享 KV、Mailbox、技能和角色上下文通道。

**修复方案**

引入带版本与作用域的 `TaskContextManifest`：

- 必传：任务目标、冻结验收标准、相关约束、文件清单、Spec 版本、依赖产出引用；
- 可选：经检索与授权裁剪的用户偏好、项目经验、相关历史证据；
- 不传：与任务无关的私密画像、其他项目内容、未审核的团队经验；
- 记录来源、作用域、版本和过期条件，父级更新约束时生成新版本并通知相关子 Agent。

**验收**

修改关键约束后子 Agent 收到正确版本；压缩／恢复后仍能追溯 Spec；两个不同项目与同一会话的不同权限角色不发生记忆串用。无需向所有子 Agent 注入完整顶层记忆。

### B-09 建立统一、持久的 AgentResult，但保留现有 Mailbox 通道

**现状证据**

`backend/internal/domain/subagent/dispatcher.go` 的 `notify`、`notifyTerminal` 负责回传与台账终态；`backend/internal/mailbox/mailbox.go` 的 `Message` 已有 `Payload`、`FilesModified`、状态与线程字段。当前不是“完全没有结构化数据”，而是结果摘要、失败标记、校验说明、文件清单和事件／台账分散在多个结构中。

**修复方案**

定义版本化 `AgentResult`，通过 `Message.Payload` 引用或携带，并持久化：

```text
result_id / task_id / attempt_id / agent_identity / spec_version
status / summary / artifacts[{path,hash,kind}]
evidence[{kind,command,exit_code,source}]
verification / unresolved[] / files_modified[]
usage / started_at / finished_at / provenance / idempotency_key
```

Mailbox 仍是通知通道，父 Agent 仍读短摘要；机器状态转换、UI 与后续依赖任务读取同一 Result，不再从自然语言猜是否成功。

**验收**

长摘要截断不会丢失工件与证据；部分成功、未验证、失败、取消有明确状态；下游任务只能引用已完成且满足依赖策略的 artifact hash；重复通知不产生新结果。

### B-10 从“有日志”升级到 Team 级运营指标

**现状证据**

项目已有 session_events、session_logs、任务台账、Agent 树、看板、效率查询和 Prometheus 指标。`backend/internal/agent/metrics.go` 的 `metricsCollector` 聚合调用次数、超时与耗时，token 参数目前不进入这个聚合器。因此缺口不是“没有可观测性”，而是跨 Agent 的团队级聚合与归因。

**修复方案**

按 session／未来 team_id 聚合：排队时长、执行时长、LLM 与工具耗时、重试率、返工率、未验证率、Mailbox 重投、worktree 冲突、模型与角色成本。先建立基线，再定 SLO，不凭感觉设阈值。

**验收**

- 一棵树的 token／费用等于各 attempt 之和，可与模型日志对账。
- 能区分“排队慢、模型慢、工具慢、等待审批、返工多”。
- 一次交付可下钻到失败任务、重试、验证证据和最终结果。

## 6. P2：真实 AgentTeam 还缺的能力

### C-01 一等、持久、版本化 TeamSpec

**建议模型**

```text
TeamSpec: id + version + roles[] + policies + default_budget + dod + memory_scopes
TeamRun:  team_id + spec_version + session_id + root_agent + status + budget_ledger
```

TeamSpec 管可复用定义，TeamRun 管一次执行，现有 Role Registry 管角色本身，Dispatcher 继续负责单次派发。首版可以是配置文件 + 校验 + Dispatcher 适配，不需要先造新编排引擎。

**验收**：同一 TeamSpec 可重建团队；一次运行固定 spec_version，运行中改配置不污染旧运行；角色删除／停用不影响历史审计。

### C-02 有界嵌套子团队，而非无限制 Domain 互派

当前 `Registry.CanCall` 明确拒绝 Domain→Domain，hub-and-spoke 是已有架构决策。大型项目需要的是“一个总 Domain 下挂有边界的子项目团队”，不是把星型拓扑改成自由对等网络。

**建议**：子团队有最大深度、最大节点、独立预算与文件范围；权限只能收窄，父团队可查看与取消后代，根 Meta 仍是治理边界。保留普通三层作为默认路径。

**验收**：循环定义拒绝、深度／扇出受限、子团队不能越过父级工具与预算、取消能级联、跨团队消息仍走受控通道。只有真实多子项目案例证明收益后再做。

### C-03 可选的依赖就绪自动调度

`write_plan` 已有 DAG 校验与依赖门；依赖未满足会拒派，完成后通过 waiter、通知与唤醒让父 Agent 继续决策。**它不会自动创建就绪子任务**，且当前“只通知”是明确用户决策。

**建议**：增加 opt-in `auto_dispatch` 执行器，只读取已批准计划与已冻结 DispatchSpec，按依赖就绪触发；每次仍经过 A-01～A-03 权限门、B-04 预算门和 B-05 冲突门。默认保留现有通知模式。

**验收**：依赖乱序完成、多个依赖同时完成、父 Agent 暂停、进程重启、重复通知时，同一个 `(plan_version,node_id,attempt)` 最多创建一次有效派发；前置失败可配置跳过、阻断或等待人工。

### C-04 可复用的组织角色包，而不是只加 Prompt

固定角色中未形成完整的产品经理、架构师、发布经理、安全评审等组织职能包；动态角色机制提供了扩展入口。真实缺口是角色与工具、预算、DoD、审批和交付模板的组合，而不是角色名称数量。

**建议**：角色包声明职责、可用工具、可派发关系、默认验证、交付 Schema 与审批边界；安全评审和发布经理的机器权限必须由 Registry／工具硬门实现，不能只靠提示词自律。

**验收**：安装角色包后通过统一权限矩阵与冲突检查；无对应工具的角色不能靠文本获得能力；角色升级有版本兼容与回归测试。

### C-05 有反馈的组织学习闭环

`block_memory_write_enabled: false`、`memory.write_mode: shadow` 是防污染默认值。现有 SessionEvolver、技能与长期记忆治理可产生和管理候选，不应再简单打开旧块记忆自动写入。

**建议**：团队经验按项目／角色／任务类型隔离，保存来源任务与证据；经离线 eval、抽样复核或人工审核后才能晋升；记录采用后的成功率、返工率与成本，可撤回、过期和回滚。

**验收**：被否决方案不会经另一 Agent 再次自动注入；未验证经验不能进入 curated 层；经验撤回后新团队不再召回；晋升前后有同预算质量对照。

## 7. 真实使用场景缺口矩阵

| 场景 | 当前可直接使用的基础 | 当前主要短板 | 对应项 |
|---|---|---|---|
| 前后端小型垂直功能 | 现有工具可读写前后端并执行测试，单 Agent 能端到端负责 | 提示词可能按文件数强制双 Domain，协作、审批和摘要叠加，慢且未必正确 | U-01、B-10 |
| 小修改／日常问答 | fast／daily 顶层直达、基础工具与记忆 | 不应强行进入完整 Team；同时确保角色身份与权限不漂移 | U-01、A-01～A-03 |
| 多模块大型研发 | Meta→Domain→叶子、Spec、看板、Mailbox、树 | 依赖不自动执行、结果契约分散、缺子团队与团队预算 | B-04、B-09、C-01～C-03 |
| 多 Agent 并行写码 | 同轮路径互斥、可选 mtime、worktree 与合并门 | 共享路径没有强制所有权，热驻与 worktree 不能随意组合 | B-05、B-07 |
| 无人值守长任务 | 子任务墙钟、事件留痕、会话恢复、通知 | 审批可能超时放行、累计预算无硬止损、重启恢复语义不完整 | B-02、B-04、B-06、B-07 |
| 跨会话持续项目 | 会话历史、领域档案、长期记忆与项目偏好 | 多源上下文可能冲突、越界或在交接裁剪时丢失；缺可复用团队定义与学习反馈 | U-02、B-08、C-01、C-05 |
| 共享／高风险生产 | 角色硬门、沙箱、黑名单、三档信任模式、审批 | 默认 full-auto 不适合直接上线，缺高风险 DoD 与强制审批链 | A-01～A-03、B-01～B-03 |
| 研究／内容交付 | 多角色执行、rubric／可执行校验、结果回传 | 缺统一来源证据与未决项契约，不能把普通完成当事实核验 | B-03、B-09 |
| 团队运营与复盘 | 事件、日志、树、台账、效率数据 | 缺单任务分段耗时及 Team 级排队、成本、返工和失败原因聚合 | U-01、B-10 |

结论：**不是所有场景都要集群化，也不是进入 cluster 就必须拆成前后端两个 Agent**。小任务优先 fast／daily 或单垂直切片负责人；只有多模块、长时、可独立并行或高风险任务才启用相应 Team 治理。

## 8. 分阶段修复路线

阶段顺序是当前产品的修复顺序，不是安全重要性降级；共享环境或高风险上线仍必须先满足 A、B 类安全门。

### 阶段 0：冻结真实任务与行为基线

- 固定一组真实前后端小型垂直功能，分别记录当前双 Agent 与单 Agent 路线的墙钟、LLM 轮次、token、工具调用、首次正确交付与返工；补齐计划、摘要、整合纪要、黑板查询和 Mailbox 等待的分段计时。
- 为 U-02 建立注入清单基线，选取冲突指令、跨项目切换、子 Agent 交接、压缩恢复四类场景，记录每个上下文项的来源、作用域、版本与裁剪结果。
- 将 fast／daily／cluster 三档的角色、工具、派发、消息、超时行为写成参数化回归矩阵，并为 `Parents`、工具授予和默认验收建立当前行为测试。
- 保留一条多模块研发、一条并行写码、一条无人值守长任务，防止小任务优化破坏复杂任务能力。

**出口**：真实低效能分段归因，污染场景可复现；身份与权限影响面有基线，不能用推测替代结果。

### 阶段 1：完成 U-01／U-02 最小修复

- 统一 Meta 拆分提示，小型耦合垂直功能默认单 Agent；只有可证明净收益时才拆双 Agent。
- 收敛低风险任务中的重复计划审批、非必要摘要与整合步骤，用同模型对照决定 thinking 档位，不全局盲目关闭。
- 上线上下文注入审计、任务作用域筛选、来源优先级、失效撤回与关键约束保真；不重新打开旧块记忆自动写入。

**出口**：同任务同验收下显著减少不必要等待与返工；冲突、跨项目、交接、压缩恢复回归通过。提速但正确性下降不算完成。

### 阶段 2：统一身份和授权

按 A-01 → A-02 → A-03 实施，最后统一 Schema 可见、Dispatcher 可授与工具执行硬门。此阶段不扩 Team 功能。

**出口**：三档权限矩阵通过；父不能越权授权；目标 `Parents` 真正生效；恢复旧节点仍有明确角色。

### 阶段 3：让交付可控

落地 B-01～B-04：生产安全 profile、风险审批、分级 DoD、可选预算账本与观测模式。优先复用现有 tester、verify、tree、ledger 与 metrics。

**出口**：高风险未审批不执行，验证不足不冒充通过，达到预算后停止扩队且保留部分交付。

### 阶段 4：让并行与故障可控

落地 B-05～B-10：共享文件协议、Mailbox 消费幂等、最小恢复矩阵、统一上下文包、AgentResult 和 Team 聚合指标。按事故频率分批实施，避免一次性重写 Dispatcher。

**出口**：并行覆盖、重启、重复邮件、延迟回传、待合并 worktree 均有端到端故障注入验收。

### 阶段 5：按真实需求建设 Team 能力

先按第 11 节建立单 Agent、单写入主线＋按需专家、受控并行三条对照路线，再依据真实任务选择 C-01 TeamSpec、C-02／C-03；角色包 C-04 与组织学习 C-05 复用上述权限、DoD 和结果协议。C-06 看板自认领不再作为确定建设路线：只有观测证明中继等待是主要瓶颈、任务符合去中心化协作前提，且预研对照优于前三条路线时，才允许把第 12 节候选预案拆成开发任务。

**出口**：同一团队定义可重复运行，权限与预算只能逐层收窄，团队经验有晋升和撤回证据；任何多写入或自认领模式都必须相对同任务最佳替代基线证明质量—时间—成本净收益，而不是只证明比当前低效 `delegate` 更快。

## 9. 明确不建议现在做的事

1. **不要再按文件数量或“前端＋后端”标签强制拆单**，也不要未做对照就全局关闭思考。先证明路由收益，再调整模型与协作策略。
2. **不要把上下文变短等同于污染解决**。必须同时验证语义相关、版本有效、权限正确和关键约束未丢失。
3. **不要只新增一批角色 Prompt 就宣称完成 AgentTeam**。身份、工具上限、验收与恢复没统一前，角色越多，故障面越大。
4. **不要默认重新打开旧块记忆自动写入，也不要再叠一层无来源总结**。先建立注入审计、候选治理与效果反馈，保留当前防污染默认值。
5. **不要把计划 fail-open 的文案改成“已保证审批”**。没有代码门、计划版本与失败策略时，文档不能提供虚假保证。
6. **不要承诺 Mailbox 或恢复 exactly-once**。现实目标是至少一次通知、幂等业务效果、可审计恢复。
7. **不要把所有文件写全局串行化，也不要强制所有任务使用 worktree**。按共享路径与风险选择冲突控制。
8. **不要为了“团队”直接引入完整 DAG 引擎或自由 Domain 对等网络**。保留当前星型治理，在明确场景上有界扩展。
9. **不要未重评旧决策就默认开启预算硬熔断**。先观测、可配置、分级动作，避免长任务被误杀。
10. **不要把“有测试源码”写成“本轮测试通过”**。本轮只做静态评审，修复后的结果必须另附命令、环境和输出。

## 10. 修复执行卡模板

后续把每一项拆成代码任务时，统一填写以下字段，避免只写方向而没有完成定义：

```text
编号／标题：
类别与优先级：
当前行为（路径 + 函数／符号）：
复现场景与预期／实际：
基线（墙钟／LLM 轮次／token／返工／正确性）：
上下文注入清单（来源／作用域／版本／理由／裁剪结果）：
兼容性约束与既有 TODO 决策：
代码改动范围（限定模块，不预先全仓重写）：
配置／Schema／持久化迁移：
单元测试／集成测试／故障注入：
可观测性与回滚：
文档同步（21／22／配置／API）：
实际验证命令、环境、结果、未执行项：
```

本清单的下一步应从 **U-01／U-02 的真实任务基线与最小修复**开始：先让一个前后端小功能由单 Agent 正确、快速交付，并证明上下文来源可控，再扩展团队能力。

## 11. 业界证据重评与 BMA 主路线

> 本节用于纠正此前把“看板自认领”写成确定主路线的偏差。结论不是用另一种固定拓扑替换现有拓扑，而是按任务结构选择最少协调：能单写入就不多写入，能只读协作就不并行修改，必须并行修改时先保证隔离、集成与验证。

### 11.1 直接判断：BMA 的问题不是“还有一个 Leader”

当前 BMA 的主要工程问题不在“保留 Leader”这个名称，而在**拆分策略静态、按前后端组织边界拆任务、多个写入者共享隐式决策，以及协调、摘要和审批叠加后仍缺统一交付责任**。前后端垂直小功能通常共享需求、接口和验收，把它拆成两个 Agent 会先制造上下文与集成成本，再尝试用更多沟通弥补；这与 U-01 的真实低效现象一致。

跨 260 种配置、五种架构和六类基准的对照研究没有发现普遍最优拓扑：架构收益取决于任务可分解性、工具密度和单 Agent 基线，工具密集任务会承担协调开销，缺少中心验证的架构更容易传播错误。因此，“去掉 Leader”“增加看板”“增加 Agent 数”都不能单独作为先进性标准。[Towards a Science of Scaling Agent Systems](kimi-cite://web?url=https%3A%2F%2Farxiv.org%2Fhtml%2F2512.08296v3 "citation")

### 11.2 四类路线的证据与不可迁移边界

| 路线 | 一手证据支持了什么 | 不能据此推出什么 | 对 BMA 的定位 |
|---|---|---|---|
| **单 Agent／单写入主线** | OneFlow 在七类基准上显示，同一模型承担多个角色时，单 Agent 多轮执行可达到相近效果，并通过 KV cache 复用降低成本；角色名不同不必然需要不同实例。[OneFlow](kimi-cite://web?url=https%3A%2F%2Farxiv.org%2Fhtml%2F2601.12307 "citation") | 其编码评测主要是 MBPP／HumanEval，不等于所有大型仓库任务都应单 Agent；真正异构模型、独立探索和并行环境交互仍可能有额外价值。 | **小型、耦合、前后端垂直功能的默认路线**；先修 U-01，而不是先换协调拓扑。 |
| **单写入主线＋按需专家** | Cognition 的生产经验支持一个写入主线，其他 Agent 提供检索、干净上下文评审或跨模型第二意见；其评审 Agent 不预享编码 Agent 历史，反而能避开长上下文负担，写入者再依据完整需求筛选反馈。[Cognition](kimi-cite://web?url=https%3A%2F%2Fcognition.ai%2Fblog%2Fmulti-agents-working "citation") | 这是生产经验而非所有任务的统一对照实验；不能承诺每次评审都有净收益，也不能让 reviewer 绕过需求直接改代码。 | **首选多 Agent 增量**：scout、fresh reviewer、smart friend 按收益启用，最终写入与交付仍属一个负责人。 |
| **中心协调的受控异步并行** | CAID 在长时共享工件任务上，通过依赖图、独立 worktree、显式 merge 和测试门提高得分；移除真实 worktree 隔离会退化。[CAID](kimi-cite://web?url=https%3A%2F%2Farxiv.org%2Fhtml%2F2603.21489v2 "citation") | 其实验中多 Agent 的运行时和成本普遍更高，并行没有显著缩短端到端时间；不能把准确率收益写成提速，也不能直接套到 BMA 的五分钟级小功能。 | **大型、长时、可独立验证模块的优先并行路线**；Manager 可以薄化，但依赖与集成责任必须保留。 |
| **共享队列＋增量上下文＋自认领** | DeLM 在长时编码任务上显示了减少中继等待、重复探索和屏障等待的潜力；消融表明收益依赖早期共享、peer status 与可纠正的持久共享上下文，不是只加一条队列。[DeLM](kimi-cite://web?url=https%3A%2F%2Farxiv.org%2Fhtml%2F2606.10662v2 "citation") | DeLM 每个 Agent 有独立 workspace 并生成完整方案，和 BMA 多 Agent 共写一个仓库、只交付一个结果不同；其部分设置中 n=4 比 n=2 更快但准确率下降、集成错误和缺陷传播增加，Cost/Task 也可能更高。 | **有条件的候选实验**：只针对长时、可复用中间成果、真正可并行的任务，不先建设完整三表队列。 |

CooperBench 进一步解释了为什么“沟通更多”不等于“协作更好”：652 个协作编码任务中，GPT-5 和 Claude Sonnet 4.5 的双 Agent 成功率约为 25%，大致只有单 Agent 完成两项特性的 50%；通信减少了表层合并冲突，却没有显著提高最终协作成功率。主要失败是对伙伴状态的错误预期、不可验证承诺和语义集成，而不是缺少一个认领按钮。[CooperBench](kimi-cite://web?url=https%3A%2F%2Farxiv.org%2Fhtml%2F2601.13295v1 "citation")

主流产品也没有形成“无 Leader 自认领是唯一最佳实践”的共识。Claude Code Agent Teams 虽支持共享任务表和完成后自认领，但仍保留 team lead，功能默认关闭且标为实验；官方明确建议顺序任务、同文件修改和强依赖工作优先单会话或子 Agent。[Claude Code Agent Teams](kimi-cite://web?url=https%3A%2F%2Fcode.claude.com%2Fdocs%2Fen%2Fagent-teams "citation") OpenAI Agents API 同样把并行子 Agent 用于可独立部分，由主 Agent 协调并汇总结果。[OpenAI Agents API](kimi-cite://web?url=https%3A%2F%2Fopenai.com%2Findex%2Fintroducing-the-agents-api%2F "citation") Anthropic 的多 Agent Research 成功场景主要是宽度优先研究，其工程文章也明确指出多数编码任务真正可并行的部分少于研究任务；可借鉴的是按复杂度限制 Agent 数，以及用工件引用减少主 Agent 转述，而不是照搬研究拓扑。[Anthropic Research](kimi-cite://web?url=https%3A%2F%2Fwww.anthropic.com%2Fengineering%2Fbuilt-multi-agent-research-system "citation")

### 11.3 应删除的是“强制中继”，不是“最终责任”

“是否需要 Leader”应拆成多个独立机制，不能作为一个角色名去留来决策：

| 机制 | BMA 应采用的实现 | 是否可以没有持续运行的 LLM Leader |
|---|---|---|
| 小任务执行 | 一个垂直切片负责人直接完成，不建立 Leader—Worker 结构 | 可以，而且默认应如此 |
| 大型任务拆分与 Spec 冻结 | 一次性规划者＋服务端契约校验，或明确的 Manager | 可以不持续运行，但规划版本必须有人负责 |
| 就绪计算与任务认领 | 确定性调度器／事务队列，不靠 Leader 逐条转述 | 可以，机器控制面更合适 |
| 上下文共享 | 版本化 Manifest、增量事实和工件引用，不靠 Leader 摘要全部 History | 可以，但必须有作用域、证据和纠正语义 |
| 写入、合并与最终验收 | 单写入负责人，或唯一集成者＋显式 merge／test 门 | **不能无人负责** |
| 异常、预算与最终交付 | 根会话／集成责任拥有停止、升级和交付权 | **不能无人负责** |

因此 BMA 的目标不是“有 Leader”或“无 Leader”二选一，而是**最少必要协调**：小任务取消团队结构；大任务把 Leader 从每一步对话中继薄化为控制面与集成责任；即使未来采用 DeLM 式去中心化执行，也不能变成无 Spec 所有者、无集成者、无最终验收的自由 Agent 网络。

### 11.4 BMA 的任务自适应目标路由

| 目标模式 | 启用条件 | 写入与上下文规则 | 是否承诺提速 |
|---|---|---|---|
| `single_writer` | 小型、耦合、前后端垂直功能；接口和需求需要一起推理 | 一个 Agent 端到端写需求相关代码并验收；不为“前端／后端”标签拆实例 | 作为 P0 默认基线，以真实对照验证 |
| `single_writer_experts` | 大仓库检索、困难调试、高风险变更或需要独立评审 | 只有一个写入者；scout 只读，fresh reviewer 主要看 diff＋验收并自行补上下文，smart friend 提供第二意见 | 不默认承诺；按问题类型证明收益 |
| `controlled_parallel` | 长时、多模块、契约可冻结、工件可独立验证，并行收益能覆盖集成成本 | 中心依赖协调＋独立 worktree／等价隔离＋唯一合并者＋测试门；共享 Spec 版本化 | 不默认承诺；主要目标可能是提高完成率 |
| `self_claim_experiment` | 长时、多个独立工作区、早期部分成果能被其他任务复用，且 trace 证明中继等待是主要瓶颈 | 事务认领＋增量事实＋peer status＋纠正机制＋唯一集成；不能只实现看板 UI | 仅作为实验假设，不作为产品承诺 |

现有 `delegate` 保留兼容和作为诊断基线，不再因为“任务包含多个文件”自动成为新任务默认。`coordinated` 高风险路径继续保留人工审批与显式集成。上述路由选择应记录理由、预期收益和回退条件；路由本身也要接受 U-02 的上下文审计与 A 类权限门。

### 11.5 实施顺序与看板实验启动门槛

1. **先完成 P0 止血**：落实 U-01 单垂直切片路由、U-02 注入审计、A-01～A-03 和必要 B 类交付约束，取得当前 `delegate` 与 `single_writer` 的同任务基线。
2. **做最小的多 Agent 增量**：在同一写入主线上按需加入只读 scout、fresh reviewer 或异构第二意见。reviewer 不继承冗长实现 History，写入者负责把评审意见与原始需求、项目约束核对，避免评审循环和上下文二次污染。
3. **确有并行写入需求时先建受控并行**：优先补依赖契约、worktree／等价隔离、显式 merge、测试与唯一集成，不以“去掉 Leader”或“缩短墙钟”为未经验收的目标。
4. **最后才评估 C-06**：先用轻量预研模拟队列与增量共享，不直接建完整持久化队列；第 12 节仅在下列门槛全部满足后才可转成开发任务。

C-06 的启动门槛必须同时满足：

- 多次真实 trace 证明中继等待、重复探索或屏障等待占了主要可优化耗时，且不能用工件引用、异步通知或更简单的受控并行消除；
- 任务属于长时、可独立执行、可提前共享部分成果并有可执行验收的类型，不是 BMA 当前最主要的小型耦合前后端功能；
- 同模型、同工具、同仓库和同验收下，已与 `single_writer`、`single_writer_experts`、`controlled_parallel` 比较，而不是只与当前低效 `delegate` 比较；
- 已明确唯一集成责任、上下文作用域、纠正与撤回、权限、预算和故障恢复方案；
- 预研显示首次正确交付、端到端墙钟、总成本或完成率至少一项有稳定净收益，且其他关键指标不退化。

所有路线统一记录首次正确交付时间、端到端墙钟、排队／等待／执行／集成耗时、LLM 轮次、token 与成本、测试通过、集成返工和人工接管次数。**看板自认领不是既定主路线，也不是被否定删除；它是需要击败更强基线后才能立项的候选架构。**

## 12. C-06 混合式看板自认领：候选实验预案（未立项）

> 本节保留此前整理的技术预案，但**不再是默认建设路线，也不能直接拆成开发任务**。只有第 11.5 节启动门槛全部满足并完成实验立项后，才按本节实施；否则先执行第 11.5 节的前三条路线。它复用 U-01、U-02、A-01～A-03、B-04～B-10 的前置修复，设计原则仍是**薄控制面，而不是无控制面；拉取式认领，而不是 LLM 自由抢任务；唯一集成责任，而不是完成一批子任务后无人收口**。

### 12.1 先固定实验行为与范围

若实验立项，可在实验分支增加 `self_claim`；下表仅描述候选目标行为，不表示当前产品已有这些模式：

| 模式 | 适用任务 | 控制方式 |
|---|---|---|
| `single` | 小型前后端垂直功能、联查、耦合修改 | 一个 Agent 对需求、接口、页面与测试端到端负责 |
| `delegate` | 现有 Meta→Domain→叶子任务 | 保留当前 `call_sub_agent(s)` 与通知式依赖门，默认不变 |
| `self_claim` | 契约清楚、写入边界独立、确实值得并行的任务 | 轻量规划者冻结任务；Worker 拉取认领；机器调度；集成任务统一交付 |
| `coordinated` | 高风险、需求不清、跨项目或强依赖任务 | 保留强协调、人工审批与显式集成 |

硬性边界：

1. 小任务不得为了使用新能力而进入 `self_claim`；U-01 的单 Agent 路由先于本功能落地。
2. 首版不培养一批长期空转、反复询问看板的 LLM Agent。Worker 的取任务动作由运行时代码执行，**认领成功后才启动 ReAct／产生 LLM 调用**。
3. 首版只开放固定叶角色、冷启动、非 worktree、非热驻复用任务；建议先允许 `code_assistant`、`test_assistant`、`scout`。Domain、热驻、worktree 和嵌套 Team 在核心协议通过故障测试后分别接入。
4. 不修改 `Registry.CanCall` 的 hub-and-spoke 限制，不开放 Domain→Domain 自由互派，不为了实现队列重写整个 Dispatcher。
5. 规划仍可由现有 Meta 一次性完成；Meta 不需要在执行期间持续运行。完全无固定 LLM leader 只作为后续实验 profile，不是本阶段默认目标。

### 12.2 目标运行闭环

```text
用户目标
  → 路由：single / delegate / self_claim / coordinated
  → self_claim：Meta 或轻量规划者一次性生成 PlanEnvelope
  → 校验角色、工具、依赖、验收、写入边界、预算与审批
  → 事务写入任务、初始事件和看板投影，冻结 plan_version / spec_version
  → Ready 信号唤醒 WorkerSupervisor（无 LLM 轮询）
  → Worker 按已注册角色拉取 ClaimNext
  → 数据库事务原子认领，返回 task_id + attempt_id + 冻结任务契约
  → DispatchClaimed 复用 Dispatcher 权限、树、台账、并发池和执行链
  → 后台心跳续租；执行过程只发布必要事实／失败／工件引用
  → CompleteAttempt 校验 owner + attempt + plan_version + result_id
  → 同事务写 AgentResult、任务终态、依赖就绪事件与看板投影
  → WorkerSupervisor 唤醒后续任务，不再等待 Meta 转述和重新派发
  → 唯一 integration 任务依赖全部交付任务，合并／检查整品并产生最终结果
  → 根会话仅被唤醒一次接收最终 AgentResult；异常才回到规划者或用户
```

这里“子 Agent 自己认领”指 Worker 运行时的拉取语义，不是把角色、工具和权限作为自由入参交给模型自我声明。首版不需要增加一个会让空转 Agent 烧 LLM 轮次的 `claim_next` 模型工具；后续若支持已存活 idle Agent 主动拉取，也必须调用同一个 `ClaimService`。

### 12.3 模块划分与现有代码接入点

新增包只放协调协议，不能再造一套 Agent 执行引擎：

| 模块 | 建议位置 | 职责 |
|---|---|---|
| 任务契约 | `backend/internal/team/contracts.go` | `PlanEnvelope`、`TaskContract`、`Claim`、`ClaimBinding`、事件与错误类型；不 import Dispatcher |
| 事务存储 | `backend/internal/store/team_task_store.go` | 任务、attempt、事件的事务写、条件认领、续租、fencing、恢复查询 |
| 认领服务 | `backend/internal/team/claim_service.go` | 调 Store 与资格策略，统一 `ClaimNext`、`Heartbeat`、`Complete`、`Fail`、`Cancel` |
| Worker 控制面 | `backend/internal/team/worker_supervisor.go` | Ready 信号、角色候选、并发上限、退避、启动与回收；不调用 LLM 决定调度 |
| 执行适配 | `backend/internal/domain/subagent/dispatcher.go` | 新增 `DispatchClaimed`，复用 `dispatchOne` 与既有 ReAct／工具／树／验收 |
| 完成适配 | `backend/internal/domain/subagent/plan.go` | claimed 任务按 `task_id` 精确回写；旧任务仍按 Domain 回写，两者不得混用 |
| 增量上下文 | `backend/internal/team/fact_feed.go` + 现有上下文装配层 | 类型化事实、游标读取、纠正追加、工件展开；不广播完整 History |
| 看板投影 | 现有 `backend/internal/board/`、`agent/query_react.go` | 展示任务、owner、attempt、lease 和结果；不是跨进程认领锁 |
| 装配 | `backend/internal/bootstrap/bootstrap.go` | 建立 Store／Service／Supervisor，连接 Dispatcher，管理启动与关闭顺序 |
| 配置 | `backend/internal/config/config.go`、`config/config.yaml` | 功能开关、允许角色、租约、重试、Worker 上限；默认关闭 |

为避免 import cycle，`team` 包定义 `ClaimExecutor` 接口，由 bootstrap 适配到 `subagent.Dispatcher`；`subagent` 可以消费 `team.ClaimBinding`，但 `team` 不反向 import `subagent`。存储层沿用现有项目分层，不直接 import `board` 或 `subagent`。

### 12.4 冻结任务契约，而不是只存标题和 Domain

现有 `PlanTask{id,title,domain,depends_on,acceptance}` 对展示够用，对自动执行不够。`self_claim` 使用以下目标契约；字段名可按项目风格调整，但语义不能丢：

```go
// 目标模型，不是当前代码。
type PlanEnvelope struct {
    SessionID         string
    PlanVersion       int64
    ExecutionMode     string // self_claim
    Goal              string
    IntegrationTaskID string
    Tasks             []TaskContract
}

type TaskContract struct {
    TaskID         string
    RoleID         string
    Domain         string
    Task           string   // 自包含简报，不能只靠 title 猜任务
    Responsibility string
    DependsOn      []string
    Acceptance     []string
    RequiredTools  []string // 资格检查；不是自我扩权清单
    ReadSet        []string
    WriteSet       []string
    Priority       int
    MaxAttempts    int
    SpecVersion    string   // 冻结规范化内容的 hash
    Context        TaskContextManifest
    Integration    bool
}

type Claim struct {
    SessionID   string
    TaskID      string
    AttemptID   string
    AttemptNo   int
    PlanVersion int64
    SpecVersion string
    WorkerID    string
    LeaseUntil  time.Time
    Task        TaskContract
}

type ClaimBinding struct {
    TaskID, AttemptID, WorkerID string
    AttemptNo, PlanVersion      int64
    SpecVersion                string
}
```

`write_plan` 保持旧入参兼容；只有 `execution_mode=self_claim` 时才要求上述完整字段。规划者给出候选后，服务端必须重新校验：

- `task_id` 唯一、依赖存在且无环，IntegrationTaskID 指向唯一集成任务；
- `role_id` 在首版允许列表中，角色存在，父／Worker 与目标关系通过 A-01～A-03 统一权限门；
- `RequiredTools` 只用于判断能否胜任，实际有效工具仍由 Registry、父级上限、插件可见性和挂载交集计算；
- 任务简报、验收、Spec 版本和 B-08 的上下文清单齐全，过期 Spec 不得进入队列；
- 两个非 worktree 任务的 WriteSet 重叠时，默认拒绝并行或标出显式先后依赖，不能靠模型自觉；
- 集成任务默认依赖所有交付任务。缺少集成责任时，除纯只读且明确 `result_aggregation=none` 的计划外，不允许启动；
- 全局开关关闭时请求 `self_claim` 必须明确报“未启用”，不能静默改成自动派发，也不能把不完整契约落到旧看板后失控执行。

计划落库后对规范化契约计算 `spec_version`。修改目标、验收、依赖、角色或工具需求必须提升 `plan_version`／对应 `spec_version`，不能原地偷偷改任务文本。

### 12.5 存储与迁移：认领必须从事务开始

现有 `TaskBoard.Assign` 只修改内存后调用整板 `SaveBoard`；`Manager.persist` 失败仅记日志。这一模型继续服务旧路径即可，**不能在其上增加几个字段就宣称实现可靠认领**。

新增三张表，Go `EnsureTeamTaskSchema` 为运行时权威，同时提供下一个编号的 `migrations/*.sql`：

| 表 | 关键字段与约束 | 用途 |
|---|---|---|
| `team_tasks` | `owner/session_id/task_id` 主键；`plan_version/spec_version/revision/status/role_id/domain/priority`；`depends_on/contract/acceptance/context` JSONB；`current_attempt/lease_owner/lease_expires_at/max_attempts/result`；时间戳 | 当前任务投影与条件认领 |
| `team_task_attempts` | `attempt_id` 主键；`session_id/task_id/attempt_no/worker_id/agent_id`；`plan_version/spec_version/status/heartbeat_at/lease_expires_at`；`idempotency_key/result/error`；唯一 `(session_id,task_id,attempt_no)` | 执行代数、fencing、恢复与审计 |
| `team_task_events` | 全局递增 `event_id`；`owner/session_id/task_id/attempt_id/kind/payload/spec_version/created_at/published_at`；必要的幂等唯一键 | 同事务 outbox、Ready 通知、增量事实游标与故障排查 |

所有表带 `owner`，并加入 `sessionScopedTables` 的删除覆盖及相应测试。索引至少覆盖 `(session_id,status,priority,created_at)`、`(lease_expires_at)`、`(session_id,event_id)` 和 attempt 幂等键。

真相源规则必须写死：

- 旧 `delegate`：继续以现有 TaskBoard／树／台账路径运行。
- `self_claim`：`team_tasks + team_task_attempts` 是任务与执行代数真相源，`session_boards` 只是可重建的 UI／上下文投影；禁止 `boardAssign`、`boardUpdate` 再按 Domain 批量覆盖这些任务。
- 新计划建立时锁对应 `session_boards` 的计划行或等价 session 级计划锁，串行化“改计划、认领、完成”这些小规模控制面事务；不要把锁扩大到 LLM 或工具执行期间。
- 认领失败不能 fail-open 成内存认领。数据库不可用时不启动新 Worker，已运行 attempt 按租约和取消策略收口。
- 老 `session_boards` 数据只读兼容；首次 opt-in 可导入为未执行历史，但不得把旧 `in_progress` 自动变成可重跑任务。回滚不删除旧表。

### 12.6 状态机与原子 Claim 协议

任务对外状态尽量复用现有看板值，避免 UI 再发明一套颜色语义：

```text
pending                    未就绪或已就绪但尚未认领；ready 是查询结果，不另造漂移状态
  → in_progress            已持有有效 attempt + lease
  → done                   机器规则接受的完成
  → delivered-unverified   有产出但缺必需证据，默认不满足下游依赖
  → failed                 可重试时先回 pending，否则终态失败
  → blocked                前置失败、Spec 失效、等待审批或需人工
  → cancelled              用户／会话显式取消；不自动重排
```

`attempt` 另有 `claimed/running/completed/failed/expired/cancelled` 状态。`ready` 由“pending＋当前计划版本＋所有依赖 done＋Spec 有效＋审批满足＋角色与写入资格满足”实时计算。

`ClaimNext` 的一次事务按以下顺序实现：

1. 从服务端 Worker 注册信息取得 `session_id/worker_id/role_id` 和权威 `AgentIdentity` 候选；不得信任调用方自报 role。
2. 锁计划行，检查模式仍为 `self_claim`、版本未过期、会话未取消、预算与并发仍允许。
3. 选择该角色可执行的最高优先级 pending 任务；锁任务并重新读取所有依赖。依赖只认 `done`，失败或未验证依赖不能静默视为满足。
4. 再次检查 Spec 版本、attempt 上限、WriteSet／执行类约束和有效工具资格。没有任务返回 `ErrNoClaimableTask`，不启动 Agent。
5. 条件更新任务为 `in_progress`，写 `current_attempt+1`、worker、lease；同事务插入 attempt 和 `task_claimed` outbox 事件。任一步失败全部回滚。
6. 提交后才返回 `Claim`。随后由 Supervisor 调 `DispatchClaimed`；如果启动失败，走同一个带 fencing 的 `Fail/Release`，不能直接调用旧 `Assign`。

必须提供可区分的错误：`ErrNoClaimableTask`、`ErrTaskAlreadyClaimed`、`ErrTaskNotReady`、`ErrPlanVersionStale`、`ErrNotEligible`、`ErrAttemptFence`。调用方只对 `ErrNoClaimableTask` 做空转退避，其余错误进入明确修复或告警路径。

完成、失败和心跳都必须带 `(task_id,attempt_id,worker_id,plan_version)`。数据库条件更新影响行数为 0 时返回 `ErrAttemptFence`：旧 Worker 即使迟到，也不能覆盖新 owner、新 attempt 或新计划。

### 12.7 WorkerSupervisor 与 Dispatcher 的最小改法

**WorkerSupervisor**

- 订阅计划创建、任务终态、依赖就绪和取消事件；事件只负责唤醒，唤醒后仍回数据库查真相。增加低频兜底扫描，不能把通知到达当作唯一正确性保证。
- 为每个 session 维护有界角色候选和冷启动 Worker 数，先向现有并发池预留容量，再认领；容量不足时不认领、不烧墙钟。
- 未取得任务使用有抖动的退避；有 Ready 信号立即重查。整个过程不产生“看看有没有任务”的 LLM 调用。
- 每个 Worker 只执行一个有效 attempt；完成后回到受控候选池或销毁。首版不复用热驻 Domain，避免旧上下文跨任务污染。

**Dispatcher**

新增 `DispatchClaimed(ctx, claim)`，不要复制 `dispatchOne` 的权限代码：

1. 把工具 `Execute` 层已有的参数／Spec 预检与 `dispatchOne` 的权限预检整理成双方共用的内部函数。当前 `DispatchProgrammatic` 注释明确把部分参数合法性交给调用方，新入口不能沿用“调用方保证”的假设。
2. 将 `ClaimBinding` 放进新的 `dispatchOpts`，同时携带冻结 `TaskContextManifest`；继续走 `CanCall`、有效工具交集、角色任务匹配、并发池、树、台账、取消和 ReAct 执行。
3. claimed 路径使用 `task_id` 做精确依赖复核，不再用 `FindAllByDomain` 推断执行对象；认领已完成的 `boardAssign` 不重复执行。
4. `runSubAgentOnce` 优先从冻结 Manifest 装配任务上下文，不再为同一任务叠加全量项目偏好、无关领域档案和兄弟 History。普通派发继续旧路径，待 U-02 统一后收敛。
5. 成功、未验证、失败、超时、panic、软停和取消都进入统一的 `finishClaimAttempt` once 咽喉。它先写事务结果，再调用现有 `notifyTerminal` 做短通知；不能以 Mailbox 成功发送作为任务完成前提。
6. `boardUpdate` 对带 ClaimBinding 的任务只更新投影，真实状态由 `CompleteAttempt` 决定。热驻 `idle_pool.go`、failure prune 等旧终态路径在正式支持这些执行类之前不得接入 claimed 任务。

首版若发生“Claim 成功但 Dispatcher 启动失败”，attempt 进入可恢复状态；租约到期后按重试策略处理。不能留下永远 `in_progress` 的任务。

### 12.8 心跳、结果、依赖与唯一集成

**心跳**

- 每个 attempt 启动独立后台 ticker，建议初始 `heartbeat_interval=30s`、`lease=120s`，满足间隔显著小于租约；实际值由故障测试调整。
- 心跳只证明进程仍持有执行权，不把“仍有 LLM 文本输出”当作唯一存活依据。每次续租仍校验 attempt、plan_version 和取消状态。
- LLM、工具、审批等待期间均可续租；显式取消不续租。计划版本变化时按策略 checkpoint＋停止，不能用心跳把旧 Spec 永远续活。

**结果**

复用 B-09 的 `AgentResult`，`CompleteAttempt` 输入至少包含：

```text
task_id / attempt_id / result_id / idempotency_key
owner + worker_id + agent_id + plan_version + spec_version
status / summary / artifacts[{path,hash,kind}]
evidence[{kind,command,exit_code,source}]
verification / unresolved[] / files_modified[] / usage
```

在同一个事务里校验 fencing、写唯一 Result、更新 attempt／task、写事件与看板投影。`result_id` 或幂等键重复时返回已存结果，不重新触发下游。缺 B-03 要求的证据时进入 `delivered-unverified` 或 `blocked`，不能仅凭 Agent 文本写 done。

**依赖与集成**

- 默认只有 `done` 解锁下游。`delivered-unverified` 保持可见但不满足依赖，由集成者／用户决定补验证或终止。
- 前置失败默认把直接后继标 `blocked`，保留其他独立分支；重试成功并落新 Result 后才重新计算就绪集合。
- 每个共享工件计划有一个 integration 任务，只有它能执行最终合并／整品交付；它仍然是带 lease 的普通任务，因此只能有一个有效 attempt。
- 非 worktree 首版由集成者检查变更集合、依赖接口和整品命令。后续接 worktree 时，集成者统一调用现有 `merge_worktree` review／merge 流程，记录 base、patch hash 和 merge 状态；子 Worker 不得各自合并主目录。
- 根会话只消费最终集成 `AgentResult` 和异常通知，不再逐轮接收所有子任务全文。Meta 可以按需生成最终人读说明，但机器完成状态不依赖这段说明。

### 12.9 增量共享上下文与 U-02 的落地

不要新增“团队总 History”。`team_task_events` 既作控制面 outbox，也可承载少量协作事实；大正文和 diff 放现有文件工件，事件中只存引用。

事实记录采用统一信封：

```text
fact_id / kind=fact|fail|completion|correction|artifact
session_id / task_id / attempt_id / spec_version
source / created_at / expires_at / supersedes
summary / evidence_refs[] / artifact_refs[]
```

装配规则：

1. 每个 Agent 只读取与其任务、显式依赖和已授权项目作用域相关、且位于自身游标之后的事件；默认只注入短摘要与引用，需要细节再读文件／Result。
2. `correction` 追加新记录并通过 `supersedes` 指向旧事实，不修改历史。新调用不得再采用被纠正版本。
3. 当前用户指令、冻结 Spec、验收和项目硬约束继续走 B-08 Manifest，不淹没在普通事实流里。
4. 关键 Spec 变化不是普通增量消息：提升版本，阻止新认领，并让在途 attempt 按停止／checkpoint 策略处理。
5. 团队事件不会因“完成得好”自动写入用户画像、项目长期经验或旧块记忆；跨会话晋升仍走 C-05 的验证与撤回机制。
6. 每次实际注入记录 `fact_id/cursor/spec_version/trimmed/reason`。压缩与恢复保存游标和 Manifest 版本，不能把已撤回事实重新展开。

### 12.10 重试、恢复、改计划与回滚

| 场景 | 必须行为 |
|---|---|
| Claim 后、Agent 注册前崩溃 | lease 到期；确认无活跃执行后把 attempt 标 expired，再按策略重排 |
| LLM／工具执行中崩溃 | 同上，但有外部副作用或已有文件写时不盲目自动重放；先按工件与幂等策略判定 |
| 旧 Worker 恢复后迟到完成 | `ErrAttemptFence`，保留其结果作审计，不改变当前任务 |
| 完成已落库、Mailbox 未发 | 重启后由 outbox 补通知，业务状态不重复推进 |
| 重复完成／重复通知 | 按 `result_id/idempotency_key` 返回原结果，下游最多被有效解锁一次 |
| 用户取消 | 取消当前 Agent 与租约，任务标 cancelled，释放预算／文件预留，不自动重试 |
| 可重试失败 | 保留旧 attempt 与证据，任务回 pending；新 attempt 不复用旧身份和旧游标 |
| 达到最大重试 | task failed，独立分支继续，后继 blocked，集成者收到未决项 |
| Spec／计划改变 | 新版本冻结；旧版本停止新认领；在途任务明确完成旧版、checkpoint 或取消，不能混用 |
| 功能开关回滚 | 停止新 Claim，不新建 Worker；已运行任务选择 drain 或显式 cancel，绝不静默转旧 Domain 派发 |

恢复扫描由启动时的 Supervisor 执行：先处理过期 lease，再恢复 outbox 通知，最后开放新 Claim。所有自动重试只适用于幂等或可判定无副作用的任务；发布、外部写入、已 merge patch 等动作默认转人工，不“为了高可用”重复执行。

### 12.11 配置与默认安全姿态

以下为建议形状，最终字段以 `AgentConfig` 实际接入为准：

```yaml
agent:
  team_execution:
    self_claim_enabled: false       # 总开关，默认 false
    allowed_roles: [code_assistant, test_assistant, scout]
    max_workers_per_session: 3
    claim_lease_sec: 120
    heartbeat_interval_sec: 30
    max_attempts: 2
    ready_rescan_sec: 5             # 仅数据库控制面兜底扫描，不是 LLM 轮询
    auto_integration_enabled: false # 首批先观察，验收后开启
    allow_hot_resident: false
    allow_worktree: false
```

启动时校验允许角色、租约与心跳比例、Worker 上限和重试上限。非法配置 fail-fast；生产共享环境仍需 B-01／B-02 的审批门。配置变更只影响新计划，已有计划沿用冻结策略快照。

`auto_integration_enabled=false` 时，早期联调只允许已交付任务等待既有 Meta／用户显式收口，不自动启动集成 Agent；PR 4 完成且进入正式 self-claim 灰度前必须显式开启。若产品要求全程无人值守，则应在计划准入时同时检查 `self_claim_enabled` 与该开关，避免任务全部结束后无人集成。

### 12.12 分批开发顺序：每批可独立验收和回退

不要一次性开“架构重写”大分支。按以下 PR／提交批次实施，每批都保留 `self_claim_enabled=false` 的逃生舱，直到最后灰度。

#### PR 0：基线与路由止血（先做，不写队列）

- 文件：`backend/pkg/prompts/meta_agent.go`、U-01 选定的路由入口、观测事件。
- 完成：删除“多文件即至少两个 Domain”与“耦合任务归单 Domain”的冲突；小型前后端垂直功能默认 `single`；记录选择理由。
- 同时补齐 U-02 注入审计和 A-01～A-03 的前置修复，不能用新调度绕过旧身份问题。
- 验收：同一组真实任务比较当前双 Agent 与单 Agent；记录首次正确交付时间、墙钟、LLM 轮次、token、返工和测试。

#### PR 1：契约、Schema 与兼容投影，功能仍关闭

- 新增 `team/contracts.go`、`store/team_task_store.go`、Ensure DDL 与迁移；扩展 `write_plan` 仅在显式 `self_claim` 时解析完整契约。
- 加 `sessionScopedTables`、owner、索引和 JSON 编解码测试；现有 `board.Assign`、旧 `write_plan` 和 `delegate` 测试全部保持。
- 写 PlanEnvelope 校验：环、未知依赖、缺集成者、角色不允许、WriteSet 冲突、Spec 过期均拒绝。
- 验收：迁移可重复执行；旧会话能读；新表可随会话硬删；开关关闭无任何新执行行为。

#### PR 2：只实现事务 Claim／Heartbeat／Complete，不接 Agent

- 实现 `ClaimService` 和 Store 事务、错误类型、attempt fencing、幂等结果与 outbox。
- 用 fake Worker／fake clock 写单元和 PostgreSQL 集成测试，不调用 LLM。
- 必测：N 个并发 Worker 只有一个成功；依赖乱序；未验证依赖；旧 attempt 完成；计划版本变化；重复 result；租约过期；DB 回滚后无半成品投影。
- 验收： race 与事务故障测试通过，才允许进入真实 Dispatcher 接线。

#### PR 3：接一种最小执行路径

- 实现 WorkerSupervisor、`DispatchClaimed`、ClaimBinding 和精确任务回写。
- 首批只开放一个固定叶角色（建议 `code_assistant`）、冷启动、非 worktree、无热驻；Domain 与其他角色仍走旧路径。
- 复用现有工具权限、执行池、树、台账和测试证据；不能另写一套 ReAct 执行器。
- 验收：两个独立任务并行、一个有依赖任务等待；无任务时零 LLM 调用；相同 Domain 的两条细任务不会被整组误标完成。

#### PR 4：统一结果与唯一集成任务

- 将 B-09 AgentResult 接入 CompleteAttempt，集成者读取 artifact hash 和 evidence，而不是读长摘要猜状态。
- 自动生成／校验 integration 节点；根会话只等最终结果与异常。
- 验收：部分分支失败时独立分支继续、后继阻断、集成者能列出未决项；重复完成只解锁一次；缺证据不能显示已验证。

#### PR 5：接增量事实与上下文裁剪

- 实现 fact feed、游标、correction、artifact 引用和注入审计；将 claimed 路径切到冻结 Manifest。
- 验收：跨项目、权限不同、Spec 修改、事实撤回、压缩恢复均无旧内容复活；上下文变短同时关键约束保留。
- 此批不重开旧块记忆写入，不把团队事件自动晋升长期记忆。

#### PR 6：故障恢复与运维闭环

- 接启动恢复、过期 lease、outbox 补通知、取消、改计划、预算预留／结算和关闭顺序。
- 在 Claim 后、执行中、结果提交前后、通知前后分别故障注入；验证旧 Worker 无法覆盖新 attempt。
- 补齐看板 owner／attempt／blocked 原因和按 task／attempt 下钻的耗时视图。

#### PR 7：受控灰度与对照推广

- 先只读／独立测试任务，再独立代码模块；不先拿高风险或紧耦合前后端小功能试验。
- 固定模型、工具、仓库版本、指令和验收，对照 `delegate`、`single`、`self_claim`；完全无 leader 后续再作第四条实验路线。
- 只有质量不降低、恢复与权限全部通过，且中位净墙钟或成本有稳定收益，才扩大任务类型与默认启用范围。

#### PR 8：按真实收益逐项扩展，不捆绑首版

- 需要隔离共享工件时再接 worktree 和唯一合并者；需要 Domain 时接其全部热驻／暂停／终态路径；需要复用 Team 时再接 C-01 TeamSpec。
- 每项扩展独立开关和回归矩阵。不得因为核心 Claim 已稳定，就默认热驻、worktree、嵌套团队也自动安全。

### 12.13 测试矩阵与推广门槛

| 层 | 必测内容 |
|---|---|
| 契约单测 | DAG、唯一集成、角色、工具需求、WriteSet、Spec／Manifest、旧入参兼容 |
| Store 集成 | 原子竞争、依赖门、事务回滚、租约、幂等、fencing、版本、会话删除 |
| Dispatcher | 权限不绕门、精确 task 回写、并发池、取消、panic、超时、未验证、结果 once |
| 上下文 | 增量游标、纠正、撤回、跨项目、权限裁剪、压缩恢复、关键约束保真 |
| 故障注入 | 各阶段崩溃、重复邮件、旧 Worker 迟到、DB 暂不可用、启动恢复 |
| 端到端 | 独立并行、依赖链、共享工件、部分失败、唯一集成、用户取消 |
| 性能对照 | 首次正确交付时间、端到端墙钟、等待／排队／执行分项、LLM 轮次、token、返工 |

Go 侧至少执行受改包测试、相关 store／subagent／board／agent 测试和 `go test -race` 覆盖并发包；PostgreSQL 集成测试使用真实事务与真实并发，不用单连接 fake 代替原子性证明。文档交付时只记录实际执行过的命令与结果，不把“写了测试”写成“测试通过”。

每次观测至少带 `session_id/task_id/attempt_id/plan_version/spec_version/worker_id/agent_id`，并区分：

```text
planning → waiting_dependencies → waiting_capacity → claimed
→ dispatching → queued_exec → running → verifying → integrating → terminal
```

推广门槛不是“子任务都能变绿”，而是：

1. 所有必需验收、显式依赖检查与权限矩阵通过；
2. 重复运行下首次正确交付和中位墙钟优于适用基线，且没有以更多返工或 token 换取表面速度；
3. 重复认领、旧 attempt 覆盖、依赖误解锁、跨项目污染均为 0；
4. 默认关闭可完整回退，旧 `delegate` 行为不变；
5. 小型耦合功能仍由 `single` 路由，不因 self-claim 上线而回退成前后端双 Agent。

### 12.14 实验立项后的开发完成定义

未满足第 11.5 节门槛时，C-06 保持“未立项”，不进入下列开发验收。实验立项后，只有以下各项都能勾选，才可从“待开发”改为“已实现”：

- [ ] U-01 单 Agent 路由、U-02 注入审计、A-01～A-03 权限前置已完成并回归。
- [ ] 契约、三张表、迁移、会话删除与旧看板兼容已落地。
- [ ] Claim／Heartbeat／Complete 是数据库事务，支持 lease、attempt、版本与幂等 fencing。
- [ ] Worker 空转不调用 LLM，认领后复用 Dispatcher 执行链，没有第二条权限旁路。
- [ ] 所有终态按精确 task／attempt 收口，旧 Domain 批量回写不接触 claimed 任务。
- [ ] AgentResult、依赖级证据和唯一 integration 任务形成最终交付。
- [ ] 增量事实、Manifest、纠正与压缩恢复通过上下文污染矩阵。
- [ ] 故障注入、取消、重启、计划变更、回滚与预算行为符合第 12.10 节。
- [ ] 对照数据证明适用任务有净收益；默认开关、允许角色和执行类范围已写入配置文档。
- [ ] 21 号文档只描述实际已实现行为；本节仍保留目标设计与真实实施状态的区分。
