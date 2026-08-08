package subagent

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于控制子 Agent 的生命周期与超时
	"encoding/json" // encoding/json 解析 SharedEntry 做 stat 校验
	"errors"        // errors 提供哨兵错误 errLimitReached 与 errors.Is 判定
	"fmt"           // fmt 用于格式化子 Agent ID 与错误信息
	"log"           // log 用于记录块记忆写入失败等不影响主流程的错误
	"os"            // os 用于 stat 文件 mtime 校验（Layer 3 缓存一致性）
	"strings"       // strings 用于从 Agent ID 中提取角色 ID
	"sync"          // sync 提供 sync.Map 存储运行中的子 Agent
	"sync/atomic"   // sync/atomic 提供原子递增序列号
	"time"          // time 用于设置子 Agent 独立超时
	"unicode/utf8"  // unicode/utf8 用于 RuneCountInString 统计 task 字符数

	"github.com/blockmemory/agent/backend/internal/agent"               // agent 包提供 ReActAgent、MemoryPipeline、ModelProvider 等类型
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // orchestrator 提供 Agent 树元数据层
	"github.com/blockmemory/agent/backend/internal/domain/role"         // role 包提供角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool"         // tool 包提供工具注册表与 Result 类型
	"github.com/blockmemory/agent/backend/internal/domain/verifyloop"   // verifyloop 提供 Orchestrator/Verifier/Fixer/Reporter 状态机,供 verify_and_fix 工具复用
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
)

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
	// loopCfgByRole 按角色返回 ReAct 主循环配置:不同角色 token 预算分级
	// (domain 50K 暂停可恢复 / 叶子助手 20K 部分回灌 / meta 0 不限制)。
	// 为 nil 时用 agent.NopLoopConfig 兜底(测试场景)。
	loopCfgByRole func(string) agent.LoopConfig
	// searcher 可选的块记忆检索器；为 nil 时跳过拆分任务的块记忆召回。
	searcher BlockMemorySearcher
	// saver 可选的块记忆写入器；为 nil 或 writeEnabled 为 false 时跳过子 Agent 结果沉淀。
	saver BlockMemorySaver
	// writeEnabled 块记忆写入开关，由配置（agent.block_memory_write_enabled）注入。
	writeEnabled bool

	// orchs 按 code_role 索引的验证闭环编排器集合，由 RegisterVerifyTool 注入。
	// 非空时子 Agent 成功完成且其角色命中 orchs 键（如 code_assistant/domain），
	// 在 notify 父 Agent 前同步驱动 verifyloop 状态机（自测->修正->上级统一测试），
	// 验证结论并入回灌摘要——验证由编排层原生驱动，不依赖 LLM 自觉调 verify_and_fix。
	// 编排器内部经 ExecuteChild->runSubAgentOnce 派发验证/修正 Agent，绕开 runSubAgent
	// 包装器，不会递归触发本自动验证。为 nil/空时跳过自动验证（不影响显式工具调用）。
	orchs map[string]*verifyloop.Orchestrator

	// log 是会话级日志器，用于记录子 Agent LLM I/O（完整 prompt/response）到 session_logs。
	// 为 nil 时子 Agent 不写 LLM I/O 日志，不影响派发主流程。
	log *logger.Logger

	// liveFn 是子 Agent 实时事件转发器：把子 Agent 的 LiveEvent（token 用量/流式增量/工具事件）
	// 按 sessionID 路由回所属会话的 service.handleLiveEvent，使子 Agent token 也计入会话累计。
	// 为 nil 时子 Agent 不推送实时事件（不影响主流程）。
	liveFn func(sessionID string, ev agent.LiveEvent)
	// persona 可选的人格注入器（soul.Loader 实现该接口）；为 nil 时子 Agent 不注入人格前缀。
	// 与 MetaAgent 共享同一用户级人格，由 bootstrap 注入 runtime.Soul。
	persona agent.PersonaInjector

	// treeFn 按 sessionID 取得权威 Agent 树（lazy init）。
	// 派发前 Register 节点 + SetCancel 绑定 cancel func，完成时 Finish。
	// 为 nil 时关闭树跟踪（测试场景），不影响派发主流程。
	treeFn func(sessionID string) *orchestrator.Tree

	// msgStore 持久化 Paused DomainAgent 的完整 ReAct 消息历史。
	// DomainAgent 触达 token 上限时 SaveMessages 落库,resume 时 LoadMessages 重建上下文。
	// 为 nil 时跳过持久化(测试场景:domain 到限仍返 errPaused 但 history 不存,无法 resume)。
	msgStore agent.MessagesStore

	// factExtractor 从子 Agent 输出中提取关键事实，替代原始 result.Text 直接落库。
	// 为 nil 时回退到原始文本保存（测试场景或未配置时）；bootstrap 在启用块记忆写入时注入。
	// 提取失败（LLM 出错或返回空）自动回退原始保存，保证不丢结果。
	factExtractor FactExtractor

	// heartbeatTimeout 子 Agent 心跳超时：叶子 Agent 超过该时长无活动（generateOnce/工具派发）
	// 判定假死（LLM 流式挂起/工具 hang），巡检 goroutine 主动 cancel + notify 父 + trackChildDone，
	// 比等满 sub_agent_timeout（默认 60min）早暴露。<=0 关闭巡检（测试场景默认关闭）。
	heartbeatTimeout time.Duration
	// activity 存叶子子 Agent 最后活动时间戳（unix nano），键 subAgentID -> *atomic.Int64。
	// 仅叶子 Agent 注入（DomainAgent/MetaAgent 有 wait loop 不注入，避免误杀合法等待）。
	activity sync.Map
	// subMeta 存子 Agent 的 cancel/parentID/sessionID/doneOnce，供巡检卡死时主动 cancel + 兜底递减。
	// doneOnce 保证 patrol 与 goroutine 任一方 trackChildDone 仅触发一次，防双递减。
	subMeta sync.Map // subAgentID -> *subAgentMeta
	// patrolOnce 保证巡检 goroutine 只启动一次；patrolStop 关闭后巡检退出（测试用 ClosePatrol）。
	patrolOnce sync.Once
	patrolStop chan struct{}
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
type subAgentMeta struct {
	cancel    context.CancelFunc
	parentID  string
	sessionID string
	doneOnce  sync.Once
}

// WithHeartbeatTimeout 配置子 Agent 心跳超时；<=0 关闭巡检（测试场景默认关闭）。
// bootstrap 从 config.SubAgentHeartbeatTimeoutMin 注入（默认 5min）。
func (d *Dispatcher) WithHeartbeatTimeout(t time.Duration) *Dispatcher {
	d.heartbeatTimeout = t
	return d
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
func (d *Dispatcher) scanStuck() {
	if d.heartbeatTimeout <= 0 {
		return
	}
	threshold := time.Now().Add(-d.heartbeatTimeout).UnixNano()
	d.activity.Range(func(k, v any) bool {
		act := v.(*atomic.Int64)
		if act.Load() > threshold {
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
	log.Printf("[subagent] HEARTBEAT KILL: sub=%s parent=%s idle>%s - cancel+notify",
		subAgentID, meta.parentID, d.heartbeatTimeout)
	meta.cancel()
	meta.doneOnce.Do(func() { d.trackChildDone(meta.parentID) })
	d.notify(meta.parentID, subAgentID,
		fmt.Sprintf("子 Agent %s 超过 %s 无活动，判定假死已主动取消。请检查任务或重派。", subAgentID, d.heartbeatTimeout), nil)
	if d.treeFn != nil && meta.sessionID != "" {
		if t := d.treeFn(meta.sessionID); t != nil {
			t.Finish(subAgentID, "心跳超时疑似卡死", errors.New("heartbeat timeout"))
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

// findPendingDomainSibling 在 Agent 树快照中查找同一父 Agent 下仍在执行/暂停的
// 同领域（domain 相同）domain 子 Agent，命中返回其子 Agent ID，无则返回空串。
// domain 为空时不去重（LLM 漏填 domain 的场景无法可靠判重，放行）。
// 供 call_sub_agent 重复派发去重使用。
func (d *Dispatcher) findPendingDomainSibling(ctx context.Context, parentID, domain string) string {
	if d.treeFn == nil || parentID == "" {
		return ""
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	t := d.treeFn(sid)
	if t == nil {
		return ""
	}
	wantDomain := strings.TrimSpace(domain)
	for _, n := range t.Snapshot() {
		if n.ParentID != parentID || n.Role != "domain" {
			continue
		}
		if n.Status != orchestrator.StatusRunning && n.Status != orchestrator.StatusPaused {
			continue
		}
		if wantDomain != "" && strings.TrimSpace(n.Domain) == wantDomain {
			return n.ID
		}
	}
	return ""
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
		registry: registry,
		models:   models,
		tools:    tools,
		mailbox:  mailbox,
		memory:   memory,
		timeout:  30 * time.Minute, // 默认 30 分钟，可用 WithTimeout 覆盖；<=0 表示不限制
	}
}

// WithTimeout 配置子 Agent 独立执行的最大时长；<=0 表示不限制（仅防挂起的保底由调用方负责）。
func (d *Dispatcher) WithTimeout(t time.Duration) *Dispatcher {
	d.timeout = t
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
// 传 nil 关闭子 Agent 实时事件推送（默认关闭）。
func (d *Dispatcher) WithLiveEvents(fn func(sessionID string, ev agent.LiveEvent)) *Dispatcher {
	d.liveFn = fn
	return d
}

// WithPersonaInjector 注入人格注入器（soul.Loader），使子 Agent 系统提示词头部带人格前缀，
// 与 MetaAgent 共享用户级人格。传 nil 关闭人格注入（默认关闭）。
func (d *Dispatcher) WithPersonaInjector(p agent.PersonaInjector) *Dispatcher {
	d.persona = p
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

// RegisterCallTool 将 call_sub_agent 工具安装到传入的工具注册表中。
// 工具被注册到父 Agent 与子 Agent 共同使用的 registry 上，
// 因此子 Agent 还能继续生成自己的子 Agent，形成任意深度的递归调用。
func (d *Dispatcher) RegisterCallTool(r *tool.Registry) {
	// 注册 callSubAgentTool 实例，工具内部持有当前 Dispatcher 以便执行时调用。
	r.Register(&callSubAgentTool{dispatcher: d})
}

// RegisterMessagingTool 将 send_message 工具安装到传入的工具注册表中。
// 该工具支持任意 Agent 向另一个 Agent 实例的邮箱投递消息（请求/通知），
// 是多 Agent 协作验证闭环中"被询问方回复"与"状态同步"的基础原语。
// 目标 Agent 必须处于运行中（running map）或实例池（pool）内，否则消息会落入
// 其收件箱等待，但若目标已销毁则消息会被 Purge 一并清除。
func (d *Dispatcher) RegisterMessagingTool(r *tool.Registry) {
	r.Register(&sendMessageTool{dispatcher: d})
}

// RegisterVerifyTool 将 verify_and_fix 工具安装到工具注册表,并把按 code_role 索引的
// verifyloop.Orchestrator 集合传入工具实例。工具内部由 MetaAgent/DomainAgent 显式调用,
// 不再依赖 OnSubAgentDone 钩子自动触发(步骤 5:双控制流合并,verifyloop 折叠进 ReAct)。
// 同一集合并存到 d.orchs：子 Agent 完成路径（runSubAgent）按角色命中自动驱动验证闭环，
// 补齐"产出->验证->修正->上级统一测试"的原生触发（显式工具保留用于复检）。
// orchestrators 为空时仍注册工具但 Execute 返回"未配置验证角色对"错误。
func (d *Dispatcher) RegisterVerifyTool(r *tool.Registry, orchestrators map[string]*verifyloop.Orchestrator) {
	d.orchs = orchestrators
	r.Register(&verifyAndFixTool{dispatcher: d, orchs: orchestrators})
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
	return "向另一个 Agent 实例的邮箱投递一条消息（请求或通知），立即返回。" +
		"用于多 Agent 协作验证闭环：例如代码 Agent 完成后可向测试 Agent 发送验证请求，" +
		"测试 Agent 在下一轮 ReAct 迭代中 Drain 收件箱即可看到该消息并据此回复。" +
		"参数 to_agent_id 为目标 Agent 实例 ID（即 call_sub_agent 返回的 sub_agent_id，或父 Agent ID）；" +
		"subject 为一行摘要；body 为详情正文（可空）。"
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

	// message_type 可选：默认 request（期望回复）；显式 "info" 时为单向通知。
	msgType := mailbox.MsgRequest
	if mt, _ := args["message_type"].(string); mt == "info" {
		msgType = mailbox.MsgInfo
	} else if mt == "reply" {
		msgType = mailbox.MsgReply
	}

	// thread_id 可选：同一问答链上的消息共享 ThreadID，便于多轮验证闭环聚合。
	threadID, _ := args["thread_id"].(string)

	id := d.mailbox.Send(&mailbox.Message{
		From:     fromID,
		To:       toID,
		Type:     msgType,
		Subject:  subject,
		Body:     body,
		ReplyTo:  fromID,
		ThreadID: threadID,
	})

	return &tool.Result{
		Tool:    "send_message",
		Success: true,
		Output:  id,
	}
}

// verifyAndFixTool 实现 verify_and_fix 工具:MetaAgent/DomainAgent 显式调起验证闭环。
// 折叠进 ReAct 作工具调用,而非独立编排器自动触发(步骤 5)。内部复用 verifyloop.Orchestrator
// 状态机(SelfTest -> Fix -> UnifiedTest 往返),Reporter 把结果投递父 Agent 邮箱。
type verifyAndFixTool struct {
	dispatcher *Dispatcher
	orchs      map[string]*verifyloop.Orchestrator
}

// Name 返回工具名称。
func (t *verifyAndFixTool) Name() string { return "verify_and_fix" }

// Aliases 返回工具别名列表,当前无别名。
func (t *verifyAndFixTool) Aliases() []string { return nil }

// Description 返回 LLM 可见描述。
func (t *verifyAndFixTool) Description() string {
	entries := []string{}
	for k := range t.orchs {
		entries = append(entries, k)
	}
	pairs := "无"
	if len(entries) > 0 {
		pairs = strings.Join(entries, ", ")
	}
	return "显式触发验证闭环:对已完成的产出做自测 + 修正 + 上级统一测试往返(最多 max_rounds 轮)。" +
		"仅 MetaAgent/DomainAgent 可调用。MetaAgent 派发的复杂任务完成后,显式调用本工具决定何时验证," +
		"替代旧 OnSubAgentDone 钩子自动触发(双控制流合并)。\n\n" +
		"参数 task 为原始任务文本(供验证 Agent 知道验什么);produced 为待验证的当前产出文本;" +
		"code_role 可选,选该产出对应的角色(如 code_assistant/domain),默认用配置的第一对。" +
		"通过即返回 passed=true + 最终产出;未通过返回 passed=false + 失败原因,Reporter 同时投递结果到邮箱。\n\n" +
		"已配置验证角色对(code_role): " + pairs + "。"
}

// Execute 执行 verify_and_fix 工具调用。
func (t *verifyAndFixTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "verify_and_fix", Error: "missing caller agent context"}
	}
	task, _ := args["task"].(string)
	produced, _ := args["produced"].(string)
	if produced == "" {
		return &tool.Result{Tool: "verify_and_fix", Error: "produced is required"}
	}
	codeRole, _ := args["code_role"].(string)
	if codeRole == "" {
		// 默认取第一对(遍历 map 顺序不保证,但配置通常单对)。
		for k := range t.orchs {
			codeRole = k
			break
		}
	}
	if codeRole == "" {
		return &tool.Result{Tool: "verify_and_fix", Error: "no verify pair configured: 未配置验证角色对(self_test_enabled 关闭或 verification_role_pairs 为空)"}
	}
	o, ok := t.orchs[codeRole]
	if !ok {
		return &tool.Result{Tool: "verify_and_fix", Error: fmt.Sprintf("no orchestrator for code_role=%s", codeRole)}
	}
	// ProducerID 用 caller 自身:旧钩子路径传 subAgentID,工具路径无独立产出方 ID,复用 parentID。
	// Reporter 投递结果到 parentID 邮箱,供调用方在下一轮 ReAct 迭代 Drain 收件箱读取。
	req := verifyloop.Request{
		ParentID:    parentID,
		ProducerID:  parentID,
		InitialTask: task,
		Produced:    produced,
	}
	result := o.Run(ctx, req)
	out := fmt.Sprintf("passed=%v rounds=%d", result.Passed, result.Rounds)
	if !result.Passed {
		out += "\nfail_reason: " + result.FailReason
	}
	out += "\n\n【最终产出】\n" + result.FinalProduced
	return &tool.Result{
		Tool:    "verify_and_fix",
		Success: result.Passed,
		Output:  out,
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
	entries := []string{"domain（默认派发入口：复杂任务/不确定范围走这里，由 DomainAgent 读文件/联网/拆到单函数级再派助手或自执行）"}
	for _, fr := range t.dispatcher.registry.CallableFixedRoles() {
		entries = append(entries, fmt.Sprintf("%s（叶子执行者：%s；仅在任务已单函数级、单文件、领域明确时直派）", fr.ID, fr.Description))
	}
	return "将子任务派发给指定角色的子 Agent 异步执行。调用立即返回 sub_agent_id；" +
		"子 Agent 完成后，其结果摘要会以 [mailbox from <sub_agent_id>] 消息送达，请在后续轮次中阅读并整合。\n" +
		"task 必须自包含 <= 2000 字（按 rune 计数，含中文字符）：背景、目标、相关文件路径、前置结论与验收标准--子 Agent 看不到当前对话历史。" +
		"规格原文走 WriteSharedMemory，不塞进 task。超长 task 将被拒绝，错误提示\"task too long\"。\n\n" +
		"【前置依赖】派发前必须先调 WriteSpec(goal, acceptance, constraints, files) 写入任务规范，否则返回错误\"spec missing or stale\"。" +
		"WriteSpec 与 WriteSharedMemory 是不同工具：WriteSharedMemory 写自由 KV 供子 Agent 读，" +
		"WriteSpec 写固定 slot \"spec\" 供 dispatcher 校验并注入子 Agent 任务体前缀。两者不可互相替代。\n\n" +
		"【路由规则】\n" +
		"1. 默认走 domain：多文件/多函数/多步骤/不确定范围 -> role_id=\"domain\"，由 DomainAgent 拆分后再派助手。\n" +
		"2. 直派固定助手：仅当任务已单函数级、单文件、领域明确（如\"修改 X 函数签名\"、\"补一个测试\"）时直派对应助手。\n" +
		"3. 不确定走哪条？走 domain。domain 可自执行单点改动，不会无谓下拆。\n\n" +
		"【domain 字段】role_id=\"domain\" 时填领域分类简称（如 金融/认证/UI/数据库/配置），" +
		"用于子 Agent 展示名（\"金融领域Agent\"）。固定助手忽略此字段，用其角色名。\n" +
		"【responsibility 字段】role_id=\"domain\" 时必填：该领域 Agent 的职责边界（<= 200 字），" +
		"写明负责哪些文件/模块、不碰哪些。会注入子 Agent 系统提示词，长跑不丢。\n\n" +
		"可调用的 role_id：" + strings.Join(entries, "；") + "。"
}

// Execute 执行 call_sub_agent 工具调用。
// 参数 args 由大模型提供，包含 role_id 与 task；返回值 *tool.Result 表示调用结果。
func (t *callSubAgentTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	// 取出工具持有的调度器引用，后续操作都通过它完成。
	d := t.dispatcher

	// 从 args 中提取 role_id 与 task，类型断言失败时得到空字符串。
	roleID, _ := args["role_id"].(string)
	task, _ := args["task"].(string)
	// domain 可选：仅 role_id="domain" 时用于子 Agent 展示名（如"金融领域Agent"）。
	domain, _ := args["domain"].(string)
	// responsibility 可选：仅 role_id="domain" 时注入子 Agent 系统提示词，钉住职责边界。
	responsibility, _ := args["responsibility"].(string)

	// 校验必要参数：role_id 与 task 均不能为空。
	if roleID == "" || task == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "role_id and task are required"}
	}

	// role_id="domain" 时 responsibility 必填：dispatcher 把它注入子 Agent 系统提示词头部，
	// 防止长 ReAct 循环中 task 被历史压缩后领域身份丢失（实证：领域 Agent 越界实现他域文件）。
	// LLM 经常省略该字段，导致 DomainAgent 拿到的是通用 prompt 无职责边界——此处硬拒绝强制回填。
	if roleID == "domain" && strings.TrimSpace(responsibility) == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "responsibility is required when role_id=domain: 填该领域 Agent 的职责边界（<=200 字，负责哪些文件/模块、不碰哪些），会注入子 Agent 系统提示词"}
	}

	// task 长度上限：强制 MetaAgent 把规格写入 WriteSharedMemory，task 只写目标+验收标准。
	// 原 500 runes 实证过紧：塔防类任务的自然派发文本 ~1200-1500 runes，每轮必触发
	// "task too long" 拒绝-重写循环（单次运行最多 4 次拒绝，白烧 1-2 分钟路由轮次）。
	// 放宽到 2000 runes：容纳"背景+目标+文件清单+验收"的完整自包含描述，
	// 仍拦截 3500+ runes 的全量规格转贴（事故日志：3521/2315 runes）。
	const maxTaskRunes = 2000
	if n := utf8.RuneCountInString(task); n > maxTaskRunes {
		return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf(
			"task too long: %d runes (max %d). 把规格/原文写入 WriteSharedMemory，task 只写目标+验收标准（2000 字内）",
			n, maxTaskRunes)}
	}

	// 从当前上下文获取父 Agent ID，子 Agent 需要知道是谁调用了它。
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "missing parent agent context"}
	}

	// Spec 强制：SpecEnforcementEnabled 开启时，call_sub_agent 前必须先 WriteSpec。
	// 校验 parentID:spec 存在、SharedEntry 新鲜（verifyFileMtimes）、Spec.Goal 非空且至少一条 Acceptance。
	// 缺失则拒绝派发，逼派发方先写结构化规范，实现"规范先行"语义。
	// 校验失败不区分 stale/missing：stale（文件被改过）等价于 spec 过期，同样要求重写。
	if d.specEnforcementEnabled {
		if !d.hasFreshSpec(ctx, parentID) {
			return &tool.Result{Tool: "call_sub_agent", Error: "spec missing or stale: 先调 WriteSpec(goal, acceptance, constraints, files) 写任务规范，再 call_sub_agent"}
		}
	}

	// 从角色注册表获取目标角色定义，若角色不存在则拒绝调用。
	roleDef := d.registry.Get(roleID)
	if roleDef == nil {
		return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf("unknown role: %s", roleID)}
	}

	// 校验调用权限：只有被允许的角色关系才能发起子 Agent 调用。
	if !d.registry.CanCall(roleIDFromAgentID(parentID), roleID) {
		return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf("role %s cannot be called by %s", roleID, parentID)}
	}

	// 重复派发去重：同一父 Agent 已有同领域（domain 相同）的子 Agent 在执行/暂停中时拒绝。
	// 实证：MetaAgent 未等 mailbox 回传即重复派发同一任务（渲染引擎×3、游戏逻辑×2），
	// 多个子 Agent 并发写同一批文件互相覆盖、接口漂移。Agent 树是权威状态，直接查快照，
	// 不另维护计数（杜绝清理遗漏）。拒绝发生在限额计数之前，不烧派发配额。
	if roleID == "domain" {
		if dup := d.findPendingDomainSibling(ctx, parentID, domain); dup != "" {
			return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf(
				"duplicate dispatch: 同领域子 Agent %s 正在执行中（domain=%s）。请等待其 [mailbox from %s] 回传结果后再做下一步；如需补充或修正需求，等其完成后再派发",
				dup, domain, dup)}
		}
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
			return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf("dispatch total limit reached for session %s (max %d). 派发总数已耗尽，请直接整合已有结果答复用户", sessionID, d.maxTotalDispatches)}
		}
	}

	// 生成全局唯一的子 Agent ID，格式为 "父ID/角色ID-序号"。
	subAgentID := fmt.Sprintf("%s/%s-%d", parentID, roleID, d.seq.Add(1))

	// 在派发前递增父 Agent 的未决子 Agent 计数，供终结保护消费。
	d.trackChildStart(parentID)

	// 异步启动子 Agent，并立即返回子 Agent ID 作为句柄。
	// 使用 context.Background() 创建独立于父 ctx 的上下文：
	//   - 父会话取消不会波及子 Agent，避免长任务结果丢失。
	//   - timeout>0 时才叠加超时；defer cancel 确保 goroutine 退出时释放上下文资源。
	//   - 继承父 ctx 的 sessionID：子 Agent 工具事件经 handleToolEvent 写入会话日志，
	//     否则事件 SessionID 为空被静默丢弃（参见 service_react.handleToolEvent 的 isRunning 分支）。
	subAgentCtx := context.Background()
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	cancel := context.CancelFunc(func() {})
	if d.timeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, d.timeout)
	}
	taskBrief := truncateRunes(strings.ReplaceAll(strings.TrimSpace(task), "\n", " "), 100)
	log.Printf("[subagent] dispatch: parent=%s sub=%s role=%s task=%q", parentID, subAgentID, roleID, taskBrief)
	started := time.Now()
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
	// 心跳检活元数据：subMeta 存 cancel/parentID/sessionID/doneOnce 供巡检卡死时兜底；
	// activity 仅叶子 Agent 存（DomainAgent/MetaAgent 有 wait loop 不存，避免误杀合法等待）。
	isLeaf := roleDef.ID != "domain" && roleDef.ID != "meta"
	meta := &subAgentMeta{cancel: cancel, parentID: parentID, sessionID: tool.SessionIDFromContext(ctx)}
	d.subMeta.Store(subAgentID, meta)
	if isLeaf {
		act := new(atomic.Int64)
		act.Store(time.Now().UnixNano())
		d.activity.Store(subAgentID, act)
	}
	d.ensurePatrol()
	go func() {
		defer cancel()
		defer d.subMeta.Delete(subAgentID)
		defer d.activity.Delete(subAgentID)
		// paused=true 时(domain 触达 token 上限)不递减父未决计数:父 PendingChildren 保持 >0,
		// 由 MetaAgent PausedChildChecker 检测后主动暂停会话,等用户"继续"恢复。
		// doneOnce 保证与心跳巡检竞争时 trackChildDone 仅触发一次，防双递减。
		paused := d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, task, domain, responsibility, started)
		if !paused {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()

	// 返回成功结果，Output 为子 Agent ID，父 Agent 可用该 ID 查询或接收后续通知。
	return &tool.Result{
		Tool:    "call_sub_agent",
		Success: true,
		Output:  subAgentID,
	}
}

// runSubAgent 为指定角色创建 ReActAgent，驱动其运行，
// 并在完成后将最终结果推送到父 Agent 的邮箱。
// 这是 call_sub_agent 工具的异步执行路径：runSubAgentOnce 纯执行 + notify + 钩子。
// started 为派发起始时间，用于计算耗时并写入完成/失败日志。
// domain 为领域分类简称（仅 role_id="domain" 时有效，用于子 Agent 展示名）。
// responsibility 为职责边界描述，注入 DomainAgent 系统提示词。
// 返回 paused=true 表示 DomainAgent 触达 token 上限进入 Paused(已存 history + tree.Pause),
// 调用方不应 trackChildDone(保持父未决计数 >0 触发 MetaAgent 暂停)。
func (d *Dispatcher) runSubAgent(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility string, started time.Time) bool {
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task, domain, responsibility)
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
		partial := ""
		if result.History != nil {
			partial = truncateRunes(agent.LastAssistantText(result.History), 500)
		}
		log.Printf("[subagent] FAIL: sub=%s role=%s duration=%s err=%v partial=%q", subAgentID, roleDef.ID, duration, err, truncateRunes(partial, 200))
		d.treeFinish(ctx, subAgentID, partial, err)
		d.notify(parentID, subAgentID, formatSubAgentFailure(err, result, d.timeout, partial), files)
		return false
	}

	// 成功：先过验证闭环（该角色配置了验证对时），再把带验证结论的摘要通知父 Agent。
	log.Printf("[subagent] DONE: sub=%s role=%s duration=%s result_len=%d", subAgentID, roleDef.ID, duration, len(result.Text))
	// 本 Agent 的 ReAct 循环已结束，activity 心跳不再更新；autoVerify 可能同步跑数分钟
	// 验证/修正轮（独立子 Agent，有自己的生命周期），不摘除会被 patrol 误判假死 cancel 掉
	// （实证：VERIFY FAIL reason=round 1 cancelled: context canceled，HEARTBEAT KILL idle>5m）。
	// 摘除后该子 Agent 仅剩 d.timeout（默认 30min）兜底， goroutine defer 的重复 Delete 幂等无害。
	d.activity.Delete(subAgentID)
	summary := d.autoVerify(ctx, parentID, subAgentID, roleDef.ID, task, result.Text)
	d.treeFinish(ctx, subAgentID, summary, nil)
	d.notify(parentID, subAgentID, result.Text, files)
	return false
}

// autoVerify 在子 Agent 成功完成后同步驱动验证闭环（该角色命中 d.orchs 验证对时）。
// 未配置验证对（orchs 为空或角色未命中）时原样返回产出，零开销。
// 通过/未通过结论以【验证闭环:...】前缀并入回灌摘要，父 Agent（domain/meta）在 mailbox
// 摘要中直接看到验证结果；未通过时摘要含失败原因，由父 Agent 决定后续（重派/降级交付）。
// 修正轮产生的最终产出（FinalProduced）替代原始产出回灌，保证父 Agent 拿到的是修复后版本。
// 验证/修正子 Agent 经 ExecuteChild->runSubAgentOnce 同步派发，不回本包装器，无递归。
func (d *Dispatcher) autoVerify(ctx context.Context, parentID, subAgentID, roleID, task, produced string) string {
	o, ok := d.orchs[roleID]
	if !ok || o == nil {
		return produced
	}
	vres := o.Run(ctx, verifyloop.Request{
		ParentID:    parentID,
		ProducerID:  subAgentID,
		InitialTask: task,
		Produced:    produced,
	})
	if vres.Passed {
		log.Printf("[subagent] VERIFY PASS: sub=%s role=%s rounds=%d", subAgentID, roleID, vres.Rounds)
		return fmt.Sprintf("【验证闭环:通过】rounds=%d（自测+上级统一测试均通过）\n\n%s", vres.Rounds, vres.FinalProduced)
	}
	log.Printf("[subagent] VERIFY FAIL: sub=%s role=%s rounds=%d reason=%s", subAgentID, roleID, vres.Rounds, truncateRunes(vres.FailReason, 200))
	return fmt.Sprintf("【验证闭环:未通过】rounds=%d\n【失败原因】%s\n\n【最后产出】\n%s", vres.Rounds, vres.FailReason, vres.FinalProduced)
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

// runSubAgentOnce 纯执行路径：创建子 Agent、注入块记忆、驱动 Run、沉淀块记忆，
// 返回子 Agent 实例 + 完整结果 + 错误。不 notify、不触发钩子、不进入实例池。
// 供异步 runSubAgent 包装器与同步 ExecuteChild（编排器）共用。
//
// 错误语义：
//   - 获取 provider 失败、Run 返回 error、LimitReached 均返回非 nil err；
//   - 成功时 err == nil，result.Text 为最终答复。
func (d *Dispatcher) runSubAgentOnce(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task, domain, responsibility string) (*agent.ReActAgent, agent.ReactResult, error) {
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
		// 职责槽注入：responsibility 非空时在通用领域 prompt 前加身份头。
		// task 文本在长 ReAct 循环中会被历史压缩摘要掉，system prompt 不会，
		// 领域身份钉在系统提示词里防止跑偏（实证：领域 Agent 越界实现他域文件）。
		if resp := strings.TrimSpace(responsibility); resp != "" {
			domainLabel := strings.TrimSpace(domain)
			if domainLabel == "" {
				domainLabel = "综合"
			}
			header := fmt.Sprintf("你是负责【%s】领域的 DomainAgent。\n你的职责：%s\n"+
				"只实现/改写职责内的文件与模块；职责外的文件禁止创建或修改，"+
				"需要的跨领域数据从共享记忆契约或 ReadFile 读取。",
				textutil.TruncateRunes(domainLabel, 16, "…"), textutil.TruncateRunes(resp, 200, "…"))
			roleDef.SystemPrompt = header + "\n\n" + roleDef.SystemPrompt
		}
	}
	sub := agent.NewReActAgent(subAgentID, roleDef, provider, agent.NewToolRegistryAdapterWithFilter(d.tools, roleDef.Tools)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor(roleDef.ID)).
		WithWorkDir(d.subAgentWorkDir()).
		WithPersonaInjector(d.persona)
	// 注入未决子 Agent 检查器：子 Agent 也能递归派发（domain -> 叶子助手），
	// 无此检查时子 Agent 会在派发后立刻给出中间汇报式终答（不等待 mailbox），
	// 父链路上的 Agent 会把“中间状态”误当最终结果（实证：domain-1 拆两个子任务后
	// 直接 DONE，MetaAgent 把“等待 mailbox 结果”当终答，会话 completed 但产出缺失）。
	// Dispatcher 自身实现 PendingChildrenChecker（PendingChildren/WaitForAnyChild）。
	sub = sub.WithPendingChildrenChecker(d)

	// 心跳检活：注入活动上报回调（仅叶子 Agent 有 activity 条目，DomainAgent/MetaAgent 无则跳过）。
	// 回调闭包捕获 *atomic.Int64，generateOnce/工具派发时 Store 当前时间，巡检据此判假死。
	if actVal, ok := d.activity.Load(subAgentID); ok {
		act := actVal.(*atomic.Int64)
		sub = sub.WithActivityReporter(func() { act.Store(time.Now().UnixNano()) })
	}

	// 注入会话级日志器：派生 session-scoped logger，使子 Agent LLM I/O 写入同一会话的 session_logs。
	// sessionID 从 ctx 取（call_sub_agent 异步路径已 WithSessionID），agentName 用 roleDef.Name（DomainAgent 已按任务首行覆写）。
	sid := tool.SessionIDFromContext(ctx)
	if d.log != nil {
		sub = sub.WithLogger(d.log.WithSession(sid).WithAgent(roleDef.Name))
	}
	// 注入实时事件转发器：子 Agent emitLive 时按 sessionID 路由回会话 service，
	// 使子 Agent token 用量计入会话累计（TUI/Web 总和展示）。
	if d.liveFn != nil && sid != "" {
		forwarder := d.liveFn
		sub = sub.WithLiveEvents(func(ev agent.LiveEvent) { forwarder(sid, ev) })
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
	if bm := d.injectRecalledMemory(ctx, ""); bm != "" {
		prefixes = append(prefixes, bm)
		sid := tool.SessionIDFromContext(ctx)
		recs, _ := d.searcher.SearchBlockMemoryByGoal(ctx, sid, origTask, blockMemoryRecallTopK)
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

	result, err := sub.Run(ctx, task)
	if err != nil {
		return sub, result, fmt.Errorf("run: %w", err)
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
		log.Printf("[subagent] PARTIAL: sub=%s role=%s (token budget, partial returned)", subAgentID, roleDef.ID)
		return sub, result, errPartialReturn
	}

	_ = mem.Write(subAgentID, agent.MemoryEvent{
		Type:    "task_goal_summary",
		AgentID: subAgentID,
		Role:    roleDef.ID,
		Content: result.Text,
	})

	d.saveBlockMemory(ctx, subAgentID, roleDef.ID, origTask, result.Text)

	return sub, result, nil
}

// ResumePaused 恢复一个因触达 token 上限而 Paused 的 DomainAgent。
// 从 msgStore 加载其完整消息历史，用 fresh budget（usedTokens 局部变量自动重置）重建
// domain ReActAgent 续跑。不强制压缩——靠 Assemble 步频自动压缩（Pipeline 状态延续：
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
	sub := agent.NewReActAgent(pausedNodeID, *roleDef, provider, agent.NewToolRegistryAdapterWithFilter(d.tools, roleDef.Tools)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor("domain")).
		WithWorkDir(d.subAgentWorkDir()).
		WithPersonaInjector(d.persona)
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

// errLimitReached 是子 Agent 达到最大轮数的哨兵错误，供 formatSubAgentFailure 区分通知文案。
var errLimitReached = errors.New("sub-agent limit reached")

// errPaused 标记 DomainAgent 触达 token 上限进入 Paused(runSubAgentOnce 已存 history + tree.Pause)。
// runSubAgent 见此信号:不 notify 父、不 treeFinish、不 trackChildDone,父 PendingChildren 保持 >0,
// 由 MetaAgent 经 PausedChildChecker 检测后主动暂停会话,等用户"继续"恢复该 domain。
var errPaused = errors.New("sub-agent paused on token budget")

// errPartialReturn 标记叶子助手触达 token 上限,已把部分产出(LastAssistantText)塞入 result.Text。
// runSubAgent 见此信号:treeFinish Done("部分完成")+ notify 父 mailbox 部分 + trackChildDone 照常减
// (叶子是叶子,不持久化 history,父 domain 收部分后自行决定重派或接手)。
var errPartialReturn = errors.New("sub-agent partial return on token budget")

// formatSubAgentFailure 把 runSubAgentOnce 返回的错误格式化为父邮箱通知文案，
// 保留原有"超时/轮数上限/通用失败"三段语义与部分进度回传。
func formatSubAgentFailure(err error, result agent.ReactResult, timeout time.Duration, partial string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("子 Agent 执行超时（已运行 %v），已被终止。%s", timeout, partialSuffix(partial))
	}
	if errors.Is(err, errLimitReached) {
		return fmt.Sprintf("子 Agent 已达最大轮数上限并暂停。%s", partialSuffix(partial))
	}
	return fmt.Sprintf("sub-agent failed: %v%s", err, partialSuffix(partial))
}

// ExecuteChild 同步执行一个子 Agent 并返回其最终答复文本。
// 供 verify_and_fix 工具驱动"代码->测试->修正->统一测试"循环使用：
//   - 同步阻塞至子 Agent 完成，调用方直接拿到结果；
//   - 不 notify 父邮箱（工具自行决定如何反馈）；
//   - 不进入实例池服务态（一次性执行）。
//
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
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, *roleDef, task, "", "")
	if err != nil {
		return result.Text, err
	}
	return result.Text, nil
}

// specPrefixMarker 是任务规范注入任务前缀时的标记，便于子 Agent 区分"任务规范"与"当前任务"。
const specPrefixMarker = "【任务规范】\n"

// sharedPrefixMarker 是共享记忆注入任务前缀时的标记，便于子 Agent 区分"共享记忆"与"当前任务"。
const sharedPrefixMarker = "【共享记忆】\n"

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
	return strings.TrimRight(strings.Join(parts, "\n\n"), "\n")
}

// hasFreshSpec 校验 parentID:spec 是否存在且新鲜（Spec.Goal 非空 + 至少一条 Acceptance + files mtime 一致）。
// 供 callSubAgentTool.Execute 在 SpecEnforcementEnabled 开启时调用，缺失则拒绝派发。
func (d *Dispatcher) hasFreshSpec(ctx context.Context, parentID string) bool {
	if d.sharedMem == nil {
		return false
	}
	key := parentID + ":" + specSlotName
	val, err := d.sharedMem.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		return false
	}
	fm, _, ok := tool.DecodeSharedMD(val)
	if !ok {
		return false
	}
	if !verifyFileMtimes(fm.Files) {
		return false
	}
	return strings.TrimSpace(fm.Goal) != "" && len(fm.Acceptance) > 0
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

// saveBlockMemory 将子 Agent 成功完成后的结果沉淀到块记忆知识库。
// 未配置写入器、开关关闭或结果为空时跳过；写入失败仅记日志，不影响派发主流程。
//
// 提取策略：若 factExtractor 已注入，先调用 LLM 提取 1-5 条关键事实，
// 每条事实单独落 KnowledgeRecord（向量化后召回精度更高）。
// 提取失败或未注入时回退到原始 result.Text 落库（向后兼容）。
func (d *Dispatcher) saveBlockMemory(ctx context.Context, subAgentID, roleID, goal, result string) {
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
			d.saveFacts(ctx, subAgentID, roleID, goal, facts)
			return
		}
		log.Printf("[subagent] extract facts failed, fallback raw: sub=%s err=%v facts=%d", subAgentID, err, len(facts))
	}
	d.saveRawBlockMemory(ctx, subAgentID, roleID, goal, content)
}

// saveRawBlockMemory 把原始 result.Text 作为单条 KnowledgeRecord 落库。
// 内容采用"目标/角色/结果"三段式，meta 携带 goal/domain/session_id 标签，
// 便于召回侧（SearchBlockMemoryByGoal / SearchBlockMemory）按目标文本与领域匹配命中。
func (d *Dispatcher) saveRawBlockMemory(ctx context.Context, subAgentID, roleID, goal, content string) {
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	sid := tool.SessionIDFromContext(ctx)
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		Content:       fmt.Sprintf("目标:%s\n角色:%s\n结果:%s", trimmedGoal, roleID, truncateRunes(content, blockMemoryResultMaxRunes)),
		Meta: map[string]any{
			"goal":         trimmedGoal,
			"domain":       roleID,
			"session_id":   sid,
			"sub_agent_id": subAgentID,
			"source":       "sub_agent_result",
		},
		CreatedAt: time.Now(),
	}
	if err := d.saver.Save(ctx, rec); err != nil {
		log.Printf("[subagent] save block memory failed: sub_agent=%s err=%v", subAgentID, err)
	}
}

// saveFacts 把提取出的事实逐条落库，每条单独向量化以提升召回精度。
// 失败仅记日志，不影响其他事实或派发主流程。
func (d *Dispatcher) saveFacts(ctx context.Context, subAgentID, roleID, goal string, facts []string) {
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	sid := tool.SessionIDFromContext(ctx)
	for i, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		rec := &types.KnowledgeRecord{
			KnowledgeType: enums.KnowledgeTypeBlockMemory,
			Content:       fact,
			Meta: map[string]any{
				"goal":         trimmedGoal,
				"domain":       roleID,
				"session_id":   sid,
				"sub_agent_id": subAgentID,
				"source":       "fact_extraction",
				"fact_index":   i,
			},
			CreatedAt: time.Now(),
		}
		if err := d.saver.Save(ctx, rec); err != nil {
			log.Printf("[subagent] save fact failed: sub=%s idx=%d err=%v", subAgentID, i, err)
		}
	}
}

// injectRecalledMemory 按拆分出的子任务文本召回块记忆，并把命中内容拼到任务前。
// 未配置检索器、无命中或召回出错时返回原 task，保证派发主流程不受影响。
// sessionID 从 ctx 取：仅召回当前 session 写入的记录，防跨 session 污染。
//
// task 为空时返回纯前缀（不含【当前任务】标记），供调用方统一拼装；非空时按旧逻辑
// 返回完整 "前缀 + 【当前任务】 + task"（向后兼容 TestInjectRecalledMemory）。
func (d *Dispatcher) injectRecalledMemory(ctx context.Context, task string) string {
	if d.searcher == nil {
		return task
	}
	sid := tool.SessionIDFromContext(ctx)
	recs, err := d.searcher.SearchBlockMemoryByGoal(ctx, sid, task, blockMemoryRecallTopK)
	if err != nil || len(recs) == 0 {
		return task
	}
	// 拼接命中记忆：编号列出，便于大模型区分多条记忆条目。
	var sb strings.Builder
	sb.WriteString("【相关记忆】\n")
	for i, rec := range recs {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(rec.Content))
	}
	// task 非空：旧语义，返回完整拼装；task 为空：仅返回前缀，由调用方统一拼装。
	if task == "" {
		return strings.TrimRight(sb.String(), "\n")
	}
	sb.WriteString("\n【当前任务】\n")
	sb.WriteString(task)
	return sb.String()
}

// verifyFileMtimes 校验各 path 当前 mtime 与 frontmatter 中记录的是否一致。
// 任一 path stat 失败或 mtime 不匹配返回 false（视为 stale）。
// 空 Files 视为通过（无 path 需校验，可能是旧 entry 或纯结论摘要）。
func verifyFileMtimes(files map[string]int64) bool {
	for path, stamped := range files {
		fi, err := os.Stat(path)
		if err != nil {
			return false
		}
		if fi.ModTime().Unix() != stamped {
			return false
		}
	}
	return true
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
	d.mailbox.Send(&mailbox.Message{
		From:          subAgentID,
		To:            parentID,
		Type:          mailbox.MsgInfo,
		Subject:       "子 Agent 完成: " + subAgentID,
		Body:          summary,
		FilesModified: filesModified,
	})
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
