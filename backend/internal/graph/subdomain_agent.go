package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SubDomainAgentNode Layer 2.5: 子领域Agent。
//
// 职责：
//   - 接收父 DomainAgent 派发的领域目标
//   - 将目标拆解为 2-4 个子任务
//   - 并行调度 Assistant 执行子任务
//   - 汇总结果后弹出调用栈，返回父 DomainAgent
//
// 设计意图：处理领域内可并行化的独立模块（如前端页面头部/列表/底部）。
// 注：实测中本层在 DomainAgent.shouldSplitToSubDomains 已被禁用，
// 保留代码以备后续按需启用。
//
// 并发安全：节点字段在构造后只读；实例状态由 registry 内部锁保护。
type SubDomainAgentNode struct {
	name         string                // 节点名（固定 "SubDomainAgent"）
	instID       string                // 本实例ID
	registry     *RoleRegistry         // 角色注册表
	factory      *RoleFactory          // 动态角色工厂（创建 Assistant）
	modelFactory *model.ModelFactory   // 模型工厂，按角色获取 ChatModel
	toolCallback ToolCallback          // 工具执行结果回调（推 UI）
	progress     ProgressCallback      // 进度回调（推思考/意图/Token）
	llmTracker   *model.LLMCallTracker // LLM 调用追踪器（统计超时/Token）
	rt           *runtime.Runtime      // Runtime 聚合体（用于读取 AgentCfg 等动态参数）
	memCallback  MemoryCallbackHandler // 记忆回调处理器（驱动 Episode 写入与快照保存）
	snapshotMgr  AgentSnapshotManager  // Agent 快照管理器（启动加载/结束保存）
}

// SetRuntime 注入 Runtime。nil 时动态参数回退默认值。
// 特性2 装配路径：ThreeLayerGraph.injectDependencies 在装载阶段把 g.rt 同步给
// 三层节点，让 SubDomainAgent 也能读到 AgentCfg.RetryCount / ToolCallMaxRounds 等。
func (n *SubDomainAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// NewSubDomainAgentNode 创建子领域Agent节点。
//
// 参数：
//   - instID：实例ID
//   - registry：角色注册表
//   - factory：角色工厂
//
// 返回：装配好的节点；modelFactory/toolCallback/progress 通过 Set* 后置注入。
func NewSubDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *SubDomainAgentNode {
	return &SubDomainAgentNode{
		name:       "SubDomainAgent",               // 节点名固定
		instID:     instID,                          // 绑定实例
		registry:   registry,                        // 注入注册表
		factory:    factory,                         // 注入工厂
		llmTracker: model.NewLLMCallTracker(),       // 新建 LLM 调用追踪器
	}
}

// emit 推送进度事件。
//
// 参数：
//   - ctx：请求上下文（用于提取 SessionID）
//   - kind：事件类型（think/intend/llm/error 等）
//   - message：事件摘要
//
// 副作用：若 progress 为 nil 则无操作；否则触发回调（可能阻塞）。
func (n *SubDomainAgentNode) emit(ctx context.Context, kind, message string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 默认 Agent 名，若实例有领域则带上领域后缀便于 UI 区分
	agent := "SubDomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "SubDomainAgent[" + inst.Domain + "]"
	}
	// 推送事件，SessionID 从 ctx 提取
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message})
}

// emitDetail 推送带详情的进度事件。
// 与 emit 的区别：附带 detail 字段，用于展示 Prompt 全文/Token 明细等调试信息。
func (n *SubDomainAgentNode) emitDetail(ctx context.Context, kind, message, detail string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 默认 Agent 名，若实例有领域则带上领域后缀
	agent := "SubDomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "SubDomainAgent[" + inst.Domain + "]"
	}
	// 推送带 detail 的事件
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message, Detail: detail})
}

// SetModelFactory 设置模型工厂。
// 由图构建器在 Build 阶段注入，用于后续 GetDomainModel/GetModel。
func (n *SubDomainAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行回调。
// 工具执行后通过此回调把 ToolResult 推给 UI。
func (n *SubDomainAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// SetProgressCallback 注入进度回调。
// 用于推送思考/意图/LLM 调用/Token 消耗等事件到 UI。
func (n *SubDomainAgentNode) SetProgressCallback(cb ProgressCallback) {
	n.progress = cb
}

// SetMemoryCallbackHandler 注入记忆回调处理器。
// nil 时不触发 Episode 写入与快照保存。
func (n *SubDomainAgentNode) SetMemoryCallbackHandler(h MemoryCallbackHandler) {
	n.memCallback = h
}

// SetAgentSnapshotManager 注入 Agent 快照管理器。
// nil 时不加载/保存快照。
func (n *SubDomainAgentNode) SetAgentSnapshotManager(s AgentSnapshotManager) {
	n.snapshotMgr = s
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口。
func (n *SubDomainAgentNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID。
// 用于图调度循环在路由表里查找对应的动态节点。
func (n *SubDomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行子领域Agent逻辑。
//
// 职责：
//   - 标记实例活跃
//   - 调 analyzeSubTasks 拆解子任务（LLM 优先，规则回退）
//   - 过滤已完成任务后并行派发 Assistant
//   - 汇总结果并弹出调用栈，返回父 DomainAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改）
//
// 返回：更新后的 state；实例缺失或当前块缺失时返回错误或退化 ActionContinue。
//
// 副作用：更新本实例与父实例状态；修改 state.CallStack、ActiveBlocks、NextAction。
func (n *SubDomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 取出本实例；不存在则直接报错
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理或路由错误，终止本次执行
		if n.memCallback != nil {
			n.memCallback.OnError(ctx, n.instID, state.SessionID, fmt.Errorf("subdomain agent instance %s not found", n.instID))
		}
		return nil, fmt.Errorf("subdomain agent instance %s not found", n.instID)
	}

	// 2. 标记实例活跃
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// 2.5 记忆回调：广播 SubDomainAgent 进入 ACTIVE，并尝试加载历史快照
	if n.memCallback != nil {
		n.memCallback.OnStart(ctx, n.instID, state.SessionID)
	}
	if n.snapshotMgr != nil {
		if snap, err := n.snapshotMgr.Load(ctx, n.instID, state.SessionID); err == nil && snap != nil {
			n.emit(ctx, "think", fmt.Sprintf("已加载历史快照，未决问题 %d 个", len(snap.OpenIssues)))
		}
	}

	// 3. 拆解子任务（LLM 优先，失败回退规则）
	tasks := n.analyzeSubTasks(ctx, state)

	// 4. 取出当前会话块；不存在则直接继续图循环
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		// 块缺失（被并发清理或路由错误），交回 MetaAgent 决策
		state.NextAction = types.ActionContinue
		n.finish(ctx, state)
		return state, nil
	}
	// 懒初始化 TaskResults
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 5. 过滤已完成的任务（结果已存在则跳过，支持断点续跑）
	var pendingTasks []string
	for _, task := range tasks {
		// 已有结果的任务跳过，避免重复执行
		if _, done := block.TaskResults[task]; done {
			continue
		}
		pendingTasks = append(pendingTasks, task)
	}

	// 6. 有待处理任务则并行派发 Assistant
	if len(pendingTasks) > 0 {
		// 并行执行所有待处理任务
		results := n.dispatchAssistantsParallel(ctx, state, inst, pendingTasks)

		// 把结果合并回 block.TaskResults（此处 mu 实际多余，因 Invoke 串行，
		// 但保留以呼应 dispatchAssistantsParallel 内部的并发写入模式）
		var mu sync.Mutex
		mu.Lock()
		for task, result := range results {
			// 逐条写入块结果
			block.TaskResults[task] = result
		}
		mu.Unlock()
	}

	// 7. 子领域任务完成：弹出调用栈、汇总结果、标记完成
	state.PopCallStack()
	n.summarizeResults(state)
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	// 8. 路由回父 DomainAgent；无父则回 MetaAgent
	state.NextAction = types.ActionSwitch
	if inst.ParentID != "" {
		state.TargetRoleID = inst.ParentID                              // 切回父 DomainAgent
		n.registry.UpdateInstanceStatus(inst.ParentID, types.RoleStatusActive) // 恢复父实例活跃
	} else {
		state.TargetRoleID = "MetaAgent" // 无父则回 MetaAgent
	}

	n.finish(ctx, state)
	return state, nil
}

// finish 在 SubDomainAgent 成功结束前触发记忆回调。
// 由 CallbackHandler 内部负责 Episode 写入与快照保存；此处只发起回调。
func (n *SubDomainAgentNode) finish(ctx context.Context, state *types.ThreeLayerState) {
	if n.memCallback != nil {
		n.memCallback.OnEnd(ctx, n.instID, state.SessionID, "SubDomainAgent完成", state.Reason, 0)
	}
}

// dispatchAssistantsParallel 并行调度助手。
//
// 职责：为每个任务创建/匹配一个 Assistant，并发执行后收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 SubDomain 实例（作为 Assistant 的父）
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 并发安全：内部用 sync.Mutex 保护 results map；WaitGroup 等待全部完成。
func (n *SubDomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
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
// createAssistantForTask 委托公共实现：为指定任务创建或匹配助手实例。
//
// 保留 emitDetail 调试事件推送，其余逻辑走 CommonCreateAssistantForTask。
func (n *SubDomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	assistantInst, assistantDef := CommonCreateAssistantForTask(ctx, n.registry, n.factory, n.instID, inst, state, task)
	// 动态创建成功时推送 Agent 创建调试事件，便于 UI 观察动态角色生成
	if assistantInst != nil && assistantDef != nil && assistantDef.Type == types.RoleTypeDynamic {
		n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 Assistant: %s (任务: %s)", assistantInst.ID, task),
			fmt.Sprintf("instID=%s roleDefID=%s parentID=%s", assistantInst.ID, assistantInst.RoleDefID, n.instID))
	}
	return assistantInst, assistantDef
}

// runAssistant 运行助手执行任务。
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
func (n *SubDomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) string {
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
func (n *SubDomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (string, error) {
	// SubDomainAgent 当前与父 Domain 共享 Skill 子集（通过父 ID 查），
	// skillBrief 暂为空串，工具列表由 executeAssistantWithTools 内部默认值提供。
	// 委托公共执行入口：SubDomainAgent 不启用写文件完成门控（保持原行为）。
	return CommonExecuteAssistantTask(ctx, n.modelFactory, n.toolCallback, n.rt, def, task, state,
		"", n.progress, "SubDomainAgent["+def.Name+"]", 0, false)
}

// analyzeSubTasks 分析子领域任务（优先LLM，回退规则）。
//
// 职责：把领域目标拆成 2-4 个可执行的子任务。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（取 DomainGoal 作为分析输入）
//
// 返回：子任务列表；无目标或实例缺失时返回 nil 或单元素列表。
func (n *SubDomainAgentNode) analyzeSubTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	// 取领域目标；为空则直接返回 nil
	goal := state.DomainGoal
	if goal == "" {
		return nil // 无目标
	}

	// 取本实例；缺失则把整个目标作为单任务
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return []string{goal} // 实例缺失：单任务
	}

	// 尝试 LLM 拆解
	if n.modelFactory != nil {
		if tasks := n.analyzeSubTasksWithLLM(ctx, inst.Domain, goal); len(tasks) > 0 {
			return tasks // 返回 LLM 拆解结果
		}
	}

	// LLM 失败则回退规则
	return n.analyzeSubTasksByRules(inst.Domain, goal)
}

// analyzeSubTasksWithLLM 使用LLM分析子领域任务。
//
// 职责：调用领域模型，把目标拆成 2-4 个子任务。
//
// 参数：
//   - ctx：请求上下文
//   - subDomain：子领域名
//   - goal：领域目标
//
// 返回：子任务列表；LLM 不可用或返回空则返回 nil。
//
// 副作用：通过 emitDetail 推送 Prompt 与 Token 消耗事件。
func (n *SubDomainAgentNode) analyzeSubTasksWithLLM(ctx context.Context, subDomain, goal string) []string {
	// 取领域模型；失败则返回 nil
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return nil
	}

	// 构造拆解 prompt：要求 2-4 个可独立执行的子任务
	prompt := fmt.Sprintf(`你是子领域任务分析师。请将以下目标在子领域"%s"中拆解为2-4个具体可执行的子任务。

目标: %s

要求:
- 每个子任务具体且可独立执行
- 只输出子任务列表，每行一个，不要编号，不要其他内容

子任务:`, subDomain, goal)

	// 标识调用者，用于事件展示
	caller := "SubDomainAgent/子任务分析"
	// 推送 prompt 调试事件（含 token 估算与摘要）
	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// 带自适应超时调用 LLM（30s 软超时 / 90s 硬超时）
	resp, callErr, _ := n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller, 30*time.Second, 90*time.Second)
	if callErr != nil || resp == "" {
		return nil
	}

	// 推送最近一次调用的 Token 消耗
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1] // 取最新一条记录
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	// 推送 LLM 响应摘要调试事件（500 字截断），便于排查子任务拆解结果
	n.emitDetail(ctx, "llm_response",
		fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
		model.SummarizePrompt(resp, 500))

	// 解析响应：逐行清洗（去前缀 "- "/"* "、去 "1. " 编号），过滤过短行
	var tasks []string
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)          // 去首尾空白
		line = strings.TrimPrefix(line, "- ")   // 去 "- " 前缀
		line = strings.TrimPrefix(line, "* ")   // 去 "* " 前缀
		// 去除行首的 "1. " / "2. " 等编号
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:] // 截掉编号前缀
		}
		// 长度 >=2 视为有效任务
		if len(line) >= 2 {
			tasks = append(tasks, line) // 收集有效任务
		}
	}
	return tasks
}

// analyzeSubTasksByRules 基于规则分析子任务。
//
// 职责：LLM 不可用时的回退方案，按子领域名关键词匹配预设模板。
//
// 参数：
//   - subDomain：子领域名
//   - goal：领域目标
//
// 返回：子任务列表；无匹配时把整个 goal 作为单任务。
func (n *SubDomainAgentNode) analyzeSubTasksByRules(subDomain, goal string) []string {
	var tasks []string

	// 按子领域名关键词匹配前端组件模板
	if strings.Contains(subDomain, "头部") || strings.Contains(subDomain, "header") {
		tasks = append(tasks, "分析头部组件结构") // 第1步：理解结构
		tasks = append(tasks, "检查导航栏样式")   // 第2步：定位样式问题
		tasks = append(tasks, "修复头部布局问题") // 第3步：执行修复
	} else if strings.Contains(subDomain, "列表") || strings.Contains(subDomain, "list") {
		tasks = append(tasks, "分析列表渲染逻辑") // 第1步：理解渲染
		tasks = append(tasks, "检查分页组件")   // 第2步：定位分页
		tasks = append(tasks, "修复列表样式")   // 第3步：执行修复
	} else if strings.Contains(subDomain, "底部") || strings.Contains(subDomain, "footer") {
		tasks = append(tasks, "检查底部导航") // 第1步：检查导航
		tasks = append(tasks, "修复底部样式") // 第2步：执行修复
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
// matchFixedAssistant 委托公共实现：按技能/关键词打分匹配固定助手。
func (n *SubDomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
	return CommonMatchFixedAssistant(n.registry, task)
}

// summarizeResults 汇总助手结果。
//
// 职责：把当前块的 TaskResults 拼成简短摘要，写入 state.Reason 供上层展示。
//
// 参数：
//   - state：图全局状态（原地修改 state.Reason）
//
// 副作用：修改 state.Reason；每个结果截断到 100 字符。
func (n *SubDomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	// 取本实例
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理，直接返回
		return
	}

	// 取当前块并收集结果摘要
	block := state.ActiveBlocks[state.CurrentBlockID]
	summaries := CommonCollectTaskSummaries(block)

	// 有摘要则拼成一句话写入 state.Reason
	if len(summaries) > 0 {
		// 用分号连接所有摘要
		state.Reason = fmt.Sprintf("子领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}
