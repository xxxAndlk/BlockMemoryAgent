package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/types"
)

// MetaAgentNode Layer 1: 主Agent / 会话调度器
type MetaAgentNode struct {
	name            string
	registry        *RoleRegistry
	factory         *RoleFactory
	modelFactory    *model.ModelFactory
	timeoutTracker  *model.TimeoutTracker
	maxBlocks       int
	summaryInterval int
	stepCount       int
}

// NewMetaAgentNode 创建主Agent节点
func NewMetaAgentNode(registry *RoleRegistry, factory *RoleFactory, maxBlocks, summaryInterval int) *MetaAgentNode {
	return &MetaAgentNode{
		name:            "MetaAgent",
		registry:        registry,
		factory:         factory,
		maxBlocks:       maxBlocks,
		summaryInterval: summaryInterval,
		stepCount:       0,
		timeoutTracker:  model.NewTimeoutTracker(),
	}
}

// SetModelFactory 设置模型工厂
func (n *MetaAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// TimeoutStats 获取超时统计
func (n *MetaAgentNode) TimeoutStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	return n.timeoutTracker.Stats()
}

// Name 返回节点名称
func (n *MetaAgentNode) Name() string {
	return n.name
}

// Invoke 执行主Agent逻辑
func (n *MetaAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	n.stepCount++

	if n.stepCount%n.summaryInterval == 0 {
		n.updateSessionSummary(state)
	}

	n.registry.CleanupExpired()

	switch {
	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID == "":
		return n.handleInitial(ctx, state)

	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID != "":
		state.NextAction = types.ActionFinish
		state.Reason = "all blocks completed"

	case state.CurrentBlockID != "" && state.IsCalling():
		state.NextAction = types.ActionContinue

	case state.CurrentBlockID != "" && !state.IsCalling():
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil && len(block.Events) > 0 {
			return n.handleBlockEvents(ctx, state, block)
		}
		return n.switchToNextBlock(ctx, state)

	default:
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// handleInitial 首次启动处理
func (n *MetaAgentNode) handleInitial(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 简单问题直接回答，不拆分
	if n.modelFactory != nil && n.isSimpleQuestion(state.DomainGoal) {
		answer, err, timedOut := n.callLLM(ctx, fmt.Sprintf(
			`你是BlockMemoryAgent，一个基于大语言模型的本地AI开发助手，使用多Agent智能编排架构。
你可以帮助用户：分析代码、操作文件、执行命令、搜索代码、编写程序等。

请直接回答用户的简单问题，保持简洁友好。

用户问题：%s

你的回答：`, state.DomainGoal))
		if timedOut {
			state.SessionSummary = "LLM调用超时，请稍后重试"
			state.NextAction = types.ActionFinish
			state.Reason = "llm timeout on simple question"
			return state, nil
		}
		if err == nil && answer != "" {
			state.SessionSummary = answer
			state.NextAction = types.ActionFinish
			state.Reason = "direct answer for simple question"
			return state, nil
		}
	}

	domains := n.analyzeDomains(ctx, state)

	for _, domain := range domains {
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, domain.Name, domain.Goal, "")
		if err != nil {
			fmt.Printf("[MetaAgent] create domain agent %s failed: %v\n", domain.Name, err)
			continue
		}

		block := &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(domain.Name), len(state.ActiveBlocks)),
			SessionID:   state.SessionID,
			Domain:      domain.Name,
			Goal:        domain.Goal,
			Status:      "active",
			Agents:      []string{inst.ID},
			Events:      make([]*types.Event, 0),
			TaskResults: make(map[string]string),
		}
		state.ActiveBlocks[block.ID] = block
	}

	// 所有领域创建失败，直接结束
	if len(state.ActiveBlocks) == 0 {
		state.NextAction = types.ActionFinish
		state.Reason = "failed to create any domain agent"
		return state, nil
	}

	for blockID := range state.ActiveBlocks {
		state.CurrentBlockID = blockID
		block := state.ActiveBlocks[blockID]
		state.CurrentDomain = block.Domain
		state.DomainGoal = block.Goal
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = block.Agents[0]
		break
	}

	n.updateSessionSummary(state)
	return state, nil
}

// handleBlockEvents 处理会话块事件
func (n *MetaAgentNode) handleBlockEvents(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock) (*types.ThreeLayerState, error) {
	for _, ev := range block.Events {
		if ev.Status != types.EventPending {
			continue
		}

		switch ev.Type {
		case types.EventCrossModify:
			return n.handleCrossDomainRequest(ctx, state, ev)

		case types.EventEscalation:
			state.NextAction = types.ActionEscalate
			state.Reason = getString(ev.Payload, "reason")
			return state, nil

		default:
			ev.Status = types.EventDone
		}
	}

	state.NextAction = types.ActionContinue
	return state, nil
}

// handleCrossDomainRequest 处理跨领域请求
func (n *MetaAgentNode) handleCrossDomainRequest(ctx context.Context, state *types.ThreeLayerState, ev *types.Event) (*types.ThreeLayerState, error) {
	targetDomain := getString(ev.Payload, "target_domain")
	if targetDomain == "" {
		ev.Status = types.EventDone
		state.NextAction = types.ActionContinue
		return state, nil
	}

	var targetBlock *types.SessionBlock
	for _, b := range state.ActiveBlocks {
		if b.Domain == targetDomain {
			targetBlock = b
			break
		}
	}

	if targetBlock == nil {
		if len(state.ActiveBlocks) >= n.maxBlocks {
			state.NextAction = types.ActionEscalate
			state.Reason = fmt.Sprintf("max blocks reached, cannot create domain %s", targetDomain)
			return state, nil
		}

		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, targetDomain,
			getString(ev.Payload, "goal"), "")
		if err != nil {
			state.NextAction = types.ActionEscalate
			state.Reason = fmt.Sprintf("failed to create domain agent: %v", err)
			return state, nil
		}

		targetBlock = &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(targetDomain), len(state.ActiveBlocks)),
			SessionID:   state.SessionID,
			Domain:      targetDomain,
			Goal:        getString(ev.Payload, "goal"),
			Status:      "active",
			Agents:      []string{inst.ID},
			Events:      make([]*types.Event, 0),
			TaskResults: make(map[string]string),
		}
		state.ActiveBlocks[targetBlock.ID] = targetBlock
	}

	state.CurrentBlockID = targetBlock.ID
	state.CurrentDomain = targetBlock.Domain
	state.DomainGoal = targetBlock.Goal
	state.NextAction = types.ActionSwitch
	state.TargetRoleID = targetBlock.Agents[0]
	ev.Status = types.EventDone

	return state, nil
}

// switchToNextBlock 切换到下一个会话块
func (n *MetaAgentNode) switchToNextBlock(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	if state.CurrentBlockID != "" {
		// 汇总当前block的结果到SessionSummary
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil {
			n.collectBlockResult(state, block)
		}
		state.CompletedBlocks = append(state.CompletedBlocks, state.CurrentBlockID)
		delete(state.ActiveBlocks, state.CurrentBlockID)
	}

	for blockID, block := range state.ActiveBlocks {
		state.CurrentBlockID = blockID
		state.CurrentDomain = block.Domain
		state.DomainGoal = block.Goal
		state.NextAction = types.ActionSwitch
		state.TargetRoleID = block.Agents[0]
		return state, nil
	}

	// 所有block完成，生成最终回答
	n.finalizeSession(ctx, state)
	state.CurrentBlockID = ""
	state.CurrentDomain = ""
	state.DomainGoal = ""
	state.NextAction = types.ActionFinish
	state.Reason = "all session blocks completed"
	return state, nil
}

// collectBlockResult 收集block结果到session summary
func (n *MetaAgentNode) collectBlockResult(state *types.ThreeLayerState, block *types.SessionBlock) {
	var parts []string
	if block.Domain != "" {
		parts = append(parts, fmt.Sprintf("【%s】", block.Domain))
	}
	for task, result := range block.TaskResults {
		if result != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", task, result))
		}
	}
	if len(parts) > 0 {
		if state.SessionSummary != "" {
			state.SessionSummary += "\n"
		}
		state.SessionSummary += strings.Join(parts, "\n")
	}
}

// finalizeSession 会话结束，生成最终回答
func (n *MetaAgentNode) finalizeSession(ctx context.Context, state *types.ThreeLayerState) {
	if state.SessionSummary == "" {
		n.updateSessionSummary(state)
		return
	}
	if n.modelFactory != nil && !n.timeoutTracker.ShouldSkipLLM() {
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是BlockMemoryAgent，一个本地AI开发助手。请基于以下各助手的执行结果，生成一个清晰、完整的最终回答给用户。

各助手执行结果：
%s

请直接输出最终回答，不要加任何前缀或总结性语句。`, state.SessionSummary))
		if !timedOut && err == nil && resp != "" {
			state.SessionSummary = resp
			return
		}
		if timedOut {
			fmt.Printf("[MetaAgent] LLM timeout on finalize, keeping raw results. %s\n", n.timeoutTracker.StatsString())
		}
	}
}

// updateSessionSummary 更新会话总结
func (n *MetaAgentNode) updateSessionSummary(state *types.ThreeLayerState) {
	var parts []string
	parts = append(parts, fmt.Sprintf("会话[%s]已执行%d步", state.SessionID, n.stepCount))
	parts = append(parts, fmt.Sprintf("完成领域: %v", state.CompletedBlocks))
	parts = append(parts, fmt.Sprintf("活跃领域: %d个", len(state.ActiveBlocks)))
	if state.CurrentDomain != "" {
		parts = append(parts, fmt.Sprintf("当前领域: %s", state.CurrentDomain))
	}
	state.SessionSummary = strings.Join(parts, "; ")
}

// isSimpleQuestion 判断是否为简单直接问题
func (n *MetaAgentNode) isSimpleQuestion(goal string) bool {
	goalLower := strings.ToLower(goal)
	simplePatterns := []string{
		"你是什么", "你是谁", "什么模型", "你好", "hello", "hi", "hey",
		"叫什么名字", "介绍自己", "自我介绍", "能做什么", "有什么功能",
	}
	for _, p := range simplePatterns {
		if strings.Contains(goalLower, p) {
			return true
		}
	}
	return len(goal) < 30
}

// callLLM 统一的LLM调用入口（带自适应超时）
func (n *MetaAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	llm, err := n.modelFactory.GetMetaModel(ctx)
	if err != nil {
		return "", err, false
	}
	return n.timeoutTracker.CallWithTimeout(ctx, llm, prompt,
		30*time.Second, // 正常超时
		90*time.Second, // 深度思考超时
	)
}

// DomainInfo 领域信息
type DomainInfo struct {
	Name string
	Goal string
}

// analyzeDomains 分析用户目标，确定需要的领域（优先LLM，回退规则）
func (n *MetaAgentNode) analyzeDomains(ctx context.Context, state *types.ThreeLayerState) []DomainInfo {
	goal := state.DomainGoal
	if goal == "" {
		goal = state.SessionSummary
	}

	// 尝试使用LLM分析领域
	if n.modelFactory != nil && !n.timeoutTracker.ShouldSkipLLM() {
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个多Agent系统的领域分析器。请分析以下用户目标，确定需要哪些业务领域来协作完成。

用户目标: %s

要求:
- 每个领域名称简短（2-6个字）
- 领域之间应该尽量独立
- 输出JSON数组格式: [{"name":"领域名","goal":"该领域需要完成的目标"}]
- 只输出JSON，不要其他内容

领域列表:`, goal))
		if !timedOut && err == nil && resp != "" {
			if domains := n.parseDomainsFromLLM(resp); len(domains) > 0 {
				return domains
			}
		}
		if timedOut {
			fmt.Printf("[MetaAgent] LLM timeout on domain analysis, using rules fallback. %s\n", n.timeoutTracker.StatsString())
		}
	}

	// 规则回退
	return n.analyzeDomainsByRules(goal)
}

// parseDomainsFromLLM 从LLM响应解析领域列表
func (n *MetaAgentNode) parseDomainsFromLLM(resp string) []DomainInfo {
	jsonStr := extractJSON(resp)
	var rawDomains []struct {
		Name string `json:"name"`
		Goal string `json:"goal"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &rawDomains); err != nil || len(rawDomains) == 0 {
		return nil
	}

	var domains []DomainInfo
	for _, d := range rawDomains {
		if d.Name != "" {
			domains = append(domains, DomainInfo{Name: d.Name, Goal: d.Goal})
		}
	}
	return domains
}

// analyzeDomainsByRules 基于关键词规则的领域分析
func (n *MetaAgentNode) analyzeDomainsByRules(goal string) []DomainInfo {
	var domains []DomainInfo

	if strings.Contains(goal, "商城") || strings.Contains(goal, "页面") {
		domains = append(domains, DomainInfo{Name: "商城页面", Goal: goal})
	}
	if strings.Contains(goal, "购物车") || strings.Contains(goal, "购买") {
		domains = append(domains, DomainInfo{Name: "购物模块", Goal: goal})
	}
	if strings.Contains(goal, "订单") {
		domains = append(domains, DomainInfo{Name: "订单模块", Goal: goal})
	}
	if strings.Contains(goal, "用户") || strings.Contains(goal, "登录") {
		domains = append(domains, DomainInfo{Name: "用户模块", Goal: goal})
	}

	if len(domains) == 0 {
		domains = append(domains, DomainInfo{Name: "通用", Goal: goal})
	}

	return domains
}
