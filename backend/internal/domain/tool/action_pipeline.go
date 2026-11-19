package tool

// action_pipeline.go 唯一生产执行入口的事务管线（Harness 根治计划 WP2 §6.1/§6.4/§6.5）。
//
// 管线位置：Registry.Dispatch 内部——不是 toolRegistryAdapter。这样程序化调用
// （workflow 引擎、verifyloop、后台任务）与模型调用走同一条授权/审计路径（I-EXEC-1）。
//
// 阶段（append-only，落 action_events）：
//
//	ActionRequested → ResourceResolved → Authorized / ApprovalRequired / Rejected
//	→ CheckpointCreated → Executed → MutationObserved → ScopeVerified
//	→ Committed | RolledBack | ManualRecoveryRequired
//
// 模式：
//   - off     ：零行为（不建快照、不读合同、不写事件）；
//   - shadow  ：照常执行，但完整算出"如果 enforce 会拒绝/回滚什么"并记录；
//   - enforce ：真实阻断与回滚。快照失败时生产写入 fail closed（I-EXEC-3）。
//
// 边界（计划 §6.6 / §15，必须如实记录，不得宣称"所有命令均可撤销"）：
//   - 文件系统变更可回滚；网络、数据库、进程、部署等外部副作用不可回滚，
//     事件里显式标 external_side_effects_not_reverted；
//   - Shell 命令的静态分析只是提示，最终以执行前后的文件 diff 为准。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/checkpoint"
	"github.com/blockmemory/agent/backend/internal/team"
)

// ActionMode 三档模式（与 config.HarnessMode* 同值；tool 包不依赖 config）。
const (
	ActionModeOff     = "off"
	ActionModeShadow  = "shadow"
	ActionModeEnforce = "enforce"
)

// normalizeActionMode 归一化：非法值回落 off（最保守——不改变既有行为）。
func normalizeActionMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case ActionModeShadow:
		return ActionModeShadow
	case ActionModeEnforce:
		return ActionModeEnforce
	default:
		return ActionModeOff
	}
}

// ActionEventSink 落盘 Action 阶段事件（bootstrap 注入 store.PGActionEventStore；
// 测试注入内存实现；nil 时只在日志里留一行）。
type ActionEventSink interface {
	Append(ctx context.Context, ev ActionEvent) error
}

// ActionEvent 一条阶段事件（与 store.ActionEvent 同形，避免 tool 依赖 store）。
type ActionEvent struct {
	ActionID         string    `json:"action_id"`
	SessionID        string    `json:"session_id"`
	LogicalTaskID    string    `json:"logical_task_id,omitempty"`
	ContractRevision int64     `json:"contract_revision,omitempty"`
	Seq              int       `json:"seq"`
	Phase            string    `json:"phase"`
	Tool             string    `json:"tool"`
	RequestRef       string    `json:"request_ref,omitempty"`
	BeforeCheckpoint string    `json:"before_checkpoint,omitempty"`
	AfterCheckpoint  string    `json:"after_checkpoint,omitempty"`
	MutationRef      string    `json:"mutation_ref,omitempty"`
	ErrorCategory    string    `json:"error_category,omitempty"`
	Payload          string    `json:"payload,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ActionPipeline 统一 Action 事务管线。
type ActionPipeline struct {
	mode       string
	store      *checkpoint.Store
	events     ActionEventSink
	protected  []string
	authorizer Authorizer

	mu  sync.Mutex
	seq map[string]int // actionID -> 已分配的最大 seq
}

// ActionPipelineConfig 构造参数。
type ActionPipelineConfig struct {
	Mode      string
	Store     *checkpoint.Store
	Events    ActionEventSink
	Protected []string
}

// NewActionPipeline 构造管线；mode=off 或 store 为空时返回一个"只记录事件"的轻量管线。
func NewActionPipeline(cfg ActionPipelineConfig) *ActionPipeline {
	p := &ActionPipeline{
		mode:      normalizeActionMode(cfg.Mode),
		store:     cfg.Store,
		events:    cfg.Events,
		protected: append([]string{}, cfg.Protected...),
		seq:       map[string]int{},
	}
	return p
}

// Mode 返回当前模式。
func (p *ActionPipeline) Mode() string {
	if p == nil {
		return ActionModeOff
	}
	return p.mode
}

// SetMode 运行中切换模式（灰度）。
func (p *ActionPipeline) SetMode(mode string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.mode = normalizeActionMode(mode)
	p.mu.Unlock()
}

// SetEventSink 注入事件落点（bootstrap 延迟装配）。
func (p *ActionPipeline) SetEventSink(s ActionEventSink) {
	if p == nil {
		return
	}
	p.events = s
}

// Enabled 判定是否参与执行（off = 完全不介入）。
func (p *ActionPipeline) Enabled() bool { return p != nil && p.mode != ActionModeOff }

// nextSeq 为单个 Action 分配递增事件序号。
func (p *ActionPipeline) nextSeq(actionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq[actionID]++
	return p.seq[actionID]
}

// emit 追加一条阶段事件（best-effort：审计失败不影响执行，但会打日志）。
func (p *ActionPipeline) emit(ctx context.Context, ev ActionEvent) {
	if p == nil {
		return
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	if p.events != nil {
		if err := p.events.Append(ctx, ev); err != nil {
			log.Printf("[action] append event failed: action=%s phase=%s err=%v", ev.ActionID, ev.Phase, err)
		}
	}
	if ev.Phase == ActionPhaseRejected || ev.Phase == ActionPhaseScopeViolate ||
		ev.Phase == ActionPhaseRolledBack || ev.Phase == ActionPhaseManualRecovery ||
		ev.Phase == ActionPhaseApprovalReq {
		log.Printf("[action] %s action=%s tool=%s task=%s rev=%d detail=%s",
			ev.Phase, ev.ActionID, ev.Tool, ev.LogicalTaskID, ev.ContractRevision, ev.Payload)
	}
}

// ActionOutcome 一次 Action 的最终结论（供调用方/测试断言）。
type ActionOutcome struct {
	ActionID   string
	Decision   string // allow | ask | deny
	Executed   bool
	Mutated    bool
	RolledBack bool
	Manifest   *checkpoint.MutationManifest
	Reason     string
	// ShadowWould 描述 shadow 模式下"enforce 会做什么"（用于对拍）。
	ShadowWould string
}

// Run 执行一次动作：
//  1. 解析资源 → 2. 授权 → 3. before 快照 → 4. 执行 → 5. after 快照 + diff
//     → 6. 范围校验 → 7. commit / rollback。
//
// exec 为真正的工具执行（由 Registry 传入）。返回 (结果, 结论)。
//   - off 模式直接执行，零开销；
//   - deny 且 enforce → 不执行，返回工具级错误（模型可见，循环不中断）；
//   - 越界且 enforce → 回滚本 Action 的文件变更，返回工具级错误。
func (p *ActionPipeline) Run(ctx context.Context, name string, t Tool, args map[string]any,
	workDir string, exec func() *Result) (*Result, ActionOutcome) {
	out := ActionOutcome{ActionID: newActionID(), Decision: "allow"}
	if !p.Enabled() {
		return exec(), out
	}
	mode := p.Mode()
	enforce := mode == ActionModeEnforce
	caps := capabilitiesOf(t, name)
	// 只读工具不建快照（性能纪律 §6.4）。
	mutates := CapabilitiesMutateFiles(caps)
	sessionID := SessionIDFromContext(ctx)
	task := LogicalTaskFromContext(ctx)
	policy := ActionPolicy{
		Mode:          mode,
		Contract:      task,
		ProtectedDirs: p.protected,
		TrustMode:     TrustModeOf(ctx),
	}
	if task != nil {
		policy.TaskID = task.LogicalTaskID
		policy.Revision = task.Revision
		policy.Hash = task.ContentHash
	}
	reqs := resolveRequestedResources(name, args, workDir)
	base := ActionEvent{
		ActionID:         out.ActionID,
		SessionID:        sessionID,
		LogicalTaskID:    policy.TaskID,
		ContractRevision: policy.Revision,
		Tool:             name,
		RequestRef:       truncateForEvent(argsJSON(args), 480),
	}
	p.emit(ctx, withSeq(p, base, ActionPhaseRequested, map[string]any{
		"capabilities": caps, "mode": mode, "mutates_files": mutates,
	}))
	// Shell/写入类工具即使参数不可解析也要观察 diff（不得当只读）。
	if len(reqs) == 0 && mutates {
		reqs = []ResourceRequest{{Kind: team.RuleKindPath, Value: "", Write: true}}
	}
	p.emit(ctx, withSeq(p, base, ActionPhaseResolved, map[string]any{
		"requested":    requestSummary(reqs),
		"write_set":    ruleSummary(policy.Contract),
		"excluded":     excludedSummary(policy.Contract),
		"has_contract": policy.Contract != nil,
	}))

	auth := p.authorizer.Authorize(reqs, policy)
	out.Decision = auth.Decision
	out.Reason = auth.Reason
	authPhase := ActionPhaseAuthorized
	switch auth.Decision {
	case "deny":
		authPhase = ActionPhaseRejected
	case "ask":
		authPhase = ActionPhaseApprovalReq
	}
	p.emit(ctx, withSeq(p, base, authPhase, map[string]any{
		"decision": auth.Decision, "reason": auth.Reason,
		"violations": auth.Violations, "escalation_needed": auth.EscalationNeeded,
	}))
	// shadow：不阻断，但记录 enforce 会做什么。
	if !enforce {
		if auth.Decision != "allow" {
			out.ShadowWould = fmt.Sprintf("enforce 会拒绝/要求扩展合同：%s", auth.Reason)
			log.Printf("[action][shadow] action=%s tool=%s decision=%s task=%s rev=%d reason=%s",
				out.ActionID, name, auth.Decision, policy.TaskID, policy.Revision, auth.Reason)
		}
	} else if auth.Decision == "deny" {
		res := &Result{Tool: name, Error: "Action 被合同拒绝，未执行：" + auth.Reason +
			"（如需扩大范围请先产生新合同版本；不得通过 RunCommand 绕过）"}
		out.Executed = false
		return res, out
	} else if auth.Decision == "ask" && !p.approvalSatisfied(policy) {
		res := &Result{Tool: name, Error: "Action 需要明确授权，未执行：" + auth.Reason +
			"（当前会话信任模式不允许自动扩权）"}
		out.Executed = false
		return res, out
	} else if auth.Decision == "ask" {
		// full-auto：用户已授权自治，ask 视为已批准（等价旧行为）；审计仍保留原始 ask 事件。
		out.Decision = "allow"
		p.emit(ctx, withSeq(p, base, ActionPhaseAuthorized, map[string]any{
			"decision": "allow", "approved_by": "trust_mode", "trust_mode": policy.TrustMode,
			"original_decision": "ask", "reason": auth.Reason,
		}))
	}

	if !mutates || p.store == nil {
		// 只读/无检查点存储：直接执行 + 记录跳过原因（不静默声称已快照）。
		reason := "read-only tool"
		if p.store == nil {
			reason = "checkpoint store not configured"
		}
		p.emit(ctx, withSeq(p, base, ActionPhaseCheckpoint, map[string]any{"skipped": true, "reason": reason}))
		if mutates && enforce && p.store == nil {
			res := &Result{Tool: name, Error: "写入被拒绝：检查点未配置（enforce 模式下生产写入必须可回滚）"}
			p.emit(ctx, withSeq(p, base, ActionPhaseRejected, map[string]any{"reason": "checkpoint unavailable"}))
			return res, out
		}
		res := exec()
		out.Executed = true
		p.emit(ctx, withSeq(p, base, ActionPhaseExecuted, map[string]any{"success": res != nil && res.Success}))
		return res, out
	}

	before, err := p.store.Capture(workDir)
	if err != nil {
		// I-EXEC-3：检查点失败时 enforce 必须 fail closed。
		p.emit(ctx, withSeq(p, base, ActionPhaseCheckpoint, map[string]any{"error": err.Error()}))
		if enforce {
			return &Result{Tool: name, Error: "写入被拒绝：无法创建检查点（" + err.Error() + "）"}, out
		}
		log.Printf("[action][shadow] checkpoint failed: action=%s tool=%s err=%v", out.ActionID, name, err)
		res := exec()
		out.Executed = true
		return res, out
	}
	base.BeforeCheckpoint = before.ID
	p.emit(ctx, withSeq(p, base, ActionPhaseCheckpoint, map[string]any{
		"before": before.ID, "files": len(before.Files), "degraded": before.Degraded, "reason": before.Reason,
	}))
	if before.Degraded && enforce {
		// 降级快照（超上限/不可读）不静默放行：enforce 下拒绝并说明原因。
		p.emit(ctx, withSeq(p, base, ActionPhaseRejected, map[string]any{"reason": "degraded checkpoint: " + before.Reason}))
		return &Result{Tool: name, Error: "写入被拒绝：检查点不完整（" + before.Reason + "）"}, out
	}

	res := exec()
	out.Executed = true
	p.emit(ctx, withSeq(p, base, ActionPhaseExecuted, map[string]any{
		"success": res != nil && res.Success, "error": resultError(res),
	}))

	after, capErr := p.store.Capture(workDir)
	if capErr != nil {
		p.emit(ctx, withSeq(p, base, ActionPhaseMutated, map[string]any{"error": capErr.Error()}))
		out.Manifest = &checkpoint.MutationManifest{BeforeID: before.ID}
		return p.finishWithoutDiff(ctx, &out, base, name, res, enforce)
	}
	base.AfterCheckpoint = after.ID
	manifest := p.store.Diff(before, after)
	out.Manifest = manifest
	out.Mutated = !manifest.Empty()
	mutPayload := mutationSummary(manifest)
	p.emit(ctx, withSeq(p, base, ActionPhaseMutated, mutPayload))

	violations := p.scopeViolations(manifest, reqs, policy)
	if len(violations) == 0 {
		p.emit(ctx, withSeq(p, base, ActionPhaseScopeOK, map[string]any{"changed": len(manifest.AllPaths())}))
		p.emit(ctx, withSeq(p, base, ActionPhaseCommitted, map[string]any{"changed": len(manifest.AllPaths())}))
		return res, out
	}
	out.Reason = strings.Join(violations, "; ")
	p.emit(ctx, withSeq(p, base, ActionPhaseScopeViolate, map[string]any{"violations": violations}))
	if !enforce {
		out.ShadowWould = "enforce 会回滚：" + out.Reason
		log.Printf("[action][shadow] action=%s tool=%s violation=%s", out.ActionID, name, out.Reason)
		return res, out
	}
	rep, restoreErr := p.store.Restore(workDir, before, after, checkpoint.RestoreOptions{
		Only:        manifest.AllPaths(),
		DetectDrift: true,
	})
	payload := map[string]any{"violations": violations, "restored": rep.Restored,
		"drifted": rep.Drifted, "failed": rep.Failed, "unavailable": rep.Unavailable}
	if restoreErr != nil {
		payload["restore_error"] = restoreErr.Error()
		p.emit(ctx, withSeq(p, base, ActionPhaseManualRecovery, payload))
		out.RolledBack = false
		return &Result{Tool: name, Success: false, Category: ResultCategoryExecutionFailed,
			Error: "越界写入已发生且回滚失败（需人工恢复）：" + out.Reason}, out
	}
	p.emit(ctx, withSeq(p, base, ActionPhaseRolledBack, payload))
	out.RolledBack = true
	msg := "越界写入已回滚：" + out.Reason
	if len(rep.Drifted) > 0 {
		msg += "（注意：以下文件在 Action 期间被外部修改，未回滚：" + strings.Join(rep.Drifted, ", ") + "）"
	}
	if len(rep.Failed) > 0 {
		msg += "（回滚失败需人工处理：" + strings.Join(rep.Failed, ", ") + "）"
	}
	return &Result{Tool: name, Success: false, Category: ResultCategoryExecutionFailed, Error: msg}, out
}

// finishWithoutDiff 无法生成 after 快照时保守处理：enforce 下要求人工确认恢复基线。
func (p *ActionPipeline) finishWithoutDiff(ctx context.Context, out *ActionOutcome, base ActionEvent,
	name string, res *Result, enforce bool) (*Result, ActionOutcome) {
	p.emit(ctx, withSeq(p, base, ActionPhaseManualRecovery, map[string]any{
		"reason":                             "after checkpoint failed; mutation scope unknown",
		"external_side_effects_not_reverted": true,
	}))
	if !res.Success {
		return res, *out
	}
	if enforce {
		return &Result{Tool: name, Success: false, Category: ResultCategoryExecutionFailed,
			Error: "执行已完成但无法生成变更清单（after 快照失败），已标记为需人工恢复确认"}, *out
	}
	return res, *out
}

// approvalSatisfied 判断当前信任模式是否已覆盖 ask 决策：
// full-auto 视为用户已授权自治（等价旧行为），suggest/auto-edit 下 ask 必须走审批。
func (p *ActionPipeline) approvalSatisfied(policy ActionPolicy) bool {
	return policy.TrustMode == TrustModeFullAuto
}

// scopeViolations 计算越界变更：manifest 中不在允许写入范围的文件。
// 注意：只对"写文件/Shell"类动作生效；.bma 内部产物（工具输出、临时脚本）默认放行，
// 但受保护目录例外（验收规则/检查点）。
func (p *ActionPipeline) scopeViolations(m *checkpoint.MutationManifest, reqs []ResourceRequest, policy ActionPolicy) []string {
	if m == nil || m.Empty() {
		return nil
	}
	// 无合同（legacy 会话）：只做受保护目录校验，不做写范围判定（兼容旧会话）。
	allowAllFiles := policy.Contract == nil
	var out []string
	for _, rel := range m.AllPaths() {
		if inProtectedDir(rel, policy.ProtectedDirs) {
			out = append(out, rel+": 受保护目录禁止写入")
			continue
		}
		if allowAllFiles {
			continue
		}
		if strings.HasPrefix(rel, ".bma/") {
			continue // BMA 自身产物（tool_outputs/artifacts/snapshots 等）
		}
		req := ResourceRequest{Kind: team.RuleKindPath, Value: rel, Write: true}
		if hitsExcluded(rel, req, policy) {
			out = append(out, rel+": 命中合同禁止集合")
			continue
		}
		effect, matched := matchRules(rel, req, policy)
		if !matched || effect == team.RuleEffectDeny {
			if !matched && len(policy.Contract.WriteSet) == 0 {
				// 空 WriteSet = 尚未解析：视为全部越界（enforce 下会回滚）。
				out = append(out, rel+": 合同未解析写入范围")
				continue
			}
			if !matched {
				out = append(out, rel+": 不在合同写入范围内")
				continue
			}
			out = append(out, rel+": 命中 deny 规则")
		}
	}
	sort.Strings(out)
	// shell 请求（Value 为空）不额外放宽：diff 已经覆盖真实写入。
	_ = reqs
	return out
}

// --- 上下文注入 --------------------------------------------------------------

type logicalTaskKey struct{}

// WithLogicalTask 把当前逻辑任务合同注入 ctx（agent 每轮注入；enforce 下据此授权）。
// nil 表示 legacy 会话（无合同）：只做受保护目录校验，不按合同授权。
func WithLogicalTask(ctx context.Context, c *team.LogicalTaskContract) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, logicalTaskKey{}, c)
}

// LogicalTaskFromContext 取出当前合同；未注入返回 nil。
func LogicalTaskFromContext(ctx context.Context) *team.LogicalTaskContract {
	if v, ok := ctx.Value(logicalTaskKey{}).(*team.LogicalTaskContract); ok {
		return v
	}
	return nil
}

// --- 小工具 ------------------------------------------------------------------

func capabilitiesOf(t Tool, name string) []string {
	if t != nil {
		if d, ok := t.(CapabilityDeclarer); ok {
			if caps := d.Capabilities(); len(caps) > 0 {
				return caps
			}
		}
	}
	if caps := BuiltinCapabilities(name); len(caps) > 0 {
		return caps
	}
	if strings.HasPrefix(name, "forged__") {
		return []string{CapShell}
	}
	if strings.Contains(name, "__") {
		// 插件 MCP 工具：未声明能力按 unknown 处理（不当只读）。
		return []string{CapUnknown}
	}
	return []string{CapReadOnly}
}

func newActionID() string {
	return fmt.Sprintf("act-%d-%d", time.Now().UnixNano(), os.Getpid())
}

func withSeq(p *ActionPipeline, base ActionEvent, phase string, payload map[string]any) ActionEvent {
	ev := base
	ev.Phase = phase
	ev.Seq = p.nextSeq(base.ActionID)
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			ev.Payload = string(data)
		}
	}
	return ev
}

func argsJSON(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	if data, err := marshalNoHTMLEscape(args); err == nil {
		return string(data)
	}
	return ""
}

func truncateForEvent(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func requestSummary(reqs []ResourceRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		kind := r.Kind
		val := r.Value
		if val == "" && kind == team.RuleKindPath {
			val = "<unparsed shell write>"
		}
		if r.Semantic != nil {
			val = r.Semantic.ID
		}
		out = append(out, kind+":"+val)
	}
	return out
}

func ruleSummary(c *team.LogicalTaskContract) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.WriteSet))
	for _, r := range c.WriteSet {
		out = append(out, r.Kind+":"+r.Value)
	}
	return out
}

func excludedSummary(c *team.LogicalTaskContract) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.ExcludedSet))
	for _, r := range c.ExcludedSet {
		out = append(out, r.Kind+":"+r.Value)
	}
	return out
}

func mutationSummary(m *checkpoint.MutationManifest) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return map[string]any{
		"added": m.Added, "modified": m.Modified, "deleted": m.Deleted,
		"mode_changed": m.ModeChanged, "before": m.BeforeID, "after": m.AfterID,
		"excluded": m.Excluded, "degraded": m.Degraded,
	}
}

func resultError(res *Result) string {
	if res == nil {
		return ""
	}
	if res.Error != "" {
		return res.Error
	}
	return ""
}

// 编译期断言：checkpoint 的 manifest 类型直接复用（避免两套形状）。
var _ = filepath.ToSlash
