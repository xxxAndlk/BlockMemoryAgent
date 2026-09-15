// Package enums 集中定义项目所有结构体字段使用的枚举类型。
//
// 设计意图:
//   - 把散落在 types / server / graph 等包中的字符串字面量与 iota 常量统一收敛到一处,
//     作为单一可信源 (single source of truth),避免同一语义在不同包内出现多种写法
//     (如 "running" vs "Running" vs "RUNNING")。
//   - 每个枚举值都附详细中文注释,说明语义、触发场景与流转方向,提升可读性。
//   - 类型全部以 typed string 形式定义,既保留 JSON 序列化的可读性,又获得编译期类型检查。
//
// 使用约定:
//  1. struct 字段不再使用裸 string,而是引用本包的具体枚举类型 (如 SessionStatus / ChatRole)。
//  2. 赋值时使用枚举常量,不要硬编码字符串字面量。
//  3. 与外部系统 (HTTP / DB / YAML) 交互时,枚举值的字符串形式即协议字面量,
//     修改常量值等同于破坏协议,需同步迁移存量数据。
package enums

// ==================== 章节分隔 ====================
// 1. 工作区事件
// ==================== 章节分隔 ====================

// EventType 工作区事件类型:刻画 Agent 间协作事件的语义分类。
// 由 Mailbox 分发,消费方按 Type 走不同处理分支。
type EventType string

const (
	// EventCrossModify 跨 Agent 修改事件:某 Agent 改动了他人负责的领域数据。
	// 触发场景:DomainAgent 写入了不属于自己领域的文件 / 状态。
	// 处理:MetaAgent 介入仲裁,可能回滚或升级。
	EventCrossModify EventType = "CrossModify"

	// EventDependencyMet 依赖满足事件:被等待的前置产物已就绪。
	// 触发场景:某 Agent 发布新版本输出后,通知等待该产物的下游 Agent。
	EventDependencyMet EventType = "DependencyMet"

	// EventEscalation 升级事件:将问题 / 上下文上抛给 MetaAgent 裁决。
	// 触发场景:DomainAgent 自身无法解决,需要更高层决策。
	// 注意:与 ActionEscalate 区分 — ActionEscalate 是 Graph 控制信号,
	// EventEscalation 是 Mailbox 中流转的事件。
	EventEscalation EventType = "Escalation"
)

// EventStatus 事件状态:事件在生命周期中的处理阶段。
// 流转:Pending → Processing → Done。
type EventStatus string

const (
	// EventPending 待处理:事件已入队但尚未被消费。
	// 初始状态,Mailbox 接收事件后立即置此状态。
	EventPending EventStatus = "Pending"

	// EventProcessing 处理中:目标 Agent 已接收但未完成。
	// 中间态,用于异常恢复时识别未完成的事件。
	EventProcessing EventStatus = "Processing"

	// EventDone 已完成:事件处理结束,可归档。
	// 终态,Mailbox 不再投递此事件。
	EventDone EventStatus = "Done"
)

// ==================== 章节分隔 ====================
// 3. 记忆压缩层级
// ==================== 章节分隔 ====================

// CompressionLevel 记忆压缩层级：按设计文档简化为两级。
// 见 internal/memory/compress.go。
// 层级关系：Raw（完整记录） > Standard（摘要）。
// 使用 int 类型并通过 iota 递增，便于按数值比较压缩程度。
type CompressionLevel int

const (
	// LevelRaw 完整原始记录：保留 FullObservation 等全部字段。
	// 用于近期高频访问的 Episode，保留完整上下文供回放。
	LevelRaw CompressionLevel = iota

	// LevelStandard 标准级：保留摘要，丢弃 FullObservation。
	// Episode 进入长期记忆后的默认层级，平衡可读性与成本。
	LevelStandard
)

// ==================== 章节分隔 ====================
// 4. 角色体系
// ==================== 章节分隔 ====================

// RoleType 角色类型:四层 Agent 体系中的角色分类,决定实例化路径与生命周期管理。
type RoleType string

const (
	// RoleTypeMeta 主 Agent / MetaAgent:顶层调度者,负责会话级任务分解与升级裁决。
	// 全局唯一,每个会话一个 MetaAgent 实例。
	RoleTypeMeta RoleType = "meta"

	// RoleTypeDomain 会话块 Agent / DomainAgent:负责单一领域的子任务执行与会话块管理。
	// 由 MetaAgent 按领域动态创建,会话内可存在多个。
	RoleTypeDomain RoleType = "domain"

	// RoleTypeSubDomain 子领域 Agent / SubDomainAgent:DomainAgent 进一步拆分的子领域执行者。
	// 第三层,处理 DomainAgent 内部复杂子任务。
	RoleTypeSubDomain RoleType = "subdomain"

	// RoleTypeFixed 固定助手角色:由配置文件 (roles.yaml) 预定义,长期可复用。
	// 跨会话存在,如 ReadFileAssistant / WriteFileAssistant 等工具型助手。
	RoleTypeFixed RoleType = "fixed"

	// RoleTypeDynamic 动态助手角色:由 LLM 在运行时按需创建,随任务结束消亡。
	// DomainAgent 根据子任务特征让 LLM 决定创建何种助手。
	RoleTypeDynamic RoleType = "dynamic"
)

// RoleStatus 角色状态:实例在运行时状态机中的当前阶段。
// 流转:Idle → Active → (Waiting | Calling) → Done | Error。
type RoleStatus string

const (
	// RoleStatusIdle 空闲:已创建但未开始执行。
	// 初始状态,实例化后默认置此状态,等待被调度。
	RoleStatusIdle RoleStatus = "idle"

	// RoleStatusActive 活跃:正在执行任务。
	// 运行中状态,LLM 调用 / 工具执行时置此状态。
	RoleStatusActive RoleStatus = "active"

	// RoleStatusWaiting 等待:阻塞等待依赖 / 外部事件。
	// 如等待其他 Agent 输出、等待人机对话答复。
	RoleStatusWaiting RoleStatus = "waiting"

	// RoleStatusCalling 调用中:正在调用下层助手角色。
	// DomainAgent 调用 SubDomainAgent / Assistant 时置此状态。
	RoleStatusCalling RoleStatus = "calling"

	// RoleStatusDone 完成:任务已成功结束。
	// 终态,实例不再参与调度,可被归档。
	RoleStatusDone RoleStatus = "done"

	// RoleStatusError 错误:执行失败,需升级或重试。
	// 终态,触发升级/上抛给更高层 Agent 处理。
	RoleStatusError RoleStatus = "error"

	// RoleStatusUnverified 未验证:产出已交付但缺机器可执行验证证据（TODO #60 三态化）。
	// 终态,非失败语义——展示标黄不标红,由父 Agent 决定补验证或收口。
	RoleStatusUnverified RoleStatus = "unverified"
)

// ==================== 章节分隔 ====================
// 5. 会话与对话
// ==================== 章节分隔 ====================

// SessionStatus 会话状态:Session 在生命周期中的当前阶段。
// 见 internal/server/session.go。
// 流转:running → (awaiting_clarify → running)* → completed | error。
type SessionStatus string

const (
	// SessionStatusRunning 运行中:Graph 循环正在执行。
	// 初始状态,会话创建后立即进入。
	SessionStatusRunning SessionStatus = "running"

	// SessionStatusCompleted 已完成:Graph 正常结束,所有子任务成功。
	// 终态,前端可展示结果。
	SessionStatusCompleted SessionStatus = "completed"

	// SessionStatusError 错误:Graph 执行失败或超时。
	// 终态,前端展示错误信息。
	SessionStatusError SessionStatus = "error"

	// SessionStatusAwaitingClarify 等待澄清:会话因等待用户答复而挂起。
	// 中间态,用户通过 /clarify 提交答复后回到 running。
	SessionStatusAwaitingClarify SessionStatus = "awaiting_clarify"

	// SessionStatusPausedOnChild 暂停于子 Agent:某 DomainAgent 触达 token 上限进入 Paused,
	// MetaAgent 检测到后主动暂停会话。用户发任意消息(如"继续")将优先恢复该 Paused DomainAgent。
	// 与 AwaitingClarify 区分:后者等澄清答复,前者等恢复暂停的子 Agent。
	SessionStatusPausedOnChild SessionStatus = "paused_on_child"

	// SessionStatusAwaitingChild 挂起等子:任务已全部派发,Meta 挂起等待子 Agent 回传。
	// 子 Agent 完成(trackChildDone 回调)自动唤醒续跑;用户也可直接发新消息唤醒。
	// 与 PausedOnChild 区分:后者是子 domain 触限暂停需用户续跑,前者是正常等回传。
	SessionStatusAwaitingChild SessionStatus = "awaiting_child"
)

// ChatRole 对话角色:ChatMessage 的角色分类,与 OpenAI Chat Completion 协议对齐。
// 修改值等同于破坏 LLM API 协议。
type ChatRole string

const (
	// ChatRoleSystem 系统消息:注入 LLM 上下文的系统指令 (soul.md / 角色定义)。
	// 不直接展示给用户,作为 LLM 行为约束。
	ChatRoleSystem ChatRole = "system"

	// ChatRoleUser 用户消息:来自用户的输入。
	// 触发 Agent 执行,作为任务目标来源。
	ChatRoleUser ChatRole = "user"

	// ChatRoleAssistant 助手消息:Agent 的回复。
	// 作为 Agent 输出记录到对话历史,供后续轮次引用。
	ChatRoleAssistant ChatRole = "assistant"
)

// ==================== 章节分隔 ====================
// 6. 话题状态
// ==================== 章节分隔 ====================

// TopicStatus 话题状态:TopicMeta.Status 的取值,描述话题生命周期。
// 见 internal/memory/topic.go。
// 流转:active → done → archived (可跳过 done 直接 archive)。
type TopicStatus string

const (
	// TopicStatusActive 活跃:话题正在被 Agent 处理。
	// 初始状态,话题创建后默认置此状态。
	TopicStatusActive TopicStatus = "active"

	// TopicStatusDone 完成:话题目标已达成,Agent 不再主动推进。
	// 仍可被检索,但不再参与调度。
	TopicStatusDone TopicStatus = "done"

	// TopicStatusArchived 已归档:话题长期未访问,转入冷存储。
	// 默认检索不返回,需显式查询归档话题。
	TopicStatusArchived TopicStatus = "archived"
)

// ==================== 章节分隔 ====================
// 9. 知识库类型
// ==================== 章节分隔 ====================

// KnowledgeType 知识库类型:KnowledgeRecord.KnowledgeType 的取值,
// 用于在 global_knowledge 表中按类型分类检索。
// 见 internal/store/postgres.go SearchKnowledgeByType。
// 修改值等同于破坏 DB 协议,需同步迁移存量数据。
type KnowledgeType string

const (
	// KnowledgeTypePlaybook 运维手册:AIOps 场景下沉淀的标准操作流程。
	// 检索命中后作为 Agent 处理类似告警的参考步骤。
	KnowledgeTypePlaybook KnowledgeType = "playbook"

	// KnowledgeTypePostmortem 事故复盘:故障事后总结,记录根因与改进措施。
	// 供未来类似故障参考,避免重复踩坑。
	KnowledgeTypePostmortem KnowledgeType = "postmortem"

	// KnowledgeTypeRule 业务规则:领域专家输入的硬性约束与规则。
	// 作为 Agent 决策的不可违反前提。
	KnowledgeTypeRule KnowledgeType = "rule"

	// KnowledgeTypeBlockMemory 块记忆:DomainAgent 完成后归档的领域级摘要。
	// 见 internal/memory/block_vector.go,跨会话复用 DomainAgent 经验。
	KnowledgeTypeBlockMemory KnowledgeType = "block_memory"

	// KnowledgeTypeExternalKB 外部知识库:预置领域文档（TODO #27 外部知识库检索层）。
	// 只读为主、与块记忆（block_memory）分层：摄入管道切块落库（meta.namespace=external），
	// search_knowledge 工具 / 混合检索按此类型召回。
	KnowledgeTypeExternalKB KnowledgeType = "external_kb"

	// KnowledgeTypeDomainProfile 领域档案（TODO #17 领域注册表）：按 meta->>'domain'
	// 唯一 upsert 的跨场景领域注册条目，记录展示名/别名/常改文件清单/子项目归属，
	// 供派发侧做「任务路径 ∩ 档案文件」匹配与冷复活种子注入。零新表，复用 global_knowledge。
	KnowledgeTypeDomainProfile KnowledgeType = "domain_profile"

	// KnowledgeTypeDomainArchive 领域归档:历史遗留知识类型，召回机制已在 P1-1 删除
	// （原 internal/store/domain_archive.go 已移除）。常量保留以兼容历史数据，
	// 新代码不应再写入此类型，统一使用 KnowledgeTypeBlockMemory。
	KnowledgeTypeDomainArchive KnowledgeType = "domain_archive"
)
