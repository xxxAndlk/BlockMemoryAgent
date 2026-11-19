package tool

// action.go 统一 Action 授权模型（Harness 根治计划 WP2 §6.2/§6.3）。
//
// 问题：改动可以通过 WriteFile / EditFile / RunCommand(Node/Python 脚本) / 插件 MCP 工具
// 四条不同入口落地，各自只有零散的破坏性审批，没有共同的"合同允许我改哪些资源"事实。
// 事故会话第 15 轮就是用 RunCommand → node fs.writeFileSync 把两个页面的共享列宽一起
// 改掉，路径检查、审批、事后审计全部绕过。
//
// 本文件定义与执行器无关的中立类型：
//   - ResourceRequest  一次 Action 请求的资源（读/写/删除 + 语义目标）
//   - ActionPolicy     授权决策所需的输入（合同规则 + 模式 + 验收保护目录）
//   - Authorizer       纯函数式判定：deny 优先、ExcludedSet 恒 deny、空 WriteSet 不等于全允许
//   - Capability      工具声明的能力（只读/文件写/网络/数据库/外部副作用）
//
// 管线（checkpoint + 事后 diff + 回滚）在 action_pipeline.go。

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/internal/team"
)

// Action 阶段（append-only 事件序列，与计划 §6.1 一一对应）。
const (
	ActionPhaseRequested      = "requested"
	ActionPhaseResolved       = "resolved"
	ActionPhaseAuthorized     = "authorized"
	ActionPhaseApprovalReq    = "approval_required"
	ActionPhaseRejected       = "rejected"
	ActionPhaseCheckpoint     = "checkpoint_created"
	ActionPhaseExecuted       = "executed"
	ActionPhaseMutated        = "mutation_observed"
	ActionPhaseScopeOK        = "scope_verified"
	ActionPhaseScopeViolate   = "scope_violated"
	ActionPhaseCommitted      = "committed"
	ActionPhaseRolledBack     = "rolled_back"
	ActionPhaseManualRecovery = "manual_recovery_required"
)

// 工具能力声明（插件/内建工具都能实现，未声明按 unknown 处理）。
const (
	CapReadOnly       = "read_only"
	CapFileWrite      = "file_write"
	CapShell          = "shell"
	CapNetwork        = "network"
	CapDatabase       = "database"
	CapExternalEffect = "external_side_effect"
	CapUnknown        = "unknown"
)

// ResourceRequest 一次 Action 请求的资源。
type ResourceRequest struct {
	// Kind team.RuleKindPath / RuleKindSemanticTarget / RuleKindTool / RuleKindNetwork / RuleKindDatabase
	Kind string
	// Value 路径（相对工作区、正斜杠）或语义目标 ID。
	Value string
	// Write true=写/删；false=只读。
	Write bool
	// Semantic 语义目标（Kind=semantic_target 时填）。
	Semantic *team.SemanticTarget
}

// ActionPolicy 授权输入。
type ActionPolicy struct {
	// Mode off|shadow|enforce（agent.action_transaction_mode）。
	Mode string
	// Contract 当前逻辑任务合同；nil = legacy 会话无合同。
	Contract *team.LogicalTaskContract
	// TaskID/Revision/Hash 冗余记录（合同可为 nil，但审计需要知道实际用的是哪版）。
	TaskID   string
	Revision int64
	Hash     string
	// ProtectedDirs 任何角色都不得写入的目录（工作区相对路径），例如
	// .bma/verification（验收规则）与 .bma/checkpoints（检查点内部文件）。
	ProtectedDirs []string
	// TrustMode 会话信任模式（suggest/auto-edit/full-auto）。
	TrustMode string
}

// Authorization 授权结果。
type Authorization struct {
	Decision string // allow | ask | deny
	// Reason 人可读原因（写进工具错误/事件）。
	Reason string
	// Violations 具体越界项（deny/ask 时非空）。
	Violations []string
	// EscalationNeeded 需要新合同版本才能放行（模型不能自行扩权）。
	EscalationNeeded bool
}

// Authorizer 纯函数授权器（无 IO，便于单测与 shadow 对拍）。
type Authorizer struct{}

// Authorize 判定一次 Action 请求。
//
// 规则（计划 §6.3）：
//  1. ProtectedDirs 内任何写入 deny（验收规则/守护测试不在执行角色可编辑范围，I-VERIFY-2）；
//  2. ExcludedSet 恒 deny（且优先于任何 allow）；
//  3. 空 WriteSet 不等于"全部允许"：写请求一律 ask（enforce 下视为 deny，
//     因为"ask"在无人值守会话里等价拒绝；由管线按模式决定是拒绝还是仅记录）；
//  4. WriteSet 内 allow → allow；命中 ask 规则 → ask；
//  5. 不在任何规则中 → ask（未知资源默认不放行）。
func (Authorizer) Authorize(reqs []ResourceRequest, policy ActionPolicy) Authorization {
	res := Authorization{Decision: "allow"}
	var violations []string
	decide := func(d, reason, detail string) {
		// deny > ask > allow
		if d == "deny" || (d == "ask" && res.Decision == "allow") {
			res.Decision = d
			if res.Reason == "" {
				res.Reason = reason
			}
		}
		if detail != "" {
			violations = append(violations, detail)
		}
	}
	for _, req := range reqs {
		if !req.Write {
			continue // 读请求不设硬门（读取范围由角色白名单/路径沙箱负责）
		}
		rel := normalizeActionPath(req.Value)
		if inProtectedDir(rel, policy.ProtectedDirs) {
			decide("deny", "验收规则/检查点内部文件禁止写入",
				fmt.Sprintf("%s: 位于受保护目录（验收规则与检查点不可由执行角色修改）", rel))
			continue
		}
		if hitsExcluded(rel, req, policy) {
			decide("deny", "该资源在合同禁止触碰集合内",
				fmt.Sprintf("%s: 命中 ExcludedSet（用户明确要求不要动）", rel))
			continue
		}
		effect, matched := matchRules(rel, req, policy)
		switch {
		case !matched:
			if policy.Contract == nil || len(policy.Contract.WriteSet) == 0 {
				res.EscalationNeeded = true
				decide("ask", "合同未解析写入范围，生产写入需要明确授权",
					fmt.Sprintf("%s: 无写入授权（WriteSet 为空 = 尚未解析，不等于全允许）", rel))
			} else {
				res.EscalationNeeded = true
				decide("ask", "写入范围不含该资源，需先扩展合同（产生新合同版本）",
					fmt.Sprintf("%s: 不在 WriteSet 内", rel))
			}
		case effect == team.RuleEffectDeny:
			decide("deny", "该资源规则为 deny",
				fmt.Sprintf("%s: 命中 deny 规则", rel))
		case effect == team.RuleEffectAsk:
			res.EscalationNeeded = true
			decide("ask", "该资源规则要求确认", fmt.Sprintf("%s: 命中 ask 规则", rel))
		default:
			// allow
		}
	}
	sort.Strings(violations)
	res.Violations = violations
	if res.Decision == "allow" {
		res.Reason = ""
	}
	return res
}

// normalizeActionPath 统一相对路径（正斜杠、去 ./ 前缀）。
func normalizeActionPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// inProtectedDir 判定路径是否落在受保护目录内。
func inProtectedDir(rel string, dirs []string) bool {
	rel = normalizeActionPath(rel)
	for _, d := range dirs {
		d = normalizeActionPath(d)
		if d == "" {
			continue
		}
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// hitsExcluded 判定是否命中合同禁止集合（路径/glob/语义目标）。
func hitsExcluded(rel string, req ResourceRequest, policy ActionPolicy) bool {
	if policy.Contract == nil {
		return false
	}
	for _, rule := range policy.Contract.ExcludedSet {
		if ruleMatches(rule.Kind, rule.Value, rel, req) {
			return true
		}
	}
	return false
}

// matchRules 返回命中的最强效果（deny > ask > allow）与是否命中。
func matchRules(rel string, req ResourceRequest, policy ActionPolicy) (string, bool) {
	if policy.Contract == nil {
		return "", false
	}
	rank := func(e string) int {
		switch e {
		case team.RuleEffectDeny:
			return 3
		case team.RuleEffectAsk:
			return 2
		default:
			return 1
		}
	}
	best, matched := "", false
	consider := func(effect string) {
		if !matched || rank(effect) > rank(best) {
			best = effect
			matched = true
		}
	}
	for _, rule := range policy.Contract.WriteSet {
		if ruleMatches(rule.Kind, rule.Value, rel, req) {
			consider(rule.Effect)
		}
	}
	// 语义目标规则：WriteSet 里声明 semantic_target 时，只有语义目标命中才算允许。
	if req.Kind == team.RuleKindSemanticTarget && req.Semantic != nil {
		for _, t := range policy.Contract.Targets {
			if t.ID != req.Semantic.ID {
				continue
			}
			if req.Semantic.File != "" && normalizeActionPath(t.File) != normalizeActionPath(req.Semantic.File) {
				// 同一语义 ID 被指到别的文件（跨页共享节点）→ 视为不匹配。
				continue
			}
			consider(team.RuleEffectAllow)
		}
	}
	return best, matched
}

// ruleMatches 单条规则匹配。
func ruleMatches(kind, value, rel string, req ResourceRequest) bool {
	switch kind {
	case team.RuleKindPath:
		return normalizeActionPath(value) == rel
	case team.RuleKindGlob:
		return globMatch(normalizeActionPath(value), rel)
	case team.RuleKindSemanticTarget:
		if req.Kind != team.RuleKindSemanticTarget || req.Semantic == nil {
			return false
		}
		return value == req.Semantic.ID
	default:
		return false
	}
}

// globMatch 简单 glob 匹配：** 匹配跨目录，* 不跨目录。
func globMatch(pattern, target string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == target
	}
	// 逐段匹配，** 表示任意层。
	ps := strings.Split(pattern, "/")
	ts := strings.Split(target, "/")
	var match func(i, j int) bool
	match = func(i, j int) bool {
		if i == len(ps) {
			return j == len(ts)
		}
		if ps[i] == "**" {
			for k := j; k <= len(ts); k++ {
				if match(i+1, k) {
					return true
				}
			}
			return false
		}
		if j >= len(ts) {
			return false
		}
		if ok, err := path.Match(ps[i], ts[j]); err != nil || !ok {
			return false
		}
		return match(i+1, j+1)
	}
	return match(0, 0)
}

// CapabilityDeclarer 工具声明自身能力（未实现的工具按 unknown 处理，不当只读）。
type CapabilityDeclarer interface {
	Capabilities() []string
}

// ToolCapabilities 返回工具声明的能力；未声明返回 unknown。
func ToolCapabilities(t Tool) []string {
	if t == nil {
		return []string{CapUnknown}
	}
	if d, ok := t.(CapabilityDeclarer); ok {
		caps := d.Capabilities()
		if len(caps) > 0 {
			return caps
		}
	}
	return []string{CapUnknown}
}

// CapabilitiesMutateFiles 判定能力集是否示意"可能写文件"（unknown 按可能写处理）。
func CapabilitiesMutateFiles(caps []string) bool {
	for _, c := range caps {
		switch c {
		case CapFileWrite, CapShell, CapExternalEffect, CapUnknown:
			return true
		}
	}
	return false
}

// builtinCapabilities 内置工具的能力声明表（未列出的内置读类工具按只读处理）。
var builtinCapabilities = map[string][]string{
	"WriteFile":               {CapFileWrite},
	"EditFile":                {CapFileWrite},
	"RestoreFile":             {CapFileWrite},
	"RunCommand":              {CapShell},
	"HTTPPost":                {CapNetwork},
	"HTTPGet":                 {CapReadOnly, CapNetwork},
	"GitDiff":                 {CapReadOnly},
	"GitStatus":               {CapReadOnly},
	"GitLog":                  {CapReadOnly},
	"GitBlame":                {CapReadOnly},
	"ReadFile":                {CapReadOnly},
	"ListDir":                 {CapReadOnly},
	"SearchInFiles":           {CapReadOnly},
	"ReadMedia":               {CapReadOnly},
	"ShowArtifact":            {CapReadOnly},
	"RefreshProjectDoc":       {CapFileWrite},
	"widget_save":             {CapFileWrite},
	"widget_delete":           {CapFileWrite},
	"widget_restore":          {CapFileWrite},
	"tool_forge":              {CapFileWrite},
	"merge_worktree":          {CapFileWrite},
	"WriteSharedMemory":       {CapFileWrite},
	"WriteSpec":               {CapFileWrite},
	"remember_preference":     {CapFileWrite},
	"update_evolution_prompt": {CapFileWrite},
	"schedule_task":           {CapDatabase},
	"schedule_followup":       {CapDatabase},
	"CallExternalAgent":       {CapExternalEffect},
}

// BuiltinCapabilities 查表（未知工具返回 nil，由 ToolCapabilities 决定 unknown）。
func BuiltinCapabilities(name string) []string {
	if caps, ok := builtinCapabilities[name]; ok {
		return append([]string{}, caps...)
	}
	// ReadSharedMemory/ask_user/tool_catalog 等默认只读不写文件。
	return nil
}

// resolveRequestedResources 从工具名与参数解析本次 Action 请求的资源。
// 解析不了（RunCommand 等）返回 CapShell 级别的"未知写入范围"标记，由管线按
// "先快照后 diff"兜底——绝不因为参数不可解析就当作只读。
func resolveRequestedResources(name string, args map[string]any, workDir string) []ResourceRequest {
	pathArg, _ := args["path"].(string)
	switch name {
	case "WriteFile", "EditFile", "RestoreFile":
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: relativeToWorkDir(pathArg, workDir), Write: true}}
	case "widget_delete", "widget_restore":
		if n, _ := args["name"].(string); n != "" {
			return []ResourceRequest{{Kind: team.RuleKindPath, Value: ".bma/widgets/" + n, Write: true}}
		}
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: ".bma/widgets", Write: true}}
	case "RefreshProjectDoc":
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: ".bma/PROJECT.md", Write: true}}
	case "WriteSharedMemory", "WriteSpec", "remember_preference", "update_evolution_prompt":
		// 记忆/规格类写入落 .bma，不进源码写范围（但受受保护目录约束）。
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: ".bma", Write: true}}
	case "RunCommand":
		// Shell 无法安全解析时按"可能写任意源码"处理：不生成伪精确路径，
		// 只标记为 shell 写入请求，由管线用快照 diff 判定实际越界。
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: "", Write: true, Semantic: nil}}
	}
	if pathArg != "" {
		return []ResourceRequest{{Kind: team.RuleKindPath, Value: relativeToWorkDir(pathArg, workDir), Write: true}}
	}
	return nil
}

// relativeToWorkDir 把绝对路径转成工作区相对路径（正斜杠）；不在工作区内则返回绝对路径。
// 传入已经是相对路径时原样规范化（工具参数本来就能写相对路径）。
func relativeToWorkDir(p, workDir string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return normalizeActionPath(p)
	}
	if workDir == "" {
		return normalizeActionPath(p)
	}
	absWork, err2 := filepath.Abs(workDir)
	if err2 != nil {
		return normalizeActionPath(p)
	}
	rel, err := filepath.Rel(absWork, filepath.Clean(p))
	if err != nil {
		return normalizeActionPath(p)
	}
	return normalizeActionPath(rel)
}
