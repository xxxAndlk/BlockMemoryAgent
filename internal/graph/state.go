package graph

import (
	"github.com/blockmemory/agent/pkg/types"
)

// State 包装 types.GraphState，提供便捷方法
type State struct {
	*types.GraphState
}

// NewState 创建新的 GraphState
func NewState(topicID, goal string) *State {
	return &State{
		GraphState: &types.GraphState{
			TopicID:      topicID,
			TopicGoal:    goal,
			Constraints:  make(map[string]string),
			EventQueue:   make([]*types.Event, 0),
			AgentOutputs: make(map[string]*types.AgentOutput),
			SnapshotRefs: make(map[string]string),
			NextAction:   types.ActionContinue,
		},
	}
}

// SetAgentOutput 设置 Agent 输出
func (s *State) SetAgentOutput(agentID string, output *types.AgentOutput) {
	if s.AgentOutputs == nil {
		s.AgentOutputs = make(map[string]*types.AgentOutput)
	}
	s.AgentOutputs[agentID] = output
}

// GetAgentOutput 获取 Agent 输出
func (s *State) GetAgentOutput(agentID string) *types.AgentOutput {
	if s.AgentOutputs == nil {
		return nil
	}
	return s.AgentOutputs[agentID]
}

// AddEvent 添加事件到队列
func (s *State) AddEvent(event *types.Event) {
	s.EventQueue = append(s.EventQueue, event)
}

// SetSnapshotRef 设置快照引用
func (s *State) SetSnapshotRef(agentID, ref string) {
	if s.SnapshotRefs == nil {
		s.SnapshotRefs = make(map[string]string)
	}
	s.SnapshotRefs[agentID] = ref
}

// GetSnapshotRef 获取快照引用
func (s *State) GetSnapshotRef(agentID string) string {
	if s.SnapshotRefs == nil {
		return ""
	}
	return s.SnapshotRefs[agentID]
}

// IsFinished 判断是否已完成
func (s *State) IsFinished() bool {
	return s.NextAction == types.ActionFinish
}

// IsEscalated 判断是否已升级
func (s *State) IsEscalated() bool {
	return s.NextAction == types.ActionEscalate
}
