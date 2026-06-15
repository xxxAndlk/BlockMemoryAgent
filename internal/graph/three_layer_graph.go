package graph

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/pkg/types"
)

// ThreeLayerNode 三层架构节点接口
type ThreeLayerNode interface {
	Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error)
	Name() string
}

// ThreeLayerGraph 三层图
type ThreeLayerGraph struct {
	nodes     map[string]ThreeLayerNode
	registry  *RoleRegistry
	factory   *RoleFactory
}

// ThreeLayerGraphBuilder 三层图构建器
type ThreeLayerGraphBuilder struct {
	nodes    map[string]ThreeLayerNode
	registry *RoleRegistry
	factory  *RoleFactory
}

// NewThreeLayerGraphBuilder 创建三层图构建器
func NewThreeLayerGraphBuilder(registry *RoleRegistry, factory *RoleFactory) *ThreeLayerGraphBuilder {
	return &ThreeLayerGraphBuilder{
		nodes:    make(map[string]ThreeLayerNode),
		registry: registry,
		factory:  factory,
	}
}

// SetFactory 设置角色工厂
func (b *ThreeLayerGraphBuilder) SetFactory(factory *RoleFactory) {
	b.factory = factory
}

// AddNode 添加节点
func (b *ThreeLayerGraphBuilder) AddNode(node ThreeLayerNode) {
	b.nodes[node.Name()] = node
}

// Build 构建图
func (b *ThreeLayerGraphBuilder) Build() *ThreeLayerGraph {
	return &ThreeLayerGraph{
		nodes:     b.nodes,
		registry:  b.registry,
		factory:   b.factory,
	}
}

// Invoke 执行三层图
func (g *ThreeLayerGraph) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	current := "MetaAgent"
	stepCount := 0
	maxSteps := 200 // 防止无限循环

	for {
		if stepCount >= maxSteps {
			return nil, fmt.Errorf("max steps exceeded")
		}
		stepCount++

		node, ok := g.nodes[current]
		if !ok {
			// 尝试动态创建实例节点
			node = g.resolveInstanceNode(current)
			if node == nil {
				return nil, fmt.Errorf("node %s not found", current)
			}
		}

		newState, err := node.Invoke(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("node %s failed: %w", current, err)
		}
		state = newState

		// 检查是否完成
		if state.NextAction == types.ActionFinish {
			return state, nil
		}

		// 确定下一个节点
		next := g.determineNext(current, state)
		if next == "" {
			return state, nil
		}
		current = next
	}
}

// resolveInstanceNode 根据实例ID解析节点（动态创建）
func (g *ThreeLayerGraph) resolveInstanceNode(instID string) ThreeLayerNode {
	inst := g.registry.GetInstance(instID)
	if inst == nil {
		return nil
	}

	switch inst.Type {
	case types.RoleTypeDomain:
		node := NewDomainAgentNode(instID, g.registry, g.factory)
		g.nodes[instID] = node
		return node
	case types.RoleTypeSubDomain:
		node := NewSubDomainAgentNode(instID, g.registry, g.factory)
		g.nodes[instID] = node
		return node
	case types.RoleTypeFixed, types.RoleTypeDynamic:
		node := NewAssistantNode(instID, g.registry, nil)
		g.nodes[instID] = node
		return node
	default:
		return nil
	}
}

// determineNext 三层调度逻辑
func (g *ThreeLayerGraph) determineNext(current string, state *types.ThreeLayerState) string {
	switch current {
	case "MetaAgent":
		return g.metaAgentNext(state)
	case "DomainAgent":
		return g.domainAgentNext(state)
	case "Assistant":
		return g.assistantNext(state)
	default:
		// 实例节点（DomainAgent / SubDomainAgent / Assistant）
		if inst := g.registry.GetInstance(current); inst != nil {
			switch inst.Type {
			case types.RoleTypeDomain:
				return g.domainAgentNext(state)
			case types.RoleTypeSubDomain:
				return g.subDomainAgentNext(state)
			case types.RoleTypeFixed, types.RoleTypeDynamic:
				return g.assistantNext(state)
			}
		}
	}
	return ""
}

// metaAgentNext MetaAgent的下一步
func (g *ThreeLayerGraph) metaAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		// 切换到目标DomainAgent
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
		// 没有目标，检查是否有活跃会话块
		for blockID := range state.ActiveBlocks {
			block := state.ActiveBlocks[blockID]
			if len(block.Agents) > 0 {
				return block.Agents[0]
			}
		}
	case types.ActionEscalate:
		return "EscalationHandler"
	case types.ActionFinish:
		return "Sinker"
	case types.ActionContinue:
		// 继续MetaAgent自身（循环）
		return "MetaAgent"
	}
	return "MetaAgent"
}

// domainAgentNext DomainAgent的下一步
func (g *ThreeLayerGraph) domainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		// 切换到目标助手
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		// 助手调用完成，返回MetaAgent
		if !state.IsCalling() {
			return "MetaAgent"
		}
		// 继续当前助手
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID
		}
	}
	// 默认返回MetaAgent
	return "MetaAgent"
}

// subDomainAgentNext SubDomainAgent的下一步
func (g *ThreeLayerGraph) subDomainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		if !state.IsCalling() {
			return "MetaAgent"
		}
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID
		}
	}
	return "MetaAgent"
}

// assistantNext Assistant的下一步
func (g *ThreeLayerGraph) assistantNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		// 返回调用者（DomainAgent 或 SubDomainAgent）
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		// 继续调用者
		return "MetaAgent"
	}
	return "MetaAgent"
}

// GetNode 获取指定名称的节点（公共方法）
func (g *ThreeLayerGraph) GetNode(name string) (ThreeLayerNode, bool) {
	node, ok := g.nodes[name]
	return node, ok
}

// ResolveInstanceNode 根据实例ID动态解析节点（公共方法）
func (g *ThreeLayerGraph) ResolveInstanceNode(instID string) ThreeLayerNode {
	return g.resolveInstanceNode(instID)
}

// DetermineNext 确定下一个节点（公共方法）
func (g *ThreeLayerGraph) DetermineNext(current string, state *types.ThreeLayerState) string {
	return g.determineNext(current, state)
}

// BuildThreeLayerGraph 构建默认三层图
func BuildThreeLayerGraph(
	metaAgent ThreeLayerNode,
	escalation ThreeLayerNode,
	sinker ThreeLayerNode,
	registry *RoleRegistry,
	factory *RoleFactory,
) *ThreeLayerGraph {
	builder := NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	return builder.Build()
}
