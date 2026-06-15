package graph

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/pkg/types"
)

// Node 定义 Graph 节点接口
type Node interface {
	Invoke(ctx context.Context, state *State) (*State, error)
	Name() string
}

// CompiledGraph 编译后的可执行图
type CompiledGraph struct {
	nodes      map[string]Node
	edges      map[string]map[string]bool
	startNode  string
}

// GraphBuilder 图构建器
type GraphBuilder struct {
	nodes     map[string]Node
	edges     map[string]map[string]bool
	startNode string
}

// NewGraphBuilder 创建图构建器
func NewGraphBuilder() *GraphBuilder {
	return &GraphBuilder{
		nodes: make(map[string]Node),
		edges: make(map[string]map[string]bool),
	}
}

// AddNode 添加节点
func (b *GraphBuilder) AddNode(node Node) {
	b.nodes[node.Name()] = node
}

// AddEdge 添加边
func (b *GraphBuilder) AddEdge(from, to string) {
	if b.edges[from] == nil {
		b.edges[from] = make(map[string]bool)
	}
	b.edges[from][to] = true
}

// SetStartNode 设置起始节点
func (b *GraphBuilder) SetStartNode(name string) {
	b.startNode = name
}

// Compile 编译图
func (b *GraphBuilder) Compile() (*CompiledGraph, error) {
	if b.startNode == "" {
		return nil, fmt.Errorf("start node not set")
	}
	if _, ok := b.nodes[b.startNode]; !ok {
		return nil, fmt.Errorf("start node %s not found", b.startNode)
	}
	return &CompiledGraph{
		nodes:     b.nodes,
		edges:     b.edges,
		startNode: b.startNode,
	}, nil
}

// Invoke 执行图
func (g *CompiledGraph) Invoke(ctx context.Context, state *State) (*State, error) {
	current := g.startNode
	stepCount := 0
	maxSteps := 100 // 防止无限循环

	for {
		if stepCount >= maxSteps {
			return nil, fmt.Errorf("max steps exceeded")
		}
		stepCount++

		node, ok := g.nodes[current]
		if !ok {
			return nil, fmt.Errorf("node %s not found", current)
		}

		newState, err := node.Invoke(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("node %s failed: %w", current, err)
		}
		state = newState

		// 检查是否完成
		if state.IsFinished() {
			return state, nil
		}

		// 确定下一个节点
		next := g.determineNext(current, state)
		if next == "" {
			return state, nil // 无后续节点，结束
		}
		current = next
	}
}

// determineNext 根据状态确定下一个节点
func (g *CompiledGraph) determineNext(current string, state *State) string {
	// 使用 Router 节点的决策
	if current == "Router" {
		switch state.NextAction {
		case types.ActionContinue:
			// 继续当前 Agent
			if state.CurrentAgent != "" {
				return state.CurrentAgent
			}
			return "AgentExecutor"
		case types.ActionSwitch:
			// 切换到目标 Agent
			if state.TargetAgent != "" {
				return state.TargetAgent
			}
			return "AgentExecutor"
		case types.ActionEscalate:
			return "EscalationHandler"
		case types.ActionFinish:
			return "Sinker"
		}
	}

	// 默认流程
	if current == "AgentExecutor" || current == state.CurrentAgent {
		return "Validator"
	}
	if current == "Validator" {
		return "WorkspaceUpdater"
	}
	if current == "WorkspaceUpdater" {
		return "Router"
	}
	if current == "EscalationHandler" {
		return "Router"
	}

	// 查找预定义边
	if nextNodes, ok := g.edges[current]; ok {
		for next := range nextNodes {
			return next
		}
	}

	return ""
}

// BuildDefaultGraph 构建默认多 Agent 图
func BuildDefaultGraph(
	router Node,
	agentExecutor Node,
	validator Node,
	workspaceUpdater Node,
	escalationHandler Node,
	sinker Node,
) (*CompiledGraph, error) {
	builder := NewGraphBuilder()
	builder.AddNode(router)
	builder.AddNode(agentExecutor)
	builder.AddNode(validator)
	builder.AddNode(workspaceUpdater)
	builder.AddNode(escalationHandler)
	builder.AddNode(sinker)

	builder.SetStartNode("Router")

	// Router -> 各节点 (运行时决定)
	builder.AddEdge("Router", "AgentExecutor")
	builder.AddEdge("Router", "EscalationHandler")
	builder.AddEdge("Router", "Sinker")

	// AgentExecutor -> Validator
	builder.AddEdge("AgentExecutor", "Validator")

	// Validator -> WorkspaceUpdater
	builder.AddEdge("Validator", "WorkspaceUpdater")

	// WorkspaceUpdater -> Router (循环)
	builder.AddEdge("WorkspaceUpdater", "Router")

	// EscalationHandler -> Router
	builder.AddEdge("EscalationHandler", "Router")

	return builder.Compile()
}
