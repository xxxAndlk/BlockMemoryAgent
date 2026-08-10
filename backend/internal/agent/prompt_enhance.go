package agent

// prompt_enhance.go 实现 TODO #36 用户输入自动提示词补全（Phase 0 纯规则版，无 LLM 依赖）：
//   - 意图分类（规则优先）：续跑/控制/诊断词命中；
//   - 消歧绑定：意图命中时把最近任务状态（看板失败任务 + 树失败/取消节点）拼进提示词，
//     让 MetaAgent 看到机器可读的"未完成事项"锚点——"重新执行"这类无宾语短指令
//     不再只能锚定上下文中最完整的信息（首条 user 原文）导致全量重跑（2026-08-10 事故根因 1）；
//   - 结构化格式化：【用户原始指令】原文逐字保留 +【系统补全】意图标签/绑定状态/建议，两段分离。
//
// 边界与安全（TODO 明确）：
//   - 只增不改：永不改写/替换用户原文，只做附加；模板固定、无自由生成；
//   - 高歧义不猜：无绑定对象时跳过消歧段，不编造；多候选并列时列出候选（MetaAgent 可 ask_user）；
//   - 全部失败可降级为原文直通：分类未命中 = 原文返回，零行为变化。

import (
	"fmt"
	"strings"
)

// IntentKind 输入意图分类（规则优先，Phase 0 无 LLM）。
type IntentKind int

const (
	// IntentNone 普通任务输入：不触发补全（零行为变化）。
	IntentNone IntentKind = iota
	// IntentResume 续跑意图：继续/重新执行/接着做等——绑定时优先引用最近失败任务。
	IntentResume
	// IntentControl 控制意图：停/取消/暂停等——绑定时给出当前在看任务供决策。
	IntentControl
	// IntentDiagnose 诊断意图：为什么/查一下/分析等——绑定时列出失败任务原因。
	IntentDiagnose
)

// String 返回意图的中文标签（注入模板用）。
func (k IntentKind) String() string {
	switch k {
	case IntentResume:
		return "续跑"
	case IntentControl:
		return "控制"
	case IntentDiagnose:
		return "诊断"
	default:
		return "普通任务"
	}
}

// intentRules 意图词表（中文优先，TODO 明确不做英文词表）。
// 组序即优先级：续跑词先于控制/诊断判定，避免"继续检查"类复合表达归错类。
// 词条子串匹配（"继续"覆盖"继续执行/继续刚才"），首命中生效。
var intentRules = []struct {
	kind  IntentKind
	words []string
}{
	{IntentResume, []string{"继续", "重新执行", "接着做", "接着来", "重跑", "续跑", "再来", "再执行", "继续做", "继续执行"}},
	{IntentControl, []string{"停止", "取消", "停下", "暂停", "终止", "别做了", "不用做了", "先停"}},
	{IntentDiagnose, []string{"为什么", "查一下", "检查", "分析", "诊断", "失败原因", "怎么回事", "咋回事", "看看"}},
}

// classifyIntent 按规则表分类输入文本；返回意图与命中词。
// 未命中返回 (IntentNone, "")，调用方直接原文直通。
func classifyIntent(text string) (IntentKind, string) {
	for _, rule := range intentRules {
		for _, w := range rule.words {
			if strings.Contains(text, w) {
				return rule.kind, w
			}
		}
	}
	return IntentNone, ""
}

// EnhanceTask 是绑定用的任务状态条目（看板子任务或树节点折叠）。
type EnhanceTask struct {
	Title  string
	Domain string
	Status string // 机器可读状态（failed / cancelled / pending / in_progress / done）
	Result string // 失败原因或完成结果（截断后注入）
}

// EnhanceState 是补全器的会话状态输入。
type EnhanceState struct {
	BoardGoal  string
	BoardState string // 看板整体状态（NEW/IN_PROGRESS/DONE/FAILED），空=无看板
	// PendingTasks 未完成任务（非终态）：续跑/控制意图的绑定候选。
	PendingTasks []EnhanceTask
	// FailedTasks 失败/被取消任务（含原因）：续跑/诊断意图的绑定对象（核心消歧数据）。
	FailedTasks []EnhanceTask
}

// EnhancePrompt 对用户输入做意图分类 + 消歧绑定 + 结构化格式化。
// 输出：
//   - 意图未命中（IntentNone）或开关关闭 → 原样返回原文（零行为变化）；
//   - 命中 → "【用户原始指令】\n<原文>\n\n【系统补全】\n意图: ...\n<状态段>\n<建议段>"。
//
// 只增不改：原文逐字保留在【用户原始指令】段；补全段全部为固定模板文本。
func EnhancePrompt(original string, st EnhanceState) string {
	kind, word := classifyIntent(original)
	if kind == IntentNone {
		return original
	}

	var sb strings.Builder
	sb.WriteString("【系统补全】（自动附加，非用户原话；仅辅助理解，请以【用户原始指令】为准）\n")
	sb.WriteString("意图: ")
	sb.WriteString(kind.String())
	sb.WriteString("（命中词: ")
	sb.WriteString(word)
	sb.WriteString("）\n")

	// 消歧绑定段：仅在有真实状态时输出，无绑定对象不编造。
	if st.BoardGoal != "" {
		sb.WriteString("当前目标: ")
		sb.WriteString(truncateRunes(strings.TrimSpace(st.BoardGoal), 120))
		sb.WriteString("\n")
	}
	if len(st.FailedTasks) > 0 {
		sb.WriteString("最近失败/中断任务:\n")
		for _, t := range st.FailedTasks {
			domain := t.Domain
			if domain == "" {
				domain = "-"
			}
			sb.WriteString(fmt.Sprintf("  - %s [%s] 状态=%s 原因: %s\n", t.Title, domain, t.Status, truncateRunes(strings.TrimSpace(t.Result), 200)))
		}
		sb.WriteString("建议: 优先继续最近失败/中断的任务（失败原因见上），不要重跑已完成或进行中的任务；如多个失败任务并列，先询问用户续跑哪一个。\n")
	} else if len(st.PendingTasks) > 0 {
		sb.WriteString("未完成任务: ")
		var titles []string
		for _, t := range st.PendingTasks {
			titles = append(titles, fmt.Sprintf("%s[%s]", t.Title, t.Domain))
		}
		sb.WriteString(strings.Join(titles, "、"))
		sb.WriteString("\n")
		sb.WriteString("建议: 任务未全部完成，继续推进未完成任务。\n")
	}
	return "【用户原始指令】\n" + original + "\n\n" + strings.TrimRight(sb.String(), "\n")
}
