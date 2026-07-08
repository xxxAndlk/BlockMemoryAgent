package graph

import (
	"strings"
)

// 本文件实现路由规则层的多维打分（替代单关键词命中）。
//
// 设计意图：原 classifyByRules 用 isSingleToolRequest / isSingleDomainSimpleTask
// 等独立关键词列表做命中，"修复" 一词即判 direct_assistant。导致
// "查看塔防游戏找出 bug 修复并优化" 这类多动作开放探索目标被误判为单领域简单任务，
// 单 Assistant 包揽复杂目标后上下文爆炸。
//
// 改造为多维 profile：步骤数 / 动作数 / 领域数 / 范围 / 开放度，组合判定。
// 规则层只放极窄高置信命中，其余交 LLM 兜底。

// taskProfile 多维任务画像。
type taskProfile struct {
	Goal string

	StepCount int // 步骤分隔信号数：然后/接着/同时/并/再/之后/；/数字序号
	ActionCount int // 不同动作动词数（去重）
	DomainCount int // 不同领域关键词数（去重）
	Scope int // 0=单文件/单对象, 1=目录, 2=workspace/项目/整个
	OpenEnded bool // 含开放探索词：找出/排查/分析/优化/查找/检查/探索/重构

	HasReadAction bool // 读/查看/看一下/列一下
	HasRunAction bool // 运行/执行/跑一下
	HasLightAction bool // 修复/改/调整/替换/重命名/添加/删除/修改
	HasHeavyAction bool // 设计/开发/实现/构建/搭建/重构/架构/重写
	HasWriteAction bool // 写/创建/生成/制作

	IsGreeting bool // 纯寒暄
	HasMultiDomainCombo bool // 跨领域组合命中（复用 isMultiDomainHint）
}

// stepSignals 步骤分隔信号词。命中任一即 StepCount+1。
// 注意：仅检测存在性（strings.Contains），不计算重复次数。
// 移除 "。" "；" "并" — 太常见，几乎所有中文句子都命中，会让单步任务误判为多步。
var stepSignals = []string{
	"然后", "接着", "并且", "同时", "再", "之后", "而后", "随后",
	"第一步", "第二步", "第三步", "首先", "其次", "最后",
	"1.", "2.", "3.", "1、", "2、", "3、",
}

// readActions 读类动作词。
var readActions = []string{"读一下", "读取", "看一下", "查看", "列一下", "列出", "浏览"}

// runActions 运行类动作词。
var runActions = []string{"运行", "执行", "跑一下", "启动"}

// lightActions 轻量编辑动作词（小改动，单助手可完成）。
var lightActions = []string{"修复", "改一下", "改个", "调整", "修改", "替换", "重命名", "添加", "删除", "改动"}

// heavyActions 重量级动作词（架构性，需 DomainAgent 拆分）。
var heavyActions = []string{"设计", "开发", "实现", "构建", "搭建", "重构", "架构", "重写", "编排", "集成"}

// writeActions 写入类动作词。
var writeActions = []string{"写", "创建", "生成", "制作", "新建", "写入"}

// openEndedMarkers 开放探索标记词。命中即 OpenEnded=true。
var openEndedMarkers = []string{"找出", "排查", "分析", "优化", "查找", "检查", "探索", "定位", "诊断", "评估", "审视"}

// domainKeywords 领域关键词。用于 DomainCount 计数。
var domainKeywords = []string{
	"前端", "后端", "数据库", "缓存", "ui", "api", "算法",
	"页面", "接口", "服务", "表结构", "模型",
}

// scopeMarkers 范围标记。命中即提升 Scope。
var scopeMarkers = []struct {
	words []string
	scope int
}{
	{[]string{"整个项目", "整个 workspace", "workspace", "项目", "全部", "所有"}, 2},
	{[]string{"目录", "文件夹", "模块"}, 1},
}

// profileGoal 对目标做多维打分。
//
// 职责：扫描 goal，统计步骤数/动作数/领域数/范围/开放度等维度，
// 返回 taskProfile 供 classifyByRules 决策。
//
// 参数：
//   - goal：用户目标原文。
//
// 返回：填好的 taskProfile。
func profileGoal(goal string) taskProfile {
	p := taskProfile{Goal: goal}
	if goal == "" {
		return p
	}
	gl := strings.ToLower(goal)

	// 步骤数：每个信号词出现即 +1（不去重，"然后...然后" 也是多步骤）
	for _, s := range stepSignals {
		if strings.Contains(gl, s) {
			p.StepCount++
		}
	}

	// 动作维度：分别标 Has* 标志，ActionCount 统计不同动作类别
	for _, a := range readActions {
		if strings.Contains(gl, a) {
			p.HasReadAction = true
			break
		}
	}
	for _, a := range runActions {
		if strings.Contains(gl, a) {
			p.HasRunAction = true
			break
		}
	}
	for _, a := range lightActions {
		if strings.Contains(gl, a) {
			p.HasLightAction = true
			break
		}
	}
	for _, a := range heavyActions {
		if strings.Contains(gl, a) {
			p.HasHeavyAction = true
			break
		}
	}
	for _, a := range writeActions {
		if strings.Contains(gl, a) {
			p.HasWriteAction = true
			break
		}
	}
	// ActionCount = 不同动作类别命中数（0-5）
	if p.HasReadAction {
		p.ActionCount++
	}
	if p.HasRunAction {
		p.ActionCount++
	}
	if p.HasLightAction {
		p.ActionCount++
	}
	if p.HasHeavyAction {
		p.ActionCount++
	}
	if p.HasWriteAction {
		p.ActionCount++
	}

	// 开放度
	for _, m := range openEndedMarkers {
		if strings.Contains(gl, m) {
			p.OpenEnded = true
			break
		}
	}

	// 领域数：去重统计
	seen := map[string]bool{}
	for _, d := range domainKeywords {
		if strings.Contains(gl, d) && !seen[d] {
			seen[d] = true
			p.DomainCount++
		}
	}

	// 范围：取命中的最高 scope
	for _, sm := range scopeMarkers {
		for _, w := range sm.words {
			if strings.Contains(gl, w) {
				if sm.scope > p.Scope {
					p.Scope = sm.scope
				}
			}
		}
	}

	// 跨领域组合：复用既有 isMultiDomainHint
	p.HasMultiDomainCombo = isMultiDomainHint(goal)

	return p
}

// classifyByProfile 基于多维 profile 决定路由路径。
//
// 决策规则（从严到宽）：
//  1. direct_tool：单读或单运行动作 + 无多步骤 + 无开放探索 + 无写动作 + 单领域/无领域 + 范围≤1
//  2. direct_assistant：仅轻量编辑 + 无重量级动作 + 无多步骤 + 无开放探索 + 单领域 + 范围≤1
//  3. multi_domain：跨领域组合命中
//  4. 其余返回 "" 交 LLM 兜底
//
// 设计要点：direct_assistant 的"再评估"已内化到规则——含多步骤/开放探索/多动作即不命中。
// direct_tool 极窄：必须单动作且无任何探索/写/多步骤信号。
func classifyByProfile(p taskProfile) RoutePath {
	// 寒暄走 direct_tool（MetaAgent 直接回答）
	if p.IsGreeting {
		return RouteDirectTool
	}

	// multi_domain：跨领域组合优先判定
	if p.HasMultiDomainCombo {
		return RouteMultiDomain
	}

	// direct_tool：极窄条件
	// - ActionCount==1（恰好一类动作）
	// - 仅读或仅运行
	// - 无写/轻量/重量级动作
	// - 无多步骤
	// - 无开放探索
	// - 范围≤1（不是整个项目/workspace）
	// - 领域数≤1
	if p.ActionCount == 1 && (p.HasReadAction || p.HasRunAction) &&
		!p.HasWriteAction && !p.HasLightAction && !p.HasHeavyAction &&
		p.StepCount == 0 && !p.OpenEnded && p.Scope <= 1 && p.DomainCount <= 1 {
		return RouteDirectTool
	}

	// direct_assistant：单领域小改动
	// - 含轻量动作但无重量级动作
	// - 动作数≤2（修复 + 改一下 等）
	// - 无多步骤
	// - 无开放探索
	// - 单领域
	// - 范围≤1
	if p.HasLightAction && !p.HasHeavyAction &&
		p.ActionCount <= 2 && p.StepCount == 0 && !p.OpenEnded &&
		p.DomainCount <= 1 && p.Scope <= 1 {
		return RouteDirectAssistant
	}

	// 其余交 LLM 兜底
	return ""
}
