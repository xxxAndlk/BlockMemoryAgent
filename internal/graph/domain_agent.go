package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/pkg/types"
)

// DomainAgentNode Layer 2: 会话块Agent / 领域上下文管理器
type DomainAgentNode struct {
	name      string
	instID    string            // 本实例ID
	registry  *RoleRegistry
	factory   *RoleFactory
}

// NewDomainAgentNode 创建领域Agent节点
func NewDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *DomainAgentNode {
	return &DomainAgentNode{
		name:     "DomainAgent",
		instID:   instID,
		registry: registry,
		factory:  factory,
	}
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

	// 更新状态为活跃
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// 1. 分析当前领域任务，确定需要哪些助手
	tasks := n.analyzeTasks(state)

	// 获取当前会话块
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = types.ActionContinue
		return state, nil
	}
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// 1.5 检查是否需要拆分为子领域
	if n.shouldSplitToSubDomains(state, tasks) {
		return n.handleSubDomainSplit(ctx, state, inst)
	}

	// 2. 为每个未完成任务匹配或创建助手
	for _, task := range tasks {
		if _, done := block.TaskResults[task]; done {
			continue
		}

		var assistantInst *types.RoleInstance
		var assistantDef *types.RoleDefinition

		// 先匹配固定助手
		assistantDef = n.matchFixedAssistant(task)
		if assistantDef != nil {
			// 固定助手：检查权限并创建实例
			if !n.registry.CanCall(n.instID, assistantDef.ID) {
				continue
			}
			var err error
			assistantInst, err = n.registry.CreateInstance(assistantDef.ID, state.SessionID, inst.Domain, n.instID)
			if err != nil {
				continue
			}
		} else {
			// 无固定助手匹配，动态创建（工厂已注册定义并创建实例）
			var err error
			assistantInst, err = n.factory.CreateAssistant(ctx, state.SessionID, task, n.instID, inst.RoleDefID)
			if err != nil {
				continue
			}
			assistantDef = n.registry.GetRoleDef(assistantInst.RoleDefID)
			if assistantDef == nil {
				continue
			}
			if !n.registry.CanCall(n.instID, assistantDef.ID) {
				continue
			}
		}

		if assistantInst == nil {
			continue
		}

		// 构建调用请求
		callReq := &types.CallRequest{
			ID:       fmt.Sprintf("call_%s_%d", assistantInst.ID, len(state.CallStack)),
			CallerID: n.instID,
			CalleeID: assistantInst.ID,
			Task:     task,
			Context: map[string]any{
				"domain":          inst.Domain,
				"block_id":        block.ID,
				"domain_goal":     state.DomainGoal,
				"session_summary": state.SessionSummary,
			},
			Priority: 5,
		}

		// 压入调用栈
		state.PushCallStack(callReq)
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = assistantInst.ID
		return state, nil
	}

	// 3. 没有需要调用的助手，领域任务完成
	// 汇总助手结果
	n.summarizeResults(state)

	// 更新状态
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	// 向工作区发布输出
	block = state.ActiveBlocks[state.CurrentBlockID]
	if block != nil {
		block.Status = "completed"
	}

	state.NextAction = types.ActionContinue
	return state, nil
}

// analyzeTasks 分析领域任务，拆解为子任务
func (n *DomainAgentNode) analyzeTasks(state *types.ThreeLayerState) []string {
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	var tasks []string

	// 简单规则拆解（实际应由大模型做）
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
		// 默认：直接处理
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

// shouldSplitToSubDomains 判断是否需要拆分为子领域
// 规则：当任务数量超过阈值且领域包含复杂模块时
func (n *DomainAgentNode) shouldSplitToSubDomains(state *types.ThreeLayerState, tasks []string) bool {
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block != nil && block.SubDomainSplit {
		// 已拆分，检查是否还有未处理的子领域
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
	// 商城页面等复杂领域自动拆分
	if strings.Contains(inst.Domain, "商城") || strings.Contains(inst.Domain, "页面") {
		return true
	}
	return false
}

// handleSubDomainSplit 拆分子领域并调度（支持依次调度多个）
func (n *DomainAgentNode) handleSubDomainSplit(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance) (*types.ThreeLayerState, error) {
	block := state.ActiveBlocks[state.CurrentBlockID]

	// 首次拆分：初始化子领域列表
	if block != nil && !block.SubDomainSplit {
		block.SubDomainSplit = true
		subDomains := n.inferSubDomains(inst.Domain)
		for _, sd := range subDomains {
			block.SubDomainList = append(block.SubDomainList, sd.Name)
		}
	}

	// 检查是否还有未处理的子领域
	if block == nil || block.SubDomainIndex >= len(block.SubDomainList) {
		// 所有子领域处理完成，回退到直接处理
		state.NextAction = types.ActionContinue
		return state, nil
	}

	// 获取当前子领域
	subDomainName := block.SubDomainList[block.SubDomainIndex]
	block.SubDomainIndex++

	subInst, err := n.factory.CreateSubDomainAgent(ctx, state.SessionID, subDomainName, state.DomainGoal, n.instID)
	if err != nil {
		// 创建失败，尝试下一个
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

// inferSubDomains 推断子领域列表
func (n *DomainAgentNode) inferSubDomains(domain string) []DomainInfo {
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
	// 收集本Domain下所有助手实例的输出
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return
	}

	var summaries []string
	for _, childID := range inst.Children {
		child := n.registry.GetInstance(childID)
		if child == nil {
			continue
		}
		// TODO: 从工作区获取助手输出
		summaries = append(summaries, fmt.Sprintf("助手[%s]: 已完成", child.RoleDefID))
	}

	if len(summaries) > 0 {
		state.Reason = fmt.Sprintf("领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}
