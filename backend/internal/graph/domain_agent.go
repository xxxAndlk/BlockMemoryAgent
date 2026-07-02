package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// DomainAgentNode Layer 2: 会话块Agent / 领域上下文管理器。
//
// 职责：
//   - 接收 MetaAgent 派发的领域目标，装配领域 Skill 子集
//   - 调 analyzeTasks 拆解子任务（LLM 优先，规则回退）
//   - 串行派发 Assistant 执行子任务（含 3 次指数退避重试）
//   - 汇总结果后标记块完成，回控制权给 MetaAgent
//
// 并发安全：节点字段在构造后只读；实例状态由 registry 内部锁保护。
// dispatchAssistantsParallel 路径（当前未启用）用 sync.Mutex 保护结果 map。
type DomainAgentNode struct {
	name         string                // 节点名（固定 "DomainAgent"）
	instID       string                // 本实例ID
	registry     *RoleRegistry         // 角色注册表
	factory      *RoleFactory          // 动态角色工厂（创建 Assistant/SubDomain）
	modelFactory *model.ModelFactory   // 模型工厂，按角色获取 ChatModel
	toolCallback ToolCallback          // 工具执行结果回调（推 UI）
	llmTracker   *model.LLMCallTracker // LLM 调用追踪器（统计超时/Token）
	rt           *runtime.Runtime      // Runtime 聚合体（板/邮箱/Skill/人格/Watchdog）
	progress     ProgressCallback      // 进度回调（推思考/意图/Token）
	blockMemory  BlockMemoryStore      // 块记忆存储（特性3：向量检索归档）
	archiveStore DomainArchiveStore    // domainAgent 归档存储（特性4：跨会话复用）
	recalledMemory string              // 本次 Invoke 检索到的相似块记忆文本（注入 analyzeTasks）
	memCallback  MemoryCallbackHandler // 记忆回调处理器（驱动 Episode 写入与快照保存）
	snapshotMgr  AgentSnapshotManager  // Agent 快照管理器（启动加载/结束保存）
	snapshot     *types.AgentSnapshot  // 本次 Invoke 加载到的快照
}

// NewDomainAgentNode 创建领域Agent节点。
//
// 参数：
//   - instID：实例ID
//   - registry：角色注册表
//   - factory：角色工厂
//
// 返回：装配好的节点；modelFactory/toolCallback/runtime/progress 通过 Set* 后置注入。
func NewDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *DomainAgentNode {
	return &DomainAgentNode{
		name:       "DomainAgent",                 // 节点名固定
		instID:     instID,                         // 绑定实例
		registry:   registry,                       // 注入注册表
		factory:    factory,                        // 注入工厂
		llmTracker: model.NewLLMCallTracker(),      // 新建 LLM 调用追踪器
	}
}

// SetModelFactory 设置模型工厂（用于LLM任务分析）。
// 由图构建器在 Build 阶段注入。
func (n *DomainAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行回调。
// 工具执行后通过此回调把 ToolResult 推给 UI。
func (n *DomainAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// SetRuntime 注入 Runtime（板/邮箱/Skill）。
// 由图构建器注入；nil 时 Skill 装配与跨域协作能力退化。
func (n *DomainAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// SetProgressCallback 注入进度回调。
// 用于推送思考/意图/LLM 调用/Token 消耗等事件到 UI。
func (n *DomainAgentNode) SetProgressCallback(cb ProgressCallback) {
	n.progress = cb
}

// SetBlockMemoryStore 注入块记忆存储（特性3）。
// nil 时 DomainAgent 不做向量检索与归档，仅退化为无记忆模式。
func (n *DomainAgentNode) SetBlockMemoryStore(s BlockMemoryStore) {
	n.blockMemory = s
}

// SetArchiveStore 注入 domainAgent 归档存储（特性4）。
// nil 时不做跨会话归档与复用。
func (n *DomainAgentNode) SetArchiveStore(s DomainArchiveStore) {
	n.archiveStore = s
}

// SetMemoryCallbackHandler 注入记忆回调处理器。
// nil 时不触发 Episode 写入与快照保存。
func (n *DomainAgentNode) SetMemoryCallbackHandler(h MemoryCallbackHandler) {
	n.memCallback = h
}

// SetAgentSnapshotManager 注入 Agent 快照管理器。
// nil 时不加载/保存快照。
func (n *DomainAgentNode) SetAgentSnapshotManager(s AgentSnapshotManager) {
	n.snapshotMgr = s
}

// emit 推送进度事件。
//
// 参数：
//   - ctx：请求上下文（用于提取 SessionID）
//   - kind：事件类型（think/intend/llm/error 等）
//   - message：事件摘要
//
// 副作用：若 progress 为 nil 则无操作；否则触发回调（可能阻塞）。
func (n *DomainAgentNode) emit(ctx context.Context, kind, message string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 默认 Agent 名，若实例有领域则带上领域后缀便于 UI 区分
	agent := "DomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "DomainAgent[" + inst.Domain + "]"
	}
	// 推送事件
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message})
}

// emitDetail 推送带详情的进度事件。
// 与 emit 的区别：附带 detail 字段，用于展示 Prompt 全文/Token 明细等调试信息。
func (n *DomainAgentNode) emitDetail(ctx context.Context, kind, message, detail string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 默认 Agent 名，若实例有领域则带上领域后缀
	agent := "DomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "DomainAgent[" + inst.Domain + "]"
	}
	// 推送带 detail 的事件
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message, Detail: detail})
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口。
func (n *DomainAgentNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID。
// 用于图调度循环在路由表里查找对应的动态节点。
func (n *DomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行领域Agent逻辑。
//
// 职责：
//   - 标记实例活跃
//   - 装配领域 Skill 子集（ensureSkillSet）
//   - 调 analyzeTasks 拆解子任务
//   - 判断是否拆分子领域（当前禁用）
//   - 串行派发 Assistant 执行待处理任务
//   - 汇总结果、标记块完成、回控制权给 MetaAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改）
//
// 返回：更新后的 state；实例缺失或当前块缺失时返回错误或退化 ActionContinue。
//
// 副作用：更新实例状态；修改 state.ActiveBlocks、NextAction。
func (n *DomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 取出本实例；不存在则直接报错
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理或路由错误
		if n.memCallback != nil {
			n.memCallback.OnError(ctx, n.instID, state.SessionID, fmt.Errorf("domain agent instance %s not found", n.instID))
		}
		return nil, fmt.Errorf("domain agent instance %s not found", n.instID)
	}

	// 2. 推送启动事件并标记实例活跃
	n.emit(ctx, "think", fmt.Sprintf("DomainAgent 启动，领域目标: %s", state.DomainGoal))
	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusActive)

	// 2.5 记忆回调：广播 DomainAgent 进入 ACTIVE，并尝试加载历史快照
	if n.memCallback != nil {
		n.memCallback.OnStart(ctx, n.instID, state.SessionID)
	}
	if n.snapshotMgr != nil {
		if snap, err := n.snapshotMgr.Load(ctx, n.instID, state.SessionID); err == nil && snap != nil {
			n.snapshot = snap
			n.emit(ctx, "think", fmt.Sprintf("已加载历史快照，未决问题 %d 个", len(snap.OpenIssues)))
		}
	}

	// 3. v3 §5：为本 DomainAgent 装配领域 Skill 子集（如未装配）
	n.ensureSkillSet(ctx, inst, state)
	// 装配后把 Skill ID 列表推给 UI，便于观察可见技能
	if n.rt != nil && n.rt.Skills != nil {
		// 取本实例已绑定的 SkillSet
		if set := n.rt.Skills.GetForAgent(n.instID); set != nil && len(set.Skills) > 0 {
			// 收集所有 Skill ID 用于事件展示
			ids := make([]string, 0, len(set.Skills))
			for _, s := range set.Skills {
				ids = append(ids, s.SkillID)
			}
			// 推送装配结果到 UI
			n.emit(ctx, "think", "已装配 Skill 子集: "+strings.Join(ids, ", "))
		}
	}

	// 3.5 特性3：检索相似块记忆，注入 analyzeTasks 作为上下文
	// 用 DomainGoal 做查询，取 topK=3；命中结果在 analyzeTasks prompt 里拼成"参考段"
	// 让 LLM 知晓过往类似领域已做过的任务，避免重复劳动或漏掉关键步骤
	n.recalledMemory = ""
	if n.blockMemory != nil {
		if recalled, err := n.blockMemory.SearchBlockMemory(ctx, state.CurrentDomain, state.DomainGoal, 3); err == nil && recalled != "" {
			n.recalledMemory = recalled
			n.emit(ctx, "think", "已检索到历史相似块记忆，将作为上下文注入任务拆解")
		}
	}

	// 4. 拆解子任务（LLM 优先，规则回退）
	tasks := n.analyzeTasks(ctx, state)

	// 5. 取出当前会话块；不存在则直接继续图循环
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		// 块缺失，交回 MetaAgent 决策
		state.NextAction = enums.ActionContinue
		n.finish(ctx, state)
		return state, nil
	}
	// 懒初始化 TaskResults
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 5.5 Plan-and-Execute（TODO #1）：多任务且启用时生成结构化计划，按步骤派发
	//     仅在尚未生成计划时生成（断点续行时不重复生成）；解析失败回退为原 tasks（零回归）
	if block.Plan == nil && planEnabledFromRT(n.rt) && len(tasks) > 1 {
		block.Plan = n.generatePlan(ctx, state, tasks)
	}

	// 6. 判断是否需要拆分为子领域（自适应：仅 state.EnableSubdomain 且跨子领域边界）
	if n.shouldSplitToSubDomains(ctx, state, tasks) {
		n.emit(ctx, "intend", fmt.Sprintf("领域较复杂（%d 个子任务），拆分为子领域并行处理", len(tasks)))
		state, err := n.handleSubDomainSplit(ctx, state, inst)
		if err == nil {
			n.finish(ctx, state)
		}
		return state, err
	}

	// 7. 确定待处理任务列表：有 Plan 时用计划的未完成步骤，否则过滤原 tasks（支持断点续跑）
	var pendingTasks []string
	if block.Plan != nil {
		// Plan-and-Execute：用计划的未完成步骤目标
		pendingTasks = block.Plan.PendingGoals()
	} else {
		for _, task := range tasks {
			// 结果已存在则跳过，支持断点续跑
			if _, done := block.TaskResults[task]; done {
				continue
			}
			pendingTasks = append(pendingTasks, task)
		}
	}

	// 8. 无待处理任务：汇总结果、标记完成、继续图循环
	if len(pendingTasks) == 0 {
		n.emit(ctx, "think", "所有子任务已完成，汇总结果")
		n.summarizeResults(state)                                            // 汇总写入 state.Reason
		n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusDone)      // 标记实例完成
		block.Status = enums.BlockStatusCompleted                            // 标记块完成
		state.NextAction = enums.ActionContinue                               // 交回 MetaAgent
		n.finish(ctx, state)
		return state, nil
	}

	// 9. 串行派发助手执行待处理任务
	n.emit(ctx, "intend", fmt.Sprintf("派发 %d 个助手任务（串行）: %s", len(pendingTasks), strings.Join(pendingTasks, "; ")))
	// 串行执行：子任务间常有依赖（如"启动游戏"依赖"写代码"），并行会导致后续任务找不到文件。
	results := n.dispatchAssistantsSerial(ctx, state, inst, pendingTasks)

	// 10. 合并结果到 block.TaskResults
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}
	for task, result := range results {
		// 逐条写入块结果
		block.TaskResults[task] = result
	}

	// 11. 汇总结果、标记完成、继续图循环（交回 MetaAgent 决策下一步）
	n.summarizeResults(state)                                            // 汇总写入 state.Reason
	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusDone)      // 标记实例完成
	block.Status = enums.BlockStatusCompleted                            // 标记块完成
	state.NextAction = enums.ActionContinue                               // 交回 MetaAgent
	n.finish(ctx, state)
	return state, nil
}

// finish 在 DomainAgent 成功结束前触发记忆回调。
// 由 CallbackHandler 内部负责 Episode 写入与快照保存；此处只发起回调。
func (n *DomainAgentNode) finish(ctx context.Context, state *types.ThreeLayerState) {
	if n.memCallback != nil {
		n.memCallback.OnEnd(ctx, n.instID, state.SessionID, "DomainAgent完成", state.Reason, 0)
	}
}
