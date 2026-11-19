package team

// acceptance_eval.go 合同验收的确定性判定（Harness 根治计划 WP3 §7.2 的 L0 层）。
//
// 为什么先做 L0：事故里"构建通过"被当成验收通过——第 21 轮改了 flex/min-width，
// npm run build 退出码 0，于是模型与测试助手都判 PASS，但用户第 22 轮立刻发现
// 下单时间位置被破坏。构建通过只证明能编译，不证明"目标完成且非目标不变"（I-VERIFY-1）。
//
// 本文件只做**纯函数、零 IO** 的判定：
//   - 输入：合同（含 active 不变量 + Acceptance 规则）+ 调用方收集的证据；
//   - 输出：verified / delivered-unverified + 具体缺口；
//   - 规则：Required 证据缺失 → 只能 unverified；不变量失败 → failed；
//     "构建成功"本身不是任何 Required 规则的证据。
//
// L1/L2/L3（真实 DOM 几何、截图、集成测试）由上层 runner 提供证据，本层不猜。

import (
	"fmt"
	"sort"
	"strings"
)

// 验收状态（与任务状态机对齐，复用 TaskStatusVerified / TaskStatusDeliveredUnverified）。
const (
	VerdictVerified   = TaskStatusVerified
	VerdictUnverified = TaskStatusDeliveredUnverified
	VerdictFailed     = TaskStatusFailed
)

// EvidenceKind 证据种类（与计划 §7.2 分层对应）。
const (
	EvidenceStatic      = "static"      // 格式化/lint/typecheck/编译（L1）
	EvidenceUnit        = "unit"        // 相关单测（L2）
	EvidenceIntegration = "integration" // 集成/API（L2）
	EvidenceDOM         = "dom"         // 真实 DOM 几何（L3）
	EvidenceVisual      = "visual"      // 截图对照（L3）
	EvidenceAPI         = "api"         // API schema
	EvidenceManual      = "manual"      // 人工判定（不计机器证据）
)

// Evidence 一条验收证据（由 runner 收集，harness 只做判定）。
type Evidence struct {
	// Kind 证据种类（上面常量）。
	Kind string
	// ID 对应 AcceptanceRule.ID 或 Invariant.ID（空=不绑定具体规则，只作补充证据）。
	ID string
	// Passed 是否通过。
	Passed bool
	// Detail 人可读说明（失败原因/命令退出码/指标差值）。
	Detail string
	// EnvironmentError 环境性失败（EBUSY/浏览器缺失/网络不可达）：
	// 与"行为回归"分开分类（计划 R-25），既不算通过也不算代码失败——记为缺口。
	EnvironmentError bool
	// TaskID/Revision/ContractHash 可选绑定：提供时必须与当前合同一致，
	// 否则视为陈旧证据（跨 revision 复用是被禁止的，见 I-VERIFY-3）。
	TaskID       string
	Revision     int64
	ContractHash string
	// Collector 采集器标识（build/dom-probe/playwright/…），审计用。
	Collector string
	// Artifact 采集产物引用（截图路径/DOM 快照/日志），审计用。
	Artifact string
}

// InvariantResult 单条不变量的校验结果。
type InvariantResult struct {
	ID string
	OK bool
	// Observed 实测值（DOM 指标差值/文件 hash 等），失败时写进缺口说明。
	Observed string
	// Skipped 无法校验（缺少基线/工具不可用）：记为缺口，不当作通过。
	Skipped bool
}

// VerificationVerdict 判定结果。
type VerificationVerdict struct {
	Status string `json:"status"`
	// Failures 明确的失败（不变量被破坏/必需证据判负）。
	Failures []string `json:"failures,omitempty"`
	// Gaps 缺失或无法判定的证据（必需证据缺失、环境错误、不变量无法校验）。
	Gaps []string `json:"gaps,omitempty"`
	// BuildOnlyWarning 仅当"只有构建证据、没有任何 Required 证据"时置位：
	// 提示调用方这是典型的"构建通过 ≠ 验收通过"场景。
	BuildOnlyWarning bool `json:"build_only_warning,omitempty"`
}

// EvaluateVerification 按合同判定验收结论（确定性、保守）。
//
// 规则：
//  1. 任一 active 不变量校验失败 → failed（即使构建通过）；
//  2. 任一 active 不变量无法校验（Skipped）→ 至少 unverified（不得判 verified）；
//  3. Required 的 Acceptance 规则缺少对应通过证据 → unverified，缺口逐条列出；
//  4. 证据里出现 EnvironmentError → 记为缺口（不计行为失败），整体至多 unverified；
//  5. 全部满足 → verified；
//  6. 没有任何 Required 规则且没有不变量时，只看是否有失败证据：无失败即 verified
//     （轻任务不该被迫产出证据链），但 BuildOnlyWarning 会提示"仅有构建证据"。
func EvaluateVerification(c *LogicalTaskContract, invariants []InvariantResult, evidence []Evidence) VerificationVerdict {
	v := VerificationVerdict{}
	if c == nil {
		// 无合同（legacy 会话）：保持既有行为——不阻断，但明确 unverified。
		return VerificationVerdict{Status: VerdictUnverified, Gaps: []string{"无逻辑任务合同，无法判定目标与非目标约束"}}
	}
	byID := map[string]Evidence{}
	envErr := make([]string, 0, 4)
	stale := make([]string, 0, 4)
	hasStatic := false
	for _, e := range evidence {
		if e.EnvironmentError {
			detail := e.Kind
			if e.ID != "" {
				detail += "/" + e.ID
			}
			if e.Detail != "" {
				detail += ": " + e.Detail
			}
			envErr = append(envErr, detail)
			continue
		}
		if e.Kind == EvidenceStatic {
			hasStatic = true
		}
		if e.ID == "" {
			continue
		}
		// 证据绑定校验（I-VERIFY-3）：提供方声明了 task/revision/hash 时必须与当前
		// 合同一致，否则视为陈旧证据——绝不拿旧 revision 的证据充当本次验收。
		if why := evidenceBindingMismatch(c, e); why != "" {
			stale = append(stale, e.ID+": "+why)
			continue
		}
		// 同一规则多次证据：任一失败即算失败（保守）。
		if prev, ok := byID[e.ID]; ok && !prev.Passed {
			continue
		}
		byID[e.ID] = e
	}
	// 1/2. 不变量。
	invSeen := map[string]bool{}
	for _, inv := range c.ActiveInvariants() {
		res, ok := findInvariantResult(invariants, inv.ID)
		if !ok {
			v.Gaps = append(v.Gaps, fmt.Sprintf("不变量 %s（%s）缺少校验结果", inv.ID, inv.Kind))
			invSeen[inv.ID] = true
			continue
		}
		invSeen[inv.ID] = true
		switch {
		case res.Skipped:
			v.Gaps = append(v.Gaps, fmt.Sprintf("不变量 %s 无法校验：%s", inv.ID, res.Observed))
		case !res.OK:
			msg := fmt.Sprintf("不变量被破坏 %s（%s）", inv.ID, inv.Text)
			if res.Observed != "" {
				msg += "：实测 " + res.Observed
			}
			v.Failures = append(v.Failures, msg)
		}
	}
	for _, res := range invariants {
		if !invSeen[res.ID] {
			// 提供方给了合同里没有的不变量结果：忽略但记录（防"多算通过"）。
			v.Gaps = append(v.Gaps, "不变量结果未在合同中声明，已忽略："+res.ID)
		}
	}
	// 3. Required 规则。
	requiredCount := 0
	satisfiedRequired := 0
	satisfiedStatic := 0
	for _, rule := range c.Acceptance {
		if !rule.Required {
			continue
		}
		requiredCount++
		if rule.Kind == EvidenceManual {
			// 人工判定不计机器证据：除非显式提供 manual 证据，否则记为缺口。
			if e, ok := byID[rule.ID]; ok && e.Passed && e.Kind == EvidenceManual {
				satisfiedRequired++
				continue
			}
			v.Gaps = append(v.Gaps, fmt.Sprintf("验收规则 %s（manual）缺少人工确认", rule.ID))
			continue
		}
		e, ok := byID[rule.ID]
		if !ok {
			v.Gaps = append(v.Gaps, fmt.Sprintf("必需证据缺失 %s（kind=%s）", rule.ID, rule.Kind))
			continue
		}
		// I-VERIFY-1：证据必须与规则同种类。构建通过（static）不得满足 DOM 规则——
		// 这正是 2026-11-18 事故里"EXIT=0 当验收通过"的同类错误（2026-11-19 验收 P0-3）。
		if !evidenceKindSatisfies(rule.Kind, e.Kind) {
			v.Gaps = append(v.Gaps, fmt.Sprintf("证据类型不匹配 %s：规则要求 %s，实际得到 %s（不得代替）",
				rule.ID, rule.Kind, e.Kind))
			continue
		}
		if !e.Passed {
			v.Failures = append(v.Failures, fmt.Sprintf("验收规则失败 %s：%s", rule.ID, e.Detail))
			continue
		}
		satisfiedRequired++
		if e.Kind == EvidenceStatic {
			satisfiedStatic++
		}
	}
	// 4. 环境错误与陈旧证据一律记缺口。
	v.Gaps = append(v.Gaps, envErr...)
	v.Gaps = append(v.Gaps, stale...)
	// "只有构建证据"识别（计划 §7.2 / I-VERIFY-1）：只要"已满足的必需规则全是静态类型的"，
	// 而又存在未满足的必需规则，就必须显式提示"构建通过 ≠ 验收通过"。
	// 单独一条未绑定的 static 证据也算（旧口径）——它不能证明任何目标。
	if requiredCount > 0 && hasStatic {
		if satisfiedRequired == 0 || (satisfiedRequired < requiredCount && satisfiedStatic == satisfiedRequired) {
			v.BuildOnlyWarning = true
		}
	}
	sort.Strings(v.Failures)
	sort.Strings(v.Gaps)
	switch {
	case len(v.Failures) > 0:
		v.Status = VerdictFailed
	case len(v.Gaps) > 0:
		v.Status = VerdictUnverified
	default:
		v.Status = VerdictVerified
	}
	return v
}

func findInvariantResult(results []InvariantResult, id string) (InvariantResult, bool) {
	for _, r := range results {
		if r.ID == id {
			return r, true
		}
	}
	return InvariantResult{}, false
}

// evidenceKindSatisfies 判定证据种类是否满足规则种类（I-VERIFY-1）。
//
// 判定是单向的：证据种类必须**等于**规则种类，不跨层冒充。
// 唯一的内部兼容：integration 规则接受 api 证据（API schema 校验属于集成层）。
// 反例（必须拒绝）：static（build/lint）满足 dom/visual/unit/integration。
func evidenceKindSatisfies(ruleKind, evidenceKind string) bool {
	if ruleKind == evidenceKind {
		return true
	}
	if ruleKind == EvidenceIntegration && evidenceKind == EvidenceAPI {
		// api 证据可以满足 integration 规则（同一集成层，只是更具体）。
		return true
	}
	return false
}

// evidenceBindingMismatch 校验证据绑定（I-VERIFY-3）：提供方若声明了
// task/revision/hash，就必须与当前合同一致，否则是陈旧证据。
// 未声明的字段不参与比较（兼容只提供 Kind/ID 的简单采集器）。
func evidenceBindingMismatch(c *LogicalTaskContract, e Evidence) string {
	if c == nil {
		return ""
	}
	if e.TaskID != "" && e.TaskID != c.LogicalTaskID {
		return "task_id 不匹配（证据属于 " + e.TaskID + "）"
	}
	if e.Revision > 0 && e.Revision != c.Revision {
		return fmt.Sprintf("revision 不匹配（证据属于 rev=%d，当前 rev=%d）", e.Revision, c.Revision)
	}
	if e.ContractHash != "" && c.ContentHash != "" && e.ContractHash != c.ContentHash {
		return "contract_hash 不匹配（合同已变更）"
	}
	return ""
}

func RenderVerdict(v VerificationVerdict) string {
	var b strings.Builder
	switch v.Status {
	case VerdictVerified:
		b.WriteString("验收：已通过（目标与不变量同时满足）")
	case VerdictFailed:
		b.WriteString("验收：失败")
	default:
		b.WriteString("验收：未验证（证据不完整，不等于通过）")
	}
	if len(v.Failures) > 0 {
		b.WriteString("\n失败项：")
		for _, f := range v.Failures {
			b.WriteString("\n- " + f)
		}
	}
	if len(v.Gaps) > 0 {
		b.WriteString("\n未验证项：")
		for _, g := range v.Gaps {
			b.WriteString("\n- " + g)
		}
	}
	if v.BuildOnlyWarning {
		b.WriteString("\n注意：当前只有构建/静态证据。构建通过不代表目标完成与非目标未变。")
	}
	return b.String()
}
