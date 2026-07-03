package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"log"
	"strings"
	"time"
)

// handleInitial 首次启动处理。
//
// 职责：
//   - 调 ClassifyTask 做 5 路径路由
//   - RouteDirectTool / RouteDirectAssistant：MetaAgent 直接执行并结束（0-1 层，不创建领域 Agent）
//   - RouteCreateDomain / RouteMultiDomain：调 analyzeDomains 拆分领域，创建 DomainAgent（不启用 SubDomain）
//   - RouteFullFourLayer：同上但启用 SubDomain
//   - 初始化 TaskBoard，把领域名作为顶层子任务
//   - 切换到第一个块交控制权给 DomainAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 返回：更新后的 state；所有领域创建失败则 ActionFinish。
//
// 副作用：创建 DomainAgent 实例；写入 ActiveBlocks、TaskBoard；更新 SessionSummary。
func (n *MetaAgentNode) handleInitial(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	n.emit(ctx, "think", "分析用户目标，决定执行路径")

	// 5 路径路由：规则层（零 LLM）→ LLM 兜底 → 安全兜底（RouteCreateDomain）
	decision := n.ClassifyTask(ctx, state)
	n.emit(ctx, "intend", fmt.Sprintf("路由判定: %s", decision.Path))
	if log := n.sessionLogger(ctx); log != nil {
		log.Event(ctx, "routing", fmt.Sprintf("goal=%s → path=%s", state.DomainGoal, decision.Path), map[string]any{
			"goal":      state.DomainGoal,
			"path":      decision.Path,
			"subdomain": decision.EnableSubdomain,
		})
	}

	switch decision.Path {
	case RouteDirectTool, RouteDirectAssistant:
		// 0-1 层：MetaAgent 不直接执行工具，而是创建 Assistant 代为执行（P0-1）。
		// RouteDirectTool 原意为"简单工具请求"，同样走助手路径，避免 MetaAgent 直接调用工具。
		return n.executeDirectAssistant(ctx, state)
	default:
		// RouteCreateDomain / RouteMultiDomain / RouteFullFourLayer：走领域拆分
		return n.handleInitialCreateDomains(ctx, state, decision.EnableSubdomain)
	}
}

// handleInitialCreateDomains 领域拆分路径：analyzeDomains → 创建 DomainAgent/SessionBlock → 切换。
//
// 被 handleInitial 的 RouteCreateDomain/MultiDomain/FullFourLayer 路径，以及直接执行失败回退调用。
// enableSubdomain=true 时在 state 置位，DomainAgent 据此自适应启用 SubDomain。
func (n *MetaAgentNode) handleInitialCreateDomains(ctx context.Context, state *types.ThreeLayerState, enableSubdomain bool) (*types.ThreeLayerState, error) {
	// 标记是否允许 DomainAgent 自适应启用 SubDomain（仅 RouteFullFourLayer 为 true）
	state.EnableSubdomain = enableSubdomain
	if enableSubdomain {
		n.emit(ctx, "intend", "启用 SubDomain 自适应（完整四层编排候选）")
	}

	// 复杂问题：调 LLM 拆分领域
	domains := n.analyzeDomains(ctx, state)
	if len(domains) == 0 {
		// 无领域返回：若启用人机对话（特性5），向用户请求澄清而非直接结束
		if n.humanClarifyEnabled() {
			// 构造澄清请求：ID 用 SessionID+纳秒时间戳保证唯一；Context 回放原始 goal 便于用户对照
			clr := &types.ClarifyRequest{
				ID:        fmt.Sprintf("clarify_%s_%d", state.SessionID, time.Now().UnixNano()),
				Question:  "无法从目标中识别出可执行的领域，请补充说明你希望完成的具体任务或目标。",
				Context:   fmt.Sprintf("原始目标: %s", state.DomainGoal),
				AgentID:   "MetaAgent",
				CreatedAt: time.Now(),
			}
			// 挂起图循环：PendingClarify 非空 + ActionWait 触发 server 把 session 置 awaiting_clarify
			state.PendingClarify = clr
			state.NextAction = enums.ActionWait
			state.Reason = "awaiting human clarification"
			n.emit(ctx, "wait", "已向用户请求澄清: "+clr.Question)
			return state, nil
		}
		// 未启用：推送错误事件，下方继续走 Finish 流程
		n.emit(ctx, "error", "领域分析未返回任何领域，将结束会话")
	} else {
		// 推送拆分结果
		names := make([]string, 0, len(domains))
		for _, d := range domains {
			// 收集领域名用于事件展示
			names = append(names, d.Name)
		}
		n.emit(ctx, "intend", fmt.Sprintf("拆分出 %d 个领域: %s", len(domains), strings.Join(names, ", ")))
		if log := n.sessionLogger(ctx); log != nil {
			log.Event(ctx, "task_split", fmt.Sprintf("拆分出 %d 个领域", len(domains)), map[string]any{
				"domains": names,
				"count":   len(domains),
			})
		}
	}

	// 初始化 TaskBoard（v3 §7.1）：把领域名作为顶层子任务
	if n.rt != nil && n.rt.Boards != nil {
		// 获取或创建本会话的看板
		bd := n.rt.Boards.GetOrCreate(state.SessionID, state.DomainGoal)
		for _, d := range domains {
			// 每个领域作为顶层子任务
			bd.AddSubTask(d.Name + " - " + d.Goal)
		}
	}

	// 为每个领域创建 DomainAgent 与 SessionBlock
	for _, domain := range domains {
		// 达到最大并发块数则停止
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		n.emit(ctx, "intend", fmt.Sprintf("创建 DomainAgent: %s (目标: %s)", domain.Name, domain.Goal))
		if log := n.sessionLogger(ctx); log != nil {
			log.Event(ctx, "domain_create", fmt.Sprintf("创建 DomainAgent: %s", domain.Name), map[string]any{
				"domain": domain.Name,
				"goal":   domain.Goal,
			})
		}
		// 二次检查（防御性）
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		// 创建 DomainAgent 实例
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, domain.Name, domain.Goal, "")
		if err != nil {
			// 创建失败：打印日志并跳过该领域
			log.Printf("[MetaAgent] 创建领域 Agent %s 失败: %v\n", domain.Name, err)
			continue
		}
		// 推送 Agent 创建调试事件
		roleDef := n.registry.GetRoleDef(inst.RoleDefID) // 取角色定义
		agentName := domain.Name + "负责人"                 // 默认名称
		if roleDef != nil {
			// 有角色定义则用其名称
			agentName = roleDef.Name
		}
		n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 DomainAgent: %s (领域: %s)", agentName, domain.Name),
			fmt.Sprintf("instID=%s roleDefID=%s goal=%s", inst.ID, inst.RoleDefID, domain.Goal))

		// 构造会话块
		block := &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(domain.Name), len(state.ActiveBlocks)), // 块ID（领域名+序号）
			SessionID:   state.SessionID,                                                              // 所属会话
			Domain:      domain.Name,                                                                  // 领域名
			Goal:        domain.Goal,                                                                  // 领域目标
			Status:      enums.BlockStatusActive,                                                      // 初始状态活跃
			Agents:      []string{inst.ID},                                                            // 关联的 DomainAgent 实例
			Events:      make([]*types.Event, 0),                                                      // 事件队列
			TaskResults: make(map[string]string),                                                      // 任务结果
		}
		// 写入活跃块表
		state.ActiveBlocks[block.ID] = block
	}

	// 所有领域创建失败，直接结束
	if len(state.ActiveBlocks) == 0 {
		state.NextAction = enums.ActionFinish              // 结束会话
		state.Reason = "failed to create any domain agent" // 记录原因
		return state, nil
	}

	// 切换到第一个块（map 迭代顺序不固定，但只取一个）
	for blockID := range state.ActiveBlocks {
		block := state.ActiveBlocks[blockID] // 取块引用
		n.emitTopicSwitch(ctx, "", block.Domain)
		state.CurrentBlockID = blockID        // 设为当前块
		state.CurrentDomain = block.Domain    // 更新当前领域
		state.DomainGoal = block.Goal         // 更新领域目标
		state.NextAction = enums.ActionSwitch // 切换到 DomainAgent
		state.TargetRoleID = block.Agents[0]  // 路由目标
		break                                 // 只取第一个
	}

	// 更新会话摘要
	n.updateSessionSummary(state)
	return state, nil
}

// handleBlockEvents 处理会话块事件。
//
// 职责：遍历块的事件队列，按类型分派：
//   - EventCrossModify：转交跨域请求处理器
//   - EventEscalation：设置 ActionEscalate 并返回
//   - 其他：标记为已完成
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - block：当前会话块
//
// 返回：更新后的 state。
func (n *MetaAgentNode) handleBlockEvents(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock) (*types.ThreeLayerState, error) {
	for _, ev := range block.Events {
		// 跳过非待处理事件
		if ev.Status != enums.EventPending {
			continue
		}

		// 按事件类型分派
		switch ev.Type {
		case enums.EventCrossModify:
			// 跨域修改请求：交给跨域处理器
			return n.handleCrossDomainRequest(ctx, state, ev)

		case enums.EventEscalation:
			// 升级事件：标记完成避免重复处理（H7），设置 ActionEscalate 交 EscalationHandler 仲裁
			ev.Status = enums.EventDone                    // 先标记完成，防止 handleBlockEvents 下轮重复触发
			state.NextAction = enums.ActionEscalate        // 设置升级动作
			state.Reason = getString(ev.Payload, "reason") // 记录升级原因
			return state, nil

		default:
			// 未知类型：直接标记完成
			ev.Status = enums.EventDone
		}
	}

	// 无待处理事件或已处理完：继续图循环
	state.NextAction = enums.ActionContinue
	return state, nil
}

// handleCrossDomainRequest 处理跨领域请求。
//
// 职责：
//   - 从事件载荷取目标领域
//   - 在活跃块中查找匹配领域；找不到则创建新块（受 maxBlocks 限制）
//   - 切换到目标块，交控制权给对应 DomainAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - ev：跨域请求事件
//
// 返回：更新后的 state；达到最大块数或创建失败则 ActionEscalate。
func (n *MetaAgentNode) handleCrossDomainRequest(ctx context.Context, state *types.ThreeLayerState, ev *types.Event) (*types.ThreeLayerState, error) {
	// 取目标领域；为空则标记事件完成并继续
	targetDomain := getString(ev.Payload, "target_domain")
	if targetDomain == "" {
		ev.Status = enums.EventDone             // 标记事件完成
		state.NextAction = enums.ActionContinue // 继续图循环
		return state, nil
	}

	// 在活跃块中查找匹配领域
	var targetBlock *types.SessionBlock
	for _, b := range state.ActiveBlocks {
		// 领域名匹配
		if b.Domain == targetDomain {
			targetBlock = b
			break
		}
	}

	// 未找到目标块：创建新块
	if targetBlock == nil {
		// 达到最大块数：升级处理
		if len(state.ActiveBlocks) >= n.maxBlocks {
			state.NextAction = enums.ActionEscalate // 升级处理
			state.Reason = fmt.Sprintf("max blocks reached, cannot create domain %s", targetDomain)
			return state, nil
		}

		// 创建新 DomainAgent 实例
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, targetDomain,
			getString(ev.Payload, "goal"), "")
		if err != nil {
			// 创建失败：升级处理
			state.NextAction = enums.ActionEscalate // 升级处理
			state.Reason = fmt.Sprintf("failed to create domain agent: %v", err)
			return state, nil
		}

		// 构造新会话块
		targetBlock = &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(targetDomain), len(state.ActiveBlocks)), // 块ID（领域名+序号）
			SessionID:   state.SessionID,                                                               // 所属会话
			Domain:      targetDomain,                                                                  // 领域名
			Goal:        getString(ev.Payload, "goal"),                                                 // 领域目标
			Status:      enums.BlockStatusActive,                                                       // 初始状态活跃
			Agents:      []string{inst.ID},                                                             // 关联的 DomainAgent 实例
			Events:      make([]*types.Event, 0),                                                       // 事件队列
			TaskResults: make(map[string]string),                                                       // 任务结果
		}
		// 写入活跃块表
		state.ActiveBlocks[targetBlock.ID] = targetBlock
	}

	// 切换到目标块
	prevDomain := state.CurrentDomain
	n.emitTopicSwitch(ctx, prevDomain, targetBlock.Domain)
	state.CurrentBlockID = targetBlock.ID      // 设为当前块
	state.CurrentDomain = targetBlock.Domain   // 更新当前领域
	state.DomainGoal = targetBlock.Goal        // 更新领域目标
	state.NextAction = enums.ActionSwitch      // 切换到 DomainAgent
	state.TargetRoleID = targetBlock.Agents[0] // 路由目标
	ev.Status = enums.EventDone                // 标记事件已处理

	return state, nil
}

// switchToNextBlock 切换到下一个会话块。
// 返回：更新后的 state；无活跃块时调 finalizeSession 生成最终回答。
func (n *MetaAgentNode) switchToNextBlock(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 收尾当前块
	prevDomain := ""
	if state.CurrentBlockID != "" {
		prevDomain = state.CurrentDomain
		// 汇总当前block的结果到SessionSummary
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil {
			n.collectBlockResult(state, block)
			// 幂等兜底归档（TODO #4）：异步归档未确认成功时，同步补一次，避免异常退出漏写
			n.ensureBlockArchived(ctx, state, block)
		}
		// 移入已完成列表
		state.CompletedBlocks = append(state.CompletedBlocks, state.CurrentBlockID)
		delete(state.ActiveBlocks, state.CurrentBlockID)
	}

	// 2. 切换到下一个活跃块
	for blockID, block := range state.ActiveBlocks {
		n.emitTopicSwitch(ctx, prevDomain, block.Domain)
		state.CurrentBlockID = blockID        // 设为当前块
		state.CurrentDomain = block.Domain    // 更新当前领域
		state.DomainGoal = block.Goal         // 更新领域目标
		state.NextAction = enums.ActionSwitch // 切换到 DomainAgent
		state.TargetRoleID = block.Agents[0]  // 路由目标
		return state, nil                     // 只取第一个
	}

	// 3. 所有block完成，生成最终回答
	n.finalizeSession(ctx, state)                 // 生成最终回答
	state.CurrentBlockID = ""                     // 清空当前块
	state.CurrentDomain = ""                      // 清空当前领域
	state.DomainGoal = ""                         // 清空领域目标
	state.NextAction = enums.ActionFinish         // 结束会话
	state.Reason = "all session blocks completed" // 记录原因
	return state, nil
}
