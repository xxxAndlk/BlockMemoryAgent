package subagent

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于控制子 Agent 的生命周期与超时
	"encoding/json" // encoding/json 解析 SharedEntry 做 stat 校验
	"errors"        // errors 提供哨兵错误 errLimitReached 与 errors.Is 判定
	"fmt"           // fmt 用于格式化子 Agent ID 与错误信息
	"log"           // log 用于记录块记忆写入失败等不影响主流程的错误
	"log/slog"      // slog 用于块记忆连续失败阈值告警（单条 log 在长任务中被淹没）
	"os"            // os 用于 stat 文件 mtime 校验（Layer 3 缓存一致性）
	"sort"          // sort 用于召回结果按 outcome/reuse_count 价值排序
	"strings"       // strings 用于从 Agent ID 中提取角色 ID
	"sync"          // sync 提供 sync.Map 存储运行中的子 Agent
	"sync/atomic"   // sync/atomic 提供原子递增序列号
	"time"          // time 用于设置子 Agent 独立超时
	"unicode/utf8"  // unicode/utf8 用于 RuneCountInString 统计 task 字符数

	"github.com/blockmemory/agent/backend/internal/agent"               // agent 包提供 ReActAgent、MemoryPipeline、ModelProvider 等类型
	"github.com/blockmemory/agent/backend/internal/board" // board 提供任务看板（TODO #22 执行计划）
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // orchestrator 提供 Agent 树元数据层
	"github.com/blockmemory/agent/backend/internal/domain/role"         // role 包提供角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool"         // tool 包提供工具注册表与 Result 类型
	"github.com/blockmemory/agent/backend/internal/logger"              // logger 包提供会话级日志器，记录子 Agent LLM I/O
	"github.com/blockmemory/agent/backend/internal/mailbox"             // mailbox 包用于子 Agent 向父 Agent 发送完成通知
	"github.com/blockmemory/agent/backend/pkg/enums"                    // enums 包提供 KnowledgeTypeBlockMemory 等枚举常量
	"github.com/blockmemory/agent/backend/pkg/textutil"                 // textutil 提供截断展示名用工具
	"github.com/blockmemory/agent/backend/pkg/types"                    // types 包提供 RoleDefinition 类型
)

// 子 Agent 的上下文与父会话故意隔离（context.Background 派生）：
//   - 父会话若被用户手动取消，子 Agent 仍可在独立上下文中继续运行，避免长任务结果丢失。
//   - 超时仅用于防止无限制挂起，由 WithTimeout 配置；<=0 表示不限制。

// callSubAgentInput 定义 call_sub_agent 工具的 JSON 入参结构。
// 大模型在调用 call_sub_agent 时应提供 role_id（被调用角色）与 task（任务描述），
// 可选 domain（领域分类简称，仅 role_id="domain" 时有效，用于子 Agent 展示名）。
type callSubAgentInput struct {
	RoleID string `json:"role_id"` // RoleID 被调用子 Agent 的角色标识。
	Task   string `json:"task"`    // Task 交给子 Agent 执行的具体任务描述。
	Domain string `json:"domain"`  // Domain 领域分类简称（金融/认证/UI 等），仅 role_id="domain" 时有效。
	// Responsibility 职责边界描述（仅 role_id="domain" 时有效），注入子 Agent 系统提示词，
	// 防止长 ReAct 循环中 task 被历史压缩后领域身份丢失。
	Responsibility string `json:"responsibility"`
	// Mode 派发执行模式（TODO #29）：react（默认）/ reflection / plan_execute。
	// 空串按 react 处理（零行为变化）。
	Mode string `json:"mode"`
	// VerifyKind 校验分层（TODO #43）：auto（默认，按角色/模式自动选）/ executable（L0 证据）/
	// rubric（L2 交叉模型 judge）/ none。空串按 auto 处理。
	VerifyKind string `json:"verify_kind"`
	// ToolsHint 建议工具集（TODO #52 执行项 4）：父 Agent 声明希望子 Agent 使用的工具名列表，
	// dispatcher 校验 ∩ 子 Agent 角色权限天花板后收窄其插件工具可见集（相当于预挂载）。
	// 天花板外（插件 roles 白名单不允许）的越界项被忽略并随派发结果回告父 Agent，不放大权限。
	ToolsHint []string `json:"tools_hint"`
	// WallClockMin 派发级墙钟预算（分钟，可选）。代码级 context.WithTimeout 强制收口，
	// 替代提示词墙钟（roles.yaml 的"墙钟约 15 分钟"对 LLM 只是软约束，实证验收 Agent
	// 拿 15 分钟预算实际跑了 39 分钟）。>0 时取 min(本值, sub_agent_timeout)；到期前
	// 预警窗口内向子 Agent 邮箱投递收口警告。省略=用全局 sub_agent_timeout。
	WallClockMin float64 `json:"wall_clock_min"`
}

// ModelProviderFactory 是 model.ModelFactory 的子集，
// Dispatcher 只需要从中获取指定角色对应的模型提供者即可创建子 Agent。
type ModelProviderFactory interface {
	// GetBladesProvider 根据 roleID 返回对应的模型提供者实例。
	GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error)
}

// BlockMemorySearcher 抽象块记忆（block_memory）的语义检索能力，
// 由 store.PostgresStore 实现；为 nil 时跳过召回，不影响子 Agent 派发。
type BlockMemorySearcher interface {
	// SearchBlockMemoryByGoal 按目标文本做向量语义匹配，返回最相关的 topK 条块记忆。
	// sessionID 非空时仅召回该 session 写入的记录，避免跨 session 污染（实证：旧 session
	// 的"重写全部 JS"任务文本被召回，污染新 session 的 HTML+CSS 任务上下文）。
	SearchBlockMemoryByGoal(ctx context.Context, sessionID, goal string, topK int) ([]*types.KnowledgeRecord, error)
}

// BlackboardSearcher 抽象黑板模式（TODO #42）的 scope 确定性检索能力，由 store.PostgresStore 实现。
// 在 BlockMemorySearcher 语义召回之上叠加 parent_id + task_domain 精确过滤：兄弟产出按 scope
// 共享，每个 Agent 只取自己 scope 的切片（替 mailbox 全量广播 / 父注入全量 spec 的上下文互染）。
//
// query 非空：叠加向量语义排序（cosine 阈值过滤 + 距离排序）；
// 空串：跳过 cosine（纯 scope 过滤 + created_at DESC），省 embedding，供每轮摄取等无需语义重排场景。
// excludeSubAgentID 非空时排除自身产出（每轮摄取不回显自己刚写的结论）。
type BlackboardSearcher interface {
	Query(ctx context.Context, sessionID, parentID, taskDomain, query string, topK int, excludeSubAgentID string) ([]*types.KnowledgeRecord, error)
}

// CrossSessionSearcher 抽象跨 session 块记忆补位检索能力，由 store.PostgresStore 实现。
// session 内召回不足 topK 时补位历史任务沉淀（"外脑"）：块记忆是全局沉淀，任何会话
// 只要相关度足够高即可召回（纯语义过滤，不做项目隔离）；仅排除当前 session。
// 实现方自持更严的相似度阈值（crossSessionBlockMemoryScore）。
// 未实现时跳过补位（向后兼容 mock/测试）。
type CrossSessionSearcher interface {
	SearchBlockMemoryCrossSession(ctx context.Context, excludeSessionID, goal string, topK int) ([]*types.KnowledgeRecord, error)
}

// blockMemoryRecallTopK 是派发子 Agent 时召回块记忆的条数上限。
const blockMemoryRecallTopK = 3

// BlockMemorySaver 抽象块记忆（block_memory）的写入能力，
// 由 bootstrap 装配的 store.PostgresStore 适配器实现（负责补齐 embedding 后落库）；
// 为 nil 或写入开关关闭时跳过沉淀，不影响子 Agent 派发主流程。
type BlockMemorySaver interface {
	// Save 写入一条块记忆知识记录；实现方负责生成 embedding 并持久化。
	Save(ctx context.Context, rec *types.KnowledgeRecord) error
}

const (
	// blockMemoryGoalMaxRunes 是写入块记忆时目标（拆分任务）文本的最大 rune 数。
	blockMemoryGoalMaxRunes = 200
	// blockMemoryResultMaxRunes 是写入块记忆时子 Agent 结果摘要的最大 rune 数。
	blockMemoryResultMaxRunes = 500
	// blockSaveFailAlertThreshold 是块记忆连续写入失败触发醒目告警的阈值。
	// 单条失败 log 在长任务中被淹没；连续失败达阈值说明链路级损坏（embedding
	// 端点 404/表缺失），应显式提示运维，而非继续静默降级。
	blockSaveFailAlertThreshold = 10
)

// 块记忆 outcome 取值：沉淀结果的执行状态，召回排序按 success 优先。
const (
	blockOutcomeSuccess = "success"
	blockOutcomePartial = "partial"
	blockOutcomeFail    = "fail"
)

// reuseBumper 是 BlockMemorySearcher 的可选扩展接口：召回命中后递增 reuse_count。
// 实现方（store.PostgresStore）用 jsonb_set 就地更新 Meta 字段；
// 未实现时跳过递增，不影响召回（best-effort）。
type reuseBumper interface {
	BumpReuse(ctx context.Context, id int64) error
}

// Dispatcher 负责创建并跟踪异步运行的子 Agent。
// 它会将 call_sub_agent 工具注册到 domain/tool 注册表中，
// 这样任何 ReActAgent 都可以在运行过程中动态生成子 Agent。
type Dispatcher struct {
	registry *role.Registry       // registry 角色注册表，用于校验角色与权限。
	models   ModelProviderFactory // models 模型工厂，用于为子 Agent 获取模型提供者。
	tools    *tool.Registry       // tools 工具注册表，子 Agent 与父 Agent 共用同一套工具。
	mailbox  *mailbox.Mailbox     // mailbox 邮箱，用于子 Agent 向父 Agent 发送完成通知。
	memory   agent.MemoryPipeline // memory 可选的记忆管道，为 nil 时内部会使用空实现。
	seq      atomic.Uint64        // seq 原子递增序列号，保证生成的子 Agent ID 唯一。
	running  sync.Map             // running 存储正在运行的子 Agent，键为 subAgentID，值为 *agent.ReActAgent。

	// pluginVisibility 热插拔插件角色可见性回调（设计文档 §4.3）：
	// fn(roleID, toolName) -> (owned, visible)；nil 时插件工具不额外过滤。
	// 由 bootstrap 注入 plugins.Manager.ToolVisibility。
	pluginVisibility agent.ToolVisibilityFunc

	// pending 跟踪每个父 Agent 当前未完成的子 Agent 数量，键为 parentID，
	// 值为 *pendingState。用于父会话终结保护：父 Agent 给出终答前若有未决子 Agent，
	// 应等待其完成再终结，防止迟到 mailbox 消息丢失（参见 agent.ReActAgent 的终结保护分支）。
	pending sync.Map

	// sessionCounts 跟踪每个 session 的累计派发总数（所有角色合计），用于全局派发限额。
	// 键为 sessionID（parentID 首段），值为 *atomic.Int64。用户发送新消息时重置。
	sessionCounts sync.Map

	// sharedMem 是共享记忆的只读视图（tool.SharedMemoryStore 接口的子集），
	// 供子 Agent 派发时读取主 Agent 写入的关键上下文与任务规范。
	// 为 nil 时关闭共享记忆注入，不影响派发主流程。
	// 写入由主线程 Agent 直接通过 WriteSharedMemory/WriteSpec 工具完成，不经 Dispatcher。
	// 同一后端承载两类槽位：自由槽位（WriteSharedMemory）与固定 spec 槽位（WriteSpec）。
	sharedMem tool.SharedMemoryStore

	// specEnforcementEnabled 派发方调用 call_sub_agent 前是否强制先写 WriteSpec。
	// 为 true 时 Execute 入口校验 parentID:spec 存在且新鲜（Spec.Goal 非空 + 至少一条 Acceptance），
	// 缺失则拒绝派发，返回 "先调 WriteSpec 再 call_sub_agent"。
	// 为 false 时跳过强制，injectSpec 仍生效（graceful degrade，spec 缺失则无前缀注入）。
	specEnforcementEnabled bool

	// maxTotalDispatches 全局派发总数上限：同一 session 内所有角色的派发合计超过该值时
	// 拒绝进一步派发，防止编排失控。<=0 表示不限制。计数随用户新消息重置。
	maxTotalDispatches int

	// timeout 是子 Agent 独立执行的最大时长；<=0 表示不限制。默认 30 分钟。
	timeout time.Duration
	// domainReconClock 是 DomainAgent 派发无显式 wall_clock_min 时的默认墙钟（分钟）
	//（2026-08-21 慢任务根因修复：domain 侦察阶段失控——炮塔领域 Agent 1.5h 零交付，
	// 全程"契约反推"侦察 15+ 轮慢思考从未进入派发/写入）。取 min(recon, timeout)。
	// 中点投递"停止侦察开始产出"预警邮件。<=0 关闭（用全局 timeout）。
	domainReconClock time.Duration
	// taskRuneSoftLimit / taskRuneHardLimit 是派发 task 文本长度双档上限（TODO #35 放开预算）。
	// 超软上限但未达硬上限：放行并附压缩警告（软着陆）；超硬上限：拒绝（全量规格转贴区间）。
	// 默认 3000/4000（原 2000/2600 实证过紧，强模型吃大上下文后转贴代价低），bootstrap 按配置覆盖。
	taskRuneSoftLimit int
	taskRuneHardLimit int
	// loopCfgByRole 按角色返回 ReAct 主循环配置:不同角色 token 预算分级
	// (config.yaml token_budget_per_role，resume 重置，超限暂停可恢复)。
	// 为 nil 时用 agent.NopLoopConfig 兜底(测试场景)。
	loopCfgByRole func(string) agent.LoopConfig
	// searcher 可选的块记忆检索器；为 nil 时跳过拆分任务的块记忆召回。
	searcher BlockMemorySearcher
	// saver 可选的块记忆写入器；为 nil 或 writeEnabled 为 false 时跳过子 Agent 结果沉淀。
	saver BlockMemorySaver
	// writeEnabled 块记忆写入开关，由配置（agent.block_memory_write_enabled）注入。
	writeEnabled bool

	// log 是会话级日志器，用于记录子 Agent LLM I/O（完整 prompt/response）到 session_logs。
	// 为 nil 时子 Agent 不写 LLM I/O 日志，不影响派发主流程。
	log *logger.Logger

	// liveFn 是子 Agent 实时事件转发器：把子 Agent 的 LiveEvent（token 用量/流式增量/工具事件）
	// 按 sessionID 路由回所属会话的 service.handleLiveEvent，使子 Agent token 也计入会话累计。
	// 为 nil 时子 Agent 不推送实时事件（不影响主流程）。
	liveFn func(sessionID string, ev agent.LiveEvent)

	// treeFn 按 sessionID 取得权威 Agent 树（lazy init）。
	// 派发前 Register 节点 + SetCancel 绑定 cancel func，完成时 Finish。
	// 为 nil 时关闭树跟踪（测试场景），不影响派发主流程。
	treeFn func(sessionID string) *orchestrator.Tree

	// boardFn 按 sessionID 取得会话任务看板（TODO #22 执行计划）。
	// 为 nil 时关闭计划功能（依赖门/回写/写计划工具均零行为变化）。
	boardFn func(sessionID string) *board.TaskBoard
	// boardFnCreate 按 sessionID 取或创建看板（write_plan 工具用）。
	// 为 nil 时 write_plan 返回 board not available。
	boardFnCreate func(sessionID, goal string) *board.TaskBoard

	// msgStore 持久化 Paused DomainAgent 的完整 ReAct 消息历史。
	// DomainAgent 触达 token 上限时 SaveMessages 落库,resume 时 LoadMessages 重建上下文。
	// 为 nil 时跳过持久化(测试场景:domain 到限仍返 errPaused 但 history 不存,无法 resume)。
	msgStore agent.MessagesStore

	// pausedResumes 跟踪每个 Paused domain 节点已续跑的次数（nodeID -> *atomic.Int64）。
	// ResumePaused 入口按 maxPausedResumes 校验，触顶后强制收口部分返回——
	// 续跑重置 fresh budget 使 token 上限永不绑定（v10 实证：验收领域暂停-续跑研磨
	// 30 分钟不收敛），绑定点必须落在续跑层。
	pausedResumes sync.Map
	// maxPausedResumes 同一 Paused domain 允许的最大续跑次数；<=0 时按默认值 1。
	maxPausedResumes int

	// dispatchRetryCount 叶子助手 kind=error 失败的自动重派次数（TODO #23，最小一档）。
	// 同任务同前缀重跑一次；domain/timeout/killed/loop_guard 不自动重试（交 MetaAgent 决策）。
	// 与 LLM 调用层重试（react_agent retry_count）正交：那层重试的是模型调用本身。
	dispatchRetryCount int

	// reflectionMaxRounds 派发 mode=reflection 时自检不达标重试轮数上限（TODO #29）。
	// <=0 时引擎内部按默认 2 兜底；bootstrap 从 config.ReflectionMaxRounds 注入。
	reflectionMaxRounds int
	// planMaxSteps 派发 mode=plan_execute 时最大执行步数（TODO #29）。
	// <=0 时引擎内部按默认 8 兜底；bootstrap 从 config.PlanExecuteMaxSteps 注入。
	planMaxSteps int
	// judgeRole 校验 judge 的角色 ID（TODO #43 交叉模型）：engineLLMForJudge 优先取该角色的
	// provider（与被审角色不同模型），取不到回退同角色。空串=仅同角色回退。
	judgeRole string
	// engineLLMTimeout 引擎辅助 LLM（reflection 自检 judge / plan_execute 规划）单次调用超时。
	// 该路径不走 ReAct 主循环的 CallLLM 超时包装，只吃 SDK 默认 600s/请求；无独立超时时
	// provider 层 3 次重试 × judge 内部重试叠加可烧 ~70 分钟直到 sub_agent_timeout 强杀
	//（2026-08-19 引擎 Agent 事故：文件 15:54 已全部落盘，收尾自检被流式超时循环拖到 17:03）。
	// 超时包住整次调用（含 provider 内部重试）。<=0 仅受子 Agent 墙钟控制。
	engineLLMTimeout time.Duration

	// factExtractor 从子 Agent 输出中提取关键事实，替代原始 result.Text 直接落库。
	// 为 nil 时回退到原始文本保存（测试场景或未配置时）；bootstrap 在启用块记忆写入时注入。
	// 提取失败（LLM 出错或返回空）自动回退原始保存，保证不丢结果。
	factExtractor FactExtractor

	// blockSaveFailures 连续块记忆写入失败计数（观测）：达阈值触发醒目告警，
	// 提示链路损坏（embedding 端点/DB 表缺失）。成功写入时重置为 0。
	// 事故实证：embedding URL 配置错误导致 08-11 起全部写入静默失败两天无感知。
	blockSaveFailures atomic.Int64

	// salvageExtractor 从失败子 Agent 输出中提取打捞摘要（已读文件清单/已得结论/卡点）。
	// 为 nil 时回退末条 assistant 文本截断；bootstrap 注入轻量模型实现（prompt 与事实提取不同）。
	// salvageExtractor 从失败子 Agent 输出中提取打捞摘要（已读文件清单/已得结论/卡点）。
	// TODO #20 第二层：注入后失败路径尝试 LLM 提取，失败回退文本截断。
	salvageExtractor SalvageExtractor
	// salvageTimeout 打捞轻量调用的超时：思考型模型（glm/deepseek 推理系）首 token 就要数十秒，
	// 旧 5s 硬编码致打捞提取全超时降级（TODO #33 事故链）；默认 30s，配置下限 60s 供思考型场景。
	salvageTimeout time.Duration
	// softStops 记录处于"软停止中"的 sessionID（TODO #37）：ReactService.Stop 先标记再
	// 触发子 Agent cancel；dispatcher 的 context.Canceled 收尾分支据此分流——
	// domain 落 Paused（存 history 可续跑）、叶子部分回灌，而非静默跳过。
	softStops   map[string]bool
	softStopMu  sync.Mutex

	// heartbeatTimeout 子 Agent 心跳超时：叶子 Agent 超过该时长无活动（generateOnce/工具派发）
	// 判定假死（LLM 流式挂起/工具 hang），巡检 goroutine 主动 cancel + notify 父 + trackChildDone，
	// 比等满 sub_agent_timeout（默认 60min）早暴露。<=0 关闭巡检（测试场景默认关闭）。
	heartbeatTimeout time.Duration
	// domainHeartbeatTimeout DomainAgent 心跳超时（TODO #25-3 防误杀版）：
	// 默认 2× 叶子——domain 等子/等回信期间自身无 LLM/工具活动，靠后代活动冒泡保活；
	// 后代全静默后超该阈值才判假死。<=0 时按 2× heartbeatTimeout 兜底。
	domainHeartbeatTimeout time.Duration
	// activity 存叶子子 Agent 最后活动时间戳（unix nano），键 subAgentID -> *atomic.Int64。
	// 仅叶子 Agent 注入（DomainAgent/MetaAgent 有 wait loop 不注入，避免误杀合法等待）。
	activity sync.Map
	// lastWrites 存子 Agent 近期写入的文件清单（subAgentID -> *fileWriteState），
	// 从实时工具事件识别（WriteFile/EditFile）。心跳巡检 kill 时把清单回告父 Agent--
	// 收尾卡死被杀的子 Agent 常已把产物全部落盘（2026-08-19 引擎 Agent：文件 15:54 落盘、
	// 回执因 judge 挂死拖到 17:03），父级需要知道盘上有货可按现状验收，而非从零重派。
	lastWrites sync.Map
	// subMeta 存子 Agent 的 cancel/parentID/sessionID/doneOnce，供巡检卡死时主动 cancel + 兜底递减。
	// doneOnce 保证 patrol 与 goroutine 任一方 trackChildDone 仅触发一次，防双递减。
	subMeta sync.Map // subAgentID -> *subAgentMeta
	// patrolOnce 保证巡检 goroutine 只启动一次；patrolStop 关闭后巡检退出（测试用 ClosePatrol）。
	patrolOnce sync.Once
	patrolStop chan struct{}

	// hotCfg DomainAgent 热驻留配置（idle_pool.go）；Enabled=false（默认零值）时
	// 所有热驻路径零变化。bootstrap 按 config domain_hot_resident_enabled 注入。
	hotCfg domainHotConfig
	// pool 热驻 domain 槽池（hotCfg.Enabled 时由 WithDomainHotResident 初始化）。
	pool *domainPool
	// suspendStates 会话级挂起状态（sessionID -> *sessionSuspendState），
	// 热驻模式下触限暂停波及全树：叶子与 domain 的 SuspendGate.Park 阻塞在 wake 上。
	suspendStates sync.Map
}

// pendingState 跟踪单个父 Agent 的未决子 Agent 计数与完成信号。
// count 为当前在飞的子 Agent 数；notify 在任一子 Agent 完成时被发送（非阻塞），
// 供 WaitForAnyChild 的 select 消费。
type pendingState struct {
	count  atomic.Int64
	notify chan struct{} // 缓冲 1：允许多次完成信号合并，不阻塞发送方
}

// getOrCreatePending 取或创建父 Agent 的 pendingState。
// 并发安全：同一 parentID 的并发派发会拿到同一实例。
func (d *Dispatcher) getOrCreatePending(parentID string) *pendingState {
	if v, ok := d.pending.Load(parentID); ok {
		return v.(*pendingState)
	}
	ps := &pendingState{notify: make(chan struct{}, 1)}
	v, loaded := d.pending.LoadOrStore(parentID, ps)
	if loaded {
		return v.(*pendingState)
	}
	return ps
}

// trackChildStart 在父 Agent 派发子 Agent 时递增其未决计数。
func (d *Dispatcher) trackChildStart(parentID string) {
	d.getOrCreatePending(parentID).count.Add(1)
}

// trackChildDone 在子 Agent 结束（成功/失败/超时）时递减父 Agent 未决计数，
// 并非阻塞地通知一声，唤醒可能在 WaitForAnyChild 中等待的父 Agent。
func (d *Dispatcher) trackChildDone(parentID string) {
	ps := d.getOrCreatePending(parentID)
	ps.count.Add(-1)
	select {
	case ps.notify <- struct{}{}:
	default:
	}
}

// subAgentMeta 存子 Agent 巡检所需元数据：cancel 用于主动取消卡死子 Agent ctx；
// parentID/sessionID 用于 notify 父与 treeFinish；doneOnce 保证 trackChildDone 仅触发一次
//（patrol 与 goroutine 竞争时防双递减，PendingChildren 不会为负）。
// wallClock 是本次派发的有效墙钟（wall_clock_min ∩ sub_agent_timeout），供失败文案
// 报准确上限（否则 15 分钟预算被杀时文案误报"上限 2h0m0s"）。
type subAgentMeta struct {
	cancel    context.CancelFunc
	parentID  string
	sessionID string
	wallClock time.Duration
	doneOnce  sync.Once
}

// effectiveTimeout 返回子 Agent 的有效墙钟：优先派发级 wall_clock_min（已 min 全局值），
// 未派发级预算时回退全局 d.timeout。
func (d *Dispatcher) effectiveTimeout(subAgentID string) time.Duration {
	if v, ok := d.subMeta.Load(subAgentID); ok {
		if wc := v.(*subAgentMeta).wallClock; wc > 0 && (d.timeout <= 0 || wc < d.timeout) {
			return wc
		}
	}
	return d.timeout
}

// WithHeartbeatTimeout 配置子 Agent 心跳超时；<=0 关闭巡检（测试场景默认关闭）。
// bootstrap 从 config.SubAgentHeartbeatTimeoutMin 注入（默认 5min）。
func (d *Dispatcher) WithHeartbeatTimeout(t time.Duration) *Dispatcher {
	d.heartbeatTimeout = t
	return d
}

// WithDomainHeartbeatTimeout 配置 DomainAgent 心跳超时（TODO #25-3 防误杀版）。
// <=0 时按 2× heartbeatTimeout 兜底；bootstrap 从 config.DomainHeartbeatTimeoutMin 注入。
func (d *Dispatcher) WithDomainHeartbeatTimeout(t time.Duration) *Dispatcher {
	d.domainHeartbeatTimeout = t
	return d
}

// bubbleActivity 把子 Agent 活动沿 parentID 链向上冒泡（TODO #25-3）：
// domain 等子/等回信期间自身无 LLM/工具活动，靠后代活动刷新保持存活；
// 后代全静默后 domain 超其阈值才判假死。subMeta 缺失或链顶（meta/会话）终止。
func (d *Dispatcher) bubbleActivity(agentID string, now int64) {
	cur := agentID
	for depth := 0; depth < 32; depth++ { // 深度上限防御（三层 Agent 树足够）
		v, ok := d.subMeta.Load(cur)
		if !ok {
			return
		}
		meta := v.(*subAgentMeta)
		if meta.parentID == "" {
			return
		}
		if av, ok := d.activity.Load(meta.parentID); ok {
			av.(*atomic.Int64).Store(now)
		}
		cur = meta.parentID
	}
}

// fileWriteRecord 是单个子 Agent 的一次文件写入记录（路径 + 时间）。
type fileWriteRecord struct {
	path string
	at   time.Time
}

// fileWriteState 是单个子 Agent 的写入清单（互斥保护，写事件流与 kill 巡检读并发）。
type fileWriteState struct {
	mu   sync.Mutex
	recs []fileWriteRecord
}

// fileWriteStateMaxRecords 是单 Agent 保留的最大写入记录数（去重后上限，防长任务膨胀）。
const fileWriteStateMaxRecords = 8

// recordFileWrite 从工具调用实时事件识别文件写入（WriteFile/EditFile），记录路径与时间。
// 同路径去重保最新写入时间。
func (d *Dispatcher) recordFileWrite(subAgentID string, ev agent.LiveEvent) {
	if ev.Kind != agent.LiveEventToolCall || (ev.Tool != "WriteFile" && ev.Tool != "EditFile") {
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(ev.Input), &in) != nil || strings.TrimSpace(in.Path) == "" {
		return
	}
	path := strings.TrimSpace(in.Path)
	v, _ := d.lastWrites.LoadOrStore(subAgentID, &fileWriteState{})
	st := v.(*fileWriteState)
	st.mu.Lock()
	defer st.mu.Unlock()
	next := make([]fileWriteRecord, 0, len(st.recs)+1)
	for _, r := range st.recs {
		if r.path != path {
			next = append(next, r)
		}
	}
	next = append(next, fileWriteRecord{path: path, at: time.Now()})
	if len(next) > fileWriteStateMaxRecords {
		next = next[len(next)-fileWriteStateMaxRecords:]
	}
	st.recs = next
}

// recentWrittenFiles 返回子 Agent 近 within 窗口内写入的文件清单（保序去重）。
func (d *Dispatcher) recentWrittenFiles(subAgentID string, within time.Duration) []string {
	v, ok := d.lastWrites.Load(subAgentID)
	if !ok {
		return nil
	}
	st := v.(*fileWriteState)
	cutoff := time.Now().Add(-within)
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, r := range st.recs {
		if r.at.After(cutoff) {
			out = append(out, r.path)
		}
	}
	return out
}

// PingActivity 外部保活探针（等待用户答复场景）：审批/提问阻塞期间由会话层周期性调用，
// 刷新该 Agent 活动时间并沿父链冒泡，防止心跳巡检把"等用户操作"误判为假死 kill。
// agentID 未注册（meta/已终结）时静默跳过。
func (d *Dispatcher) PingActivity(agentID string) {
	if agentID == "" {
		return
	}
	now := time.Now().UnixNano()
	if av, ok := d.activity.Load(agentID); ok {
		av.(*atomic.Int64).Store(now)
	}
	d.bubbleActivity(agentID, now)
}

// ensurePatrol 幂等启动心跳巡检 goroutine：仅 heartbeatTimeout>0 时启动，Dispatcher 生命周期内一次。
func (d *Dispatcher) ensurePatrol() {
	if d.heartbeatTimeout <= 0 {
		return
	}
	d.patrolOnce.Do(func() {
		d.patrolStop = make(chan struct{})
		go d.patrol()
	})
}

// ClosePatrol 关闭心跳巡检 goroutine，供测试清理；生产生命周期内无需调用。
func (d *Dispatcher) ClosePatrol() {
	if d.patrolStop != nil {
		close(d.patrolStop)
		d.patrolStop = nil
	}
}

// patrol 周期扫描叶子子 Agent，超 heartbeatTimeout 无活动则判定假死并 kill。
func (d *Dispatcher) patrol() {
	interval := d.heartbeatTimeout / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.patrolStop:
			return
		case <-ticker.C:
			d.scanStuck()
		}
	}
}

// scanStuck 扫描 activity map，对超阈值无活动的子 Agent 执行 killStuckSubAgent。
// 叶子按 heartbeatTimeout；domain 按 domainHeartbeatTimeout（默认 2× 叶子，
// 等子/等回信期间靠后代活动冒泡保活，防误杀合法等待，TODO #25-3）。
func (d *Dispatcher) scanStuck() {
	if d.heartbeatTimeout <= 0 {
		return
	}
	now := time.Now().UnixNano()
	domainThreshold := d.domainHeartbeatTimeout
	if domainThreshold <= 0 {
		domainThreshold = 2 * d.heartbeatTimeout
	}
	d.activity.Range(func(k, v any) bool {
		act := v.(*atomic.Int64)
		threshold := d.heartbeatTimeout
		if roleIDFromAgentID(k.(string)) == "domain" {
			threshold = domainThreshold
		}
		if act.Load() > now-int64(threshold) {
			return true // 仍活跃
		}
		d.killStuckSubAgent(k.(string))
		return true
	})
}

// killStuckSubAgent 主动取消假死子 Agent：cancel ctx + 兜底 trackChildDone + notify 父 +
// 树节点置 Failed + 清理 activity/subMeta/running。runSubAgent goroutine 若因 cancel 返回，
// 其 doneOnce.Do 为 no-op；若不尊重 ctx（流式挂起），此处 doneOnce 兜底递减防父永久空等。
func (d *Dispatcher) killStuckSubAgent(subAgentID string) {
	v, ok := d.subMeta.LoadAndDelete(subAgentID)
	if !ok {
		return
	}
	meta := v.(*subAgentMeta)
	// 展示用真实阈值：domain 按 domainHeartbeatTimeout（默认 2× 叶子），
	// 与 scanStuck 判定口径一致——旧文案恒用叶子阈值，domain 被杀时报"超过 5m0s"与实际不符。
	threshold := d.heartbeatTimeout
	if roleIDFromAgentID(subAgentID) == "domain" && d.domainHeartbeatTimeout > 0 {
		threshold = d.domainHeartbeatTimeout
	} else if roleIDFromAgentID(subAgentID) == "domain" {
		threshold = 2 * d.heartbeatTimeout
	}
	log.Printf("[subagent] HEARTBEAT KILL: sub=%s parent=%s idle>%s - cancel+notify",
		subAgentID, meta.parentID, threshold)
	meta.cancel()
	meta.doneOnce.Do(func() { d.trackChildDone(meta.parentID) })
	killMsg := failureMarker(FailureKindKilled, false) + "\n" +
		fmt.Sprintf("子 Agent %s 超过 %s 无活动，判定假死已主动取消。请检查任务或重派。", subAgentID, threshold)
	// 盘上产物回告（2026-08-19）：收尾卡死被杀的子 Agent 常已把文件全部落盘，
	// 只是回执被挂死的收尾自检拖住--父级据此可按盘上现状直接验收，不必从零重派。
	if paths := d.recentWrittenFiles(subAgentID, 30*time.Minute); len(paths) > 0 {
		killMsg += "\n该 Agent 近期已写入以下文件（盘上产物大概率可用，可按盘上现状直接验收，无需从零重派）：\n- " +
			strings.Join(paths, "\n- ")
	}
	d.notify(meta.parentID, subAgentID, killMsg, nil)
	if d.treeFn != nil && meta.sessionID != "" {
		if t := d.treeFn(meta.sessionID); t != nil {
			t.Finish(subAgentID, "心跳超时疑似卡死", errors.New("heartbeat timeout"))
		}
	}
	// 失败打捞（kill 场景）：从树节点取 domain，以 kill 消息文本作回退摘要写槽位，
	// 供同域重派带前序摘要；goroutine 若尊重 cancel 会经 runSubAgent 失败路径覆盖为真实摘要。
	if d.treeFn != nil && meta.sessionID != "" {
		if t := d.treeFn(meta.sessionID); t != nil {
			for _, n := range t.Snapshot() {
				if n.ID != subAgentID {
					continue
				}
				d.salvageFailure(context.Background(), meta.parentID, subAgentID,
					types.RoleDefinition{ID: n.Role}, n.Domain, agent.ReactResult{}, killMsg)
				break
			}
		}
	}
	d.activity.Delete(subAgentID)
	d.running.Delete(subAgentID)
	if d.mailbox != nil {
		d.mailbox.Purge(subAgentID)
	}
}

// PendingChildren 返回指定父 Agent 当前未完成的子 Agent 数。
// 实现 agent.PendingChildrenChecker 接口，供 ReActAgent 在终结前检查。
func (d *Dispatcher) PendingChildren(parentID string) int {
	v, ok := d.pending.Load(parentID)
	if !ok {
		return 0
	}
	ps := v.(*pendingState)
	c := ps.count.Load()
	if c < 0 {
		return 0
	}
	return int(c)
}

// WaitForAnyChild 阻塞等待父 Agent 的任一子 Agent 完成，最长 timeout。
// 返回 true 表示收到完成信号（或调用时已无未决子 Agent）；false 表示超时。
// 实现 agent.PendingChildrenChecker 接口，供 ReActAgent 终结保护分支消费。
func (d *Dispatcher) WaitForAnyChild(parentID string, timeout time.Duration) bool {
	ps := d.getOrCreatePending(parentID)
	if ps.count.Load() <= 0 {
		return true
	}
	if timeout <= 0 {
		return false
	}
	select {
	case <-ps.notify:
		// 收到信号后清空缓冲，使下一次等待能再次阻塞。
		select {
		case <-ps.notify:
		default:
		}
		return true
	case <-time.After(timeout):
		return false
	}
}

// pokeParent 唤醒父 Agent 的终结保护 wait loop（不改变 PendingChildren 计数）。
// 软停止 Pause 路径调用：让父 MetaAgent 尽快检测 Paused 子节点转为 PausedOnChild，
// 否则父要等满 30s wait 周期。无缓冲竞争时丢弃（wait loop 会在下个周期自检）。
func (d *Dispatcher) pokeParent(parentID string) {
	ps := d.getOrCreatePending(parentID)
	select {
	case ps.notify <- struct{}{}:
	default:
	}
}

// HasPausedChild 返回父 Agent 是否有 StatusPaused 的子 DomainAgent 节点。
// 实现 agent.PausedChildChecker 接口，供 MetaAgent 父终结保护 wait loop 检测：
// 子 domain 触达 token 上限进入 Paused 后，父 MetaAgent 无限 budget 不会自行暂停，
// 在 wait loop 中调此方法检测，命中则跳出返回 PausedOnChild，由上层 pauseSession
// 置会话暂停态，等用户"继续"恢复该 domain（各 Agent 独立上下文）。
// parentID 对 MetaAgent 即 sessionID（其 a.name）；含 "/" 时取前段（防御性，子 Agent 派发场景不达）。
// treeFn 为 nil 时返回 false（测试场景）。
func (d *Dispatcher) HasPausedChild(parentID string) bool {
	if d.treeFn == nil || parentID == "" {
		return false
	}
	sid := parentID
	if i := strings.Index(parentID, "/"); i > 0 {
		sid = parentID[:i]
	}
	t := d.treeFn(sid)
	if t == nil {
		return false
	}
	for _, n := range t.Snapshot() {
		if n.ParentID == parentID && n.Status == orchestrator.StatusPaused {
			return true
		}
	}
	return false
}

// NewDispatcher 创建一个新的子 Agent 调度器。
// registry、models、tools、mailbox 必须传入；memory 可以为 nil，nil 时内部使用 NopMemoryPipeline。
func NewDispatcher(
	registry *role.Registry,
	models ModelProviderFactory,
	tools *tool.Registry,
	mailbox *mailbox.Mailbox,
	memory agent.MemoryPipeline,
) *Dispatcher {
	// 构造 Dispatcher 实例，将依赖注入到对应字段。
	return &Dispatcher{
		registry:          registry,
		models:            models,
		tools:             tools,
		mailbox:           mailbox,
		memory:            memory,
		timeout:           30 * time.Minute, // 默认 30 分钟，可用 WithTimeout 覆盖；<=0 表示不限制
		salvageTimeout:    30 * time.Second, // 默认 30s，可用 WithSalvageTimeout 覆盖（TODO #33）
		taskRuneSoftLimit: 3000,             // task 文本软上限（TODO #35），WithTaskRuneLimits 覆盖
		taskRuneHardLimit: 4000,
		softStops:         make(map[string]bool),
	}
}

// SetSoftStop 标记会话进入软停止（TODO #37）：ReactService.Stop 调用，先标记再触发
// 子 Agent cancel，dispatcher 收尾分支据此刻意落 Paused/部分回灌。幂等。
func (d *Dispatcher) SetSoftStop(sessionID string) {
	d.softStopMu.Lock()
	defer d.softStopMu.Unlock()
	d.softStops[sessionID] = true
}

// ClearSoftStop 清除会话软停止标记（续跑触发时调用）。幂等。
func (d *Dispatcher) ClearSoftStop(sessionID string) {
	d.softStopMu.Lock()
	defer d.softStopMu.Unlock()
	delete(d.softStops, sessionID)
}

// isSoftStop 查询会话是否处于软停止中。
func (d *Dispatcher) isSoftStop(sessionID string) bool {
	if d == nil {
		return false
	}
	d.softStopMu.Lock()
	defer d.softStopMu.Unlock()
	return d.softStops[sessionID]
}

// IsSoftStop 公开查询会话软停止标记（ReactService.isSoftStopCancel 经接口断言调用，
// 区分用户停止与硬取消的 context.Canceled 分流）。
func (d *Dispatcher) IsSoftStop(sessionID string) bool {
	return d.isSoftStop(sessionID)
}

// WithTimeout 配置子 Agent 独立执行的最大时长；<=0 表示不限制（仅防挂起的保底由调用方负责）。
func (d *Dispatcher) WithTimeout(t time.Duration) *Dispatcher {
	d.timeout = t
	return d
}

// WithDomainReconClock 配置 DomainAgent 派发无显式 wall_clock_min 时的默认墙钟。
// <=0 关闭（回退全局 timeout）。bootstrap 从 config domain_recon_wall_clock_min 注入。
func (d *Dispatcher) WithDomainReconClock(t time.Duration) *Dispatcher {
	d.domainReconClock = t
	return d
}

// WithPluginVisibility 注入插件工具角色可见性回调（设计文档 §4.3）。
// 由 bootstrap 注入 plugins.Manager.ToolVisibility；传 nil 关闭插件可见性过滤。
func (d *Dispatcher) WithPluginVisibility(fn agent.ToolVisibilityFunc) *Dispatcher {
	d.pluginVisibility = fn
	return d
}
// WithTaskRuneLimits 配置派发 task 文本双档上限（TODO #35 放开预算）：
// 超 soft 未达 hard 软着陆放行附警告，超 hard 硬拒。<=0 按默认 3000/4000。
// bootstrap 按 cfg.Agent.TaskMaxRunes / TaskMaxRunesHard 注入。
func (d *Dispatcher) WithTaskRuneLimits(soft, hard int) *Dispatcher {
	if soft > 0 {
		d.taskRuneSoftLimit = soft
	}
	if hard > 0 {
		d.taskRuneHardLimit = hard
	}
	return d
}

	// WithMaxPausedResumes 设置同一 Paused domain 的最大续跑次数（<=0 按默认 1）。
// 触顶后 ResumePaused 不再给 fresh budget 续跑，强制收口：部分产出 notify 父 + 标 Done，
// 由 MetaAgent 决定返工——与叶子助手 errPartialReturn 同哲学。
// bootstrap 按 cfg.Agent.PausedDomainMaxResumes 注入。
func (d *Dispatcher) WithMaxPausedResumes(n int) *Dispatcher {
	d.maxPausedResumes = n
	return d
}

// WithLoopConfigByRole 注入按角色返回 LoopConfig 的函数,使不同角色 token 预算分级
// (domain 50K / 叶子助手 20K / meta 0)。bootstrap 传 reactCfg.LoopConfigByRole 方法值。
func (d *Dispatcher) WithLoopConfigByRole(fn func(string) agent.LoopConfig) *Dispatcher {
	d.loopCfgByRole = fn
	return d
}

// loopConfigFor 返回角色的 LoopConfig。loopCfgByRole 未注入(测试场景)时用零值默认:
// 由 ReActAgent 的 WithLoopConfig 兜底(MaxIterations=0 即不限制,budget=0 不限制)。
func (d *Dispatcher) loopConfigFor(roleID string) agent.LoopConfig {
	if d.loopCfgByRole != nil {
		return d.loopCfgByRole(roleID)
	}
	return agent.LoopConfig{}
}

// WithBlockMemorySearcher 注入块记忆检索器，使子 Agent 启动前能按拆分任务文本召回相关块记忆。
// 传 nil 表示关闭召回（默认关闭）。
func (d *Dispatcher) WithBlockMemorySearcher(s BlockMemorySearcher) *Dispatcher {
	d.searcher = s
	return d
}

// WithMaxTotalDispatches 注入全局派发总数上限：同一 session 内所有角色派发合计
// 超过 n 时拒绝。n<=0 表示不限制。计数在用户发送新消息时经 ResetDispatchCounts 重置。
func (d *Dispatcher) WithMaxTotalDispatches(n int) *Dispatcher {
	d.maxTotalDispatches = n
	return d
}

// ResetDispatchCounts 清空指定 session 的派发计数。
// 新用户消息 = 新任务起点：上一任务的限额消耗不应卡死下一任务。
func (d *Dispatcher) ResetDispatchCounts(sessionID string) {
	if sessionID == "" {
		return
	}
	d.sessionCounts.Delete(sessionID)
}

// WithLogger 注入会话级日志器，使子 Agent 的 LLM I/O 写入 session_logs。
// 传 nil 关闭子 Agent LLM I/O 日志（默认关闭）。
func (d *Dispatcher) WithLogger(l *logger.Logger) *Dispatcher {
	d.log = l
	return d
}

// WithLiveEvents 注入实时事件转发器，使子 Agent 的 LiveEvent（token 用量/流式/工具）
// 按 sessionID 路由回所属会话的 service.handleLiveEvent。
// WithLiveEvents 注入子 Agent 实时事件转发器：子 Agent emitLive 时按 sessionID 路由回会话 service。
// 传 nil 关闭子 Agent 实时事件推送（默认关闭）。
func (d *Dispatcher) WithLiveEvents(fn func(sessionID string, ev agent.LiveEvent)) *Dispatcher {
	d.liveFn = fn
	return d
}

// WithTree 注入权威 Agent 树访问器。
// fn 按 sessionID 返回 *orchestrator.Tree（lazy init），供 Dispatcher 在派发时
// Register 节点 + SetCancel 绑定 cancel func，完成时 Finish。
// 传 nil 关闭树跟踪（默认关闭）；HTTP API 的 /tree 与 /cancel 端点依赖此树。
func (d *Dispatcher) WithTree(fn func(sessionID string) *orchestrator.Tree) *Dispatcher {
	d.treeFn = fn
	return d
}

// WithBoard 注入任务看板访问器（TODO #22 执行计划）。
// get 按 sessionID 取既有看板（无则 nil）；create 取或创建（write_plan 工具用）。
// 传 nil 关闭计划功能（默认关闭）：依赖门/回写/写计划工具零行为变化。
func (d *Dispatcher) WithBoard(get func(sessionID string) *board.TaskBoard, create func(sessionID, goal string) *board.TaskBoard) *Dispatcher {
	d.boardFn = get
	d.boardFnCreate = create
	return d
}

// WithMessagesStore 注入 Paused DomainAgent 消息历史持久化层。
// DomainAgent 触达 token 上限时 SaveMessages 落库,resume 时 LoadMessages 重建上下文。
// 为 nil 时跳过持久化(测试场景)。
func (d *Dispatcher) WithMessagesStore(s agent.MessagesStore) *Dispatcher {
	d.msgStore = s
	return d
}

// WithFactExtractor 注入事实提取器，使 saveBlockMemory 先尝试 LLM 提取关键事实
// 再落库（每条事实单独 KnowledgeRecord，向量化后召回精度更高）。
// 传 nil 关闭提取（默认关闭），saveBlockMemory 回退原始文本保存。
func (d *Dispatcher) WithFactExtractor(e FactExtractor) *Dispatcher {
	d.factExtractor = e
	return d
}

// WithDispatchRetryCount 设置叶子助手 kind=error 失败的自动重派次数（TODO #23）。
// n<=0 关闭自动重派（默认关闭，测试场景）；bootstrap 按 cfg.Agent.DispatchRetryCount 注入。
func (d *Dispatcher) WithDispatchRetryCount(n int) *Dispatcher {
	d.dispatchRetryCount = n
	return d
}

// WithEngineConfig 配置派发执行模式引擎参数（TODO #29）。
// reflectionMaxRounds 为 mode=reflection 自检重试轮数（<=0 引擎按默认 2）；
// planMaxSteps 为 mode=plan_execute 最大执行步数（<=0 引擎按默认 8）。
// bootstrap 按 cfg.Agent.ReflectionMaxRounds / PlanExecuteMaxSteps 注入。
func (d *Dispatcher) WithEngineConfig(reflectionMaxRounds, planMaxSteps int) *Dispatcher {
	d.reflectionMaxRounds = reflectionMaxRounds
	d.planMaxSteps = planMaxSteps
	return d
}

// WithEngineLLMTimeout 配置引擎辅助 LLM（自检 judge/规划）单次调用超时；<=0 仅受子 Agent 墙钟控制。
// bootstrap 按 cfg.Agent.EngineLLMTimeoutSec 注入（默认 300s）。
func (d *Dispatcher) WithEngineLLMTimeout(t time.Duration) *Dispatcher {
	d.engineLLMTimeout = t
	return d
}

// WithJudgeRole 配置校验 judge 的角色 ID（TODO #43 交叉模型）。
// engineLLMForJudge 优先取该角色 provider（与被审角色不同模型，防同模型自评放水）；
// 取不到回退同角色。bootstrap 按 cfg.Agent.JudgeRole 注入。
func (d *Dispatcher) WithJudgeRole(roleID string) *Dispatcher {
	d.judgeRole = roleID
	return d
}

// WithSalvageExtractor 注入失败打捞提取器，使失败路径（超时/被杀/守卫终止）尝试
// LLM 提取打捞摘要（已读文件/已得结论/卡点）写入共享槽位并回灌父 mailbox。
// 传 nil 关闭 LLM 提取（默认关闭），回退末条 assistant 文本截断。
func (d *Dispatcher) WithSalvageExtractor(e SalvageExtractor) *Dispatcher {
	d.salvageExtractor = e
	return d
}

// WithSalvageTimeout 配置打捞轻量调用的超时（默认 30s；思考型模型场景建议 >=60s）。
// <=0 时按 30s 兜底。
func (d *Dispatcher) WithSalvageTimeout(t time.Duration) *Dispatcher {
	if t <= 0 {
		t = 30 * time.Second
	}
	d.salvageTimeout = t
	return d
}

// WithBlockMemorySaver 注入块记忆写入器与写入开关，使子 Agent 成功完成后
// 能把结果摘要沉淀到块记忆知识库，与召回侧形成"召回→执行→沉淀"闭环。
// enabled 为 false 或 s 为 nil 时关闭沉淀（默认关闭）。
func (d *Dispatcher) WithBlockMemorySaver(s BlockMemorySaver, enabled bool) *Dispatcher {
	d.saver = s
	d.writeEnabled = enabled
	return d
}

// WithSpecEnforcement 开启/关闭 call_sub_agent 前的 WriteSpec 强制校验。
// enabled 为 true 时，dispatcher 在 Execute 入口校验 parentID:spec 存在且新鲜，
// 缺失则拒绝派发；为 false 时跳过强制，spec 注入仍生效（graceful degrade）。
// bootstrap 按 cfg.Agent.SpecEnforcementEnabled 注入（默认 true）。
func (d *Dispatcher) WithSpecEnforcement(enabled bool) *Dispatcher {
	d.specEnforcementEnabled = enabled
	return d
}

// RegisterCallTool 将 call_sub_agent / call_sub_agents 工具安装到传入的工具注册表中。
// 工具被注册到父 Agent 与子 Agent 共同使用的 registry 上，
// 递归深度固定三层：MetaAgent -> DomainAgent -> 叶子助手（CanCall 拒绝 domain->domain 平级派发）。
func (d *Dispatcher) RegisterCallTool(r *tool.Registry) {
	// 注册 callSubAgentTool / callSubAgentsTool 实例，工具内部持有当前 Dispatcher 以便执行时调用。
	r.Register(&callSubAgentTool{dispatcher: d})
	r.Register(&callSubAgentsTool{dispatcher: d})
}

// RegisterMessagingTool 将 send_message 工具安装到传入的工具注册表中。
// 该工具支持任意 Agent 向另一个 Agent 实例的邮箱投递消息（请求/通知），
// 是多 Agent 协作验证闭环中"被询问方回复"与"状态同步"的基础原语。
// 目标 Agent 必须处于运行中（running map）或实例池（pool）内，否则消息会落入
// 其收件箱等待，但若目标已销毁则消息会被 Purge 一并清除。
func (d *Dispatcher) RegisterMessagingTool(r *tool.Registry) {
	r.Register(&sendMessageTool{dispatcher: d})
}

// sendMessageTool 实现 send_message 工具：向指定 Agent 实例邮箱投递一条消息。
// 消息 Type 为 MsgRequest，携带 ReplyTo=caller Agent ID，使接收方可按请求-响应
// 语义回投递回复（用 send_message 再发一条 Type=MsgReply 的消息）。
type sendMessageTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *sendMessageTool) Name() string { return "send_message" }

// Aliases 返回工具别名。
func (t *sendMessageTool) Aliases() []string { return []string{"send_msg"} }

// Description 返回 LLM 可见的工具描述。
func (t *sendMessageTool) Description() string {
	return "向另一个 Agent 实例的邮箱投递一条消息（请求/通知/升级），立即返回。" +
		"用于多 Agent 协作验证闭环：例如代码 Agent 完成后可向测试 Agent 发送验证请求，" +
		"测试 Agent 在下一轮 ReAct 迭代中 Drain 收件箱即可看到该消息并据此回复。" +
		"参数 to_agent_id 为目标 Agent 实例 ID（即 call_sub_agent 返回的 sub_agent_id，或父 Agent ID）；" +
		"subject 为一行摘要；body 为详情正文（可空）；message_type 可选 info（单向通知）/reply（回复）/escalate（升级求助，父 Agent 收到 [升级] 前缀消息需按规程处置）。" +
		"目标 Agent 已销毁时返回\"消息未送达\"。"
}

// Execute 执行 send_message 工具调用。
// 参数 args 由大模型提供，包含 to_agent_id / subject / body / message_type（可选）。
func (t *sendMessageTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	if d.mailbox == nil {
		return &tool.Result{Tool: "send_message", Error: "mailbox not configured"}
	}

	toID, _ := args["to_agent_id"].(string)
	subject, _ := args["subject"].(string)
	body, _ := args["body"].(string)
	if toID == "" || subject == "" {
		return &tool.Result{Tool: "send_message", Error: "to_agent_id and subject are required"}
	}

	fromID := agent.AgentIDFromContext(ctx)
	if fromID == "" {
		return &tool.Result{Tool: "send_message", Error: "missing caller agent context"}
	}

	// message_type 可选：默认 request（期望回复）；"info" 单向通知；"reply" 回复；"escalate" 升级请求。
	msgType := mailbox.MsgRequest
	if mt, _ := args["message_type"].(string); mt == "info" {
		msgType = mailbox.MsgInfo
	} else if mt == "reply" {
		msgType = mailbox.MsgReply
	} else if mt == "escalate" {
		msgType = mailbox.MsgEscalate
	}

	// thread_id 可选：同一问答链上的消息共享 ThreadID，便于多轮验证闭环聚合。
	threadID, _ := args["thread_id"].(string)

	id, err := d.mailbox.Send(&mailbox.Message{
		From:     fromID,
		To:       toID,
		Type:     msgType,
		Subject:  subject,
		Body:     body,
		ReplyTo:  fromID,
		ThreadID: threadID,
	})
	if err != nil {
		// 死信可见（TODO #23）：目标已销毁时告知发送方，不再静默消失。
		return &tool.Result{Tool: "send_message", Error: fmt.Sprintf("消息未送达: %v", err)}
	}

	return &tool.Result{
		Tool:    "send_message",
		Success: true,
		Output:  id,
	}
}

// RegisterControlTool 将 cancel_agent 工具安装到传入的工具注册表中。
// 供 MetaAgent/DomainAgent 主动取消失控/不再需要的子 Agent（TODO #25 控制面）。
func (d *Dispatcher) RegisterControlTool(r *tool.Registry) {
	r.Register(&cancelAgentTool{dispatcher: d})
}

// cancelAgentTool 实现 cancel_agent 工具：取消指定子 Agent（树节点 + cancel func + 父计数兜底）。
// 目标已 terminal 或不存在时返回错误；取消后父 mailbox 收到"已被上级取消"通知。
type cancelAgentTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *cancelAgentTool) Name() string { return "cancel_agent" }

// Aliases 返回工具别名列表，当前无别名。
func (t *cancelAgentTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *cancelAgentTool) Description() string {
	return "取消一个正在运行的子 Agent（输入 call_sub_agent 返回的 sub_agent_id）。" +
		"适用场景：子 Agent 跑偏/失控/不再需要（如需求变更）时主动止损。" +
		"取消后该子 Agent 任务终止，父 Agent 会收到其失败回传；已完成的 Agent 无法取消。"
}

// Execute 执行 cancel_agent 工具调用。
// 参数 args 由大模型提供，包含 agent_id。
func (t *cancelAgentTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	agentID, _ := args["agent_id"].(string)
	if agentID == "" {
		return &tool.Result{Tool: "cancel_agent", Error: "agent_id is required"}
	}
	if d.treeFn == nil {
		return &tool.Result{Tool: "cancel_agent", Error: "agent tree not available"}
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(agentID, "/"); i > 0 {
			sid = agentID[:i]
		}
	}
	tr := d.treeFn(sid)
	if tr == nil {
		return &tool.Result{Tool: "cancel_agent", Error: "agent tree not available for session"}
	}
	// 父 ID 从调用方 ctx 取：取消动作的发起者。
	callerID := agent.AgentIDFromContext(ctx)
	// 从树快照取节点父 ID（通知父"已被上级取消"）。
	parentID := ""
	for _, n := range tr.Snapshot() {
		if n.ID == agentID {
			parentID = n.ParentID
			break
		}
	}
	if !tr.Cancel(agentID) {
		return &tool.Result{Tool: "cancel_agent", Error: fmt.Sprintf("agent %s 不存在或已终止，无需取消", agentID)}
	}
	// 计数兜底（参照 killStuckSubAgent）：子 Agent 若挂起不尊重 ctx，父 PendingChildren
	// 由此处 doneOnce 递减，父终结保护不会永久阻塞。
	if metaV, ok := d.subMeta.LoadAndDelete(agentID); ok {
		meta := metaV.(*subAgentMeta)
		meta.doneOnce.Do(func() { d.trackChildDone(meta.parentID) })
	}
	if parentID != "" && d.mailbox != nil {
		_, _ = d.mailbox.Send(&mailbox.Message{
			From:    callerID,
			To:      parentID,
			Type:    mailbox.MsgInfo,
			Subject: "子 Agent 被取消: " + agentID,
			Body:    failureMarker(FailureKindKilled, false) + "\n子 Agent " + agentID + " 已被上级取消，任务终止。",
		})
	}
	return &tool.Result{
		Tool:    "cancel_agent",
		Success: true,
		Output:  "已取消 " + agentID,
	}
}

// callSubAgentTool 实现内部 tool.Tool 接口，代表 call_sub_agent 这一可调用工具。
type callSubAgentTool struct {
	dispatcher *Dispatcher // dispatcher 持有调度器引用，工具执行时通过它创建子 Agent。
}

// Name 返回工具名称。
func (t *callSubAgentTool) Name() string { return "call_sub_agent" }

// Aliases 返回工具别名列表，当前无别名。
func (t *callSubAgentTool) Aliases() []string { return nil }

// Description 返回工具的 LLM 可见描述，包含当前可调用角色的动态清单。
// domain/tool.Registry.Schema 通过可选接口断言读取该描述，
// 使工具 schema 始终反映 roles.yaml 的最新角色配置。
func (t *callSubAgentTool) Description() string {
	// 角色清单：domain 作为默认派发入口列首，固定助手标为叶子执行者。
	entries := []string{"domain（默认派发入口：复杂任务/不确定范围走这里，DomainAgent 是该领域的直接执行者，收到后默认自执行，仅其自主判断需要时才下拆叶子助手）"}
	for _, fr := range t.dispatcher.registry.CallableFixedRoles() {
		entries = append(entries, fmt.Sprintf("%s（叶子执行者：%s；仅在任务已单函数级、单文件、领域明确时直派）", fr.ID, fr.Description))
	}
	return "将子任务派发给指定角色的子 Agent 异步执行。调用立即返回 sub_agent_id；" +
		"子 Agent 完成后，其结果摘要会以 [mailbox from <sub_agent_id>] 消息送达，请在后续轮次中阅读并整合。\n" +
		fmt.Sprintf("task 必须自包含 <= %d 字（按 rune 计数，含中文字符）：背景、目标、相关文件路径、前置结论与验收标准--子 Agent 看不到当前对话历史。", t.dispatcher.taskRuneSoftLimit) +
		"规格原文走 WriteSharedMemory，不塞进 task。超长 task 将被拒绝，错误提示\"task too long\"（" +
		fmt.Sprintf("%d-%d 字轻微超限会放行但附压缩警告，>%d 字硬拒）。\n\n", t.dispatcher.taskRuneSoftLimit, t.dispatcher.taskRuneHardLimit, t.dispatcher.taskRuneHardLimit) +
		"【前置依赖】派发前必须先调 WriteSpec(goal, acceptance, constraints, files) 写入任务规范，否则返回错误（spec missing=未写 / spec stale=涉及文件已变更且列出失配路径 / spec invalid=缺 goal 或验收）。" +
		"WriteSpec 与 WriteSharedMemory 是不同工具：WriteSharedMemory 写自由 KV 供子 Agent 读，" +
		"WriteSpec 写固定 slot \"spec\" 供 dispatcher 校验并注入子 Agent 任务体前缀。两者不可互相替代。\n\n" +
		"【路由规则】\n" +
		"1. 默认走 domain：多文件/多函数/多步骤/不确定范围 -> role_id=\"domain\"。DomainAgent 是该领域的直接执行者，收到后默认自执行，仅其自主判断需要时才下拆叶子。\n" +
		"2. 直派固定助手：仅当任务已单函数级、单文件、领域明确（如\"修改 X 函数签名\"、\"补一个测试\"）时直派对应助手。\n" +
		"3. 不确定走哪条？走 domain。\n" +
		"4. 若你本身就是 DomainAgent：你不能派 domain（会被拒绝）。默认自执行，仅按你提示词中的【拆分决策】必要时直派固定助手。\n\n" +
		"【domain 字段】role_id=\"domain\" 时填领域分类简称（如 金融/认证/UI/数据库/配置），" +
		"用于子 Agent 展示名（\"金融领域Agent\"）。固定助手忽略此字段，用其角色名。\n" +
		"【responsibility 字段】role_id=\"domain\" 时必填：该领域 Agent 的职责边界（<= 200 字），" +
		"写明负责哪些文件/模块、不碰哪些。会注入子 Agent 系统提示词，长跑不丢。\n\n" +
		"【mode 字段】（可选）派发执行模式：react（默认）/ reflection / plan_execute。" +
		"琐碎单步任务省略；正确性敏感任务（算法/迁移/重构）用 reflection——执行后自动对照验收标准自检，不达标带反馈重试；" +
		"多步骤长任务（多文件/多阶段）用 plan_execute——先出步骤计划（TUI 可见）再逐步执行。判断不准时省略，默认 react。\n\n" +
		"【verify_kind 字段】（可选）校验分层：auto（默认，按角色与执行模式自动选——代码/测试助手自动要求可执行证据、自检模式自动 rubric 评审）/ executable（必须有测试/lint/--check 成功运行的客观证据，否则会反馈重试 1 轮）/ rubric（独立评审模型按验收标准逐条判）/ none（跳过校验）。判断不准时省略，默认 auto。注意：verify_kind 与 mode 是不同字段，勿把 mode 的值填到本字段。\n\n" +
		"【tools_hint 字段】（可选）建议工具集：子 Agent 需要插件工具（如画图/搜索/浏览器）时，在此声明工具名列表（来自 tool_catalog），" +
		"dispatcher 校验 ∩ 子 Agent 角色权限天花板后预挂载——子 Agent 当轮即可见对应插件工具，无需自己挂载。" +
		"天花板外（插件 roles 白名单不允许）的越界项会被忽略并随本调用结果回告，不放大权限。\n\n" +
		"【wall_clock_min 字段】（可选）本次派发的墙钟预算（分钟，代码级强制执行，与全局 sub_agent_timeout 取小）：" +
		"到期前子 Agent 会收到收口警告，超时直接终止。验收/巡检类任务建议显式给预算（如 15）防止无边界扩张；普通建设任务省略" +
		"（省略时 domain 默认侦察墙钟 30 分钟，过半会收到\"停止侦察开始产出\"预警）。\n\n" +
		"【reuse_agent_id 字段】（可选，热驻复用）复用已完成的热驻领域 Agent：填【空闲领域Agent】清单中的 agent_id。" +
		"新任务与该领域强相关时优先复用（保留全部上下文与领域知识，省冷启动）；弱相关则省略本字段新建 domain。" +
		"复用时 role_id/domain/responsibility 可省略（沿用槽内冻结值），task 必填。目标 Agent 忙碌时任务入队，当前任务完成后自动执行。\n\n" +
		"可调用的 role_id：" + strings.Join(entries, "；") + "。"
}

// validateDispatchArgs 校验单次派发的必要参数；返回 (msg, warning)：
// msg 非空=硬拒绝（校验拒绝，Category=validation_rejected）；否则通过，
// warning 非空=放行但附提示（task 轻微超限软着陆，TODO #38-3）。
// 供 call_sub_agent 与 call_sub_agents 复用（批量工具逐项校验）。
func (d *Dispatcher) validateDispatchArgs(roleID, task, responsibility, mode, verifyKind string) (msg, warning string) {
	// 校验必要参数：role_id 与 task 均不能为空。
	if roleID == "" || task == "" {
		return "role_id and task are required", ""
	}
	// role_id="domain" 时 responsibility 必填：dispatcher 把它注入子 Agent 系统提示词头部，
	// 防止长 ReAct 循环中 task 被历史压缩后领域身份丢失（实证：领域 Agent 越界实现他域文件）。
	// LLM 经常省略该字段，导致 DomainAgent 拿到的是通用 prompt 无职责边界——此处硬拒绝强制回填。
	if roleID == "domain" && strings.TrimSpace(responsibility) == "" {
		return "responsibility is required when role_id=domain: 填该领域 Agent 的职责边界（<=200 字，负责哪些文件/模块、不碰哪些），会注入子 Agent 系统提示词", ""
	}
	// task 长度双档上限（TODO #35 放开）：强制 MetaAgent 把规格写入 WriteSharedMemory，task 只写目标+验收。
	// 原 500 runes 实证过紧：塔防类任务的自然派发文本 ~1200-1500 runes，每轮必触发
	// "task too long" 拒绝-重写循环（单次运行最多 4 次拒绝，白烧 1-2 分钟路由轮次）；
	// 2000/2600 档在强模型+大上下文时代仍偏紧，放开到 3000/4000（配置 task_max_runes 可调）。
	// 软上限与"task 自包含（背景+目标+验收）"要求天然冲突，硬拒白烧一整轮 MetaAgent 往返——
	// 超软限但未达硬限软着陆放行并附压缩警告；超硬限仍硬拒（全量规格转贴区间）。
	softLimit, hardLimit := d.taskRuneSoftLimit, d.taskRuneHardLimit
	if n := utf8.RuneCountInString(task); n > softLimit {
		if n > hardLimit {
			return fmt.Sprintf(
				"task too long: %d runes (hard max %d). 把规格/原文写入 WriteSharedMemory，task 只写目标+验收标准（%d 字内）",
				n, hardLimit, softLimit), ""
		}
		return "", fmt.Sprintf("task 已 %d runes，超出 %d 字预算但未达硬上限 %d，本次放行；下次派发请压缩至 %d 字内",
			n, softLimit, hardLimit, softLimit)
	}
	// mode 枚举校验（TODO #29）：空串=react（默认），非法值拒绝，防引擎拼错静默跑错模式。
	switch mode {
	case "", agent.ModeReact, agent.ModeReflection, agent.ModePlanExecute:
	default:
		return fmt.Sprintf("unknown mode %q: 可选 react / reflection / plan_execute（省略=react）", mode), ""
	}
	// verify_kind 枚举校验（TODO #43）：空串=auto（默认），非法值拒绝。
	switch verifyKind {
	case "", "auto", "executable", "rubric", "none":
	default:
		return fmt.Sprintf("unknown verify_kind %q: 可选 auto / executable / rubric / none（省略=auto）", verifyKind), ""
	}
	return "", ""
}

// checkSpecBeforeDispatch 做 WriteSpec 强制校验：SpecEnforcementEnabled 开启时，
// 派发前必须先 WriteSpec（校验 parentID:spec 存在、新鲜、Spec.Goal 非空且至少一条 Acceptance）。
// 返回空串表示通过，否则为错误文案。批量派发（call_sub_agents）只校验一次。
func (d *Dispatcher) checkSpecBeforeDispatch(ctx context.Context, parentID string) string {
	if !d.specEnforcementEnabled {
		return ""
	}
	if ok, reason := d.hasFreshSpec(ctx, parentID); !ok {
		return reason + " 先调 WriteSpec(goal, acceptance, constraints, files) 写任务规范，再派发"
	}
	return ""
}

// Execute 执行 call_sub_agent 工具调用。
// 参数 args 由大模型提供，包含 role_id 与 task；返回值 *tool.Result 表示调用结果。
func (t *callSubAgentTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher

	// 从 args 中提取 role_id 与 task，类型断言失败时得到空字符串。
	roleID, _ := args["role_id"].(string)
	task, _ := args["task"].(string)
	// domain 可选：仅 role_id="domain" 时用于子 Agent 展示名（如"金融领域Agent"）。
	domain, _ := args["domain"].(string)
	// responsibility 可选：仅 role_id="domain" 时注入子 Agent 系统提示词，钉住职责边界。
	responsibility, _ := args["responsibility"].(string)
	// mode 可选：派发执行模式（react/reflection/plan_execute），空串=react（默认）。
	mode, _ := args["mode"].(string)
	// verify_kind 可选：校验分层（auto/executable/rubric/none），空串=auto（默认，TODO #43）。
	verifyKind, _ := args["verify_kind"].(string)
	// tools_hint 可选：建议工具集（TODO #52）——dispatcher 校验 ∩ 子 Agent 天花板后预挂载。
	toolsHint := d.toolsHintArg(args)
	// wall_clock_min 可选：派发级墙钟（分钟），代码级强制收口，替代提示词墙钟。
	wallClock := d.wallClockArg(args)
	// reuse_agent_id 可选：热驻复用（idle domain 唤醒/忙碌入队），非空时忽略 role_id。
	reuseAgentID, _ := args["reuse_agent_id"].(string)

	msg, warning := t.dispatcher.validateDispatchArgs(roleID, task, responsibility, mode, verifyKind)
	if reuseAgentID == "" && msg != "" {
		return &tool.Result{Tool: "call_sub_agent", Error: msg, Category: tool.ResultCategoryValidationRejected}
	}
	// reuse 模式 role_id 可省（复用槽沿用原角色）；task 仍必填。
	if reuseAgentID != "" && strings.TrimSpace(task) == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "task is required", Category: tool.ResultCategoryValidationRejected}
	}
	// 从当前上下文获取父 Agent ID，子 Agent 需要知道是谁调用了它。
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "missing parent agent context"}
	}
	if msg := d.checkSpecBeforeDispatch(ctx, parentID); msg != "" {
		return &tool.Result{Tool: "call_sub_agent", Error: msg, Category: tool.ResultCategoryValidationRejected}
	}

	subAgentID, errRes := d.dispatchOne(ctx, roleID, domain, task, responsibility, mode, verifyKind, toolsHint, wallClock, reuseAgentID)
	if errRes != nil {
		errRes.Tool = "call_sub_agent"
		return errRes
	}
	out := subAgentID
	// task 轻微超限软着陆警告（TODO #38-3）：放行但提示下次压缩。
	if warning != "" {
		out += "。警告: " + warning
	}
	return &tool.Result{Tool: "call_sub_agent", Success: true, Output: out}
}

// dispatchOne 执行一次子 Agent 异步派发：权限校验→同领域去重→全局限额→注册树→goroutine。
// 成功返回 subAgentID；失败返回 *tool.Result（Error 非空，Tool 字段由调用方按工具名覆盖）。
// mode 为派发执行模式（react/reflection/plan_execute，空串=react，TODO #29），
// verifyKind 为校验分层（auto/executable/rubric/none，空串=auto，TODO #43），
// toolsHint 为建议工具集（TODO #52，可空）：校验 ∩ 子 Agent 角色天花板后预挂载到子 scope，
// wallClock 为派发级墙钟（>0 时取 min(wallClock, sub_agent_timeout) 替代全局值，到期前预警）。
// reuseAgentID 非空时走热驻复用（idle_pool.go dispatchToIdleSlot）：唤醒 idle domain
// 或忙碌入队，忽略 roleID/task 以外的派发参数。均穿透到子 Agent 构造时的引擎选择与完成后校验。
// 前置：调用方已完成参数校验（validateDispatchArgs）与 spec 强制校验。
// 供 call_sub_agent（单个）与 call_sub_agents（批量同波）两个工具复用。
func (d *Dispatcher) dispatchOne(ctx context.Context, roleID, domain, task, responsibility, mode, verifyKind string, toolsHint []string, wallClock time.Duration, reuseAgentID string) (string, *tool.Result) {
	// 从当前上下文获取父 Agent ID，子 Agent 需要知道是谁调用了它。
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return "", &tool.Result{Error: "missing parent agent context"}
	}

	// 热驻复用分流（reuse_agent_id 参数）：槽 idle 唤醒 + 注入新任务；busy 入队；
	// 不存在报错提示新建。热驻未开启时报错（防误参数静默新建）。
	if reuseAgentID != "" {
		if !d.hotEnabled() {
			return "", &tool.Result{Error: "reuse_agent_id 仅在 domain_hot_resident_enabled 开启时可用", Category: tool.ResultCategoryValidationRejected}
		}
		return d.dispatchToIdleSlot(ctx, parentID, reuseAgentID, task, wallClock, mode, verifyKind)
	}

	// 从角色注册表获取目标角色定义，若角色不存在则拒绝调用。
	roleDef := d.registry.Get(roleID)
	if roleDef == nil {
		return "", &tool.Result{Error: fmt.Sprintf("unknown role: %s", roleID), Category: tool.ResultCategoryValidationRejected}
	}

	// 校验调用权限：只有被允许的角色关系才能发起子 Agent 调用。
	if !d.registry.CanCall(roleIDFromAgentID(parentID), roleID) {
		return "", &tool.Result{Error: fmt.Sprintf("role %s cannot be called by %s", roleID, parentID), Category: tool.ResultCategoryValidationRejected}
	}

	// 派发依赖门（TODO #22 Phase 1）：计划中该领域子任务的 depends_on 未全部完成时拒绝，
	// 提示等谁（实证：MetaAgent 未等回传重复派发渲染引擎×3 互相覆盖——依赖门治本）。
	// 无计划（board nil/领域未覆盖）零行为变化；拒绝不烧派发配额。
	if roleID == "domain" {
		if gate := d.checkDepGate(ctx, parentID, domain); gate != "" {
			return "", &tool.Result{Error: gate, Category: tool.ResultCategoryValidationRejected}
		}
	}

	// 前序失败打捞（TODO #20 第三层）：同父同 scope 存在 Failed/Cancelled 前任时，
	// 把其打捞摘要追加到新任务文本，机制上保证重派不重复探索。
	// 2026-08-19 扩展到叶子：domain 派发按领域；叶子派发按角色（心跳误杀的叶子重派
	// 此前从零重跑，损失 20 分钟量级）。
	task = d.withPriorSalvage(ctx, parentID, domain, roleID, task)

	// 全局派发总数限额：同一 session 内所有角色的派发合计超过 maxTotalDispatches 时拒绝。
	// 早期实现按 (callerRole->calleeRole) 对计数，实为"每角色最多 N 次"，多文件编排任务
	// 中途即被卡死（实证：meta->code_assistant 5 次烧光后剩余文件无法派发）。
	// 改为 session 级总数，用户发送新消息时重置（ReactService.AddUserMessage）。
	if d.maxTotalDispatches > 0 {
		sessionID := sessionIDFromAgentID(parentID)
		v, _ := d.sessionCounts.LoadOrStore(sessionID, new(atomic.Int64))
		count := v.(*atomic.Int64)
		if count.Add(1) > int64(d.maxTotalDispatches) {
			count.Add(-1)
			return "", &tool.Result{Error: fmt.Sprintf("dispatch total limit reached for session %s (max %d). 派发总数已耗尽，请直接整合已有结果答复用户", sessionID, d.maxTotalDispatches), Category: tool.ResultCategoryValidationRejected}
		}
	}

	// 生成全局唯一的子 Agent ID，格式为 "父ID/角色ID-序号"。
	subAgentID := fmt.Sprintf("%s/%s-%d", parentID, roleID, d.seq.Add(1))

	// tools_hint 预挂载（TODO #52 执行项 4）：父 Agent 建议的工具集 ∩ 子 Agent 角色天花板后
	// 挂载进子 scope，子 Agent 构造的 adapter 据此收窄插件工具可见集——
	// 实现"派 UI 任务时提示用画图插件"而不放权。越界项拒绝并回告父 Agent（日志可查）。
	var hintRejected []string
	if len(toolsHint) > 0 && d.tools != nil {
		if _, rejected := d.tools.MountForScope(subAgentID, roleDef.ID, toolsHint); len(rejected) > 0 {
			hintRejected = rejected
		}
	}

	// 在派发前递增父 Agent 的未决子 Agent 计数，供终结保护消费。
	d.trackChildStart(parentID)

	// 异步启动子 Agent，并立即返回子 Agent ID 作为句柄。
	// 使用 context.Background() 创建独立于父 ctx 的上下文：
	//   - 父会话取消不会波及子 Agent，避免长任务结果丢失。
	//   - timeout>0 时才叠加超时；defer cancel 确保 goroutine 退出时释放上下文资源。
	//   - 继承父 ctx 的 sessionID：子 Agent 工具事件经 handleToolEvent 写入会话日志，
	//     否则事件 SessionID 为空被静默丢弃（参见 service_react.handleToolEvent 的 isRunning 分支）。
	// 派发级墙钟（2026-08-19）：wall_clock_min 是代码级预算，替代 roles.yaml 提示词墙钟
	//（对 LLM 只是软约束，实证验收 Agent 拿 15 分钟预算实际跑 39 分钟）。
	// 取 min(wallClock, d.timeout)：派发级预算不放大全局上限。
	// Domain 侦察墙钟（2026-08-21）：domain 无显式 wall_clock_min 时注入默认预算——
	// 实证领域 Agent 侦察失控（炮塔 1.5h 零交付：15+ 轮慢思考"契约反推"从未派发/写入），
	// reconClock 兜住侦察阶段，中点邮件预警"停止侦察开始产出"。
	effectiveTimeout := d.timeout
	if wallClock <= 0 && roleDef.ID == "domain" && d.domainReconClock > 0 &&
		(effectiveTimeout <= 0 || d.domainReconClock < effectiveTimeout) {
		effectiveTimeout = d.domainReconClock
	}
	if wallClock > 0 && (effectiveTimeout <= 0 || wallClock < effectiveTimeout) {
		effectiveTimeout = wallClock
	}
	taskBrief := truncateRunes(strings.ReplaceAll(strings.TrimSpace(task), "\n", " "), 100)
	started := time.Now()

	// 热驻模式 domain 派发：走 supervisor 常驻 goroutine（idle_pool.go）。
	// ctx 不带 deadline（墙钟由 slot timer 管理，挂起可停表）。
	if d.hotEnabled() && roleDef.ID == "domain" {
		return d.dispatchHotDomain(ctx, parentID, subAgentID, *roleDef, domain, task, responsibility, effectiveTimeout, taskBrief, started)
	}

	subAgentCtx := context.Background()
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	cancel := context.CancelFunc(func() {})
	if effectiveTimeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, effectiveTimeout)
	}
	log.Printf("[subagent] dispatch: parent=%s sub=%s role=%s task=%q", parentID, subAgentID, roleID, taskBrief)
	// 权威树注册：在 goroutine 启动前同步 Register + SetCancel，保证 Cancel 端点不会因时序漏掉句柄。
	// treeFn 为 nil 时（测试场景）跳过，不影响派发主流程。
	if d.treeFn != nil {
		if sid := tool.SessionIDFromContext(ctx); sid != "" {
			if t := d.treeFn(sid); t != nil {
				t.Register(orchestrator.Node{
					ID:       subAgentID,
					ParentID: parentID,
					Role:     roleID,
					Domain:   domain,
					Task:     taskBrief,
					Started:  started,
					Status:   orchestrator.StatusRunning,
				})
				t.SetCancel(subAgentID, cancel)
			}
		}
	}
	// 计划状态回写（TODO #22 Phase 1 补全）：派发即把该领域的计划子任务置为 in_progress，
	// 否则任务只有完成/失败才翻状态，TUI 执行计划面板全程 Waiting、进度 0%。
	d.boardAssign(ctx, parentID, domain, subAgentID)
	// 心跳检活元数据：subMeta 存 cancel/parentID/sessionID/doneOnce 供巡检卡死时兜底。
	// activity：所有非 meta 子 Agent 注册（TODO #25-3 domain 防误杀版）——叶子活动沿
	// parentID 链向上冒泡刷新祖先时间戳，domain 等子/等回信期间靠后代活动保持存活；
	// 自身无 LLM/工具活动且无活跃后代超阈值才判假死。meta 不注册（会话级由用户/墙钟控制）。
	isMeta := roleDef.ID == "meta"
	meta := &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: tool.SessionIDFromContext(ctx), wallClock: effectiveTimeout}
	d.subMeta.Store(subAgentID, meta)
	if !isMeta {
		act := new(atomic.Int64)
		act.Store(time.Now().UnixNano())
		d.activity.Store(subAgentID, act)
	}
	d.ensurePatrol()
	go func() {
		defer cancel()
		defer d.subMeta.Delete(subAgentID)
		defer d.activity.Delete(subAgentID)
		defer d.lastWrites.Delete(subAgentID)
		// paused=true 时(domain 触达 token 上限)不递减父未决计数:父 PendingChildren 保持 >0,
		// 由 MetaAgent PausedChildChecker 检测后主动暂停会话,等用户"继续"恢复。
		// doneOnce 保证与心跳巡检竞争时 trackChildDone 仅触发一次，防双递减。
		paused := d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, task, domain, responsibility, mode, verifyKind, started)
		if !paused {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()

	// 墙钟预警：到期前 grace 窗口向子 Agent 邮箱投递收口警告（子 Agent 主循环 drainMailbox
	// 会读到），避免"预算 15 分钟跑到 39 分钟"无感知硬杀。agent 提前完成时 ctx 被 defer cancel，
	// 定时器随之退出，零泄漏。grace 取 min(2min, 墙钟/4)，墙钟过短（<2min）时跳过预警只硬杀。
	if effectiveTimeout > 0 && d.mailbox != nil {
		grace := effectiveTimeout / 4
		if grace > 2*time.Minute {
			grace = 2 * time.Minute
		}
		if grace >= 30*time.Second {
			go func() {
				select {
				case <-time.After(effectiveTimeout - grace):
				case <-subAgentCtx.Done():
					return
				}
				_, _ = d.mailbox.Send(&mailbox.Message{
					From:    "dispatcher",
					To:      subAgentID,
					Type:    mailbox.MsgInfo,
					Subject: "墙钟预警",
					Body: fmt.Sprintf("【墙钟预警】距执行上限（%v）只剩约 %v，请立即收口：停止继续探索，基于已有产出输出终答。",
						effectiveTimeout, grace),
				})
			}()
		}
	}

	// 侦察中点预警（2026-08-21）：domain 用侦察墙钟（未显式给 wall_clock_min）时，
	// 过半投递"停止侦察开始产出"。实证领域 Agent 把全部预算花在契约反推侦察
	// （读消费点文件 15+ 轮慢思考），到墙钟仍零派发零写入；中点预警在仍余半预算时
	// 把模型推入产出阶段，比终点收口警告多留一半执行时间。ctx 结束即退出，零泄漏。
	if wallClock <= 0 && roleDef.ID == "domain" && d.domainReconClock > 0 && d.mailbox != nil {
		half := effectiveTimeout / 2
		if half >= time.Minute {
			go func() {
				select {
				case <-time.After(half):
				case <-subAgentCtx.Done():
					return
				}
				_, _ = d.mailbox.Send(&mailbox.Message{
					From:    "dispatcher",
					To:      subAgentID,
					Type:    mailbox.MsgInfo,
					Subject: "侦察预算过半",
					Body: fmt.Sprintf("【侦察预算预警】已用约 %v（侦察墙钟 %v 的过半），停止继续侦察：已读文件的结论已足够，立即转入派发叶子/写文件。剩余预算必须全部用于产出。", half, effectiveTimeout),
				})
			}()
		}
	}

	// 返回子 Agent ID 作为句柄，父 Agent 可用该 ID 查询或接收后续通知。
	if len(hintRejected) > 0 {
		// tools_hint 越界项回告父 Agent（TODO #52 验收 (a)：越界委派被拒绝且可查）。
		return subAgentID + "。tools_hint 越界忽略（子 Agent 角色权限天花板外）: " + strings.Join(hintRejected, "; "), nil
	}
	return subAgentID, nil
}

// toolsHintArg 从 args 提取 tools_hint 参数（兼容 []string / []any）。
func (d *Dispatcher) toolsHintArg(args map[string]any) []string {
	raw, ok := args["tools_hint"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// wallClockArg 从 args 提取 wall_clock_min 参数（分钟，JSON number）转为 Duration。
// 缺失/非数值/<=0 返回 0（用全局 sub_agent_timeout）。
func (d *Dispatcher) wallClockArg(args map[string]any) time.Duration {
	v, ok := args["wall_clock_min"].(float64)
	if !ok || v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Minute))
}

// callSubAgentsTool 实现 call_sub_agents 工具：同一波多个子任务一次性原子并行派出。
// 与连续多次 call_sub_agent 等长，但工具形态本身引导"同波一次派出"——
// 实证 v6 基准 MetaAgent 把 4 个建设领域分 2 波串行（第二波晚 24 分钟判负），
// v11 也有 3+1 迷你分波；提示词硬约束对模型只是软约束，批量工具是结构级引导。
type callSubAgentsTool struct {
	dispatcher *Dispatcher // dispatcher 持有调度器引用，工具执行时通过它创建子 Agent。
}

// Name 返回工具名称。
func (t *callSubAgentsTool) Name() string { return "call_sub_agents" }

// Aliases 返回工具别名列表，当前无别名。
func (t *callSubAgentsTool) Aliases() []string { return nil }

// Description 返回工具的 LLM 可见描述。
func (t *callSubAgentsTool) Description() string {
	return "把同一波多个子任务一次性原子并行派出（等价于连续多次 call_sub_agent，但保证同波同时启动）。\n" +
		"多文件创建/多领域拆分任务的**全部建设领域必须用它一次派出**——" +
		"共享契约已钉死集成点，领域产物互为独立文件，后写的不需要等先写的落盘。\n" +
		"参数：tasks 为数组（<=6 项），每项 {role_id, task, domain?, responsibility?, mode?, verify_kind?, tools_hint?}，" +
		fmt.Sprintf("字段规则与 call_sub_agent 一致（domain 角色的 responsibility 必填，task 自包含 <=%d 字；", t.dispatcher.taskRuneSoftLimit) +
		"mode 可选 react/reflection/plan_execute，省略=react；verify_kind 可选 auto/executable/rubric/none，省略=auto；" +
		"tools_hint 可选：建议子 Agent 使用的插件工具名列表，校验 ∩ 子 Agent 权限天花板后预挂载，越界项忽略并回告）；" +
		"wall_clock_min 可选：本次派发的墙钟预算（分钟，代码级强制执行，与全局上限取小，到期前收口警告，验收类任务建议显式给）；" +
		"reuse_agent_id 可选：复用【空闲领域Agent】清单中的热驻领域 Agent（强相关任务优先复用，弱相关新建 domain）。\n" +
		"【前置依赖】与 call_sub_agent 相同：派发前必须先调 WriteSpec 写任务规范（整波共用一份），" +
		"否则返回 spec missing/stale 错误（stale 会列出已变更文件路径）。逐项返回派出结果：某项失败不影响其他项。"
}

// Execute 执行 call_sub_agents 工具调用：逐项校验→spec 校验一次→逐项 dispatchOne。
// 逐项收集结果：失败项不阻塞其他项派出，最终在 Output 中汇总成功/失败清单。
func (t *callSubAgentsTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher

	raw, ok := args["tasks"].([]any)
	if !ok || len(raw) == 0 {
		return &tool.Result{Tool: "call_sub_agents", Error: "tasks is required: 非空数组，每项 {role_id, task, domain?, responsibility?}", Category: tool.ResultCategoryValidationRejected}
	}
	const maxBatch = 6
	if len(raw) > maxBatch {
		return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("batch too large: %d 项（max %d）。超过请合并领域或分批", len(raw), maxBatch), Category: tool.ResultCategoryValidationRejected}
	}

	// 逐项校验参数；轻微超限（2000-2600 runes）软着陆放行并收集警告（TODO #38-3）。
	type batchItem struct {
		roleID, domain, task, responsibility, mode, verifyKind string
		reuseAgentID                                           string
		toolsHint                                              []string
		wallClock                                              time.Duration
	}
	items := make([]batchItem, 0, len(raw))
	var batchWarnings []string
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("tasks[%d] 必须是对象 {role_id, task, ...}", i), Category: tool.ResultCategoryValidationRejected}
		}
		it := batchItem{}
		it.roleID, _ = m["role_id"].(string)
		it.task, _ = m["task"].(string)
		it.domain, _ = m["domain"].(string)
		it.responsibility, _ = m["responsibility"].(string)
		it.mode, _ = m["mode"].(string)
		it.verifyKind, _ = m["verify_kind"].(string)
		it.reuseAgentID, _ = m["reuse_agent_id"].(string)
		it.toolsHint = d.toolsHintArg(m) // TODO #52：建议工具集（∩ 子 Agent 天花板后预挂载）
		it.wallClock = d.wallClockArg(m)
		if msg, warning := t.dispatcher.validateDispatchArgs(it.roleID, it.task, it.responsibility, it.mode, it.verifyKind); msg != "" {
			if it.reuseAgentID == "" {
				return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("tasks[%d]: %s", i, msg), Category: tool.ResultCategoryValidationRejected}
			}
		} else if warning != "" {
			batchWarnings = append(batchWarnings, fmt.Sprintf("tasks[%d]: %s", i, warning))
		}
		items = append(items, it)
	}

	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agents", Error: "missing parent agent context"}
	}
	if msg := d.checkSpecBeforeDispatch(ctx, parentID); msg != "" {
		return &tool.Result{Tool: "call_sub_agents", Error: msg, Category: tool.ResultCategoryValidationRejected}
	}

	var okIDs, errs []string
	for _, it := range items {
		subAgentID, errRes := d.dispatchOne(ctx, it.roleID, it.domain, it.task, it.responsibility, it.mode, it.verifyKind, it.toolsHint, it.wallClock, it.reuseAgentID)
		if errRes != nil {
			errs = append(errs, fmt.Sprintf("%s(%s): %s", it.roleID, it.domain, errRes.Error))
			continue
		}
		okIDs = append(okIDs, subAgentID)
	}
	out := fmt.Sprintf("已并行派出 %d 个子 Agent：%s", len(okIDs), strings.Join(okIDs, ", "))
	if len(errs) > 0 {
		out += fmt.Sprintf("\n未派出 %d 个：%s", len(errs), strings.Join(errs, "；"))
	}
	if len(batchWarnings) > 0 {
		out += "\n警告: " + strings.Join(batchWarnings, "；")
	}
	return &tool.Result{Tool: "call_sub_agents", Success: len(errs) == 0, Output: out}
}

// runSubAgent 为指定角色创建 ReActAgent，驱动其运行，
// 并在完成后将最终结果推送到父 Agent 的邮箱。
// 这是 call_sub_agent 工具的异步执行路径：runSubAgentOnce 纯执行 + notify + 钩子。
// started 为派发起始时间，用于计算耗时并写入完成/失败日志。
// domain 为领域分类简称（仅 role_id="domain" 时有效，用于子 Agent 展示名）。
// responsibility 为职责边界描述，注入 DomainAgent 系统提示词。
// mode 为派发执行模式（react/reflection/plan_execute，TODO #29），穿透到引擎选择。
// 返回 paused=true 表示 DomainAgent 触达 token 上限进入 Paused(已存 history + tree.Pause),
// 调用方不应 trackChildDone(保持父未决计数 >0 触发 MetaAgent 暂停)。
func (d *Dispatcher) runSubAgent(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility, mode, verifyKind string, started time.Time) bool {
	// 会话级 logger 挂 ctx：子 Agent 事实提取（saveBlockMemory）与失败打捞（salvageFailure）
	// 的轻量 LLM 调用经 CallLightweightWithRetry 从 ctx 取 logger 写 session_logs。
	if d.log != nil {
		if sid := tool.SessionIDFromContext(ctx); sid != "" {
			ctx = logger.NewContext(ctx, d.log.WithSession(sid).WithAgent(roleDef.Name))
		}
	}
	result, err, retried := d.runSubAgentWithAutoRetry(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind)
	// Layer 5：从子 Agent 历史扫 WriteFile 调用收集修改文件，随完成通知回灌父 LLM。
	files := agent.FilesModifiedFromHistory(result.History)
	duration := time.Since(started)
	if errors.Is(err, errPaused) {
		// DomainAgent 暂停:history 已存,tree 已 Pause。不 notify、不 treeFinish、不 trackChildDone。
		log.Printf("[subagent] PAUSED: sub=%s role=%s duration=%s (token budget, awaiting resume)", subAgentID, roleDef.ID, duration)
		return true
	}
	if errors.Is(err, errPartialReturn) {
		// 叶子助手部分回灌:treeFinish Done + notify 父部分产出。trackChildDone 照常减。
		partial := result.Text
		log.Printf("[subagent] PARTIAL: sub=%s role=%s duration=%s partial_len=%d", subAgentID, roleDef.ID, duration, len(partial))
		d.treeFinish(ctx, subAgentID, "部分完成: "+partial, nil)
		d.notify(parentID, subAgentID, "子 Agent 已达 token 上限,返回部分完成。\n"+partial, files)
		return false
	}
	if err != nil {
		// 取消路径（会话取消/cancel_agent/心跳杀）：通知与树收尾由取消方负责，
		// 此处跳过避免双通知与覆盖 Cancelled 状态（TODO #25 控制面）。
		if errors.Is(err, context.Canceled) {
			// 软停止分流（TODO #37）：会话软停止标记命中时——
			//   domain：SaveMessages 存完整 history + tree.Pause（可续跑），不 notify 不
			//     trackChildDone（PendingChildren 保持 >0 → 父终结保护 → MetaAgent PausedOnChild，
			//     恢复路由零改动生效）；
			//   叶子助手：无 Pause 语义，部分回灌父（treeFinish Done + notify + trackChildDone），
			//     domain 续跑后按需重派。
			if sid := tool.SessionIDFromContext(ctx); d.isSoftStop(sid) {
				if roleDef.ID == "domain" {
					if d.msgStore != nil && result.History != nil {
						// 本分支的 ctx 已被 StopRunning 取消：SaveMessages 必须用脱离取消的 ctx，
						// 否则 history 存不进去、resume 无消息可加载（e2e 实证 "no persisted messages"）。
						// 附加 10s 超时防慢 PG 阻塞子 Agent 收尾 goroutine。
						saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
						err := d.msgStore.SaveMessages(saveCtx, subAgentID, sid, result.History)
						cancelSave()
						if err != nil {
							log.Printf("[subagent] soft-stop save messages failed: sub=%s err=%v", subAgentID, err)
						}
					}
					if d.treeFn != nil && sid != "" {
						if t := d.treeFn(sid); t != nil {
							t.Pause(subAgentID, "user stop")
						}
					}
					// 唤醒父 MetaAgent 的 wait loop（不改变 PendingChildren 计数）：
					// 否则父要等满 30s wait 周期才检测到 Paused 子节点，PausedOnChild 转换被拖慢。
					d.pokeParent(parentID)
					log.Printf("[subagent] SOFT-STOP PAUSED: sub=%s role=%s duration=%s (awaiting resume)", subAgentID, roleDef.ID, duration)
					return true
				}
				partial := truncateRunes(agent.LastAssistantText(result.History), 500)
				log.Printf("[subagent] SOFT-STOP LEAF PARTIAL: sub=%s role=%s duration=%s partial_len=%d", subAgentID, roleDef.ID, duration, len(partial))
				d.treeFinish(ctx, subAgentID, "软停止部分完成: "+partial, nil)
				d.notify(parentID, subAgentID, "子 Agent 已被软停止（会话停止中），返回当前部分成果；续跑后可按需重派。\n"+partial, files)
				// Domain 热驻：父是热驻 domain 时追加一条取消提示邮件（"记忆补一句：子 Agent 已取消"）——
				// domain 转 Idle 前读邮箱即可感知下属被停止，复用续跑时不误以为叶子仍在执行。
				if d.hotEnabled() && d.mailbox != nil {
					if _, err := d.mailbox.Send(&mailbox.Message{
						From:    "dispatcher",
						To:      parentID,
						Type:    mailbox.MsgInfo,
						Subject: "子 Agent 已取消",
						Body:    fmt.Sprintf("【系统通知】用户已停止当前任务，你的子 Agent %s 已被取消（部分成果已随上条消息回传）。当前任务中断，成果已保留。", subAgentID),
					}); err != nil {
						log.Printf("[subagent] soft-stop notify parent failed: to=%s from=dispatcher err=%v", parentID, err)
					}
				}
				return false
			}
			log.Printf("[subagent] CANCELLED: sub=%s role=%s duration=%s", subAgentID, roleDef.ID, duration)
			return false
		}
		partial := ""
		if result.History != nil {
			partial = truncateRunes(agent.LastAssistantText(result.History), 500)
		}
		log.Printf("[subagent] FAIL: sub=%s role=%s duration=%s retried=%t err=%v partial=%q", subAgentID, roleDef.ID, duration, retried, err, truncateRunes(partial, 200))
		// 失败打捞（TODO #20 第二层）：提取已读文件/已得结论/卡点摘要双路送达——
		// 写共享槽位 <parentID>:salvage:<domain> 供同域重派带前序摘要；追加进父 mailbox 失败消息。
		salvage := d.salvageFailure(ctx, parentID, subAgentID, roleDef, domain, result, partial)
		// 结构化失败（TODO #23）：头部机读标记 [failure kind=X retryable=Y]，人读文案在后。
		kind := failureKindOf(err)
		retryable := kind == FailureKindError && roleDef.ID != "domain" && roleDef.ID != "meta" && ctx.Err() == nil
		msg := failureMarker(kind, retryable) + "\n" + formatSubAgentFailure(err, result, d.effectiveTimeout(subAgentID), partial)
		// 校验分层（TODO #43）两类"未验证/缺证据"：附产出全文供父 Agent 自决
		// （重派/降级/收口）——非"失败"语义，产出可能可用，不能只给 500 字截断。
		if kind == FailureKindUnverified || kind == FailureKindVerifyMissing {
			if t := strings.TrimSpace(result.Text); t != "" {
				msg += "\n\n产出(未验证):\n" + t
			}
			// 已附产出全文，不再追加失败打捞摘要：提取器常整段回传同一答案，
			// 与全文重复（实证 08-13 塔防 verify_missing 正文翻倍）。槽位仍写，供重派用。
		} else if salvage != "" {
			msg += "\n\n" + salvagePrefixMarker + salvage
		}
		d.boardUpdate(ctx, parentID, domain, false, truncateRunes(msg, 300))
		d.treeFinish(ctx, subAgentID, partial, err)
		d.notify(parentID, subAgentID, msg, files)
		return false
	}

	// 成功：把结果摘要通知父 Agent。产出质量由分层自检保证（叶子自检 / 领域整体性验收 /
	// meta 整品验收+返工，见 roles.yaml 提示词），完成路径不再自动派验证 Agent——
	// A/B 实证自动验证闭环是负资产（开 5/16 vs 关 16/16），机制移至扩展设计文档 §12 作后期扩展。
	log.Printf("[subagent] DONE: sub=%s role=%s duration=%s result_len=%d", subAgentID, roleDef.ID, duration, len(result.Text))
	d.boardUpdate(ctx, parentID, domain, true, result.Text)
	d.treeFinish(ctx, subAgentID, result.Text, nil)
	// 校验分层（TODO #43）状态标注：VerifyNote 非空=校验通过（L0 证据/L2 rubric），
	// 完成摘要前缀一行，父 Agent 可见校验依据；空=未启用校验（none），零变化。
	summary := result.Text
	if result.VerifyNote != "" {
		summary = fmt.Sprintf("【校验:通过(%s)】\n%s", result.VerifyNote, result.Text)
	}
	d.notify(parentID, subAgentID, summary, files)
	return false
}

// runSubAgentWithAutoRetry 包装 runSubAgentOnce：叶子助手 kind=error 失败自动重派一次
//（TODO #23 最小一档，同任务同前缀，fresh 计数）。domain/timeout/killed/loop_guard/
// 预算部分返回不自动重试——domain 交 MetaAgent 决策、墙钟类重试无意义，避免放大故障。
// 与 LLM 调用层重试（react_agent retry_count）正交：那层重试模型调用本身，这层重跑整个 Agent。
// mode 为派发执行模式，自动重派沿用同一模式。
// 返回 (result, err, retried)：retried=true 表示本轮失败已重试过一次（二次失败终报）。
func (d *Dispatcher) runSubAgentWithAutoRetry(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility, mode, verifyKind string) (agent.ReactResult, error, bool) {
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind)
	if err == nil || d.dispatchRetryCount <= 0 {
		return result, err, false
	}
	kind := failureKindOf(err)
	retryable := kind == FailureKindError && roleDef.ID != "domain" && roleDef.ID != "meta" && ctx.Err() == nil
	if !retryable {
		return result, err, false
	}
	log.Printf("[subagent] AUTO-RETRY: sub=%s role=%s err=%v (dispatch retry %d)", subAgentID, roleDef.ID, err, d.dispatchRetryCount)
	_, result2, err2 := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind)
	return result2, err2, true
}

// treeFinish 把子 Agent 终态写入权威树。treeFn 为 nil 或 sessionID 缺失时静默跳过。
// 幂等：orchestrator.Tree.Finish 对已 terminal 节点 no-op。
func (d *Dispatcher) treeFinish(ctx context.Context, subAgentID, summary string, err error) {
	if d.treeFn == nil {
		return
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		return
	}
	if t := d.treeFn(sid); t != nil {
		t.Finish(subAgentID, summary, err)
	}
}

// runSubAgentOnce 纯执行路径：创建子 Agent、注入块记忆、驱动 Run、校验分层（TODO #43）、沉淀块记忆，
// 返回子 Agent 实例 + 完整结果 + 错误。不 notify、不触发钩子、不进入实例池。
// 供异步 runSubAgent 包装器与同步 ExecuteChild（编排器）共用。
//
// mode 为派发执行模式（react/reflection/plan_execute，TODO #29）：
// 空串/未知值走默认 ReAct 引擎；reflection/plan_execute 由 runEngine 按引擎包装。
// verifyKind 为校验分层（auto/executable/rubric/none，TODO #43）：auto 按角色/模式解析；
// executable 完成后扫可执行验证证据（缺证据反馈重试 1 轮）；rubric 强制交叉模型 judge 引擎。
//
// 错误语义：
//   - 获取 provider 失败、Run 返回 error、LimitReached 均返回非 nil err；
//   - errVerifyMissing：L0 校验缺验证证据（重试 1 轮后仍缺）；
//   - errUnverified：judge LLM 不可用（fail-closed，result.Unverified=true）；
//   - 成功时 err == nil，result.Text 为最终答复。
func (d *Dispatcher) runSubAgentOnce(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility, mode, verifyKind string) (*agent.ReActAgent, agent.ReactResult, error) {
	provider, err := d.models.GetBladesProvider(ctx, roleDef.ID)
	if err != nil {
		return nil, agent.ReactResult{}, fmt.Errorf("get model: %w", err)
	}

	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}

	// 按角色 Tools 白名单过滤暴露给子 Agent 的工具：
	// - DomainAgent 只见 call_sub_agent（继续拆分到函数级派发）；
	// - 固定助手只见 ReadFile/WriteFile/RunCommand 等执行类工具，不再能 call_sub_agent（叶子执行者）。
	// Dispatch 路径不受白名单限制，verifyloop 的 ExecuteChild 仍可直接调任意工具。
	//
	// DomainAgent 是合成角色，roleDef.Name 固定为 "DomainAgent"，所有 DomainAgent 实例无法区分；
	// 优先用 LLM 提供的 domain 字段命名（如"金融"->"金融领域Agent"），无则按 task 首行兜底，
	// 让日志/对话页一眼看出在做什么领域。固定助手用 roleDef.Name（"代码助手"/"UI助手" 等），不覆写。
	if roleDef.ID == "domain" {
		if hint := strings.TrimSpace(domain); hint != "" {
			roleDef.Name = textutil.TruncateRunes(hint, 16, "…") + "领域Agent"
		} else {
			firstLine := strings.SplitN(strings.TrimSpace(task), "\n", 2)[0]
			if hint := textutil.TruncateRunes(firstLine, 16, "…"); hint != "" {
				roleDef.Name = "领域Agent:" + hint
			}
		}
		// 职责槽注入：responsibility 非空时在通用领域 prompt 末尾加身份头。
		// task 文本在长 ReAct 循环中会被历史压缩摘要掉，system prompt 不会，
		// 领域身份钉在系统提示词里防止跑偏（实证：领域 Agent 越界实现他域文件）。
		// 置于末尾而非开头（2026-08-15 缓存优化）：开头是逐领域分叉点，会把
		// envBlock + 通用领域 prompt 的公共前缀截断在几百 token；挪到末尾后
		// 所有领域 Agent 的 system 前缀逐字节一致，DeepSeek/glm 前缀缓存可跨
		// 领域复用整个通用 prompt（公共前缀规则：2 次落盘、第 3 次起命中）。
		// 末尾紧邻首条 user 消息，处于注意力近因区，身份约束力不降。
		if resp := strings.TrimSpace(responsibility); resp != "" {
			domainLabel := strings.TrimSpace(domain)
			if domainLabel == "" {
				domainLabel = "综合"
			}
			header := fmt.Sprintf("你是负责【%s】领域的 DomainAgent。\n你的职责：%s\n"+
				"只实现/改写职责内的文件与模块；职责外的文件禁止创建或修改，"+
				"需要的跨领域数据从共享记忆契约或 ReadFile 读取。",
				textutil.TruncateRunes(domainLabel, 16, "…"), textutil.TruncateRunes(resp, 200, "…"))
			roleDef.SystemPrompt = roleDef.SystemPrompt + "\n\n" + header
		}
	}
	// 不注入用户级人格（soul.md"多 Agent 编排助手"）：人格前缀首行即编排者身份，
	// 子 Agent（领域/叶子）读到的第一身份是"编排助手"，与角色提示词冲突，
	// thinking 模型据此长期停留在"等待兄弟回传"的编排者叙事里空转
	// （实证 2026-08-13：三个领域 Agent 与叶子 code_assistant 的思考流全是
	// "三个领域 Agent 已成功派发，等待回传"，3 参数改动跑 12 分钟）。
	// 子 Agent 身份只由角色提示词（叶子=执行者/领域=领域负责人）定义。
	// 黑板模式（TODO #42）每轮兄弟产出摄取：仅 DomainAgent + 非空 domain + searcher 实现
	// BlackboardSearcher 时包装记忆流水线，每轮 Assemble 末尾按 scope 查询兄弟产出注入【兄弟产出】段
	// （等待兄弟时见其完成结论，替"等待回传"叙事空转）。持指针供播种召回后 seedSeen 去重。
	var uptake *siblingUptakePipeline
	if roleDef.ID == "domain" && strings.TrimSpace(domain) != "" {
		if bb, ok := d.searcher.(BlackboardSearcher); ok {
			uptake = newSiblingUptakePipeline(mem, bb, tool.SessionIDFromContext(ctx), parentID, domain, subAgentID)
			mem = uptake
		}
	}
	sub := agent.NewReActAgent(subAgentID, roleDef, provider, agent.NewToolRegistryAdapterForRole(d.tools, subAgentID, roleDef.Tools, roleDef.ID, d.pluginVisibility)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor(roleDef.ID)).
		WithWorkDir(d.subAgentWorkDir())
	// 注入未决子 Agent 检查器：子 Agent 也能递归派发（domain -> 叶子助手），
	// 无此检查时子 Agent 会在派发后立刻给出中间汇报式终答（不等待 mailbox），
	// 父链路上的 Agent 会把“中间状态”误当最终结果（实证：domain-1 拆两个子任务后
	// 直接 DONE，MetaAgent 把“等待 mailbox 结果”当终答，会话 completed 但产出缺失）。
	// Dispatcher 自身实现 PendingChildrenChecker（PendingChildren/WaitForAnyChild）。
	sub = sub.WithPendingChildrenChecker(d)

	// 心跳检活：注入活动上报回调（generateOnce/工具派发时 Store 当前时间，巡检据此判假死）。
	// 回调同时把活动沿 parentID 链向上冒泡（TODO #25-3）：domain 等子期间自身无活动，
	// 靠后代活动刷新保持存活，巡检不误杀合法等待。
	if actVal, ok := d.activity.Load(subAgentID); ok {
		act := actVal.(*atomic.Int64)
		sub = sub.WithActivityReporter(func() {
			now := time.Now().UnixNano()
			act.Store(now)
			d.bubbleActivity(subAgentID, now)
		})
	}

	// 注入会话级日志器：派生 session-scoped logger，使子 Agent LLM I/O 写入同一会话的 session_logs。
	// sessionID 从 ctx 取（call_sub_agent 异步路径已 WithSessionID），agentName 用 roleDef.Name（DomainAgent 已按任务首行覆写）。
	sid := tool.SessionIDFromContext(ctx)
	if d.log != nil {
		sub = sub.WithLogger(d.log.WithSession(sid).WithAgent(roleDef.Name))
	}
	// 注入实时事件转发器：子 Agent emitLive 时按 sessionID 路由回会话 service，
	// 使子 Agent token 用量计入会话累计（TUI/Web 总和展示）。
	// 转发前拦截 WriteFile/EditFile 调用事件记录盘上产物（kill 时回告父级）。
	if d.liveFn != nil && sid != "" {
		forwarder := d.liveFn
		sub = sub.WithLiveEvents(func(ev agent.LiveEvent) {
			d.recordFileWrite(subAgentID, ev)
			forwarder(sid, ev)
		})
	}

	d.running.Store(subAgentID, sub)
	defer d.running.Delete(subAgentID)
	defer func() {
		if d.mailbox != nil {
			d.mailbox.Purge(subAgentID)
		}
	}()

	// 保留原始任务文本，供块记忆沉淀时作为 goal 标签使用（避免混入召回前缀）。
	origTask := task
	// 上下文前缀注入：共享记忆（spec + 自由槽位）+ 块记忆召回。两段独立前缀统一拼装，避免嵌套
	// 【当前任务】标记（实证：嵌套后 UI 助手把 KV 内容当作任务主体，空转 16 分钟）。
	var prefixes []string
	if sp := d.buildSharedPrefix(ctx, parentID); sp != "" {
		prefixes = append(prefixes, sp)
	}
	if bm, recs := d.injectScopedRecall(ctx, parentID, domain, origTask, ""); bm != "" {
		prefixes = append(prefixes, bm)
		log.Printf("[subagent] inject block-memory: sub=%s role=%s hits=%d task_len=%d",
			subAgentID, roleDef.ID, len(recs), len(origTask))
		for i, rec := range recs {
			domain := ""
			if rec.Meta != nil {
				if v, ok := rec.Meta["domain"].(string); ok {
					domain = v
				}
			}
			log.Printf("[subagent]   hit[%d] domain=%s goal=%q content=%q",
				i+1, domain, truncateRunes(fmt.Sprintf("%v", rec.Meta["goal"]), 80), truncateRunes(strings.TrimSpace(rec.Content), 200))
		}
		// seed uptake wrapper 的 seen：播种召回的事实标为已见，防每轮摄取重复注入同一条。
		if uptake != nil {
			uptake.seedSeen(recs)
		}
	}
	// 领域标签前缀：与系统提示词职责槽互补（prompt 管长效，前缀管当下），
	// 让子 Agent 每次读任务时都看到自己的领域归属。
	if roleDef.ID == "domain" {
		if label := strings.TrimSpace(domain); label != "" {
			task = "【你的领域】" + textutil.TruncateRunes(label, 16, "…") + "\n" + task
		}
	}
	// 统一拼装前缀与原任务：单一【当前任务】标记，避免嵌套混淆模型。
	if len(prefixes) > 0 {
		task = strings.Join(prefixes, "\n\n") + "\n\n【当前任务】\n" + task
	}

	vk := resolveVerifyKind(verifyKind, roleDef.ID, mode)
	result, err := d.runEngine(ctx, sub, subAgentID, roleDef.ID, mode, vk, task)
	if err != nil {
		return sub, result, fmt.Errorf("run: %w", err)
	}

	// 校验分层（TODO #43）fail-closed：judge LLM 不可用时 result.Unverified=true，
	// 上抛 errUnverified（绝不静默放行——旧 fail-open 使 reflection 自检形同虚设）。
	if result.Unverified {
		return sub, result, errUnverified
	}

	// L0 可执行校验：验证类命令成功执行的客观证据扫描（零 LLM 零执行）。
	// 缺证据 → 1 轮反馈重试（"终答前必须运行验证命令"）→ 仍缺 → errVerifyMissing 报父。
	if vk == verifyKindExecutable {
		if !agent.HasExecutableVerification(result.History) {
			log.Printf("[subagent] verify L0 missing evidence: sub=%s role=%s (retry 1 round)", subAgentID, roleDef.ID)
			result, err = sub.RunWithHistory(ctx, l0RetryMessage, result.History)
			if err != nil {
				return sub, result, fmt.Errorf("run: %w", err)
			}
			if result.LimitReached {
				return sub, result, errVerifyMissing
			}
			if !agent.HasExecutableVerification(result.History) {
				log.Printf("[subagent] verify L0 still missing: sub=%s role=%s", subAgentID, roleDef.ID)
				return sub, result, errVerifyMissing
			}
		}
		result.VerifyNote = "L0 证据"
	}

	if result.LimitReached {
		if roleDef.ID == "domain" {
			// DomainAgent 触达 token 上限:存完整 history + tree.Pause,不 notify 父、不 trackChildDone。
			// 父 MetaAgent 经 PausedChildChecker 检测后主动暂停会话;用户"继续"时 resumePausedDomain
			// 从 agent_messages 加载 history 续跑(resume 重置 budget 给新 50K)。
			sid := tool.SessionIDFromContext(ctx)
			if d.msgStore != nil && sid != "" {
				if err := d.msgStore.SaveMessages(ctx, subAgentID, sid, result.History); err != nil {
					log.Printf("[subagent] PAUSE save messages failed: sub=%s err=%v", subAgentID, err)
				}
			}
			if d.treeFn != nil && sid != "" {
				if t := d.treeFn(sid); t != nil {
					t.Pause(subAgentID, "token budget exhausted")
				}
			}
			log.Printf("[subagent] PAUSED: sub=%s role=domain (token budget, history persisted)", subAgentID)
			return sub, result, errPaused
		}
		// 叶子助手触达 token 上限:不持久化,把部分产出塞 result.Text 返回给父 mailbox + 标 Done。
		partial := truncateRunes(agent.LastAssistantText(result.History), 500)
		result.Text = partial
		d.saveBlockMemory(ctx, subAgentID, roleDef.ID, parentID, domain, origTask, partial, blockOutcomePartial, agent.FilesModifiedFromHistory(result.History))
		log.Printf("[subagent] PARTIAL: sub=%s role=%s (token budget, partial returned)", subAgentID, roleDef.ID)
		return sub, result, errPartialReturn
	}

	_ = mem.Write(subAgentID, agent.MemoryEvent{
		Type:    "task_goal_summary",
		AgentID: subAgentID,
		Role:    roleDef.ID,
		Content: result.Text,
	})

	d.saveBlockMemory(ctx, subAgentID, roleDef.ID, parentID, domain, origTask, result.Text, blockOutcomeSuccess, agent.FilesModifiedFromHistory(result.History))

	return sub, result, nil
}

// runEngine 按派发执行模式（TODO #29）选择引擎驱动子 Agent：
//   - react（默认/空串）= 裸 ReAct 主循环（零行为变化）；
//   - reflection = ReflectEngine：产出后对照验收标准自检，不达标带反馈重试；
//   - plan_execute = PlanExecuteEngine：先出步骤计划（落 board）再逐步执行。
//
// 引擎辅助 LLM（自检/规划）从模型工厂取同角色 provider 适配；缺失/失败时
// 引擎内部 fail-open 降级为纯 ReAct，不阻塞派发主流程。
func (d *Dispatcher) runEngine(ctx context.Context, sub *agent.ReActAgent, subAgentID, roleID, mode, verifyKind, task string) (agent.ReactResult, error) {
	// reflection 模式或显式 rubric 校验：交叉模型 judge 引擎（TODO #43）。
	// verifyKind=rubric 覆盖 mode（react+rubric = 完成后 judge 评审）。
	if mode == agent.ModeReflection || verifyKind == verifyKindRubric {
		return agent.NewReflectEngine(sub, agent.EngineOptions{
			LLM:                 d.engineLLMForJudge(ctx, roleID, subAgentID),
			MaxReflectionRounds: d.reflectionMaxRounds,
		}).Run(ctx, task)
	}
	switch mode {
	case agent.ModePlanExecute:
		return agent.NewPlanExecuteEngine(sub, agent.EngineOptions{
			LLM:          d.engineLLM(ctx, roleID, subAgentID),
			PlanMaxSteps: d.planMaxSteps,
			PlanSink:     d.planSink(ctx, subAgentID),
			PlanProgress: d.planProgress(ctx, subAgentID),
		}).Run(ctx, task)
	default:
		return sub.Run(ctx, task)
	}
}

// engineLLMForJudge 构造校验 judge 的 LLMComplete（TODO #43 交叉模型）：
// 优先取配置 judgeRole（默认 prompt_reviewer，与被审角色不同模型/供应商——同模型自评
// 偏放水/幻觉 pass）；judgeRole 取不到（mock 测试/角色未注册）回退同角色 provider，
// 两者都失败返回恒 err 的 LLMComplete——ReflectEngine 收到错误走 fail-closed（Unverified），
// 绝不静默放行。
func (d *Dispatcher) engineLLMForJudge(ctx context.Context, roleID, subAgentID string) agent.LLMComplete {
	if d.judgeRole != "" && d.judgeRole != roleID {
		if p, err := d.models.GetBladesProvider(ctx, d.judgeRole); err == nil {
			return d.wrapEngineLLM(subAgentID, agent.NewEngineLLM(p))
		}
	}
	if p, err := d.models.GetBladesProvider(ctx, roleID); err == nil {
		return d.wrapEngineLLM(subAgentID, agent.NewEngineLLM(p))
	}
	return func(ctx context.Context, prompt string) (string, error) {
		return "", fmt.Errorf("judge provider unavailable for role=%s judge=%s", roleID, d.judgeRole)
	}
}

// engineLLMKeepalive 是引擎辅助 LLM 调用期间的心跳保活间隔（thinking 模型 judge
// 单次生成可达数分钟，期间子 Agent 无 LLM/工具活动，不保活会被巡检误判假死）。
const engineLLMKeepalive = 30 * time.Second

// wrapEngineLLM 为引擎辅助 LLM 调用（自检 judge / plan_execute 规划）加超时与心跳保活。
// 两个盲区一起堵（2026-08-19 引擎 Agent 70 分钟事故）：
//  1. 超时：该路径无 CallLLM 包装，SDK 默认 600s/请求 × provider 3 次重试 × judge 内部
//     重试叠加可烧 ~70 分钟；外层 WithTimeout 包住整次调用（含 provider 重试），到期后
//     后续重试因 ctx 已耗尽立即失败（快速失败，不再重试-再超时循环）。
//  2. 心跳：engineLLMStream 流式消费不 touchActivity，judge 长生成期间子 Agent 零活动
//     会被巡检按假死杀掉；保活定时器补上报（真实挂死由墙钟兜底，不无限续命）。
func (d *Dispatcher) wrapEngineLLM(subAgentID string, inner agent.LLMComplete) agent.LLMComplete {
	return func(ctx context.Context, prompt string) (string, error) {
		if d.engineLLMTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d.engineLLMTimeout)
			defer cancel()
		}
		if subAgentID == "" {
			return inner(ctx, prompt)
		}
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(engineLLMKeepalive)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					now := time.Now().UnixNano()
					if av, ok := d.activity.Load(subAgentID); ok {
						av.(*atomic.Int64).Store(now)
					}
					d.bubbleActivity(subAgentID, now)
				}
			}
		}()
		return inner(ctx, prompt)
	}
}

// verifyKind 取值（TODO #43 校验分层，call_sub_agent 的 verify_kind 参数）。
const (
	verifyKindAuto       = "auto"
	verifyKindExecutable = "executable"
	verifyKindRubric     = "rubric"
	verifyKindNone       = "none"
)

// resolveVerifyKind 解析校验分层（TODO #43）：显式值优先；auto/空串按任务形态推断——
// reflection 模式 → rubric（引擎内 judge 即校验）；代码/测试/评审固定角色 → executable
// （L0 客观证据）；其余（domain 编排/叶子默认 react）→ none（不加校验成本）。
func resolveVerifyKind(verifyKind, roleID, mode string) string {
	switch verifyKind {
	case verifyKindExecutable, verifyKindRubric, verifyKindNone:
		return verifyKind
	}
	if mode == agent.ModeReflection {
		return verifyKindRubric
	}
	switch roleID {
	case "code_assistant", "test_assistant", "code_reviewer":
		return verifyKindExecutable
	}
	return verifyKindNone
}

// l0RetryMessage 是 L0 校验缺验证证据时的 1 轮反馈重试指令。
const l0RetryMessage = "【验证要求】终答前必须运行验证命令（测试/lint/build 检查，如 node test.js、go test ./...、npm run lint），并依据结果修正问题。请运行验证命令后重新产出最终答复。"

// engineLLM 构造引擎辅助 LLM（自检/规划）：从模型工厂取同角色 provider 适配为文本补全。
// 取 provider 失败时返回的 LLMComplete 每次调用报错，引擎 fail-open 降级为纯 ReAct。
func (d *Dispatcher) engineLLM(ctx context.Context, roleID, subAgentID string) agent.LLMComplete {
	return func(ctx context.Context, prompt string) (string, error) {
		p, err := d.models.GetBladesProvider(ctx, roleID)
		if err != nil {
			return "", err
		}
		return d.wrapEngineLLM(subAgentID, agent.NewEngineLLM(p))(ctx, prompt)
	}
}

// planTaskID 推导 plan_execute 步骤的看板任务 ID（subAgentID 前缀防与 MetaAgent write_plan 冲突）。
func planTaskID(subAgentID string, stepIdx int) string {
	prefix := strings.ReplaceAll(subAgentID, "/", "_")
	return fmt.Sprintf("%s_p%d", prefix, stepIdx)
}

// planSink 把 plan_execute 引擎的步骤计划写入会话看板（TUI 可见，TODO #29）。
// 任务 ID 用 subAgentID 前缀防与 MetaAgent write_plan 冲突；board 不可用时静默跳过。
// 步骤不设 domain：plan_execute 步骤由执行 Agent 内部逐步跑，不经派发依赖门。
// SetPlan 传空 goal：不覆盖 MetaAgent write_plan 已写入的全局目标（仅新建看板时用本任务作 goal）。
func (d *Dispatcher) planSink(ctx context.Context, subAgentID string) func(goal string, steps []agent.PlanStep) error {
	return func(goal string, steps []agent.PlanStep) error {
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" || d.boardFnCreate == nil || len(steps) == 0 {
			return nil
		}
		b := d.boardFnCreate(sid, goal)
		if b == nil {
			return nil
		}
		tasks := make([]board.PlanTask, 0, len(steps))
		for i, st := range steps {
			tasks = append(tasks, board.PlanTask{
				ID:    planTaskID(subAgentID, i+1),
				Title: st.Title,
			})
		}
		return b.SetPlan("", tasks)
	}
}

// planProgress 把 plan_execute 单步执行状态回写看板（TODO #22 面板实时化）：
// 起步 Assign 置 in_progress，完成/失败 MarkDone/MarkFailed。此前步骤任务落板后永不迁移，
// 执行计划面板全程 Waiting、总体进度 0%（实证：验收会话 20 项计划 3.9 小时全 Waiting）。
// 无计划/任务不存在静默跳过（零行为变化）。
func (d *Dispatcher) planProgress(ctx context.Context, subAgentID string) func(stepIdx int, status agent.PlanStepStatus, summary string) {
	return func(stepIdx int, status agent.PlanStepStatus, summary string) {
		if d.boardFn == nil {
			return
		}
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" {
			if i := strings.Index(subAgentID, "/"); i > 0 {
				sid = subAgentID[:i]
			}
		}
		b := d.boardFn(sid)
		if b == nil {
			return
		}
		taskID := planTaskID(subAgentID, stepIdx)
		switch status {
		case agent.PlanStepInProgress:
			_ = b.Assign(taskID, subAgentID)
		case agent.PlanStepDone:
			_ = b.MarkDone(taskID, summary)
		case agent.PlanStepFailed:
			_ = b.MarkFailed(taskID, summary)
		}
	}
}

// ResumePaused 恢复一个因触达上下文 token 上限而 Paused 的 DomainAgent。
// 从 msgStore 加载其完整消息历史，用 fresh budget（上下文 token 每轮独立估算）重建
// domain ReActAgent 续跑。不强制压缩——靠 Assemble 压缩自动触发（Pipeline 状态延续：
// 同 subAgentID -> compressCounters/events 跨 resume 保留）。
//
// 生命周期：
//   - 再触限：SaveMessages 覆盖 + tree.Pause + 返回 result.LimitReached=true（ReactService 置会话 PausedOnChild）。
//   - 完成：tree.Finish Done + notify 父 mailbox + trackChildDone（父 MetaAgent 解除阻塞）。
//   - 出错：treeFinish Failed + notify 失败 + trackChildDone + 返回 err。
//
// pausedNodeID 为 Paused 节点 ID；parentID 从节点 ParentID 取。
// 前置缺失（sid/treeFn/msgStore 为空）或加载空时返回错误，调用方应回退 pauseSession 不跑（防丢上下文）。
func (d *Dispatcher) ResumePaused(ctx context.Context, pausedNodeID string) (agent.ReactResult, error) {
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" || d.treeFn == nil || d.msgStore == nil {
		return agent.ReactResult{}, fmt.Errorf("resume prerequisites not met (sid/tree/msgStore)")
	}
	t := d.treeFn(sid)
	if t == nil {
		return agent.ReactResult{}, fmt.Errorf("tree not found for session %s", sid)
	}
	var pausedNode orchestrator.Node
	found := false
	for _, n := range t.Snapshot() {
		if n.ID == pausedNodeID {
			pausedNode = n
			found = true
			break
		}
	}
	if !found {
		return agent.ReactResult{}, fmt.Errorf("paused node not found: %s", pausedNodeID)
	}
	if pausedNode.Role != "domain" {
		return agent.ReactResult{}, fmt.Errorf("paused node %s is not a domain agent (role=%s)", pausedNodeID, pausedNode.Role)
	}
	parentID := pausedNode.ParentID

	// 续跑次数上限：续跑会重置 fresh budget，若不设上限，"触限-暂停-续跑"环路永不绑定
	// （v10 实证：验收领域研磨 32 轮 30 分钟不收敛）。触顶后强制收口部分返回，
	// 返回 (result, nil) 使上层 resumePausedDomain 走"完成"分支——MetaAgent drain mailbox
	// 整合部分产出并决定返工，与叶子助手 errPartialReturn 同哲学。
	maxRes := d.maxPausedResumes
	if maxRes <= 0 {
		maxRes = 1
	}
	resumeCntV, _ := d.pausedResumes.LoadOrStore(pausedNodeID, new(atomic.Int64))
	resumeCnt := resumeCntV.(*atomic.Int64)
	if resumeCnt.Load() >= int64(maxRes) {
		return d.concludePaused(ctx, pausedNode, maxRes)
	}
	resumeCnt.Add(1)

	msgs, err := d.msgStore.LoadMessages(ctx, pausedNodeID)
	if err != nil {
		return agent.ReactResult{}, fmt.Errorf("load messages: %w", err)
	}
	if len(msgs) == 0 {
		return agent.ReactResult{}, fmt.Errorf("no persisted messages for %s", pausedNodeID)
	}

	roleDef := d.registry.Get("domain")
	if roleDef == nil {
		return agent.ReactResult{}, fmt.Errorf("domain role not found")
	}
	if hint := strings.TrimSpace(pausedNode.Domain); hint != "" {
		roleDef.Name = textutil.TruncateRunes(hint, 16, "…") + "领域Agent"
	}

	provider, err := d.models.GetBladesProvider(ctx, "domain")
	if err != nil {
		return agent.ReactResult{}, fmt.Errorf("get model: %w", err)
	}
	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}
	// 黑板模式（TODO #42）：resume 的 domain Agent 同样每轮摄取兄弟产出--它正是因等兄弟而暂停的，
	// 恢复后兄弟可能已完成，uptake 让它立即见到兄弟结论而非空等/重做。
	if bb, ok := d.searcher.(BlackboardSearcher); ok && strings.TrimSpace(pausedNode.Domain) != "" {
		mem = newSiblingUptakePipeline(mem, bb, sid, pausedNode.ParentID, pausedNode.Domain, pausedNodeID)
	}
	// 不注入编排者人格（理由同 runSubAgentOnce：身份混淆实证）。
	sub := agent.NewReActAgent(pausedNodeID, *roleDef, provider, agent.NewToolRegistryAdapterForRole(d.tools, pausedNodeID, roleDef.Tools, roleDef.ID, d.pluginVisibility)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor("domain")).
		WithWorkDir(d.subAgentWorkDir())
	// 同 runSubAgentOnce：resume 重建的 domain Agent 也可能继续递归派发，
	// 需要终结保护等待自己的子 Agent（Dispatcher 自身实现 PendingChildrenChecker）。
	sub = sub.WithPendingChildrenChecker(d)
	if d.log != nil {
		sub = sub.WithLogger(d.log.WithSession(sid).WithAgent(roleDef.Name))
	}
	if d.liveFn != nil {
		forwarder := d.liveFn
		sub = sub.WithLiveEvents(func(ev agent.LiveEvent) { forwarder(sid, ev) })
	}

	subCtx := context.Background()
	subCtx = tool.WithSessionID(subCtx, sid)
	cancel := context.CancelFunc(func() {})
	if d.timeout > 0 {
		subCtx, cancel = context.WithTimeout(subCtx, d.timeout)
	}
	t.Resume(pausedNodeID, cancel)
	d.running.Store(pausedNodeID, sub)
	// 心跳（TODO #25-3）：resume 的 domain 注册 activity，其子 Agent 活动冒泡保活；
	// 巡检超 domain 阈值判假死。defer 清理与子 Agent 派发路径一致。
	act := new(atomic.Int64)
	act.Store(time.Now().UnixNano())
	d.activity.Store(pausedNodeID, act)
	defer d.activity.Delete(pausedNodeID)
	defer d.running.Delete(pausedNodeID)
	defer cancel()
	defer func() {
		if d.mailbox != nil {
			d.mailbox.Purge(pausedNodeID)
		}
	}()

	log.Printf("[subagent] resume: sub=%s parent=%s domain=%s msgs=%d", pausedNodeID, parentID, pausedNode.Domain, len(msgs))

	result, err := sub.RunWithHistory(subCtx, "继续", msgs)
	files := agent.FilesModifiedFromHistory(result.History)
	if err != nil {
		partial := truncateRunes(agent.LastAssistantText(result.History), 500)
		log.Printf("[subagent] resume FAIL: sub=%s err=%v", pausedNodeID, err)
		d.treeFinish(subCtx, pausedNodeID, partial, err)
		d.notify(parentID, pausedNodeID, formatSubAgentFailure(err, result, d.timeout, partial), files)
		d.trackChildDone(parentID)
		return result, err
	}
	if result.LimitReached {
		if saveErr := d.msgStore.SaveMessages(subCtx, pausedNodeID, sid, result.History); saveErr != nil {
			log.Printf("[subagent] resume re-pause save messages failed: sub=%s err=%v", pausedNodeID, saveErr)
		}
		t.Pause(pausedNodeID, "token budget exhausted (resume)")
		log.Printf("[subagent] resume RE-PAUSED: sub=%s (token budget)", pausedNodeID)
		return result, nil
	}

	log.Printf("[subagent] resume DONE: sub=%s result_len=%d", pausedNodeID, len(result.Text))
	d.treeFinish(subCtx, pausedNodeID, result.Text, nil)
	d.notify(parentID, pausedNodeID, result.Text, files)
	d.trackChildDone(parentID)
	return result, nil
}

// concludePaused 在 Paused domain 触达续跑上限时强制收口：
// 从 msgStore 加载已持久化历史提取部分产出（LastAssistantText 截 500 字），
// tree.Finish 标 Done + notify 父 mailbox（文案明确标注"触限强制收口"）+ trackChildDone + 清邮箱。
// 与 ResumePaused 的完成分支同构；返回 (result, nil) 让上层走"完成"路径恢复 MetaAgent。
func (d *Dispatcher) concludePaused(ctx context.Context, pausedNode orchestrator.Node, maxRes int) (agent.ReactResult, error) {
	sid := tool.SessionIDFromContext(ctx)
	pausedNodeID := pausedNode.ID
	parentID := pausedNode.ParentID

	var partial string
	var files []string
	if msgs, err := d.msgStore.LoadMessages(ctx, pausedNodeID); err == nil && len(msgs) > 0 {
		partial = truncateRunes(agent.LastAssistantText(msgs), 500)
		files = agent.FilesModifiedFromHistory(msgs)
	}
	note := fmt.Sprintf("子 Agent 已达续跑上限（%d 次）仍未收敛，已强制收口并部分返回；请据部分产出决定返工或接手", maxRes)
	text := note + "。" + partialSuffix(partial)

	subCtx := context.Background()
	if sid != "" {
		subCtx = tool.WithSessionID(subCtx, sid)
	}
	d.treeFinish(subCtx, pausedNodeID, text, nil)
	d.notify(parentID, pausedNodeID, text, files)
	d.trackChildDone(parentID)
	if d.mailbox != nil {
		d.mailbox.Purge(pausedNodeID)
	}
	log.Printf("[subagent] CONCLUDED: sub=%s parent=%s (resume cap %d reached, partial len=%d)", pausedNodeID, parentID, maxRes, len(partial))
	return agent.ReactResult{Text: text}, nil
}

// FailureKind 是子 Agent 失败的结构化类型（TODO #23），随父 mailbox 失败消息的
// 机读标记 [failure kind=X retryable=Y] 透出，供上层（board/调度）按类型决策。
type FailureKind string

const (
	// FailureKindTimeout 超时终止（sub_agent_timeout 墙钟）。
	FailureKindTimeout FailureKind = "timeout"
	// FailureKindError 通用执行错误（模型/运行错误，未知分类的兜底）。
	FailureKindError FailureKind = "error"
	// FailureKindBudget 触达 token 预算的部分返回（errPartialReturn）。
	FailureKindBudget FailureKind = "budget_partial"
	// FailureKindKilled 被心跳巡检判定假死主动取消。
	FailureKindKilled FailureKind = "killed"
	// FailureKindLoopGuard 被循环守卫（连读/探索预算/连败）终止。
	FailureKindLoopGuard FailureKind = "loop_guard"
	// FailureKindUnverified 校验 judge LLM 不可用，结论未验证（TODO #43 fail-closed）。
	// 非"失败"而是"无法判定"：retryable=false，父 Agent 自决（重派/降级/收口）。
	FailureKindUnverified FailureKind = "unverified"
	// FailureKindVerifyMissing L0 可执行校验缺验证证据（无成功运行的测试/lint/--check，
	// 重试 1 轮后仍缺）。retryable=false，父 Agent 自决。
	FailureKindVerifyMissing FailureKind = "verify_missing"
)

// failureKindOf 从失败错误分类失败类型；未知错误归 error。
func failureKindOf(err error) FailureKind {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureKindTimeout
	case errors.Is(err, tool.ErrLoopExit):
		return FailureKindLoopGuard
	case errors.Is(err, errPartialReturn):
		return FailureKindBudget
	case errors.Is(err, errUnverified):
		return FailureKindUnverified
	case errors.Is(err, errVerifyMissing):
		return FailureKindVerifyMissing
	default:
		return FailureKindError
	}
}

// failureMarker 渲染失败消息头部的机读标记行。
// retryable 表示该 kind+角色是否落入 dispatcher 自动重派策略（error + 叶子 + 未取消），
// 供父 LLM 与未来结构化消费者判断该失败是否可自动恢复。
func failureMarker(kind FailureKind, retryable bool) string {
	return fmt.Sprintf("[failure kind=%s retryable=%t]", kind, retryable)
}

// errPaused 标记 DomainAgent 触达 token 上限进入 Paused(runSubAgentOnce 已存 history + tree.Pause)。
// runSubAgent 见此信号:不 notify 父、不 treeFinish、不 trackChildDone,父 PendingChildren 保持 >0,
// 由 MetaAgent 经 PausedChildChecker 检测后主动暂停会话,等用户"继续"恢复该 domain。
var errPaused = errors.New("sub-agent paused on token budget")

// errPartialReturn 标记叶子助手触达 token 上限,已把部分产出(LastAssistantText)塞入 result.Text。
// runSubAgent 见此信号:treeFinish Done("部分完成")+ notify 父 mailbox 部分 + trackChildDone 照常减
// (叶子是叶子,不持久化 history,父 domain 收部分后自行决定重派或接手)。
var errPartialReturn = errors.New("sub-agent partial return on token budget")

// errUnverified 标记校验 judge LLM 不可用（TODO #43 fail-closed）：result.Unverified=true，
// runSubAgent 见此信号按 FailureKindUnverified（retryable=false）notify 父并附未验证产出全文，
// 绝不静默放行（旧 fail-open 使 reflection 自检形同虚设）。
var errUnverified = errors.New("sub-agent result unverified: judge LLM unavailable")

// errVerifyMissing 标记 L0 可执行校验缺验证证据（无成功运行的测试/lint/--check，
// 重试 1 轮后仍缺）。runSubAgent 见此信号按 FailureKindVerifyMissing notify 父。
var errVerifyMissing = errors.New("sub-agent missing executable verification evidence")

// formatSubAgentFailure 把 runSubAgentOnce 返回的错误格式化为父邮箱通知文案，
// 保留原有"超时/循环守卫/通用失败"三段语义与部分进度回传；
// 校验分层两类（TODO #43）单独文案，明确"未验证"而非"失败"语义。
func formatSubAgentFailure(err error, result agent.ReactResult, timeout time.Duration, partial string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("子 Agent 执行超时（上限 %v），已被终止。%s", timeout, partialSuffix(partial))
	}
	if errors.Is(err, tool.ErrLoopExit) {
		reason := strings.TrimPrefix(err.Error(), tool.ErrLoopExit.Error()+": ")
		return fmt.Sprintf("子 Agent 陷入循环被守卫终止（%s）。%s", reason, partialSuffix(partial))
	}
	if errors.Is(err, errUnverified) {
		return fmt.Sprintf("子 Agent 校验不可用（judge LLM 失败：%s），结论未验证。", result.VerifyNote)
	}
	if errors.Is(err, errVerifyMissing) {
		return "子 Agent 未提供可执行验证证据（没有成功运行的测试/lint/--check 命令）。"
	}
	return fmt.Sprintf("sub-agent failed: %v%s", err, partialSuffix(partial))
}

// ExecuteChild 同步执行一个子 Agent 并返回其最终答复文本。
// 实现 verifyloop.Runner 接口（verifyloop 当前未接线，保留为业务验收测试工作流原型，
// 见 doc/扩展设计_Agent工作流平台.md §12）：
//   - 同步阻塞至子 Agent 完成，调用方直接拿到结果；
//   - 不 notify 父邮箱（调用方自行决定如何反馈）；
//   - 不进入实例池服务态（一次性执行）。
//
// TODO #21 吸收 #2 开放动作：同步子 Agent 同样入权威树（Register/SetCancel/Finish）——
// TUI 可见、HTTP cancel 可取消、叶子心跳覆盖（与异步派发同语义）。
// 权限校验与 ID 生成与 call_sub_agent 工具一致；失败时返回 partial 结果与 err。
func (d *Dispatcher) ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error) {
	roleDef := d.registry.Get(roleID)
	if roleDef == nil {
		return "", fmt.Errorf("unknown role: %s", roleID)
	}
	if !d.registry.CanCall(roleIDFromAgentID(parentID), roleID) {
		return "", fmt.Errorf("role %s cannot be called by %s", roleID, parentID)
	}
	subAgentID := fmt.Sprintf("%s/%s-%d", parentID, roleID, d.seq.Add(1))
	d.trackChildStart(parentID)
	defer d.trackChildDone(parentID)

	// 树/心跳需要可取消子 ctx；无树无心跳时直接用调用方 ctx（零行为变化）。
	childCtx := ctx
	cancel := context.CancelFunc(func() {})
	if d.treeFn != nil || d.heartbeatTimeout > 0 {
		childCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	sid := tool.SessionIDFromContext(ctx)
	started := time.Now()
	taskBrief := truncateRunes(strings.ReplaceAll(strings.TrimSpace(task), "\n", " "), 100)
	if d.treeFn != nil && sid != "" {
		if t := d.treeFn(sid); t != nil {
			t.Register(orchestrator.Node{
				ID:       subAgentID,
				ParentID: parentID,
				Role:     roleID,
				Task:     taskBrief,
				Started:  started,
				Status:   orchestrator.StatusRunning,
			})
			t.SetCancel(subAgentID, cancel)
		}
	}
	// 心跳检活：叶子注册 activity + subMeta（巡检超时 cancel -> childCtx err -> 本方法返回错误）。
	isLeaf := roleDef.ID != "domain" && roleDef.ID != "meta"
	if isLeaf && d.heartbeatTimeout > 0 {
		act := new(atomic.Int64)
		act.Store(time.Now().UnixNano())
		d.activity.Store(subAgentID, act)
		d.subMeta.Store(subAgentID, &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: sid})
		d.ensurePatrol()
		defer func() {
			d.activity.Delete(subAgentID)
			d.subMeta.Delete(subAgentID)
		}()
	}

	// ExecuteChild 是 verifyloop 原型（未接线）的执行通道：verifyKind 传 "none" 保持原型
	// 语义纯净（不引入校验分层副作用）。
	_, result, err := d.runSubAgentOnce(childCtx, parentID, subAgentID, *roleDef, task, "", "", "", "none")
	if d.treeFn != nil && sid != "" {
		if t := d.treeFn(sid); t != nil {
			if err != nil {
				t.Finish(subAgentID, truncateRunes(agent.LastAssistantText(result.History), 200), err)
			} else {
				t.Finish(subAgentID, result.Text, nil)
			}
		}
	}
	if err != nil {
		return result.Text, err
	}
	return result.Text, nil
}

// specPrefixMarker 是任务规范注入任务前缀时的标记，便于子 Agent 区分"任务规范"与"当前任务"。
const specPrefixMarker = "【任务规范】\n"

// sharedPrefixMarker 是共享记忆注入任务前缀时的标记，便于子 Agent 区分"共享记忆"与"当前任务"。
const sharedPrefixMarker = "【共享记忆】\n"

// sharedPrefixDisciplineNote 是共享前缀尾部固定的反重读纪律行。
// 实证（2026-08-14 塔防 9 叶子并行重绘）：契约已含共享方法签名清单，9 个叶子仍各自
// ReadFile 重读共享代码区（合计 4722 行 ≈ 文件 3.7 倍）——注入内容必须显式声明
// "视为已验证、禁止重读"，否则"认真查询"类通用纪律会驱使子 Agent 回读原文。
// 注意：措辞不得含字面量【任务规范】/【共享记忆】/【当前任务】，避免干扰按标记切分前缀的既有逻辑与测试。
const sharedPrefixDisciplineNote = "【读取纪律】以上注入的任务规范与共享记忆内容视为已验证事实：" +
	"其中已给出的代码、签名与行号无需再用 ReadFile 核对或重读；" +
	"ReadFile 仅限当前任务指派给你的行号范围，不读兄弟任务的代码区段。"

// specSlotName 是 WriteSpec 写入的固定 slot 名，与 tool.SpecSlot 保持一致。
const specSlotName = "spec"

// specMirror 是 tool.Spec 的本地镜像，避免 subagent 反向 import tool 包。
// 字段名与 JSON tag 必须与 tool.Spec 保持一致。
type specMirror struct {
	Goal        string   `json:"goal"`
	Acceptance  []string `json:"acceptance,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	Files       []string `json:"files,omitempty"`
}

// buildSharedPrefix 读取 parentID 下所有共享记忆槽位，渲染为【任务规范】+【共享记忆】前缀。
// 缺失/stale/解析失败时返回空串（graceful degrade，不阻塞派发主流程）。
//
// 槽位两类：
//   - spec 槽位（"<parentID>:spec"）：WriteSpec 写入，MD frontmatter 含 goal/acceptance/constraints/files。
//     渲染为【任务规范】段，files mtime 校验失败视为 stale 跳过。
//   - 自由槽位（"<parentID>:<key>"，key != spec）：WriteSharedMemory 写入，MD body 为 content。
//     渲染为【共享记忆】段，files mtime 校验失败跳过。
//
// 返回纯前缀（不含【当前任务】标记），供 runSubAgentOnce 统一拼装多段前缀避免嵌套。
func (d *Dispatcher) buildSharedPrefix(ctx context.Context, parentID string) string {
	if d.sharedMem == nil {
		return ""
	}
	prefix := parentID + ":"
	var specPart, sharedParts []string
	slotCount, totalLen := 0, 0
	slotNames := []string{}
	for _, k := range d.sharedMem.Keys(ctx) {
		if !strings.HasPrefix(k, prefix) || k == prefix {
			continue
		}
		val, err := d.sharedMem.Get(ctx, k)
		if err != nil || strings.TrimSpace(val) == "" {
			continue
		}
		slotCount++
		totalLen += len(val)
		slotName := strings.TrimPrefix(k, prefix)
		// 打捞槽位（salvage:<domain>）不走通用共享注入：域相关性强，通用注入会污染
		// 无关子 Agent 上下文；由同域重派经 withPriorSalvage 显式读回（TODO #20 第三层）。
		if strings.HasPrefix(slotName, salvageSlotPrefix) {
			continue
		}
		slotNames = append(slotNames, slotName)

		fm, body, ok := tool.DecodeSharedMD(val)
		if !ok {
			// 旧格式（无 frontmatter 的纯字符串）：直接当共享内容用，向后兼容。
			if slotName == specSlotName {
				continue
			}
			sharedParts = append(sharedParts, val)
			continue
		}
		// Layer 3 mtime 校验：任一 file stat 不匹配视为 stale，丢弃避免子 Agent 读旧摘要。
		if !verifyFileMtimes(fm.Files) {
			continue
		}
		if slotName == specSlotName {
			// spec 槽位需 goal + 至少一条 acceptance 才视为合法规范。
			if strings.TrimSpace(fm.Goal) == "" || len(fm.Acceptance) == 0 {
				continue
			}
			files := make([]string, 0, len(fm.Files))
			for fp := range fm.Files {
				files = append(files, fp)
			}
			specPart = []string{renderSpecPrefix(specMirror{
				Goal:        fm.Goal,
				Acceptance:  fm.Acceptance,
				Constraints: fm.Constraints,
				Files:       files,
			})}
			continue
		}
		sharedParts = append(sharedParts, body)
	}
	if len(specPart) == 0 && len(sharedParts) == 0 {
		return ""
	}
	log.Printf("[subagent] inject shared-memory: parent=%s slots=%d total_len=%d slot_names=%v spec=%v shared=%d",
		parentID, slotCount, totalLen, slotNames, len(specPart) > 0, len(sharedParts))

	// 风险兜底：有共享槽位但缺 file_tree slot 时，注入提示让首个探索者写入供后续兄弟 Agent 复用。
	hasFileTree := false
	for _, name := range slotNames {
		if name == "file_tree" {
			hasFileTree = true
			break
		}
	}
	if len(sharedParts) > 0 && !hasFileTree {
		log.Printf("[subagent] shared-memory missing file_tree: parent=%s - injecting exploration hint", parentID)
		sharedParts = append(sharedParts, "【项目结构未知】共享记忆缺 file_tree slot。请用 ListDir 扫项目结构 + ReadFile 抽签名，WriteSharedMemory(key=\"file_tree\", files=[只读参照文件]) 写入紧凑树供后续兄弟 Agent 复用。")
	}

	var parts []string
	parts = append(parts, specPart...)
	if len(sharedParts) > 0 {
		joined := strings.Join(sharedParts, "\n\n---\n\n")
		// 共享前缀长度上限：防极端情况（如规格全文塞 KV）撑爆；超限截断尾部并标提示。
		const sharedPrefixMaxRunes = 30000
		if r := []rune(joined); len(r) > sharedPrefixMaxRunes {
			joined = string(r[:sharedPrefixMaxRunes]) + "\n\n…（共享记忆超过 30KB 上限，已截断尾部；完整规格请用 ReadFile 读取相关文件）"
		}
		parts = append(parts, sharedPrefixMarker+joined)
	}
	// 尾部固定反重读纪律行：注入内容视为已验证，禁止为其区间再 ReadFile。
	parts = append(parts, sharedPrefixDisciplineNote)
	return strings.TrimRight(strings.Join(parts, "\n\n"), "\n")
}

// hasFreshSpec 校验 parentID:spec 是否存在且新鲜（Spec.Goal 非空 + 至少一条 Acceptance + files mtime 一致）。
// 供 callSubAgentTool.Execute 在 SpecEnforcementEnabled 开启时调用，缺失则拒绝派发。
// 返回 (通过, 失败原因文案)：失败时区分 missing（未写）/ invalid（内容不合法）/ stale（文件已变更），
// stale 精确列出失配文件路径，让 LLM 定向修正（重写被改文件或剔除无关文件）而非盲猜重写整个 spec。
func (d *Dispatcher) hasFreshSpec(ctx context.Context, parentID string) (bool, string) {
	if d.sharedMem == nil {
		return false, "spec missing: 共享记忆未启用"
	}
	key := parentID + ":" + specSlotName
	val, err := d.sharedMem.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		return false, "spec missing: 未找到 WriteSpec 写入的任务规范"
	}
	fm, _, ok := tool.DecodeSharedMD(val)
	if !ok {
		return false, "spec invalid: 规范文件解析失败（frontmatter 缺失或损坏）"
	}
	if bad := staleFilePaths(fm.Files); len(bad) > 0 {
		return false, "spec stale: 涉及文件已变更: " + strings.Join(bad, ", ") + "。请用 WriteSpec 重写（或剔除无关文件）"
	}
	if strings.TrimSpace(fm.Goal) == "" || len(fm.Acceptance) == 0 {
		return false, "spec invalid: 规范缺 goal 或验收标准（acceptance 至少一条）"
	}
	return true, ""
}

// renderSpecPrefix 把 Spec 渲染为【任务规范】前缀文本。
func renderSpecPrefix(s specMirror) string {
	var b strings.Builder
	b.WriteString(specPrefixMarker)
	b.WriteString("目标: ")
	b.WriteString(strings.TrimSpace(s.Goal))
	b.WriteByte('\n')
	if len(s.Acceptance) > 0 {
		b.WriteString("验收:\n")
		for _, a := range s.Acceptance {
			b.WriteString("  - ")
			b.WriteString(strings.TrimSpace(a))
			b.WriteByte('\n')
		}
	}
	if len(s.Constraints) > 0 {
		b.WriteString("约束:\n")
		for _, c := range s.Constraints {
			b.WriteString("  - ")
			b.WriteString(strings.TrimSpace(c))
			b.WriteByte('\n')
		}
	}
	if len(s.Files) > 0 {
		b.WriteString("涉及文件:\n")
		for _, f := range s.Files {
			b.WriteString("  - ")
			b.WriteString(strings.TrimSpace(f))
			b.WriteByte('\n')
		}
	}
	// 范围锚定：规范是父 Agent 的全局目标（常含多领域拆分与其他 Agent 职责），
	// 你的执行范围以 task 正文为准。其他 Agent 的进度与你无关——不要等待、不要汇报、
	// 不要模仿它们的状态；父 Agent 会统一整合各领域回传（实证 2026-08-13 身份混淆空转）。
	b.WriteString("\n【范围】你的职责只在本任务 task 正文；上面的目标/验收是父 Agent 的全局背景。")
	return strings.TrimRight(b.String(), "\n")
}

// subAgentWorkDir 返回子 Agent 的工作目录，从工具注册表取，供 ReActAgent 注入系统提示词。
// 为空时 ReActAgent 回退到进程 cwd。
func (d *Dispatcher) subAgentWorkDir() string {
	if d == nil || d.tools == nil {
		return ""
	}
	return d.tools.WorkDir()
}

// WithSharedMemory 注入共享记忆后端（tool.SharedMemoryStore），使子 Agent 派发时能读取
// 主 Agent 通过 WriteSharedMemory/WriteSpec 工具写入的关键上下文与任务规范。
// 传 nil 关闭共享记忆注入（默认）。
func (d *Dispatcher) WithSharedMemory(r tool.SharedMemoryStore) *Dispatcher {
	d.sharedMem = r
	return d
}

// saveBlockMemory 将子 Agent 完成后的结果沉淀到块记忆知识库。
// 未配置写入器、开关关闭或结果为空时跳过；写入失败仅记日志，不影响派发主流程。
//
// outcome 标记执行状态（blockOutcomeSuccess/Partial/Fail），写入 Meta 供召回侧排序：
// success 优先、partial/fail 降权为避坑经验，避免失败记忆与成功记忆并列误导子 Agent。
//
// parentID/taskDomain/filesModified 是黑板模式（TODO #42）scope 标签：写入 Meta 的 parent_id/
// task_domain/files_modified，供 BlackboardSearcher.Query 按 scope 确定性过滤（兄弟产出按 scope
// 共享，替纯语义召回的跨 scope 串扰）。taskDomain 为空时仅落语义召回可用字段，scope 查询召回不到。
//
// 提取策略：若 factExtractor 已注入，先调用 LLM 提取 1-5 条关键事实，
// 每条事实单独落 KnowledgeRecord（向量化后召回精度更高）。
// 提取失败或未注入时回退到原始 result.Text 落库（向后兼容）。
func (d *Dispatcher) saveBlockMemory(ctx context.Context, subAgentID, roleID, parentID, taskDomain, goal, result, outcome string, filesModified []string) {
	if d.saver == nil || !d.writeEnabled {
		return
	}
	content := strings.TrimSpace(result)
	if content == "" {
		return
	}
	if d.factExtractor != nil {
		facts, err := d.factExtractor.Extract(ctx, content, goal, roleID)
		if err == nil && len(facts) > 0 {
			d.saveFacts(ctx, subAgentID, roleID, parentID, taskDomain, goal, facts, outcome, filesModified)
			return
		}
		log.Printf("[subagent] extract facts failed, fallback raw: sub=%s err=%v facts=%d", subAgentID, err, len(facts))
	}
	d.saveRawBlockMemory(ctx, subAgentID, roleID, parentID, taskDomain, goal, content, outcome, filesModified)
}

// domainReuseCountOf 返回热驻槽的当前复用次数（Domain 热驻两层权重：域级 reuse_count
// 与条目级 reuse_count）；非热驻/槽不存在返回 0。saveRawBlockMemory/saveFacts 写
// domain_reuse_count Meta 标签，供召回侧把"高频复用领域的沉淀"排序靠前。
func (d *Dispatcher) domainReuseCountOf(sessionID, subAgentID string) int {
	if !d.hotEnabled() || sessionID == "" {
		return 0
	}
	s := d.pool.slot(sessionID, subAgentID)
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reuseCount
}

// saveBlockRecord 落库单条块记忆并维护失败观测计数。
// 连续失败达到阈值时 slog.Warn 醒目告警（提示 embedding 端点/DB 链路损坏），
// 成功时重置计数。best-effort：失败仅告警，不影响派发主流程。
func (d *Dispatcher) saveBlockRecord(ctx context.Context, rec *types.KnowledgeRecord, tag string) {
	if err := d.saver.Save(ctx, rec); err != nil {
		n := d.blockSaveFailures.Add(1)
		log.Printf("[subagent] save %s failed: sub=%v err=%v", tag, rec.Meta["sub_agent_id"], err)
		if n == blockSaveFailAlertThreshold {
			slog.Warn(fmt.Sprintf("block memory save failed consecutively %d times: "+
				"块记忆写入链路可能损坏（embedding 端点 404 / global_knowledge 表缺失）。"+
				"检查 EMBEDDING_BASE_URL 与数据库表；修复前块记忆沉淀/召回持续静默降级", n))
		}
		return
	}
	d.blockSaveFailures.Store(0)
}

// saveRawBlockMemory 把原始 result.Text 作为单条 KnowledgeRecord 落库。
// 内容采用"目标/角色/结果"三段式，meta 携带 goal/domain/session_id/parent_id/task_domain/
// files_modified/outcome/reuse_count 标签，便于召回侧（SearchBlockMemoryByGoal / BlackboardSearcher.Query）
// 按目标文本、scope 与价值排序命中。
func (d *Dispatcher) saveRawBlockMemory(ctx context.Context, subAgentID, roleID, parentID, taskDomain, goal, content, outcome string, filesModified []string) {
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	sid := tool.SessionIDFromContext(ctx)
	meta := map[string]any{
		"goal":           trimmedGoal,
		"domain":         roleID,
		"session_id":     sid,
		"sub_agent_id":   subAgentID,
		"parent_id":      parentID,
		"task_domain":    strings.TrimSpace(taskDomain),
		"files_modified": filesModified,
		"source":         "sub_agent_result",
		"outcome":        outcome,
		"reuse_count":    0,
	}
	// Domain 热驻两层权重：域级复用次数写 domain_reuse_count 标签。
	if rc := d.domainReuseCountOf(sid, subAgentID); rc > 0 {
		meta["domain_reuse_count"] = rc
	}
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		Content:       fmt.Sprintf("目标:%s\n角色:%s\n结果:%s", trimmedGoal, roleID, truncateRunes(content, blockMemoryResultMaxRunes)),
		Meta:          meta,
		CreatedAt:     time.Now(),
	}
	d.saveBlockRecord(ctx, rec, "block memory")
}

// saveFacts 把提取出的事实逐条落库，每条单独向量化以提升召回精度。
// 失败仅记日志，不影响其他事实或派发主流程。
func (d *Dispatcher) saveFacts(ctx context.Context, subAgentID, roleID, parentID, taskDomain, goal string, facts []string, outcome string, filesModified []string) {
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	sid := tool.SessionIDFromContext(ctx)
	for i, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		meta := map[string]any{
			"goal":           trimmedGoal,
			"domain":         roleID,
			"session_id":     sid,
			"sub_agent_id":   subAgentID,
			"parent_id":      parentID,
			"task_domain":    strings.TrimSpace(taskDomain),
			"files_modified": filesModified,
			"source":         "fact_extraction",
			"fact_index":     i,
			"outcome":        outcome,
			"reuse_count":    0,
		}
		if rc := d.domainReuseCountOf(sid, subAgentID); rc > 0 {
			meta["domain_reuse_count"] = rc
		}
		rec := &types.KnowledgeRecord{
			KnowledgeType: enums.KnowledgeTypeBlockMemory,
			Content:       fact,
			Meta:          meta,
			CreatedAt:     time.Now(),
		}
		d.saveBlockRecord(ctx, rec, "fact")
	}
}

// blockMemoryRecallHeader 是块记忆召回注入段头部。
// 标注"已完成结论"语义：这些是过去任务的沉淀事实，仅作参考背景，
// 防止模型把旧任务结果当成当前任务的待办去重复执行（实证：怪物 UI 改动
// 被后续 UI 任务再次执行）。
const blockMemoryRecallHeader = "【相关记忆】（以下为已完成任务的结论沉淀，仅作背景参考，不得把其中已完成的改动当作当前任务的待办重复执行）"

// rankBlockMemory 就地按 outcome（success 优先）+ reuse_count 降序 + 新近优先排序。
// 召回排序（价值反馈闭环）：成功记忆在前，失败/部分记忆降权为避坑经验段。
func rankBlockMemory(recs []*types.KnowledgeRecord) {
	sort.SliceStable(recs, func(i, j int) bool {
		ri, rj := blockOutcomeRank(recs[i].Meta), blockOutcomeRank(recs[j].Meta)
		if ri != rj {
			return ri < rj
		}
		if ui, uj := blockReuseCount(recs[i].Meta), blockReuseCount(recs[j].Meta); ui != uj {
			return ui > uj
		}
		return recs[i].CreatedAt.After(recs[j].CreatedAt)
	})
}

// bumpReuses best-effort 递增命中记录的 reuse_count（JSONB 就地更新），失败仅记日志。
func (d *Dispatcher) bumpReuses(ctx context.Context, recs []*types.KnowledgeRecord) {
	bumper, ok := d.searcher.(reuseBumper)
	if !ok {
		return
	}
	for _, rec := range recs {
		if rec.ID <= 0 {
			continue
		}
		if err := bumper.BumpReuse(ctx, rec.ID); err != nil {
			log.Printf("[subagent] bump block-memory reuse failed: id=%d err=%v", rec.ID, err)
		}
	}
}

// renderRecalledMemory 把命中记录渲染为成功经验/避坑经验两段文本。
// header 由调用方指定（播种召回用【相关记忆】，每轮摄取用【兄弟产出】）。
func renderRecalledMemory(header string, recs []*types.KnowledgeRecord) string {
	var sb strings.Builder
	sb.WriteString(header + "\n")
	var successLines, pitfallLines []string
	for _, rec := range recs {
		if blockOutcomeRank(rec.Meta) == 0 {
			successLines = append(successLines, strings.TrimSpace(rec.Content))
		} else {
			pitfallLines = append(pitfallLines, strings.TrimSpace(rec.Content))
		}
	}
	if len(successLines) > 0 {
		sb.WriteString("成功经验:\n")
		for i, l := range successLines {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, l)
		}
	}
	if len(pitfallLines) > 0 {
		sb.WriteString("避坑经验:\n")
		for i, l := range pitfallLines {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, l)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// injectScopedRecall 黑板模式（TODO #42）scope 确定性召回：优先 BlackboardSearcher.Query
// 按 parent_id + task_domain 过滤（兄弟产出按 scope 共享，每个 Agent 只取自己 scope 切片）；
// searcher 未实现 BlackboardSearcher（mock）/ scope 0 命中 / 出错时回退 SearchBlockMemoryByGoal
// 语义召回（向后兼容旧数据与现有测试）。query 为语义检索目标文本（原始任务，避免空串向量召回垃圾）。
//
// task 为空时返回纯前缀（不含【当前任务】标记），供调用方统一拼装；非空返回完整 "前缀 + 【当前任务】 + task"。
// 返回命中记录切片，供调用方日志留痕与 uptake wrapper 去重 seed（防播种召回与每轮摄取重复）。
func (d *Dispatcher) injectScopedRecall(ctx context.Context, parentID, taskDomain, query, task string) (string, []*types.KnowledgeRecord) {
	if d.searcher == nil {
		return task, nil
	}
	sid := tool.SessionIDFromContext(ctx)
	var recs []*types.KnowledgeRecord
	// scope 确定性路径：BlackboardSearcher + 非空 parent/domain。
	if bb, ok := d.searcher.(BlackboardSearcher); ok && strings.TrimSpace(parentID) != "" && strings.TrimSpace(taskDomain) != "" {
		if rs, err := bb.Query(ctx, sid, parentID, taskDomain, query, blockMemoryRecallTopK, ""); err == nil && len(rs) > 0 {
			recs = rs
		}
	}
	// 回退：语义召回（旧数据无 parent_id/task_domain / mock 未实现 BlackboardSearcher / scope 0 命中）。
	if len(recs) == 0 {
		rs, err := d.searcher.SearchBlockMemoryByGoal(ctx, sid, query, blockMemoryRecallTopK)
		if err != nil || len(rs) == 0 {
			rs = nil
		}
		recs = rs
	}
	// 跨 session 补位：session 内命中不足 topK 时，按更严阈值召回历史任务沉淀
	// （排除当前 session，与已召回记录天然不重叠；纯语义过滤，跨项目弱相关
	// 事实相似度低于阈值自然被滤掉）。未实现 CrossSessionSearcher / 出错 /
	// 0 命中时静默跳过，不影响 session 内召回。
	if len(recs) < blockMemoryRecallTopK {
		if cs, ok := d.searcher.(CrossSessionSearcher); ok {
			extra, err := cs.SearchBlockMemoryCrossSession(ctx, sid, query, blockMemoryRecallTopK-len(recs))
			if err != nil {
				log.Printf("[subagent] cross-session block-memory supplement failed: session=%s err=%v", sid, err)
			} else {
				recs = append(recs, extra...)
			}
		}
	}
	if len(recs) == 0 {
		return task, nil
	}
	rankBlockMemory(recs)
	d.bumpReuses(ctx, recs)
	prefix := renderRecalledMemory(blockMemoryRecallHeader, recs)
	if task == "" {
		return prefix, recs
	}
	return prefix + "\n\n【当前任务】\n" + task, recs
}

// injectRecalledMemory 旧语义召回 wrapper（向后兼容现有测试调用点）：不带 scope，
// 走 injectScopedRecall 的回退路径（纯语义 SearchBlockMemoryByGoal）。
func (d *Dispatcher) injectRecalledMemory(ctx context.Context, query, task string) (string, []*types.KnowledgeRecord) {
	return d.injectScopedRecall(ctx, "", "", query, task)
}

// blockOutcomeRank 返回 outcome 的排序权重：success=0 优先召回，partial/fail 降权为避坑经验。
// 历史记录缺 outcome 字段时按 success 处理（旧数据全部来自成功路径沉淀，向后兼容）。
func blockOutcomeRank(meta map[string]any) int {
	if meta == nil {
		return 0
	}
	switch v, ok := meta["outcome"].(string); {
	case !ok:
		return 0
	case v == blockOutcomePartial:
		return 1
	case v == blockOutcomeFail:
		return 2
	default:
		return 0
	}
}

// blockReuseCount 读取 Meta 中的 reuse_count（缺省 0）。
// DB JSON 反序列化数值为 float64，测试构造可能为 int，兼容两者。
func blockReuseCount(meta map[string]any) int {
	if meta == nil {
		return 0
	}
	switch v := meta["reuse_count"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// staleFilePaths 返回未通过 mtime 校验的 path 列表（stat 失败或 mtime 不匹配），
// 持续增长目录（logs/.bma，tool.IsGrowingPath）下的文件豁免。排序保证文案确定性。
func staleFilePaths(files map[string]int64) []string {
	var bad []string
	for path, stamped := range files {
		if tool.IsGrowingPath(path) {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().Unix() != stamped {
			bad = append(bad, path)
		}
	}
	sort.Strings(bad)
	return bad
}

// verifyFileMtimes 校验各 path 当前 mtime 与 frontmatter 中记录的是否一致。
// 任一 path stat 失败或 mtime 不匹配返回 false（视为 stale）。
// 空 Files 视为通过（无 path 需校验，可能是旧 entry 或纯结论摘要）。
func verifyFileMtimes(files map[string]int64) bool {
	return len(staleFilePaths(files)) == 0
}

// partialSuffix 把部分进度文本拼接到通知末尾；为空时返回空串。
func partialSuffix(partial string) string {
	if partial == "" {
		return ""
	}
	return "\n当前已完成的部分进度：\n" + partial
}

// truncateRunes 按 rune 数截断字符串并追加省略提示。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
}

// notify 向父 Agent 邮箱发送一条子 Agent 完成或失败的通知消息。
// filesModified 为子 Agent 本次修改的文件路径列表（Layer 5，从 result.History 扫 WriteFile 得来），
// 父 drainMailbox 时展示给父 LLM；无修改文件时传 nil。
func (d *Dispatcher) notify(parentID, subAgentID, summary string, filesModified []string) {
	// 若未配置邮箱，直接返回，避免 nil 指针 panic。
	if d.mailbox == nil {
		return
	}
	// 构造并发送消息：发件人为子 Agent，收件人为父 Agent，主题为子 Agent 完成提示，正文为摘要。
	// 死信错误（父已销毁）仅记日志：notify 是 best-effort 通知，不阻塞失败主流程。
	if _, err := d.mailbox.Send(&mailbox.Message{
		From:          subAgentID,
		To:            parentID,
		Type:          mailbox.MsgInfo,
		Subject:       "子 Agent 完成: " + subAgentID,
		Body:          summary,
		FilesModified: filesModified,
	}); err != nil {
		log.Printf("[subagent] notify dead-letter: to=%s from=%s err=%v", parentID, subAgentID, err)
	}
}

// roleIDFromAgentID 从 Agent 句柄中还原出角色 ID。
// 子 Agent 的标识格式为 "parent/role-n"（见 Execute 中的 subAgentID 生成），
// 还原规则：取最后一个 '/' 之后的末段，再去掉末尾最后一个 '-' 之后的序号。
// 角色 ID 自身可能含连字符（如 code-assistant），因此必须用 LastIndex 定位
// 序号分隔符，不能用第一个 '-' 截断，否则会把角色 ID 截短（code-assistant → code）。
// 顶层 Agent 以会话 ID（session-N）命名，其角色恒为 MetaAgent，返回 "meta"。
func roleIDFromAgentID(agentID string) string {
	// 查找最后一个 '/'，若存在则取最后一段 "role-n"。
	if idx := strings.LastIndex(agentID, "/"); idx >= 0 {
		seg := agentID[idx+1:]
		// 在 "role-n" 中查找最后一个 '-'：序号恒由生成方追加在末尾，
		// 末个 '-' 之前即为完整角色 ID（含角色自身可能的连字符）。
		if dash := strings.LastIndex(seg, "-"); dash >= 0 {
			return seg[:dash]
		}
		return seg
	}
	// 顶层 Agent 没有 '/'：生产环境中其名称为会话 ID（session-N），
	// 由 runSession/resumeSession 以 meta 角色创建，故角色恒为 "meta"。
	return "meta"
}

// sessionIDFromAgentID 从 Agent 句柄中提取所属 session ID（首段）。
// 句柄格式 "session-N[/parent/role-n]"，无 '/' 时整体即 session ID。
func sessionIDFromAgentID(agentID string) string {
	if idx := strings.Index(agentID, "/"); idx >= 0 {
		return agentID[:idx]
	}
	return agentID
}

// MarshalResult 将 tool.Result 序列化为 JSON 字符串，用于 blades 工具响应。
func MarshalResult(r *tool.Result) string {
	// 忽略序列化错误：tool.Result 结构由可控字段组成，通常不会序列化失败。
	b, _ := json.Marshal(r)
	return string(b)
}
