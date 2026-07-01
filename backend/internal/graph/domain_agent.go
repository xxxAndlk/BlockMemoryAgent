package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/skill"
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
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

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
		if recalled, err := n.blockMemory.SearchBlockMemory(ctx, state.DomainGoal, 3); err == nil && recalled != "" {
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
		state.NextAction = types.ActionContinue
		n.finish(ctx, state)
		return state, nil
	}
	// 懒初始化 TaskResults
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 6. 判断是否需要拆分为子领域（当前实现已禁用，恒返回 false）
	if n.shouldSplitToSubDomains(ctx, state, tasks) {
		n.emit(ctx, "intend", fmt.Sprintf("领域较复杂（%d 个子任务），拆分为子领域并行处理", len(tasks)))
		state, err := n.handleSubDomainSplit(ctx, state, inst)
		if err == nil {
			n.finish(ctx, state)
		}
		return state, err
	}

	// 7. 过滤已完成的任务（结果已存在则跳过，支持断点续跑）
	var pendingTasks []string
	for _, task := range tasks {
		// 结果已存在则跳过，支持断点续跑
		if _, done := block.TaskResults[task]; done {
			continue
		}
		pendingTasks = append(pendingTasks, task)
	}

	// 8. 无待处理任务：汇总结果、标记完成、继续图循环
	if len(pendingTasks) == 0 {
		n.emit(ctx, "think", "所有子任务已完成，汇总结果")
		n.summarizeResults(state)                                            // 汇总写入 state.Reason
		n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)      // 标记实例完成
		block.Status = enums.BlockStatusCompleted                            // 标记块完成
		state.NextAction = types.ActionContinue                               // 交回 MetaAgent
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
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)      // 标记实例完成
	block.Status = enums.BlockStatusCompleted                            // 标记块完成
	state.NextAction = types.ActionContinue                               // 交回 MetaAgent
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

// ensureSkillSet 确保该 DomainAgent 已装配 Skill 子集。
//
// 职责：
//   - 若已装配则直接返回
//   - 特性4：先查归档存储是否有同领域历史 Agent，命中则复用其 Skill 子集
//     并权重+1、延后过期；未命中再走 LLM AssembleSet
//   - 调 Pool.AssembleSet 触发 LLM 选择 ≤8 个技能
//   - 通过 Registry.Bind 绑定到本 agent 实例 ID
//
// 参数：
//   - ctx：请求上下文
//   - inst：本实例
//   - state：图全局状态（取 DomainGoal 作为选择输入）
//
// 副作用：装配成功后向 registry 写入 SkillSet；命中归档时 BumpWeight。
//
// 设计意图：v3 §5，让每个 DomainAgent 只看到与其领域相关的技能子集，
// 避免全局技能列表污染 system prompt。特性4 在此基础上跨会话复用历史装配结果。
func (n *DomainAgentNode) ensureSkillSet(ctx context.Context, inst *types.RoleInstance, state *types.ThreeLayerState) {
	// Runtime 或 Skill 注册表缺失则跳过（退化模式）
	if n.rt == nil || n.rt.Skills == nil {
		return
	}
	// 已装配则直接返回，避免重复 LLM 调用
	if existing := n.rt.Skills.GetForAgent(n.instID); existing != nil {
		return
	}

	// 特性4：先查归档，命中则复用历史 Skill 子集
	// 检索路径：domain + DomainGoal 双条件做相似查询，topK=1 取最相关的一条
	if n.archiveStore != nil {
		if archives, err := n.archiveStore.SearchDomainArchive(ctx, inst.Domain, state.DomainGoal, 1); err == nil && len(archives) > 0 {
			arc := archives[0]
			// 用归档里保存的 Skill ID 列表重建 SkillSet；查不到任何技能则 fall-through 到 LLM 路径
			if reused := n.rt.Skills.Pool().AssembleFromIDs(n.instID, arc.Skills); reused != nil && len(reused.Skills) > 0 {
				n.rt.Skills.Bind(reused)
				// 权重 +1 且延后过期：让热点领域的归档越用越不容易被回收
				ttl := time.Duration(168) * time.Hour // 默认 7 天
				if n.rt.AgentCfg != nil && n.rt.AgentCfg.DomainArchiveTTLHours > 0 {
					ttl = time.Duration(n.rt.AgentCfg.DomainArchiveTTLHours) * time.Hour
				}
				_ = n.archiveStore.BumpDomainArchiveWeight(ctx, arc.ArchiveID, ttl)
				n.emit(ctx, "think", fmt.Sprintf("复用历史 domainAgent 归档: domain=%s weight=%d skills=%v", arc.Domain, arc.Weight, arc.Skills))
				return
			}
		}
	}

	// 取领域模型作为技能选择的 LLMClient；取不到则 llm 为 nil，AssembleSet 内部回退规则
	var llm skill.LLMClient
	if n.modelFactory != nil {
		// 尝试取领域模型
		if c, err := n.modelFactory.GetDomainModel(ctx); err == nil {
			llm = c
		}
	}
	// 装配：≤8 个技能（默认），可被 AgentCfg.SkillSetSize 覆盖（特性2）
	skillSetSize := 8
	if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.SkillSetSize > 0 {
		skillSetSize = n.rt.AgentCfg.SkillSetSize
	}
	set := n.rt.Skills.Pool().AssembleSet(ctx, llm, n.instID, inst.Domain, state.DomainGoal, skillSetSize)
	// 绑定到本 agent
	n.rt.Skills.Bind(set)
}

// dispatchAssistantsParallel 并行派发助手。
//
// 职责：为每个任务创建/匹配一个 Assistant，并发执行后收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例（作为 Assistant 的父）
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 并发安全：内部用 sync.Mutex 保护 results map；WaitGroup 等待全部完成。
//
// 注意：当前 Invoke 走串行路径，此函数保留以备并行场景使用。
func (n *DomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	results := make(map[string]string) // 结果收集
	var mu sync.Mutex                  // 保护 results 的并发写入
	var wg sync.WaitGroup              // 等待所有 goroutine 完成

	for _, task := range tasks {
		// 为任务创建/匹配 Assistant 实例与角色定义
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			// 创建失败：直接写入错误结果，不进入 goroutine
			mu.Lock()
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			mu.Unlock()
			continue
		}

		// 启动 goroutine 并行执行
		wg.Add(1)
		go func(task string, aInst *types.RoleInstance, aDef *types.RoleDefinition) {
			defer wg.Done()

			// 在 goroutine 内运行助手
			result := n.runAssistant(ctx, state, aInst, aDef, task)

			// 加锁写回结果
			mu.Lock()
			results[task] = result
			mu.Unlock()
		}(task, assistantInst, assistantDef)
	}

	// 等待所有助手完成
	wg.Wait()
	return results
}

// dispatchAssistantsSerial 串行派发助手。
//
// 职责：按顺序为每个任务创建/匹配 Assistant 并执行，收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 设计意图：子任务间常有依赖（"启动游戏"依赖"写代码"、"运行 db_check"依赖"写 db_check"），
// 并行会导致后续任务找不到前置产物而反复 ListDir/ReadFile 空转。
// 串行虽慢，但 ReAct 循环能读到前置产物，任务成功率显著提升。
func (n *DomainAgentNode) dispatchAssistantsSerial(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	// 预分配容量，避免 map 扩容
	results := make(map[string]string, len(tasks))
	for _, task := range tasks {
		// 为任务创建/匹配 Assistant
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			// 创建失败：写入错误结果
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			continue
		}
		// 串行执行：上一个完成后再跑下一个，确保依赖产物可见
		results[task] = n.runAssistant(ctx, state, assistantInst, assistantDef, task)
	}
	return results // 返回所有任务的结果
}

// createAssistantForTask 为指定任务创建助手实例。
//
// 职责：
//   - 优先用 matchFixedAssistant 匹配固定助手（按技能/关键词打分）
//   - 匹配失败则用 factory.CreateAssistant 动态创建
//   - 校验 CanCall 权限
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：父实例
//   - task：任务文本
//
// 返回：助手实例与角色定义；任一环节失败返回 (nil, nil)。
func (n *DomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	// 1. 先尝试匹配固定助手
	assistantDef := n.matchFixedAssistant(task)
	if assistantDef != nil {
		// 权限校验：本实例是否可调用该角色
		if !n.registry.CanCall(n.instID, assistantDef.ID) {
			return nil, nil // 无权限
		}
		// 创建固定助手实例
		assistantInst, err := n.registry.CreateInstance(assistantDef.ID, state.SessionID, inst.Domain, n.instID)
		if err != nil {
			// 创建失败：打印日志
			fmt.Printf("[DomainAgent] create fixed assistant %s failed: %v\n", assistantDef.ID, err)
			return nil, nil
		}
		return assistantInst, assistantDef // 返回固定助手
	}

	// 2. 动态创建助手（由 LLM 推断角色定义）
	assistantInst, err := n.factory.CreateAssistant(ctx, state.SessionID, task, n.instID, inst.RoleDefID)
	if err != nil {
		// 创建失败：打印日志
		fmt.Printf("[DomainAgent] create dynamic assistant for %q failed: %v\n", task, err)
		return nil, nil
	}
	// 推送 Agent 创建调试事件，便于 UI 观察动态角色生成
	n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 Assistant: %s (任务: %s)", assistantInst.ID, task),
		fmt.Sprintf("instID=%s roleDefID=%s parentID=%s", assistantInst.ID, assistantInst.RoleDefID, n.instID))

	// 3. 取出动态创建的角色定义
	assistantDef = n.registry.GetRoleDef(assistantInst.RoleDefID)
	if assistantDef == nil {
		return nil, nil // 角色定义缺失
	}
	// 权限校验
	if !n.registry.CanCall(n.instID, assistantDef.ID) {
		return nil, nil // 无权限
	}
	return assistantInst, assistantDef // 返回动态助手
}

// runAssistant 在当前goroutine中运行助手执行任务。
//
// 职责：标记活跃 → 带 3 次指数退避重试执行 → 标记完成。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：助手实例
//   - def：助手角色定义
//   - task：任务文本
//
// 返回：结果文本；失败则返回 [ERROR] 前缀的描述。
func (n *DomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) string {
	// 标记助手活跃
	n.registry.UpdateInstanceStatus(inst.ID, types.RoleStatusActive)

	var result string // 任务结果
	var err error     // 执行错误

	// 重试次数与初始退避：默认 3 次 / 100ms，可被 AgentCfg 覆盖（特性2）
	retryCount := 3
	retryDelay := 100 * time.Millisecond
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.RetryCount > 0 {
			retryCount = n.rt.AgentCfg.RetryCount
		}
		if n.rt.AgentCfg.RetryBackoffMs > 0 {
			retryDelay = time.Duration(n.rt.AgentCfg.RetryBackoffMs) * time.Millisecond
		}
	}
	err = retryWithBackoff(retryCount, retryDelay, func() error {
		// 在闭包内执行助手任务
		result, err = n.executeAssistantTask(ctx, def, task, state)
		return err
	})

	// 标记助手完成
	n.registry.UpdateInstanceStatus(inst.ID, types.RoleStatusDone)

	// 失败则返回错误信息
	if err != nil {
		return fmt.Sprintf("[ERROR] 助手[%s]执行失败: %v", def.Name, err)
	}
	return result // 返回成功结果
}

// executeAssistantTask 执行助手任务（LLM+工具循环 或 回退模拟）。
//
// 职责：
//   - 优先走 executeAssistantWithTools（blades.Agent + function-calling ReAct 循环）
//   - 工具循环无输出则退化到单次 llm.Generate
//   - 无 modelFactory 则回退到模拟结果
//   - 写文件类任务失败时返回 error 触发上层重试
//
// 参数：
//   - ctx：请求上下文
//   - def：助手角色定义
//   - task：任务文本
//   - state：图全局状态
//
// 返回：结果文本与 error。
//
// 副作用：可能调用工具/写文件/运行命令（由工具循环内部决定）。
func (n *DomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (string, error) {
	// 1. 优先使用 LLM + 工具执行
	if n.modelFactory != nil {
		// 新建工具执行器并注入回调
		executor := NewToolExecutor("")
		if n.toolCallback != nil {
			executor.SetCallback(n.toolCallback)
		}

		// 取出本 DomainAgent 装配的 Skill 列表（v3 §5），作为 system prompt 的可见技能段
		var skillBrief string
		if n.rt != nil && n.rt.Skills != nil {
			// 取本实例已绑定的 SkillSet
			if set := n.rt.Skills.GetForAgent(n.instID); set != nil {
				// 转为 prompt 可用的技能简介文本
				skillBrief = set.PromptList()
			}
		}

		// 调用 blades.Agent + 工具循环执行
		maxIters := 12
		if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.ToolCallMaxRounds > 0 {
			maxIters = n.rt.AgentCfg.ToolCallMaxRounds
		}
		result, _ := executeAssistantWithTools(ctx, n.modelFactory, executor, def, task, state, skillBrief, n.progress, "助手["+def.Name+"]", maxIters)
		if result != "" {
			// 完成门控：若任务要求写文件但结果含失败标记，返回 error 触发上层重试/告警
			if strings.HasPrefix(result, "[失败:") {
				return result, fmt.Errorf("助手未完成写文件任务: %s", task)
			}
			return result, nil // 成功返回
		}

		// 工具执行回退到普通LLM（单次生成，无工具）
		llm, err := n.modelFactory.GetModel(ctx, def.ID)
		if err == nil {
			// 拼 prompt：角色 system prompt + 当前任务 + 领域目标
			prompt := fmt.Sprintf("%s\n\n当前任务: %s\n领域目标: %s\n请执行任务并返回结果。",
				def.SystemPrompt, task, state.DomainGoal)
			resp, err := llm.Generate(ctx, prompt)
			if err == nil && resp != "" {
				return resp, nil // 成功返回
			}
		}
	}

	// 2. 回退到模拟结果（无 modelFactory 或上述路径都失败）
	contextInfo := ""
	if state.CurrentDomain != "" {
		// 拼接领域前缀
		contextInfo = fmt.Sprintf("[领域: %s] ", state.CurrentDomain)
	}
	// 返回模拟结果
	return fmt.Sprintf("%s助手[%s]完成任务: %s", contextInfo, def.Name, task), nil
}

// analyzeTasks 分析领域任务（优先使用LLM，回退到规则）。
//
// 职责：把领域目标拆成 2-4 个可直接用工具执行的子任务。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（取 DomainGoal 作为分析输入）
//
// 返回：子任务列表；无目标返回 nil；LLM 拆解均为思考类任务时降级为单任务。
//
// DirectExecute 模式：MetaAgent 判定为查询/搜索类简单任务时置 state.DirectExecute=true，
// 此处跳过 LLM 拆解，直接把 goal 作为单任务交给一个 Assistant，避免无谓拆分。
func (n *DomainAgentNode) analyzeTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	// 取领域目标；为空则直接返回 nil
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	// DirectExecute 模式：跳过 LLM 拆解，直接派单任务
	if state.DirectExecute {
		n.emit(ctx, "think", "DirectExecute 模式：跳过子任务拆解，直接派发单助手执行")
		return []string{goal}
	}

	// 尝试使用LLM进行任务拆解
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		n.emit(ctx, "llm", "调用 LLM 拆解子任务...")
		// 构造拆解 prompt：强调"可直接用工具执行"，禁止纯思考类子任务
		// 若本次 Invoke 检索到历史相似块记忆，作为参考段注入（特性3）
		memorySection := ""
		if n.recalledMemory != "" {
			memorySection = fmt.Sprintf("\n相关历史块记忆（参考，避免重复劳动）:\n%s\n", n.recalledMemory)
		}
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个任务分析专家。请将以下目标拆解为2-4个独立可执行的子任务。

目标: %s

%s
%s

要求:
- 每个子任务必须是一个可直接用工具执行的动作（如"用 WriteFile 写 X 文件"、"用 RunCommand 运行 Y"、"用 HTTPGet 抓取 Z"）
- 严禁出现"分析/确定/规划/设计/思考/研究/需求/方案"等纯思考类子任务，这类工作应在执行动作中一并完成
- 涉及创建文件的目标，必须有子任务明确写出文件路径与内容来源
- 子任务之间可以有依赖但应尽量并行
- 只输出子任务列表，每行一个，不要编号，不要其他内容
- 任务匹配工具，不要"为了用工具而用工具"：
  * 信息查询/搜索/新闻/行情类目标 → 用 HTTPGet 抓取公开 URL，禁止"写 Python 脚本去搜索"
  * 只有目标明确要求"写代码/生成文件/运行程序"时，才用 WriteFile / RunCommand

示例（好）:
用 HTTPGet 抓取 https://news.example.com/ai 获取最近 AI 新闻
用 WriteFile 把贪吃蛇游戏代码写到 workspace/snake.py
用 RunCommand 运行 python workspace/snake.py 验证

示例（坏，禁止）:
需求分析
设计方案
编写代码
用 WriteFile 写一个 Python 脚本去搜索新闻（应该直接用 HTTPGet）

子任务:`, goal, memorySection, fmtEnvSection()))
		if !timedOut && err == nil && resp != "" {
			// 解析响应为任务列表
			if tasks := parseTaskListFromResp(resp); len(tasks) > 0 {
				n.emit(ctx, "think", fmt.Sprintf("LLM 拆解出 %d 个子任务", len(tasks)))
				return tasks // 返回 LLM 拆解的任务
			}
			// LLM 返回的尽是思考类任务，过滤后为空 → 把整个 goal 作为单任务，
			// 让一个 assistant 用 ReAct 循环完整执行（写文件+运行+验证）。
			n.emit(ctx, "think", "LLM 拆解均为思考类任务，降级为单任务整体执行")
			return []string{goal} // 降级为单任务
		}
		// 超时或失败：推送事件并回退规则
		if timedOut {
			n.emit(ctx, "error", "任务拆解 LLM 调用超时，回退到规则")
			fmt.Printf("[DomainAgent] LLM timeout on task analysis, using rules. %s\n", n.llmTracker.StatsString())
		} else if err != nil {
			n.emitDetail(ctx, "error", "任务拆解 LLM 调用失败: "+err.Error(), "")
		}
	}

	// 规则回退
	return n.analyzeTasksByRules(goal)
}

// callLLM 统一LLM调用入口（带自适应超时 + Prompt/Token 日志）。
// caller 固定为 "DomainAgent/任务拆解"。
func (n *DomainAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	return n.callLLMAs(ctx, "DomainAgent/任务拆解", prompt)
}

// callLLMAs 以指定调用者身份执行 LLM 调用。
//
// 职责：
//   - 取领域模型
//   - 推送 prompt 调试事件（含 token 估算与摘要）
//   - 带自适应超时调用 LLM（30s 软超时 / 90s 硬超时）
//   - 推送最近一次调用的 Token 消耗
//
// 参数：
//   - ctx：请求上下文
//   - caller：调用者标识，用于事件展示
//   - prompt：发送给 LLM 的完整 prompt
//
// 返回：(响应文本, 错误, 是否超时)。
//
// 副作用：通过 emitDetail 推送 prompt 与 token_usage 事件。
func (n *DomainAgentNode) callLLMAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	// 取领域模型；失败则直接返回错误
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return "", err, false
	}

	// 推送 prompt 调试事件（含 token 估算与 500 字摘要）
	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// 带自适应超时调用 LLM：默认 30s 软超时 / 90s 硬超时，可被 AgentCfg 覆盖（特性2）
	softTimeout := 30 * time.Second
	hardTimeout := 90 * time.Second
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.LLMSoftTimeoutSec > 0 {
			softTimeout = time.Duration(n.rt.AgentCfg.LLMSoftTimeoutSec) * time.Second
		}
		if n.rt.AgentCfg.LLMHardTimeoutSec > 0 {
			hardTimeout = time.Duration(n.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
		}
	}
	resp, callErr, timedOut := n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller,
		softTimeout,
		hardTimeout,
	)

	// 推送最近一次调用的 Token 消耗（in/out/耗时）
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1] // 取最新一条记录
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	// 推送 LLM 响应摘要调试事件（500 字截断），便于排查任务拆解/子任务结果
	if resp != "" {
		n.emitDetail(ctx, "llm_response",
			fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
			model.SummarizePrompt(resp, 500))
	}

	return resp, callErr, timedOut
}

// parseTaskListFromResp 从LLM响应解析任务列表。
//
// 职责：逐行清洗响应（去前缀/编号），过滤纯思考类任务名。
//
// 参数：
//   - resp：LLM 返回的原始文本
//
// 返回：清洗后的任务列表；过滤后为空则返回 nil（调用方应回退到规则拆解）。
//
// 设计意图：纯思考类任务（需求分析/设计方案/测试验证 等）不产生可执行产物，
// 只会浪费 ReAct 轮数并触发 LLM 超时。
func parseTaskListFromResp(resp string) []string {
	// 纯思考类任务名黑名单
	thinkPatterns := []string{"需求分析", "设计方案", "设计", "分析", "规划", "思考", "研究",
		"确定", "需求", "方案", "测试验证", "验证", "总结", "review"}
	var tasks []string
	for _, line := range strings.Split(resp, "\n") {
		// 去首尾空白
		line = strings.TrimSpace(line)
		// 去除 "- " / "* " 前缀
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		// 去除行首的 "1. " / "2. " 等编号
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:] // 截掉编号前缀
		}
		// 过短行（<2 字符）视为无效
		if len(line) < 2 {
			continue
		}
		// 过滤纯思考类任务名（整行就是"需求分析"这种短词）
		isThinkOnly := false
		for _, p := range thinkPatterns {
			// 整行匹配思考类关键词
			if line == p || strings.TrimSpace(line) == p {
				isThinkOnly = true
				break
			}
		}
		if isThinkOnly {
			continue // 跳过纯思考类任务
		}
		tasks = append(tasks, line) // 收集有效任务
	}
	return tasks
}

// analyzeTasksByRules 基于规则的任务拆解（回退方案）。
//
// 职责：LLM 不可用时的回退，按目标关键词匹配预设模板。
//
// 参数：
//   - goal：领域目标
//
// 返回：子任务列表；无匹配时把整个 goal 作为单任务。
func (n *DomainAgentNode) analyzeTasksByRules(goal string) []string {
	var tasks []string

	// 按"修复/实现/优化"等关键词匹配模板
	if strings.Contains(goal, "修复") {
		tasks = append(tasks, "分析根因")       // 第1步：根因分析
		tasks = append(tasks, "定位问题代码")   // 第2步：定位代码
		tasks = append(tasks, "生成修复方案")   // 第3步：生成方案
		tasks = append(tasks, "验证修复")       // 第4步：验证
	} else if strings.Contains(goal, "实现") || strings.Contains(goal, "开发") {
		tasks = append(tasks, "需求分析")   // 第1步：需求
		tasks = append(tasks, "设计方案")   // 第2步：设计
		tasks = append(tasks, "编写代码")   // 第3步：编码
		tasks = append(tasks, "测试验证")   // 第4步：测试
	} else if strings.Contains(goal, "优化") {
		tasks = append(tasks, "性能分析")   // 第1步：性能分析
		tasks = append(tasks, "识别瓶颈")   // 第2步：识别瓶颈
		tasks = append(tasks, "实施优化")   // 第3步：实施
	} else {
		// 无匹配：把整个目标作为单任务
		tasks = append(tasks, goal)
	}

	return tasks
}

// matchFixedAssistant 匹配固定助手。
//
// 职责：遍历所有固定助手角色定义，按技能/关键词命中打分，返回最高分者。
//
// 参数：
//   - task：任务文本
//
// 返回：匹配的角色定义；最高分 < 10 返回 nil（视为无匹配，转动态创建）。
func (n *DomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
	// 任务文本转小写，做大小写不敏感匹配
	taskLower := strings.ToLower(task)
	var bestMatch *types.RoleDefinition
	bestScore := 0

	// 遍历所有助手角色定义
	for _, def := range n.registry.GetAssistantRoleDefs() {
		// 只考虑固定角色（动态角色不参与匹配）
		if def.Type != types.RoleTypeFixed {
			continue
		}
		score := 0
		// 技能命中：+10 分
		for _, skill := range def.Skills {
			// 任务文本包含技能关键词则加分
			if strings.Contains(taskLower, strings.ToLower(skill)) {
				score += 10
			}
		}
		// 关键词命中：+5 分
		for _, kw := range def.Keywords {
			// 任务文本包含角色关键词则加分
			if strings.Contains(taskLower, strings.ToLower(kw)) {
				score += 5
			}
		}
		// 更新最高分
		if score > bestScore {
			bestScore = score   // 更新最高分
			bestMatch = def     // 记录最佳匹配
		}
	}

	// 阈值 10：至少一个技能命中才视为有效匹配
	if bestScore >= 10 {
		return bestMatch // 返回最佳匹配
	}
	return nil // 无有效匹配
}

// shouldSplitToSubDomains 判断是否需要拆分为子领域。
//
// 设计意图（已禁用）：实测 SubDomain 拆分会产生 3+ 子领域，每个子领域又派 3-4 个 assistant，
// 每个 assistant 跑 8 轮 ReAct，总 LLM 调用数爆炸（单任务 500+ events），
// 5 分钟全局超时内根本跑不完，且子领域间重复执行同一任务。
// DomainAgent 直接 dispatchAssistantsParallel 并行派 assistant 即可，
// 不再走 SubDomain 层。
//
// 返回：恒 false（保留签名以备后续按需启用）。
func (n *DomainAgentNode) shouldSplitToSubDomains(ctx context.Context, state *types.ThreeLayerState, tasks []string) bool {
	return false
}

// shouldSplitWithLLM 使用LLM判断是否需要拆分子领域。
//
// 职责：调用 LLM 判断领域是否包含多个独立模块或可并行的任务组。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//   - tasks：子任务列表
//
// 返回：LLM 回答"是"/"yes"返回 true；否则 false。
//
// 注意：当前 shouldSplitToSubDomains 恒返回 false，本函数未被主路径调用，保留以备启用。
func (n *DomainAgentNode) shouldSplitWithLLM(ctx context.Context, domain string, tasks []string) bool {
	// 无模型工厂则直接返回 false
	if n.modelFactory == nil {
		return false
	}

	// 构造判断 prompt：要求只回答"是"或"否"
	prompt := fmt.Sprintf(`判断以下领域是否需要拆分为多个子领域并行处理。

领域: %s
子任务数量: %d
子任务列表:
%s

如果该领域包含多个独立模块（如前端页面的头部/列表/底部），或者子任务可以明确分为2-4个并行组，回答"是"。
否则回答"否"。
只回答"是"或"否"。`, domain, len(tasks), strings.Join(tasks, "\n"))

	// 调用 LLM（caller 为"拆分判断"）
	resp, err, _ := n.callLLMAs(ctx, "DomainAgent/拆分判断", prompt)
	if err != nil {
		return false // 调用失败：不拆分
	}

	// 响应含"是"或"yes"即视为需要拆分
	return strings.Contains(resp, "是") || strings.Contains(strings.ToLower(resp), "yes")
}

// handleSubDomainSplit 拆分子领域并调度（支持依次调度多个）。
//
// 职责：
//   - 首次进入时初始化子领域列表（inferSubDomains）
//   - 依次为每个子领域创建 SubDomainAgent 实例
//   - 通过 PushCallStack 派发任务，切换到子领域节点
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例
//
// 返回：更新后的 state；子领域全部派发完后返回 ActionContinue。
//
// 注意：当前 shouldSplitToSubDomains 恒返回 false，本函数未被主路径调用，保留以备启用。
func (n *DomainAgentNode) handleSubDomainSplit(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance) (*types.ThreeLayerState, error) {
	// 取当前块
	block := state.ActiveBlocks[state.CurrentBlockID]

	// 首次拆分：初始化子领域列表
	if block != nil && !block.SubDomainSplit {
		block.SubDomainSplit = true                       // 标记已初始化，避免重复
		subDomains := n.inferSubDomains(ctx, inst.Domain) // 推断子领域
		for _, sd := range subDomains {
			// 收集子领域名到列表
			block.SubDomainList = append(block.SubDomainList, sd.Name)
		}
	}

	// 子领域全部处理完：继续图循环
	if block == nil || block.SubDomainIndex >= len(block.SubDomainList) {
		state.NextAction = types.ActionContinue // 继续图循环
		return state, nil
	}

	// 取当前子领域名并推进游标
	subDomainName := block.SubDomainList[block.SubDomainIndex] // 取当前子领域
	block.SubDomainIndex++                                       // 推进游标

	// 创建 SubDomainAgent 实例
	subInst, err := n.factory.CreateSubDomainAgent(ctx, state.SessionID, subDomainName, state.DomainGoal, n.instID)
	if err != nil {
		// 创建失败：打印日志并继续
		fmt.Printf("[DomainAgent] create subdomain agent %s failed: %v\n", subDomainName, err)
		state.NextAction = types.ActionContinue
		return state, nil
	}

	// 构造调用请求：领域目标作为任务，附带领域/子领域/块ID等上下文
	callReq := &types.CallRequest{
		ID:       fmt.Sprintf("call_%s_%d", subInst.ID, len(state.CallStack)), // 唯一调用ID
		CallerID: n.instID,                                                     // 调用者=本 Domain
		CalleeID: subInst.ID,                                                   // 被调用者=子领域
		Task:     state.DomainGoal,                                             // 任务=领域目标
		Context: map[string]any{
			"domain":          inst.Domain,          // 父领域名
			"sub_domain":      subDomainName,        // 子领域名
			"block_id":        state.CurrentBlockID, // 所属块ID
			"domain_goal":     state.DomainGoal,     // 领域目标
			"session_summary": state.SessionSummary, // 会话摘要（跨块共享）
		},
		Priority: 5, // 默认优先级
	}

	// 入栈调用请求并切换到子领域节点
	state.PushCallStack(callReq)            // 压入调用栈
	state.NextAction = types.ActionSwitch   // 切换到子领域节点
	state.TargetRoleID = subInst.ID         // 路由目标
	return state, nil
}

// inferSubDomains 推断子领域列表（优先LLM，回退规则）。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//
// 返回：子领域列表；LLM 失败回退规则。
func (n *DomainAgentNode) inferSubDomains(ctx context.Context, domain string) []DomainInfo {
	// 有模型工厂则优先 LLM 推断
	if n.modelFactory != nil {
		if subs := n.inferSubDomainsWithLLM(ctx, domain); len(subs) > 0 {
			return subs // 返回 LLM 推断结果
		}
	}
	// 回退规则
	return n.inferSubDomainsByRules(domain)
}

// inferSubDomainsWithLLM 使用LLM推断子领域。
//
// 职责：调用 LLM 把领域拆成 2-4 个独立子领域。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//
// 返回：子领域列表；LLM 失败或返回空返回 nil。
func (n *DomainAgentNode) inferSubDomainsWithLLM(ctx context.Context, domain string) []DomainInfo {
	// 无模型工厂则返回 nil
	if n.modelFactory == nil {
		return nil
	}

	// 标识调用者
	caller := "DomainAgent/子领域推断"
	// 构造拆分 prompt：要求 2-4 个简短独立的子领域名
	prompt := fmt.Sprintf(`将以下领域拆分为2-4个独立的子领域。

领域: %s

要求:
- 每个子领域名称简短（2-6个字）
- 子领域之间尽量独立，可并行处理
- 只输出子领域名称，每行一个，不要编号，不要其他内容

子领域:`, domain)

	// 调用 LLM
	resp, err, _ := n.callLLMAs(ctx, caller, prompt)
	if err != nil || resp == "" {
		return nil
	}

	// 解析响应：逐行清洗，构造 DomainInfo
	var result []DomainInfo
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)        // 去首尾空白
		line = strings.TrimPrefix(line, "- ") // 去 "- " 前缀
		// 去除行首编号
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:] // 截掉编号前缀
		}
		// 长度 >=2 视为有效
		if len(line) >= 2 {
			// 构造 DomainInfo，目标自动生成
			result = append(result, DomainInfo{
				Name: line,
				Goal: fmt.Sprintf("处理%s相关的子任务", line),
			})
		}
	}
	return result
}

// inferSubDomainsByRules 基于规则推断子领域。
//
// 职责：LLM 不可用时的回退，按领域名关键词匹配前端组件模板。
//
// 参数：
//   - domain：领域名
//
// 返回：子领域列表；无匹配时返回单元素（领域名+"子任务"）。
func (n *DomainAgentNode) inferSubDomainsByRules(domain string) []DomainInfo {
	// 商城/页面类：拆为头部/列表/底部三个子领域
	if strings.Contains(domain, "商城") || strings.Contains(domain, "页面") {
		return []DomainInfo{
			{Name: "首页头部", Goal: "修复头部导航样式问题"},   // 头部子领域
			{Name: "商品列表", Goal: "修复商品列表布局问题"},   // 列表子领域
			{Name: "底部导航", Goal: "修复底部导航样式问题"},   // 底部子领域
		}
	}
	// 默认：单子领域
	return []DomainInfo{{Name: domain + "子任务", Goal: "执行细分任务"}} // 兜底单子领域
}

// summarizeResults 汇总助手结果。
//
// 职责：把当前块的 TaskResults 拼成简短摘要，写入 state.Reason 供上层展示。
// 同时若启用块记忆存储（特性3），把摘要归档到 pgvector，供后续相似检索。
//
// 参数：
//   - state：图全局状态（原地修改 state.Reason）
//
// 副作用：修改 state.Reason；可能写 Postgres（块记忆归档）。
func (n *DomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	// 取本实例
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理，直接返回
		return
	}

	// 取当前块并收集结果摘要
	block := state.ActiveBlocks[state.CurrentBlockID]
	var summaries []string

	// 从 TaskResults 收集
	if block != nil && block.TaskResults != nil {
		for task, result := range block.TaskResults {
			// 结果过长则截断到 100 字符
			shortResult := result
			if len(shortResult) > 100 {
				// 截断并加省略号
				shortResult = shortResult[:100] + "..."
			}
			// 拼成 "任务: 结果" 格式
			summaries = append(summaries, fmt.Sprintf("%s: %s", task, shortResult))
		}
	}

	// 有摘要则拼成一句话写入 state.Reason
	if len(summaries) > 0 {
		// 用分号连接所有摘要
		state.Reason = fmt.Sprintf("领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}

	// 特性3：把块记忆归档到 pgvector，供后续 DomainAgent 相似检索
	if n.blockMemory != nil && block != nil {
		// 优先用 state.Reason（含各任务结果摘要）；为空时退化为领域目标
		summary := state.Reason
		if summary == "" {
			summary = block.Goal
		}
		// 异步归档避免阻塞图循环；失败仅记录日志，不影响主流程
		// 注意：通过参数显式捕获 block/domain/goal/sum，避免闭包捕获迭代变量
		go func(b *types.SessionBlock, domain, goal, sum string) {
			// 独立 ctx：与会话 ctx 解耦，会话结束后归档仍能完成
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := n.blockMemory.SaveBlockMemory(bgCtx, b.SessionID, domain, goal, sum); err != nil {
				fmt.Printf("[DomainAgent] save block memory: %v\n", err)
			}
		}(block, inst.Domain, block.Goal, summary)
	}

	// 特性4：把 domainAgent 信息（领域/技能/上下文摘要）归档，跨会话可复用
	if n.archiveStore != nil && block != nil {
		// 收集当前实例绑定的 Skill ID 列表，作为下次复用的种子
		var archivedSkills []string
		if n.rt != nil && n.rt.Skills != nil {
			if set := n.rt.Skills.GetForAgent(n.instID); set != nil {
				for _, s := range set.Skills {
					archivedSkills = append(archivedSkills, s.SkillID)
				}
			}
		}
		// TTL 来自配置；默认 168h（7 天）保证热点领域归档不会过快失效
		ttl := 168 * time.Hour
		if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.DomainArchiveTTLHours > 0 {
			ttl = time.Duration(n.rt.AgentCfg.DomainArchiveTTLHours) * time.Hour
		}
		// 摘要优先取 state.Reason（含各任务结果）；为空时退化为领域目标
		summary := state.Reason
		if summary == "" {
			summary = block.Goal
		}
		// 新归档权重从 1 起步；每次被复用时 BumpDomainArchiveWeight 会自增
		rec := &DomainArchiveRecord{
			SessionID:      block.SessionID,
			Domain:         inst.Domain,
			Goal:           block.Goal,
			RoleDefID:      inst.RoleDefID,
			Skills:         archivedSkills,
			ContextSummary: summary,
			Weight:         1,
			ExpiresAt:      time.Now().Add(ttl),
			CreatedAt:      time.Now(),
		}
		// 异步落库：独立 ctx 不受会话生命周期影响；失败仅日志，主流程已结束不影响结果
		go func(r *DomainArchiveRecord) {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := n.archiveStore.SaveDomainArchive(bgCtx, r); err != nil {
				fmt.Printf("[DomainAgent] save domain archive: %v\n", err)
			}
		}(rec)
	}
}

// retryWithBackoff 指数退避重试。
//
// 职责：最多重试 maxRetries 次 fn；每次失败后 sleep delay 并翻倍 delay。
//
// 参数：
//   - maxRetries：最大重试次数（含首次执行）
//   - initialDelay：首次失败后的初始退避时长
//   - fn：待重试的函数，返回 nil 视为成功
//
// 返回：最后一次 fn 的错误；全部成功返回 nil。
//
// 并发安全：纯函数，无共享状态。
//
// 用途：Domain/SubDomain 的 runAssistant 用此包装 executeAssistantTask，
// 应对写文件/工具调用的瞬时失败。
func retryWithBackoff(maxRetries int, initialDelay time.Duration, fn func() error) error {
	var err error
	delay := initialDelay
	for i := 0; i < maxRetries; i++ {
		// 执行 fn；成功则立即返回
		if err = fn(); err == nil {
			return nil
		}
		// 非最后一次失败则退避后重试
		if i < maxRetries-1 {
			time.Sleep(delay) // 退避等待
			delay *= 2        // 指数翻倍
		}
	}
	return err // 返回最后一次错误
}
