package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/types"
	"log"
	"strings"
)

// DomainInfo 领域信息。
// 由 analyzeDomains 产出，描述一个待创建的领域。
type DomainInfo struct {
	Name string // 领域名（简短，2-6 字）
	Goal string // 领域目标
}

// analyzeDomains 分析用户目标，确定需要的领域（优先LLM，回退规则）。
//
// 职责：
//   - 取领域目标（优先 DomainGoal，回退 SessionSummary）
//   - 加载历史与对话段落
//   - 调 LLM 分析领域（输出 JSON 数组）
//   - 解析失败/超时则回退规则
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 返回：领域列表；LLM 不可用或无目标时回退规则。
func (n *MetaAgentNode) analyzeDomains(ctx context.Context, state *types.ThreeLayerState) []DomainInfo {
	// 取目标；DomainGoal 为空则用 SessionSummary
	goal := state.DomainGoal
	if goal == "" {
		goal = state.SessionSummary
	}

	messagesSection := n.loadMessagesSection(state)

	// 尝试使用LLM分析领域
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		n.emit(ctx, "llm", "调用 LLM 进行领域分析...")
		// 构造分析 prompt：要求 JSON 数组，含指代词时优先创建"检索历史与文件"领域
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个多Agent系统的领域分析器。请分析以下用户目标，确定需要哪些业务领域来协作完成。

用户目标: %s
%s
%s
要求:
- 每个领域名称简短（2-6个字），禁止使用"通用"作为领域名——必须根据目标语义给出具体领域名（如"AI股票分析"、"贪吃蛇游戏"、"数据库检查"）
- 领域之间应该尽量独立
- 若用户目标涉及"查找/刚才/上次"等指代词，应优先创建一个"检索历史与文件"领域
- 简单查询/搜索类目标只需一个领域即可，不要强行拆分
- 输出JSON数组格式: [{"name":"领域名","goal":"该领域需要完成的目标"}]
- 只输出JSON数组，不要代码块标记，不要任何解释文字

领域列表:`, goal, fmtEnvSection(), messagesSection))
		if !timedOut && err == nil && resp != "" {
			n.emit(ctx, "think", "LLM 返回领域分析结果，正在解析")
			// 解析 JSON 为领域列表
			if domains := n.parseDomainsFromLLM(resp); len(domains) > 0 {
				return domains // 返回 LLM 解析的领域
			}
			// 解析失败：推送详情
			n.emitDetail(ctx, "think", "LLM 返回内容无法解析为领域列表", truncateStr(resp, 200))
		}
		// 超时或失败：推送事件并回退规则
		if timedOut {
			n.emit(ctx, "error", "领域分析 LLM 调用超时，回退到规则")
			log.Printf("[MetaAgent] LLM 调用超时(领域分析阶段)，使用规则兜底。%s\n", n.llmTracker.StatsString())
		} else if err != nil {
			n.emitDetail(ctx, "error", "领域分析 LLM 调用失败: "+err.Error(), "")
		}
	}

	// 回退规则
	n.emit(ctx, "think", "回退到规则方式分析领域")
	return n.analyzeDomainsByRules(goal)
}

// parseDomainsFromLLM 从LLM响应解析领域列表。
//
// 职责：
//   - 用 extractJSON 抽取 JSON 片段
//   - 反序列化为 [{name, goal}] 数组
//   - 过滤空 name 的条目
//
// 参数：
//   - resp：LLM 返回的原始文本
//
// 返回：领域列表；解析失败或为空返回 nil。
func (n *MetaAgentNode) parseDomainsFromLLM(resp string) []DomainInfo {
	// 抽取 JSON 片段（LLM 可能附带多余文本）
	jsonStr := extractJSON(resp)
	// 兜底：若 extractJSON 返回单个对象，包成数组再解析
	trimmed := strings.TrimSpace(jsonStr)
	if strings.HasPrefix(trimmed, "{") {
		jsonStr = "[" + trimmed + "]"
	}
	// 反序列化为匿名结构数组
	var rawDomains []struct {
		Name string `json:"name"` // 领域名
		Goal string `json:"goal"` // 领域目标
	}
	if err := json.Unmarshal([]byte(jsonStr), &rawDomains); err != nil || len(rawDomains) == 0 {
		// 解析失败或为空：返回 nil 触发回退
		return nil
	}

	// 过滤空 name 并转换为 DomainInfo
	var domains []DomainInfo
	for _, d := range rawDomains {
		// 跳过空 name 的条目
		if d.Name != "" {
			domains = append(domains, DomainInfo{Name: d.Name, Goal: d.Goal})
		}
	}
	return domains
}

// analyzeDomainsByRules 基于关键词规则的领域分析。
//
// 职责：LLM 不可用时的回退，按目标关键词匹配预设领域模板。
//
// 参数：
//   - goal：领域目标
//
// 返回：领域列表；无匹配时返回单元素领域（名称从 goal 推断，不再一律"通用"）。
func (n *MetaAgentNode) analyzeDomainsByRules(goal string) []DomainInfo {
	var domains []DomainInfo

	// 按关键词匹配预设领域
	if strings.Contains(goal, "商城") || strings.Contains(goal, "页面") {
		domains = append(domains, DomainInfo{Name: "商城页面", Goal: goal}) // 商城/页面领域
	}
	if strings.Contains(goal, "购物车") || strings.Contains(goal, "购买") {
		domains = append(domains, DomainInfo{Name: "购物模块", Goal: goal}) // 购物模块领域
	}
	if strings.Contains(goal, "订单") {
		domains = append(domains, DomainInfo{Name: "订单模块", Goal: goal}) // 订单模块领域
	}
	if strings.Contains(goal, "用户") || strings.Contains(goal, "登录") {
		domains = append(domains, DomainInfo{Name: "用户模块", Goal: goal}) // 用户模块领域
	}

	// 无匹配：从 goal 截取前若干字符作为领域名，避免一律叫"通用"
	if len(domains) == 0 {
		name := inferDomainName(goal)
		domains = append(domains, DomainInfo{Name: name, Goal: goal}) // 兜底单领域
	}

	return domains
}

// inferDomainName 从 goal 文本启发式推断领域名，避免一律用"通用"。
// 取前若干个有意义的字符（中文按 rune 计，最多 6 字），去除常见动词前缀。
func inferDomainName(goal string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		return "通用"
	}
	// 去掉常见动词前缀，让领域名更贴近主题
	prefixes := []string{"完成", "请", "帮我", "帮助我", "实现", "做", "写", "创建", "查询", "查一下", "查下", "搜索", "搜一下"}
	for _, p := range prefixes {
		if strings.HasPrefix(g, p) {
			g = strings.TrimSpace(strings.TrimPrefix(g, p))
			break
		}
	}
	if g == "" {
		return "通用"
	}
	// 按 rune 截取前 6 个字符
	rs := []rune(g)
	if len(rs) > 6 {
		rs = rs[:6]
	}
	return string(rs)
}
