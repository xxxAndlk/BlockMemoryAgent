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
	"github.com/blockmemory/agent/backend/pkg/types"
)

// DomainAgentNode Layer 2: 会话块Agent / 领域上下文管理器
type DomainAgentNode struct {
	name           string
	instID         string // 本实例ID
	registry       *RoleRegistry
	factory        *RoleFactory
	modelFactory   *model.ModelFactory
	toolCallback   ToolCallback
	timeoutTracker *model.TimeoutTracker
	rt             *runtime.Runtime
	progress       ProgressCallback
}

// NewDomainAgentNode 创建领域Agent节点
func NewDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *DomainAgentNode {
	return &DomainAgentNode{
		name:           "DomainAgent",
		instID:         instID,
		registry:       registry,
		factory:        factory,
		timeoutTracker: model.NewTimeoutTracker(),
	}
}

// SetModelFactory 设置模型工厂（用于LLM任务分析）
func (n *DomainAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行回调
func (n *DomainAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// SetRuntime 注入 Runtime（板/邮箱/Skill）
func (n *DomainAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// SetProgressCallback 注入进度回调
func (n *DomainAgentNode) SetProgressCallback(cb ProgressCallback) {
	n.progress = cb
}

// emit 推送进度事件
func (n *DomainAgentNode) emit(kind, message string) {
	if n.progress == nil {
		return
	}
	agent := "DomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "DomainAgent[" + inst.Domain + "]"
	}
	n.progress(ProgressEvent{Kind: kind, Agent: agent, Message: message})
}

// emitDetail 推送带详情的进度事件
func (n *DomainAgentNode) emitDetail(kind, message, detail string) {
	if n.progress == nil {
		return
	}
	agent := "DomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "DomainAgent[" + inst.Domain + "]"
	}
	n.progress(ProgressEvent{Kind: kind, Agent: agent, Message: message, Detail: detail})
}

// Name 返回节点名称
func (n *DomainAgentNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID
func (n *DomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行领域Agent逻辑
func (n *DomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return nil, fmt.Errorf("domain agent instance %s not found", n.instID)
	}

	n.emit("think", fmt.Sprintf("DomainAgent 启动，领域目标: %s", state.DomainGoal))
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// v3 §5：为本 DomainAgent 装配领域 Skill 子集（如未装配）
	n.ensureSkillSet(ctx, inst, state)
	if n.rt != nil && n.rt.Skills != nil {
		if set := n.rt.Skills.GetForAgent(n.instID); set != nil && len(set.Skills) > 0 {
			ids := make([]string, 0, len(set.Skills))
			for _, s := range set.Skills {
				ids = append(ids, s.SkillID)
			}
			n.emit("think", "已装配 Skill 子集: "+strings.Join(ids, ", "))
		}
	}

	tasks := n.analyzeTasks(ctx, state)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = types.ActionContinue
		return state, nil
	}
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 检查是否需要拆分为子领域
	if n.shouldSplitToSubDomains(ctx, state, tasks) {
		n.emit("intend", fmt.Sprintf("领域较复杂（%d 个子任务），拆分为子领域并行处理", len(tasks)))
		return n.handleSubDomainSplit(ctx, state, inst)
	}

	// 过滤已完成的任务
	var pendingTasks []string
	for _, task := range tasks {
		if _, done := block.TaskResults[task]; done {
			continue
		}
		pendingTasks = append(pendingTasks, task)
	}

	if len(pendingTasks) == 0 {
		n.emit("think", "所有子任务已完成，汇总结果")
		n.summarizeResults(state)
		n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)
		block.Status = "completed"
		state.NextAction = types.ActionContinue
		return state, nil
	}

	n.emit("intend", fmt.Sprintf("派发 %d 个助手任务: %s", len(pendingTasks), strings.Join(pendingTasks, "; ")))
	// 并行执行所有待处理任务
	results := n.dispatchAssistantsParallel(ctx, state, inst, pendingTasks)

	var mu sync.Mutex
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}
	mu.Lock()
	for task, result := range results {
		block.TaskResults[task] = result
	}
	mu.Unlock()

	n.summarizeResults(state)
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)
	block.Status = "completed"
	state.NextAction = types.ActionContinue
	return state, nil
}

// ensureSkillSet 确保该 DomainAgent 已装配 Skill 子集。
//
// 若未装配则按"领域名 + 目标"调用 Pool.AssembleSet 触发 LLM 选择，
// 装配后通过 Registry 绑定到当前 agent 实例 ID。
func (n *DomainAgentNode) ensureSkillSet(ctx context.Context, inst *types.RoleInstance, state *types.ThreeLayerState) {
	if n.rt == nil || n.rt.Skills == nil {
		return
	}
	if existing := n.rt.Skills.GetForAgent(n.instID); existing != nil {
		return
	}
	var llm skill.LLMClient
	if n.modelFactory != nil {
		if c, err := n.modelFactory.GetDomainModel(ctx); err == nil {
			llm = c
		}
	}
	set := n.rt.Skills.Pool().AssembleSet(ctx, llm, n.instID, inst.Domain, state.DomainGoal, 8)
	n.rt.Skills.Bind(set)
}
func (n *DomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, task := range tasks {
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			mu.Lock()
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(task string, aInst *types.RoleInstance, aDef *types.RoleDefinition) {
			defer wg.Done()

			result := n.runAssistant(ctx, state, aInst, aDef, task)

			mu.Lock()
			results[task] = result
			mu.Unlock()
		}(task, assistantInst, assistantDef)
	}

	wg.Wait()
	return results
}

// createAssistantForTask 为指定任务创建助手实例
func (n *DomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	// 先匹配固定助手
	assistantDef := n.matchFixedAssistant(task)
	if assistantDef != nil {
		if !n.registry.CanCall(n.instID, assistantDef.ID) {
			return nil, nil
		}
		assistantInst, err := n.registry.CreateInstance(assistantDef.ID, state.SessionID, inst.Domain, n.instID)
		if err != nil {
			fmt.Printf("[DomainAgent] create fixed assistant %s failed: %v\n", assistantDef.ID, err)
			return nil, nil
		}
		return assistantInst, assistantDef
	}

	// 动态创建助手
	assistantInst, err := n.factory.CreateAssistant(ctx, state.SessionID, task, n.instID, inst.RoleDefID)
	if err != nil {
		fmt.Printf("[DomainAgent] create dynamic assistant for %q failed: %v\n", task, err)
		return nil, nil
	}
	assistantDef = n.registry.GetRoleDef(assistantInst.RoleDefID)
	if assistantDef == nil {
		return nil, nil
	}
	if !n.registry.CanCall(n.instID, assistantDef.ID) {
		return nil, nil
	}
	return assistantInst, assistantDef
}

// runAssistant 在当前goroutine中运行助手执行任务
func (n *DomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) string {
	n.registry.UpdateInstanceStatus(inst.ID, types.RoleStatusActive)

	var result string
	var err error

	// 使用重试机制执行任务
	err = retryWithBackoff(3, 100*time.Millisecond, func() error {
		result, err = n.executeAssistantTask(ctx, def, task, state)
		return err
	})

	n.registry.UpdateInstanceStatus(inst.ID, types.RoleStatusDone)

	if err != nil {
		return fmt.Sprintf("[ERROR] 助手[%s]执行失败: %v", def.Name, err)
	}
	return result
}

// executeAssistantTask 执行助手任务（LLM+工具循环 或 回退模拟）
func (n *DomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (string, error) {
	// 优先使用 LLM + 工具执行
	if n.modelFactory != nil {
		executor := NewToolExecutor("")
		if n.toolCallback != nil {
			executor.SetCallback(n.toolCallback)
		}

		// 取出本 DomainAgent 装配的 Skill 列表（v3 §5）
		var skillBrief string
		if n.rt != nil && n.rt.Skills != nil {
			if set := n.rt.Skills.GetForAgent(n.instID); set != nil {
				skillBrief = set.PromptList()
			}
		}

		result, _ := executeAssistantWithTools(ctx, n.modelFactory, executor, def, task, state, skillBrief, n.progress, "助手["+def.Name+"]")
		if result != "" {
			// 完成门控：若任务要求写文件但结果含失败标记，返回 error 触发上层重试/告警
			if strings.HasPrefix(result, "[失败:") {
				return result, fmt.Errorf("助手未完成写文件任务: %s", task)
			}
			return result, nil
		}

		// 工具执行回退到普通LLM
		llm, err := n.modelFactory.GetModel(ctx, def.ID)
		if err == nil {
			prompt := fmt.Sprintf("%s\n\n当前任务: %s\n领域目标: %s\n请执行任务并返回结果。",
				def.SystemPrompt, task, state.DomainGoal)
			resp, err := llm.Generate(ctx, prompt)
			if err == nil && resp != "" {
				return resp, nil
			}
		}
	}

	// 回退到模拟结果
	contextInfo := ""
	if state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("[领域: %s] ", state.CurrentDomain)
	}
	return fmt.Sprintf("%s助手[%s]完成任务: %s", contextInfo, def.Name, task), nil
}

// analyzeTasks 分析领域任务（优先使用LLM，回退到规则）
func (n *DomainAgentNode) analyzeTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	// 尝试使用LLM进行任务拆解
	if n.modelFactory != nil && !n.timeoutTracker.ShouldSkipLLM() {
		n.emit("llm", "调用 LLM 拆解子任务...")
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个任务分析专家。请将以下目标拆解为2-4个独立可执行的子任务。

目标: %s

要求:
- 每个子任务必须是一个可直接用工具执行的动作（如"用 WriteFile 写 X 文件"、"用 RunCommand 运行 Y"）
- 严禁出现"分析/确定/规划/设计/思考/研究"等纯思考类子任务，这类工作应在执行动作中一并完成
- 涉及创建文件的目标，必须有子任务明确写出文件路径与内容来源
- 子任务之间可以有依赖但应尽量并行
- 只输出子任务列表，每行一个，不要编号，不要其他内容

子任务:`, goal))
		if !timedOut && err == nil && resp != "" {
			if tasks := parseTaskListFromResp(resp); len(tasks) > 0 {
				n.emit("think", fmt.Sprintf("LLM 拆解出 %d 个子任务", len(tasks)))
				return tasks
			}
		}
		if timedOut {
			n.emit("error", "任务拆解 LLM 调用超时，回退到规则")
			fmt.Printf("[DomainAgent] LLM timeout on task analysis, using rules. %s\n", n.timeoutTracker.StatsString())
		} else if err != nil {
			n.emitDetail("error", "任务拆解 LLM 调用失败: "+err.Error(), "")
		}
	}

	// 规则回退
	return n.analyzeTasksByRules(goal)
}

// callLLM 统一LLM调用入口（带自适应超时）
func (n *DomainAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return "", err, false
	}
	return n.timeoutTracker.CallWithTimeout(ctx, llm, prompt,
		30*time.Second,
		90*time.Second,
	)
}

// parseTaskListFromResp 从LLM响应解析任务列表
func parseTaskListFromResp(resp string) []string {
	var tasks []string
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:]
		}
		if len(line) >= 2 {
			tasks = append(tasks, line)
		}
	}
	return tasks
}

// analyzeTasksByRules 基于规则的任务拆解（回退方案）
func (n *DomainAgentNode) analyzeTasksByRules(goal string) []string {
	var tasks []string

	if strings.Contains(goal, "修复") {
		tasks = append(tasks, "分析根因")
		tasks = append(tasks, "定位问题代码")
		tasks = append(tasks, "生成修复方案")
		tasks = append(tasks, "验证修复")
	} else if strings.Contains(goal, "实现") || strings.Contains(goal, "开发") {
		tasks = append(tasks, "需求分析")
		tasks = append(tasks, "设计方案")
		tasks = append(tasks, "编写代码")
		tasks = append(tasks, "测试验证")
	} else if strings.Contains(goal, "优化") {
		tasks = append(tasks, "性能分析")
		tasks = append(tasks, "识别瓶颈")
		tasks = append(tasks, "实施优化")
	} else {
		tasks = append(tasks, goal)
	}

	return tasks
}

// matchFixedAssistant 匹配固定助手
func (n *DomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
	taskLower := strings.ToLower(task)
	var bestMatch *types.RoleDefinition
	bestScore := 0

	for _, def := range n.registry.GetAssistantRoleDefs() {
		if def.Type != types.RoleTypeFixed {
			continue
		}
		score := 0
		for _, skill := range def.Skills {
			if strings.Contains(taskLower, strings.ToLower(skill)) {
				score += 10
			}
		}
		for _, kw := range def.Keywords {
			if strings.Contains(taskLower, strings.ToLower(kw)) {
				score += 5
			}
		}
		if score > bestScore {
			bestScore = score
			bestMatch = def
		}
	}

	if bestScore >= 10 {
		return bestMatch
	}
	return nil
}

// shouldSplitToSubDomains 判断是否需要拆分为子领域（基于任务复杂度+LLM判断）
func (n *DomainAgentNode) shouldSplitToSubDomains(ctx context.Context, state *types.ThreeLayerState, tasks []string) bool {
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block != nil && block.SubDomainSplit {
		if block.SubDomainIndex < len(block.SubDomainList) {
			return true
		}
		return false
	}

	if len(tasks) < 4 {
		return false
	}

	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return false
	}

	// 尝试使用LLM判断是否需要拆分
	if n.modelFactory != nil {
		return n.shouldSplitWithLLM(ctx, inst.Domain, tasks)
	}

	// 规则回退：复杂领域自动拆分
	return strings.Contains(inst.Domain, "商城") || strings.Contains(inst.Domain, "页面")
}

// shouldSplitWithLLM 使用LLM判断是否需要拆分子领域
func (n *DomainAgentNode) shouldSplitWithLLM(ctx context.Context, domain string, tasks []string) bool {
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return false
	}

	prompt := fmt.Sprintf(`判断以下领域是否需要拆分为多个子领域并行处理。

领域: %s
子任务数量: %d
子任务列表:
%s

如果该领域包含多个独立模块（如前端页面的头部/列表/底部），或者子任务可以明确分为2-4个并行组，回答"是"。
否则回答"否"。
只回答"是"或"否"。`, domain, len(tasks), strings.Join(tasks, "\n"))

	resp, err := llm.Generate(ctx, prompt)
	if err != nil {
		return false
	}

	return strings.Contains(resp, "是") || strings.Contains(strings.ToLower(resp), "yes")
}

// handleSubDomainSplit 拆分子领域并调度（支持依次调度多个）
func (n *DomainAgentNode) handleSubDomainSplit(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance) (*types.ThreeLayerState, error) {
	block := state.ActiveBlocks[state.CurrentBlockID]

	// 首次拆分：初始化子领域列表
	if block != nil && !block.SubDomainSplit {
		block.SubDomainSplit = true
		subDomains := n.inferSubDomains(ctx, inst.Domain)
		for _, sd := range subDomains {
			block.SubDomainList = append(block.SubDomainList, sd.Name)
		}
	}

	// 检查是否还有未处理的子领域
	if block == nil || block.SubDomainIndex >= len(block.SubDomainList) {
		state.NextAction = types.ActionContinue
		return state, nil
	}

	// 获取当前子领域
	subDomainName := block.SubDomainList[block.SubDomainIndex]
	block.SubDomainIndex++

	subInst, err := n.factory.CreateSubDomainAgent(ctx, state.SessionID, subDomainName, state.DomainGoal, n.instID)
	if err != nil {
		fmt.Printf("[DomainAgent] create subdomain agent %s failed: %v\n", subDomainName, err)
		state.NextAction = types.ActionContinue
		return state, nil
	}

	callReq := &types.CallRequest{
		ID:       fmt.Sprintf("call_%s_%d", subInst.ID, len(state.CallStack)),
		CallerID: n.instID,
		CalleeID: subInst.ID,
		Task:     state.DomainGoal,
		Context: map[string]any{
			"domain":          inst.Domain,
			"sub_domain":      subDomainName,
			"block_id":        state.CurrentBlockID,
			"domain_goal":     state.DomainGoal,
			"session_summary": state.SessionSummary,
		},
		Priority: 5,
	}

	state.PushCallStack(callReq)
	state.NextAction = types.ActionSwitch
	state.TargetRoleID = subInst.ID
	return state, nil
}

// inferSubDomains 推断子领域列表（优先LLM，回退规则）
func (n *DomainAgentNode) inferSubDomains(ctx context.Context, domain string) []DomainInfo {
	if n.modelFactory != nil {
		if subs := n.inferSubDomainsWithLLM(ctx, domain); len(subs) > 0 {
			return subs
		}
	}
	return n.inferSubDomainsByRules(domain)
}

// inferSubDomainsWithLLM 使用LLM推断子领域
func (n *DomainAgentNode) inferSubDomainsWithLLM(ctx context.Context, domain string) []DomainInfo {
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return nil
	}

	prompt := fmt.Sprintf(`将以下领域拆分为2-4个独立的子领域。

领域: %s

要求:
- 每个子领域名称简短（2-6个字）
- 子领域之间尽量独立，可并行处理
- 只输出子领域名称，每行一个，不要编号，不要其他内容

子领域:`, domain)

	resp, err := llm.Generate(ctx, prompt)
	if err != nil || resp == "" {
		return nil
	}

	var result []DomainInfo
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:]
		}
		if len(line) >= 2 {
			result = append(result, DomainInfo{
				Name: line,
				Goal: fmt.Sprintf("处理%s相关的子任务", line),
			})
		}
	}
	return result
}

// inferSubDomainsByRules 基于规则推断子领域
func (n *DomainAgentNode) inferSubDomainsByRules(domain string) []DomainInfo {
	if strings.Contains(domain, "商城") || strings.Contains(domain, "页面") {
		return []DomainInfo{
			{Name: "首页头部", Goal: "修复头部导航样式问题"},
			{Name: "商品列表", Goal: "修复商品列表布局问题"},
			{Name: "底部导航", Goal: "修复底部导航样式问题"},
		}
	}
	return []DomainInfo{{Name: domain + "子任务", Goal: "执行细分任务"}}
}

// summarizeResults 汇总助手结果
func (n *DomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return
	}

	block := state.ActiveBlocks[state.CurrentBlockID]
	var summaries []string

	// 从 TaskResults 收集
	if block != nil && block.TaskResults != nil {
		for task, result := range block.TaskResults {
			shortResult := result
			if len(shortResult) > 100 {
				shortResult = shortResult[:100] + "..."
			}
			summaries = append(summaries, fmt.Sprintf("%s: %s", task, shortResult))
		}
	}

	if len(summaries) > 0 {
		state.Reason = fmt.Sprintf("领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}

// retryWithBackoff 指数退避重试
func retryWithBackoff(maxRetries int, initialDelay time.Duration, fn func() error) error {
	var err error
	delay := initialDelay
	for i := 0; i < maxRetries; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i < maxRetries-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return err
}
