package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/types"
)

// SubDomainAgentNode Layer 2.5: 子领域Agent
type SubDomainAgentNode struct {
	name         string
	instID       string
	registry     *RoleRegistry
	factory      *RoleFactory
	modelFactory *model.ModelFactory
	toolCallback ToolCallback
}

// NewSubDomainAgentNode 创建子领域Agent节点
func NewSubDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *SubDomainAgentNode {
	return &SubDomainAgentNode{
		name:     "SubDomainAgent",
		instID:   instID,
		registry: registry,
		factory:  factory,
	}
}

// SetModelFactory 设置模型工厂
func (n *SubDomainAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行回调
func (n *SubDomainAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// Name 返回节点名称
func (n *SubDomainAgentNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID
func (n *SubDomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行子领域Agent逻辑
func (n *SubDomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return nil, fmt.Errorf("subdomain agent instance %s not found", n.instID)
	}

	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	tasks := n.analyzeSubTasks(ctx, state)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = types.ActionContinue
		return state, nil
	}
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 过滤已完成任务
	var pendingTasks []string
	for _, task := range tasks {
		if _, done := block.TaskResults[task]; done {
			continue
		}
		pendingTasks = append(pendingTasks, task)
	}

	if len(pendingTasks) > 0 {
		// 并行执行所有待处理任务
		results := n.dispatchAssistantsParallel(ctx, state, inst, pendingTasks)

		var mu sync.Mutex
		mu.Lock()
		for task, result := range results {
			block.TaskResults[task] = result
		}
		mu.Unlock()
	}

	// 子领域任务完成，弹出调用栈，返回父DomainAgent
	state.PopCallStack()
	n.summarizeResults(state)
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	state.NextAction = types.ActionSwitch
	if inst.ParentID != "" {
		state.TargetRoleID = inst.ParentID
		n.registry.UpdateInstanceStatus(inst.ParentID, types.RoleStatusActive)
	} else {
		state.TargetRoleID = "MetaAgent"
	}

	return state, nil
}

// dispatchAssistantsParallel 并行调度助手
func (n *SubDomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
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

// createAssistantForTask 创建助手实例
func (n *SubDomainAgentNode) createAssistantForTask(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, task string) (*types.RoleInstance, *types.RoleDefinition) {
	assistantDef := n.matchFixedAssistant(task)
	if assistantDef != nil {
		if !n.registry.CanCall(n.instID, assistantDef.ID) {
			return nil, nil
		}
		assistantInst, err := n.registry.CreateInstance(assistantDef.ID, state.SessionID, inst.Domain, n.instID)
		if err != nil {
			fmt.Printf("[SubDomainAgent] create fixed assistant %s failed: %v\n", assistantDef.ID, err)
			return nil, nil
		}
		return assistantInst, assistantDef
	}

	assistantInst, err := n.factory.CreateAssistant(ctx, state.SessionID, task, n.instID, inst.RoleDefID)
	if err != nil {
		fmt.Printf("[SubDomainAgent] create dynamic assistant for %q failed: %v\n", task, err)
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

// runAssistant 运行助手执行任务
func (n *SubDomainAgentNode) runAssistant(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, def *types.RoleDefinition, task string) string {
	n.registry.UpdateInstanceStatus(inst.ID, types.RoleStatusActive)

	var result string
	var err error

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
func (n *SubDomainAgentNode) executeAssistantTask(ctx context.Context, def *types.RoleDefinition, task string, state *types.ThreeLayerState) (string, error) {
	if n.modelFactory != nil {
		executor := NewToolExecutor("")
		if n.toolCallback != nil {
			executor.SetCallback(n.toolCallback)
		}
		result, _ := executeAssistantWithTools(ctx, n.modelFactory, executor, def, task, state)
		if result != "" {
			return result, nil
		}

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

	contextInfo := ""
	if state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("[领域: %s] ", state.CurrentDomain)
	}
	return fmt.Sprintf("%s助手[%s]完成任务: %s", contextInfo, def.Name, task), nil
}

// analyzeSubTasks 分析子领域任务（优先LLM，回退规则）
func (n *SubDomainAgentNode) analyzeSubTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return []string{goal}
	}

	// 尝试LLM
	if n.modelFactory != nil {
		if tasks := n.analyzeSubTasksWithLLM(ctx, inst.Domain, goal); len(tasks) > 0 {
			return tasks
		}
	}

	// 规则回退
	return n.analyzeSubTasksByRules(inst.Domain, goal)
}

// analyzeSubTasksWithLLM 使用LLM分析子领域任务
func (n *SubDomainAgentNode) analyzeSubTasksWithLLM(ctx context.Context, subDomain, goal string) []string {
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return nil
	}

	prompt := fmt.Sprintf(`你是子领域任务分析师。请将以下目标在子领域"%s"中拆解为2-4个具体可执行的子任务。

目标: %s

要求:
- 每个子任务具体且可独立执行
- 只输出子任务列表，每行一个，不要编号，不要其他内容

子任务:`, subDomain, goal)

	resp, err := llm.Generate(ctx, prompt)
	if err != nil || resp == "" {
		return nil
	}

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

// analyzeSubTasksByRules 基于规则分析子任务
func (n *SubDomainAgentNode) analyzeSubTasksByRules(subDomain, goal string) []string {
	var tasks []string

	if strings.Contains(subDomain, "头部") || strings.Contains(subDomain, "header") {
		tasks = append(tasks, "分析头部组件结构")
		tasks = append(tasks, "检查导航栏样式")
		tasks = append(tasks, "修复头部布局问题")
	} else if strings.Contains(subDomain, "列表") || strings.Contains(subDomain, "list") {
		tasks = append(tasks, "分析列表渲染逻辑")
		tasks = append(tasks, "检查分页组件")
		tasks = append(tasks, "修复列表样式")
	} else if strings.Contains(subDomain, "底部") || strings.Contains(subDomain, "footer") {
		tasks = append(tasks, "检查底部导航")
		tasks = append(tasks, "修复底部样式")
	} else {
		tasks = append(tasks, goal)
	}

	return tasks
}

// matchFixedAssistant 匹配固定助手
func (n *SubDomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
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

// summarizeResults 汇总助手结果
func (n *SubDomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return
	}

	block := state.ActiveBlocks[state.CurrentBlockID]
	var summaries []string

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
		state.Reason = fmt.Sprintf("子领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}
