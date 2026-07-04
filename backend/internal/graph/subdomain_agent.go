package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
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
// 注：实测中本层在 DomainAgent.shouldSplitToSubDomains 已被禁用，
// 保留代码以备后续按需启用。
//
// 并发安全：节点字段在构造后只读；实例状态由 registry 内部锁保护。
type SubDomainAgentNode struct {
	name         string                // 节点名（固定 "SubDomainAgent"）
	instID       string                // 本实例ID
	registry     *RoleRegistry         // 角色注册表
	factory      *RoleFactory          // 动态角色工厂（创建 Assistant）
	modelFactory *model.ModelFactory   // 模型工厂，按角色获取 ChatModel
	toolCallback ToolCallback          // 工具执行结果回调（推 UI）
	progress     ProgressCallback      // 进度回调（推思考/意图/Token）
	llmTracker   *model.LLMCallTracker // LLM 调用追踪器（统计超时/Token）
	rt           *runtime.Runtime      // Runtime 聚合体（用于读取 AgentCfg 等动态参数）
	memCallback  MemoryCallbackHandler // 记忆回调处理器（驱动 Episode 写入与快照保存）
	snapshotMgr  AgentSnapshotManager  // Agent 快照管理器（启动加载/结束保存）
	logger       *logger.Logger        // 结构化日志器（P1-2）
	stepCounter  atomic.Int64          // 单调步骤计数器，作为 Episode/Snapshot 幂等键
}

// SetRuntime 注入 Runtime。nil 时动态参数回退默认值。
func (n *SubDomainAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// NewSubDomainAgentNode 创建子领域Agent节点。
func NewSubDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *SubDomainAgentNode {
	return &SubDomainAgentNode{
		name:       "SubDomainAgent",
		instID:     instID,
		registry:   registry,
		factory:    factory,
		llmTracker: model.NewLLMCallTracker(),
	}
}

// emit 推送进度事件。
func (n *SubDomainAgentNode) emit(ctx context.Context, kind, message string) {
	if n.progress == nil {
		return
	}
	agent := "SubDomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "SubDomainAgent[" + inst.Domain + "]"
	}
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message})
}

// emitDetail 推送带详情的进度事件。
func (n *SubDomainAgentNode) emitDetail(ctx context.Context, kind, message, detail string) {
	if n.progress == nil {
		return
	}
	agent := "SubDomainAgent"
	if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
		agent = "SubDomainAgent[" + inst.Domain + "]"
	}
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message, Detail: detail})
}

// SetModelFactory 设置模型工厂。
func (n *SubDomainAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行回调。
func (n *SubDomainAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// SetProgressCallback 注入进度回调。
func (n *SubDomainAgentNode) SetProgressCallback(cb ProgressCallback) {
	n.progress = cb
}

// SetMemoryCallbackHandler 注入记忆回调处理器。
func (n *SubDomainAgentNode) SetMemoryCallbackHandler(h MemoryCallbackHandler) {
	n.memCallback = h
}

// SetAgentSnapshotManager 注入 Agent 快照管理器。
func (n *SubDomainAgentNode) SetAgentSnapshotManager(s AgentSnapshotManager) {
	n.snapshotMgr = s
}

// SetLogger 注入结构化日志器（P1-2）。
func (n *SubDomainAgentNode) SetLogger(l *logger.Logger) {
	n.logger = l
	if l != nil {
		n.llmTracker.SetRecordCallback(func(ctx context.Context, r model.CallRecord) {
			sessionID := SessionIDFromContext(ctx)
			if sessionID == "" {
				return
			}
			level := "info"
			msg := "llm_call"
			if r.Err != nil {
				level = "error"
				msg = "llm_call_error: " + r.Err.Error()
			}
			agent := "SubDomainAgent"
			if inst := n.registry.GetInstance(n.instID); inst != nil && inst.Domain != "" {
				agent = "SubDomainAgent[" + inst.Domain + "]"
			}
			l.WithSession(sessionID).WithAgent(agent).WithPhase("llm_call").
				Event(ctx, "llm_call", msg, map[string]any{
					"input_tokens":  r.InputTokens,
					"output_tokens": r.OutputTokens,
					"latency_ms":    int(r.Duration.Milliseconds()),
					"timed_out":     r.TimedOut,
					"prompt":        r.Prompt,
					"response":      r.Response,
					"level":         level,
				})
		})
	}
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
