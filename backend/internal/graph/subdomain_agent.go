package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SubDomainAgentNode Layer 2.5: 子领域Agent。
//
// 职责：
//   - 接收父 DomainAgent 派发的领域目标
//   - 将目标拆解为 2-4 个子任务
//   - 并行调度 Assistant 执行子任务
//   - 汇总结果后弹出调用栈，返回父 DomainAgent
//
// 设计意图：处理领域内可并行化的独立模块（如前端页面头部/列表/底部）。
//
// 现状（P2-03）：实测中本层在 DomainAgent.shouldSplitToSubDomains 已被运行时禁用
//（state.EnableSubdomain 默认为 false），因此当前二进制中 SubDomainAgent 不会被主动创建。
// 代码仍保留以备后续按需启用；彻底删除或加 build tag 需要同步修改 graph_resolve /
// three_layer_graph / graph_routes 等多处类型断言，属于较大范围重构，暂作为架构债务保留。
//
// 并发安全：节点字段在构造后只读；实例状态由 registry 内部锁保护。
type SubDomainAgentNode struct {
	BaseAgentNode
	name        string                // 节点名（固定 "SubDomainAgent"）
	instID      string                // 本实例ID
	memCallback MemoryCallbackHandler // 记忆回调处理器（驱动 Episode 写入与快照保存）
	snapshotMgr AgentSnapshotManager  // Agent 快照管理器（启动加载/结束保存）
	stepCounter atomic.Int64          // 单调步骤计数器，作为 Episode/Snapshot 幂等键
}

// NewSubDomainAgentNode 创建子领域Agent节点。
func NewSubDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *SubDomainAgentNode {
	return &SubDomainAgentNode{
		BaseAgentNode: BaseAgentNode{
			llmTracker: model.NewLLMCallTracker(),
			registry:   registry,
			factory:    factory,
			agentLabel: func() string {
				if inst := registry.GetInstance(instID); inst != nil && inst.Domain != "" {
					return "SubDomainAgent[" + inst.Domain + "]"
				}
				return "SubDomainAgent"
			},
		},
		name:   "SubDomainAgent",
		instID: instID,
	}
}

// SetMemoryCallbackHandler 注入记忆回调处理器。
func (n *SubDomainAgentNode) SetMemoryCallbackHandler(h MemoryCallbackHandler) {
	n.memCallback = h
}

// SetAgentSnapshotManager 注入 Agent 快照管理器。
func (n *SubDomainAgentNode) SetAgentSnapshotManager(s AgentSnapshotManager) {
	n.snapshotMgr = s
}

// Name 返回节点名称。
func (n *SubDomainAgentNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID。
func (n *SubDomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行子领域Agent逻辑。
//
// 职责：
//   - 标记实例活跃
//   - 调 analyzeSubTasks 拆解子任务（LLM 优先，规则回退）
//   - 过滤已完成任务后并行派发 Assistant
//   - 汇总结果并弹出调用栈，返回父 DomainAgent
func (n *SubDomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		if n.memCallback != nil {
			n.memCallback.OnError(ctx, n.instID, state.SessionID, fmt.Errorf("subdomain agent instance %s not found", n.instID))
		}
		return nil, fmt.Errorf("subdomain agent instance %s not found", n.instID)
	}

	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusActive)

	if n.memCallback != nil {
		n.memCallback.OnStart(ctx, n.instID, state.SessionID)
	}
	if n.snapshotMgr != nil {
		if snap, err := n.snapshotMgr.Load(ctx, n.instID, state.SessionID); err == nil && snap != nil {
			n.emit(ctx, "think", fmt.Sprintf("已加载历史快照，未决问题 %d 个", len(snap.OpenIssues)))
		}
	}

	tasks := n.analyzeSubTasks(ctx, state)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = enums.ActionContinue
		n.finish(ctx, state)
		return state, nil
	}
	// R8: 启动时合并会话级 MetaMemory，避免跨域失忆
	mergeSessionMetaMemory(state, block)
	// R7: 消费其他块通过 Mailbox 发来的跨域通知，注入块记忆
	applyMailboxToBlock(state, block, n.rt, n.instID)
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	var pendingTasks []string
	for _, task := range tasks {
		if _, done := block.TaskResults[task]; done {
			continue
		}
		pendingTasks = append(pendingTasks, task)
	}

	var combinedSummary []string
	var combinedMemory []string
	if len(pendingTasks) > 0 {
		results := n.dispatchAssistantsParallel(ctx, state, inst, pendingTasks)

		var mu sync.Mutex
		mu.Lock()
		for task, result := range results {
			if result == nil {
				continue
			}
			block.TaskResults[task] = result.SummaryForUser
			combinedSummary = append(combinedSummary, fmt.Sprintf("%s: %s", task, result.SummaryForUser))
			if result.MemoryForMeta != "" {
				block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
					Timestamp: time.Now(),
					Source:    n.instID,
					Content:   result.MemoryForMeta,
					Tags:      []string{"summary"},
				})
				combinedMemory = append(combinedMemory, result.MemoryForMeta)
			}
			for _, fact := range result.Facts {
				if strings.TrimSpace(fact) == "" {
					continue
				}
				block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
					Timestamp: time.Now(),
					Source:    n.instID,
					Content:   fact,
					Tags:      []string{"fact"},
				})
			}
		}
		mu.Unlock()
	}

	state.PopCallStack()
	block.Result = buildBlockResult(inst.Domain, block, combinedSummary, combinedMemory)
	n.summarizeResults(state)
	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusDone)

	state.NextAction = enums.ActionSwitch
	if inst.ParentID != "" {
		state.TargetRoleID = inst.ParentID
		n.registry.UpdateInstanceStatus(inst.ParentID, enums.RoleStatusActive)
	} else {
		state.TargetRoleID = "MetaAgent"
	}

	n.finish(ctx, state)
	return state, nil
}

// finish 在 SubDomainAgent 成功结束前触发记忆回调。
func (n *SubDomainAgentNode) finish(ctx context.Context, state *types.ThreeLayerState) {
	if n.memCallback != nil {
		stepCount := int(n.stepCounter.Add(1))
		n.memCallback.OnEnd(ctx, n.instID, state.SessionID, "SubDomainAgent完成", state.Reason, stepCount)
	}
}
