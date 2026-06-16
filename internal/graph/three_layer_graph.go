package graph

import (
	"context"
	"fmt"
	"sync"

	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/types"
)

// ThreeLayerNode 三层架构节点接口
type ThreeLayerNode interface {
	Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error)
	Name() string
}

// ThreeLayerGraph 三层图
type ThreeLayerGraph struct {
	mu           sync.RWMutex
	nodes        map[string]ThreeLayerNode
	registry     *RoleRegistry
	factory      *RoleFactory
	modelFactory *model.ModelFactory
	toolCallback ToolCallback
}

// ThreeLayerGraphBuilder 三层图构建器
type ThreeLayerGraphBuilder struct {
	nodes        map[string]ThreeLayerNode
	registry     *RoleRegistry
	factory      *RoleFactory
	modelFactory *model.ModelFactory
	toolCallback ToolCallback
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

// SetModelFactory 设置模型工厂
func (b *ThreeLayerGraphBuilder) SetModelFactory(mf *model.ModelFactory) {
	b.modelFactory = mf
}

// SetToolCallback 设置工具执行回调
func (b *ThreeLayerGraphBuilder) SetToolCallback(cb ToolCallback) {
	b.toolCallback = cb
}

// AddNode 添加节点
func (b *ThreeLayerGraphBuilder) AddNode(node ThreeLayerNode) {
	b.nodes[node.Name()] = node
}

// Build 构建图
func (b *ThreeLayerGraphBuilder) Build() *ThreeLayerGraph {
	g := &ThreeLayerGraph{
		nodes:        b.nodes,
		registry:     b.registry,
		factory:      b.factory,
		modelFactory: b.modelFactory,
		toolCallback: b.toolCallback,
	}

	// 为已有节点注入 ModelFactory
	for _, node := range g.nodes {
		g.injectModelFactory(node)
	}

	return g
}

// injectModelFactory 为节点注入模型工厂和工具回调
func (g *ThreeLayerGraph) injectModelFactory(node ThreeLayerNode) {
	if g.modelFactory != nil {
		switch n := node.(type) {
		case *MetaAgentNode:
			n.SetModelFactory(g.modelFactory)
		case *DomainAgentNode:
			n.SetModelFactory(g.modelFactory)
		case *SubDomainAgentNode:
			n.SetModelFactory(g.modelFactory)
		}
	}
	if g.toolCallback != nil {
		switch n := node.(type) {
		case *DomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		case *SubDomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		}
	}
}

// Invoke 执行三层图
func (g *ThreeLayerGraph) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	current := "MetaAgent"
	stepCount := 0
	maxSteps := 200

	for {
		if stepCount >= maxSteps {
			return nil, fmt.Errorf("max steps exceeded")
		}
		stepCount++

		g.mu.RLock()
		node, ok := g.nodes[current]
		g.mu.RUnlock()
		if !ok {
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

		if state.NextAction == types.ActionFinish {
			return state, nil
		}

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

	var node ThreeLayerNode

	switch inst.Type {
	case types.RoleTypeDomain:
		node = NewDomainAgentNode(instID, g.registry, g.factory)
	case types.RoleTypeSubDomain:
		node = NewSubDomainAgentNode(instID, g.registry, g.factory)
	case types.RoleTypeFixed, types.RoleTypeDynamic:
		node = NewAssistantNode(instID, g.registry, nil)
	default:
		return nil
	}

	// 注入 ModelFactory
	g.injectModelFactory(node)

	g.mu.Lock()
	g.nodes[instID] = node
	g.mu.Unlock()
	return node
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

func (g *ThreeLayerGraph) metaAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
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
		return "MetaAgent"
	}
	return "MetaAgent"
}

func (g *ThreeLayerGraph) domainAgentNext(state *types.ThreeLayerState) string {
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

func (g *ThreeLayerGraph) assistantNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		return "MetaAgent"
	}
	return "MetaAgent"
}

// SetToolCallback 设置工具执行回调
func (g *ThreeLayerGraph) SetToolCallback(cb ToolCallback) {
	g.toolCallback = cb
}

// NewToolExecutor 创建带回调的工具执行器
func (g *ThreeLayerGraph) NewToolExecutor(workDir string) *ToolExecutor {
	executor := NewToolExecutor(workDir)
	if g.toolCallback != nil {
		executor.SetCallback(g.toolCallback)
	}
	return executor
}

// GetNode 获取指定名称的节点
func (g *ThreeLayerGraph) GetNode(name string) (ThreeLayerNode, bool) {
	g.mu.RLock()
	node, ok := g.nodes[name]
	g.mu.RUnlock()
	return node, ok
}

// ResolveInstanceNode 根据实例ID动态解析节点
func (g *ThreeLayerGraph) ResolveInstanceNode(instID string) ThreeLayerNode {
	return g.resolveInstanceNode(instID)
}

// DetermineNext 确定下一个节点
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
