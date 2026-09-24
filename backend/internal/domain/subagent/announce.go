package subagent

// announce.go announce 边界协议（TODO #22②，对标 Hermes 委派预算 + 规范化回灌）。
//
// 三件套：
//  1. 回灌规范化：子→父回传统一包成【回报】信封（Result+Status+Notes+统计行），
//     逐级上灌纪律不变（notify 只发直接父级，跨域走黑板——与 #2 正交，不动 mailbox）；
//  2. 回灌预算公式：min(静态封顶, 父剩余余量×0.5÷子数)，floor 2K——高扇出时每份
//     回报自动收窄，防 N 个子 Agent 回传同时灌满父上下文；
//  3. 派发前上下文预算：前缀+任务超 fork 硬顶转 isolated（前缀全弃、不硬灌）。
//
// minimal 提示词面（人格/用户画像不入子上下文）由既有装配保证：persona/profile
// 仅注入 MetaAgent（bootstrap metaPersona），runSubAgentOnce/buildDomainAgent 不下发
// ——本协议不重复建设，仅在信封层保持"子 Agent 回报只含任务面"。

import (
	"fmt"
	"strings"
)

// 回灌预算参数（rune 即 token 粗估口径，CJK 1:1，与 EstimateTokens 一致）。
const (
	// announceStaticCapRunes 静态封顶：单份回报进父邮箱的正文上限（沿用旧固定阈值）。
	announceStaticCapRunes = 4000
	// announceBudgetFloorRunes 预算下限（Hermes floor 2K）：再挤也不低于此值，
	// 保证子 Agent 最小可读结论（纯验证任务也有一行结论的空间）。
	announceBudgetFloorRunes = 2000
	// announceParentRemainingTokens 父剩余余量基准（对齐 context_token_budget 默认150K）。
	// 精确余量需父历史遥测，dispatcher 拿不到——取配置基准作保守上界，公式在高扇出时生效。
	announceParentRemainingTokens = 150000
	// announceDigestRunes 超预算落盘后邮箱保留的摘要头长度。
	announceDigestRunes = 1500
)

// spawnForkHardCapRunes 派发 fork 上下文硬顶（TODO #22②，对标 Hermes 父 fork 100K 硬顶）：
// 注入前缀+任务合计超此值转 isolated（前缀全弃不硬灌），只带任务正文与一句隔离说明。
const spawnForkHardCapRunes = 100000

// announceBudgetRunes 回灌预算公式：min(静态封顶, 父剩余余量×0.5÷子数)，floor 2K。
// childCount<=0 按 1。返回值单位 rune。
func announceBudgetRunes(childCount int) int {
	if childCount <= 0 {
		childCount = 1
	}
	b := announceParentRemainingTokens / 2 / childCount
	if b > announceStaticCapRunes {
		b = announceStaticCapRunes
	}
	if b < announceBudgetFloorRunes {
		b = announceBudgetFloorRunes
	}
	return b
}

// deriveAnnounceStatus 从回报文本机械推导状态行（调用点零改动）：
// 机读失败标记优先（unverified/verify_missing → delivered-unverified 三态黄），
// 其次"部分完成"，否则 done。
func deriveAnnounceStatus(summary string) string {
	if m := failureMarkerRe.FindStringSubmatch(summary); m != nil {
		switch m[1] {
		case "unverified", "verify_missing":
			return "delivered-unverified"
		default:
			return "failed"
		}
	}
	if strings.Contains(summary, "部分完成") {
		return "partial"
	}
	return "done"
}

// buildAnnounce 组装规范化回报信封（TODO #22②）：Result+Status+Notes+统计行。
// 布局纪律：summary 原文（含机读失败标记首行）置顶不动——`^` 锚定的机读消费方
//（failureMarkerRe/父 LLM 提示词口径）依赖标记在正文首行；信封以尾部收口块追加
// Status/Notes/统计行。budget<=0 用 announceBudgetRunes(1)。
func (d *Dispatcher) buildAnnounce(parentID, subAgentID, summary string, files []string, budget int) string {
	if budget <= 0 {
		budget = announceBudgetRunes(1)
	}
	result := d.returnBodyForBudget(subAgentID, summary, budget)
	notes := ""
	if len(files) > 0 {
		shown := capFiles(files)
		notes = fmt.Sprintf("修改文件 %d 个（%s）", len(files), strings.Join(shown, ", "))
	} else {
		notes = "无文件修改"
	}
	stats := fmt.Sprintf("统计: 回报 %d runes | 修改文件 %d 个", runeLen(summary), len(files))
	return fmt.Sprintf("%s\n\n【回报】\n- Status: %s\n- Notes: %s\n- %s",
		result, deriveAnnounceStatus(summary), notes, stats)
}

// capSpawnPrefixes 派发前上下文预算（TODO #22②）：前缀+任务合计超 fork 硬顶时
// 转 isolated——前缀全弃（不截断硬灌），任务正文追加一行隔离说明。
// 返回 (keptPrefixes, task, isolated)。
func capSpawnPrefixes(prefixes []string, task string) ([]string, string, bool) {
	total := runeLen(task)
	for _, p := range prefixes {
		total += runeLen(p)
	}
	if total <= spawnForkHardCapRunes {
		return prefixes, task, false
	}
	task = task + "\n\n【上下文预算】父侧注入超 fork 硬顶（" + fmt.Sprintf("%d", spawnForkHardCapRunes) +
		" runes），已转 isolated 派发：共享记忆/召回/项目自述未注入。需要背景时用 ReadFile/SearchInFiles 按需获取。"
	return nil, task, true
}
