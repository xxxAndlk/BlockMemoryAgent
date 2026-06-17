package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/internal/mailbox"
	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/internal/runtime"
	"github.com/blockmemory/agent/internal/soul"
	"github.com/blockmemory/agent/internal/watchdog"
	"github.com/blockmemory/agent/pkg/types"
)

// HistoryEntry 跨会话历史摘要（与 store.SessionHistoryRecord 解耦，避免 graph 反向依赖 store）
type HistoryEntry struct {
	SessionID   string
	Goal        string
	Summary     string
	ToolResults []map[string]any
	CreatedAt   time.Time
}

// HistoryStore 跨会话历史读取接口
type HistoryStore interface {
	RecentSessionHistories(ctx context.Context, limit int) ([]HistoryEntry, error)
}

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
	rt              *runtime.Runtime
	history         HistoryStore
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

// SetRuntime 注入运行时（看板/邮箱/Watchdog/人格）
func (n *MetaAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// SetHistoryStore 注入跨会话历史读取器，用于 handleInitial 加载"上次做过什么"
func (n *MetaAgentNode) SetHistoryStore(h HistoryStore) {
	n.history = h
}

// Runtime 暴露运行时（其他节点动态构造时使用）
func (n *MetaAgentNode) Runtime() *runtime.Runtime { return n.rt }

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

	if n.summaryInterval > 0 && n.stepCount%n.summaryInterval == 0 {
		n.updateSessionSummary(state)
	}

	n.registry.CleanupExpired()

	// Watchdog: 监控当前活跃 Agent 的上下文规模（v3 §4.4）
	n.runWatchdog(state)

	// 邮箱：拉取广播桶里的消息并尝试转交（v3 §7.2）
	n.processMailbox(state)

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

// runWatchdog 评估当前活跃 Agent 的上下文规模并视情况触发动作
func (n *MetaAgentNode) runWatchdog(state *types.ThreeLayerState) {
	if n.rt == nil || n.rt.Watchdog == nil {
		return
	}
	// 评估当前活跃块的目标 + 任务结果作为粗略上下文规模代理
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		return
	}
	var ctxBuf strings.Builder
	ctxBuf.WriteString(block.Domain)
	ctxBuf.WriteString("\n")
	ctxBuf.WriteString(block.Goal)
	ctxBuf.WriteString("\n")
	for k, v := range block.TaskResults {
		ctxBuf.WriteString(k)
		ctxBuf.WriteString(": ")
		ctxBuf.WriteString(v)
		ctxBuf.WriteString("\n")
	}
	d := n.rt.Watchdog.Check(state.CurrentBlockID, ctxBuf.String())
	if d.Level == watchdog.LevelEvict {
		// 强制注入一条升级事件，让 EscalationHandler 处理
		ev := &types.Event{
			ID:        fmt.Sprintf("watchdog_%d", time.Now().UnixNano()),
			Type:      types.EventEscalation,
			Payload:   map[string]any{"reason": "context evict: " + d.Reason, "topic_id": state.SessionID},
			Priority:  10,
			CreatedAt: time.Now(),
			Status:    types.EventPending,
		}
		block.Events = append(block.Events, ev)
	}
}

// processMailbox 把广播邮件按目标 domain 转给具体 DomainAgent 实例
func (n *MetaAgentNode) processMailbox(state *types.ThreeLayerState) {
	if n.rt == nil || n.rt.Mailbox == nil {
		return
	}
	bcasts := n.rt.Mailbox.DrainBroadcast()
	if len(bcasts) == 0 {
		return
	}
	for _, msg := range bcasts {
		var domainHint string
		if v, ok := msg.Payload["target_domain"].(string); ok {
			domainHint = v
		}
		// 在活跃块里寻找匹配领域
		for _, b := range state.ActiveBlocks {
			if domainHint == "" || b.Domain == domainHint {
				if len(b.Agents) > 0 {
					_ = msg.From
					n.rt.Mailbox.Forward(msg.ID, b.Agents[0])
					if msg.Type == mailbox.MsgEscalate {
						b.Events = append(b.Events, &types.Event{
							ID:        msg.ID,
							Type:      types.EventEscalation,
							Payload:   map[string]any{"reason": msg.Subject},
							Priority:  msg.Priority,
							CreatedAt: msg.CreatedAt,
							Status:    types.EventPending,
						})
					}
					break
				}
			}
		}
	}
}

// loadHistorySection 读取最近 N 条会话历史，拼成可注入 prompt 的中文段落。
// 失败/无数据时返回空串，不影响主流程。
func (n *MetaAgentNode) loadHistorySection(ctx context.Context) string {
	if n.history == nil {
		return ""
	}
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	entries, err := n.history.RecentSessionHistories(hctx, 5)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n已知历史（最近会话，倒序）:\n")
	for i, e := range entries {
		b.WriteString(fmt.Sprintf("%d. [%s] 目标: %s\n   结果: %s\n", i+1, e.SessionID, e.Goal, truncateStr(e.Summary, 300)))
		// 列出该会话中有意义的工具调用（特别是 WriteFile/RunCommand 的 path）
		for _, tr := range e.ToolResults {
			tool, _ := tr["tool"].(string)
			path, _ := tr["path"].(string)
			if path == "" {
				continue
			}
			b.WriteString(fmt.Sprintf("   - %s -> %s\n", tool, path))
		}
	}
	b.WriteString("\n当用户提到指代词（在哪/刚才/上次/那个文件）时，请优先结合上述历史作答或检索。\n")
	return b.String()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// handleInitial 首次启动处理
func (n *MetaAgentNode) handleInitial(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 加载跨会话历史，拼成"已知历史"段落注入后续 prompt
	historySection := n.loadHistorySection(ctx)

	// 简单问题直接回答，不拆分
	if n.modelFactory != nil && n.isSimpleQuestion(state.DomainGoal) {
		answer, err, timedOut := n.callLLM(ctx, fmt.Sprintf(
			`你是BlockMemoryAgent，一个基于大语言模型的本地AI开发助手，使用多Agent智能编排架构。
你可以帮助用户：分析代码、操作文件、执行命令、搜索代码、编写程序等。

请直接回答用户的简单问题，保持简洁友好。
%s
用户问题：%s

你的回答：`, historySection, state.DomainGoal))
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

	// 初始化 TaskBoard（v3 §7.1）：把领域名作为顶层子任务
	if n.rt != nil && n.rt.Boards != nil {
		bd := n.rt.Boards.GetOrCreate(state.SessionID, state.DomainGoal)
		for _, d := range domains {
			bd.AddSubTask(d.Name + " - " + d.Goal)
		}
	}

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

// isSimpleQuestion 判断是否为简单直接问题（仅寒暄/自我介绍类）。
//
// 注意：之前的实现用 len(goal) < 30 字节判定，对中文极不靠谱——
// "贪吃蛇小游戏在哪" 这种指代类问题（8 汉字 = 24 字节）会被误判成
// 简单问题，直接跳过 Graph 走 LLM 一问一答，既不读历史也不调工具，
// 表现为"Agent 失忆"。
//
// 现在：必须命中明确的寒暄模式，并且不含任何指代/查找/操作词。
func (n *MetaAgentNode) isSimpleQuestion(goal string) bool {
	goalLower := strings.ToLower(goal)

	// 指代/历史/操作类关键词：命中即视为非简单问题，需走完整 Graph
	referencePatterns := []string{
		"在哪", "哪里", "刚才", "上次", "之前", "上次", "之前", "那个",
		"这个", "刚才", "记得", "记忆", "历史", "之前",
		"文件", "代码", "目录", "项目", "找", "查找", "搜索",
		"写", "创建", "修改", "删除", "运行", "执行",
	}
	for _, p := range referencePatterns {
		if strings.Contains(goalLower, p) {
			return false
		}
	}

	// 仅在命中明确寒暄/自我介绍模式时才视为简单问题
	simplePatterns := []string{
		"你是什么", "你是谁", "什么模型", "你好", "hello", "hi", "hey",
		"叫什么名字", "介绍自己", "自我介绍", "能做什么", "有什么功能",
	}
	for _, p := range simplePatterns {
		if strings.Contains(goalLower, p) {
			return true
		}
	}

	// 极短且纯 ASCII（如 "ping"）仍视为简单；中文短句一律不在此列
	if utf8.RuneCountInString(goal) <= 6 && isASCII(goal) {
		return true
	}
	return false
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// callLLM 统一的LLM调用入口（带自适应超时 + 人格注入 + 温度调节）
func (n *MetaAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	llm, err := n.modelFactory.GetMetaModel(ctx)
	if err != nil {
		return "", err, false
	}

	// 注入人格
	if n.rt != nil && n.rt.Soul != nil {
		prompt = n.rt.Soul.Inject(prompt)
	}

	// MetaAgent 主要做"路由 / 总结"决策，使用 0 温度
	if t, ok := llm.(model.TemperatureAware); ok {
		desired := soul.Temperature(soul.KindRouting, 0)
		// 通过包装一个临时 LLMClient 让 timeoutTracker 仍能记录耗时
		wrapped := &temperatureWrappedLLM{base: llm, t: t, temperature: desired}
		return n.timeoutTracker.CallWithTimeout(ctx, wrapped, prompt,
			30*time.Second, 90*time.Second)
	}

	return n.timeoutTracker.CallWithTimeout(ctx, llm, prompt,
		30*time.Second, // 正常超时
		90*time.Second, // 深度思考超时
	)
}

// temperatureWrappedLLM 在 LLMClient 外层叠加 per-call temperature
type temperatureWrappedLLM struct {
	base        model.LLMClient
	t           model.TemperatureAware
	temperature float64
}

func (w *temperatureWrappedLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return w.t.GenerateWithOptions(ctx, prompt, w.temperature)
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

	historySection := n.loadHistorySection(ctx)

	// 尝试使用LLM分析领域
	if n.modelFactory != nil && !n.timeoutTracker.ShouldSkipLLM() {
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个多Agent系统的领域分析器。请分析以下用户目标，确定需要哪些业务领域来协作完成。

用户目标: %s
%s
要求:
- 每个领域名称简短（2-6个字）
- 领域之间应该尽量独立
- 若用户目标涉及"查找/刚才/上次"等指代词，应优先创建一个"检索历史与文件"领域
- 输出JSON数组格式: [{"name":"领域名","goal":"该领域需要完成的目标"}]
- 只输出JSON，不要其他内容

领域列表:`, goal, historySection))
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
