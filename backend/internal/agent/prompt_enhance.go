package agent

// prompt_enhance.go 实现 TODO #36 用户输入自动提示词补全 + TODO #39 误判根治四层管线：
//   - L0 输入形态闸门（规则，零成本）：非"控制类短指令"形态直接直通，不进意图分类；
//     长文本/多行/编号列表/【】段标记均为任务描述特征（2026-08-11 事故输入全被拦）；
//   - L1 分级规则：强词（会话控制专用语）句中命中即高置信直出；弱词（日常高频词）
//     仅全句匹配（高置信）或句首匹配（低置信，交 L2 仲裁）；
//   - L2 轻量模型仲裁（仅灰区）：L1 低置信（句首弱词）或 L1 未命中但句中有弱词信号时
//     调四分类；仲裁判 none 直通（不猜），超时/失败降级 L1 规则结果；
//   - L3 输出防护：忽略声明 + 状态交叉验证（resume 无绑定对象直通）+ 仲裁命中不给强引导建议。
//
// 边界与安全：
//   - 只增不改：永不改写/替换用户原文，只做附加；模板固定、无自由生成；
//   - 全部失败路径降级原文直通，绝不阻塞用户输入（L2 最坏延迟 = ArbiterTimeout）；
//   - 消歧绑定：意图命中时把最近任务状态（看板失败任务 + 树失败/取消节点）拼进提示词。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// 默认参数（config 可覆盖）：
const (
	// DefaultEnhanceMaxInputRunes L0 闸门长度上限（rune）：续跑/控制/诊断本质是短指令
	// （实证 ≤15 字），超过视为任务描述，跳过意图分类。
	DefaultEnhanceMaxInputRunes = 30
	// DefaultEnhanceLLMTimeout L2 仲裁单次超时：轻量模型为推理系首 token 慢，
	// 宁可短超时降级也不阻塞用户输入。
	DefaultEnhanceLLMTimeout = 10 * time.Second
)

// IntentKind 输入意图分类。
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

// IntentArbiter 是 L2 轻量模型仲裁器（TODO #39，依赖注入便于测试 mock）：
// 对灰区输入做四分类（none/resume/control/diagnose）。返回错误（含低置信）
// 一律按 IntentNone 处理——宁漏判不误判。
type IntentArbiter func(ctx context.Context, text string) (IntentKind, error)

// strongWords 强词：会话控制专用语，句中命中即高置信（L1 直出，不调 LLM）。
var strongWords = map[IntentKind][]string{
	IntentResume: {"重新执行", "接着做", "接着来", "重跑", "续跑", "再执行", "再来"},
}

// weakWords 弱词：日常高频词（任务描述也常用），仅全句匹配（高置信）或句首匹配
// （低置信→L2）才命中；句中命中仅作 L2 仲裁的弱信号，不直接判意图。
// 2026-08-11 事故即"1.继续游戏"子串命中"继续"被误判续跑——弱词绝不子串直判。
var weakWords = map[IntentKind][]string{
	IntentResume:   {"继续"},
	IntentControl:  {"停止", "取消", "停下", "暂停", "终止", "别做了", "不用做了", "先停"},
	IntentDiagnose: {"为什么", "查一下", "检查", "分析", "诊断", "失败原因", "怎么回事", "咋回事", "看看"},
}

// ruleConfidence L1 命中的置信度分级（决定是否进 L2、是否给强引导建议）。
type ruleConfidence int

const (
	confNone ruleConfidence = iota
	// confWeak 句首弱词命中：低置信，有仲裁器时交 L2 复核。
	confWeak
	// confStrong 强词/全句弱词命中：高置信，直出。
	confStrong
)

// numberedListRe 匹配编号列表项（"1." "2、" "3)" 等，前面是行首或非数字字符）。
// 编号列表是任务描述的高频特征（2026-08-11 事故输入即多行编号列表）。
var numberedListRe = regexp.MustCompile(`(^|[^\d])\d+[\.、．)）]`)

// EnhanceOptions 补全管线的可调参数。
type EnhanceOptions struct {
	// MaxInputRunes L0 闸门长度上限（rune）；<=0 用 DefaultEnhanceMaxInputRunes。
	MaxInputRunes int
	// Arbiter L2 仲裁器；nil 关闭 L2（降级纯规则，Phase 0 行为）。
	Arbiter IntentArbiter
	// ArbiterTimeout L2 单次超时；<=0 用 DefaultEnhanceLLMTimeout。
	ArbiterTimeout time.Duration
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

// EnhancePrompt 保持 Phase 0 签名：纯规则版（L0+L1+L3），无 LLM 依赖。
// 等价于 EnhancePromptWithOptions 传空 EnhanceOptions。
func EnhancePrompt(original string, st EnhanceState) string {
	out, _ := EnhancePromptWithOptions(context.Background(), original, st, EnhanceOptions{})
	return out
}

// EnhancePromptWithOptions 执行 TODO #39 四层管线（L0 闸门 → L1 规则 → L2 仲裁 → L3 输出防护）。
// 返回补全后的文本与判定路径事件备注（gate_skip / rule_strong / rule_weak /
// llm_hit / llm_miss / llm_timeout / llm_error；直通且无判定时为空串，供可观测性落事件）。
func EnhancePromptWithOptions(ctx context.Context, original string, st EnhanceState, opts EnhanceOptions) (string, string) {
	maxRunes := opts.MaxInputRunes
	if maxRunes <= 0 {
		maxRunes = DefaultEnhanceMaxInputRunes
	}

	// L0 输入形态闸门：非短指令形态直接直通（本次事故输入：数百字多行编号列表）。
	if !passesInputGate(original, maxRunes) {
		return original, "gate_skip"
	}

	// L1 分级规则。
	kind, word, conf, weakSignal := classifyByRules(original, maxRunes)

	// L2 轻量模型仲裁：仅灰区（L1 低置信句首弱词命中 / L1 未命中但句中有弱词信号）且
	// 仲裁器已接线。仲裁明确判 none（llm_miss）→ 直通（宁漏判不误判，不猜）；
	// 仲裁超时/失败 → 降级回 L1 规则结果（Phase 0 行为不回归）。
	note := ""
	l2Candidate := conf == confWeak || (conf == confNone && weakSignal)
	if l2Candidate && opts.Arbiter != nil {
		k, n := arbiterDecide(ctx, opts.Arbiter, opts.ArbiterTimeout, original)
		note = n
		switch {
		case k != IntentNone:
			kind, word, conf = k, "LLM", confWeak
		case n == "llm_miss":
			return original, note
		default: // llm_timeout / llm_error：降级 L1 结果
		}
	}
	if kind == IntentNone {
		return original, note
	}
	if note == "" {
		switch conf {
		case confStrong:
			note = "rule_strong"
		default:
			note = "rule_weak"
		}
	}
	return buildEnhancement(original, st, kind, word, conf), note
}

// passesInputGate L0 输入形态闸门：控制类指令本质是短单行无结构文本。
// 返回 false = 直通（不分类不补全）。
func passesInputGate(text string, maxRunes int) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if len([]rune(t)) > maxRunes {
		return false
	}
	if strings.ContainsAny(t, "\n\r") {
		return false
	}
	if numberedListRe.MatchString(t) {
		return false
	}
	if strings.Contains(t, "【") || strings.Contains(t, "】") {
		return false
	}
	return true
}

// stripPunct 去空白与标点，只留字母/数字/汉字，用于弱词的全句/句首匹配。
func stripPunct(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// classifyByRules L1 分级规则匹配：
//   - 强词句中命中 → (kind, word, confStrong)；
//   - 弱词全句匹配（去标点后整句即词）→ confStrong；
//   - 弱词句首匹配（且全长 ≤ maxRunes）→ confWeak（低置信，交 L2）；
//   - 弱词句中命中 → 仅置 weakSignal（L2 候选），不直判；
//
// 组序即优先级：续跑词先于控制/诊断判定（"继续检查"类复合表达归续跑）。
func classifyByRules(text string, maxRunes int) (kind IntentKind, word string, conf ruleConfidence, weakSignal bool) {
	for k, ws := range strongWords {
		for _, w := range ws {
			if strings.Contains(text, w) {
				return k, w, confStrong, false
			}
		}
	}
	stripped := stripPunct(text)
	weakSignal = false
	for k, ws := range weakWords {
		for _, w := range ws {
			if stripped == w {
				return k, w, confStrong, false
			}
			if len([]rune(stripped)) <= maxRunes && strings.HasPrefix(stripped, w) {
				return k, w, confWeak, false
			}
			if strings.Contains(stripped, w) {
				weakSignal = true
			}
		}
	}
	return IntentNone, "", confNone, weakSignal
}

// classifyIntent 规则表直判（L1 无 L2）：供测试与规则-only 调用点使用。
// 弱词句首命中按低置信命中处理（有仲裁器的调用点应走 EnhancePromptWithOptions）。
func classifyIntent(text string) (IntentKind, string) {
	kind, word, conf, _ := classifyByRules(text, DefaultEnhanceMaxInputRunes)
	if kind == IntentNone || conf == confNone {
		return IntentNone, ""
	}
	return kind, word
}

// arbiterDecide 调用 L2 仲裁器并归一化结果：错误（含超时/低置信）一律 IntentNone。
// 返回判定路径备注（llm_hit / llm_miss / llm_timeout / llm_error）。
func arbiterDecide(ctx context.Context, arbiter IntentArbiter, timeout time.Duration, text string) (IntentKind, string) {
	if timeout <= 0 {
		timeout = DefaultEnhanceLLMTimeout
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	kind, err := arbiter(c, text)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return IntentNone, "llm_timeout"
		}
		return IntentNone, "llm_error"
	}
	if kind == IntentNone {
		return IntentNone, "llm_miss"
	}
	return kind, "llm_hit"
}

// buildEnhancement L3 输出层：结构化模板 + 状态交叉验证 + 按置信度给引导。
// 只增不改：原文逐字保留在【用户原始指令】段；补全段全部为固定模板文本。
func buildEnhancement(original string, st EnhanceState, kind IntentKind, word string, conf ruleConfidence) string {
	// 状态交叉验证：续跑意图必须存在可绑定任务，否则视为误判直通（宁漏判不误判）。
	// 控制/诊断意图无状态也保留标签（指令本身语义明确，绑定为可选增强）。
	if kind == IntentResume && len(st.FailedTasks) == 0 && len(st.PendingTasks) == 0 {
		return original
	}

	var sb strings.Builder
	sb.WriteString("【系统补全】（自动分类推断，可能与原意不符；与【用户原始指令】语义无关时请整段忽略）\n")
	sb.WriteString("意图: ")
	sb.WriteString(kind.String())
	if word != "" {
		sb.WriteString("（命中词: ")
		sb.WriteString(word)
		sb.WriteString("）")
	}
	sb.WriteString("\n")

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
		// 强引导建议仅 L1 高置信（强词/全句弱词）给出：仲裁/弱命中的输出只给
		// 标签 + 状态绑定，不给"优先继续……"式强指令（防 LLM 被低置信建议带偏）。
		if conf == confStrong {
			sb.WriteString("建议: 优先继续最近失败/中断的任务（失败原因见上），不要重跑已完成或进行中的任务；如多个失败任务并列，先询问用户续跑哪一个。\n")
		}
	} else if len(st.PendingTasks) > 0 {
		sb.WriteString("未完成任务: ")
		var titles []string
		for _, t := range st.PendingTasks {
			titles = append(titles, fmt.Sprintf("%s[%s]", t.Title, t.Domain))
		}
		sb.WriteString(strings.Join(titles, "、"))
		sb.WriteString("\n")
		if conf == confStrong {
			sb.WriteString("建议: 任务未全部完成，继续推进未完成任务。\n")
		}
	}
	return "【用户原始指令】\n" + original + "\n\n" + strings.TrimRight(sb.String(), "\n")
}
