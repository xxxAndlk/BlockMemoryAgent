package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/pkg/types"
)

// MetaAgentNode Layer 1: 主Agent / 会话调度器
type MetaAgentNode struct {
	name       string
	registry   *RoleRegistry
	factory    *RoleFactory
	maxBlocks  int
	summaryInterval int
	stepCount  int
}

// NewMetaAgentNode 创建主Agent节点
func NewMetaAgentNode(registry *RoleRegistry, factory *RoleFactory, maxBlocks, summaryInterval int) *MetaAgentNode {
	return &MetaAgentNode{
		name:            "MetaAgent",
		registry:        registry,
		factory:         factory,
		maxBlocks:       maxBlocks,
		summaryInterval: summaryInterval,
		stepCount:       0,
	}
}

// Name 返回节点名称
func (n *MetaAgentNode) Name() string {
	return n.name
}

// Invoke 执行主Agent逻辑
func (n *MetaAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	n.stepCount++

	// 1. 检查是否需要会话总结
	if n.stepCount%n.summaryInterval == 0 {
		n.updateSessionSummary(state)
	}

	// 2. 清理过期实例
	n.registry.CleanupExpired()

	// 3. 决策下一步
	switch {
	// 首次启动：分析用户目标，创建第一个DomainAgent
	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID == "":
		return n.handleInitial(ctx, state)

	// 所有会话块完成
	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID != "":
		state.NextAction = types.ActionFinish
		state.Reason = "all blocks completed"

	// 当前有活跃会话块，需要调度
	case state.CurrentBlockID != "" && state.IsCalling():
		// 有助手调用正在进行，继续执行助手
		state.NextAction = types.ActionContinue

	// 当前会话块有助手调用请求
	case state.CurrentBlockID != "" && !state.IsCalling():
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil && len(block.Events) > 0 {
			// 处理DomainAgent提交的事件
			return n.handleBlockEvents(ctx, state, block)
		}
		// 当前会话块完成，切换到下一个
		return n.switchToNextBlock(ctx, state)

	default:
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// handleInitial 首次启动处理
func (n *MetaAgentNode) handleInitial(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 分析用户目标，确定需要哪些领域
	domains := n.analyzeDomains(state)

	// 为每个领域创建DomainAgent
	for _, domain := range domains {
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, domain.Name, domain.Goal, "")
		if err != nil {
			continue
		}

		block := &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(domain.Name), len(state.ActiveBlocks)),
			SessionID:   state.SessionID,
			Domain:      domain.Name,
			Goal:        domain.Goal,
			Status:      "active",
			Agents:      []string{inst.ID},
			Events:      make([]*types.Event, 0),
			TaskResults: make(map[string]string),
		}
		state.ActiveBlocks[block.ID] = block
	}

	// 激活第一个会话块
	for blockID := range state.ActiveBlocks {
		state.CurrentBlockID = blockID
		block := state.ActiveBlocks[blockID]
		state.CurrentDomain = block.Domain
		state.DomainGoal = block.Goal
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = block.Agents[0] // DomainAgent实例ID
		break
	}

	n.updateSessionSummary(state)
	return state, nil
}

// handleBlockEvents 处理会话块事件
func (n *MetaAgentNode) handleBlockEvents(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock) (*types.ThreeLayerState, error) {
	for _, ev := range block.Events {
		if ev.Status != types.EventPending {
			continue
		}

		switch ev.Type {
		case types.EventCrossModify:
			// DomainAgent请求其他领域协作
			return n.handleCrossDomainRequest(ctx, state, block, ev)

		case types.EventEscalation:
			// DomainAgent升级请求
			state.NextAction = types.ActionEscalate
			state.Reason = getString(ev.Payload, "reason")
			return state, nil

		default:
			ev.Status = types.EventDone
		}
	}

	// 事件处理完毕，继续当前DomainAgent
	state.NextAction = types.ActionContinue
	return state, nil
}

// handleCrossDomainRequest 处理跨领域请求
func (n *MetaAgentNode) handleCrossDomainRequest(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock, ev *types.Event) (*types.ThreeLayerState, error) {
	targetDomain := getString(ev.Payload, "target_domain")
	if targetDomain == "" {
		ev.Status = types.EventDone
		state.NextAction = types.ActionContinue
		return state, nil
	}

	// 检查是否已有该领域的会话块
	var targetBlock *types.SessionBlock
	for _, b := range state.ActiveBlocks {
		if b.Domain == targetDomain {
			targetBlock = b
			break
		}
	}

	// 没有则创建
	if targetBlock == nil {
		if len(state.ActiveBlocks) >= n.maxBlocks {
			state.NextAction = types.ActionEscalate
			state.Reason = fmt.Sprintf("max blocks reached, cannot create domain %s", targetDomain)
			return state, nil
		}

		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, targetDomain,
			getString(ev.Payload, "goal"), "")
		if err != nil {
			state.NextAction = types.ActionEscalate
			state.Reason = fmt.Sprintf("failed to create domain agent: %v", err)
			return state, nil
		}

		targetBlock = &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(targetDomain), len(state.ActiveBlocks)),
			SessionID:   state.SessionID,
			Domain:      targetDomain,
			Goal:        getString(ev.Payload, "goal"),
			Status:      "active",
			Agents:      []string{inst.ID},
			Events:      make([]*types.Event, 0),
			TaskResults: make(map[string]string),
		}
		state.ActiveBlocks[targetBlock.ID] = targetBlock
	}

	// 切换到目标会话块
	state.CurrentBlockID = targetBlock.ID
	state.CurrentDomain = targetBlock.Domain
	state.DomainGoal = targetBlock.Goal
	state.NextAction = types.ActionSwitch
	state.TargetRoleID = targetBlock.Agents[0]
	ev.Status = types.EventDone

	return state, nil
}

// switchToNextBlock 切换到下一个会话块
func (n *MetaAgentNode) switchToNextBlock(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 标记当前会话块完成
	if state.CurrentBlockID != "" {
		state.CompletedBlocks = append(state.CompletedBlocks, state.CurrentBlockID)
		delete(state.ActiveBlocks, state.CurrentBlockID)
	}

	// 查找下一个活跃会话块
	for blockID, block := range state.ActiveBlocks {
		state.CurrentBlockID = blockID
		state.CurrentDomain = block.Domain
		state.DomainGoal = block.Goal
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = block.Agents[0]
		return state, nil
	}

	// 全部完成，更新会话总结
	n.updateSessionSummary(state)
	state.CurrentBlockID = ""
	state.CurrentDomain = ""
	state.DomainGoal = ""
	state.NextAction = types.ActionFinish
	state.Reason = "all session blocks completed"
	return state, nil
}

// updateSessionSummary 更新会话总结
func (n *MetaAgentNode) updateSessionSummary(state *types.ThreeLayerState) {
	var parts []string
	parts = append(parts, fmt.Sprintf("会话[%s]已执行%d步", state.SessionID, n.stepCount))
	parts = append(parts, fmt.Sprintf("完成领域: %v", state.CompletedBlocks))
	parts = append(parts, fmt.Sprintf("活跃领域: %d个", len(state.ActiveBlocks)))
	if state.CurrentDomain != "" {
		parts = append(parts, fmt.Sprintf("当前领域: %s", state.CurrentDomain))
	}
	state.SessionSummary = strings.Join(parts, "; ")
}

// DomainInfo 领域信息
type DomainInfo struct {
	Name string
	Goal string
}

// analyzeDomains 分析用户目标，确定需要的领域
func (n *MetaAgentNode) analyzeDomains(state *types.ThreeLayerState) []DomainInfo {
	// 简化实现：根据关键词匹配固定角色，推断领域
	goal := state.DomainGoal
	if goal == "" {
		// 从会话总结中提取
		goal = state.SessionSummary
	}

	var domains []DomainInfo

	// 关键词匹配
	if strings.Contains(goal, "商城") || strings.Contains(goal, "页面") {
		domains = append(domains, DomainInfo{Name: "商城页面", Goal: goal})
	}
	if strings.Contains(goal, "购物车") || strings.Contains(goal, "购买") {
		domains = append(domains, DomainInfo{Name: "购物模块", Goal: goal})
	}
	if strings.Contains(goal, "订单") {
		domains = append(domains, DomainInfo{Name: "订单模块", Goal: goal})
	}
	if strings.Contains(goal, "用户") || strings.Contains(goal, "登录") {
		domains = append(domains, DomainInfo{Name: "用户模块", Goal: goal})
	}

	// 默认领域
	if len(domains) == 0 {
		domains = append(domains, DomainInfo{Name: "通用", Goal: goal})
	}

	return domains
}
