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
	"path/filepath" // filepath 用于 spec 文件路径规范化（冒烟检查/契约检查）
	"sort"          // sort 用于召回结果按 outcome/reuse_count 价值排序
	"strings"       // strings 用于从 Agent ID 中提取角色 ID
	"sync"          // sync 提供 sync.Map 存储运行中的子 Agent
	"sync/atomic"   // sync/atomic 提供原子递增序列号
	"time"          // time 用于设置子 Agent 独立超时
	"unicode"       // unicode 用于汉字判定（domain 中文领域名校验）
	"unicode/utf8"  // unicode/utf8 用于 RuneCountInString 统计 task 字符数

	"github.com/blockmemory/agent/backend/internal/agent"               // agent 包提供 ReActAgent、MemoryPipeline、ModelProvider 等类型
	"github.com/blockmemory/agent/backend/internal/board"               // board 提供任务看板（TODO #22 执行计划）
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // orchestrator 提供 Agent 树元数据层
	"github.com/blockmemory/agent/backend/internal/domain/role"         // role 包提供角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool"         // tool 包提供工具注册表与 Result 类型
	"github.com/blockmemory/agent/backend/internal/logger"              // logger 包提供会话级日志器，记录子 Agent LLM I/O
	"github.com/blockmemory/agent/backend/internal/mailbox"             // mailbox 包用于子 Agent 向父 Agent 发送完成通知
	"github.com/blockmemory/agent/backend/internal/project"             // project 包提供 AGENTS.md/CLAUDE.md 项目自述读取（TODO 第10项⑦）
	"github.com/blockmemory/agent/backend/internal/skill"               // skill 包提供技能池与元数据块渲染（技能分发）
	"github.com/blockmemory/agent/backend/pkg/enums"                    // enums 包提供 KnowledgeTypeBlockMemory 等枚举常量
	"github.com/blockmemory/agent/backend/pkg/textutil"                 // textutil 提供截断展示名用工具
	"github.com/blockmemory/agent/backend/pkg/types"                    // types 包提供 RoleDefinition 类型
)

// 子 Agent 的上下文与父会话故意隔离（context.Background 派生）：
//   - 父会话若被用户手动取消，子 Agent 仍可在独立上下文中继续运行，避免长任务结果丢失。
//   - 超时仅用于防止无限制挂起，由 WithTimeout 配置；<=0 表示不限制。

// ModelProviderFactory 是 model.ModelFactory 的子集，
// Dispatcher 只需要从中获取指定角色对应的模型提供者即可创建子 Agent。
type ModelProviderFactory interface {
	// GetBladesProvider 根据 roleID 返回对应的模型提供者实例。
	GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error)
}

// AgentModelOverrider 实例级模型覆盖能力（*model.ModelFactory 实现，可选）。
// 经接口断言使用：测试桩未实现时回落角色级解析，不破坏既有桩。
type AgentModelOverrider interface {
	// GetBladesProviderForAgent 解析 agentID 的模型提供者（有实例覆盖用覆盖，否则角色级）。
	GetBladesProviderForAgent(ctx context.Context, roleID, agentID string) (agent.ModelProvider, error)
	// ClearAgentModel 回收实例级覆盖（节点终结/取消/槽销毁时调用）。
	ClearAgentModel(agentID string)
}

// AgentModelSwitcher 实例级模型覆盖写入能力（*model.ModelFactory 实现，可选）。
type AgentModelSwitcher interface {
	// SetAgentModel 为单个 Agent 实例覆盖模型（仅进程内存、不落盘、不影响同角色其他实例）。
	SetAgentModel(ctx context.Context, agentID, roleID, modelID, thinking string) (types.AgentModelConfig, error)
}

// providerForAgent 实例级 provider 解析（工厂未实现覆盖能力时回落角色级）。
func (d *Dispatcher) providerForAgent(ctx context.Context, roleID, agentID string) (agent.ModelProvider, error) {
	if ov, ok := d.models.(AgentModelOverrider); ok && agentID != "" {
		return ov.GetBladesProviderForAgent(ctx, roleID, agentID)
	}
	return d.models.GetBladesProvider(ctx, roleID)
}

// clearAgentModel 回收实例级模型覆盖（节点死亡钩子）；工厂未实现覆盖能力时 no-op。
// 暂停路径不得调用（覆盖要跨 resume 存活）。
func (d *Dispatcher) clearAgentModel(agentID string) {
	if ov, ok := d.models.(AgentModelOverrider); ok && agentID != "" {
		ov.ClearAgentModel(agentID)
	}
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

	// projectPrefs 项目偏好读取回调（2026-09-02 设计 §5）：按 ctx 会话目录解析
	// .bma/project_preferences.md 全文（S2）；派发前缀拼【项目偏好】段下发子 Agent。
	// nil 或空串时零注入。
	projectPrefs func(ctx context.Context) string

	// sessionGear 会话档位只读回调（TODO #14 T22）：sessionID -> 当前档位
	//（auto/fast/cluster）。热驻槽 enterIdle 固化档位、隐式复用解析按它裁决。
	// 刻意用普通 func 而非 agent 包类型（与 projectPrefs 同风格）；nil 时空串、守卫放行。
	sessionGearFn func(sessionID string) string

	// domainProfilesFn 领域档案快照回调（TODO #17 T24）：ctx -> 全部未归档领域档案。
	// 派发侧做名字/别名/路径匹配；nil 时档案匹配整体关闭。
	domainProfilesFn func(ctx context.Context) []*DomainProfile
	// domainMemoriesFn 领域记忆链回调（TODO #17 T24）：task_domain -> 最近 n 条块记忆正文。
	// 仅档案命中后惰性调用；nil 时种子不含结论链。
	domainMemoriesFn func(ctx context.Context, domain string, n int) []string
	// domainProfileSinkFn 领域档案增量写入回调（TODO #17 T25）：块记忆收尾旁路
	// 把 files_modified 并进档案文件清单；nil 时旁路关闭。
	domainProfileSinkFn func(ctx context.Context, up DomainProfileUpdate)

	// skillRecall 经验技能向量预答回调（2026-09-02 设计 §6.5）：task 文本 -> 提示行列表
	//（「有相关经验技能 <name>——<title>，可 load_skill 查看」）。nil 时零注入。
	skillRecall func(ctx context.Context, task string) []SkillHint
	// skillUseCounter learned 技能 use_count++ 回调（设计 §6.5）；nil 时零计数。
	skillUseCounter func(name string) error

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

	// ledger 任务台账（2026-08-28 旧需求重派事故根治）：本会话派发任务的权威
	// 状态流水——派发记"进行中"，notify 终态收口（完成/失败+原因+修改文件），
	// 经 TaskLedgerBrief 渲染注入 MetaAgent 每轮上下文。NewDispatcher 初始化，
	// 进程内有效；重启后 Render 时从权威树（PG 恢复）播种。
	ledger *TaskLedger

	// parentSpecs 缓存每个父 Agent 的 spec 结构化切片（TODO #56/#57/#65）：
	// 键为 specRecKey{parentID, domain}，值为 *parentSpecRecord（files 绝对路径 +
	// 跨域契约 + 验收层级）。domain 空=遗留单键 spec；非空=该领域专属 spec（多 key 化）。
	// 派发时从 spec 槽位捕获（此时 spec 尚新鲜，随后子 Agent 写入会触发 Layer 2
	// 失效删除 spec，完成收尾时无法再读到），供冒烟检查目标匹配与兄弟域全完成后的
	// 契约检查使用。逐键覆盖、按 parentID 增长（与 pending map 同增长剖面，
	// 条目为小切片 + 契约指针），进程重启不保留。
	parentSpecs sync.Map

	// smokeRunner 冒烟命令执行器（TODO #56）；nil 时用 defaultSmokeRunner 真跑命令。
	// 测试注入假实现，避免依赖机器上的 node/gofmt 工具链。
	smokeRunner smokeRunner
	// smokeLookPath 工具链探测函数（TODO #56）；nil 时用 exec.LookPath。测试注入假实现。
	smokeLookPath lookPathFunc

	// specEnforcementEnabled 派发方调用 call_sub_agent 前是否强制先写 WriteSpec。
	// 为 true 时 Execute 入口校验 parentID:spec 存在且新鲜（Spec.Goal 非空 + 至少一条 Acceptance），
	// 缺失则拒绝派发，返回 "先调 WriteSpec 再 call_sub_agent"。
	// 为 false 时跳过强制，injectSpec 仍生效（graceful degrade，spec 缺失则无前缀注入）。
	specEnforcementEnabled bool

	// planConfirmEnabled 计划确认机制开关（plan_confirm.go）：下级中大型任务动手前
	// submit_plan 给上级（顶层给用户）确认，批准后才执行。默认 false（零值），
	// bootstrap 按 cfg.Agent.PlanConfirmationEnabled 注入（默认 true）。关闭时
	// submit_plan 直通不阻塞，零行为变化。
	planConfirmEnabled bool
	// planConfirmTimeout 单次计划审批等待超时；<=0 时用 defaultPlanConfirmTimeout（fail-open）。
	planConfirmTimeout time.Duration
	// planMaxRevisions 每 Agent 计划被驳回重提上限；<=0 表示不限制（循环直到批准，
	// 默认）。>0 时达上限不放行，转 send_message(escalate) 升级仲裁（防无限循环）。
	planMaxRevisions int
	// planState 计划确认会话级状态（等待者注册表 + 驳回计数），NewDispatcher 初始化。
	planState *planConfirmState

	// depWaiters 依赖门就绪通知登记（TODO #22 依赖门排队化轻量版）：checkDepGate
	// 拒派时登记（键 parentID+"\x00"+domain，同键覆盖幂等），boardUpdate 终态回写
	// 后扫描该父的 waiter——依赖已全 done 则删登记 + 邮箱通知「可派发」+ 唤醒
	// 挂起会话（只通知不自动派发）。失败依赖不算了结（DependsDone 只认 done），
	// waiter 小结构静默留存无害（会话结束自然失效）。
	depWaiters sync.Map

	// maxTotalDispatches 全局派发总数上限：同一 session 内所有角色的派发合计超过该值时
	// 拒绝进一步派发，防止编排失控。<=0 表示不限制。计数随用户新消息重置。
	maxTotalDispatches int

	// aggByAgent 聚合模式登记（TODO 第七项⑤ map_sub_agents）：subAgentID → *mapAggEntry。
	// 命中时 notify 不直发父邮箱，改记入聚合器；收口后删除登记。
	aggByAgent sync.Map

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
	// agentsMDMaxRunes AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune 计数）。
	// workDir 根部 AGENTS.md 优先、其次 CLAUDE.md，以【项目自述】段注入派发前缀首位
	//（会话内最稳的段，前缀缓存友好）；缺失零开销。<=0 关闭（bootstrap 从
	// config agents_md_max_runes 注入，配置默认 4000）。
	agentsMDMaxRunes int
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

	// userNotifyFn 用户对话页系统消息回调（验收闭环进度通告）：
	// AcceptanceManager 在验收开始/发现问题派回修复/通过/熔断时经它发用户可见系统消息。
	// 由 bootstrap 接线到 ReactService 的 addEvent 包装；nil 时静默跳过（不影响主流程）。
	userNotifyFn func(sessionID, msg string)

	// childDoneFn 子 Agent 完成回调（「挂起等子」awaiting_child 的唤醒入口）：
	// trackChildDone 每次递减后异步触发（含失败/被杀/收口路径），接收方按会话态过滤
	// 幂等空转。由 bootstrap 接线到 ReactService.WakeOnChildDone；nil 时跳过。
	childDoneFn func(parentID string)

	// sessionWakeFn 挂起会话唤醒回调（邮箱请求死信修复）：上级会话处于
	// awaiting_child 挂起时不会 drain 邮箱，submit_plan 审批请求与 send_message
	// request/escalate 直问会滞留到超时白付延迟。Send 成功后经它唤醒挂起上级
	//（翻态 + resumeSession，wakeInput 提示查收邮箱）；目标非顶层会话（ID 含
	// "/"，getSession 不可达）接收方自然空转——子 Agent 间请求走
	// waitForChildren/pokeParent 既有路径。由 bootstrap 接线到
	// ReactService.WakeSuspended；nil 时跳过。
	sessionWakeFn func(parentID, hint string) bool

	// summaryMerger 整合纪要合成器（call_sub_agents 波聚合，C-3a）：同波各领域
	// 回传合成一条紧凑纪要（轻量 LLM 归并）。nil 时回退逐行拼接（fail-open）。
	// 由 bootstrap 注入（digest_merger.go）；测试可注入 stub。
	summaryMerger SummaryMerger
	// batchDigestEnabled call_sub_agents 波聚合开关（config agent.batch_digest_enabled，
	// 默认 true）：false 时退回逐条回传（逃生舱）。零值 false 会让测试环境意外
	// 静默——NewDispatcher 显式置 true，bootstrap 按配置覆盖。
	batchDigestEnabled bool

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

	// msgLogger 消息热层记录器（编排页对话视图）：构造子 Agent 时装配到 ReActAgent。
	msgLogger agent.MessageLogger

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
	softStops  map[string]bool
	softStopMu sync.Mutex
	// pauseRequests 记录被手动暂停的节点（TODO 第9⑥/10③ 审计面止血入口）：
	// ReactService.PauseAgent 对单个运行中 domain 标记后再 StopRunning，
	// context.Canceled 收尾分支据此把**该节点**（不影响会话其余节点）走
	// domain→Pause 软停收尾（SaveMessages + tree.Pause），ResumePaused 可续。
	// PauseAgent 收尾后/竞态时清除，不留会话级残留。
	pauseRequests  map[string]bool
	pauseRequestMu sync.Mutex

	// worktreeEnabled 允许 call_sub_agent 携带 worktree=true 派发到 git worktree 副本
	// （TODO 第9项⑤/#10⑤）。false 时 worktree 参数按 validation_rejected 拒绝
	//（bootstrap 从 config worktree_enabled 注入，config.yaml 默认 true）。
	worktreeEnabled bool
	// worktrees 登记每个 worktree 隔离派发的句柄（TODO 第9⑤），键 subAgentID ->
	// *worktreeHandle（path/branch/baseCommit/patch 等）。供完成收尾产出 patch、
	// merge_worktree 合并门与 CleanupSessionWorktrees 清理消费。
	// NewDispatcher 初始化；条目随 merge/remove 语义流转，进程内有效。
	worktrees sync.Map

	// heartbeatTimeout 子 Agent 心跳超时：叶子 Agent 超过该时长无活动（generateOnce/工具派发）
	// 判定假死（LLM 流式挂起/工具 hang），巡检 goroutine 主动 cancel + notify 父 + trackChildDone，
	// 比等满 sub_agent_timeout（默认 60min）早暴露。<=0 关闭巡检（测试场景默认关闭）。
	heartbeatTimeout time.Duration
	// domainHeartbeatTimeout DomainAgent 心跳超时（TODO #25-3 防误杀版）：
	// 默认 2× 叶子——domain 等子/等回信期间自身无 LLM/工具活动，靠后代活动冒泡保活；
	// 后代全静默后超该阈值才判假死。<=0 时按 2× heartbeatTimeout 兜底。
	domainHeartbeatTimeout time.Duration
	// activity 存子 Agent 活动证据（TODO 第10项②），键 subAgentID -> *activityEvidence：
	// lastTS/lastKind + llmInFlight/toolStartTS（在飞豁免与挂死工具判定）。
	// 仅叶子 Agent 与热驻 domain 槽注入（MetaAgent wait loop 不注入，避免误杀合法等待）。
	activity sync.Map
	// lastWrites 存子 Agent 近期写入的文件清单（subAgentID -> *fileWriteState），
	// 从实时工具事件识别（WriteFile/EditFile）。心跳巡检 kill 时把清单回告父 Agent--
	// 收尾卡死被杀的子 Agent 常已把产物全部落盘（2026-08-19 引擎 Agent：文件 15:54 落盘、
	// 回执因 judge 挂死拖到 17:03），父级需要知道盘上有货可按现状验收，而非从零重派。
	lastWrites sync.Map
	// recentActs 存子 Agent 杀前最近活动（llm_delta 尾文本 + 最近工具调用行），
	// kill 回告时机械归纳"已完成事项"回传父 Agent。
	recentActs sync.Map // subAgentID -> *recentActState
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

	// pushedViolations 已推送契约违例指纹集合（TODO #70 去重）：键 "parentID\x00指纹"，
	// 同指纹不重推——每波兄弟完成即重跑契约检查时，未修复的旧违例只推一次，
	// 复验仍未过时升级文案"已知违例仍未修复"而非原样重发（实证 2026-08-25：
	// 同一假违例 3 次推送）。生命周期随 Dispatcher（会话级）。
	pushedViolations sync.Map
	// pushedViolationTimes 记录同指纹违例首次推送时间（升级文案用），与 pushedViolations 同键。
	pushedViolationTimes sync.Map

	// dispatchGenerations per-(parent, domain) 派发代数计数（TODO #76 接力熔断）：
	// 第 3 代起 task 前缀注入【重写评估】强制段（继任者须先评估"整文件重写 vs 继续修补"），
	// 第 4 代起需 meta 在 task 显式声明继续理由否则拒派。键 "parentID\x00domain"。
	// 计数含异名接管链（takeover 声明时代数累加到新 domain）。
	dispatchGenerations sync.Map

	// skillPool 全局技能池（技能分发渐进披露）：nil 时技能参数/工具全部零行为。
	// bootstrap 经 WithSkillPool 注入（skills.yaml + 约定目录扫描合并后的同一池）。
	skillPool *skill.Pool
	// heldSkills 每个 Agent 实例当前持有的技能名（agentID -> []string）：
	// 派发时写入（角色固定集 ∪ 父分配集），子 Agent 的【可用技能】提示块与
	// load_skill/list_skills 范围都以此为权威。一次性 Agent 结束时删除；
	// 热驻槽随槽存活（destroySlot 清理）；未命中回退角色固定集。
	heldSkills sync.Map
}

// relayRewriteAssessGen 是注入【重写评估】强制段的起始代数（TODO #76）。
const relayRewriteAssessGen = 3

// relayHardDeclineGen 是无显式理由拒派的起始代数（TODO #76）。
const relayHardDeclineGen = 4

// relayContinueMarker 是第 4 代起 task 中须含的继续修补声明标记（TODO #76）。
// meta 在 task 里写 "【接力理由】..." 才放行派发。
const relayContinueMarker = "【接力理由】"

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
// 计数归零（全部兄弟完成）时异步触发跨域契约检查（TODO #57）。
// childDoneFn（「挂起等子」唤醒回调）每次递减后异步触发：各路径 mailbox 完成摘要
// 落箱与 trackChildDone 的先后序不一，异步回调 + 接收方按 awaiting_child 态过滤，
// 保证唤醒 resume 时摘要已落箱且重复唤醒幂等。
func (d *Dispatcher) trackChildDone(parentID string) {
	ps := d.getOrCreatePending(parentID)
	ps.count.Add(-1)
	select {
	case ps.notify <- struct{}{}:
	default:
	}
	if d.childDoneFn != nil && parentID != "" {
		fn := d.childDoneFn
		go fn(parentID)
	}
	if ps.count.Load() == 0 {
		go d.maybeRunContractChecks(parentID)
	}
}

// maybeRunContractChecks 在父节点下全部兄弟域完成时跑跨域契约静态检查（TODO #57）。
// 触发条件：父 spec 缓存含非空契约 + 无 spec 缓存/空契约/新一波派发已开始（未决计数
// 回升）则跳过。结果经 mailbox 通知父 Agent：通过=【机器校验】段（纸面对照证据），
// 违例=failure marker + 按文件归属批量列出全部违例（打回责任域，一次消息列全）。
//
// 违例去重（TODO #70）：同 (parent, 违例内容指纹) 只推一次——每波兄弟完成都会重跑
// 检查，未修复旧违例原样重发只会刷屏误导（实证 2026-08-25 同一违例 3 次推送）。
// 已推过的违例在复验仍未过时升级为"已知违例仍未修复（首次报告于 HH:MM）"单行提示。
func (d *Dispatcher) maybeRunContractChecks(parentID string) {
	if d.mailbox == nil {
		return
	}
	recs := d.parentSpecRecordsOf(parentID)
	hasContract := false
	for _, rec := range recs {
		if rec != nil && rec.contract != nil && !rec.contract.Empty() {
			hasContract = true
			break
		}
	}
	if !hasContract {
		return
	}
	// 新一波派发已开始（归零后又递增）时跳过本波检查，避免对着半成品误报。
	if d.PendingChildren(parentID) != 0 {
		return
	}
	var rep contractReport
	checked := 0
	for _, rec := range recs {
		if rec == nil || rec.contract == nil || rec.contract.Empty() {
			continue
		}
		// 变更屏障（TODO #62 时序串行化）：capture 后涉及文件已变（兄弟返工落地）时
		// 跳过该份契约，避免对着旧状态误报——等下一波全完成再查。
		if !recMtimesMatch(rec.filesMtime) {
			continue
		}
		sub := d.runContractChecks(rec.contract, rec.files)
		rep.entries = append(rep.entries, sub.entries...)
		rep.violations = append(rep.violations, sub.violations...)
		checked++
	}
	if checked == 0 {
		return
	}
	// 违例去重分流（TODO #70）：新违例正常推送；已推过且仍未修复的违例
	// 收敛为升级提示行（不进 violations 打回正文，只出现在报告尾部）。
	var fresh, known []contractViolation
	var knownNotes []string
	now := time.Now()
	for _, v := range rep.violations {
		fp := violationFingerprint(parentID, v)
		if _, pushed := d.pushedViolations.Load(fp); !pushed {
			d.pushedViolations.Store(fp, struct{}{})
			d.pushedViolationTimes.Store(fp, now)
			fresh = append(fresh, v)
			continue
		}
		firstAt := now
		if t, ok := d.pushedViolationTimes.Load(fp); ok {
			if tt, ok2 := t.(time.Time); ok2 {
				firstAt = tt
			}
		}
		known = append(known, v)
		knownNotes = append(knownNotes, fmt.Sprintf("- %s: 已知违例仍未修复（首次报告于 %s），修复后下一波自动复验", v.file, firstAt.Format("15:04")))
	}
	body := contractReportText(rep)
	if len(knownNotes) > 0 {
		body += "\n" + strings.Join(knownNotes, "\n")
	}
	if len(fresh) > 0 {
		body = failureMarker(FailureKindContractViolation, false) +
			"\n跨域契约机器校验失败（dispatcher 执行），按文件归属打回责任域：\n" +
			contractViolationText(contractReport{entries: rep.entries, violations: fresh}) + "\n\n" + body
	} else if len(known) > 0 {
		// 全部违例均已推过：不再带 failure marker 重推打回正文，只发升级提示
		//（避免同一批违例反复打回占用父 Agent 决策轮次）。
		body = "跨域契约复验：仍有 " + fmt.Sprintf("%d", len(known)) + " 条已报告违例未修复，未重发明细。\n\n" + body
	}
	if _, err := d.mailbox.Send(&mailbox.Message{
		From:    "dispatcher",
		To:      parentID,
		Type:    mailbox.MsgInfo,
		Subject: "跨域契约机器校验: " + parentID,
		Body:    body,
	}); err != nil {
		log.Printf("[subagent] contract check notify dead-letter: to=%s err=%v", parentID, err)
	}
}

// violationFingerprint 计算契约违例指纹（TODO #70 去重键）：parent + 文件 + 明细。
func violationFingerprint(parentID string, v contractViolation) string {
	return parentID + "\x00" + v.file + "\x00" + v.detail
}

// subAgentMeta 存子 Agent 巡检所需元数据：cancel 用于主动取消卡死子 Agent ctx；
// parentID/sessionID 用于 notify 父与 treeFinish；doneOnce 保证 trackChildDone 仅触发一次
// （patrol 与 goroutine 竞争时防双递减，PendingChildren 不会为负）。
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

// WithAgentsMDMaxRunes 配置 AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune）；
// <=0 关闭注入。bootstrap 从 config.AgentsMDMaxRunes 注入（默认 4000）。
func (d *Dispatcher) WithAgentsMDMaxRunes(n int) *Dispatcher {
	d.agentsMDMaxRunes = n
	return d
}

// projectBriefPrefix 读取 workDir 根部项目自述并渲染为【项目自述】前缀段（TODO 第10项⑦）。
// 关闭/缺失时返回空串（零注入）。mtime 缓存命中时零读取（project 包进程级缓存）。
func (d *Dispatcher) projectBriefPrefix(ctx context.Context) string {
	if d.agentsMDMaxRunes <= 0 {
		return ""
	}
	brief := project.LoadProjectBrief(d.subAgentWorkDirFor(ctx), d.agentsMDMaxRunes)
	if brief == "" {
		return ""
	}
	return "【项目自述】\n" + brief
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
// activityReporterFn 返回注入 ReActAgent 的活动上报闭包（WithActivityReporter）。
// Load-per-call 而非捕获指针：热驻 domain 的 activity 条目会被 enterIdle/挂起收尾
// Delete 后由 rearmSlotActivity 重建为新证据，闭包捕获旧指针会写进已废弃条目。
func (d *Dispatcher) activityReporterFn(agentID string) func(kind string) {
	return func(kind string) {
		now := time.Now().UnixNano()
		if kind == "child_wait" {
			// 展示态专用（编排页等待下级标识，waitForChildren 上报）：
			// 标记 waitingChildren 供 ActivityEvidenceOf 读取；不刷新 lastTS（不续命）、
			// 不向上冒泡——等子期间存活判定仍由后代活动冒泡与既有心跳阈值决定，
			// 避免"全部后代已死、父在干等"被展示态刷新误判为合法存活。
			// 该标记在后代冒泡期间保持（见 activityEvidence.stamp），否则展示态与
			// 直连发送闸门会被 descendant 冒泡秒刷掉。
			if e := d.activityEvidenceFor(agentID); e != nil {
				e.markChildWait()
			}
			return
		}
		if e := d.activityEvidenceFor(agentID); e != nil {
			e.report(kind, now)
		}
		d.bubbleActivity(agentID, now)
	}
}

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
		if e := d.activityEvidenceFor(meta.parentID); e != nil {
			e.stamp("descendant", now)
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

// recentActState 是单个子 Agent 的最近活动缓存（kill 时机械归纳"已完成事项"用：
// ctx 已取消无法再起 LLM 总结，只能取杀前最后一手信息）。
type recentActState struct {
	mu       sync.Mutex
	lastText string   // 最近一次 LLM 叙述片段（llm_delta 累积文本）
	tools    []string // 最近工具调用行（新→旧）
}

// recentActMaxTools 是保留的最近工具调用条数上限。
const recentActMaxTools = 6

// recordRecentActivity 在 liveFn 拦截点记录最近叙述与工具调用（低频事件，量级同
// 工具调用次数；llm_delta 每次仅覆盖 lastText 一个字符串）。
func (d *Dispatcher) recordRecentActivity(subAgentID string, ev agent.LiveEvent) {
	switch ev.Kind {
	case agent.LiveEventLLMDelta:
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			return
		}
		v, _ := d.recentActs.LoadOrStore(subAgentID, &recentActState{})
		st := v.(*recentActState)
		st.mu.Lock()
		st.lastText = textutil.TruncateRunes(text, 300, "…")
		st.mu.Unlock()
	case agent.LiveEventToolCall:
		line := strings.Join(strings.Fields(ev.Input), " ")
		v, _ := d.recentActs.LoadOrStore(subAgentID, &recentActState{})
		st := v.(*recentActState)
		st.mu.Lock()
		next := []string{ev.Tool + "(" + textutil.TruncateRunes(line, 80, "…") + ")"}
		next = append(next, st.tools...)
		if len(next) > recentActMaxTools {
			next = next[:recentActMaxTools]
		}
		st.tools = next
		st.mu.Unlock()
	}
}

// recentActivitySummary 渲染 kill 回告附言段；无记录返回空串。
func (d *Dispatcher) recentActivitySummary(subAgentID string) string {
	v, ok := d.recentActs.Load(subAgentID)
	if !ok {
		return ""
	}
	st := v.(*recentActState)
	st.mu.Lock()
	defer st.mu.Unlock()
	var b strings.Builder
	if st.lastText != "" {
		b.WriteString("- 最近自述：" + st.lastText + "\n")
	}
	if len(st.tools) > 0 {
		b.WriteString("- 最近工具调用（由新到旧）：" + strings.Join(st.tools, "、"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// PingActivity 外部保活探针（等待用户答复场景）：审批/提问阻塞期间由会话层周期性调用，
// 以 user_wait 证据刷新该 Agent 活动时间并沿父链冒泡，防止巡检把"等用户操作"
// 误判为假死 kill。agentID 未注册（meta/已终结）时静默跳过。
func (d *Dispatcher) PingActivity(agentID string) {
	if agentID == "" {
		return
	}
	now := time.Now().UnixNano()
	if e := d.activityEvidenceFor(agentID); e != nil {
		e.report("user_wait", now)
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

// scanStuck 扫描 activity map，对超阈值无有效活动的子 Agent 执行 killStuckSubAgent。
// 叶子按 heartbeatTimeout；domain 按 domainHeartbeatTimeout（默认 2× 叶子，
// 等子/等回信期间靠后代活动冒泡保活，防误杀合法等待，TODO #25-3）。
// 证据化判定（TODO 第10项②）：
//  1. 在飞 LLM（llmInFlight，含引擎 judge）→ 豁免——慢思考单呼可远超阈值，流式 chunk、
//     流空闲卡口、调用超时与会话墙钟另有兜底；
//  2. 工具在飞超阈值（toolStartTS）→ 杀——真挂死工具（keepalive 盲报不再续命）；
//  3. 步间静默超阈值（lastTS）→ 杀——步间死锁。
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
		e := v.(*activityEvidence)
		threshold := d.heartbeatTimeout
		if roleIDFromAgentID(k.(string)) == "domain" {
			threshold = domainThreshold
		}
		// 1. 在飞 LLM：豁免（只展示不杀）。
		if e.llmInFlight.Load() {
			return true
		}
		// 2. 工具在飞超阈值：真挂死工具。
		if ts := e.toolStartTS.Load(); ts > 0 && now-ts > int64(threshold) {
			d.killStuckSubAgent(k.(string), fmt.Sprintf("tool=%s in-flight=%s", e.toolNameString(), time.Duration(now-ts)))
			return true
		}
		// 3. 步间静默超阈值。
		if last := e.lastTS.Load(); now-last > int64(threshold) {
			d.killStuckSubAgent(k.(string), fmt.Sprintf("last_kind=%s quiet=%s", e.kindString(), time.Duration(now-last)))
			return true
		}
		return true
	})
}

// killStuckSubAgent 主动取消假死子 Agent：cancel ctx + 兜底 trackChildDone + notify 父 +
// 树节点置 Failed + 清理 activity/subMeta/running。runSubAgent goroutine 若因 cancel 返回，
// 其 doneOnce.Do 为 no-op；若不尊重 ctx（流式挂起），此处 doneOnce 兜底递减防父永久空等。
// evidence 为 scanStuck 判定的活动证据摘要（kind/工具名/静默时长），进日志与父通知文案。
func (d *Dispatcher) killStuckSubAgent(subAgentID, evidence string) {
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
	log.Printf("[subagent] STALL KILL: sub=%s parent=%s threshold>%s evidence=[%s] - cancel+notify",
		subAgentID, meta.parentID, threshold, evidence)
	// cancel 可能为 nil：热驻 domain 槽的 subMeta 在任务 ctx 创建前注册（idle_pool
	// dispatchHotDomain），首任务换绑前/挂起等用户续跑期间被巡检命中时（2026-08-26
	// 实证进程级 panic）不得空指针崩溃。nil 时跳过主动 cancel，仍走 doneOnce 兜底
	// 递减 + notify 收尾。
	if meta.cancel != nil {
		meta.cancel()
	}
	meta.doneOnce.Do(func() { d.trackChildDone(meta.parentID) })
	// retryable=true（2026-08-27）：killed 实际多死于验证阶段长工具执行中，盘上产物
	// 大概率可续建——retryable=false 曾误导父 LLM 从零重派（fruit 任务实证）。
	// 自动重派策略不受影响：runSubAgentWithAutoRetry 只认 kind=error+叶子，标记仅供父决策。
	killMsg := failureMarker(FailureKindKilled, true) + "\n" +
		fmt.Sprintf("子 Agent %s 超过 %s 无有效活动（证据：%s），判定假死已主动取消。", subAgentID, threshold, evidence)
	// 盘上产物回告（2026-08-19）：收尾卡死被杀的子 Agent 常已把文件全部落盘，
	// 只是回执被挂死的收尾自检拖住--父级据此可按盘上现状直接验收，不必从零重派。
	if paths := d.recentWrittenFiles(subAgentID, 30*time.Minute); len(paths) > 0 {
		killMsg += "\n该 Agent 近期已写入以下文件（盘上产物大概率可用，可按盘上现状直接验收，无需从零重派）：\n- " +
			strings.Join(paths, "\n- ")
	}
	// 杀前最后一手信息（叙述片段+工具轨迹）：机械归纳已完成事项回传父 Agent，
	// 供其判断续建范围而非重派。
	if s := d.recentActivitySummary(subAgentID); s != "" {
		killMsg += "\n\n终止前活动摘要：\n" + s
	}
	d.notify(meta.parentID, subAgentID, killMsg, nil)
	if d.treeFn != nil && meta.sessionID != "" {
		if t := d.treeFn(meta.sessionID); t != nil {
			t.Finish(subAgentID, "心跳超时疑似卡死", errors.New("heartbeat timeout"))
		}
	}
	// 被杀节点不再续跑（热驻槽随后走 destroySlot），回收实例级模型覆盖。
	d.clearAgentModel(subAgentID)
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
	// 级联停止：被杀 Agent 名下所有后代子 Agent 一并取消（防孤儿叶子继续空跑，
	// 其结果无人消费）。每代复用 killStuckSubAgent 完整收尾链。
	d.killDescendants(subAgentID, meta.sessionID)
	d.activity.Delete(subAgentID)
	d.running.Delete(subAgentID)
	if d.mailbox != nil {
		d.mailbox.Purge(subAgentID)
	}
}

// killDescendants BFS 遍历树快照，递归对后代调用 killStuckSubAgent（cancel + notify 父 +
// tree.Finish + salvage + purge）。只处理 Running/Paused 节点；LoadAndDelete 幂等 +
// visited 防环，重复杀无害。
func (d *Dispatcher) killDescendants(rootID, sessionID string) {
	if sessionID == "" || d.treeFn == nil {
		return
	}
	t := d.treeFn(sessionID)
	if t == nil {
		return
	}
	visited := map[string]bool{rootID: true}
	queue := []string{rootID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, n := range t.Snapshot() {
			if n.ParentID != pid || visited[n.ID] {
				continue
			}
			if n.Status != orchestrator.StatusRunning && n.Status != orchestrator.StatusPaused {
				continue
			}
			visited[n.ID] = true
			queue = append(queue, n.ID)
			log.Printf("[subagent] CASCADE KILL: parent=%s child=%s", pid, n.ID)
			d.killStuckSubAgent(n.ID, "cascade")
		}
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

// InjectUserMessage 用户直连注入（编排页对话面板）：向目标 Agent 邮箱投一封
// From="user" 的 MsgRequest，并 pokeParent 唤醒其 wait loop——mailbox.Send 本身
// 不唤醒 WaitForAnyChild 阻塞方，不显式 poke 要等满 wait 周期才看到消息。
func (d *Dispatcher) InjectUserMessage(agentID, content string) error {
	if d.mailbox == nil {
		return fmt.Errorf("mailbox 未初始化")
	}
	if _, err := d.mailbox.Send(&mailbox.Message{
		From:    "user",
		To:      agentID,
		Type:    mailbox.MsgRequest,
		Subject: "用户直连消息",
		Body:    content,
	}); err != nil {
		return err
	}
	d.pokeParent(agentID)
	return nil
}

// ReviveWithMessage 复活终态子 Agent 并以用户消息为增量输入同 ID 重跑。
// 种子 = 原任务 + 上轮 Summary/Err + 用户新消息；运行骨架完全镜像 dispatchOne
// （ctx 重建 → Reopen+SetCancel → subMeta/activity/ensurePatrol → goroutine
// runSubAgent → 父 poke+邮件通知 → ledger 重记）。
func (d *Dispatcher) ReviveWithMessage(ctx context.Context, node orchestrator.Node, userMsg string) error {
	roleDef := d.registry.Get(node.Role)
	if roleDef == nil {
		return fmt.Errorf("角色 %s 未注册，无法复活", node.Role)
	}
	parentID := node.ParentID
	subAgentID := node.ID

	subAgentCtx := tool.StopContextFrom(ctx)
	if subAgentCtx == nil {
		subAgentCtx = context.Background()
	}
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	subAgentCtx = tool.WithWorkDir(subAgentCtx, d.subAgentWorkDirFor(ctx))
	// 墙钟与派发路径同口径：domain 未显式给出预算时用侦察墙钟兜底（d.timeout 是全局上限），
	// 否则复活一个侦察失控的 domain 会拿满全局墙钟（默认 30min）继续空转。
	effectiveTimeout := d.timeout
	if roleDef.ID == "domain" && d.domainReconClock > 0 &&
		(effectiveTimeout <= 0 || d.domainReconClock < effectiveTimeout) {
		effectiveTimeout = d.domainReconClock
	}
	var cancel context.CancelFunc = func() {}
	if effectiveTimeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, effectiveTimeout)
	}

	if d.treeFn == nil {
		cancel()
		return fmt.Errorf("权威树未接线，无法复活 %s", subAgentID)
	}
	t := d.treeFn(tool.SessionIDFromContext(subAgentCtx))
	if t == nil || !t.Reopen(subAgentID) {
		cancel()
		return fmt.Errorf("节点 %s 非终态，不可复活", subAgentID)
	}
	t.SetCancel(subAgentID, cancel)

	// 复活种子：原任务 + 上轮结果留痕 + 用户新指令，让模型明确"这是返工/追加"。
	seed := node.Task + "\n\n【上一轮结果】\n" + node.Summary
	if node.Err != "" {
		seed += "\n【上轮错误】\n" + node.Err
	}
	seed += "\n\n【用户直连消息】\n" + userMsg

	if parentID != "" {
		d.trackChildStart(parentID)
	}
	// 复活必须重开邮箱：原 run 退出路径已 Purge 该 ID（closed 标记永久），不撤销则
	// 新起的下游子 Agent 回传与用户直连注入全部死信（结果静默丢失、wait loop 空手退出）。
	if d.mailbox != nil {
		d.mailbox.Reopen(subAgentID)
	}
	// 复活前清消息热层：新 run 的 history 从 0 重新编号，与旧 run 的 seq 重叠会让
	// 对话页增量游标（after_seq=旧最大值）再也取不到新消息、面板出现重复 seq 条目。
	if d.msgLogger != nil {
		d.msgLogger.Clear(subAgentID)
	}
	// 看板回写：该领域对应计划条目从"完成/失败"翻回进行中，否则面板显示已完成、
	// 实际又在重跑（与派发路径同口径）。
	d.boardAssign(ctx, parentID, node.Domain, subAgentID)
	meta := &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: tool.SessionIDFromContext(subAgentCtx), wallClock: effectiveTimeout}
	d.subMeta.Store(subAgentID, meta)
	ev := newEvidence()
	if roleDef.ID != "meta" {
		d.activity.Store(subAgentID, ev)
	}
	d.ensurePatrol()
	started := time.Now()
	go func() {
		defer cancel()
		// CompareAndDelete：只清自己登记的那份（见 dispatchOne 同名注释）。
		defer d.subMeta.CompareAndDelete(subAgentID, meta)
		defer d.activity.CompareAndDelete(subAgentID, ev)
		defer d.lastWrites.Delete(subAgentID)
		defer d.heldSkills.Delete(subAgentID)
		// 复活按默认 ReAct 模式重跑（mode=react、verify_kind 留空走角色默认校验分层）——
		// 原派发的 mode/responsibility 未随节点持久化，无法复原；职责边界由种子里的
		// 原任务文本与领域标签承载，必要时模型可自行重新派发下游。
		paused := d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, seed, node.Domain, "", agent.ModeReact, "", started)
		if !paused && parentID != "" {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()
	// 父感知（提示词 Task 8 配套）：复活返工属调度事实，邮件通知父"等重新回传，勿重复派发"。
	if parentID != "" && d.mailbox != nil {
		_, _ = d.mailbox.Send(&mailbox.Message{
			From: "dispatcher", To: parentID, Type: mailbox.MsgInfo,
			Subject: "子 Agent 复活返工",
			Body:    fmt.Sprintf("子 Agent %s 已被用户直连复活重跑，等待其重新回传，勿重复派发同领域任务。", subAgentID),
		})
		d.pokeParent(parentID)
	}
	if sid := tool.SessionIDFromContext(subAgentCtx); sid != "" {
		d.ledger.RecordDispatch(sid, parentID, subAgentID, node.Domain, truncateRunes(node.Task, 80), "")
	}
	return nil
}

// HasPausedChild 返回父 Agent 是否有 StatusPaused 的子 DomainAgent 节点。
// 实现 agent.PausedChildChecker 接口，供 MetaAgent 父终结保护 wait loop 检测：
// 子 domain 触达 token 上限进入 Paused 后，父 MetaAgent 无限 budget 不会自行暂停，
// 在 wait loop 中调此方法检测，命中则跳出返回 PausedOnChild，由上层 pauseSession
// 置会话暂停态，等用户"继续"恢复该 domain（各 Agent 独立上下文）。
//
// 仅认 domain（2026-09-10 收窄）：暂停的叶子是父 domain 的自主中转态（解药是 domain
// 经 pause/resume/换档自行处置），若计入会让 meta 误转会话 PausedOnChild 要求用户
// "继续"；domain 无 pausedChecker，靠暂停通知邮件驱动处置。
//
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
		if n.ParentID == parentID && n.Status == orchestrator.StatusPaused && n.Role == "domain" {
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
		ledger:            newTaskLedger(),
		timeout:           30 * time.Minute, // 默认 30 分钟，可用 WithTimeout 覆盖；<=0 表示不限制
		salvageTimeout:    30 * time.Second, // 默认 30s，可用 WithSalvageTimeout 覆盖（TODO #33）
		taskRuneSoftLimit: 3000,             // task 文本软上限（TODO #35），WithTaskRuneLimits 覆盖
		taskRuneHardLimit: 4000,
		softStops:         make(map[string]bool),
		pauseRequests:     make(map[string]bool),
		planState:         newPlanConfirmState(),
		batchDigestEnabled: true, // call_sub_agents 波聚合默认开（config agent.batch_digest_enabled 可关）
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

// MarkPauseNode 标记单个节点为"手动暂停中"（TODO 第9⑥/10③ 审计面）：
// ReactService.PauseAgent 先标记再 StopRunning，context.Canceled 收尾分支据此把
// 该节点按软停语义落 Paused（SaveMessages + tree.Pause），不影响会话其余节点。
func (d *Dispatcher) MarkPauseNode(nodeID string) {
	if d == nil || nodeID == "" {
		return
	}
	d.pauseRequestMu.Lock()
	defer d.pauseRequestMu.Unlock()
	if d.pauseRequests == nil {
		d.pauseRequests = make(map[string]bool)
	}
	d.pauseRequests[nodeID] = true
}

// ClearPauseNode 清除节点手动暂停标记（收尾落 Pause 后/竞态时调用）。幂等。
func (d *Dispatcher) ClearPauseNode(nodeID string) {
	if d == nil || nodeID == "" {
		return
	}
	d.pauseRequestMu.Lock()
	defer d.pauseRequestMu.Unlock()
	delete(d.pauseRequests, nodeID)
}

// isPauseRequested 查询节点是否被手动暂停标记。
func (d *Dispatcher) isPauseRequested(nodeID string) bool {
	if d == nil || nodeID == "" {
		return false
	}
	d.pauseRequestMu.Lock()
	defer d.pauseRequestMu.Unlock()
	return d.pauseRequests[nodeID]
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

// WithProjectPreferences 注入项目偏好读取回调（2026-09-02 设计 §5）：
// bootstrap 接 userprofile.ProjectStore.Current(ctx).Content（按 ctx 会话目录解析，S2）；
// 派发前缀拼【项目偏好】段下发所有子 Agent（项目经验是执行层要遵守的工艺）。
// 传 nil（或回调返回空串）时不注入。
func (d *Dispatcher) WithProjectPreferences(fn func(ctx context.Context) string) *Dispatcher {
	d.projectPrefs = fn
	return d
}

// WithSessionGearResolver 注入会话档位只读回调（TODO #14 T22）：
// bootstrap 接 agentSvc.SessionGear；enterIdle 固化与隐式复用裁决用。nil 时守卫全放行。
func (d *Dispatcher) WithSessionGearResolver(fn func(sessionID string) string) *Dispatcher {
	d.sessionGearFn = fn
	return d
}

// sessionGearOf nil 安全读会话档位；回调未接线或返回空串均得空串（空=守卫放行）。
func (d *Dispatcher) sessionGearOf(sessionID string) string {
	if d.sessionGearFn == nil {
		return ""
	}
	return d.sessionGearFn(sessionID)
}

// SkillHint 经验技能召回提示（设计 §6.5：只注一行提示，不注全文）。
type SkillHint struct {
	Name  string
	Title string
}

// WithSkillRecall 注入经验技能向量预答回调（设计 §6.5）：
// bootstrap 接 learned_skills 向量检索（goal/task embedding top-3，相似度阈值过滤）。
// 派发前缀拼【相关经验】段（每技能一行提示）；nil 时零注入。
func (d *Dispatcher) WithSkillRecall(fn func(ctx context.Context, task string) []SkillHint) *Dispatcher {
	d.skillRecall = fn
	return d
}

// WithSkillUseCounter 注入 learned 技能 load 计数回调（设计 §6.5）。
func (d *Dispatcher) WithSkillUseCounter(fn func(name string) error) *Dispatcher {
	d.skillUseCounter = fn
	return d
}

// skillRecallPrefix 渲染【相关经验】派发前缀段；无回调/无命中返回空串（零注入）。
func (d *Dispatcher) skillRecallPrefix(ctx context.Context, task string) string {
	if d.skillRecall == nil || strings.TrimSpace(task) == "" {
		return ""
	}
	hints := d.skillRecall(ctx, task)
	if len(hints) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【相关经验】\n以下经验技能与当前任务相关，可用 load_skill(名称) 获取完整工艺指引：\n")
	for _, h := range hints {
		b.WriteString("- ")
		b.WriteString(h.Name)
		b.WriteString(" —— ")
		b.WriteString(h.Title)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// projectPrefsRunes 项目偏好段注入的 rune 上限（与 MetaAgent 侧 persona 段一致）。
const projectPrefsRunes = 2000

// projectPrefsPrefix 渲染【项目偏好】派发前缀段；空偏好返回空串（零注入）。
func (d *Dispatcher) projectPrefsPrefix(ctx context.Context) string {
	if d.projectPrefs == nil {
		return ""
	}
	content := strings.TrimSpace(d.projectPrefs(ctx))
	if content == "" {
		return ""
	}
	if len([]rune(content)) > projectPrefsRunes {
		content = string([]rune(content)[:projectPrefsRunes]) + "\n...（项目偏好截断）"
	}
	return "【项目偏好】\n" + content + "\n（以上是本项目的约定与经验，执行任务时遵守）"
}

// WithSkillPool 注入全局技能池（技能渐进披露 + 树形分发）。传 nil 时技能参数、
// 技能工具、【可用技能】提示块全部零行为（测试/未配置场景）。
func (d *Dispatcher) WithSkillPool(p *skill.Pool) *Dispatcher {
	d.skillPool = p
	return d
}

// fixedSkillNames 把角色固定技能列表解析为池内规范名（Name 优先，缺 Name 回退
// SkillID）；未知项跳过并记日志。roleDef 为 nil 时返回 nil。
func fixedSkillNames(pool *skill.Pool, roleDef *types.RoleDefinition) []string {
	if pool == nil || roleDef == nil || len(roleDef.Skills) == 0 {
		return nil
	}
	var out []string
	for _, key := range roleDef.Skills {
		s := pool.FindByNameOrID(strings.TrimSpace(key))
		if s == nil {
			log.Printf("[subagent] skill: role %s 固定技能 %q 不在池中，跳过", roleDef.ID, key)
			continue
		}
		name := s.Name
		if name == "" {
			name = s.SkillID
		}
		out = append(out, name)
	}
	return out
}

// assignableSkills 返回父 Agent 可分配给下级的技能查找表：键为池内技能的 Name
// 与 SkillID（两种写法都接受），值为规范展示名。meta 持全池；其他 Agent 只持
// heldSkills 已登记集合（未登记/为空 = 仅角色固定集由 resolveChildSkills 单独并入）。
func (d *Dispatcher) assignableSkills(agentID string) map[string]string {
	out := map[string]string{}
	if d.skillPool == nil {
		return out
	}
	if roleIDFromAgentID(agentID) == "meta" {
		for _, s := range d.skillPool.All() {
			name := s.Name
			if name == "" {
				name = s.SkillID
			}
			out[s.SkillID] = name
			if s.Name != "" {
				out[s.Name] = name
			}
		}
		return out
	}
	v, ok := d.heldSkills.Load(agentID)
	if !ok {
		return out
	}
	for _, n := range v.([]string) {
		s := d.skillPool.FindByNameOrID(n)
		if s == nil {
			continue
		}
		out[s.SkillID] = n
		if s.Name != "" {
			out[s.Name] = n
		}
	}
	return out
}

// resolveChildSkills 解析子 Agent 持有集 = 角色固定集（直接并入，不经父权限）
// ∪ 父分配集（skillsHint 逐项校验 ⊆ 父持有集，越界项返回 rejected）。
// skillPool 为 nil 时零行为（空集）。
func (d *Dispatcher) resolveChildSkills(parentID string, roleDef *types.RoleDefinition, skillsHint []string) (held []string, rejected []string) {
	if d.skillPool == nil {
		return nil, nil
	}
	held = fixedSkillNames(d.skillPool, roleDef)
	assignable := d.assignableSkills(parentID)
	for _, key := range skillsHint {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		name, ok := assignable[key]
		if !ok {
			rejected = append(rejected, key)
			continue
		}
		if !containsString(held, name) {
			held = append(held, name)
		}
	}
	return held, rejected
}

// containsString 判断切片中是否含指定字符串。
func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// skillBlockFor 渲染指定 Agent 实例的【可用技能】系统提示块（渐进披露第一层：
// 仅名称+一句话描述）。持有集以 heldSkills 为权威，未登记回退角色固定集；
// skillPool 为 nil 或结果为空时返回空串（不注入）。
func (d *Dispatcher) skillBlockFor(agentID string, roleDef *types.RoleDefinition) string {
	if d.skillPool == nil {
		return ""
	}
	names := []string(nil)
	if v, ok := d.heldSkills.Load(agentID); ok {
		names = v.([]string)
	}
	if len(names) == 0 {
		names = fixedSkillNames(d.skillPool, roleDef)
	}
	return skill.MetadataBlock(d.skillPool, names)
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

// WithUserNotify 注入用户对话页系统消息回调（验收闭环进度通告用）。
// fn(sessionID, msg) 由 bootstrap 接线到 ReactService 的 addEvent 包装；nil 时静默跳过。
func (d *Dispatcher) WithUserNotify(fn func(sessionID, msg string)) *Dispatcher {
	d.userNotifyFn = fn
	return d
}

// WithChildDoneNotify 注入子 Agent 完成回调（「挂起等子」awaiting_child 唤醒用）。
// fn(parentID) 由 bootstrap 接线到 ReactService.WakeOnChildDone；nil 时跳过。
func (d *Dispatcher) WithChildDoneNotify(fn func(parentID string)) *Dispatcher {
	d.childDoneFn = fn
	return d
}

// WithSessionWake 注入挂起会话唤醒回调（邮箱请求死信修复）：submit_plan 审批请求
// 与 send_message request/escalate Send 成功后调用 fn(parentID, hint)，唤醒
// awaiting_child 挂起的上级会话续跑 drain 邮箱（返回是否完成翻态，调用方不消费）。
// 由 bootstrap 接线到 ReactService.WakeSuspended；nil 时跳过。
func (d *Dispatcher) WithSessionWake(fn func(parentID, hint string) bool) *Dispatcher {
	d.sessionWakeFn = fn
	return d
}

// WithSummaryMerger 注入整合纪要合成器（call_sub_agents 波聚合，C-3a）：
// 同波各领域回传经 Merge 合成一条紧凑纪要单条送达父邮箱。nil 时回退逐行拼接。
func (d *Dispatcher) WithSummaryMerger(m SummaryMerger) *Dispatcher {
	d.summaryMerger = m
	return d
}

// WithBatchDigest 开关 call_sub_agents 波聚合整合纪要（config agent.batch_digest_enabled
// 逃生舱）：false 时退回逐条回传。
func (d *Dispatcher) WithBatchDigest(enabled bool) *Dispatcher {
	d.batchDigestEnabled = enabled
	return d
}

// SummaryMerger 整合纪要合成器（call_sub_agents 波聚合消费）：把同波各领域回传
// 合成一条紧凑纪要（按领域归并事实、冲突点单列、保留文件清单与验证状态）。
// Merge 失败或未注入时调用方回退逐行拼接（fail-open）。
type SummaryMerger interface {
	Merge(ctx context.Context, goal string, entries []DigestEntry) (string, error)
}

// DigestEntry 波聚合中单个领域的回传项（Files 取自任务台账，不进 mapAggregation）。
type DigestEntry struct {
	Domain  string
	Summary string
	OK      bool
	Files   []string
}

// digestMergeTimeout 整合纪要合成（轻量 LLM）的单次超时；超时回退逐行拼接。
const digestMergeTimeout = 2 * time.Minute

// deliverWaveDigest 整合纪要单条送达父邮箱：优先注入的 SummaryMerger 归并，
// 失败/未注入回退逐行拼接（fail-open）。经 notify 咽喉走台账 + >4000 runes 落盘收口。
func (d *Dispatcher) deliverWaveDigest(batchID, parentID, goal string, entries []DigestEntry) {
	text := ""
	if d.summaryMerger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), digestMergeTimeout)
		merged, err := d.summaryMerger.Merge(ctx, goal, entries)
		cancel()
		if err != nil {
			log.Printf("[subagent] wave digest merge failed (fallback concat): batch=%s err=%v", batchID, err)
		} else {
			text = merged
		}
	}
	if strings.TrimSpace(text) == "" {
		var b strings.Builder
		okCount := 0
		for _, e := range entries {
			status := "失败"
			if e.OK {
				status = "完成"
				okCount++
			}
			b.WriteString(fmt.Sprintf("【%s】[%s] %s\n", e.Domain, status, truncateRunes(firstLine(e.Summary), 300)))
			if len(e.Files) > 0 {
				b.WriteString("  文件: " + strings.Join(e.Files, ", ") + "\n")
			}
		}
		b.WriteString(fmt.Sprintf("\n共 %d 个领域：完成 %d / 失败 %d。全文见任务台账或各子 Agent 回传。", len(entries), okCount, len(entries)-okCount))
		text = b.String()
	}
	d.notify(parentID, batchID, "【整合纪要】\n"+text, nil)
}

// deliverAbandonedWaveItem 波聚合放弃后的单项直发（成功派出 domain <2 回退旧
// 逐条回传行为）：不经 notify（其台账登记与聚合拦截已在原路径完成），只投邮箱。
func (d *Dispatcher) deliverAbandonedWaveItem(parentID, domain, summary string) {
	if d.mailbox == nil {
		return
	}
	_, _ = d.mailbox.Send(&mailbox.Message{
		From:    domain,
		To:      parentID,
		Type:    mailbox.MsgInfo,
		Subject: "子 Agent 完成: " + domain,
		Body:    d.returnBodyFor(domain, summary),
	})
}

// returnBodyFor 回传正文收口：超阈值全文落盘，邮箱只留摘要头 + 全文路径；
// 落盘失败降级原样发送（notify 是 best-effort，不因收口失败丢消息）。
func (d *Dispatcher) returnBodyFor(subAgentID, summary string) string {
	if runeLen(summary) <= mailboxReturnDumpRunes {
		return summary
	}
	path, dumpErr := d.dumpReturnToDisk(subAgentID, summary)
	if dumpErr != nil {
		log.Printf("[subagent] return dump failed (degrade to full body): sub=%s err=%v", subAgentID, dumpErr)
		return summary
	}
	log.Printf("[subagent] return dumped: sub=%s path=%s total=%d runes", subAgentID, path, runeLen(summary))
	return truncateRunes(summary, mailboxReturnDigestRunes) + "\n\n【全文已落盘】" + path
}

// wakeSuspendedParent 唤醒挂起中的上级会话（nil 安全，幂等）：仅目标为顶层会话且
// 处于 awaiting_child 时生效，其余自然空转。
func (d *Dispatcher) wakeSuspendedParent(parentID, hint string) {
	if d.sessionWakeFn == nil || parentID == "" {
		return
	}
	d.sessionWakeFn(parentID, hint)
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

// WithMessageLogger 注入消息热层记录器（编排页对话视图）。nil 时跳过（测试场景）。
func (d *Dispatcher) WithMessageLogger(l agent.MessageLogger) *Dispatcher {
	d.msgLogger = l
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

// WithPlanConfirmation 配置计划确认机制（plan_confirm.go）：enabled 开启后 submit_plan
// 阻塞等待上级/用户确认，关闭时直通不阻塞；timeout 单次审批等待上限（<=0 用默认 10m，
// 超时 fail-open 按计划继续）；maxRevisions 每 Agent 被驳回重提上限（<=0 不限制，
// 循环直到批准；>0 时达上限转 escalate 仲裁仍不放行）。bootstrap 按 cfg.Agent 注入。
func (d *Dispatcher) WithPlanConfirmation(enabled bool, timeout time.Duration, maxRevisions int) *Dispatcher {
	d.planConfirmEnabled = enabled
	d.planConfirmTimeout = timeout
	d.planMaxRevisions = maxRevisions
	if d.planState == nil {
		d.planState = newPlanConfirmState()
	}
	return d
}

// RegisterPlanTools 将 submit_plan / review_plan 工具安装到传入的工具注册表中
// （plan_confirm.go 计划确认机制）。角色可见性由 role.Registry 的 meta/domain
// 内置工具白名单 + roles.yaml 覆盖控制。
func (d *Dispatcher) RegisterPlanTools(r *tool.Registry) {
	r.Register(&submitPlanTool{dispatcher: d})
	r.Register(&reviewPlanTool{dispatcher: d})
}

// RegisterCallTool 将 call_sub_agent / call_sub_agents 工具安装到传入的工具注册表中。
// 工具被注册到父 Agent 与子 Agent 共同使用的 registry 上，
// 递归深度固定三层：MetaAgent -> DomainAgent -> 叶子助手（CanCall 拒绝 domain->domain 平级派发）。
func (d *Dispatcher) RegisterCallTool(r *tool.Registry) {
	// 注册 callSubAgentTool / callSubAgentsTool / mapSubAgentsTool 实例，工具内部持有当前 Dispatcher 以便执行时调用。
	r.Register(&callSubAgentTool{dispatcher: d})
	r.Register(&callSubAgentsTool{dispatcher: d})
	r.Register(&mapSubAgentsTool{dispatcher: d})
	// merge_worktree（TODO 第9⑤/#10⑤）：worktree 隔离派发的合并门工具，白名单仅 meta。
	r.Register(&mergeWorktreeTool{dispatcher: d})
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

	// 请求/升级类消息到达挂起（awaiting_child）的顶层会话时唤醒续跑：挂起会话
	// 不 drain 邮箱，不唤醒要等子完成或用户发消息才看到（死信）。info 单向通知
	// 不唤醒——不值得为中间信息烧上级一轮。
	if msgType == mailbox.MsgRequest || msgType == mailbox.MsgEscalate {
		if toID != fromID {
			hint := "【系统】有 Agent 发来询问（request），请查收邮箱并当轮答复。"
			if msgType == mailbox.MsgEscalate {
				hint = "【系统】有 Agent 发来升级请求（escalate），请查收邮箱处置。"
			}
			d.wakeSuspendedParent(toID, hint)
		}
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
	r.Register(&pauseAgentTool{dispatcher: d})
	r.Register(&resumeAgentTool{dispatcher: d})
	r.Register(&setAgentModelTool{dispatcher: d})
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
	// 取消即终结：回收实例级模型覆盖（热驻槽由 destroySlot 兜底，此处覆盖非热驻路径）。
	d.clearAgentModel(agentID)
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

// pauseLandingTimeout pause_agent 等待槽进入暂停落地态的上限；超时不报错（收尾可能
// 仍在保存消息），只提示 metas 稍后重试 resume。
const pauseLandingTimeout = 15 * time.Second

// pauseAgentTool 实现 pause_agent 工具：暂停运行中的 DomainAgent，上下文完整保留，
// 可换模型后经 resume_agent 从原任务续跑。语义同 ReactService.PauseAgent——先登记
// 暂停意图再触发 cancel，cancel 收尾分支（idle_pool runDomainTask / dispatcher
// runSubAgent）据标记走 Pause 落库而非销毁。
type pauseAgentTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *pauseAgentTool) Name() string { return "pause_agent" }

// Aliases 返回工具别名列表，当前无别名。
func (t *pauseAgentTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *pauseAgentTool) Description() string {
	return "暂停一个运行中的子 Agent（你直派的 domain，或你直接派的固定角色）：保存当前进度并让出执行" +
		"（停止烧 token），上下文与已写文件保留，之后可用 set_agent_model/set_role_model 换模型、" +
		"resume_agent 从原任务继续，或 cancel_agent 放弃。只能暂停自己直派的 Agent（越级会被拒绝）。"
}

// Execute 执行 pause_agent 工具调用。args 含 agent_id（必填）与 reason（可选，仅回执）。
func (t *pauseAgentTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	agentID, _ := args["agent_id"].(string)
	reason, _ := args["reason"].(string)
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return &tool.Result{Tool: "pause_agent", Error: "agent_id is required"}
	}
	if d.treeFn == nil {
		return &tool.Result{Tool: "pause_agent", Error: "agent tree not available"}
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(agentID, "/"); i > 0 {
			sid = agentID[:i]
		}
	}
	tr := d.treeFn(sid)
	if tr == nil {
		return &tool.Result{Tool: "pause_agent", Error: "agent tree not available for session"}
	}
	node, ok := tr.Get(agentID)
	if !ok {
		return &tool.Result{Tool: "pause_agent", Error: fmt.Sprintf("agent %s 不存在", agentID)}
	}
	// 授权：只能暂停自己直派的子 Agent（domain 名下的叶子归 domain 管，越级拒绝）。
	callerID := agent.AgentIDFromContext(ctx)
	if callerID != "" && callerID != node.ParentID {
		return &tool.Result{Tool: "pause_agent", Error: fmt.Sprintf(
			"agent %s 由 %s 派发，不能由 %s 越级暂停（只能管理自己直派的子 Agent）", agentID, node.ParentID, callerID),
			Category: tool.ResultCategoryValidationRejected}
	}
	if node.Status != orchestrator.StatusRunning {
		return &tool.Result{Tool: "pause_agent", Error: fmt.Sprintf(
			"agent %s 状态为 %s，仅运行中可暂停", agentID, node.Status)}
	}
	// 先登记暂停意图，再触发 cancel（顺序与 ReactService.PauseAgent 一致）；触发失败回滚标记。
	d.MarkPauseNode(agentID)
	if !tr.StopRunning(agentID) {
		d.ClearPauseNode(agentID)
		return &tool.Result{Tool: "pause_agent", Error: fmt.Sprintf("agent %s 已不在运行", agentID)}
	}
	// 等收尾落地才返回：紧随其后的 resume_agent 才能稳定命中（history 未存完就续跑
	// 会撞 "no persisted messages"）。热驻槽等 slotPaused，非热驻等树节点转 Paused。
	hot := d.hotEnabled() && d.pool != nil && d.pool.slot(sid, agentID) != nil
	parked := false
	deadline := time.Now().Add(pauseLandingTimeout)
	for time.Now().Before(deadline) {
		if hot {
			if d.slotPaused(sid, agentID) {
				parked = true
				break
			}
		} else if n, ok := tr.Get(agentID); ok && n.Status == orchestrator.StatusPaused {
			parked = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	out := fmt.Sprintf("已暂停 %s：当前任务的上下文与已写文件保留；可 set_agent_model(该 id) 换模型后 resume_agent(该 id) 续跑，或 cancel_agent 放弃。", agentID)
	if reason != "" {
		out += "（原因: " + reason + "）"
	}
	if !parked {
		out += "\n注意：收尾尚未落地（仍在保存进度），resume_agent 可能需稍后重试。"
	}
	return &tool.Result{Tool: "pause_agent", Success: true, Output: out}
}

// resumeAgentTool 实现 resume_agent 工具：唤醒被 pause_agent 暂停的热驻槽，
// 从暂停时的任务继续（模型按当前绑定解析，期间换过模型即用新模型）。
type resumeAgentTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *resumeAgentTool) Name() string { return "resume_agent" }

// Aliases 返回工具别名列表，当前无别名。
func (t *resumeAgentTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *resumeAgentTool) Description() string {
	return "恢复被 pause_agent 暂停的子 Agent：从暂停时的任务继续执行，上下文与已写文件保留，" +
		"模型按当前解析（期间 set_agent_model/set_role_model 过即用新模型）。" +
		"非热驻节点异步续跑，完成后结果照常经 mailbox 回传。只能恢复自己直派且处于暂停态的 Agent。"
}

// Execute 执行 resume_agent 工具调用。args 含 agent_id（必填）。
func (t *resumeAgentTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	agentID, _ := args["agent_id"].(string)
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return &tool.Result{Tool: "resume_agent", Error: "agent_id is required"}
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(agentID, "/"); i > 0 {
			sid = agentID[:i]
		}
	}
	// 热驻槽路径：优先（槽存活时按 opResume 唤醒，上下文在槽内）。
	if d.hotEnabled() && d.pool != nil {
		if s := d.pool.slot(sid, agentID); s != nil {
			s.mu.Lock()
			suspended := s.suspended
			if suspended {
				s.suspended = false
			}
			s.mu.Unlock()
			if !suspended {
				return &tool.Result{Tool: "resume_agent", Error: fmt.Sprintf("agent %s 未处于暂停态，无需恢复", agentID)}
			}
			select {
			case s.ops <- domainOp{kind: opResume, resumeMsg: "继续"}:
			default:
				// 指令通道拥塞：回滚标记，保持暂停态可重试。
				s.mu.Lock()
				s.suspended = true
				s.mu.Unlock()
				return &tool.Result{Tool: "resume_agent", Error: fmt.Sprintf("agent %s 指令通道繁忙，请稍后重试", agentID)}
			}
			return &tool.Result{Tool: "resume_agent", Success: true, Output: fmt.Sprintf(
				"已恢复 %s：从暂停时的任务继续执行。", agentID)}
		}
	}
	// 非热驻路径：树节点须为 Paused（history 已落 msgStore），异步续跑。
	if d.treeFn == nil {
		return &tool.Result{Tool: "resume_agent", Error: "agent tree not available"}
	}
	tr := d.treeFn(sid)
	if tr == nil {
		return &tool.Result{Tool: "resume_agent", Error: "agent tree not available for session"}
	}
	node, ok := tr.Get(agentID)
	if !ok {
		return &tool.Result{Tool: "resume_agent", Error: fmt.Sprintf("agent %s 不存在", agentID)}
	}
	callerID := agent.AgentIDFromContext(ctx)
	if callerID != "" && callerID != node.ParentID {
		return &tool.Result{Tool: "resume_agent", Error: fmt.Sprintf(
			"agent %s 由 %s 派发，不能由 %s 越级恢复（只能管理自己直派的子 Agent）", agentID, node.ParentID, callerID),
			Category: tool.ResultCategoryValidationRejected}
	}
	if node.Role == "meta" {
		return &tool.Result{Tool: "resume_agent", Error: "meta 主 Agent 不支持恢复",
			Category: tool.ResultCategoryValidationRejected}
	}
	if node.Status != orchestrator.StatusPaused {
		return &tool.Result{Tool: "resume_agent", Error: fmt.Sprintf(
			"agent %s 状态为 %s，仅已暂停的 Agent 可恢复", agentID, node.Status)}
	}
	if d.msgStore == nil {
		return &tool.Result{Tool: "resume_agent", Error: "消息存储未接线，无法续跑（请重新派发任务）"}
	}
	d.resumePausedNode(ctx, agentID)
	return &tool.Result{Tool: "resume_agent", Success: true, Output: fmt.Sprintf(
		"已发起 %s 的续跑（异步执行，从暂停时的任务继续；完成后结果经 mailbox 回传）。", agentID)}
}

// agentModelSetTimeout set_agent_model 的整体墙钟上限（含连通性探测；探活内部 60s）。
const agentModelSetTimeout = 60 * time.Second

// setAgentModelTool 实现 set_agent_model 工具：为单个 Agent 实例覆盖模型（仅本实例、
// 仅进程内存、不落盘）。与 set_role_model 的区别：后者改角色绑定（全局、持久）。
// 授权：只能管理自己直派的子 Agent（caller == node.ParentID）——叶子归其 domain 管。
type setAgentModelTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *setAgentModelTool) Name() string { return "set_agent_model" }

// Aliases 返回工具别名列表，当前无别名。
func (t *setAgentModelTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *setAgentModelTool) Description() string {
	return "为单个 Agent 实例（你直派的 domain，或你直派的固定角色）覆盖模型，只影响这一个实例：" +
		"同角色其他实例与后续派发不变，不写入 models.json，实例终结（完成/取消/槽销毁）即回收。" +
		"参数：agent_id（必填）、model_id（注册表条目 ID，先用 list_models 查看）、reason（可选，写入回执）。" +
		"典型用法：pause_agent(该实例) → set_agent_model(该实例) → resume_agent(该实例)，全程保留上下文。" +
		"整类角色都不合适时用 set_role_model。只能管理自己直派的 Agent，越级会被拒绝。"
}

// Execute 执行 set_agent_model 工具调用。
func (t *setAgentModelTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	agentID, _ := args["agent_id"].(string)
	modelID, _ := args["model_id"].(string)
	reason, _ := args["reason"].(string)
	agentID = strings.TrimSpace(agentID)
	modelID = strings.TrimSpace(modelID)
	if agentID == "" || modelID == "" {
		return &tool.Result{Tool: "set_agent_model", Error: "agent_id 与 model_id 必填（先 list_models 查看候选模型）",
			Category: tool.ResultCategoryValidationRejected}
	}
	switchr, ok := d.models.(AgentModelSwitcher)
	if !ok {
		return &tool.Result{Tool: "set_agent_model", Error: "实例级模型覆盖不可用（模型工厂未接线）"}
	}
	if d.treeFn == nil {
		return &tool.Result{Tool: "set_agent_model", Error: "agent tree not available"}
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(agentID, "/"); i > 0 {
			sid = agentID[:i]
		}
	}
	tr := d.treeFn(sid)
	if tr == nil {
		return &tool.Result{Tool: "set_agent_model", Error: "agent tree not available for session"}
	}
	node, found := tr.Get(agentID)
	if !found {
		return &tool.Result{Tool: "set_agent_model", Error: fmt.Sprintf("agent %s 不存在", agentID)}
	}
	// 授权：只能管理自己直派的子 Agent（domain 名下的叶子归 domain 管）。
	callerID := agent.AgentIDFromContext(ctx)
	if callerID != "" && callerID != node.ParentID {
		return &tool.Result{Tool: "set_agent_model", Error: fmt.Sprintf(
			"agent %s 由 %s 派发，不能由 %s 越级换档（只能管理自己直派的子 Agent）", agentID, node.ParentID, callerID),
			Category: tool.ResultCategoryValidationRejected}
	}
	if node.Role == "meta" {
		return &tool.Result{Tool: "set_agent_model", Error: "meta 主 Agent 不支持实例级换档",
			Category: tool.ResultCategoryValidationRejected}
	}
	switch node.Status {
	case orchestrator.StatusRunning, orchestrator.StatusPaused, orchestrator.StatusIdle:
	default:
		return &tool.Result{Tool: "set_agent_model", Error: fmt.Sprintf(
			"agent %s 状态为 %s，仅运行中/已暂停/空闲可换档", agentID, node.Status)}
	}

	setCtx, cancel := context.WithTimeout(ctx, agentModelSetTimeout)
	defer cancel()
	cfg, err := switchr.SetAgentModel(setCtx, agentID, node.Role, modelID, "")
	if err != nil {
		return &tool.Result{Tool: "set_agent_model", Error: err.Error(), Category: tool.ResultCategoryExecutionFailed}
	}
	out := fmt.Sprintf("已为 %s 覆盖模型 %s（provider=%s, model=%s, thinking=%s）。仅本实例生效，不写 models.json，实例终结即回收；下次 LLM 调用即生效，进行中调用不中断。",
		agentID, modelID, cfg.Provider, cfg.Model, cfg.Thinking)
	if reason != "" {
		out += "（原因: " + reason + "）"
	}
	return &tool.Result{Tool: "set_agent_model", Success: true, Output: out}
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
		"完成后结果摘要以 [mailbox from <sub_agent_id>] 送达，后续轮次阅读整合。" +
		fmt.Sprintf("task 必须自包含 <=%d 字：背景/目标/文件路径/验收——子 Agent 看不到对话历史；规格原文走 WriteSharedMemory（超限 %d 字硬拒）。\n", t.dispatcher.taskRuneSoftLimit, t.dispatcher.taskRuneHardLimit) +
		"前置依赖：必须先 WriteSpec（否则 spec missing/stale/invalid 拒派）；WriteSpec=固定 spec 槽供校验注入，WriteSharedMemory=自由 KV，不可互替。\n" +
		"路由：不确定一律 role_id=\"domain\"（默认自执行）；单函数级单文件领域明确的任务才直派固定助手；DomainAgent 不能派 domain。\n" +
		"字段：domain=**中文领域名**（仅 domain 角色用；它是子 Agent 对用户可见的展示名，如「文档修订-第3章」「UI 渲染领域」，禁止英文缩写/编号 doc-rev-a 这类）；responsibility=职责边界 <=200 字（domain 必填，注入子提示词防越界）；" +
		"mode=react(默认)/reflection/plan_execute（判断不准省略）；verify_kind=auto(默认)/executable/rubric/none；" +
		"tools_hint=预挂载插件工具名列表（受角色白名单天花板约束）；wall_clock_min=墙钟分钟（普通任务省略，仅侦察/巡检给小预算）；" +
		"reuse_agent_id=热驻复用（填【空闲领域Agent】的 agent_id，保留全部上下文，spec 照写、key 与领域名对齐）；" +
		"takeover=异名续建时填旧 domain 名，看板未完成条目迁移留痕（同名续建自动覆盖无需填）；" +
		"worktree=true 隔离派发到 git worktree 副本（子 Agent 全部文件写入落在副本，主目录零写入；成功收尾产出全量 patch，用 merge_worktree review 看完整 diff 后 merge/reject——仅文件级独立且需并行的任务使用；与 reuse_agent_id 互斥，热驻 domain 不支持）。\n" +
		"可调用的 role_id：" + strings.Join(entries, "；") + "。"
}

// hasHanRunes 判断字符串是否含汉字（中文领域名校验用）：纯 ASCII/拼音不算。
func hasHanRunes(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// joinWarnings 以"；"拼接两条非空警告（空串自动跳过），保持工具结果可读。
func joinWarnings(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "；" + b
	}
}

// validateDispatchArgs 校验单次派发的必要参数；返回 (msg, warning)：
// msg 非空=硬拒绝（校验拒绝，Category=validation_rejected）；否则通过，
// warning 非空=放行但附提示（task 轻微超限软着陆 TODO #38-3、domain 不是中文领域名）。
// 供 call_sub_agent 与 call_sub_agents 复用（批量工具逐项校验）。
func (d *Dispatcher) validateDispatchArgs(roleID, task, responsibility, mode, verifyKind, domain string) (msg, warning string) {
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
	// domain 中文展示名（2026-09-12 用户实证）：domain 就是子 Agent 对用户可见的展示名，
	// 出现在对话栏子 Agent 列表、编排页树、面包屑与事件流上。模型爱填 doc-rev-a 这类英文
	// 编号，用户完全读不懂。这里**软着陆**（放行 + 警告进工具结果，模型下一次派发即改口）
	// 而非硬拒：与"一波 9 个领域批量派发"叠加时硬拒会把整轮打成拒绝循环，而同日实证
	// WriteSpec 连续 6 次拒绝已触发 loop guard 强退（改参数的成本远高于读一条警告）。
	if roleID == "domain" && strings.TrimSpace(domain) != "" && !hasHanRunes(domain) {
		warning = fmt.Sprintf("domain=%q 建议改用中文领域名（如「文档修订-第3章」「UI 渲染领域」）：它是子 Agent 对用户可见的展示名，英文编号用户读不懂", domain)
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
		return "", joinWarnings(warning, fmt.Sprintf("task 已 %d runes，超出 %d 字预算但未达硬上限 %d，本次放行；下次派发请压缩至 %d 字内",
			n, softLimit, hardLimit, softLimit))
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
	// 注意带上 warning：domain 中文名提示只在该变量里，写死 return "", "" 会把它丢掉
	//（2026-09-12 实证：软提示静默失效，测试才发现）。
	return "", warning
}

// checkSpecBeforeDispatch 做 WriteSpec 强制校验：SpecEnforcementEnabled 开启时，
// 派发前必须先 WriteSpec（校验 parentID:spec 存在、新鲜、Spec.Goal 非空且至少一条 Acceptance）。
// domain 非空时校验该领域专属 spec（TODO #65 多 key 化），缺失回退遗留单键。
// 返回 (错误文案, 警告文案)：错误非空=拒绝派发；警告非空=放行附提示（唯一候选回退等）。
// 批量派发（call_sub_agents）对有 domain 的项逐领域调用本函数校验（任一失败整批拒），
// 无 domain 项回退遗留单键一次校验（TODO #65）。
func (d *Dispatcher) checkSpecBeforeDispatch(ctx context.Context, parentID, domain string) (string, string) {
	if !d.specEnforcementEnabled {
		return "", ""
	}
	ok, reason := d.hasFreshSpec(ctx, parentID, domain)
	if !ok {
		return reason + " 先调 WriteSpec(goal, acceptance, constraints, files) 写任务规范，再派发", ""
	}
	if reason != "" {
		return "", reason
	}
	return "", ""
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
	// skills 可选：下放技能集（渐进披露）——校验 ⊆ 父持有集后并入子持有集。
	skillsHint := d.skillsArg(args)
	// wall_clock_min 可选：派发级墙钟（分钟），代码级强制收口，替代提示词墙钟。
	wallClock := d.wallClockArg(args)
	// reuse_agent_id 可选：热驻复用（idle domain 唤醒/忙碌入队），非空时忽略 role_id。
	reuseAgentID, _ := args["reuse_agent_id"].(string)
	// takeover 可选（TODO #73 看板接力认领）：声明接管的旧 domain 名，
	// dispatcher 迁移其非 Done 看板条目（含 Failed）到本次 domain 并留痕。
	takeover, _ := args["takeover"].(string)
	takeover = strings.TrimSpace(takeover)
	// worktree 可选（TODO 第9⑤/#10⑤）：git worktree 隔离派发——子 Agent 在主仓库
	// 副本内工作，成功收尾产出全量 patch，meta 经 merge_worktree 合并门合入。
	wantWorktree := boolArg(args, "worktree")

	// 从当前上下文获取父 Agent ID，子 Agent 需要知道是谁调用了它。
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "missing parent agent context"}
	}

	// 同名热驻 Idle 槽隐式复用（先于参数校验：空 responsibility 会被 validateDispatchArgs
	// 先拒，永远到不了复用分流，见 resolveIdleSiblingReuse 注释）。
	if reuseAgentID == "" && roleID == "domain" {
		if id := d.resolveIdleSiblingReuse(ctx, parentID, domain); id != "" {
			reuseAgentID = id
		}
	}

	// 领域档案冷复活（TODO #17 T24）：热驻/同名复用未命中才走——档案只管跨会话冷复活。
	// 名字（精确/别名）或路径（任务文本+spec 文件 ∩ 档案清单）命中 → domain 归一化到
	// 档案正名 + 种子段（摘要/存活文件/既有结论链）拼进 task；未命中零改动自由命名。
	if reuseAgentID == "" && roleID == "domain" {
		task, domain = d.applyDomainProfileSeed(ctx, parentID, domain, task)
	}

	msg, warning := t.dispatcher.validateDispatchArgs(roleID, task, responsibility, mode, verifyKind, domain)
	if reuseAgentID == "" && msg != "" {
		return &tool.Result{Tool: "call_sub_agent", Error: msg, Category: tool.ResultCategoryValidationRejected}
	}
	// reuse 模式 role_id 可省（复用槽沿用原角色）；task 仍必填。
	if reuseAgentID != "" && strings.TrimSpace(task) == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "task is required", Category: tool.ResultCategoryValidationRejected}
	}
	// spec 豁免（TODO 第七项②）：spec_exempt 角色跳过 WriteSpec 强制门——
	// scout 类侦察角色本身就是"先定位"的工具，spec 先于侦察存在则门永远挡住合法路径。
	var specWarn string
	if rd := d.registry.Get(roleID); rd == nil || !rd.SpecExempt {
		specMsg, warn := d.checkSpecBeforeDispatch(ctx, parentID, domain)
		if specMsg != "" {
			// worktree 派发豁免 spec 新鲜度（mtime stale）拒绝（TODO 第9⑤）：
			// spec mtime 相对主目录，他域合入/工作目录漂移会让合法的 worktree 派发误报
			// stale——降级为警告放行（missing/invalid 仍硬拒）。
			if wantWorktree && strings.Contains(specMsg, "stale") {
				specWarn = strings.TrimSpace(specWarn + "；worktree 派发豁免 spec 新鲜度检查: " + specMsg)
			} else {
				return &tool.Result{Tool: "call_sub_agent", Error: specMsg, Category: tool.ResultCategoryValidationRejected}
			}
		}
		specWarn += warn
	}
	// worktree 与热驻复用互斥：复用槽沿主目录上下文冻结，切副本无意义。
	if wantWorktree && reuseAgentID != "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "worktree 与 reuse_agent_id 互斥：复用槽沿主目录上下文续作，请二选一", Category: tool.ResultCategoryValidationRejected}
	}

	subAgentID, errRes := d.dispatchOne(ctx, roleID, domain, task, responsibility, mode, verifyKind, toolsHint, skillsHint, wallClock, reuseAgentID, takeover, &dispatchOpts{worktree: wantWorktree})
	if errRes != nil {
		errRes.Tool = "call_sub_agent"
		return errRes
	}
	out := subAgentID
	if takeover != "" && takeover != strings.TrimSpace(domain) {
		out += fmt.Sprintf("（已接管旧领域 %q 的未完成看板条目并留痕）", takeover)
	}
	// task 轻微超限软着陆警告（TODO #38-3）：放行但提示下次压缩。
	var warns []string
	if warning != "" {
		warns = append(warns, warning)
	}
	if specWarn != "" {
		warns = append(warns, specWarn)
	}
	if len(warns) > 0 {
		out += "。警告: " + strings.Join(warns, "；")
	}
	return &tool.Result{Tool: "call_sub_agent", Success: true, Output: out}
}

// checkActiveSiblingDomain 检查同父 Agent 下是否已存在同名且仍活跃的 domain 节点，
// 存在时返回拒绝文案（引导按职责细分命名或等其回传）；不存在/无法判断时返回空串放行。
// 仅活跃态（Running/Paused/Idle）算冲突；终态（Failed/Cancelled/Done）放行——
// 失败打捞重派、完结后新任务沿用领域名均为合法路径（withPriorSalvage 依赖前者）。
func (d *Dispatcher) checkActiveSiblingDomain(ctx context.Context, parentID, domain string) string {
	domainKey := strings.TrimSpace(domain)
	if domainKey == "" || d.treeFn == nil {
		return ""
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	if sid == "" {
		return ""
	}
	t := d.treeFn(sid)
	if t == nil {
		return ""
	}
	for _, n := range t.Snapshot() {
		if n.ParentID != parentID || n.Role != "domain" || strings.TrimSpace(n.Domain) != domainKey {
			continue
		}
		switch n.Status {
		case orchestrator.StatusRunning, orchestrator.StatusPaused, orchestrator.StatusIdle:
			return fmt.Sprintf("domain %q 已有同名活跃实例（%s，状态 %s）：同名 domain 并行会让回灌摘要无法区分责任域。请按职责细分命名（如 %s核心层/%s命令层），或等其回传后再派", domainKey, n.ID, n.Status, domainKey, domainKey)
		}
	}
	return ""
}

// resolveIdleSiblingReuse 同名热驻空闲槽隐式复用解析：domain 与同父下某热驻 Idle
// 实例同名、且池内槽健在时返回该槽 agent_id，调用方将其视同显式 reuse_agent_id 走
// dispatchToIdleSlot——等效自动复用。仅精确同名；Running/Paused 不命中（仍走
// checkActiveSiblingDomain 并行拒绝）；终态节点无热驻槽，天然不命中。
// T22 档位守卫：槽 enterIdle 固化的档位与当前会话档位不符则跳过（不隐式接管旧档
// 上下文）；槽 gear 为空（存量槽/回调未接线）或当前档位空时按匹配放行。
// 背景（2026-08-27 派发死循环实证）：MetaAgent 叙述复用意图却漏传 reuse_agent_id
// （整场 0 次），同名+空 responsibility 双错循环 8 连败；与其硬拒自纠，不如直接路由。
// 注意必须在 validateDispatchArgs 之前解析——空 responsibility 会被参数校验先拒，
// 永远到不了复用分流。热驻关闭/树不可用返回空串零行为变化。
func (d *Dispatcher) resolveIdleSiblingReuse(ctx context.Context, parentID, domain string) string {
	domain = strings.TrimSpace(domain)
	if !d.hotEnabled() || domain == "" || d.treeFn == nil {
		return ""
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		sid = sessionIDFromAgentID(parentID)
	}
	if sid == "" {
		return ""
	}
	t := d.treeFn(sid)
	if t == nil {
		return ""
	}
	for _, n := range t.Snapshot() {
		if n.ParentID != parentID || n.Role != "domain" || strings.TrimSpace(n.Domain) != domain {
			continue
		}
		if n.Status == orchestrator.StatusIdle {
			if slot := d.pool.slot(sid, n.ID); slot != nil {
				// T22 档位守卫：热驻槽固化档位与会话当前档位不符时不隐式复用——
				// 用户切档（如转快速档）后，不该静默吃到旧档攒下的热驻上下文。
				// 槽 gear 为空（存量槽/回调未接线）或当前档位空=按匹配放行，防误杀。
				if slotGear := slot.gearOf(); slotGear != "" {
					if cur := d.sessionGearOf(sid); cur != "" && cur != slotGear {
						continue
					}
				}
				return n.ID
			}
		}
	}
	return ""
}

// dispatchOne 执行一次子 Agent 异步派发：权限校验→同领域去重→全局限额→注册树→goroutine。
// 成功返回 subAgentID；失败返回 *tool.Result（Error 非空，Tool 字段由调用方按工具名覆盖）。
// mode 为派发执行模式（react/reflection/plan_execute，空串=react，TODO #29），
// verifyKind 为校验分层（auto/executable/rubric/none，空串=auto，TODO #43），
// toolsHint 为建议工具集（TODO #52，可空）：校验 ∩ 子 Agent 角色天花板后预挂载到子 scope，
// skillsHint 为下放技能集（技能渐进披露，可空）：校验 ⊆ 父持有集后并入子持有集
// （角色固定集自动并入，越界项忽略并随结果回告父 Agent），
// wallClock 为派发级墙钟（>0 时取 min(wallClock, sub_agent_timeout) 替代全局值，到期前预警）。
// reuseAgentID 非空时走热驻复用（idle_pool.go dispatchToIdleSlot）：唤醒 idle domain
// 或忙碌入队，忽略 roleID/task 以外的派发参数（复用 Agent 技能集沿用槽内冻结值，skillsHint 忽略）。
// 均穿透到子 Agent 构造时的引擎选择与完成后校验。
// takeover 非空时（TODO #73）迁移旧 domain 的非 Done 看板条目到本次派发 domain 并留痕。
// 前置：调用方已完成参数校验（validateDispatchArgs）与 spec 强制校验。
// 供 call_sub_agent（单个）与 call_sub_agents（批量同波）两个工具复用。
// dispatchOpts 是 dispatchOne 的可选项（TODO 第七项⑤ map_sub_agents 聚合模式）。
type dispatchOpts struct {
	// aggregate 非空时：本子 Agent 的完成通知不直发父邮箱，改记入聚合器；
	// 全部项收口后由聚合器统一发一条汇总消息。
	aggregate *mapAggregation
	// aggItem 是聚合模式下本项的原始 item 文本（聚合消息回显用）。
	aggItem string
	// aggIdx 是聚合模式下本项在 items 中的下标（聚合消息按派发顺序排列用）。
	aggIdx int
	// worktree 为 true 时（TODO 第9⑤/#10⑤）：本项派发到 git worktree 副本，
	// 成功收尾产出 patch，meta 经 merge_worktree 合并门合入主仓库。
	worktree bool
}

func (d *Dispatcher) dispatchOne(ctx context.Context, roleID, domain, task, responsibility, mode, verifyKind string, toolsHint, skillsHint []string, wallClock time.Duration, reuseAgentID, takeover string, opts ...*dispatchOpts) (string, *tool.Result) {
	var opt *dispatchOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
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

	// worktree 派发约束（TODO 第9⑤）：开关关闭拒绝；热驻 domain 拒绝（槽沿主目录
	// 上下文冻结，切副本破坏 reuse 语义）。callSubAgentTool 已拒 reuse 组合。
	if opt != nil && opt.worktree {
		if !d.worktreeEnabled {
			return "", &tool.Result{Error: "worktree 派发未启用（agent.worktree_enabled=false）", Category: tool.ResultCategoryValidationRejected}
		}
		if d.hotEnabled() && roleDef.ID == "domain" {
			return "", &tool.Result{Error: "worktree 不支持热驻 domain 派发（热驻槽沿主目录上下文续作）；请关闭热驻或取消 worktree 参数", Category: tool.ResultCategoryValidationRejected}
		}
	}

	// scout 类 spec_exempt 角色缺省墙钟 5 分钟（TODO 第七项②）：侦察任务必须有预算上限，
	// LLM 未显式给 wall_clock_min 时兜底，防侦察失控无收口。
	if wallClock <= 0 && roleDef.SpecExempt {
		wallClock = 5 * time.Minute
		// light 速修手查修一体（定位+修改+自检），比纯侦察多留一倍预算。
		if roleDef.ID == "light" {
			wallClock = 10 * time.Minute
		}
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
			d.registerDepWaiter(parentID, domain, gate)
			return "", &tool.Result{Error: gate + "；依赖完成时会自动通知你，勿重复尝试派发", Category: tool.ResultCategoryValidationRejected}
		}
		// 同父同名活跃 domain 查重（跨调用）：call_sub_agents 的批内查重拦不住同一轮
		// 多次 call_sub_agent 单派同名 domain（实证 parallel-independent 任务 Meta 同轮
		// 两次单派 "CLI工具"，9ms 之差并行启动，回灌摘要/树展示无法区分责任域）。
		// 工具调用在同轮内串行执行（react_agent.go），首个派发注册节点后第二个必被拦。
		if msg := d.checkActiveSiblingDomain(ctx, parentID, domain); msg != "" {
			return "", &tool.Result{Error: msg, Category: tool.ResultCategoryValidationRejected}
		}
		// 复用守卫（热驻开启时）：新建 domain 疑似与某热驻槽同目标（命名包含/
		// spec 文件重叠）时拒一次，引导 reuse_agent_id 复用；task 含【新领域声明】放行。
		if d.hotEnabled() {
			if msg := d.checkIdleDomainReuse(ctx, parentID, domain, task); msg != "" {
				return "", &tool.Result{Error: msg, Category: tool.ResultCategoryValidationRejected}
			}
		}
	}

	// 前序失败打捞（TODO #20 第三层）：同父同 scope 存在 Failed/Cancelled 前任时，
	// 把其打捞摘要追加到新任务文本，机制上保证重派不重复探索。
	// 2026-08-19 扩展到叶子：domain 派发按领域；叶子派发按角色（心跳误杀的叶子重派
	// 此前从零重跑，损失 20 分钟量级）。
	task = d.withPriorSalvage(ctx, parentID, domain, roleID, task)

	// 接力熔断（TODO #76）：per-(parent, domain) 派发代数计数（含 takeover 接管链）。
	// 第 3 代起注入【重写评估】强制段（防别名创可贴式惯性续修，质量逐棒劣化无干预——
	// 实证 2026-08-25：game.js 三棒接力 domain-2 骨架→domain-3 整建→domain-4 别名创可贴）；
	// 第 4 代起 task 无【接力理由】声明即拒派。
	gen := d.bumpDispatchGeneration(parentID, domain, takeover)
	if gen >= relayHardDeclineGen && !taskHasRelayReason(task) {
		return "", &tool.Result{Error: relayDeclineMessage(gen, domain), Category: tool.ResultCategoryValidationRejected}
	} else if gen >= relayRewriteAssessGen {
		task += "\n\n【重写评估】你是该领域第 " + fmt.Sprintf("%d", gen) + " 代执行者，前序多棒接力可能已积累质量劣化。" +
			"开工前必须先评估并声明：对核心文件做【整文件重写】还是【继续修补】——" +
			"若前序代码结构混乱/补丁摞补丁，重写成本低于继续修补；声明理由后再动手。"
	}

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

	// worktree 隔离派发（TODO 第9⑤）：trackChildStart 之前创建副本——失败按
	// validation_rejected 拒绝，不泄漏父 pending 计数；成功后句柄登记进 d.worktrees，
	// 供完成收尾 producePatch、merge_worktree 合并门与终态清理消费。
	var wt *worktreeHandle
	if opt != nil && opt.worktree {
		h, rej := d.createWorktreeForDispatch(ctx, parentID, subAgentID, sessionIDFromAgentID(parentID), domain, roleID)
		if h == nil {
			return "", rej
		}
		wt = h
	}

	// tools_hint 预挂载（TODO #52 执行项 4）：父 Agent 建议的工具集 ∩ 子 Agent 角色天花板后
	// 挂载进子 scope，子 Agent 构造的 adapter 据此收窄插件工具可见集——
	// 实现"派 UI 任务时提示用画图插件"而不放权。越界项拒绝并回告父 Agent（日志可查）。
	var hintRejected []string
	if len(toolsHint) > 0 && d.tools != nil {
		if _, rejected := d.tools.MountForScope(subAgentID, roleDef.ID, toolsHint); len(rejected) > 0 {
			hintRejected = rejected
		}
	}

	// 技能分发（渐进披露）：子持有集 = 角色固定集 ∪ 父分配集（⊆ 父持有集，越界忽略回告）。
	// 持有集登记进 heldSkills，子 Agent 的【可用技能】提示块与 load_skill/list_skills
	// 范围均以此为权威。热驻复用路径在上方已提前返回，技能集沿用槽内冻结值。
	heldSkills, skillRejected := d.resolveChildSkills(parentID, roleDef, skillsHint)
	if len(heldSkills) > 0 {
		d.heldSkills.Store(subAgentID, heldSkills)
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
		id, res := d.dispatchHotDomain(ctx, parentID, subAgentID, *roleDef, domain, task, responsibility, effectiveTimeout, taskBrief, started)
		if res != nil {
			return id, res
		}
		return appendRejectNotes(id, hintRejected, skillRejected), nil
	}

	// 中断传播基底（TODO 第10④）：父 ctx 携带会话级 stopCtx（tool.WithStopContext 注入）
	// 时以其为基底——会话 Stop/cancel 取消 stopCtx，stop 窗口期新派发即刻终止、深层孙代
	// 随链级联；未携带（测试/旧路径）回退 Background 保持既有独立语义。
	subAgentCtx := tool.StopContextFrom(ctx)
	if subAgentCtx == nil {
		subAgentCtx = context.Background()
	}
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	// 每会话工作目录（S2）：子 Agent ctx 由 Background 重建，父 ctx 的 value 不会自动
	// 流入，须显式重注入（父未注入时 WorkDirFromContext 返回空，WithWorkDir 空值 no-op）。
	workDirForChild := tool.WorkDirFromContext(ctx)
	if wt != nil {
		// worktree 派发（TODO 第9⑤）：工作目录切换到副本路径——subAgentWorkDirFor
		// 读 ctx 值优先，引擎提示词/工具/沙箱基线随之自然隔离（主目录零写入）。
		workDirForChild = wt.Path
	}
	subAgentCtx = tool.WithWorkDir(subAgentCtx, workDirForChild)
	// 本轮用户图片（Alt+V 粘贴）带外穿透：子 Agent ctx 由 Background 重建，
	// 父 ctx 的 value 不会自动流入，须显式重注入--子 Agent 首条 user 消息挂图，
	// 其自身工具调用链（含再派发叶子）递归携带。
	if imgs := agent.UserImagesFromContext(ctx); len(imgs) > 0 {
		subAgentCtx = agent.WithUserImages(subAgentCtx, imgs)
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
	// 任务台账登记（2026-08-28 旧需求重派事故根治）：派发即记"进行中"，
	// 终态由 notify 收口；仅 Meta 直派入账（RecordDispatch 内部过滤父角色）。
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		d.ledger.RecordDispatch(sid, parentID, subAgentID, domain, taskBrief, "")
	}
	// 计划状态回写（TODO #22 Phase 1 补全）：派发即把该领域的计划子任务置为 in_progress，
	// 否则任务只有完成/失败才翻状态，TUI 执行计划面板全程 Waiting、进度 0%。
	d.boardAssign(ctx, parentID, domain, subAgentID)
	// 看板接力认领（TODO #73）：takeover 声明接管旧 domain 时，迁移其非 Done 条目
	// （含 Failed）到本 domain 并留痕——旧 FAILED 条目随本次派发/完成翻绿，
	// 不再永久红误导 meta 与用户。
	if takeover != "" && takeover != strings.TrimSpace(domain) {
		if b := d.boardFor(parentID); b != nil {
			if n := b.TakeoverFrom(takeover, strings.TrimSpace(domain)); n > 0 {
				log.Printf("[subagent] board takeover: parent=%s from=%s to=%s migrated=%d", parentID, takeover, domain, n)
			}
		}
	}
	// 心跳检活元数据：subMeta 存 cancel/parentID/sessionID/doneOnce 供巡检卡死时兜底。
	// activity：所有非 meta 子 Agent 注册（TODO #25-3 domain 防误杀版）——叶子活动沿
	// parentID 链向上冒泡刷新祖先时间戳，domain 等子/等回信期间靠后代活动保持存活；
	// 自身无 LLM/工具活动且无活跃后代超阈值才判假死。meta 不注册（会话级由用户/墙钟控制）。
	isMeta := roleDef.ID == "meta"
	meta := &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: tool.SessionIDFromContext(ctx), wallClock: effectiveTimeout}
	d.subMeta.Store(subAgentID, meta)
	ev := newEvidence()
	if !isMeta {
		d.activity.Store(subAgentID, ev)
	}
	d.ensurePatrol()
	// 聚合模式登记（TODO 第七项⑤）：先于 goroutine 注册，消除 notify 时序竞态。
	if opt != nil && opt.aggregate != nil {
		d.aggByAgent.Store(subAgentID, &mapAggEntry{agg: opt.aggregate, idx: opt.aggIdx, item: opt.aggItem})
	}
	go func() {
		defer cancel()
		// CompareAndDelete（而非 Delete）：复活/复用会在同一 ID 上重新 Store 新条目，
		// 旧 run 的无条件 Delete 会把新 run 的条目误删——被复活的 Agent 随即失去活动监控
		// 与取消句柄（scanStuck/ActivityEvidenceOf/cancel_agent 全部落空）。
		defer d.subMeta.CompareAndDelete(subAgentID, meta)
		defer d.activity.CompareAndDelete(subAgentID, ev)
		defer d.lastWrites.Delete(subAgentID)
		defer d.heldSkills.Delete(subAgentID)
		// paused=true 时(domain 触达 token 上限)不递减父未决计数:父 PendingChildren 保持 >0,
		// 由 MetaAgent PausedChildChecker 检测后主动暂停会话,等用户"继续"恢复。
		// doneOnce 保证与心跳巡检竞争时 trackChildDone 仅触发一次，防双递减。
		paused := d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, task, domain, responsibility, mode, verifyKind, started)
		// 聚合模式：notify 未触达（暂停/无邮箱等路径）时兜底收口本项，防聚合器永久悬挂。
		// 正常完成路径 notify 已先消费登记，此处 Load 不到即 no-op。
		if opt != nil && opt.aggregate != nil {
			if e, ok := d.aggByAgent.Load(subAgentID); ok {
				d.aggByAgent.Delete(subAgentID)
				entry := e.(*mapAggEntry)
				entry.once.Do(func() {
					opt.aggregate.record(entry.idx, entry.item, "（未回传：暂停或异常终止，恢复/排查后另行通知）", false)
				})
			}
		}
		if !paused {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()

	// 墙钟预警阶梯：50%/75%/90% 三段递进 mailbox 收口警告。原单次终点预警（到期前
	// grace=min(2min,T/4) 才投）实证失效：2h 预算第 118 分钟才警告，慢模型单轮 2-5 分钟，
	// 警告被 drain 时 Agent 已被杀，全程无感知硬杀（2026-08-28 渲染领域 Agent 两小时撞墙）。
	// agent 提前完成时 ctx 被 defer cancel，goroutine 随之退出，零泄漏。
	if effectiveTimeout > 0 && d.mailbox != nil {
		wallClockWarnLadder(d.mailbox, subAgentID, effectiveTimeout, subAgentCtx.Done())
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
					Body:    fmt.Sprintf("【侦察预算预警】已用约 %v（侦察墙钟 %v 的过半），停止继续侦察：已读文件的结论已足够，立即转入派发叶子/写文件。剩余预算必须全部用于产出。", half, effectiveTimeout),
				})
			}()
		}
	}

	// 返回子 Agent ID 作为句柄，父 Agent 可用该 ID 查询或接收后续通知。
	if len(hintRejected) > 0 || len(skillRejected) > 0 {
		// tools_hint / skills 越界项回告父 Agent：越界委派被拒绝且可查。
		return appendRejectNotes(subAgentID, hintRejected, skillRejected), nil
	}
	return subAgentID, nil
}

// appendRejectNotes 把 tools_hint / skills 越界忽略说明追加到派发结果尾部；
// 均为空时原样返回。与既有 tools_hint 回告文案同构。
func appendRejectNotes(id string, hintRejected, skillRejected []string) string {
	if len(hintRejected) > 0 {
		id += "。tools_hint 越界忽略（子 Agent 角色权限天花板外）: " + strings.Join(hintRejected, "; ")
	}
	if len(skillRejected) > 0 {
		id += "。skills 越界忽略（父 Agent 未持有）: " + strings.Join(skillRejected, "; ")
	}
	return id
}

// wallClockWarnMinOffset / wallClockWarnMinRemain 是预警档位跳过阈值：触发点距派发
// <minOffset 或距到期 <minRemain 的档位不投（短墙钟零行为变化，退化为仅到期硬杀）。
// var 而非 const：测试可临时缩小阈值以在毫秒内验证投递。
var (
	wallClockWarnMinOffset = 30 * time.Second
	wallClockWarnMinRemain = 30 * time.Second
)

// wallClockWarnLadder 墙钟预警阶梯：在预算的 50%/75%/90% 处向子 Agent 邮箱投递递进
// 收口警告（子 Agent 主循环 drainMailbox 会读到）。一次性子 Agent（dispatchOne）与热驻
// domain（armWallClock）共用。距派发 <30s 或距到期 <30s 的档位跳过（短墙钟零行为变化，
// 退化为仅到期硬杀）。done 关闭（任务完成/取消/到期）时 goroutine 立即退出，零泄漏。
func wallClockWarnLadder(mb *mailbox.Mailbox, subAgentID string, total time.Duration, done <-chan struct{}) {
	if mb == nil || total <= 0 || done == nil {
		return
	}
	steps := []struct {
		frac float64
		body func(total, remain time.Duration) string
	}{
		{0.50, func(total, remain time.Duration) string {
			return fmt.Sprintf("【墙钟预警】执行预算（%v）已用约一半（剩约 %v）。规划收口节奏：不再开启新的探索方向，优先完成手头产出。",
				total, remain)
		}},
		{0.75, func(total, remain time.Duration) string {
			return fmt.Sprintf("【墙钟预警】执行预算（%v）只剩约 %v。立即停止新探索与新读取，基于已有产出收尾并准备终答。",
				total, remain)
		}},
		{0.90, func(total, remain time.Duration) string {
			return fmt.Sprintf("【墙钟最终预警】距执行上限（%v）只剩约 %v。下一轮必须输出终答：总结已有产出与未完成项，不再调用任何工具。",
				total, remain)
		}},
	}
	go func() {
		start := time.Now()
		for _, st := range steps {
			offset := time.Duration(float64(total) * st.frac)
			remain := total - offset
			if offset < wallClockWarnMinOffset || remain < wallClockWarnMinRemain {
				continue
			}
			wait := time.Until(start.Add(offset))
			if wait <= 0 {
				continue
			}
			t := time.NewTimer(wait)
			select {
			case <-t.C:
				_, _ = mb.Send(&mailbox.Message{
					From:    "dispatcher",
					To:      subAgentID,
					Type:    mailbox.MsgInfo,
					Subject: "墙钟预警",
					Body:    st.body(total, remain),
				})
			case <-done:
				t.Stop()
				return
			}
		}
	}()
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

// skillsArg 从 args 提取 skills 参数（兼容 []string / []any），口径同 toolsHintArg。
func (d *Dispatcher) skillsArg(args map[string]any) []string {
	raw, ok := args["skills"]
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
	return "同波多领域批量原子派发：多文件/多领域拆分任务的全部建设领域用它一次派出" +
		"（等价多次 call_sub_agent，但保证同波同时启动；同波 domain 名必须唯一）。\n" +
		"参数：tasks 数组（<=6 项），每项 {role_id, task, domain?, responsibility?, mode?, " +
		"verify_kind?, tools_hint?, wall_clock_min?, reuse_agent_id?}，字段规则与 call_sub_agent 一致" +
		fmt.Sprintf("（domain 角色 responsibility 必填，task 自包含 <=%d 字）。", t.dispatcher.taskRuneSoftLimit) +
		"前置依赖与 call_sub_agent 相同：派发前先 WriteSpec（整波共用一份，spec missing/stale 拒绝）。" +
		"逐项返回派出结果，某项失败不影响其他项。"
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
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agents", Error: "missing parent agent context"}
	}

	// 逐项校验参数；超软限（默认 3000 runes）未达硬限（默认 4000）软着陆放行并收集警告（TODO #38-3，口径见 validateDispatchArgs）。
	type batchItem struct {
		roleID, domain, task, responsibility, mode, verifyKind, takeover string
		reuseAgentID                                                     string
		toolsHint                                                        []string
		skillsHint                                                       []string
		wallClock                                                        time.Duration
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
		it.takeover, _ = m["takeover"].(string)
		it.takeover = strings.TrimSpace(it.takeover)
		it.toolsHint = d.toolsHintArg(m) // TODO #52：建议工具集（∩ 子 Agent 天花板后预挂载）
		it.skillsHint = d.skillsArg(m)   // 下放技能集（⊆ 父持有集校验后并入子持有集）
		it.wallClock = d.wallClockArg(m)
		// 同名热驻 Idle 槽隐式复用（先于参数校验，口径同单派入口）。
		if it.reuseAgentID == "" && it.roleID == "domain" {
			if id := d.resolveIdleSiblingReuse(ctx, parentID, it.domain); id != "" {
				it.reuseAgentID = id
			}
		}
		if msg, warning := t.dispatcher.validateDispatchArgs(it.roleID, it.task, it.responsibility, it.mode, it.verifyKind, it.domain); msg != "" {
			if it.reuseAgentID == "" {
				return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("tasks[%d]: %s", i, msg), Category: tool.ResultCategoryValidationRejected}
			}
		} else if warning != "" {
			batchWarnings = append(batchWarnings, fmt.Sprintf("tasks[%d]: %s", i, warning))
		}
		items = append(items, it)
	}

	// 同波 domain 名称查重：同名 domain 并行派出会让回灌摘要/树展示无法区分责任域
	// （实证 longctx 任务同波派出两个"后端" domain，判为重复派发）。
	// 发现重复即整批拒绝，引导 MetaAgent 按职责细分命名后重发。
	seenDomains := make(map[string]int, len(items))
	for i, it := range items {
		if it.roleID != "domain" {
			continue
		}
		name := strings.TrimSpace(it.domain)
		if name == "" {
			continue
		}
		if j, dup := seenDomains[name]; dup {
			return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("tasks[%d] 与 tasks[%d] 重复 domain %q：同波 domain 名称必须唯一，请按职责细分命名（如 %s核心层/%s命令层）后重发", i, j, name, name, name), Category: tool.ResultCategoryValidationRejected}
		}
		seenDomains[name] = i
	}

	// 批量派发（call_sub_agents）：多 domain 各自有专属 spec 时逐项校验（每个 domain
	// 的 spec 必须新鲜，TODO #65）；无 domain 项时校验遗留单键一次。
	anyDomain := false
	for _, it := range items {
		if it.roleID != "domain" || strings.TrimSpace(it.domain) == "" {
			continue
		}
		anyDomain = true
		if msg, warn := d.checkSpecBeforeDispatch(ctx, parentID, it.domain); msg != "" {
			return &tool.Result{Tool: "call_sub_agents", Error: fmt.Sprintf("domain %q: %s", it.domain, msg), Category: tool.ResultCategoryValidationRejected}
		} else if warn != "" {
			batchWarnings = append(batchWarnings, fmt.Sprintf("domain %q: %s", it.domain, warn))
		}
	}
	if !anyDomain {
		// spec 豁免（TODO 第七项②）：整批均为 spec_exempt 角色（如 scout 批量侦察）时跳过门。
		allExempt := true
		for _, it := range items {
			if rd := d.registry.Get(it.roleID); rd == nil || !rd.SpecExempt {
				allExempt = false
				break
			}
		}
		if !allExempt {
			if msg, warn := d.checkSpecBeforeDispatch(ctx, parentID, ""); msg != "" {
				return &tool.Result{Tool: "call_sub_agents", Error: msg, Category: tool.ResultCategoryValidationRejected}
			} else if warn != "" {
				batchWarnings = append(batchWarnings, warn)
			}
		}
	}

	// 波聚合整合纪要（C-3a）：domain 项 ≥2 时整波聚合——各领域完成回传汇成一条
	// 【整合纪要】经 notify 单条送达父邮箱（中间完成不逐条打扰父，配合智能唤醒
	// N 子完成从 N 次唤醒轮降为 1 轮消化）。单项/全拒/开关关闭不聚合（行为不变）。
	domainCount := 0
	for _, it := range items {
		if it.roleID == "domain" {
			domainCount++
		}
	}
	var waveAgg *mapAggregation
	batchID := ""
	domainToSub := &sync.Map{} // 领域名 → subAgentID（纪要合成时从台账取文件清单；并发安全：onDone 在子 Agent goroutine 读）
	if d.batchDigestEnabled && domainCount >= 2 {
		waveAgg = &mapAggregation{total: domainCount}
		batchID = fmt.Sprintf("%s/wave-%d", parentID, d.seq.Add(1))
		sid := tool.SessionIDFromContext(ctx)
		goal := ""
		if b := d.boardFor(parentID); b != nil {
			goal = b.Snapshot().Goal
		}
		waveAgg.onDone = func(results []mapAggItemResult) {
			entries := make([]DigestEntry, 0, len(results))
			for _, r := range results {
				e := DigestEntry{Domain: r.Item, Summary: r.Summary, OK: r.OK}
				if subID, ok := domainToSub.Load(r.Item); ok {
					if le, ok := d.ledger.LastEntryByChild(sid, subID.(string)); ok {
						e.Files = le.Files
					}
				}
				entries = append(entries, e)
			}
			d.deliverWaveDigest(batchID, parentID, goal, entries)
		}
	}

	type waveReject struct {
		idx    int
		domain string
		reason string
	}
	var okIDs, errs []string
	var rejects []waveReject
	domainIdx := 0
	okDomains := 0
	for _, it := range items {
		isDomain := it.roleID == "domain"
		idx := -1
		var callOpts []*dispatchOpts
		if isDomain {
			idx = domainIdx
			domainIdx++
			if waveAgg != nil {
				callOpts = append(callOpts, &dispatchOpts{aggregate: waveAgg, aggItem: strings.TrimSpace(it.domain), aggIdx: idx})
			}
		}
		subAgentID, errRes := d.dispatchOne(ctx, it.roleID, it.domain, it.task, it.responsibility, it.mode, it.verifyKind, it.toolsHint, it.skillsHint, it.wallClock, it.reuseAgentID, it.takeover, callOpts...)
		if errRes != nil {
			errs = append(errs, fmt.Sprintf("%s(%s): %s", it.roleID, it.domain, errRes.Error))
			if isDomain && waveAgg != nil {
				rejects = append(rejects, waveReject{idx: idx, domain: strings.TrimSpace(it.domain), reason: errRes.Error})
			}
			continue
		}
		if isDomain && waveAgg != nil {
			okDomains++
			domainToSub.Store(strings.TrimSpace(it.domain), subAgentID)
		}
		id := subAgentID
		if it.takeover != "" && it.takeover != strings.TrimSpace(it.domain) {
			id += fmt.Sprintf("（接管 %q）", it.takeover)
		}
		okIDs = append(okIDs, id)
	}
	if waveAgg != nil {
		if okDomains >= 2 {
			// 整波聚合成立：拒派项事后补记（ok=false），聚合器收口齐后发整合纪要。
			for _, r := range rejects {
				waveAgg.record(r.idx, r.domain, "派发失败："+r.reason, false)
			}
		} else {
			// 成功 domain <2：放弃聚合回退逐条直发（拒绝项错误已在 Output 汇总）。
			// abandon 后已登记项的 notify 拦截仍在，record 改走 flush 直发，防丢消息。
			waveAgg.abandon(func(idx int, item, summary string, ok bool) {
				d.deliverAbandonedWaveItem(parentID, item, summary)
			})
		}
	}
	out := fmt.Sprintf("已并行派出 %d 个子 Agent：%s", len(okIDs), strings.Join(okIDs, ", "))
	if waveAgg != nil && okDomains >= 2 {
		out += fmt.Sprintf("；本波 %d 个领域完成后将汇总为一条【整合纪要】经邮箱送达，一次消化即可", okDomains)
	}
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
		d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)
		d.treeFinish(ctx, subAgentID, "部分完成: "+partial, nil)
		d.notify(parentID, subAgentID, "子 Agent 已达 token 上限,返回部分完成。\n"+partial, files)
		return false
	}
	if err != nil {
		// 取消路径（会话取消/cancel_agent/心跳杀）：通知与树收尾由取消方负责，
		// 此处跳过避免双通知与覆盖 Cancelled 状态（TODO #25 控制面）。
		if errors.Is(err, context.Canceled) {
			sid := tool.SessionIDFromContext(ctx)
			// 手动暂停分流（pause_agent / TUI PauseAgent）：domain 与叶子同路径——存完整
			// history + tree.Pause（可 resume_agent 续跑），不 notify 结果、不递减
			// PendingChildren（父等续跑后的回灌，中途放弃由 cancel_agent 收尾递减）。
			// 叶子暂停是父（domain）的自主中转态，不进会话 PausedOnChild（HasPausedChild
			// 仅认 domain），靠下方邮箱通知驱动父当轮处置。
			if d.isPauseRequested(subAgentID) {
				d.savePausedHistory(ctx, subAgentID, sid, result.History)
				if d.treeFn != nil && sid != "" {
					if t := d.treeFn(sid); t != nil {
						t.Pause(subAgentID, "manual pause")
					}
				}
				d.ClearPauseNode(subAgentID)
				d.pokeParent(parentID)
				if d.mailbox != nil {
					if _, err := d.mailbox.Send(&mailbox.Message{
						From:    "dispatcher",
						To:      parentID,
						Type:    mailbox.MsgInfo,
						Subject: "子 Agent 已暂停: " + subAgentID,
						Body: fmt.Sprintf("【系统通知】子 Agent %s 已按指令暂停（上下文与已写文件保留）。"+
							"请当轮处置：先 list_models 看候选，必要时 set_agent_model(该 id) 换档后 resume_agent(该 id) 续跑原任务；"+
							"无价值则 cancel_agent(该 id) 放弃。长时间不处置该节点占用未决计数。", subAgentID),
					}); err != nil {
						log.Printf("[subagent] pause notify parent failed: to=%s err=%v", parentID, err)
					}
				}
				log.Printf("[subagent] MANUAL-PAUSED: sub=%s role=%s duration=%s (awaiting resume)", subAgentID, roleDef.ID, duration)
				return true
			}
			// 软停止分流（TODO #37）：会话软停止标记命中时——
			//   domain：SaveMessages 存完整 history + tree.Pause（可续跑），不 notify 不
			//     trackChildDone（PendingChildren 保持 >0 → 父终结保护 → MetaAgent PausedOnChild，
			//     恢复路由零改动生效）；
			//   叶子助手：无 Pause 语义，部分回灌父（treeFinish Done + notify + trackChildDone），
			//     domain 续跑后按需重派。
			if d.isSoftStop(sid) {
				if roleDef.ID == "domain" {
					d.savePausedHistory(ctx, subAgentID, sid, result.History)
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
				d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)
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
			// 硬取消路径清残留手动暂停标记（竞态：StopRunning 前节点已被取消）。
			d.ClearPauseNode(subAgentID)
			// 终态落库（编排页对话视图）：取消路径 ctx 已取消，saveTerminalHistory 内部脱取消。
			d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)
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
		// 结构化遗产清单（TODO #72）：成功写入文件 + 看板在办步骤 + 打捞摘要，
		// 文案明示"可直接作为续建 spec 骨架"——守卫终止后 meta 人肉盘点 5 步未做
		// 白烧一轮（实证 2026-08-25 domain-2 被杀时 plan_execute 仅 1/6）。
		legacy := d.renderLegacyList(ctx, parentID, domain, result)
		// 结构化失败（TODO #23）：头部机读标记 [failure kind=X retryable=Y]，人读文案在后。
		kind := failureKindOf(ctx, err)
		retryable := kind == FailureKindError && roleDef.ID != "domain" && roleDef.ID != "meta" && ctx.Err() == nil
		failText := formatSubAgentFailure(ctx, err, result, d.effectiveTimeout(subAgentID), partial)
		msg := failureMarker(kind, retryable) + "\n" + failText
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
		// 遗产清单（TODO #72）随失败消息送达：成功写入文件 + 在办步骤 + 打捞摘要。
		if legacy != "" {
			msg += "\n\n" + legacy
		}
		// 状态语义三态化（TODO #60）：缺验证证据（verify_missing/unverified）不是失败——
		// 树落 delivered-unverified、看板标黄不标红，让父 Agent 看到"产出已交付但没证据"
		// 而非满屏错误；真失败（冒烟不过/超时/守卫）仍标红。
		boardSt := board.TaskFailed
		treeStatus := orchestrator.StatusFailed
		if kind == FailureKindUnverified || kind == FailureKindVerifyMissing {
			boardSt = board.TaskUnverified
			treeStatus = orchestrator.StatusUnverified
		}
		d.boardUpdate(ctx, parentID, domain, boardSt, truncateRunes(msg, 300))
		d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)
		d.treeFinishStatus(ctx, subAgentID, partial, treeStatus, failText)
		d.notify(parentID, subAgentID, msg, files)
		return false
	}

	// 成功：把结果摘要通知父 Agent。产出质量由分层自检保证（叶子自检 / 领域收尾验收 /
	// meta 纸面交付对照+返工，见 roles.yaml 提示词），完成路径不再自动派验证 Agent--
	// 整品验收 Agent 2026-08-24 切出默认流程：isVerificationTask +【集成验证任务模式】保留为口子。
	// A/B 实证自动验证闭环是负资产（开 5/16 vs 关 16/16），机制移至扩展设计文档 §12 作后期扩展。
	log.Printf("[subagent] DONE: sub=%s role=%s duration=%s result_len=%d", subAgentID, roleDef.ID, duration, len(result.Text))
	d.boardUpdate(ctx, parentID, domain, board.TaskDone, result.Text)
	d.saveTerminalHistory(ctx, subAgentID, tool.SessionIDFromContext(ctx), result.History)
	d.treeFinish(ctx, subAgentID, result.Text, nil)
	// 校验分层（TODO #43）状态标注：VerifyNote 非空=校验通过（L0 证据/L2 rubric），
	// 完成摘要前缀一行，父 Agent 可见校验依据；空=未启用校验（none），零变化。
	// 域完成机器校验（TODO #56）：MachineCheck 非空时把【机器校验】段追加进摘要，
	// dispatcher 执行的客观证据，meta 验收只信这段 + spec 纸面对照（#58）。
	// 终答【未验证项】强制（TODO #76）：建设域（domain）摘要缺该段时追加机器警告行
	//（可观测不硬拒），meta 纸面对照时不得按"全过"口径采信缺声明的摘要。
	// 叶子助手简短回传不适用（非 meta 验收对象，警告只会刷屏）。
	if roleDef.ID == "domain" && unverifiedSectionMissing(result.Text) {
		result.MachineCheck = appendUnverifiedWarning(result.MachineCheck)
	}
	summary := result.Text
	if result.MachineCheck != "" {
		summary = strings.TrimRight(summary, "\n") + "\n\n" + result.MachineCheck
	}
	if result.VerifyNote != "" {
		summary = fmt.Sprintf("【校验:通过(%s)】\n%s", result.VerifyNote, summary)
	}
	// worktree 隔离派发（TODO 第9⑤/#10⑤）成功收尾：产出全量 patch 落盘并把
	// 【worktree 交付】附言拼进摘要——meta 据此经 merge_worktree review/merge/reject
	// 走合并门；句柄不存在（非 worktree 派发）返回空串零打扰。
	if note := d.worktreePatchNote(ctx, subAgentID); note != "" {
		summary += note
	}
	d.notify(parentID, subAgentID, summary, files)
	return false
}

// runSubAgentWithAutoRetry 包装 runSubAgentOnce：叶子助手 kind=error 失败自动重派一次
// （TODO #23 最小一档，同任务同前缀，fresh 计数）。domain/timeout/killed/loop_guard/
// 预算部分返回不自动重试——domain 交 MetaAgent 决策、墙钟类重试无意义，避免放大故障。
// 与 LLM 调用层重试（react_agent retry_count）正交：那层重试模型调用本身，这层重跑整个 Agent。
// mode 为派发执行模式，自动重派沿用同一模式。
// 返回 (result, err, retried)：retried=true 表示本轮失败已重试过一次（二次失败终报）。
func (d *Dispatcher) runSubAgentWithAutoRetry(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility, mode, verifyKind string) (agent.ReactResult, error, bool) {
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind)
	if err == nil || d.dispatchRetryCount <= 0 {
		return result, err, false
	}
	kind := failureKindOf(ctx, err)
	retryable := kind == FailureKindError && roleDef.ID != "domain" && roleDef.ID != "meta" && ctx.Err() == nil
	if !retryable {
		return result, err, false
	}
	log.Printf("[subagent] AUTO-RETRY: sub=%s role=%s err=%v (dispatch retry %d)", subAgentID, roleDef.ID, err, d.dispatchRetryCount)
	_, result2, err2 := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task, domain, responsibility, mode, verifyKind)
	return result2, err2, true
}

// savePausedHistory 暂停收尾把子 Agent 完整 history 落 msgStore，供 ResumePaused 续跑。
// 收尾 ctx 已被 StopRunning 取消：必须用脱离取消的 ctx（否则存不进去、续跑报
// "no persisted messages"，e2e 实证），附 10s 超时防慢 PG 阻塞收尾 goroutine。
func (d *Dispatcher) savePausedHistory(ctx context.Context, subAgentID, sid string, history []agent.ReactMessage) {
	if d.msgStore == nil || history == nil {
		return
	}
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelSave()
	if err := d.msgStore.SaveMessages(saveCtx, subAgentID, sid, history); err != nil {
		log.Printf("[subagent] pause save messages failed: sub=%s err=%v", subAgentID, err)
	}
}

// saveTerminalHistory 子 Agent 终态落库完整 history（编排页对话视图 PG 全量源）。
// 复用 savePausedHistory 的"脱离取消 ctx + 10s 超时"语义——取消/软停路径 ctx 已取消也能存。
func (d *Dispatcher) saveTerminalHistory(ctx context.Context, subAgentID, sid string, history []agent.ReactMessage) {
	d.savePausedHistory(ctx, subAgentID, sid, history)
}

// treeFinish 把子 Agent 终态写入权威树。treeFn 为 nil 或 sessionID 缺失时静默跳过。
// 幂等：orchestrator.Tree.Finish 对已 terminal 节点 no-op。
func (d *Dispatcher) treeFinish(ctx context.Context, subAgentID, summary string, err error) {
	if err != nil {
		d.treeFinishStatus(ctx, subAgentID, summary, orchestrator.StatusFailed, err.Error())
		return
	}
	d.treeFinishStatus(ctx, subAgentID, summary, orchestrator.StatusDone, "")
}

// treeFinishStatus 按指定终态写树（TODO #60 三态化）：StatusUnverified 走
// Tree.FinishUnverified（非失败语义、看板标黄），其余与 treeFinish 同语义。
func (d *Dispatcher) treeFinishStatus(ctx context.Context, subAgentID, summary string, status orchestrator.Status, errText string) {
	// 终态漏斗：节点死亡即回收实例级模型覆盖（暂停路径不走本函数，覆盖跨 resume 存活）。
	d.clearAgentModel(subAgentID)
	if d.treeFn == nil {
		return
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		return
	}
	if t := d.treeFn(sid); t != nil {
		switch status {
		case orchestrator.StatusUnverified:
			t.FinishUnverified(subAgentID, summary, errText)
		default:
			if errText != "" {
				t.Finish(subAgentID, summary, errors.New(errText))
			} else {
				t.Finish(subAgentID, summary, nil)
			}
		}
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
	provider, err := d.providerForAgent(ctx, roleDef.ID, subAgentID)
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
		WithProviderFunc(func(callCtx context.Context) (agent.ModelProvider, error) {
			return d.providerForAgent(callCtx, roleDef.ID, subAgentID)
		}).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor(roleDef.ID)).
		WithWorkDir(d.subAgentWorkDirFor(ctx))
	// 技能渐进披露第一层：【可用技能】元数据块注入系统提示（正文经 load_skill 按需取）。
	// 持有集以 heldSkills 为权威（未登记回退角色固定集），skillPool nil 时零行为。
	sub = sub.WithSkillBlock(d.skillBlockFor(subAgentID, &roleDef))
	// 注入未决子 Agent 检查器：子 Agent 也能递归派发（domain -> 叶子助手），
	// 无此检查时子 Agent 会在派发后立刻给出中间汇报式终答（不等待 mailbox），
	// 父链路上的 Agent 会把“中间状态”误当最终结果（实证：domain-1 拆两个子任务后
	// 直接 DONE，MetaAgent 把“等待 mailbox 结果”当终答，会话 completed 但产出缺失）。
	// Dispatcher 自身实现 PendingChildrenChecker（PendingChildren/WaitForAnyChild）。
	sub = sub.WithPendingChildrenChecker(d)

	// 心跳检活：注入语义化活动上报回调（TODO 第10项②证据化——llm_start/llm_end/tool:<名>/
	// tool_end/stream/keepalive 各 kind 由 activityEvidence.report 分类记录，巡检据此
	// 区分"真静默"与"在飞 LLM/长工具"）。回调同时把活动沿 parentID 链向上冒泡
	//（TODO #25-3）：domain 等子期间自身无活动，靠后代活动刷新保持存活，巡检不误杀合法等待。
	if e := d.activityEvidenceFor(subAgentID); e != nil {
		sub = sub.WithActivityReporter(d.activityReporterFn(subAgentID))
	}

	// 消息热层（编排页对话视图）：逐条热写 Redis，终态全量落 PG（runSubAgent 收尾）。
	if d.msgLogger != nil {
		sub = sub.WithMessageLogger(d.msgLogger)
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
			d.recordRecentActivity(subAgentID, ev)
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
	// 捕获父 spec 结构化切片（TODO #56/#57）：派发时 spec 尚新鲜，完成后 Layer 2
	// 失效删除 spec 就再也读不到——冒烟检查目标与契约检查依赖此刻的捕获。
	d.recordParentSpec(ctx, parentID, domain)
	// 上下文前缀注入：共享记忆（spec + 自由槽位）+ 块记忆召回。两段独立前缀统一拼装，避免嵌套
	// 【当前任务】标记（实证：嵌套后 UI 助手把 KV 内容当作任务主体，空转 16 分钟）。
	// TODO 第七项③：各路注入 runes 逐路记账（ctx_inject 日志），供注入收口定默认值。
	var prefixes []string
	injectRunes := map[string]int{}
	// 项目自述（TODO 第10项⑦）：workDir 根部 AGENTS.md/CLAUDE.md。置于所有前缀首位——
	// 它是会话内最稳的段（文件不变则逐字节一致），越靠前越利于跨派发前缀缓存复用。
	if brief := d.projectBriefPrefix(ctx); brief != "" {
		prefixes = append(prefixes, brief)
		injectRunes["agents_md"] = len([]rune(brief))
	}
	if sp := d.buildSharedPrefix(ctx, parentID, domain); sp != "" {
		prefixes = append(prefixes, sp)
		injectRunes["shared_prefix"] = len([]rune(sp))
	}
	// 项目偏好（2026-09-02 设计 §5）：本项目约定与经验下发给全部子 Agent（执行层工艺）。
	if pp := d.projectPrefsPrefix(ctx); pp != "" {
		prefixes = append(prefixes, pp)
		injectRunes["project_prefs"] = len([]rune(pp))
	}
	// 经验技能召回（2026-09-02 设计 §6.5）：task 向量预筛 top-3，只注一行提示不注全文。
	if sr := d.skillRecallPrefix(ctx, origTask); sr != "" {
		prefixes = append(prefixes, sr)
		injectRunes["skill_recall"] = len([]rune(sr))
	}
	if bm, recs := d.injectScopedRecall(ctx, parentID, domain, origTask, ""); bm != "" {
		prefixes = append(prefixes, bm)
		injectRunes["block_recall"] = len([]rune(bm))
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
	// TODO 第七项③：五路注入 runes 记账（数据供 ⑥ 参数化 knobs 与 P0-4 收口 <20K 定默认值）。
	if roleDef.ID != "meta" {
		log.Printf("[subagent] ctx_inject: sub=%s role=%s agents_md=%d shared_prefix=%d project_prefs=%d skill_recall=%d block_recall=%d task=%d runes",
			subAgentID, roleDef.ID, injectRunes["agents_md"], injectRunes["shared_prefix"], injectRunes["project_prefs"], injectRunes["skill_recall"], injectRunes["block_recall"], len([]rune(origTask)))
	}

	vk := resolveVerifyKind(verifyKind, roleDef.ID, mode)
	// 验证证据格式模板（TODO #63）：L0 可执行校验角色在任务尾部注入证据格式要求，
	// 与识别器口径同源（verificationCommandPatterns）——从源头消灭"回传了验证输出
	// 但格式不认"的 verify_missing（实证 2026-08-24 塔防 7 连发）。
	if vk == verifyKindExecutable {
		task += "\n\n" + tool.VerificationEvidenceTemplate()
	}
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
			result, err = sub.RunWithHistory(ctx, l0RetryMessage(), result.History)
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
		d.saveBlockMemory(ctx, subAgentID, roleDef.ID, parentID, domain, origTask, partial, blockOutcomePartial, agent.FilesModifiedFromHistory(result.History), result.History)
		log.Printf("[subagent] PARTIAL: sub=%s role=%s (token budget, partial returned)", subAgentID, roleDef.ID)
		return sub, result, errPartialReturn
	}

	// 域完成机器校验冒烟层（TODO #56）：对 spec.files ∩ 本子 Agent 实际写入文件
	// 自动派生并执行语法检查（node --check 等），结果以【机器校验】段入完成摘要。
	// 失败走 verify_kind 反馈重试通道：反馈 1 轮自修，仍失败按 errSmokeFailed 打回父
	// （复用 #43 校验分层路由，不新建通路）。
	if targets := d.smokeTargetsFor(parentID, domain, result.History); len(targets) > 0 {
		results := runSmokeChecks(ctx, d.subAgentWorkDir(), targets, d.smokeRunner, d.smokeLookPath)
		if failed := smokeFailed(results); len(failed) > 0 {
			log.Printf("[subagent] smoke check failed: sub=%s role=%s failed=%d (retry 1 round)", subAgentID, roleDef.ID, len(failed))
			result, err = sub.RunWithHistory(ctx, smokeFixMessage(failed), result.History)
			if err != nil {
				return sub, result, fmt.Errorf("run: %w", err)
			}
			if result.LimitReached {
				return sub, result, errSmokeFailed
			}
			results = runSmokeChecks(ctx, d.subAgentWorkDir(), targets, d.smokeRunner, d.smokeLookPath)
			if failed := smokeFailed(results); len(failed) > 0 {
				log.Printf("[subagent] smoke check still failed: sub=%s role=%s failed=%d", subAgentID, roleDef.ID, len(failed))
				return sub, result, fmt.Errorf("%w:\n%s", errSmokeFailed, smokeFixMessage(failed))
			}
		}
		if smokeRunCount(results) > 0 {
			result.MachineCheck = renderSmokeReport(results)
		}
		// JS/HTML 单文件引用完整性档（TODO #71 冒烟层第三档）：>300 行的 .js/.html
		// 追加 tsc --allowJs --checkJs（硬判）或轻量扫描（标存疑）。防"定义 playSwoosh
		// 调用 playSwooshSound"类文件内引用不一致——LLM 单文件长代码最高发错误
		// （实证 2026-08-25 水果忍者线上缺陷 100% 属此类）。
		if refNote, refFailed := runJSRefChecks(ctx, d.subAgentWorkDir(), targets, d.smokeRunner, d.smokeLookPath); refNote != "" {
			if refFailed {
				log.Printf("[subagent] js reference check failed: sub=%s role=%s (retry 1 round)", subAgentID, roleDef.ID)
				result, err = sub.RunWithHistory(ctx, "【机器校验失败】JS/HTML 引用完整性检查未通过（调用点标识符未定义）——请修复未定义引用后重新自检:\n"+refNote, result.History)
				if err != nil {
					return sub, result, fmt.Errorf("run: %w", err)
				}
				if result.LimitReached {
					return sub, result, errSmokeFailed
				}
				if refNote2, refFailed2 := runJSRefChecks(ctx, d.subAgentWorkDir(), targets, d.smokeRunner, d.smokeLookPath); refFailed2 {
					log.Printf("[subagent] js reference check still failed: sub=%s role=%s", subAgentID, roleDef.ID)
					return sub, result, fmt.Errorf("%w:\nJS/HTML 引用完整性检查未通过:\n%s", errSmokeFailed, refNote2)
				}
			}
			result.MachineCheck = strings.TrimRight(result.MachineCheck, "\n") + "\n\n【机器校验】JS/HTML 引用完整性（冒烟层第三档）:\n" + refNote
		}
	}

	// 验收分层（TODO #59）：spec verify_levels 驱动集成层探针 + 视觉层证据强制。
	// 存在性/静态层已由上方冒烟检查覆盖；此处补 integration（入口引用图）、
	// runtime（探针证据，TODO #67 机器强制）与 visual（截图，TODO #69 场景化去重）。
	// 失败均走反馈重试 1 轮：integration 违例=真缺陷打回（复用 errSmokeFailed 路由），
	// runtime/visual 缺证据=产出可能可用但未验证（errVisualEvidenceMissing → delivered-unverified 黄态）。
	if rec := d.parentSpecRecordOf(parentID, domain); rec != nil && len(rec.verifyLevels) > 0 {
		levels := make(map[string]bool, len(rec.verifyLevels))
		for _, l := range rec.verifyLevels {
			levels[l] = true
		}
		if levels["integration"] {
			irep := runIntegrationChecks(d.subAgentWorkDir(), rec.files)
			if failed := integrationFailed(irep); len(failed) > 0 {
				log.Printf("[subagent] integration check failed: sub=%s role=%s failed=%d (retry 1 round)", subAgentID, roleDef.ID, len(failed))
				result, err = sub.RunWithHistory(ctx, integrationFixMessage(failed), result.History)
				if err != nil {
					return sub, result, fmt.Errorf("run: %w", err)
				}
				if result.LimitReached {
					return sub, result, errSmokeFailed
				}
				irep = runIntegrationChecks(d.subAgentWorkDir(), rec.files)
				if failed := integrationFailed(irep); len(failed) > 0 {
					log.Printf("[subagent] integration check still failed: sub=%s role=%s failed=%d", subAgentID, roleDef.ID, len(failed))
					return sub, result, fmt.Errorf("%w:\n%s", errSmokeFailed, integrationFixMessage(failed))
				}
			}
			if section := renderIntegrationReport(irep); section != "" {
				result.MachineCheck = strings.TrimRight(result.MachineCheck, "\n") + "\n\n" + section
			}
		}
		// runtime 层机器强制（TODO #67）：探针证据 = ui_preview__browser_navigate 成功 +
		// ui_preview__browser_evaluate 断言成功 + console 回读无 error，缺任一判未验证
		//（HasRuntimeProbeEvidence 按裸名匹配，兼容无前缀历史记录）。
		// 与 visual 层同构：缺证据重试 1 轮 → 仍缺 → delivered-unverified 黄态。
		if levels["runtime"] && !agent.HasRuntimeProbeEvidence(result.History) {
			log.Printf("[subagent] verify runtime probe missing: sub=%s role=%s (retry 1 round)", subAgentID, roleDef.ID)
			result, err = sub.RunWithHistory(ctx, runtimeProbeRetryMessage(rec.probes), result.History)
			if err != nil {
				return sub, result, fmt.Errorf("run: %w", err)
			}
			if result.LimitReached {
				return sub, result, errVisualEvidenceMissing
			}
			if !agent.HasRuntimeProbeEvidence(result.History) {
				log.Printf("[subagent] verify runtime probe still missing: sub=%s role=%s", subAgentID, roleDef.ID)
				return sub, result, errVisualEvidenceMissing
			}
		}
		// visual 层场景化判定（TODO #69）：scenes 非空时按内容去重 + 数量覆盖 +
		// 邻接证据判定（同图连拍充数无效）；scenes 空时退回单截图判定（零行为变化）。
		if levels["visual"] {
			ok, retryMsg := visualEvidenceCheck(result.History, rec.scenes)
			if !ok {
				log.Printf("[subagent] verify visual insufficient: sub=%s role=%s scenes=%d (retry 1 round)", subAgentID, roleDef.ID, len(rec.scenes))
				result, err = sub.RunWithHistory(ctx, retryMsg, result.History)
				if err != nil {
					return sub, result, fmt.Errorf("run: %w", err)
				}
				if result.LimitReached {
					return sub, result, errVisualEvidenceMissing
				}
				if ok2, _ := visualEvidenceCheck(result.History, rec.scenes); !ok2 {
					log.Printf("[subagent] verify visual still insufficient: sub=%s role=%s scenes=%d", subAgentID, roleDef.ID, len(rec.scenes))
					return sub, result, errVisualEvidenceMissing
				}
			}
		}
		// 验收条目逐项计分（TODO #68/#75）：按 evidence 类型挂机器证据，
		// N/M 计数由 dispatcher 计算（meta 只读不自算）；quality 层条目缺证据
		// 时整体不标绿（计分报告标记 qualityBlocked）。
		if score := scoreAcceptance(result.History, rec.acceptance, rec); score.total > 0 {
			if section := renderAcceptanceScore(score); section != "" {
				result.MachineCheck = strings.TrimRight(result.MachineCheck, "\n") + "\n\n" + section
			}
			// 有 quality 层条目未过 → 整体降级为未验证（不标绿，TODO #75）：
			// quality 条目全部 manual 时无机器证据可判，不在此拦截（归 rubric/人工）。
			if score.qualityMissing > 0 && score.qualityEvidence > 0 {
				return sub, result, errVisualEvidenceMissing
			}
		}
	}

	_ = mem.Write(subAgentID, agent.MemoryEvent{
		Type:    "task_goal_summary",
		AgentID: subAgentID,
		Role:    roleDef.ID,
		Content: result.Text,
	})

	d.saveBlockMemory(ctx, subAgentID, roleDef.ID, parentID, domain, origTask, result.Text, blockOutcomeSuccess, agent.FilesModifiedFromHistory(result.History), result.History)

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

// engineLLMKeepalive 退役（TODO 第10项②证据化）：引擎辅助 LLM 在飞改由
// activityEvidence.beginAuxLLM/endAuxLLM 豁免巡检，保活 ticker 盲报随之移除。

// wrapEngineLLM 为引擎辅助 LLM 调用（自检 judge / plan_execute 规划）加超时与活动豁免。
// 两个盲区一起堵（2026-08-19 引擎 Agent 70 分钟事故）：
//  1. 超时：该路径无 CallLLM 包装，SDK 默认 600s/请求 × provider 3 次重试 × judge 内部
//     重试叠加可烧 ~70 分钟；外层 WithTimeout 包住整次调用（含 provider 重试），到期后
//     后续重试因 ctx 已耗尽立即失败（快速失败，不再重试-再超时循环）。
//  2. 巡检：judge 长生成期间 ReAct 主循环零活动，证据化判定（TODO 第10项②）把在飞 LLM
//     一律豁免——beginAuxLLM/endAuxLLM 标记在飞态即可，旧保活 ticker（盲报刷新 lastTS
//     续命）随之退役；真实挂死由 engineLLMTimeout 与子 Agent 墙钟兜底。
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
		if e := d.activityEvidenceFor(subAgentID); e != nil {
			e.beginAuxLLM(time.Now().UnixNano())
			defer e.endAuxLLM(time.Now().UnixNano())
		}
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
// 证据格式要求与识别器口径同源（TODO #63）：tool.VerificationEvidenceTemplate
// 由 verificationCommandPatterns 生成，识别器认什么 prompt 就要求什么。
func l0RetryMessage() string {
	return "【验证要求】终答前必须运行验证命令并依据结果修正问题（识别口径见下）：\n" +
		tool.VerificationEvidenceTemplate() + "\n请运行验证命令后重新产出最终答复。"
}

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

// ResumePaused 恢复一个 Paused 的子 Agent（触限挂起、会话软停或 pause_agent 手动暂停）：
// 从 msgStore 加载其完整消息历史，用 fresh budget（上下文 token 每轮独立估算）按节点角色
// 重建 ReActAgent 续跑。不强制压缩——靠 Assemble 压缩自动触发（Pipeline 状态延续：
// 同 subAgentID -> compressCounters/events 跨 resume 保留）。
//
// 角色泛化：domain 与固定角色叶子同路径；domain 专属行为（领域名覆写、兄弟产出摄取）
// 按 roleDef.ID == "domain" 分支。
//
// 生命周期：
//   - 再触限：SaveMessages 覆盖 + tree.Pause + 通知父 + 返回 result.LimitReached=true。
//   - 完成：tree.Finish Done + notify 父 mailbox + trackChildDone（父 Agent 解除阻塞）。
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

	roleDef := d.registry.Get(pausedNode.Role)
	if roleDef == nil {
		return agent.ReactResult{}, fmt.Errorf("role %q not found", pausedNode.Role)
	}
	// 领域名覆写仅 domain（叶子用角色名，与派发路径一致）。
	if roleDef.ID == "domain" {
		if hint := strings.TrimSpace(pausedNode.Domain); hint != "" {
			roleDef.Name = textutil.TruncateRunes(hint, 16, "…") + "领域Agent"
		}
	}

	provider, err := d.providerForAgent(ctx, roleDef.ID, pausedNodeID)
	if err != nil {
		return agent.ReactResult{}, fmt.Errorf("get model: %w", err)
	}
	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}
	// 黑板模式（TODO #42）：resume 的 domain Agent 同样每轮摄取兄弟产出--它正是因等兄弟而暂停的，
	// 恢复后兄弟可能已完成，uptake 让它立即见到兄弟结论而非空等/重做。叶子无兄弟语义。
	if roleDef.ID == "domain" {
		if bb, ok := d.searcher.(BlackboardSearcher); ok && strings.TrimSpace(pausedNode.Domain) != "" {
			mem = newSiblingUptakePipeline(mem, bb, sid, pausedNode.ParentID, pausedNode.Domain, pausedNodeID)
		}
	}
	// 不注入编排者人格（理由同 runSubAgentOnce：身份混淆实证）。
	sub := agent.NewReActAgent(pausedNodeID, *roleDef, provider, agent.NewToolRegistryAdapterForRole(d.tools, pausedNodeID, roleDef.Tools, roleDef.ID, d.pluginVisibility)).
		WithProviderFunc(func(callCtx context.Context) (agent.ModelProvider, error) {
			return d.providerForAgent(callCtx, roleDef.ID, pausedNodeID)
		}).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor(roleDef.ID)).
		WithWorkDir(d.subAgentWorkDirFor(ctx))
	// resume 重建的 Agent 恢复技能块：持有集从 heldSkills 取（一次性路径派发时已登记；
	// 进程重启丢失则回退角色固定集）。
	sub = sub.WithSkillBlock(d.skillBlockFor(pausedNodeID, roleDef))
	// 同 runSubAgentOnce：resume 重建的 domain Agent 也可能继续递归派发，
	// 需要终结保护等待自己的子 Agent（Dispatcher 自身实现 PendingChildrenChecker）。
	sub = sub.WithPendingChildrenChecker(d)
	// 消息热层（编排页对话视图）：resume 重建的 Agent 同样逐条热写；缺了会让对话页
	// 停在暂停前的旧热层快照（热层非空即不再回退 PG），续跑内容永久不可见。
	if d.msgLogger != nil {
		sub = sub.WithMessageLogger(d.msgLogger)
	}
	if d.log != nil {
		sub = sub.WithLogger(d.log.WithSession(sid).WithAgent(roleDef.Name))
	}
	if d.liveFn != nil {
		forwarder := d.liveFn
		sub = sub.WithLiveEvents(func(ev agent.LiveEvent) { forwarder(sid, ev) })
	}

	// 中断传播基底（TODO 第10④）：入站 ctx 携带会话 stopCtx 时以其为基底（恢复路径
	// 同样受会话 stop 管辖），未携带回退 Background。
	subCtx := tool.StopContextFrom(ctx)
	if subCtx == nil {
		subCtx = context.Background()
	}
	subCtx = tool.WithSessionID(subCtx, sid)
	// 每会话工作目录（S2）：subCtx 由基底重建，从入站 ctx（resumePausedDomain
	// 已注入）显式重注入，保证 resume 的 domain Agent 工具执行落在会话工作目录。
	subCtx = tool.WithWorkDir(subCtx, tool.WorkDirFromContext(ctx))
	cancel := context.CancelFunc(func() {})
	if d.timeout > 0 {
		subCtx, cancel = context.WithTimeout(subCtx, d.timeout)
	}
	t.Resume(pausedNodeID, cancel)
	d.running.Store(pausedNodeID, sub)
	// 心跳（TODO #25-3）：resume 的 domain 注册 activity 证据，其子 Agent 活动冒泡保活；
	// 巡检超 domain 阈值判假死。defer 清理与子 Agent 派发路径一致。
	d.activity.Store(pausedNodeID, newEvidence())
	defer d.activity.Delete(pausedNodeID)
	defer d.running.Delete(pausedNodeID)
	defer d.heldSkills.Delete(pausedNodeID)
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
		d.notify(parentID, pausedNodeID, formatSubAgentFailure(subCtx, err, result, d.timeout, partial), files)
		d.trackChildDone(parentID)
		return result, err
	}
	if result.LimitReached {
		if saveErr := d.msgStore.SaveMessages(subCtx, pausedNodeID, sid, result.History); saveErr != nil {
			log.Printf("[subagent] resume re-pause save messages failed: sub=%s err=%v", pausedNodeID, saveErr)
		}
		t.Pause(pausedNodeID, "token budget exhausted (resume)")
		// 再触限：父必须知情（否则父等 resume 时该节点静默占着未决计数）。
		d.notify(parentID, pausedNodeID, "子 Agent 再次触达 token 上限已重新暂停（上下文保留）；可稍后 resume_agent 继续，或 cancel_agent 放弃并按部分产出收口。", agent.FilesModifiedFromHistory(result.History))
		log.Printf("[subagent] resume RE-PAUSED: sub=%s (token budget)", pausedNodeID)
		return result, nil
	}

	log.Printf("[subagent] resume DONE: sub=%s result_len=%d", pausedNodeID, len(result.Text))
	// 终态全量落 PG（编排页对话视图权威源）：与 pause/失败分支同口径。
	d.saveTerminalHistory(subCtx, pausedNodeID, sid, result.History)
	d.treeFinish(subCtx, pausedNodeID, result.Text, nil)
	d.notify(parentID, pausedNodeID, result.Text, files)
	d.trackChildDone(parentID)
	return result, nil
}

// resumePausedNode 非热驻暂停节点的续跑入口：自建携带 SessionID/WorkDir/StopContext 的
// ctx（工具调用 ctx 在工具返回后即失效），goroutine 内调 ResumePaused——续跑是完整任务执行，
// 不能阻塞调用方（父 Agent）的工具轮。
//
// 失败兜底：ResumePaused 的前置/加载失败分支不回传父，此处若节点仍为 Paused 则补一条
// 失败通知（否则父等不到回灌也收不到失败，永久挂账）；成功路径的完成/失败回传由
// ResumePaused 内部负责（不重复通知）。
func (d *Dispatcher) resumePausedNode(ctx context.Context, nodeID string) {
	sid := tool.SessionIDFromContext(ctx)
	base := tool.StopContextFrom(ctx)
	if base == nil {
		base = context.Background()
	}
	subCtx := tool.WithSessionID(base, sid)
	if wd := tool.WorkDirFromContext(ctx); wd != "" {
		subCtx = tool.WithWorkDir(subCtx, wd)
	}
	go func() {
		if _, err := d.ResumePaused(subCtx, nodeID); err != nil {
			log.Printf("[subagent] resume failed: sub=%s err=%v", nodeID, err)
			if d.treeFn != nil {
				if t := d.treeFn(sid); t != nil {
					if n, ok := t.Get(nodeID); ok && n.Status == orchestrator.StatusPaused {
						d.notify(n.ParentID, nodeID,
							failureMarker(FailureKindError, false)+"\n续跑失败: "+err.Error()+
								"（该节点仍为暂停态：可重试 resume_agent，或 cancel_agent 放弃并按已有产出收口）", nil)
					}
				}
			}
		}
	}()
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
	// FailureKindSmokeFailed 域完成机器校验冒烟失败（TODO #56）：dispatcher 对产出文件
	// 自动执行的语法检查（node --check 等）未通过，反馈重试 1 轮后仍失败。
	// retryable=false，父 Agent 打回责任域自修。
	FailureKindSmokeFailed FailureKind = "smoke_failed"
	// FailureKindContractViolation 跨域契约违例（TODO #57）：兄弟域全完成后的静态契约检查
	// 发现违例条目。retryable=false，父 Agent 按文件归属打回责任域。
	FailureKindContractViolation FailureKind = "contract_violation"
)

// failureKindOf 从失败错误分类失败类型；未知错误归 error。
func failureKindOf(ctx context.Context, err error) FailureKind {
	switch {
	case isLLMCallDeadline(ctx, err):
		// LLM 调用内部超时（endpoint 假死/单次调用超时）≠ 子 Agent 墙钟超时：
		// 归 error 让父 Agent 看到"模型层故障"而非"执行超时"。
		return FailureKindError
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
	case errors.Is(err, errVisualEvidenceMissing):
		// 视觉层缺证据（TODO #59）：语义同 verify_missing——产出可用但缺截图回显，
		// 非失败；统一走 FailureKindVerifyMissing 的三态化（delivered-unverified 黄态）。
		return FailureKindVerifyMissing
	case errors.Is(err, errSmokeFailed):
		return FailureKindSmokeFailed
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

// errVisualEvidenceMissing 标记视觉层缺截图回显证据（TODO #59 验收分层 visual 层）：
// spec verify_levels 含 visual 的任务（UI/游戏/绘制类），重试 1 轮后仍无成功的
// ui_preview__browser_take_screenshot 证据。语义同 verify_missing（产出可用但未验证），
// failureKindOf 归并到 FailureKindVerifyMissing → delivered-unverified 黄态。
var errVisualEvidenceMissing = errors.New("sub-agent missing visual (screenshot) evidence")

// visualRetryMessage 是视觉层缺截图证据时的 1 轮反馈重试指令（TODO #59）。
const visualRetryMessage = "【视觉证据要求】本任务验收层级含 visual（UI/游戏/绘制类）：终答前必须" +
	"经 tool_catalog 挂载 ui_preview，ui_preview__browser_navigate 打开页面后 ui_preview__browser_take_screenshot " +
	"截图回显实际渲染效果（file:///workspace/<相对工作目录> 路径），截图成功即视觉证据。" +
	"截图必须展示真实渲染结果（贴图/动画/布局可见），仅空页面不算。请补充截图证据后重新产出最终答复。"

// errSmokeFailed 标记域完成机器校验冒烟失败（TODO #56）：dispatcher 自动执行的
// 语法检查未通过（反馈重试 1 轮后仍失败），错误文本携带失败明细。
// runSubAgent 见此信号按 FailureKindSmokeFailed notify 父（打回责任域）。
var errSmokeFailed = errors.New("sub-agent smoke check failed")

// isLLMCallDeadline 判定 DeadlineExceeded 是否源自单次 LLM 调用内部（provider
// http.Client 整体超时 / react_llm_timeout），而非子 Agent 墙钟到期。两者同走
// context.DeadlineExceeded（墙钟到期同样从 provider 读流处冒出同形错误），用
// 运行 ctx 是否已终结判别：ctx 已到期=墙钟终止；ctx 存活而调用超时=LLM 层故障。
// 邮箱文案不能把 LLM 10 分钟假死写成"墙钟上限超时"误导父 Agent 决策
// （实证 2026-08-27 domain-1/4 双 10m0.0s FAIL 被报成 2h 超时）。
func isLLMCallDeadline(ctx context.Context, err error) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "llm generate") || strings.Contains(msg, "Client.Timeout")
}

// formatSubAgentFailure 把 runSubAgentOnce 返回的错误格式化为父邮箱通知文案，
// 保留原有"超时/循环守卫/通用失败"三段语义与部分进度回传；
// 校验分层两类（TODO #43）单独文案，明确"未验证"而非"失败"语义。
func formatSubAgentFailure(ctx context.Context, err error, result agent.ReactResult, timeout time.Duration, partial string) string {
	if isLLMCallDeadline(ctx, err) {
		return fmt.Sprintf("子 Agent 的 LLM 调用超时（模型端无响应或连接假死，非墙钟到期）：%v。%s", err, partialSuffix(partial))
	}
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
	if errors.Is(err, errVisualEvidenceMissing) {
		return "子 Agent 未提供视觉验证证据（UI/游戏/绘制类任务要求 ui_preview__browser_take_screenshot 截图回显，没有成功截图记录）。"
	}
	if errors.Is(err, errSmokeFailed) {
		// 错误文本携带冒烟失败明细（命令 + 退出码 + 输出尾部），剥掉哨兵前缀直陈证据。
		detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), errSmokeFailed.Error()+":"))
		return "子 Agent 产出未通过 dispatcher 机器校验（冒烟检查）：\n" + detail
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
	// 心跳检活：叶子注册 activity 证据 + subMeta（巡检超时 cancel -> childCtx err -> 本方法返回错误）。
	isLeaf := roleDef.ID != "domain" && roleDef.ID != "meta"
	if isLeaf && d.heartbeatTimeout > 0 {
		d.activity.Store(subAgentID, newEvidence())
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

// sharedStaleWarning 是普通共享记忆槽 stale（所涉文件已被修改或已标 invalidated_at）时
// 仍注入、前置的警告行：行号类结论可能漂移，常量值/签名类结论仍可直接采信。
// 旧逻辑 stale 即跳过注入（叠加 registry 物理删除），子 Agent 下次派发从零重读同一批文件
// （实证：单领域 Agent 两小时 ReadFile 610 次 + SearchInFiles 351 次）。
// 措辞不含字面量【任务规范】/【共享记忆】/【当前任务】，避免干扰按标记切分前缀的既有逻辑与测试。
const sharedStaleWarning = "【失效警告】以下共享记忆所涉文件已被修改，行号可能漂移，常量值/签名类结论仍可直接采信：\n"

// sharedPrefixDisciplineNote 是共享前缀尾部固定的反重读纪律行。
// 实证（2026-08-14 塔防 9 叶子并行重绘）：契约已含共享方法签名清单，9 个叶子仍各自
// ReadFile 重读共享代码区（合计 4722 行 ≈ 文件 3.7 倍）——注入内容必须显式声明
// "视为已验证、禁止重读"，否则"认真查询"类通用纪律会驱使子 Agent 回读原文。
// 注意：措辞不得含字面量【任务规范】/【共享记忆】/【当前任务】，避免干扰按标记切分前缀的既有逻辑与测试。
const sharedPrefixDisciplineNote = "【读取纪律】以上注入的任务规范与共享记忆内容视为已验证事实：" +
	"其中已给出的代码、签名与行号无需再用 ReadFile 核对或重读；" +
	"ReadFile 仅限当前任务指派给你的行号范围，不读兄弟任务的代码区段。"

// specSlotName 是 WriteSpec 写入的默认 slot 名，与 tool.SpecSlot 保持一致。
const specSlotName = "spec"

// specKeyFor 计算 spec 存储键（TODO #65 多 key 化）：domain 非空 →
// "<parentID>:spec:<domain>"（兄弟各持各的，staleness 按各自 files 交集隔离）；
// domain 空 → 遗留单键 "<parentID>:spec"（全兄弟共享一份）。
func specKeyFor(parentID, domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return parentID + ":" + specSlotName
	}
	return parentID + ":" + specSlotName + ":" + domain
}

// specRecKey 是 parentSpecs 缓存键：同一 parent 下按 domain 区分 spec。
type specRecKey struct {
	parentID string
	domain   string
}

// specMirror 是 tool.Spec 的本地镜像，避免 subagent 反向 import tool 包。
// 字段名与 JSON tag 必须与 tool.Spec 保持一致。
type specMirror struct {
	Goal         string         `json:"goal"`
	Acceptance   []string       `json:"acceptance,omitempty"`
	Constraints  []string       `json:"constraints,omitempty"`
	Files        []string       `json:"files,omitempty"`
	VerifyLevels []string       `json:"verify_levels,omitempty"`
	Contract     *tool.Contract `json:"contract,omitempty"`
}

// parentSpecRecord 是父 Agent spec 的结构化缓存（TODO #56/#57/#59）：
// files 为 spec.files 的绝对路径，contract 为跨域契约（可 nil），
// verifyLevels 为验收层级（可空，驱动集成/视觉层机器校验）。
// filesMtime 为捕获时刻的 frontmatter files mtime 快照：契约检查前比对，
// 涉及文件已变更（兄弟返工落地）则跳过该份契约（TODO #62 时序串行化变更屏障）。
// 派发时捕获（spec 尚新鲜），完成收尾/兄弟域全完成时消费（spec 可能已被 Layer 2 失效删除）。
// probes/scenes/baseline（TODO #67/#69/#75）：runtime 探针声明 / visual 场景清单 /
// 对标基线，供完成收尾路径的层级证据扫描与计分。
// acceptance（TODO #68）：结构化验收条目（含 evidence/layer 标记行），供逐项计分。
type parentSpecRecord struct {
	files        []string
	contract     *tool.Contract
	verifyLevels []string
	filesMtime   map[string]int64
	probes       []string
	scenes       []string
	baseline     []string
	acceptance   []string
}

// recordParentSpec 捕获 parentID 的 spec 结构化切片进 parentSpecs 缓存。
// domain 非空时读 "<parentID>:spec:<domain>"（多 key 化）；该键缺失时回退遗留
// 单键 "<parentID>:spec"（老流程兼容，兄弟共享一份）。
// spec 缺失/损坏/缺 goal+acceptance 时跳过；捕获失败不阻塞派发主流程
// （冒烟检查与契约检查均为增强证据，缺了就降级跳过）。
func (d *Dispatcher) recordParentSpec(ctx context.Context, parentID, domain string) {
	if d.sharedMem == nil {
		return
	}
	key := specKeyFor(parentID, domain)
	val, err := d.sharedMem.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		if domain == "" {
			return
		}
		// 领域专属 spec 缺失 → 回退遗留单键（老流程：单 spec 全兄弟共享）。
		legacyKey := parentID + ":" + specSlotName
		val, err = d.sharedMem.Get(ctx, legacyKey)
		if err != nil || strings.TrimSpace(val) == "" {
			return
		}
	}
	fm, _, ok := tool.DecodeSharedMD(val)
	if !ok {
		return
	}
	if strings.TrimSpace(fm.Goal) == "" || len(fm.Acceptance) == 0 {
		return
	}
	files := make([]string, 0, len(fm.Files))
	if len(fm.FileList) > 0 {
		// FileList 是 spec.files 全量清单（含当时不存在的待创建文件）；规范化为
		// 绝对路径供冒烟目标匹配（与 smokeTargets 的 modified 归一化同基准：工作目录）。
		workdir := d.subAgentWorkDir()
		for _, p := range fm.FileList {
			ap := filepath.Clean(p)
			if !filepath.IsAbs(ap) && workdir != "" {
				ap = filepath.Join(workdir, ap)
			}
			files = append(files, ap)
		}
	} else {
		for fp := range fm.Files {
			files = append(files, filepath.Clean(fp))
		}
	}
	d.parentSpecs.Store(specRecKey{parentID: parentID, domain: strings.TrimSpace(domain)}, &parentSpecRecord{
		files:        files,
		contract:     fm.Contract,
		verifyLevels: fm.VerifyLevels,
		filesMtime:   fm.Files,
		probes:       fm.Probes,
		scenes:       fm.Scenes,
		baseline:     fm.Baseline,
		acceptance:   fm.Acceptance,
	})
}

// parentSpecRecordOf 读取 parentID 指定 domain 的 spec 缓存；无缓存返回 nil。
// domain 空读遗留单键记录。
func (d *Dispatcher) parentSpecRecordOf(parentID, domain string) *parentSpecRecord {
	v, ok := d.parentSpecs.Load(specRecKey{parentID: parentID, domain: strings.TrimSpace(domain)})
	if !ok {
		return nil
	}
	return v.(*parentSpecRecord)
}

// parentSpecRecordsOf 返回 parentID 下全部 spec 记录（遗留单键 + 各领域专属键）。
// 供兄弟域全完成后的契约检查遍历（多 key 化后契约可能分散在各领域 spec 中）。
func (d *Dispatcher) parentSpecRecordsOf(parentID string) []*parentSpecRecord {
	var out []*parentSpecRecord
	d.parentSpecs.Range(func(k, v any) bool {
		if rec, ok := v.(*parentSpecRecord); ok {
			if key, ok := k.(specRecKey); ok && key.parentID == parentID {
				out = append(out, rec)
			}
		}
		return true
	})
	return out
}

// smokeTargetsFor 计算冒烟检查目标（TODO #56）：spec.files ∩ 本子 Agent 实际写入文件。
// 交集为空（无 spec 缓存 / 子 Agent 未写文件 / 写入文件不在 spec 范围）返回 nil，
// 跳过冒烟检查。收敛到交集是为责任归属精确：并行兄弟域中途写入共享文件时，
// 先完成的一方不会被兄弟的半成品误打回。domain 定位本子 Agent 专属 spec（#65）。
func (d *Dispatcher) smokeTargetsFor(parentID, domain string, history []agent.ReactMessage) []string {
	rec := d.parentSpecRecordOf(parentID, domain)
	if rec == nil {
		return nil
	}
	return smokeTargets(rec.files, agent.FilesModifiedFromHistory(history), d.subAgentWorkDir())
}

// buildSharedPrefix 读取 parentID 下所有共享记忆槽位，渲染为【任务规范】+【共享记忆】前缀。
// 缺失/stale/解析失败时返回空串（graceful degrade，不阻塞派发主流程）。
//
// 槽位两类：
//   - spec 槽位："<parentID>:spec"（WriteSpec 默认键，全兄弟共享）与
//     "<parentID>:spec:<domain>"（TODO #65 多 key 化，领域专属）。domain 非空时优先
//     注入本领域专属 spec；遗留单键对所有子 Agent 注入。MD frontmatter 含
//     goal/acceptance/constraints/files，渲染为【任务规范】段，files mtime 校验失败视为 stale 跳过。
//   - 自由槽位（"<parentID>:<key>"，key 不以 "spec" 开头）：WriteSharedMemory 写入，
//     MD body 为 content，渲染为【共享记忆】段。files mtime 校验失败或 frontmatter 带
//     invalidated_at 标记（所涉文件被 EditFile/WriteFile 改过）时不再丢弃，照常注入
//     并前置 sharedStaleWarning 行号漂移警告（常量值/签名类结论仍可直接采信）。
//
// 返回纯前缀（不含【当前任务】标记），供 runSubAgentOnce 统一拼装多段前缀避免嵌套。
func (d *Dispatcher) buildSharedPrefix(ctx context.Context, parentID, domain string) string {
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
		// 墓碑键（TODO #74 spec 失效留痕）不注入：值非 MD，属诊断信息非任务上下文。
		if strings.HasPrefix(strings.TrimSpace(val), tool.SpecTombstonePrefix) {
			continue
		}
		// 打捞槽位（salvage:<domain>）不走通用共享注入：域相关性强，通用注入会污染
		// 无关子 Agent 上下文；由同域重派经 withPriorSalvage 显式读回（TODO #20 第三层）。
		if strings.HasPrefix(slotName, salvageSlotPrefix) {
			continue
		}
		// 领域专属 spec（#65）：只注入本 domain 的，兄弟的 spec 不注入（各持各的）。
		if strings.HasPrefix(slotName, specSlotName+":") {
			if strings.TrimPrefix(slotName, specSlotName+":") != strings.TrimSpace(domain) {
				continue
			}
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
		// Layer 3 stale 判定：mtime 不匹配或 registry 打过 invalidated_at 标记。
		stale := !verifyFileMtimes(fm.Files) || fm.InvalidatedAt != ""
		if slotName == specSlotName || strings.HasPrefix(slotName, specSlotName+":") {
			// spec 槽位维持现状：stale 丢弃，避免子 Agent 拿旧验收依据干活。
			if stale {
				continue
			}
			// spec 槽位需 goal + 至少一条 acceptance 才视为合法规范。
			if strings.TrimSpace(fm.Goal) == "" || len(fm.Acceptance) == 0 {
				continue
			}
			files := make([]string, 0, len(fm.Files))
			for fp := range fm.Files {
				files = append(files, fp)
			}
			specPart = []string{renderSpecPrefix(specMirror{
				Goal:         fm.Goal,
				Acceptance:   fm.Acceptance,
				Constraints:  fm.Constraints,
				Files:        files,
				VerifyLevels: fm.VerifyLevels,
			})}
			continue
		}
		// 普通共享记忆槽：stale 不再丢弃——照常注入 body 并前置行号漂移警告
		//（常量值/签名类结论仍可直接采信，见 sharedStaleWarning）。
		if stale {
			body = sharedStaleWarning + body
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

// hasFreshSpec 校验 parentID 的 spec 是否存在且新鲜（Spec.Goal 非空 + 至少一条
// Acceptance + files mtime 一致）。供 callSubAgentTool.Execute 在 SpecEnforcementEnabled
// 开启时调用，缺失则拒绝派发。domain 非空时校验该领域专属 spec（TODO #65 多 key 化），
// 缺失则回退遗留单键；domain 空只校验单键 + 唯一 keyed spec 候选回退
// （reuse 派发省略 domain 的场景，2026-08-26：复用路径 spec key 不受 domain 名约束）。
// 返回 (通过, 原因文案)：失败时区分 missing（未写）/ invalid（内容不合法）/ stale（文件已变更），
// stale 精确列出失配文件路径，让 LLM 定向修正（重写被改文件或剔除无关文件）而非盲猜重写整个 spec。
// 通过且原因非空 = 唯一候选回退附警告（key/domain 错配放行，模型应对齐）。
// TODO #74 诊断精确化：missing 时列出该 parent 全部现存 spec key（key/domain 错配一眼可见）；
// 该 parent 下仅存在一个 keyed spec 时做唯一候选回退（WriteSpec key 自由命名与派发 domain
// 不匹配的真因兜底——实证 2026-08-25：key=fruit-game-spec vs domain=fruit-game 双双被拒）。
func (d *Dispatcher) hasFreshSpec(ctx context.Context, parentID, domain string) (bool, string) {
	if d.sharedMem == nil {
		return false, "spec missing: 共享记忆未启用"
	}
	ok, reason := d.checkSpecKey(ctx, specKeyFor(parentID, domain))
	if ok {
		return ok, reason
	}
	if strings.TrimSpace(domain) == "" {
		// reuse 派发省略 domain：主键（=遗留单键）已由上面查过，唯一 keyed spec
		// 候选回退对称放行--复用派发刚写的 spec 用任意 key 都能命中，消除复用路径摩擦。
		if strings.Contains(reason, "missing") {
			if keys := d.specKeysOfParent(ctx, parentID); len(keys) == 1 {
				if altOK, _ := d.checkSpecKey(ctx, keys[0]); altOK {
					return true, fmt.Sprintf("spec 按唯一候选放行（key=%q）--reuse 派发可在 call_sub_agent 的 domain 参数填该 key 对应 domain 以消除歧义", keys[0])
				}
			}
		}
		return ok, reason
	}
	// 领域专属 spec 缺失 → 回退遗留单键（老流程兼容）；仅当单键存在且新鲜才放行。
	if strings.Contains(reason, "missing") {
		if legacyOK, _ := d.checkSpecKey(ctx, parentID+":"+specSlotName); legacyOK {
			return true, ""
		}
		// 唯一候选回退（TODO #74）：该 parent 下仅存在一个 keyed spec（spec:<key>）时，
		// 按该唯一候选校验并在通过时附警告——自由命名的 key 与 domain 错配不再双双报错。
		if keys := d.specKeysOfParent(ctx, parentID); len(keys) == 1 {
			if altOK, _ := d.checkSpecKey(ctx, keys[0]); altOK {
				return true, fmt.Sprintf("spec key %q 与 domain %q 不一致，已按唯一候选放行——后续 WriteSpec 的 key 请与 call_sub_agent 的 domain 对齐", keys[0], strings.TrimSpace(domain))
			}
		}
	}
	return ok, reason
}

// specKeysOfParent 返回该 parent 下全部现存 spec 键（含遗留单键与各领域专属键），
// 排除墓碑键（已失效，对诊断无意义）。供 hasFreshSpec 唯一候选回退与 missing 诊断列 key。
func (d *Dispatcher) specKeysOfParent(ctx context.Context, parentID string) []string {
	if d.sharedMem == nil {
		return nil
	}
	prefix := parentID + ":" + specSlotName
	var out []string
	for _, k := range d.sharedMem.Keys(ctx) {
		if k != prefix && !strings.HasPrefix(k, prefix+":") {
			continue
		}
		val, err := d.sharedMem.Get(ctx, k)
		if err != nil || strings.TrimSpace(val) == "" {
			continue
		}
		if strings.HasPrefix(val, tool.SpecTombstonePrefix) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// checkSpecKey 校验单个 spec 键的新鲜度与内容合法性（hasFreshSpec 内部）。
// TODO #74：missing 文案列已有 key 清单（错配诊断）；墓碑值（Layer 2 失效）报
// "已失效（文件变更）"而非"未找到"——"被失效"与"从未写"语义不同。
func (d *Dispatcher) checkSpecKey(ctx context.Context, key string) (bool, string) {
	val, err := d.sharedMem.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		return false, d.specMissingMessage(ctx, key)
	}
	// 墓碑（TODO #74）：文件变更触发的失效删除留痕，区分于从未写。
	if strings.HasPrefix(strings.TrimSpace(val), tool.SpecTombstonePrefix) {
		return false, fmt.Sprintf("spec invalidated: %s（spec 因涉及文件被修改而失效，请用 WriteSpec 重写）", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(val), tool.SpecTombstonePrefix)))
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

// specMissingMessage 生成 missing 诊断文案（TODO #74）：列出该 parent 全部现存
// spec key——key/domain 错配（WriteSpec key 自由命名 vs dispatcher 按 domain 查键）
// 时错误信息直指真因，meta 零试错轮次。
func (d *Dispatcher) specMissingMessage(ctx context.Context, key string) string {
	msg := "spec missing: 未找到 WriteSpec 写入的任务规范"
	parentID := key
	if i := strings.Index(key, ":"); i > 0 {
		parentID = key[:i]
	}
	keys := d.specKeysOfParent(ctx, parentID)
	if len(keys) == 0 {
		return msg
	}
	// 剥 parentID 前缀只展示 slot 段（spec / spec:<key>），紧凑可读。
	short := make([]string, 0, len(keys))
	for _, k := range keys {
		short = append(short, strings.TrimPrefix(k, parentID+":"))
	}
	return msg + fmt.Sprintf("。当前已写入的 spec key: [%s]——若你的 key 与 call_sub_agent 的 domain 参数不一致请先对齐（key 必须与 domain 一致），或用已有 key 对应的 domain 派发", strings.Join(short, ", "))
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
	if len(s.VerifyLevels) > 0 {
		b.WriteString("验收层级: ")
		b.WriteString(strings.Join(s.VerifyLevels, "/"))
		b.WriteByte('\n')
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

// subAgentWorkDirFor 返回子 Agent 系统提示词用的工作目录（终审修复）：
// 优先取 ctx 注入的每会话工作目录（派发来路 runCtx 已注入会话 workDir），
// 取不到再回落 subAgentWorkDir（工具注册表默认目录）。
func (d *Dispatcher) subAgentWorkDirFor(ctx context.Context) string {
	if wd := tool.WorkDirFromContext(ctx); wd != "" {
		return wd
	}
	return d.subAgentWorkDir()
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
// 提取策略（2026-09-16 收紧）：factExtractor 已注入时只认 LLM 提取结果——
// 提取成功但零事实（模型判定无可复用信息）**不沉淀**；提取失败也不回退原文
//（原文多为本次状态快照，是存量噪声的主要来源），仅记日志。未注入提取器时
// 才回退原始 result.Text 落库（测试/未接线场景的向后兼容）。
func (d *Dispatcher) saveBlockMemory(ctx context.Context, subAgentID, roleID, parentID, taskDomain, goal, result, outcome string, filesModified []string, history []agent.ReactMessage) {
	if d.saver == nil || !d.writeEnabled {
		return
	}
	// 触发门（2026-09-16 用户定向"只沉淀改动的关键逻辑与信息"）：纯检查/调研/问答
	// 任务（无文件改动且全程只调只读类工具）不留沉淀——存量 1067 行里相当比例是
	// 验收/状态类快照，对后续任务召回是噪声。
	if !hasSubstantiveChange(history, filesModified) {
		log.Printf("[subagent] skip block memory (no substantive change): sub=%s role=%s outcome=%s", subAgentID, roleID, outcome)
		return
	}
	content := strings.TrimSpace(result)
	if content == "" {
		return
	}
	if d.factExtractor != nil {
		facts, err := d.factExtractor.Extract(ctx, content, goal, roleID)
		if err != nil {
			log.Printf("[subagent] extract facts failed, skip sediment: sub=%s err=%v", subAgentID, err)
			return
		}
		if len(facts) == 0 {
			log.Printf("[subagent] no reusable facts extracted, skip sediment: sub=%s role=%s", subAgentID, roleID)
			return
		}
		d.saveFacts(ctx, subAgentID, roleID, parentID, taskDomain, goal, facts, outcome, filesModified)
		return
	}
	d.saveRawBlockMemory(ctx, subAgentID, roleID, parentID, taskDomain, goal, content, outcome, filesModified)
}

// blockMemoryNoChangeTools 判定"本次任务无实际改动"的只读/无产出工具集：
// history 里全部工具调用都落在此集合内且无 WriteFile/EditFile 轨迹 → 视为纯检查/
// 调研/问答任务，不沉淀。未列出的工具（含插件、MCP、未知工具）一律视为有产出——
// 门只拦"确定什么都没改"的任务，宁多沉淀可疑项也不放过真实改动。
var blockMemoryNoChangeTools = map[string]bool{
	"ReadFile": true, "ListDir": true, "SearchInFiles": true, "ReadMedia": true,
	"ReadSharedMemory": true, "search_knowledge": true,
	"GitStatus": true, "GitLog": true, "GitDiff": true, "GitBlame": true,
	"list_skills": true, "load_skill": true, "tool_catalog": true,
	"ask_user": true, "send_message": true,
	"write_plan": true, "submit_plan": true, "review_plan": true,
	"list_models": true, "set_agent_model": true, "pause_agent": true, "resume_agent": true,
	"cancel_agent": true, "escalate_gear": true,
}

// hasSubstantiveChange 判断子 Agent 本次任务是否有实际改动（块记忆沉淀的触发门）。
func hasSubstantiveChange(history []agent.ReactMessage, filesModified []string) bool {
	if len(filesModified) > 0 {
		return true
	}
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !blockMemoryNoChangeTools[tc.Name] {
				return true
			}
		}
	}
	return false
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
	// 领域档案增量维护（TODO #17 T25）：files_modified 并进档案清单；
	// 成功 outcome 才用本次结论接管档案摘要（失败/部分不值得刷新）。
	summary := ""
	if outcome == "success" {
		summary = truncateRunes(strings.TrimSpace(content), 300)
	}
	d.bumpDomainProfile(ctx, taskDomain, filesModified, summary)
}

// saveFacts 把提取出的事实逐条落库，每条单独向量化以提升召回精度。
// 失败仅记日志，不影响其他事实或派发主流程。
func (d *Dispatcher) saveFacts(ctx context.Context, subAgentID, roleID, parentID, taskDomain, goal string, facts []string, outcome string, filesModified []string) {
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	sid := tool.SessionIDFromContext(ctx)
	// 领域档案增量维护（TODO #17 T25）：整批事实写一次旁路（不随循环重复）。
	summary := ""
	if outcome == "success" && len(facts) > 0 {
		summary = truncateRunes(strings.Join(facts, "；"), 300)
	}
	d.bumpDomainProfile(ctx, taskDomain, filesModified, summary)
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
		if rs, err := bb.Query(ctx, sid, parentID, taskDomain, query, blockMemoryRecallTopK, ""); err != nil {
			// 降级显式化（TODO #46）：召回失败静默跳过，但留痕供排查熔断/超时。
			log.Printf("[subagent] blackboard scope recall failed (skipped): parent=%s domain=%s err=%v", parentID, taskDomain, err)
		} else if len(rs) > 0 {
			recs = rs
		}
	}
	// 回退：语义召回（旧数据无 parent_id/task_domain / mock 未实现 BlackboardSearcher / scope 0 命中）。
	if len(recs) == 0 {
		rs, err := d.searcher.SearchBlockMemoryByGoal(ctx, sid, query, blockMemoryRecallTopK)
		if err != nil {
			log.Printf("[subagent] block-memory recall failed (skipped): session=%s err=%v", sid, err)
			rs = nil
		} else if len(rs) == 0 {
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

// recMtimesMatch 变更屏障（TODO #62 时序串行化）：契约检查前比对 capture 时刻的
// files mtime 快照与当前磁盘状态，全部一致返回 true。任何文件被兄弟返工落地改写
// 即 false（跳过该份契约检查，避免对着旧状态误报）。空快照视为一致。
func recMtimesMatch(files map[string]int64) bool {
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
	// 任务台账终态登记（先于邮箱 nil 检查：无邮箱配置时台账仍须可用）。
	// notify 是所有完成/失败/墙钟收口/被杀回传的唯一咽喉，此处一处全覆盖。
	d.ledger.RecordTerminal(sessionIDFromAgentID(parentID), parentID, subAgentID, summary, filesModified)
	// 聚合模式拦截（TODO 第七项⑤）：map_sub_agents 的项完成时记入聚合器，
	// 不逐项直发父邮箱（N 个子 Agent 完成汇成一条消息，防邮箱淹没）。
	if e, ok := d.aggByAgent.Load(subAgentID); ok {
		d.aggByAgent.Delete(subAgentID)
		entry := e.(*mapAggEntry)
		entry.once.Do(func() {
			entry.agg.record(entry.idx, entry.item, summary, true)
		})
		return
	}
	// 若未配置邮箱，直接返回，避免 nil 指针 panic。
	if d.mailbox == nil {
		return
	}
	// mailbox 收口（TODO 第七项④）：超阈值回传全文落盘，邮箱只留摘要头 + 全文路径。
	// 防 4K+ 大回传整段灌进父上下文（父 MetaAgent context 最贵）。
	body := d.returnBodyFor(subAgentID, summary)
	// 构造并发送消息：发件人为子 Agent，收件人为父 Agent，主题为子 Agent 完成提示，正文为摘要。
	// 死信错误（父已销毁）仅记日志：notify 是 best-effort 通知，不阻塞失败主流程。
	if _, err := d.mailbox.Send(&mailbox.Message{
		From:          subAgentID,
		To:            parentID,
		Type:          mailbox.MsgInfo,
		Subject:       "子 Agent 完成: " + subAgentID,
		Body:          body,
		FilesModified: filesModified,
	}); err != nil {
		log.Printf("[subagent] notify dead-letter: to=%s from=%s err=%v", parentID, subAgentID, err)
	}
}

// mailboxReturnDumpRunes 超过该 rune 数的回传全文落盘、邮箱只留摘要。
const mailboxReturnDumpRunes = 4000

// mailboxReturnDigestRunes 落盘时邮箱保留的摘要头 rune 数。
const mailboxReturnDigestRunes = 1500

// runeLen 返回字符串 rune 数。
func runeLen(s string) int { return len([]rune(s)) }

// dumpReturnToDisk 把超限回传全文写入 <workDir>/.bma/returns/<agentID>-<unix>.md，
// 返回绝对路径。workDir 取工具注册表默认目录（notify 无 ctx，会话级目录不可得；
// .bma 是项目级目录，默认 workDir 下语义一致）。
func (d *Dispatcher) dumpReturnToDisk(subAgentID, summary string) (string, error) {
	dir := filepath.Join(d.subAgentWorkDir(), ".bma", "returns")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.md", filepath.Base(subAgentID), time.Now().Unix()))
	if err := os.WriteFile(path, []byte(summary), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// TaskLedgerBrief 渲染会话任务台账（供 MetaAgent 上下文注入，bootstrap 经
// ReactService.SetTaskLedgerProvider 以 method value 接线本方法）。
// 台账为空（新会话尚未派发任何任务）返回空串，注入层跳过不产生任何上下文变化。
func (d *Dispatcher) TaskLedgerBrief(sessionID string) string {
	if d.ledger == nil || sessionID == "" {
		return ""
	}
	var tv ledgerTreeView
	if d.treeFn != nil {
		if t := d.treeFn(sessionID); t != nil {
			tv = t
		}
	}
	return d.ledger.Render(sessionID, tv)
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
