package graph

import (
	"context"
	"strings"

	"github.com/blockmemory/agent/pkg/types"
)

// AgentRegistry Agent 能力注册表
type AgentRegistry interface {
	MatchAgent(goal string) string
	GetDependencies(agentID string) []string
	GetAllAgents() []string
}

// SimpleRegistry 简单注册表实现
type SimpleRegistry struct {
	agents map[string]*AgentInfo
}

// AgentInfo Agent 信息
type AgentInfo struct {
	ID           string
	Name         string
	Description  string
	ModuleID     string
	Keywords     []string
	Dependencies []string
}

// NewSimpleRegistry 创建简单注册表
func NewSimpleRegistry() *SimpleRegistry {
	return &SimpleRegistry{
		agents: make(map[string]*AgentInfo),
	}
}

// Register 注册 Agent
func (r *SimpleRegistry) Register(info *AgentInfo) {
	r.agents[info.ID] = info
}

// MatchAgent 根据目标匹配 Agent
func (r *SimpleRegistry) MatchAgent(goal string) string {
	goalLower := strings.ToLower(goal)
	var bestMatch string
	var bestScore int

	for id, info := range r.agents {
		score := 0
		// 关键词匹配
		for _, kw := range info.Keywords {
			if strings.Contains(goalLower, strings.ToLower(kw)) {
				score += 10
			}
		}
		// 描述匹配
		if strings.Contains(strings.ToLower(info.Description), goalLower) {
			score += 5
		}
		// 名称匹配
		if strings.Contains(goalLower, strings.ToLower(info.Name)) {
			score += 8
		}

		if score > bestScore {
			bestScore = score
			bestMatch = id
		}
	}

	if bestMatch == "" && len(r.agents) > 0 {
		// 默认返回第一个
		for id := range r.agents {
			bestMatch = id
			break
		}
	}

	return bestMatch
}

// GetDependencies 获取 Agent 依赖
func (r *SimpleRegistry) GetDependencies(agentID string) []string {
	if info, ok := r.agents[agentID]; ok {
		return info.Dependencies
	}
	return nil
}

// GetAllAgents 获取所有 Agent
func (r *SimpleRegistry) GetAllAgents() []string {
	var ids []string
	for id := range r.agents {
		ids = append(ids, id)
	}
	return ids
}

// WorkspaceClient 工作区客户端接口
type WorkspaceClient interface {
	PollEvents(ctx context.Context, topicID string, count int64) ([]*types.Event, error)
	GetLatestAgentOutput(ctx context.Context, topicID, agentID string) (*types.AgentOutput, error)
}

// RouterNode 主 Agent 路由节点
type RouterNode struct {
	name      string
	registry  AgentRegistry
	workspace WorkspaceClient
}

// NewRouterNode 创建路由节点
func NewRouterNode(registry AgentRegistry, workspace WorkspaceClient) *RouterNode {
	return &RouterNode{
		name:      "Router",
		registry:  registry,
		workspace: workspace,
	}
}

// Name 返回节点名称
func (n *RouterNode) Name() string {
	return n.name
}

// Invoke 执行路由决策
func (n *RouterNode) Invoke(ctx context.Context, state *State) (*State, error) {
	// 1. 扫描事件队列
	events, err := n.workspace.PollEvents(ctx, state.TopicID, 10)
	if err != nil {
		// 非致命错误，继续执行
		events = nil
	}

	// 2. 决策逻辑
	switch {
	// 有升级事件
	case len(events) > 0 && events[0].Type == types.EventEscalation:
		state.NextAction = types.ActionEscalate
		state.Reason = getString(events[0].Payload, "reason")

	// 初始状态，无当前 Agent
	case state.CurrentAgent == "":
		state.NextAction = types.ActionSwitch
		state.TargetAgent = n.registry.MatchAgent(state.TopicGoal)

	// 有交叉修改事件
	case hasCrossModifyEvent(events, state.CurrentAgent):
		state.NextAction = types.ActionSwitch
		state.TargetAgent = findTargetFromEvent(events)

	// 当前 Agent 任务完成
	case agentTaskComplete(state):
		if n.allAgentsComplete(state) {
			state.NextAction = types.ActionFinish
		} else {
			state.NextAction = types.ActionSwitch
			state.TargetAgent = n.findNextAgent(state)
		}

	// 默认: 继续当前 Agent
	default:
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// allAgentsComplete 检查所有 Agent 是否完成
func (n *RouterNode) allAgentsComplete(state *State) bool {
	agents := n.registry.GetAllAgents()
	if len(agents) == 0 {
		return true
	}
	// 简化: 检查是否有活跃 Event
	if len(state.EventQueue) > 0 {
		return false
	}
	// 检查是否有未验证的输出
	for _, output := range state.AgentOutputs {
		if !output.Validated {
			return false
		}
	}
	return true
}

// findNextAgent 查找下一个 Agent
func (n *RouterNode) findNextAgent(state *State) string {
	agents := n.registry.GetAllAgents()
	for _, id := range agents {
		if id == state.CurrentAgent {
			continue
		}
		if _, ok := state.AgentOutputs[id]; !ok {
			return id
		}
	}
	// 默认返回第一个
	if len(agents) > 0 {
		return agents[0]
	}
	return ""
}

// hasCrossModifyEvent 检查是否有交叉修改事件
func hasCrossModifyEvent(events []*types.Event, currentAgent string) bool {
	for _, ev := range events {
		if ev.Type == types.EventCrossModify && ev.Status == types.EventPending {
			if ev.TargetAgent == currentAgent || ev.TargetAgent == "" {
				return true
			}
		}
	}
	return false
}

// findTargetFromEvent 从事件中查找目标 Agent
func findTargetFromEvent(events []*types.Event) string {
	for _, ev := range events {
		if ev.Type == types.EventCrossModify && ev.Status == types.EventPending {
			return ev.TargetAgent
		}
	}
	return ""
}

// agentTaskComplete 检查当前 Agent 任务是否完成
func agentTaskComplete(state *State) bool {
	output := state.GetAgentOutput(state.CurrentAgent)
	return output != nil && output.Validated
}

// getString 从 map 中获取字符串
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
