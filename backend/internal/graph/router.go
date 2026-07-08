package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// 本文件实现 5 路径智能路由，让 80% 简单任务不进入四层编排。
//
// 五条路径（详见 修改文档/计划）：
//   - RouteDirectTool      0 层：MetaAgent 自跑工具循环（含 0 工具的纯 QA）
//   - RouteDirectAssistant 1 层：MetaAgent 建助手 + 跑工具循环
//   - RouteCreateDomain    2 层：单领域，不启用 SubDomain
//   - RouteMultiDomain     2 层 × N：多领域并行
//   - RouteFullFourLayer   4 层：完整编排，启用 SubDomain
//
// 决策顺序：规则层（零 LLM 成本，覆盖典型 60%+）→ LLM 兜底（轻量模型）→ 安全兜底
// （RouteCreateDomain = 当前复杂任务行为，零回归）。

// RoutePath 路由路径枚举。
type RoutePath string

const (
	RouteDirectTool      RoutePath = "direct_tool"
	RouteDirectAssistant RoutePath = "direct_assistant"
	RouteCreateDomain    RoutePath = "create_domain"
	RouteMultiDomain     RoutePath = "multi_domain"
	RouteFullFourLayer   RoutePath = "full_four_layer"
)

// RouteDecision 路由决策结果。
type RouteDecision struct {
	Path            RoutePath // 命中的路径
	EnableSubdomain bool      // 是否启用 SubDomain（仅 RouteFullFourLayer 为 true）
}

// ClassifyTask 对用户目标做路由分类。
//
// 决策顺序：规则层 → LLM 兜底 → 安全兜底（RouteCreateDomain）。
// 纯 QA / 单工具 / 单领域简单任务在规则层即短路，不消耗 LLM。
//
// 参数：
//   - ctx：请求上下文（LLM 兜底用）。
//   - state：图全局状态（取 DomainGoal）。
//
// 返回：RouteDecision。
func (n *MetaAgentNode) ClassifyTask(ctx context.Context, state *types.ThreeLayerState) RouteDecision {
	goal := ""
	if state != nil {
		goal = state.DomainGoal
	}
	// 空目标：安全兜底
	if strings.TrimSpace(goal) == "" {
		return RouteDecision{Path: RouteCreateDomain}
	}

	// 1. 规则层（零 LLM 成本）
	if path := classifyByRules(n, goal); path != "" {
		return RouteDecision{Path: path, EnableSubdomain: path == RouteFullFourLayer}
	}

	// 2. LLM 兜底（轻量模型判定，复用 MetaAgent 的 CallLLM 超时/熔断机制）
	if n.modelFactory != nil && n.llmTracker != nil && !n.llmTracker.ShouldSkipLLM() {
		if path := n.classifyRouteLLM(ctx, goal); path != "" {
			return RouteDecision{Path: path, EnableSubdomain: path == RouteFullFourLayer}
		}
	}

	// 3. 安全兜底：RouteCreateDomain（= 当前复杂任务行为，零回归）
	return RouteDecision{Path: RouteCreateDomain, EnableSubdomain: false}
}

// classifyByRules 规则层路由（零 LLM 成本）。
//
// 命中则返回路径；未命中返回空串交由 LLM 兜底。
// 改造为基于多维 profile 的打分判定（替代单关键词命中），并保留
// shouldDirectExecute 作为兜底以兼容既有用例（但仅在 profile 未识别到
// 开放探索/多步骤/多动作信号时才走兜底，避免把复杂任务压到 direct_tool）。
func classifyByRules(n *MetaAgentNode, goal string) RoutePath {
	// 纯寒暄/常识 QA：MetaAgent 直接回答（0 工具）→ RouteDirectTool
	if n.isSimpleQuestion(goal) {
		return RouteDirectTool
	}

	// 多维打分：基于 profile 决策，规则层只放极窄高置信命中
	p := profileGoal(goal)
	p.IsGreeting = false // isSimpleQuestion 已在上一步处理
	if path := classifyByProfile(p); path != "" {
		return path
	}

	// 兼容兜底：查询/搜索/资讯类单工具任务（HTTPGet / Web 搜索）。
	// 仅在 profile 未识别到开放探索/多步骤/写/轻量/重动作时才走，避免
	// "分析现有代码找出问题并优化性能" 这类含开放探索词的目标被
	// shouldDirectExecute 的 "分析" 关键词误判为 direct_tool，
	// 也避免 "查询新闻改一下API" 这类含轻量动作的多动作目标被误判。
	if !p.OpenEnded && p.StepCount == 0 && !p.HasWriteAction && !p.HasHeavyAction && !p.HasLightAction && p.ActionCount <= 1 {
		if n.shouldDirectExecute(goal) {
			return RouteDirectTool
		}
	}

	return "" // 未命中，交 LLM 兜底
}

// isSingleToolRequest 判断是否为单次工具调用类请求（读文件/跑命令/查天气等）。
//
// 命中"读/运行/查/列"等动作 + 明确对象，且不含多步骤/写代码信号。
func isSingleToolRequest(goal string) bool {
	gl := strings.ToLower(goal)
	// 单工具动作词
	toolActions := []string{"读一下", "读取", "看一下", "查看", "列一下", "列出", "运行", "执行", "跑一下", "查天气", "查一下天气"}
	hit := false
	for _, a := range toolActions {
		if strings.Contains(gl, a) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	// 多步骤/写代码信号：命中则不是单工具
	complexSignals := []string{"然后", "接着", "并且", "同时", "并", "再", "之后", "且",
		"重构", "实现一个", "开发一个", "写一个", "修改"}
	for _, s := range complexSignals {
		if strings.Contains(gl, s) {
			return false
		}
	}
	return true
}

// isMultiDomainHint 判断是否含多领域并行信号。
//
// 命中"前端+后端"、"接口+页面"等跨领域组合，或显式"并行/同时"多目标。
func isMultiDomainHint(goal string) bool {
	gl := strings.ToLower(goal)
	// 跨领域组合关键词
	combos := [][]string{
		{"前端", "后端"},
		{"页面", "接口"},
		{"前端", "接口"},
		{"后端", "样式"},
		{"ui", "api"},
		{"数据库", "缓存"},
		{"数据库", "前端"},
		{"数据库", "后端"},
		{"算法", "前端"},
		{"算法", "后端"},
		{"api", "数据库"},
		{"服务", "前端"},
	}
	for _, c := range combos {
		if strings.Contains(gl, c[0]) && strings.Contains(gl, c[1]) {
			return true
		}
	}
	// 显式多目标分隔
	if strings.Contains(gl, "，同时") || strings.Contains(gl, "，并且") || strings.Contains(gl, "；") {
		return true
	}
	return false
}

// classifyRouteLLM LLM 兜底路由（轻量模型判定复杂度 + 路径 + 置信度）。
//
// 让轻量模型输出 complexity / path / confidence 三元组；解析后若置信度
// 低于 minRouteConfidence（默认 0.7）视为不可信，返回空串交安全兜底（RouteCreateDomain），
// 避免低置信度误判把复杂任务压到 direct_assistant 单助手路径。
// 未识别/超时/非法输出返回空串交安全兜底。
func (n *MetaAgentNode) classifyRouteLLM(ctx context.Context, goal string) RoutePath {
	prompt := `你是一名任务复杂度判定专家。请按以下两步分析用户目标，并严格按格式输出：

第一步：判断问题复杂度
- simple（简单问题）：仅需单次工具调用即可回答/完成，如读文件、运行命令、查天气、HTTPGet 抓取；或单领域简单修改，如修 CSS padding、改文案、调配置。
- complex（复杂问题）：需要多步骤规划、跨模块协作、领域拆分，如重构模块 API、设计数据库、前后端并行开发、全栈架构设计。

第二步：在对应复杂度下选择执行路径
- simple → direct_tool：单次工具调用或纯 QA，主 Agent 直接跑工具循环，无需创建任何子 Agent。
- simple → direct_assistant：单领域简单任务，主 Agent 直接创建一个专家助手执行，无需 DomainAgent 拆分。
- complex → create_domain：单领域复杂任务，需要创建 DomainAgent 进行任务拆分。
- complex → multi_domain：多领域并行任务，需要创建多个 DomainAgent。
- complex → full_four_layer：超复杂任务，子领域边界明显（API 层 + 数据库层 + 前端层），需要启用 SubDomainAgent 完整四层编排。

置信度（confidence）：0.0-1.0，表示你对路径选择的把握。低于 0.7 时系统会回退到更保守的 create_domain。

输出格式（严格遵循，不要其他文字）：
complexity: simple|complex
path: direct_tool|direct_assistant|create_domain|multi_domain|full_four_layer
confidence: 0.0-1.0

用户目标: ` + goal + `

请输出：`
	resp, err, timedOut := n.CallLLM(ctx, prompt, LLMCallOptions{
		Caller:      "MetaAgent/路由判定(轻量)",
		Lightweight: true,
		InjectSoul:  true,
		SoftTimeout: 30 * time.Second,
		HardTimeout: 90 * time.Second,
		Temperature: &routingTemperature,
	})
	if timedOut || err != nil || resp == "" {
		return ""
	}
	resp = strings.ToLower(strings.TrimSpace(resp))
	// 解析 path 与 confidence；容忍前后缀、空行与标点
	path := parseRoutePath(resp)
	if path == "" {
		return "" // 未识别路径，交安全兜底
	}
	// 置信度门控：低置信度不采纳 LLM 判定，交安全兜底走 create_domain
	conf := parseRouteConfidence(resp)
	if conf < minRouteConfidence {
		return ""
	}
	return path
}

// minRouteConfidence LLM 路由判定的最低置信度阈值。
// 低于此值视为不可信，回退到安全兜底 RouteCreateDomain。
const minRouteConfidence = 0.7

// parseRouteConfidence 从 LLM 输出中解析 confidence 数值。
//
// 支持格式："confidence: 0.8" / "confidence:0.8" / "置信度: 0.8"。
// 缺失或解析失败返回 0.0（视为低置信度，触发安全兜底）。
func parseRouteConfidence(resp string) float64 {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(line, "confidence:") && !strings.HasPrefix(line, "置信度:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "confidence:"))
		v = strings.TrimSpace(strings.TrimPrefix(v, "置信度:"))
		v = strings.TrimRight(v, ".。,，")
		var f float64
		if n, _ := fmt.Sscanf(v, "%f", &f); n == 1 {
			if f < 0 {
				return 0
			}
			if f > 1 {
				return 1
			}
			return f
		}
		return 0
	}
	return 0
}

// parseRoutePath 从 LLM 输出中解析路由路径。
//
// 支持两种格式：
//   - 结构化："complexity: simple\npath: direct_tool"
//   - 兜底：直接包含路径代号（兼容旧输出）。
func parseRoutePath(resp string) RoutePath {
	// 1. 尝试显式提取 "path: xxx" 行
	for _, line := range strings.Split(resp, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(line, "path:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "path:"))
		v = strings.TrimRight(v, ".。,，")
		switch v {
		case "direct_tool":
			return RouteDirectTool
		case "direct_assistant":
			return RouteDirectAssistant
		case "create_domain":
			return RouteCreateDomain
		case "multi_domain":
			return RouteMultiDomain
		case "full_four_layer":
			return RouteFullFourLayer
		}
	}
	// 2. 兜底：包含关键字即命中（兼容旧模型/非结构化输出），
	// 但需排除被否定的路径，例如 LLM 说"不应使用 direct_tool"时不能误判。
	switch {
	case strings.Contains(resp, "direct_tool") && !routePathNegated(resp, "direct_tool"):
		return RouteDirectTool
	case strings.Contains(resp, "direct_assistant") && !routePathNegated(resp, "direct_assistant"):
		return RouteDirectAssistant
	case strings.Contains(resp, "multi_domain") && !routePathNegated(resp, "multi_domain"):
		return RouteMultiDomain
	case strings.Contains(resp, "full_four_layer") && !routePathNegated(resp, "full_four_layer"):
		return RouteFullFourLayer
	case strings.Contains(resp, "create_domain") && !routePathNegated(resp, "create_domain"):
		return RouteCreateDomain
	}
	return ""
}

// routePathNegated 检查 resp 中 path 关键字前是否出现否定词（如"不应/不要/别/不用"）。
// 用于 parseRoutePath 的关键词兜底路径，避免"不应使用 direct_tool"被误判为 direct_tool。
func routePathNegated(resp, path string) bool {
	lower := strings.ToLower(resp)
	lowerPath := strings.ToLower(path)
	idx := strings.Index(lower, lowerPath)
	if idx < 0 {
		return false
	}
	window := 24 // 检查 path 前 24 个字符
	start := idx - window
	if start < 0 {
		start = 0
	}
	before := lower[start:idx]
	negations := []string{"不应", "不要", "别", "不用", "不是", "避免", "不建议", "不能"}
	for _, neg := range negations {
		if strings.Contains(before, neg) {
			return true
		}
	}
	return false
}
