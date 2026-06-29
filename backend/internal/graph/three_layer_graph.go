package graph

// 三层图状态机入口：将 MetaAgent / DomainAgent / SubDomainAgent / Assistant 串成一条
// 受 NextAction 信号驱动的执行链。本文件只负责"调度骨架"，节点本身的逻辑在
// meta_agent.go / domain_agent.go / subdomain_agent.go / assistant.go 中实现。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// ThreeLayerNode 三层架构节点接口。
// 任何被图调度循环拉起的执行体都需要实现这两个方法：
//   - Invoke：在传入的 state 上执行一步，返回更新后的 state。
//   - Name：返回节点在图中的唯一名字，用于路由表查找。
type ThreeLayerNode interface {
	Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error)
	Name() string
}

// ThreeLayerGraph 三层图运行时实例。
// 持有静态节点表、角色注册表/工厂、模型工厂、工具回调、进度回调和 Runtime 注入器。
// 通过 sync.RWMutex 保护 nodes / progress 等可被并发访问的字段。
type ThreeLayerGraph struct {
	mu           sync.RWMutex              // 保护 nodes 与 progress 的读写锁
	nodes        map[string]ThreeLayerNode // 静态节点表：MetaAgent / EscalationHandler / Sinker + 动态缓存
	registry     *RoleRegistry             // 角色定义 + 实例的注册表
	factory      *RoleFactory              // 动态角色工厂（创建 Domain/Assistant 实例）
	modelFactory *model.ModelFactory       // Eino ChatModel 工厂（按角色缓存）
	toolCallback ToolCallback              // 工具执行结果回调（推 UI）
	progress     ProgressCallback          // 思考/意图/工具调用实时推送回调
	rt           *runtime.Runtime          // 看板/邮箱/Watchdog/人格/Skill 聚合体
	blockMemory  BlockMemoryStore          // 块记忆存储（特性3：domainAgent 后向量检索）
	archiveStore DomainArchiveStore        // domainAgent 归档存储（特性4：跨会话复用）
}

// SetBlockMemoryStore 在已构建的图上注入块记忆存储（特性3）。
// 供 server / main 后注入；同步给已存在的 DomainAgent 静态节点与动态缓存。
func (g *ThreeLayerGraph) SetBlockMemoryStore(s BlockMemoryStore) {
	g.mu.Lock()
	g.blockMemory = s
	g.mu.Unlock()
	for _, node := range g.nodes {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetBlockMemoryStore(s)
		}
	}
}

// SetArchiveStore 在已构建的图上注入 domainAgent 归档存储（特性4）。
func (g *ThreeLayerGraph) SetArchiveStore(s DomainArchiveStore) {
	g.mu.Lock()
	g.archiveStore = s
	g.mu.Unlock()
	for _, node := range g.nodes {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetArchiveStore(s)
		}
		if m, ok := node.(*MetaAgentNode); ok {
			m.SetArchiveStore(s)
		}
	}
}

// ThreeLayerGraphBuilder 三层图构建器。
// 在 main.go 中按"构造 → 注入依赖 → Build"三段式组装图，避免构造函数参数爆炸。
type ThreeLayerGraphBuilder struct {
	nodes        map[string]ThreeLayerNode // 待构建到图中的节点
	registry     *RoleRegistry             // 角色注册表（必填）
	factory      *RoleFactory              // 角色工厂（可后置注入）
	modelFactory *model.ModelFactory       // 模型工厂（可后置注入）
	toolCallback ToolCallback              // 工具回调（可后置注入）
	progress     ProgressCallback          // 进度回调（可后置注入）
	rt           *runtime.Runtime          // Runtime（可后置注入）
}

// NewThreeLayerGraphBuilder 创建三层图构建器。
// 参数：
//   - registry：角色注册表，用于路由时解析实例。
//   - factory：角色工厂，用于动态创建 Domain/Assistant。
//
// 返回：空的构建器，调用方继续用 Set* 注入依赖，最后 Build()。
func NewThreeLayerGraphBuilder(registry *RoleRegistry, factory *RoleFactory) *ThreeLayerGraphBuilder {
	return &ThreeLayerGraphBuilder{
		nodes:    make(map[string]ThreeLayerNode), // 初始化空节点表
		registry: registry,
		factory:  factory,
	}
}

// SetFactory 设置角色工厂。
// 通常在构造时已经传入，这里保留 setter 以便后续替换（例如测试桩）。
func (b *ThreeLayerGraphBuilder) SetFactory(factory *RoleFactory) {
	b.factory = factory
}

// SetModelFactory 设置模型工厂。
// 必须在 Build() 之前调用，否则 Build 出来的图无法为节点注入 ChatModel。
func (b *ThreeLayerGraphBuilder) SetModelFactory(mf *model.ModelFactory) {
	b.modelFactory = mf
}

// SetToolCallback 设置工具执行回调。
// Domain/SubDomain 节点执行工具后会回调此函数，把 ToolResult 推给 UI。
func (b *ThreeLayerGraphBuilder) SetToolCallback(cb ToolCallback) {
	b.toolCallback = cb
}

// SetProgressCallback 设置进度回调（思考/意图/工具调用实时推 UI）。
// 注入后会在 Build 阶段下发给所有静态节点。
func (b *ThreeLayerGraphBuilder) SetProgressCallback(cb ProgressCallback) {
	b.progress = cb
}

// SetProgressCallback 在已构建的图上设置进度回调（供 server 后注入）。
// 用途：图先 Build 完成，session manager 启动后再注入 SSE 推送回调。
// 副作用：同步给已存在的所有静态节点，保证后续 tick 都能上报进度。
// 并发安全：写 progress 时持写锁，遍历 nodes 时降级为读锁。
func (g *ThreeLayerGraph) SetProgressCallback(cb ProgressCallback) {
	g.mu.Lock() // 先写锁更新字段
	g.progress = cb
	g.mu.Unlock()
	// 同步给已存在的静态节点
	for _, node := range g.nodes {
		g.injectProgress(node) // injectProgress 内部自行获取读锁
	}
}

// Progress 暴露进度回调（节点内部用）。
// 节点执行时通过此方法读取最新回调，避免持有旧引用。
func (g *ThreeLayerGraph) Progress() ProgressCallback {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.progress
}

// SetRuntime 注入 Runtime（看板/邮箱/Watchdog/人格/Skill）。
// Runtime 是 v3 引入的聚合依赖容器，避免每个节点单独持有 5 个指针。
func (b *ThreeLayerGraphBuilder) SetRuntime(rt *runtime.Runtime) {
	b.rt = rt
}

// AddNode 添加节点。
// 以 node.Name() 作为 key 存入节点表，重名会覆盖。
func (b *ThreeLayerGraphBuilder) AddNode(node ThreeLayerNode) {
	b.nodes[node.Name()] = node
}

// Build 构建图。
// 将 Builder 中累积的所有依赖转移到 ThreeLayerGraph，并为静态节点注入
// ModelFactory / Runtime / Progress。
// 返回：可直接 Invoke 的图实例。
func (b *ThreeLayerGraphBuilder) Build() *ThreeLayerGraph {
	g := &ThreeLayerGraph{
		nodes:        b.nodes, // 转移节点表所有权
		registry:     b.registry,
		factory:      b.factory,
		modelFactory: b.modelFactory,
		toolCallback: b.toolCallback,
		progress:     b.progress,
		rt:           b.rt,
	}

	// 为已有节点注入 ModelFactory / Runtime / Progress
	// 动态节点（DomainAgent 等）在 resolveInstanceNode 时单独注入
	for _, node := range g.nodes {
		g.injectModelFactory(node) // 模型工厂 + 工具回调 + Runtime
		g.injectProgress(node)     // 进度回调
	}

	return g
}

// injectProgress 向节点注入进度回调。
// 仅对 MetaAgent / DomainAgent / SubDomainAgent 三类节点生效；
// Assistant 节点不需要进度回调（其进度由父 Domain 上报）。
// 并发安全：读 progress 时持读锁。
func (g *ThreeLayerGraph) injectProgress(node ThreeLayerNode) {
	g.mu.RLock()
	cb := g.progress
	g.mu.RUnlock()
	if cb == nil {
		return // 未配置回调，跳过
	}
	switch n := node.(type) {
	case *MetaAgentNode:
		n.SetProgressCallback(cb)
	case *DomainAgentNode:
		n.SetProgressCallback(cb)
	case *SubDomainAgentNode:
		n.SetProgressCallback(cb)
	}
}

// injectModelFactory 为节点注入模型工厂、工具回调、运行时。
// 三类依赖按节点类型选择性注入：
//   - ModelFactory：Meta/Domain/SubDomain 都需要用来生成 LLM 客户端。
//   - ToolCallback：仅 Domain/SubDomain 需要工具执行回调。
//   - Runtime：仅 Meta/Domain 需要（Watchdog/看板只在两层调度者上启用）。
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
	if g.rt != nil {
		switch n := node.(type) {
		case *MetaAgentNode:
			n.SetRuntime(g.rt) // MetaAgent 用 Runtime 跑 Watchdog / 处理 Mailbox
		case *DomainAgentNode:
			n.SetRuntime(g.rt) // DomainAgent 用 Runtime 查看板 / 收邮件
		case *SubDomainAgentNode:
			n.SetRuntime(g.rt) // SubDomainAgent 用 Runtime 读取 AgentCfg 动态参数
		}
	}
}

// Runtime 暴露 Runtime（server 层使用）。
// 供 server 在 SSE 流处理中访问看板、邮箱等运行时组件。
func (g *ThreeLayerGraph) Runtime() *runtime.Runtime { return g.rt }

// Invoke 执行三层图。
// 状态机循环：每轮从 nodes 取出当前节点 → Invoke → 由 determineNext 决定下一节点。
// 终止条件：NextAction == Finish、next 为空、达到 200 步上限或节点报错。
// 参数：
//   - ctx：上下文，会被 WithSessionID 注入会话 ID。
//   - state：图状态，跨节点共享。
//
// 返回：终态 state 或错误。
// 副作用：每轮通过 progress 回调推送 graph_step 调试事件。
// 并发安全：同一会话不应并发 Invoke；nodes 表访问持读锁。
func (g *ThreeLayerGraph) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	ctx = WithSessionID(ctx, state.SessionID) // 把 sessionID 写入 ctx，供下游日志/存储使用
	current := "MetaAgent"                    // 入口固定从 MetaAgent 开始
	stepCount := 0                            // 已执行步数

	// —— 死循环防护配置（替代硬步数上限）——
	// 三层防御：进展检测 + 状态指纹去重 + wall-clock 超时
	// 配置缺失视为系统级故障，不做兜底；默认值在 config.Load 阶段填充，启动时已校验
	if g.rt == nil || g.rt.AgentCfg == nil {
		return nil, fmt.Errorf("运行时配置未注入 (Runtime/AgentCfg 为空)，状态机无法启动")
	}
	// 进展检测：连续 N 步状态指纹无变化 → 判死循环
	stallSteps := g.rt.AgentCfg.StallSteps
	// 状态指纹去重：同一指纹连续出现 N 次 → 判死循环
	maxRepeatFP := g.rt.AgentCfg.MaxRepeatFingerprint
	// wall-clock 超时：单次 Invoke 超过 sessionTimeout → 判死循环
	sessionTimeout := time.Duration(g.rt.AgentCfg.SessionTimeoutMin) * time.Minute

	// wall-clock 超时：若 ctx 已有 deadline 不早于 sessionTimeout 则保留，否则叠加
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sessionTimeout)
		defer cancel()
	}

	// 进展检测：记录最近一次有进展的步号。进展 = state 关键字段变化（指纹变化）。
	// 指纹只取影响路由的字段，避免无关字段抖动误判进展。
	lastProgressStep := 0
	// 状态指纹去重：记录上一指纹与连续重复次数
	prevFP := ""
	repeatCount := 0

	for {
		// 1. wall-clock 超时检查
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("会话 wall-clock 超时 (%v)，状态机终止 (已执行 %d 步)", sessionTimeout, stepCount)
		}
		stepCount++

		// 2. 查节点：先查静态节点表，未命中则尝试按实例 ID 动态解析
		g.mu.RLock()
		node, ok := g.nodes[current]
		g.mu.RUnlock()
		if !ok {
			// 静态表里没有 → 可能是某 Domain/Assistant 的实例 ID
			node = g.resolveInstanceNode(current)
			if node == nil {
				return nil, fmt.Errorf("节点 %s 未找到", current)
			}
		}

		// 3. 执行节点：传入当前 state，拿到更新后的 state
		newState, err := node.Invoke(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("节点 %s 执行失败: %w", current, err)
		}
		state = newState

		// 4. 路由：根据当前节点名 + state.NextAction 决定下一节点
		next := g.determineNext(current, state)

		// 推送图步骤调试事件（每个 tick 都推，便于 TUI / Web 调试）
		// message 含步号/节点跳转/action；detail 含 state 关键字段变化，便于定位循环卡点
		if g.progress != nil {
			g.progress(ctx, ProgressEvent{
				SessionID: state.SessionID,
				Kind:      "graph_step",
				Agent:     "Graph",
				Message: fmt.Sprintf("Step %d: %s → %s (action=%s, domain=%s, block=%s, stack=%d)",
					stepCount, current, next, state.NextAction,
					state.CurrentDomain, state.CurrentBlockID, len(state.CallStack)),
				Detail: fmt.Sprintf("completed=%d active=%d roles=%d calling=%v clarify=%v",
					len(state.CompletedBlocks), len(state.ActiveBlocks), len(state.RoleInstances),
					state.IsCalling(), state.PendingClarify != nil),
			})
		}

		// 5. 进展检测 + 指纹去重（在终止判定之前，避免挂起态误判）
		fp := fingerprintState(state)
		if fp != prevFP {
			// 指纹变化 → 有进展，重置计数
			lastProgressStep = stepCount
			repeatCount = 0
			prevFP = fp
		} else {
			// 指纹未变
			repeatCount++
			// 渐进 warning：达 50% / 80% 阈值时推送，便于观察死循环形成过程
			if g.progress != nil {
				if repeatCount == maxRepeatFP/2 && maxRepeatFP >= 2 {
					g.progress(ctx, ProgressEvent{
						SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
						Message: fmt.Sprintf("⚠ 状态指纹连续 %d/%d 步重复 (节点=%s)，疑似死循环", repeatCount, maxRepeatFP, current),
					})
				} else if repeatCount == maxRepeatFP-1 && maxRepeatFP >= 2 {
					g.progress(ctx, ProgressEvent{
						SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
						Message: fmt.Sprintf("⚠⚠ 状态指纹连续 %d/%d 步重复，即将触发死循环终止", repeatCount, maxRepeatFP),
					})
				}
			}
			if repeatCount >= maxRepeatFP {
				return nil, fmt.Errorf("检测到死循环：状态连续 %d 步无变化 (节点=%s)，状态机终止", repeatCount, current)
			}
		}
		// 无进展步数渐进 warning：达 50% / 80% 阈值时推送
		stallGap := stepCount - lastProgressStep
		if g.progress != nil && stallGap > 0 {
			if stallGap == stallSteps/2 && stallSteps >= 2 {
				g.progress(ctx, ProgressEvent{
					SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
					Message: fmt.Sprintf("⚠ 连续 %d/%d 步无进展 (节点=%s)，疑似卡死", stallGap, stallSteps, current),
				})
			} else if stallGap == stallSteps-1 && stallSteps >= 2 {
				g.progress(ctx, ProgressEvent{
					SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
					Message: fmt.Sprintf("⚠⚠ 连续 %d/%d 步无进展，即将触发死循环终止", stallGap, stallSteps),
				})
			}
		}
		// 无进展步数超阈值 → 判死循环
		if stallGap >= stallSteps {
			return nil, fmt.Errorf("检测到死循环：连续 %d 步无进展 (节点=%s)，状态机终止", stallSteps, current)
		}

		// 6. 终止判定：Finish 或 next 为空都结束循环
		if state.NextAction == types.ActionFinish {
			return state, nil
		}

		// ActionWait：挂起图循环，等待人机对话答复后由 server 侧恢复（特性5）。
		// 直接 return 而非 continue，避免下一 tick 继续推进；server 在收到 /clarify 答复后会
		// 通过 resumeSession 重新调用 Invoke，从当前 state 继续执行。
		// 注意：Wait 态指纹稳定会重复，但此处已 return，不会触发死循环判定。
		if state.NextAction == types.ActionWait {
			return state, nil
		}

		if next == "" {
			return state, nil // 无下一跳，安全退出
		}
		current = next // 推进到下一节点
	}
}

// fingerprintState 计算状态指纹：取影响路由与语义的关键字段做 SHA256。
// 只取关键字段而非全 state，避免无关字段（如时间戳）抖动误判进展。
// 用于死循环检测：连续 N 步指纹不变 → 判死循环。
func fingerprintState(s *types.ThreeLayerState) string {
	if s == nil {
		return ""
	}
	h := sha256.New()
	// 当前节点路由相关
	fmt.Fprintf(h, "block=%s|domain=%s|goal=%s|action=%s|target=%s|",
		s.CurrentBlockID, s.CurrentDomain, s.DomainGoal, s.NextAction, s.TargetRoleID)
	// 调用栈深度 + 顶层 callee（反映 Assistant 调用进展）
	fmt.Fprintf(h, "stack=%d|top=%s|", len(s.CallStack), s.CurrentAssistantID)
	// 已完成块（反映领域推进）
	fmt.Fprintf(h, "done=%v|", s.CompletedBlocks)
	// 活跃块 ID 集合（反映领域创建进展）
	activeIDs := make([]string, 0, len(s.ActiveBlocks))
	for id := range s.ActiveBlocks {
		activeIDs = append(activeIDs, id)
	}
	fmt.Fprintf(h, "active=%v|", activeIDs)
	// 角色实例数（反映 Agent 创建进展）
	fmt.Fprintf(h, "roles=%d|", len(s.RoleInstances))
	// 待处理澄清请求 ID（反映人机对话态）
	if s.PendingClarify != nil {
		fmt.Fprintf(h, "clarify=%s|", s.PendingClarify.ID)
	}
	return hex.EncodeToString(h.Sum(nil))[:16] // 16 字符够去重，节省内存
}

// resolveInstanceNode 根据实例ID解析节点（动态创建）。
// 静态节点表查不到时调用：从 registry 取实例元信息，按类型 new 一个对应节点，
// 注入依赖后缓存回 nodes 表（避免下次再 new）。
// 参数：
//   - instID：角色实例 ID（形如 domain_xxx_1 / assistant_2）。
//
// 返回：构造好的节点；实例不存在则返回 nil。
// 副作用：成功时会把新节点写入 g.nodes，后续命中走快路径。
// 并发安全：读实例无锁（registry 内部自锁），写 nodes 持写锁。
func (g *ThreeLayerGraph) resolveInstanceNode(instID string) ThreeLayerNode {
	inst := g.registry.GetInstance(instID)
	if inst == nil {
		return nil // 实例已被清理或不存在
	}

	var node ThreeLayerNode

	// 按实例类型构造对应节点
	switch inst.Type {
	case types.RoleTypeDomain:
		node = NewDomainAgentNode(instID, g.registry, g.factory)
	case types.RoleTypeSubDomain:
		node = NewSubDomainAgentNode(instID, g.registry, g.factory)
	case types.RoleTypeFixed, types.RoleTypeDynamic:
		node = NewAssistantNode(instID, g.registry, nil)
	default:
		return nil // 未知类型，无法构造
	}

	// 注入 ModelFactory（内含模型/工具/Runtime 依赖）
	g.injectModelFactory(node)
	// 注入 Progress / ToolCallback
	g.injectProgress(node)
	if g.toolCallback != nil {
		switch n := node.(type) {
		case *DomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		case *SubDomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		}
	}
	// 注入块记忆存储（特性3）
	g.mu.RLock()
	bm := g.blockMemory
	g.mu.RUnlock()
	if bm != nil {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetBlockMemoryStore(bm)
		}
	}
	// 注入 domainAgent 归档存储（特性4）
	g.mu.RLock()
	as := g.archiveStore
	g.mu.RUnlock()
	if as != nil {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetArchiveStore(as)
		}
	}

	// 缓存到静态表：下次同名 ID 直接命中，避免重复构造
	g.mu.Lock()
	g.nodes[instID] = node
	g.mu.Unlock()
	return node
}

// determineNext 三层调度逻辑。
// 入口分发：按 current 的名字路由到对应的 *Next 子函数。
// 当 current 是动态实例 ID 时，通过 registry 查类型再分发。
// 返回空串表示无下一跳（Invoke 会终止循环）。
func (g *ThreeLayerGraph) determineNext(current string, state *types.ThreeLayerState) string {
	switch current {
	case "MetaAgent":
		return g.metaAgentNext(state)
	case "DomainAgent":
		return g.domainAgentNext(state)
	case "Assistant":
		return g.assistantNext(state)
	default:
		// 动态实例 ID：查类型再分发
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

// metaAgentNext MetaAgent 的路由策略。
//   - Switch：切到目标角色；无目标则回退到第一个活跃 block 的第一个 Agent。
//   - Escalate：交给 EscalationHandler 仲裁。
//   - Finish：交给 Sinker 收尾（强制结束）。
//   - Continue：继续在 MetaAgent 循环。
func (g *ThreeLayerGraph) metaAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		// 显式目标优先
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
		// 无显式目标：从活跃 block 中挑第一个 Agent
		for blockID := range state.ActiveBlocks {
			block := state.ActiveBlocks[blockID]
			if len(block.Agents) > 0 {
				return block.Agents[0]
			}
		}
	case types.ActionEscalate:
		return "EscalationHandler" // 上抛到升级处理节点
	case types.ActionFinish:
		return "Sinker" // 走收尾节点
	case types.ActionContinue:
		return "MetaAgent" // 自循环
	}
	return "MetaAgent" // 默认回到 MetaAgent
}

// domainAgentNext DomainAgent 的路由策略。
//   - Switch：切到显式目标角色。
//   - Continue：无调用栈则回 MetaAgent；否则继续当前 Assistant。
//
// 其余情况默认回 MetaAgent 汇报。
func (g *ThreeLayerGraph) domainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		// 没有挂起的子调用 → 回 MetaAgent 汇报
		if !state.IsCalling() {
			return "MetaAgent"
		}
		// 有挂起的子调用 → 继续执行当前 Assistant
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID
		}
	}
	return "MetaAgent"
}

// subDomainAgentNext SubDomainAgent 的路由策略，与 domainAgentNext 同构。
// SubDomainAgent 完成后同样回 DomainAgent（经由 MetaAgent 调度）。
func (g *ThreeLayerGraph) subDomainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case types.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case types.ActionContinue:
		if !state.IsCalling() {
			return "MetaAgent" // 子任务做完，回上层
		}
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID // 继续当前 Assistant
		}
	}
	return "MetaAgent"
}

// assistantNext Assistant 的路由策略。
// Assistant 完成后无脑回 MetaAgent（由 MetaAgent 决定是否继续 DomainAgent 流程）。
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

// SetToolCallback 设置工具执行回调。
// 供 server 层在图构建后补充注入（与 SetProgressCallback 同样的后注入模式）。
func (g *ThreeLayerGraph) SetToolCallback(cb ToolCallback) {
	g.toolCallback = cb
}

// NewToolExecutor 创建带回调的工具执行器。
// 便捷工厂：先 NewToolExecutor 拿到默认执行器，再把图的 toolCallback 挂上。
// 参数：
//   - workDir：工具执行的工作目录（文件读写 / 命令执行的根）。
//
// 返回：已配置回调的 ToolExecutor。
func (g *ThreeLayerGraph) NewToolExecutor(workDir string) *ToolExecutor {
	executor := NewToolExecutor(workDir)
	if g.toolCallback != nil {
		executor.SetCallback(g.toolCallback)
	}
	return executor
}

// GetNode 获取指定名称的节点。
// 仅查静态表（含已缓存的动态节点），不会触发 resolveInstanceNode。
// 并发安全：读 nodes 持读锁。
func (g *ThreeLayerGraph) GetNode(name string) (ThreeLayerNode, bool) {
	g.mu.RLock()
	node, ok := g.nodes[name]
	g.mu.RUnlock()
	return node, ok
}

// ResolveInstanceNode 根据实例ID动态解析节点。
// 导出版本，供 server / 测试代码显式触发动态节点构造。
func (g *ThreeLayerGraph) ResolveInstanceNode(instID string) ThreeLayerNode {
	return g.resolveInstanceNode(instID)
}

// DetermineNext 确定下一个节点。
// 导出版本，供外部调试 / 单测验证路由表。
func (g *ThreeLayerGraph) DetermineNext(current string, state *types.ThreeLayerState) string {
	return g.determineNext(current, state)
}

// BuildThreeLayerGraph 构建默认三层图。
// 便捷函数：把三个静态节点（MetaAgent / EscalationHandler / Sinker）一次性塞进 Builder。
// 参数：
//   - metaAgent：MetaAgent 节点（入口）。
//   - escalation：升级仲裁节点。
//   - sinker：收尾节点（强制 Finish）。
//   - registry / factory：必填依赖。
//
// 返回：已 Build 的图（依赖仍需通过 Set* 后注入）。
func BuildThreeLayerGraph(
	metaAgent ThreeLayerNode,
	escalation ThreeLayerNode,
	sinker ThreeLayerNode,
	registry *RoleRegistry,
	factory *RoleFactory,
) *ThreeLayerGraph {
	builder := NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(metaAgent)  // 入口节点
	builder.AddNode(escalation) // 升级仲裁
	builder.AddNode(sinker)     // 收尾节点
	return builder.Build()
}
