# 多层编排对比：BlockMemoryAgent vs 阿里 AICP Agent 军团

> 对照文档。对象一：本仓库当前的多层编排（单 ReAct + `call_sub_agent` 递归派发）。对象二：阿里 AICP 混沌工程平台的「Agent 军团 + 共享黑板」方案（源：https://mp.weixin.qq.com/s/FopdLWQq9RU9CaaNTIzx6g ，《AI Native 下的混沌工程：Agent 军团如何重新定义系统韧性验证》）。
>
> 目的：提取对方可借鉴的优秀点，指出其缺点与不适用处，给出按优先级排序的落地建议。不是照搬方案，而是按本仓库已有架构（递归 ReAct + Agent 树 + mailbox + 共享记忆 + 块记忆）做增量判断。

---

## 1. 当前项目多层编排现状（基于代码）

核心是**单 ReAct 循环 + 递归 `call_sub_agent`**，不是固定分层状态机。

### 1.1 两层角色 + 可选叶子（2026-08-22 任务 83 拍平：domain 默认自执行）

| 层 | 角色 | 职责 | Token 预算 |
|---|---|---|---|
| 顶层 | MetaAgent | session 级，接用户目标，派 DomainAgent，整合终答 | 150K |
| 执行层 | DomainAgent | 领域直接执行者：读文件/写代码/跑验证默认自执行，仅自主判断需要时才下拆叶子 | 150K（触限暂停可恢复） |
| 可选叶子 | 固定助手（code/ui/test/doc/prompt_reviewer） | 可选下拆执行者，单函数/单文件改动；不再是默认路径 | 150K（触限部分回灌） |

- 派发入口 `backend/internal/domain/subagent/dispatcher.go:640`（`callSubAgentTool.Execute`）。
- 角色注册 `backend/internal/domain/role/registry.go`：`meta`/`domain` 保留，支持运行时动态注册（`Register`/`Unregister`）。
- 路由规则由 LLM 读工具描述决定（dispatcher.go:614 `Description` 动态拼角色清单），非配置式路由表。

### 1.2 编排基础设施

- **Agent 树**（`backend/internal/domain/orchestrator/tree.go`）：显式 `Register`/`Finish`/`Pause`/`Resume`/`SetCancel`，支持 introspect / cancel / 持久化。HTTP `/tree`、`/cancel` 依赖此树。
- **Mailbox**：异步子 Agent 摘要回传；`send_message` 工具支持任意 Agent 间请求/回复（dispatcher.go:450），是代码↔测试验证闭环的紧耦合协作原语。
- **共享记忆**（SharedMemory KV 槽位 + `spec` 固定槽）：派发前强制 `WriteSpec`（`specEnforcementEnabled`），文件 mtime 校验防 stale（dispatcher.go:1296 `hasFreshSpec`）。
- **块记忆**（pgvector）：召回→执行→沉淀闭环，LLM 事实提取逐条落库（dispatcher.go:1373 `saveBlockMemory`）。
- **`verify_and_fix` 工具**：verifyloop 状态机折叠进 ReAct 作工具调用（dispatcher.go:520），替代独立编排器自动触发。
- **pause/resume**：DomainAgent 触限存完整 history，用户「继续」续跑（dispatcher.go:1021 `ResumePaused`）。
- **工具白名单按角色过滤**：meta 无执行类工具，domain 见全套执行+派发工具（`call_sub_agent` 保留为例外下拆通道），叶子只见执行类工具（dispatcher.go:895）。
- **派发限额 + 同领域兄弟去重**（dispatcher.go:707、:719）。
- **session_logs**：完整 LLM I/O 落库（react_agent.go:568 `logLLMCall`）。

### 1.3 关键特征

- 控制流单一：只有 ReAct loop（verifyloop 已折叠为工具），无双控制流。
- Agent 间通信：mailbox（点对点）+ 共享记忆（广播式 KV），非单一媒介。
- 全内部派发：无 A2A、无 AgentCard、无外部 Agent 心跳检活。
- 安全机制：spec 强制 + 派发限额 + 重复去重 + 工具白名单 + token 预算。无多道安全闸门、无破坏性操作双签。

---

## 2. 阿里 AICP 文章编排要点

### 2.1 架构总览

- **9 层 Agent 军团**（按职责固定分层）：决策层（指挥）/ 入口层（哨兵）/ 策略层（编排）/ 执行层 / 观测层（环境）/ 认知层（诊断）/ 输出层（报告）/ 闭环层（工单）/ 知识层（产品）。
- **共享黑板**（Redis Hash）：Agent 间**唯一**交互媒介。除指挥/哨兵外，Agent 互相不知道对方存在，只读写黑板约定字段。
  - 字段四类：实验元数据 / 业务数据 / 输出反馈 / 调试审计。
  - 并发：Lua 原子写 + 乐观锁（version 字段），冲突自动重试。
  - 实时：SSE 字段变更推送，无需轮询。
  - 隔离：Redis ACL 按角色读写隔离，每个 Agent 只能写自己负责的字段前缀。
- **能力三角**：每个 Agent 配 Tool（可执行）/ Skill（领域技能）/ Knowledge（知识资产）。
- **内外分离**：内部 Agent（编排/执行/环境/报告/工单）Dispatch 直调；外部 Agent（诊断/产品）走 A2A 协议，独立部署。
- **AgentCard**：标准化能力声明 JSON（capability_id、输入输出 Schema、安全等级、超时），平台据此统一调度与发现，不硬编码调用关系。
- **Registry + 心跳检活 + 统一路由**：内部直调，外部 A2A。

### 2.2 流程与安全

- **状态机驱动**：指挥 Agent 基于状态机（9 正常 + 1 异常状态）严格驱动全流程，阶段转换伴随安全闸门校验，前端 SSE 实时展示进度。
- **四阶段**：任务发起与编排 → 预检与监控 → 注入与恢复 → 故障报告。
- **三道安全闸门**（递进式决策点）：
  1. 参数校验（环境白名单硬规则，LLM 不可绕过）。
  2. 安全预检（破坏性操作白名单 + 双签确认 + 爆炸半径评估 + 黑名单 + 并发数/失败率检查）。
  3. 恢复判定（基于基线对比确认可安全恢复，含集群连锁健康度）。
- **破坏性操作权限隔离**：唯一执行 Agent 拥有环境变更权限，其他 Agent 只能读写黑板/文件。
- **安全左移**：安全逻辑内嵌各 Agent 决策流程，非外挂审计。
- **dispatch_rules.json**：按阶段定义可调度 Agent 白名单。
- **LLM 并发限流**：防 API 429。

### 2.3 双进化回路

- **回路一（经验反馈 / 知识进化）**：诊断 Agent 输出判定 → 产品 Agent 决定用例价值写入用例库 → 下轮编排 Agent 优先从用例库检索复用，未命中才回退 LLM 模板生成。命中率高→方案生成从现场生成变为检索复用。
- **回路二（AI 飞轮 / 权重进化）**：报告 Agent 出行动建议 → 工单 Agent 创建工单并跟踪修复 → 修复结果回传编排 Agent → 编排 Agent 调整故障类型/参数组合的编排权重。低价值场景权重降、高价值场景权重升，逐轮向高发现率收敛。

### 2.4 标准化接入

- 产品接入 = 两个角色分工：产品方实现 AgentCard + 3 个 HTTP 端点（`/agent_card`、`/api/{capability_id}`、`/health`）+ 心跳；平台方做 Registry + Redis ACL + 路由白名单 + dispatch 规则 + 监控适配。
- 目标：新产品接入「像插 USB」。

---

## 3. 可借鉴的优秀点

### 3.1 块记忆加「价值反馈」字段，召回按价值排序（补回路二的轻量版）⭐ 高价值

文章回路二用修复结果反哺编排权重，让高价值场景被更频繁验证。本仓库块记忆已有召回→执行→沉淀（回路一的等价），但**沉淀时无价值标记，召回时无价值排序**——成功记忆与失败/误报记忆同等权重召回，可能把「上次这么改失败了」的记忆和「上次这么改成功了」并列注入，误导子 Agent。

**落地**：`saveBlockMemory`（dispatcher.go:1373）写 KnowledgeRecord 时 `Meta` 加 `outcome`（success/fail/partial）+ `reuse_count` 字段；`injectRecalledMemory`（dispatcher.go:1450）召回后按 `outcome=success` 优先、`reuse_count` 降序排序。失败记忆不丢，但降权或单独「避坑」段呈现。成本低，收益直接。

### 3.2 子 Agent 心跳检活，早发现卡死 ⭐ 高价值低成本

当前子 Agent 只有 30 分钟总超时（dispatcher.go:311），无心跳。卡死的子 Agent（如 LLM 端点假死、流式挂起）要等到超时才被发现，期间父 Agent 在终结保护 wait loop 里空等。

**落地**：Dispatcher 的 `running` map（dispatcher.go:89）已有子 Agent 句柄。给 ReActAgent 加最近活动时间戳（每次 `generateOnce` 更新），Dispatcher 起一个轻量巡检 goroutine，对超过 N 分钟无活动的子 Agent 主动 cancel 并 notify 父「子 Agent 疑似卡死」。比硬超时更早暴露。

### 3.3 共享记忆 slot 写入方校验（per-agent 前缀隔离）

文章用 Redis ACL 限制每个 Agent 只能写自己负责字段前缀，防越权污染。本仓库 SharedMemory 槽位是 `parentID:key`，任何子 Agent 拿到 parentID 都能写任意 key，存在兄弟 Agent 互相覆盖 `file_tree` 等共享 slot 的风险。

**落地**：写入时校验 caller Agent ID 与 slot 命名约定（如 `file_tree` 只由首个探索者写一次，后续只读）。已在 dispatcher.go:1275 有「缺 file_tree 注入提示」的启发式，加固为写入方白名单即可。低成本。

### 3.4 破坏性工具分级 + 生产环境命令用户确认（安全左移）

文章「唯一执行 Agent 有破坏性权限」+「破坏性操作双签」。本仓库叶子助手（code/test/ui）都有 `WriteFile`/`RunCommand`，是破坏性操作的分散持有——`RunCommand` 已有限只读探索预算（commit 0ecd074），但写操作无分级。

**不照搬双签**（与自主 ReAct 目标冲突，见 4.4），但可借鉴「分级」：
- 工具元数据加 `destructive` 标记（WriteFile/RunCommand 写类标记为 true）。
- 派发到生产环境工作目录（或命中危险命令模式）时，destructive 工具调用经 liveFn 推「需确认」事件，由上层暂停会话等用户确认，非生产环境照常自主。
- 比文章双签轻：只在边界触发，不阻塞常规编码。

### 3.5 固定流程可选叠加状态机（仅当 ReAct 反复失控时）

文章状态机让固定流程可控可观测。本仓库 verifyloop 已是状态机折叠进 ReAct 的范例（`verify_and_fix` 工具），证明「固定流程用状态机、其余用 ReAct」的混合模式可行。

**可推广场景**：若某类任务（如「改代码→跑测试→修失败用例」）在纯 ReAct 下反复出现步骤乱跳、漏验证，可仿 `verify_and_fix` 把该流程抽成状态机工具，由 Agent 显式调用。**不要预先铺状态机**——只在实证失控点出现时叠加。

### 3.6 AgentCard 标准化能力声明（仅当开放外部接入时）

文章 AgentCard（capability_id + I/O schema + 安全等级 + 超时）让平台统一调度外部 Agent 不硬编码。本仓库 `RoleDefinition` 是内部角色定义，无标准化 I/O schema 暴露。

**不急于 adopt**：当前无外部 Agent 接入需求。**若未来要接产品方/第三方 Agent**，AgentCard 是合理的接入契约——可让 `RoleDefinition` 扩展 AgentCard 字段，外部 Agent 走 HTTP 端点适配。属按需项，不超前做。

---

## 4. 不建议照搬的缺点 / 不适用点

### 4.1 9 层固定分层是过度设计，不适用通用编码/任务场景

9 层（决策/入口/策略/执行/观测/认知/输出/闭环/知识）是混沌工程固定阶段（注入→观测→诊断→报告→工单）的镜像。本仓库面对开放式编码与多类任务，**固定分层会僵化**：不是每个任务都需要诊断层、工单层。

本仓库的 `call_sub_agent` 递归（meta/domain/leaf）让 LLM 按任务复杂度自决深度，更灵活。DomainAgent 默认自执行、仅自主判断需要时才下拆叶子（2026-08-22 任务 83 起两层编排，路由规则见 dispatcher.go Description），避免固定层的空转。**保留递归，不引入固定 9 层。**

### 4.2 纯状态机驱动刚性，不适合开放任务

文章状态机严格定义阶段转换，对混沌实验（流程固定）合适，对开放编码任务（步骤不可预判）会束缚。本仓库 ReAct 让 LLM 按观察动态决策下一步，是开放任务的正确选择。verifyloop 折叠为工具（3.5）已是状态机的正确用法——**作为工具被显式调用，而非替代主循环**。

### 4.3 黑板唯一媒介致 Agent 互不感知，削弱紧耦合协作

文章黑板设计让 Agent「互相不知道对方存在」，解耦极致但代价是：**无法做定向提问与多轮对话**。本仓库代码↔测试验证闭环依赖 `send_message` 的请求/回复语义（dispatcher.go:450），测试 Agent 能直接向代码 Agent 提具体问题并收回复——黑板模式做不到（只能各自读写字段，无法 request/reply）。

本仓库的 mailbox（点对点）+ 共享记忆（广播 KV）是更丰富的双媒介组合。**不替换为纯黑板。** 可借鉴黑板的结构化字段与 version 乐观锁（若未来共享记忆字段增多、并发写冲突显现），但不放弃 mailbox。

### 4.4 破坏性操作双签与自主 ReAct 目标冲突

文章双签要求人确认破坏性操作，对混沌实验（研发环境故意制造故障，风险高）合理。本仓库目标是自主完成编码任务，双签会打断自动化流。**用 3.4 的分级 + 边界确认替代**，不全局双签。

### 4.5 三道安全闸门是混沌工程特有，编码场景过重

参数校验/安全预检/恢复判定（爆炸半径、集群连锁健康度）针对「在运行系统上制造故障」。编码 Agent 的工作目录是文件系统，无「爆炸半径」「集群恢复」概念。本仓库的 spec 强制 + 派发限额 + 重复去重 + verify_and_fix 已是编码场景的等价安全机制。**不引入三道闸门。**

### 4.6 AI 飞轮权重进化需大量样本，编码任务样本稀疏难收敛

回路二靠大量实验调整权重向高发现率收敛。混沌实验可高频规模化跑（文章提数十倍提效、日级迭代）。编码任务样本稀疏（每个任务差异大、难重复），权重难收敛，投入产出比低。

**回路一（记忆召回复用）更适用**——本仓库块记忆已是此模式。回路二只取其「价值反馈」思想做轻量版（3.1 的 outcome 标记 + 召回排序），不做完整权重进化。

### 4.7 外部 A2A 增加延迟与复杂度，单部署场景不值

A2A 协议、Registry 心跳、外部 Agent 独立部署，是为跨团队协作（产品方独立迭代自己的 Agent）。本仓库单后端部署，所有 Agent 同进程 Dispatch 直调，延迟低、可控。**无跨团队接入需求前不引入 A2A。**

### 4.8 「AI Native 三标准 / 10 倍提效」是营销口号非架构

「全链路 AI 驱动 / 标准化协议 / 10 倍效率」是验收叙事，非可落地的架构组件。跳过。

---

## 5. 落地建议（按优先级）

| 优先级 | 项 | 改动点 | 估算 |
|---|---|---|---|
| **P0** | 块记忆加 `outcome`/`reuse_count`，召回按价值排序（3.1） | `dispatcher.go:1373` saveBlockMemory 写字段、`:1450` injectRecalledMemory 排序 | 小 |
| **P0** | 子 Agent 心跳检活，早发现卡死（3.2） | ReActAgent 加活动时间戳，Dispatcher 巡检 goroutine | 中 |
| **P1** | 共享记忆 slot 写入方校验（3.3） | SharedMemory 写入加 caller 校验，file_tree 单写多读 | 小 |
| **P1** | 破坏性工具分级 + 生产环境命令用户确认（3.4） | 工具元数据加 destructive 标记，liveFn 推确认事件 | 中 |
| **P2** | 固定流程状态机工具化（3.5） | 仅在实证失控流程出现时仿 verify_and_fix 抽工具 | 按需 |
| **P2** | AgentCard + A2A（3.6） | 仅当开放外部 Agent 接入时，RoleDefinition 扩展 AgentCard | 按需 |

**不 adopt**：9 层固定分层（4.1）、纯状态机主循环（4.2）、黑板替代 mailbox（4.3）、全局双签（4.4）、三道安全闸门（4.5）、完整权重飞轮（4.6）、无外部需求时的 A2A（4.7）。

---

## 6. 一句话结论

AICP 的「固定分层 + 黑板 + 状态机 + 三闸门」是为高频可重复的混沌实验量身定做；本仓库的「递归 ReAct + Agent 树 + mailbox + 共享记忆 + 块记忆」是为开放式编码任务量身定做。**值得偷的是回路二的价值反馈思想（轻量版）、心跳检活、slot 写入隔离、破坏性工具分级——都是增量加固，不动主架构。不值得偷的是固定分层与刚性状态机，那会牺牲本仓库对开放任务的灵活性。**
