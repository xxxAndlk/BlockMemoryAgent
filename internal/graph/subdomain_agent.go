package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/pkg/types"
)

// SubDomainAgentNode Layer 2.5: 子领域Agent
type SubDomainAgentNode struct {
	name      string
	instID    string
	registry  *RoleRegistry
	factory   *RoleFactory
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

	// 子领域直接处理任务，不再继续拆分
	tasks := n.analyzeSubTasks(state)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = types.ActionContinue
		return state, nil
	}
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	for _, task := range tasks {
		if _, done := block.TaskResults[task]; done {
			continue
		}

		var assistantInst *types.RoleInstance
		var assistantDef *types.RoleDefinition

		assistantDef = n.matchFixedAssistant(task)
		if assistantDef != nil {
			if !n.registry.CanCall(n.instID, assistantDef.ID) {
				continue
			}
			var err error
			assistantInst, err = n.registry.CreateInstance(assistantDef.ID, state.SessionID, inst.Domain, n.instID)
			if err != nil {
				continue
			}
		} else {
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

		callReq := &types.CallRequest{
			ID:       fmt.Sprintf("call_%s_%d", assistantInst.ID, len(state.CallStack)),
			CallerID: n.instID,
			CalleeID: assistantInst.ID,
			Task:     task,
			Context: map[string]any{
				"domain":          inst.Domain,
				"block_id":        block.ID,
				"parent_domain":   inst.ParentID,
				"domain_goal":     state.DomainGoal,
				"session_summary": state.SessionSummary,
			},
			Priority: 5,
		}

		state.PushCallStack(callReq)
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = assistantInst.ID
		return state, nil
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

// analyzeSubTasks 分析子领域任务
func (n *SubDomainAgentNode) analyzeSubTasks(state *types.ThreeLayerState) []string {
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return []string{goal}
	}

	// 子领域进一步细化任务
	subDomain := inst.Domain
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
		// 默认直接处理
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

	var summaries []string
	for _, childID := range inst.Children {
		child := n.registry.GetInstance(childID)
		if child == nil {
			continue
		}
		summaries = append(summaries, fmt.Sprintf("助手[%s]: 已完成", child.RoleDefID))
	}

	if len(summaries) > 0 {
		state.Reason = fmt.Sprintf("子领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}
}
