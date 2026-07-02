package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"strings"
)

// shouldSplitToSubDomains 判断是否需要拆分为子领域。
//
// v3 路由重构后改为自适应：仅当 MetaAgent 路由判定为 RouteFullFourLayer（state.EnableSubdomain=true）
// 且任务确实跨子领域边界时才启用，避免单领域任务不必要的第三层 overhead。
//
// 判定顺序：
//  1. state.EnableSubdomain=false → 直接 false（路由层已决定不进四层）
//  2. 任务数 ≤ 1 → false（单任务无需拆子领域）
//  3. 跨子领域边界检测：命中不同子领域关键词 → true；否则 false
//  4. （可选）LLM 兜底判断
//
// 返回：是否拆分。
func (n *DomainAgentNode) shouldSplitToSubDomains(ctx context.Context, state *types.ThreeLayerState, tasks []string) bool {
	// 路由层未启用 SubDomain → 直接返回 false
	if !state.EnableSubdomain {
		return false
	}
	// 单任务无需拆子领域
	if len(tasks) <= 1 {
		return false
	}
	// 规则层：跨子领域边界检测（命中不同子领域关键词才拆）
	if detectSubdomainBoundaries(tasks) {
		n.emit(ctx, "think", fmt.Sprintf("检测到 %d 个任务跨子领域边界，启用 SubDomain 拆分", len(tasks)))
		return true
	}
	// 规则未命中：可选 LLM 兜底（模型可用时）
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		if n.shouldSplitWithLLM(ctx, state.CurrentDomain, tasks) {
			n.emit(ctx, "think", "LLM 判定需要拆分子领域，启用 SubDomain")
			return true
		}
	}
	return false
}

// detectSubdomainBoundaries 规则层子领域边界检测。
//
// 命中 ≥2 个不同子领域关键词组时返回 true。关键词组覆盖常见前后端/数据/接口分层。
func detectSubdomainBoundaries(tasks []string) bool {
	// 子领域关键词组：每组代表一个子领域
	groups := [][]string{
		{"api", "接口", "路由", "handler", "controller"},
		{"数据库", "database", "model", "schema", "sql", "表结构"},
		{"前端", "frontend", "页面", "ui", "组件", "css", "样式"},
		{"后端", "backend", "服务", "service", "逻辑"},
		{"测试", "test", "用例"},
	}
	joined := strings.ToLower(strings.Join(tasks, " "))
	hitGroups := 0
	for _, g := range groups {
		for _, kw := range g {
			if strings.Contains(joined, kw) {
				hitGroups++
				break // 每组只计一次
			}
		}
	}
	return hitGroups >= 2
}

// shouldSplitWithLLM 使用LLM判断是否需要拆分子领域。
//
// 职责：调用 LLM 判断领域是否包含多个独立模块或可并行的任务组。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//   - tasks：子任务列表
//
// 返回：LLM 回答"是"/"yes"返回 true；否则 false。
//
// 注意：当前 shouldSplitToSubDomains 恒返回 false，本函数未被主路径调用，保留以备启用。
func (n *DomainAgentNode) shouldSplitWithLLM(ctx context.Context, domain string, tasks []string) bool {
	// 无模型工厂则直接返回 false
	if n.modelFactory == nil {
		return false
	}

	// 构造判断 prompt：要求只回答"是"或"否"
	prompt := fmt.Sprintf(`判断以下领域是否需要拆分为多个子领域并行处理。

领域: %s
子任务数量: %d
子任务列表:
%s

如果该领域包含多个独立模块（如前端页面的头部/列表/底部），或者子任务可以明确分为2-4个并行组，回答"是"。
否则回答"否"。
只回答"是"或"否"。`, domain, len(tasks), strings.Join(tasks, "\n"))

	// 调用 LLM（caller 为"拆分判断"）
	resp, err, _ := n.callLLMAs(ctx, "DomainAgent/拆分判断", prompt)
	if err != nil {
		return false // 调用失败：不拆分
	}

	// 响应含"是"或"yes"即视为需要拆分
	return strings.Contains(resp, "是") || strings.Contains(strings.ToLower(resp), "yes")
}

// handleSubDomainSplit 拆分子领域并调度（支持依次调度多个）。
//
// 职责：
//   - 首次进入时初始化子领域列表（inferSubDomains）
//   - 依次为每个子领域创建 SubDomainAgent 实例
//   - 通过 PushCallStack 派发任务，切换到子领域节点
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例
//
// 返回：更新后的 state；子领域全部派发完后返回 ActionContinue。
//
// 注意：当前 shouldSplitToSubDomains 恒返回 false，本函数未被主路径调用，保留以备启用。
func (n *DomainAgentNode) handleSubDomainSplit(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance) (*types.ThreeLayerState, error) {
	// 取当前块
	block := state.ActiveBlocks[state.CurrentBlockID]

	// 首次拆分：初始化子领域列表
	if block != nil && !block.SubDomainSplit {
		block.SubDomainSplit = true                       // 标记已初始化，避免重复
		subDomains := n.inferSubDomains(ctx, inst.Domain) // 推断子领域
		for _, sd := range subDomains {
			// 收集子领域名到列表
			block.SubDomainList = append(block.SubDomainList, sd.Name)
		}
	}

	// 子领域全部处理完：继续图循环
	if block == nil || block.SubDomainIndex >= len(block.SubDomainList) {
		state.NextAction = enums.ActionContinue // 继续图循环
		return state, nil
	}

	// 取当前子领域名并推进游标
	subDomainName := block.SubDomainList[block.SubDomainIndex] // 取当前子领域
	block.SubDomainIndex++                                     // 推进游标

	// 创建 SubDomainAgent 实例
	subInst, err := n.factory.CreateSubDomainAgent(ctx, state.SessionID, subDomainName, state.DomainGoal, n.instID)
	if err != nil {
		// 创建失败：打印日志并继续
		fmt.Printf("[DomainAgent] create subdomain agent %s failed: %v\n", subDomainName, err)
		state.NextAction = enums.ActionContinue
		return state, nil
	}

	// 构造调用请求：领域目标作为任务，附带领域/子领域/块ID等上下文
	callReq := &types.CallRequest{
		ID:       fmt.Sprintf("call_%s_%d", subInst.ID, len(state.CallStack)), // 唯一调用ID
		CallerID: n.instID,                                                    // 调用者=本 Domain
		CalleeID: subInst.ID,                                                  // 被调用者=子领域
		Task:     state.DomainGoal,                                            // 任务=领域目标
		Context: map[string]any{
			"domain":          inst.Domain,          // 父领域名
			"sub_domain":      subDomainName,        // 子领域名
			"block_id":        state.CurrentBlockID, // 所属块ID
			"domain_goal":     state.DomainGoal,     // 领域目标
			"session_summary": state.SessionSummary, // 会话摘要（跨块共享）
		},
		Priority: 5, // 默认优先级
	}

	// 入栈调用请求并切换到子领域节点
	state.PushCallStack(callReq)          // 压入调用栈
	state.NextAction = enums.ActionSwitch // 切换到子领域节点
	state.TargetRoleID = subInst.ID       // 路由目标
	return state, nil
}

// inferSubDomains 推断子领域列表（优先LLM，回退规则）。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//
// 返回：子领域列表；LLM 失败回退规则。
func (n *DomainAgentNode) inferSubDomains(ctx context.Context, domain string) []DomainInfo {
	// 有模型工厂则优先 LLM 推断
	if n.modelFactory != nil {
		if subs := n.inferSubDomainsWithLLM(ctx, domain); len(subs) > 0 {
			return subs // 返回 LLM 推断结果
		}
	}
	// 回退规则
	return n.inferSubDomainsByRules(domain)
}

// inferSubDomainsWithLLM 使用LLM推断子领域。
//
// 职责：调用 LLM 把领域拆成 2-4 个独立子领域。
//
// 参数：
//   - ctx：请求上下文
//   - domain：领域名
//
// 返回：子领域列表；LLM 失败或返回空返回 nil。
func (n *DomainAgentNode) inferSubDomainsWithLLM(ctx context.Context, domain string) []DomainInfo {
	// 无模型工厂则返回 nil
	if n.modelFactory == nil {
		return nil
	}

	// 标识调用者
	caller := "DomainAgent/子领域推断"
	// 构造拆分 prompt：要求 2-4 个简短独立的子领域名
	prompt := fmt.Sprintf(`将以下领域拆分为2-4个独立的子领域。

领域: %s

要求:
- 每个子领域名称简短（2-6个字）
- 子领域之间尽量独立，可并行处理
- 只输出子领域名称，每行一个，不要编号，不要其他内容

子领域:`, domain)

	// 调用 LLM
	resp, err, _ := n.callLLMAs(ctx, caller, prompt)
	if err != nil || resp == "" {
		return nil
	}

	// 解析响应：逐行清洗，构造 DomainInfo
	var result []DomainInfo
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)        // 去首尾空白
		line = strings.TrimPrefix(line, "- ") // 去 "- " 前缀
		// 去除行首编号
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:] // 截掉编号前缀
		}
		// 长度 >=2 视为有效
		if len(line) >= 2 {
			// 构造 DomainInfo，目标自动生成
			result = append(result, DomainInfo{
				Name: line,
				Goal: fmt.Sprintf("处理%s相关的子任务", line),
			})
		}
	}
	return result
}

// inferSubDomainsByRules 基于规则推断子领域。
//
// 职责：LLM 不可用时的回退，按领域名关键词匹配前端组件模板。
//
// 参数：
//   - domain：领域名
//
// 返回：子领域列表；无匹配时返回单元素（领域名+"子任务"）。
func (n *DomainAgentNode) inferSubDomainsByRules(domain string) []DomainInfo {
	// 商城/页面类：拆为头部/列表/底部三个子领域
	if strings.Contains(domain, "商城") || strings.Contains(domain, "页面") {
		return []DomainInfo{
			{Name: "首页头部", Goal: "修复头部导航样式问题"}, // 头部子领域
			{Name: "商品列表", Goal: "修复商品列表布局问题"}, // 列表子领域
			{Name: "底部导航", Goal: "修复底部导航样式问题"}, // 底部子领域
		}
	}
	// 默认：单子领域
	return []DomainInfo{{Name: domain + "子任务", Goal: "执行细分任务"}} // 兜底单子领域
}
