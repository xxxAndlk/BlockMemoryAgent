package graph

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/watchdog"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// DomainAgentNode Layer 2: 会话块Agent / 领域上下文管理器。
//
// 职责：
//   - 接收 MetaAgent 派发的领域目标，装配领域 Skill 子集
//   - 调 analyzeTasks 拆解子任务（LLM 优先，规则回退）
//   - 串行派发 Assistant 执行子任务（含 3 次指数退避重试）
//   - 汇总结果后标记块完成，回控制权给 MetaAgent
//
// 并发安全：节点字段在构造后只读；实例状态由 registry 内部锁保护。
// dispatchAssistantsParallel 路径（当前未启用）用 sync.Mutex 保护结果 map。
type DomainAgentNode struct {
	BaseAgentNode
	name            string                // 节点名（固定 "DomainAgent"）
	instID          string                // 本实例ID
	blockMemory     BlockMemoryStore      // 块记忆存储（特性3：向量检索归档）
	recalledMemory  string                // 本次 Invoke 检索到的相似块记忆文本（注入 analyzeTasks）
	recallAttempted bool                  // 是否已尝试检索块记忆（无论命中与否）；用于 analyzeTasks 区分"未检索"与"检索未命中"
	memCallback     MemoryCallbackHandler // 记忆回调处理器（驱动 Episode 写入与快照保存）
	snapshotMgr     AgentSnapshotManager  // Agent 快照管理器（启动加载/结束保存）
	snapshot        *types.AgentSnapshot  // 本次 Invoke 加载到的快照
	stepCounter     atomic.Int64          // 单调步骤计数器，作为 Episode/Snapshot 幂等键
}

// NewDomainAgentNode 创建领域Agent节点。
//
// 参数：
//   - instID：实例ID
//   - registry：角色注册表
//   - factory：角色工厂
//
// 返回：装配好的节点；modelFactory/toolCallback/runtime/progress 通过 Set* 后置注入。
func NewDomainAgentNode(instID string, registry *RoleRegistry, factory *RoleFactory) *DomainAgentNode {
	return &DomainAgentNode{
		BaseAgentNode: BaseAgentNode{
			llmTracker: model.NewLLMCallTracker(),
			registry:   registry,
			factory:    factory,
			agentLabel: func() string {
				if inst := registry.GetInstance(instID); inst != nil && inst.Domain != "" {
					return "DomainAgent[" + inst.Domain + "]"
				}
				return "DomainAgent"
			},
		},
		name:   "DomainAgent", // 节点名固定
		instID: instID,        // 绑定实例
	}
}

// SetBlockMemoryStore 注入块记忆存储（特性3）。
// nil 时 DomainAgent 不做向量检索与归档，仅退化为无记忆模式。
func (n *DomainAgentNode) SetBlockMemoryStore(s BlockMemoryStore) {
	n.blockMemory = s
}

// SetMemoryCallbackHandler 注入记忆回调处理器。
// nil 时不触发 Episode 写入与快照保存。
func (n *DomainAgentNode) SetMemoryCallbackHandler(h MemoryCallbackHandler) {
	n.memCallback = h
}

// SetAgentSnapshotManager 注入 Agent 快照管理器。
// nil 时不加载/保存快照。
func (n *DomainAgentNode) SetAgentSnapshotManager(s AgentSnapshotManager) {
	n.snapshotMgr = s
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口。
func (n *DomainAgentNode) Name() string {
	return n.name
}

// truncateString 截断字符串并加省略号。
// 统一委托给 textutil.TruncateBytes。
func truncateString(s string, n int) string {
	return textutil.TruncateBytes(s, n, "...")
}

// agentConfig 返回当前 Runtime 注入的 AgentConfig；未注入时使用与 config.applyDefaults 一致的默认值。
func (n *DomainAgentNode) agentConfig() *config.AgentConfig {
	if n.rt != nil && n.rt.AgentCfg != nil {
		return n.rt.AgentCfg
	}
	return &config.AgentConfig{
		DomainMemoryRecallMaxChars:  300,
		DomainMemoryContextMaxChars: 500,
		DomainResultLogMaxChars:     200,
	}
}

// InstanceID 返回实例ID。
// 用于图调度循环在路由表里查找对应的动态节点。
func (n *DomainAgentNode) InstanceID() string {
	return n.instID
}

// Invoke 执行领域Agent逻辑。
//
// 职责：
//   - 标记实例活跃
//   - 装配领域 Skill 子集（ensureSkillSet）
//   - 调 analyzeTasks 拆解子任务
//   - 判断是否拆分子领域（当前禁用）
//   - 串行派发 Assistant 执行待处理任务
//   - 汇总结果、标记块完成、回控制权给 MetaAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改）
//
// 返回：更新后的 state；实例缺失或当前块缺失时返回错误或退化 ActionContinue。
//
// 副作用：更新实例状态；修改 state.ActiveBlocks、NextAction。
func (n *DomainAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	inst, err := n.prepareContext(ctx, state)
	if err != nil {
		return nil, err
	}

	pendingTasks, done, err := n.analyzeAndPlan(ctx, state, inst)
	if done {
		return state, err
	}

	results := n.dispatchAndCollect(ctx, state, inst, pendingTasks)
	return n.finalizeBlock(ctx, state, inst, results)
}

// prepareContext 负责 DomainAgent 启动阶段的上下文准备：
// 实例校验、状态广播、Watchdog、记忆回调/快照加载、Skill 装配、块记忆召回。
func (n *DomainAgentNode) prepareContext(ctx context.Context, state *types.ThreeLayerState) (*types.RoleInstance, error) {
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		if n.memCallback != nil {
			n.memCallback.OnError(ctx, n.instID, state.SessionID, fmt.Errorf("domain agent instance %s not found", n.instID))
		}
		return nil, fmt.Errorf("domain agent instance %s not found", n.instID)
	}

	n.emit(ctx, "think", fmt.Sprintf("DomainAgent 启动，领域目标: %s", state.DomainGoal))
	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusActive)

	// DomainAgent 侧 Watchdog：用真实 LLM token 总量评估上下文规模。
	n.runDomainWatchdog(ctx, state)

	// 记忆回调：广播 DomainAgent 进入 ACTIVE，并尝试加载历史快照
	if n.memCallback != nil {
		n.memCallback.OnStart(ctx, n.instID, state.SessionID)
	}
	if n.snapshotMgr != nil {
		if snap, err := n.snapshotMgr.Load(ctx, n.instID, state.SessionID); err == nil && snap != nil {
			n.snapshot = snap
			n.emit(ctx, "think", fmt.Sprintf("已加载历史快照，未决问题 %d 个", len(snap.OpenIssues)))
		}
	}

	// 装配领域 Skill 子集（如未装配）
	n.ensureSkillSet(ctx, inst, state)
	if n.rt != nil && n.rt.Skills != nil {
		if set := n.rt.Skills.GetForAgent(n.instID); set != nil && len(set.Skills) > 0 {
			ids := make([]string, 0, len(set.Skills))
			for _, s := range set.Skills {
				ids = append(ids, s.SkillID)
			}
			n.emit(ctx, "think", "已装配 Skill 子集: "+strings.Join(ids, ", "))
		}
	}

	// 检索相似块记忆，注入 analyzeTasks 作为上下文
	n.recalledMemory = ""
	n.recallAttempted = false
	if n.blockMemory != nil {
		n.recallAttempted = true
		if recalled, err := n.blockMemory.SearchBlockMemory(ctx, state.CurrentDomain, state.DomainGoal, 3); err == nil && recalled != "" {
			n.recalledMemory = recalled
			n.emit(ctx, "think", "已检索到历史相似块记忆，将作为上下文注入任务拆解")
			n.emit(ctx, "memory_recall", truncateString(recalled, n.agentConfig().DomainMemoryRecallMaxChars))
			if log := n.sessionLogger(ctx); log != nil {
				log.Event(ctx, "memory_recall", fmt.Sprintf("block_memory query=%s hits=1", state.DomainGoal), map[string]any{
					"query":  state.DomainGoal,
					"hits":   1,
					"domain": state.CurrentDomain,
				})
				log.Event(ctx, "memory_inject", "注入历史块记忆到任务拆解", map[string]any{
					"context": truncateString(recalled, n.agentConfig().DomainMemoryContextMaxChars),
				})
			}
		} else if err == nil {
			if log := n.sessionLogger(ctx); log != nil {
				log.Event(ctx, "memory_recall", fmt.Sprintf("block_memory query=%s hits=0", state.DomainGoal), map[string]any{
					"query":  state.DomainGoal,
					"hits":   0,
					"domain": state.CurrentDomain,
				})
			}
		}
	}

	return inst, nil
}

// analyzeAndPlan 拆解任务、生成计划、处理子领域拆分，并返回待处理任务列表。
// 若返回 done==true，表示当前调用已直接终了（块缺失 / 子领域拆分 / 无待处理任务），调用方应直接返回 state。
func (n *DomainAgentNode) analyzeAndPlan(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance) (pendingTasks []string, done bool, err error) {
	tasks := n.analyzeTasks(ctx, state)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		state.NextAction = enums.ActionContinue
		n.finish(ctx, state)
		return nil, true, nil
	}

	mergeSessionMetaMemory(state, block)
	applyMailboxToBlock(state, block, n.rt, n.instID)
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}

	// Plan-and-Execute：多任务且启用时生成结构化计划
	if block.Plan == nil && planEnabledFromRT(n.rt) && len(tasks) > 1 {
		block.Plan = n.generatePlan(ctx, state, tasks)
	}

	// 判断是否需要拆分为子领域（当前默认禁用）
	if n.shouldSplitToSubDomains(ctx, state, tasks) {
		n.emit(ctx, "intend", fmt.Sprintf("领域较复杂（%d 个子任务），拆分为子领域并行处理", len(tasks)))
		newState, splitErr := n.handleSubDomainSplit(ctx, state, inst)
		if splitErr == nil {
			n.finish(ctx, newState)
		}
		return nil, true, splitErr
	}

	// 确定待处理任务列表
	if block.Plan != nil {
		pendingTasks = block.Plan.PendingGoals()
	} else {
		for _, task := range tasks {
			if _, done := block.TaskResults[task]; done {
				continue
			}
			pendingTasks = append(pendingTasks, task)
		}
	}

	// 无待处理任务：汇总结果、标记完成
	if len(pendingTasks) == 0 {
		n.emit(ctx, "think", "所有子任务已完成，汇总结果")
		n.summarizeResults(state)
		n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusDone)
		block.Status = enums.BlockStatusCompleted
		state.NextAction = enums.ActionContinue
		n.finish(ctx, state)
		return nil, true, nil
	}

	return pendingTasks, false, nil
}

// dispatchAndCollect 串行派发助手执行待处理任务，并将结果合并到 block。
func (n *DomainAgentNode) dispatchAndCollect(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, pendingTasks []string) map[string]*types.AgentResult {
	n.emit(ctx, "intend", fmt.Sprintf("派发 %d 个助手任务（串行）: %s", len(pendingTasks), strings.Join(pendingTasks, "; ")))
	results := n.dispatchAssistantsSerial(ctx, state, inst, pendingTasks)

	block := state.ActiveBlocks[state.CurrentBlockID]
	if block.TaskResults == nil {
		block.TaskResults = make(map[string]string)
	}
	for task, result := range results {
		if result == nil {
			continue
		}
		block.TaskResults[task] = result.SummaryForUser
		if result.MemoryForMeta != "" {
			block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
				Timestamp: time.Now(),
				Source:    n.instID,
				Content:   result.MemoryForMeta,
				Tags:      []string{"summary"},
			})
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

	return results
}

// finalizeBlock 构建块结果、执行可选自测、记录日志并标记完成。
func (n *DomainAgentNode) finalizeBlock(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, results map[string]*types.AgentResult) (*types.ThreeLayerState, error) {
	block := state.ActiveBlocks[state.CurrentBlockID]

	var combinedSummary []string
	var combinedMemory []string
	for task, result := range results {
		if result == nil {
			continue
		}
		combinedSummary = append(combinedSummary, fmt.Sprintf("%s: %s", task, result.SummaryForUser))
		if result.MemoryForMeta != "" {
			combinedMemory = append(combinedMemory, result.MemoryForMeta)
		}
	}

	block.Result = buildBlockResult(inst.Domain, block, combinedSummary, combinedMemory)
	n.summarizeResults(state)

	// P3-2：领域级自测（默认关闭）
	if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.DomainSelfTestEnabled && block.Result != nil && block.Result.Error == "" {
		domainTask := fmt.Sprintf("领域[%s]目标: %s", inst.Domain, block.Goal)
		if testResult, err := runSelfTestAssistant(ctx, n.modelFactory, n.toolCallback, n.rt, state, domainTask, block.Result, n.progress, "DomainTester", n.llmTracker); err == nil && testResult != nil {
			block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
				Timestamp: time.Now(),
				Source:    n.instID,
				Content:   testResult.SummaryForUser,
				Tags:      []string{"test_report"},
			})
			for _, fact := range testResult.Facts {
				if strings.TrimSpace(fact) == "" {
					continue
				}
				block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
					Timestamp: time.Now(),
					Source:    n.instID,
					Content:   fact,
					Tags:      []string{"fact", "test"},
				})
			}
		}
	}

	if log := n.sessionLogger(ctx); log != nil {
		log.Event(ctx, "result", fmt.Sprintf("domain=%s summary=%s", state.CurrentDomain, truncateString(state.Reason, n.agentConfig().DomainResultLogMaxChars)), map[string]any{
			"domain":  state.CurrentDomain,
			"summary": state.Reason,
		})
	}
	n.registry.UpdateInstanceStatus(n.instID, enums.RoleStatusDone)
	block.Status = enums.BlockStatusCompleted
	state.NextAction = enums.ActionContinue
	n.finish(ctx, state)
	return state, nil
}

// runDomainWatchdog DomainAgent 侧上下文看门狗：用真实 LLM token 总量评估。
//
// 职责：
//   - 取本 DomainAgent 的 llmTracker.TokenTotals() 作为上下文规模代理
//   - 调 runtime.Watchdog.Check 评估等级
//   - LevelCompress：推送压缩提示事件 + 写日志，提示 MetaAgent 后续可压缩
//   - LevelEvict：emit 警告 + 写日志；不强制中断（避免破坏写文件等关键动作）
//
// 与 MetaAgent.runWatchdog 的区别：
//   - MetaAgent.runWatchdog 用 block.TaskResults 文本估算，严重低估（塔防事故从未触发）
//   - DomainAgent.runDomainWatchdog 用真实 LLM token 累计值，准确反映上下文消耗
//
// 设计意图：弥补原 Watchdog 仅在 MetaAgent 侧、且用文本估算的两大缺陷。
func (n *DomainAgentNode) runDomainWatchdog(ctx context.Context, state *types.ThreeLayerState) {
	if n.rt == nil || n.rt.Watchdog == nil || n.llmTracker == nil {
		return
	}
	inputTokens, outputTokens := n.llmTracker.TokenTotals()
	totalTokens := inputTokens + outputTokens
	if totalTokens <= 0 {
		return // 还没调过 LLM，跳过
	}
	// 用真实 token 数构造上下文文本喂给 Watchdog.Check。
	// Check 内部用 Estimator(bytes/4+1) 估算，会低估真实 token；直接拼一个
	// 长度为 totalTokens*4 的占位串让 Estimator 输出 ≈ totalTokens，保证等级判定准确。
	placeholder := make([]byte, totalTokens*4)
	d := n.rt.Watchdog.Check(n.instID, string(placeholder))
	switch d.Level {
	case watchdog.LevelCompress:
		n.emit(ctx, "think",
			fmt.Sprintf("Watchdog(COMPRESS): DomainAgent %s token=%d (in=%d,out=%d) ≥ soft=%d，建议压缩: %s",
				n.instID, totalTokens, inputTokens, outputTokens, d.Tokens, d.Reason))
		if log := n.sessionLogger(ctx); log != nil {
			log.Event(ctx, "watchdog_compress", fmt.Sprintf("domain=%s tokens=%d soft=%d", state.CurrentDomain, totalTokens, d.Tokens), map[string]any{
				"agent":         n.instID,
				"domain":        state.CurrentDomain,
				"tokens":        totalTokens,
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"soft_limit":    d.Tokens,
			})
		}
	case watchdog.LevelEvict:
		n.emit(ctx, "wait",
			fmt.Sprintf("Watchdog(EVICT): DomainAgent %s token=%d ≥ hard=%d，已转警告: %s",
				n.instID, totalTokens, d.Tokens, d.Reason))
		if log := n.sessionLogger(ctx); log != nil {
			log.Event(ctx, "watchdog_evict", fmt.Sprintf("domain=%s tokens=%d hard=%d", state.CurrentDomain, totalTokens, d.Tokens), map[string]any{
				"agent":      n.instID,
				"domain":     state.CurrentDomain,
				"tokens":     totalTokens,
				"hard_limit": d.Tokens,
			})
		}
	}
}

// finish 在 DomainAgent 成功结束前触发记忆回调。
// 由 CallbackHandler 内部负责 Episode 写入与快照保存；此处只发起回调。
func (n *DomainAgentNode) finish(ctx context.Context, state *types.ThreeLayerState) {
	if n.memCallback != nil {
		stepCount := int(n.stepCounter.Add(1))
		n.memCallback.OnEnd(ctx, n.instID, state.SessionID, "DomainAgent完成", state.Reason, stepCount)
	}
}
