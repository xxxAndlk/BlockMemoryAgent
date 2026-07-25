package subagent

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于控制子 Agent 的生命周期与超时
	"encoding/json" // encoding/json 用于序列化 tool.Result
	"errors"        // errors 提供哨兵错误 errLimitReached 与 errors.Is 判定
	"fmt"           // fmt 用于格式化子 Agent ID 与错误信息
	"log"           // log 用于记录块记忆写入失败等不影响主流程的错误
	"strings"       // strings 用于从 Agent ID 中提取角色 ID
	"sync"          // sync 提供 sync.Map 存储运行中的子 Agent
	"sync/atomic"   // sync/atomic 提供原子递增序列号
	"time"          // time 用于设置子 Agent 独立超时

	"github.com/blockmemory/agent/backend/internal/agent"       // agent 包提供 ReActAgent、MemoryPipeline、ModelProvider 等类型
	"github.com/blockmemory/agent/backend/internal/domain/role" // role 包提供角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool" // tool 包提供工具注册表与 Result 类型
	"github.com/blockmemory/agent/backend/internal/mailbox"     // mailbox 包用于子 Agent 向父 Agent 发送完成通知
	"github.com/blockmemory/agent/backend/pkg/enums"            // enums 包提供 KnowledgeTypeBlockMemory 等枚举常量
	"github.com/blockmemory/agent/backend/pkg/types"            // types 包提供 RoleDefinition 类型
)

// 子 Agent 的上下文与父会话故意隔离（context.Background 派生）：
//   - 父会话若被用户手动取消，子 Agent 仍可在独立上下文中继续运行，避免长任务结果丢失。
//   - 超时仅用于防止无限制挂起，由 WithTimeout 配置；<=0 表示不限制。

// callSubAgentInput 定义 call_sub_agent 工具的 JSON 入参结构。
// 大模型在调用 call_sub_agent 时应提供 role_id（被调用角色）与 task（任务描述）。
type callSubAgentInput struct {
	RoleID string `json:"role_id"` // RoleID 被调用子 Agent 的角色标识。
	Task   string `json:"task"`    // Task 交给子 Agent 执行的具体任务描述。
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
	SearchBlockMemoryByGoal(ctx context.Context, goal string, topK int) ([]*types.KnowledgeRecord, error)
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

	// roundCounts 跟踪每个 (callerRole -> calleeRole) 派发对的累计次数，用于验证闭环往返上限。
	// 键为 "callerRoleID->calleeRoleID"，值为 *atomic.Int64。
	roundCounts sync.Map

	// onSubAgentDone 是子 Agent 异步成功完成时的钩子，供 verifyloop 编排器接管自测流程。
	// 为 nil 时关闭钩子；仅在 call_sub_agent 异步路径触发，ExecuteChild 同步路径不触发。
	onSubAgentDone SubAgentDoneHandler

	// kvMemory 是可选的键值对共享记忆（只读视图），供"协程 Agent 共享主 Agent 记忆"语义使用。
	// 子 Agent 派发时按 kvKey 读取共享记忆注入任务前缀，使被询问协程能看到主 Agent 的关键记忆。
	// 为 nil 时关闭共享记忆注入，不影响派发主流程。写入由主线程 Agent 直接持可写 KVMemory 完成。
	kvMemory KVMemoryReader

	// verificationMaxRounds 验证闭环往返上限：同一父 Agent 派发同一角色的次数超过该值时
	// 拒绝进一步派发，防止 code<->test 平级互问死循环。<=0 表示不限制。
	verificationMaxRounds int

	// timeout 是子 Agent 独立执行的最大时长；<=0 表示不限制。默认 30 分钟。
	timeout time.Duration
	// loopCfg 是子 Agent ReAct 主循环的运行时配置（轮数/重试/历史滑窗等）。
	loopCfg agent.LoopConfig
	// searcher 可选的块记忆检索器；为 nil 时跳过拆分任务的块记忆召回。
	searcher BlockMemorySearcher
	// saver 可选的块记忆写入器；为 nil 或 writeEnabled 为 false 时跳过子 Agent 结果沉淀。
	saver BlockMemorySaver
	// writeEnabled 块记忆写入开关，由配置（agent.block_memory_write_enabled）注入。
	writeEnabled bool
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

// WithLoopConfig 配置子 Agent ReAct 主循环的运行时参数（轮数/重试/历史滑窗等）。
func (d *Dispatcher) WithLoopConfig(c agent.LoopConfig) *Dispatcher {
	d.loopCfg = c
	return d
}

// WithBlockMemorySearcher 注入块记忆检索器，使子 Agent 启动前能按拆分任务文本召回相关块记忆。
// 传 nil 表示关闭召回（默认关闭）。
func (d *Dispatcher) WithBlockMemorySearcher(s BlockMemorySearcher) *Dispatcher {
	d.searcher = s
	return d
}

// WithVerificationMaxRounds 注入验证闭环往返上限，防止 code<->test 等平级角色
// 互相派发死循环。n<=0 表示不限制。
func (d *Dispatcher) WithVerificationMaxRounds(n int) *Dispatcher {
	d.verificationMaxRounds = n
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
	// 角色清单：内置 domain 角色 + 所有声明可被调用的固定角色。
	entries := []string{"domain（通用领域负责人，任务不属于任何专业领域时）"}
	for _, fr := range t.dispatcher.registry.CallableFixedRoles() {
		entries = append(entries, fmt.Sprintf("%s（%s）", fr.ID, fr.Description))
	}
	return "将子任务派发给指定角色的子 Agent 异步执行。调用立即返回 sub_agent_id；" +
		"子 Agent 完成后，其结果摘要会以 [mailbox from <sub_agent_id>] 消息送达，请在后续轮次中阅读并整合。\n" +
		"task 必须自包含：背景、目标、相关文件路径、前置结论与验收标准——子 Agent 看不到当前对话历史。\n" +
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

	// 校验必要参数：role_id 与 task 均不能为空。
	if roleID == "" || task == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "role_id and task are required"}
	}

	// 从当前上下文获取父 Agent ID，子 Agent 需要知道是谁调用了它。
	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		return &tool.Result{Tool: "call_sub_agent", Error: "missing parent agent context"}
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

	// 验证闭环往返上限：同一父 Agent 对同一角色的派发次数超过 verificationMaxRounds 时拒绝，
	// 防止 code<->test 平级互问死循环。<=0 表示不限制。
	callerRole := roleIDFromAgentID(parentID)
	if d.verificationMaxRounds > 0 {
		key := callerRole + "->" + roleID
		v, _ := d.roundCounts.LoadOrStore(key, new(atomic.Int64))
		count := v.(*atomic.Int64)
		if count.Add(1) > int64(d.verificationMaxRounds) {
			count.Add(-1)
			return &tool.Result{Tool: "call_sub_agent", Error: fmt.Sprintf("verification round limit reached for %s -> %s (max %d)", callerRole, roleID, d.verificationMaxRounds)}
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
	go func() {
		defer cancel()
		// 无论 runSubAgent 以何种方式结束，都递减父 Agent 的未决计数并发出完成信号，
		// 唤醒可能在 WaitForAnyChild 中等待的父 Agent。
		defer d.trackChildDone(parentID)
		d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, task, started)
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
func (d *Dispatcher) runSubAgent(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task string, started time.Time) {
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, roleDef, task)
	duration := time.Since(started)
	if err != nil {
		partial := ""
		if result.History != nil {
			partial = truncateRunes(agent.LastAssistantText(result.History), 500)
		}
		log.Printf("[subagent] FAIL: sub=%s role=%s duration=%s err=%v partial=%q", subAgentID, roleDef.ID, duration, err, truncateRunes(partial, 200))
		d.notify(parentID, subAgentID, formatSubAgentFailure(err, result, d.timeout, partial))
		return
	}

	// 成功：通知父 Agent，触发完成钩子。
	log.Printf("[subagent] DONE: sub=%s role=%s duration=%s result_len=%d", subAgentID, roleDef.ID, duration, len(result.Text))
	d.notify(parentID, subAgentID, result.Text)

	// 完成钩子：供 verifyloop 编排器接管"代码->测试->修正->统一测试"原生状态机。
	// 仅在异步 call_sub_agent 路径触发；ExecuteChild 同步路径不触发，避免编排器递归。
	if d.onSubAgentDone != nil {
		d.onSubAgentDone(parentID, subAgentID, roleDef.ID, task, result.Text)
	}
}

// runSubAgentOnce 纯执行路径：创建子 Agent、注入块记忆、驱动 Run、沉淀块记忆，
// 返回子 Agent 实例 + 完整结果 + 错误。不 notify、不触发钩子、不进入实例池。
// 供异步 runSubAgent 包装器与同步 ExecuteChild（编排器）共用。
//
// 错误语义：
//   - 获取 provider 失败、Run 返回 error、LimitReached 均返回非 nil err；
//   - 成功时 err == nil，result.Text 为最终答复。
func (d *Dispatcher) runSubAgentOnce(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task string) (*agent.ReActAgent, agent.ReactResult, error) {
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
	sub := agent.NewReActAgent(subAgentID, roleDef, provider, agent.NewToolRegistryAdapterWithFilter(d.tools, roleDef.Tools)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopCfg)

	d.running.Store(subAgentID, sub)
	defer d.running.Delete(subAgentID)
	defer func() {
		if d.mailbox != nil {
			d.mailbox.Purge(subAgentID)
		}
	}()

	// 保留原始任务文本，供块记忆沉淀时作为 goal 标签使用（避免混入召回前缀）。
	origTask := task
	// KV 共享记忆注入：按 parentID 派生键读取主 Agent 写入的关键记忆，拼到任务前。
	// 使被询问协程 Agent 能看到主 Agent 的上下文，对应"协程 Agent 共享主 Agent 记忆"语义。
	task = d.injectKVMemory(ctx, parentID, task)
	// 块记忆召回注入：以子任务文本做语义检索，命中则拼到任务前。
	task = d.injectRecalledMemory(ctx, task)

	result, err := sub.Run(ctx, task)
	if err != nil {
		return sub, result, fmt.Errorf("run: %w", err)
	}

	if result.LimitReached {
		return sub, result, errLimitReached
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

// errLimitReached 是子 Agent 达到最大轮数的哨兵错误，供 formatSubAgentFailure 区分通知文案。
var errLimitReached = errors.New("sub-agent limit reached")

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
// 供 verifyloop 编排器驱动"代码->测试->修正->统一测试"状态机使用：
//   - 同步阻塞至子 Agent 完成，调用方直接拿到结果；
//   - 不 notify 父邮箱（编排器自行决定何时通知）；
//   - 不触发 onSubAgentDone 钩子，避免编排器内部的修正轮触发递归自测；
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
	_, result, err := d.runSubAgentOnce(ctx, parentID, subAgentID, *roleDef, task)
	if err != nil {
		return result.Text, err
	}
	return result.Text, nil
}

// SubAgentDoneHandler 是子 Agent 成功完成时的回调签名。
// parentID 为派发者 Agent ID；subAgentID 为子 Agent ID；roleID 为子 Agent 角色；
// task 为原始派发任务；result 为子 Agent 最终答复文本。
type SubAgentDoneHandler func(parentID, subAgentID, roleID, task, result string)

// SetOnSubAgentDone 注入子 Agent 完成钩子，供 verifyloop 编排器接管自测流程。
// 传入 nil 清除钩子。钩子仅在异步 call_sub_agent 路径触发，ExecuteChild 同步路径不触发。
func (d *Dispatcher) SetOnSubAgentDone(h SubAgentDoneHandler) {
	d.onSubAgentDone = h
}

// KVMemoryReader 是键值对共享记忆的窄接口（仅读），由 domain/memory.InMemoryKV 等实现。
// 用窄接口避免 subagent 反向依赖 domain/memory 包（memory 是更底层包）。
// 写入由主线程 Agent 直接持 *memory.InMemoryKV（可写实例）完成，不经 Dispatcher。
type KVMemoryReader interface {
	// Get 按键读取记忆值，不存在返回空串与 nil error。
	Get(ctx context.Context, key string) (string, error)
}

// kvMemoryKeyPrefix 是共享记忆注入任务前缀时的标记，便于子 Agent 区分"共享记忆"与"当前任务"。
const kvMemoryKeyPrefix = "【共享记忆】\n"

// WithKVMemory 注入只读 KV 共享记忆视图，使子 Agent 派发时能读取主 Agent 写入的关键记忆。
// 传 nil 关闭共享记忆注入（默认）。
func (d *Dispatcher) WithKVMemory(r KVMemoryReader) *Dispatcher {
	d.kvMemory = r
	return d
}


// saveBlockMemory 将子 Agent 成功完成后的结果摘要沉淀到块记忆知识库。
// 未配置写入器、开关关闭或结果为空时跳过；写入失败仅记日志，不影响派发主流程。
// 内容与 Meta 携带 goal/domain 标签，便于召回侧（SearchBlockMemoryByGoal /
// SearchBlockMemory）按目标文本与领域匹配命中。
func (d *Dispatcher) saveBlockMemory(ctx context.Context, subAgentID, roleID, goal, result string) {
	if d.saver == nil || !d.writeEnabled {
		return
	}
	content := strings.TrimSpace(result)
	if content == "" {
		return
	}
	// goal 为注入召回前缀之前的原始拆分任务文本，截断保留前部即可满足召回匹配所需的语义信息。
	trimmedGoal := truncateRunes(strings.TrimSpace(goal), blockMemoryGoalMaxRunes)
	// 内容采用"目标/角色/结果"三段式，同时把 domain 标签写入 Meta，
	// 与 global_knowledge 表 meta->>'domain' 过滤条件对齐。
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		Content:       fmt.Sprintf("目标:%s\n角色:%s\n结果:%s", trimmedGoal, roleID, truncateRunes(content, blockMemoryResultMaxRunes)),
		Meta: map[string]any{
			"goal":         trimmedGoal,
			"domain":       roleID,
			"sub_agent_id": subAgentID,
			"source":       "sub_agent_result",
		},
		CreatedAt: time.Now(),
	}
	if err := d.saver.Save(ctx, rec); err != nil {
		log.Printf("[subagent] save block memory failed: sub_agent=%s err=%v", subAgentID, err)
	}
}

// injectRecalledMemory 按拆分出的子任务文本召回块记忆，并把命中内容拼到任务前。
// 未配置检索器、无命中或召回出错时返回原任务，保证派发主流程不受影响。
func (d *Dispatcher) injectRecalledMemory(ctx context.Context, task string) string {
	if d.searcher == nil {
		return task
	}
	recs, err := d.searcher.SearchBlockMemoryByGoal(ctx, task, blockMemoryRecallTopK)
	if err != nil || len(recs) == 0 {
		return task
	}
	// 拼接命中记忆：编号列出，便于大模型区分多条记忆条目。
	var sb strings.Builder
	sb.WriteString("【相关记忆】\n")
	for i, rec := range recs {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(rec.Content))
	}
	sb.WriteString("\n【当前任务】\n")
	sb.WriteString(task)
	return sb.String()
}

// injectKVMemory 按 parentID 派生键读取主 Agent 写入的 KV 共享记忆，拼到任务前。
// 未配置 kvMemory、键不存在或读取出错时返回原任务，保证派发主流程不受影响。
// 键格式 "parentID:shared"，主 Agent 在派发前用同键写入关键上下文。
func (d *Dispatcher) injectKVMemory(ctx context.Context, parentID, task string) string {
	if d.kvMemory == nil {
		return task
	}
	key := parentID + ":shared"
	val, err := d.kvMemory.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		return task
	}
	return kvMemoryKeyPrefix + val + "\n\n【当前任务】\n" + task
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
func (d *Dispatcher) notify(parentID, subAgentID, summary string) {
	// 若未配置邮箱，直接返回，避免 nil 指针 panic。
	if d.mailbox == nil {
		return
	}
	// 构造并发送消息：发件人为子 Agent，收件人为父 Agent，主题为子 Agent 完成提示，正文为摘要。
	d.mailbox.Send(&mailbox.Message{
		From:    subAgentID,
		To:      parentID,
		Type:    mailbox.MsgInfo,
		Subject: "子 Agent 完成: " + subAgentID,
		Body:    summary,
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

// MarshalResult 将 tool.Result 序列化为 JSON 字符串，用于 blades 工具响应。
func MarshalResult(r *tool.Result) string {
	// 忽略序列化错误：tool.Result 结构由可控字段组成，通常不会序列化失败。
	b, _ := json.Marshal(r)
	return string(b)
}
