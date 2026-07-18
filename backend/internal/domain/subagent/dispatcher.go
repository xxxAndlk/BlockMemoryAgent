package subagent

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于控制子 Agent 的生命周期与超时
	"encoding/json" // encoding/json 用于序列化 tool.Result
	"fmt"           // fmt 用于格式化子 Agent ID 与错误信息
	"strings"       // strings 用于从 Agent ID 中提取角色 ID
	"sync"          // sync 提供 sync.Map 存储运行中的子 Agent
	"sync/atomic"   // sync/atomic 提供原子递增序列号
	"time"          // time 用于设置子 Agent 独立超时

	"github.com/blockmemory/agent/backend/internal/agent"       // agent 包提供 ReActAgent、MemoryPipeline、ModelProvider 等类型
	"github.com/blockmemory/agent/backend/internal/domain/role" // role 包提供角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool" // tool 包提供工具注册表与 Result 类型
	"github.com/blockmemory/agent/backend/internal/mailbox"     // mailbox 包用于子 Agent 向父 Agent 发送完成通知
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

	// timeout 是子 Agent 独立执行的最大时长；<=0 表示不限制。默认 30 分钟。
	timeout time.Duration
	// loopCfg 是子 Agent ReAct 主循环的运行时配置（轮数/重试/历史滑窗等）。
	loopCfg agent.LoopConfig
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

// RegisterCallTool 将 call_sub_agent 工具安装到传入的工具注册表中。
// 工具被注册到父 Agent 与子 Agent 共同使用的 registry 上，
// 因此子 Agent 还能继续生成自己的子 Agent，形成任意深度的递归调用。
func (d *Dispatcher) RegisterCallTool(r *tool.Registry) {
	// 注册 callSubAgentTool 实例，工具内部持有当前 Dispatcher 以便执行时调用。
	r.Register(&callSubAgentTool{dispatcher: d})
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

	// 生成全局唯一的子 Agent ID，格式为 "父ID/角色ID-序号"。
	subAgentID := fmt.Sprintf("%s/%s-%d", parentID, roleID, d.seq.Add(1))

	// 异步启动子 Agent，并立即返回子 Agent ID 作为句柄。
	// 使用 context.Background() 创建独立于父 ctx 的上下文：
	//   - 父会话取消不会波及子 Agent，避免长任务结果丢失。
	//   - timeout>0 时才叠加超时；defer cancel 确保 goroutine 退出时释放上下文资源。
	subAgentCtx := context.Background()
	cancel := context.CancelFunc(func() {})
	if d.timeout > 0 {
		subAgentCtx, cancel = context.WithTimeout(subAgentCtx, d.timeout)
	}
	go func() {
		defer cancel()
		d.runSubAgent(subAgentCtx, parentID, subAgentID, *roleDef, task)
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
func (d *Dispatcher) runSubAgent(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, task string) {
	// 根据角色 ID 获取模型提供者，失败则通知父 Agent 并结束。
	provider, err := d.models.GetBladesProvider(ctx, roleDef.ID)
	if err != nil {
		d.notify(parentID, subAgentID, fmt.Sprintf("sub-agent failed: get model: %v", err))
		return
	}

	// 若调度器未配置记忆管道，则使用空实现 NopMemoryPipeline，避免后续调用出现 nil 指针。
	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}

	// 创建子 Agent 实例：
	//   - 使用 subAgentID 作为 Agent 唯一标识。
	//   - roleDef 提供角色配置。
	//   - provider 提供模型调用能力。
	//   - ToolRegistryAdapter 将 domain/tool 注册表适配为 agent 层可用的工具注册表。
	// 随后通过 WithMailbox、WithMemory 与 WithLoopConfig 注入邮箱、记忆管道与循环配置。
	sub := agent.NewReActAgent(subAgentID, roleDef, provider, agent.NewToolRegistryAdapter(d.tools)).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopCfg)

	// 将子 Agent 记录到 running 映射，便于外部查询运行状态。
	d.running.Store(subAgentID, sub)
	// defer 确保无论运行成功或失败，子 Agent 结束时都从 running 中移除，防止内存泄漏。
	defer d.running.Delete(subAgentID)

	// 驱动子 Agent 执行具体任务。
	result, err := sub.Run(ctx, task)
	if err != nil {
		// 失败/超时时尽量回传已达成的部分进度（最后一条 assistant 输出），
		// 避免长时间执行后父 Agent 拿不到任何信息。
		partial := agent.LastAssistantText(result.History)
		if partial != "" {
			partial = truncateRunes(partial, 500)
		}
		if ctx.Err() == context.DeadlineExceeded {
			d.notify(parentID, subAgentID, fmt.Sprintf("子 Agent 执行超时（已运行 %v），已被终止。%s", d.timeout, partialSuffix(partial)))
			return
		}
		d.notify(parentID, subAgentID, fmt.Sprintf("sub-agent failed: %v%s", err, partialSuffix(partial)))
		return
	}

	// 轮数上限：子 Agent 暂停而非失败，同样回传部分进度并说明原因。
	if result.LimitReached {
		partial := truncateRunes(agent.LastAssistantText(result.History), 500)
		d.notify(parentID, subAgentID, fmt.Sprintf("子 Agent 已达最大轮数上限并暂停。%s", partialSuffix(partial)))
		return
	}

	// 将任务结果以 task_goal_summary 事件写入记忆管道，便于后续检索与复盘。
	_ = mem.Write(subAgentID, agent.MemoryEvent{
		Type:    "task_goal_summary",
		AgentID: subAgentID,
		Role:    roleDef.ID,
		Content: result.Text,
	})

	// 向父 Agent 邮箱发送任务完成通知，Body 为子 Agent 产出的总结文本。
	d.notify(parentID, subAgentID, result.Text)
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
// 顶层 Agent 直接使用角色 ID 作为标识；子 Agent 的标识格式为 "parent/role-n"。
func roleIDFromAgentID(agentID string) string {
	// 查找最后一个 '/'，若存在则取最后一段 "role-n"。
	if idx := strings.LastIndex(agentID, "/"); idx >= 0 {
		seg := agentID[idx+1:]
		// 在 "role-n" 中查找最后一个 '-'，以去掉序号部分，仅保留 role ID。
		if dash := strings.LastIndex(seg, "-"); dash >= 0 {
			return seg[:dash]
		}
		return seg
	}
	// 顶层 Agent 没有 '/'，直接返回整个 agentID 作为角色 ID。
	return agentID
}

// MarshalResult 将 tool.Result 序列化为 JSON 字符串，用于 blades 工具响应。
func MarshalResult(r *tool.Result) string {
	// 忽略序列化错误：tool.Result 结构由可控字段组成，通常不会序列化失败。
	b, _ := json.Marshal(r)
	return string(b)
}
