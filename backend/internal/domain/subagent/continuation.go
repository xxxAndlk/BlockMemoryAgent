package subagent

// continuation.go B-11 执行连续性（首版）：
//   - 系统触限（context_threshold/iteration_limit）生成检查点；
//   - 进程内同 Agent ID 自动续跑，父子等待不升级为整会话人工暂停；
//   - 任务级累计续跑次数、无进展、总墙钟、Team token 熔断；
//   - 同任务防重派守卫（可恢复节点存在时拒绝新建实例）。
//
// 明确不做：用户暂停/审批/澄清/权限/硬预算/不确定副作用不自动恢复；
// 不伪造用户“继续”；不重置 Team 预算与原始总截止时间。跨进程恢复默认关闭。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/team"
)

// 自动续跑模式。
const (
	AutoResumeOff     = "off"
	AutoResumeShadow  = "shadow"
	AutoResumeEnforce = "enforce"
)

// ErrResumableTaskExists 同任务已有可恢复实例，拒绝新建（B-11 防重派）。
var ErrResumableTaskExists = fmt.Errorf("resumable task already exists")

// WithAutoDispatchRoles 设置 C-03 自动调度允许的角色（固定叶角色，冷启动）。
func (d *Dispatcher) WithAutoDispatchRoles(roles []string) *Dispatcher {
	d.autoDispatchRoles = map[string]bool{}
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			d.autoDispatchRoles[r] = true
		}
	}
	return d
}

// DispatchBoundTask C-03 受控自动派发入口：只接受冻结任务契约 + task/attempt 绑定，
// 不走调用方临时提权；沿用 dispatchOne 的权限/预算/并发/树/台账。
func (d *Dispatcher) DispatchBoundTask(ctx context.Context, spec team.AutoTaskSpec) (string, error) {
	roleID := strings.TrimSpace(spec.RoleID)
	if roleID == "" || roleID == "meta" || roleID == "domain" {
		return "", fmt.Errorf("auto dispatch: role %q not allowed (fixed leaf roles only)", roleID)
	}
	if len(d.autoDispatchRoles) > 0 && !d.autoDispatchRoles[roleID] {
		return "", fmt.Errorf("auto dispatch: role %q not in allowed set", roleID)
	}
	binding := &team.TaskBinding{
		SessionID: spec.SessionID, PlanVersion: spec.PlanVersion,
		TaskID: spec.TaskID, AttemptID: spec.AttemptID, Risk: spec.Risk, Acceptance: spec.Acceptance,
	}
	ctx = withTaskBinding(ctx, *binding)
	id, res := d.dispatchOne(ctx, roleID, spec.Domain, spec.Task, spec.Responsibility,
		spec.Mode, spec.VerifyKind, spec.ToolsHint, spec.SkillsHint, nil, "", 0, "", "",
		&dispatchOpts{binding: binding})
	if res != nil {
		return "", fmt.Errorf("%s", res.Error)
	}
	return id, nil
}

// WithClaimFinisher 注入 C-06 自认领终态回写。
func (d *Dispatcher) WithClaimFinisher(fn func(ctx context.Context, b team.ClaimBinding, status, summary, resultID string)) *Dispatcher {
	d.claimFinish = fn
	return d
}

// DispatchClaimed C-06 受控执行入口：认领成功的冻结任务复用 dispatchOne，
// 权限/工具/预算/并发/树/台账全部沿用；claim 不绕过任何既有门。
func (d *Dispatcher) DispatchClaimed(ctx context.Context, claim team.Claim) (string, error) {
	roleID := strings.TrimSpace(claim.Task.RoleID)
	if roleID == "" || roleID == "meta" || roleID == "domain" {
		return "", fmt.Errorf("self claim: role %q not allowed (fixed leaf roles only)", roleID)
	}
	if len(d.autoDispatchRoles) > 0 && !d.autoDispatchRoles[roleID] {
		return "", fmt.Errorf("self claim: role %q not in allowed set", roleID)
	}
	binding := &team.TaskBinding{
		SessionID: claim.SessionID, PlanVersion: claim.PlanVersion,
		TaskID: claim.TaskID, AttemptID: claim.AttemptID, AttemptNo: claim.AttemptNo,
		WorkerID: claim.WorkerID, SpecVersion: claim.SpecVersion, Risk: claim.Task.Risk,
		Acceptance: claim.Task.Acceptance,
	}
	ctx = withTaskBinding(ctx, *binding)
	id, res := d.dispatchOne(ctx, roleID, claim.Task.Domain, claim.Task.Task, claim.Task.Responsibility,
		claim.Task.Mode, claim.Task.VerifyKind, claim.Task.RequiredTools, nil, nil, "", 0, "", "",
		&dispatchOpts{binding: binding})
	if res != nil {
		return "", fmt.Errorf("%s", res.Error)
	}
	return id, nil
}

// WithAttemptFinisher 注入 C-03 attempt 终态回写。
func (d *Dispatcher) WithAttemptFinisher(fn func(ctx context.Context, attemptID, status, resultID, errText string)) *Dispatcher {
	d.attemptFinish = fn
	return d
}

// finishDispatchAttemptIfNeeded 把 C-03 绑定的 attempt 写成终态（done/unverified/failed）。
// C-06 自认领由 claimFinish 统一收口，这里跳过 WorkerID 非空的任务。
func (d *Dispatcher) finishDispatchAttemptIfNeeded(ctx context.Context, subAgentID, summary string, runErr error) {
	d.finishDispatchAttempt(ctx, subAgentID, summary, runErr, false)
}

func (d *Dispatcher) finishDispatchAttemptNow(ctx context.Context, subAgentID, summary string, runErr error) {
	d.finishDispatchAttempt(ctx, subAgentID, summary, runErr, true)
}

func (d *Dispatcher) finishDispatchAttempt(ctx context.Context, subAgentID, summary string, runErr error, force bool) {
	if d.attemptFinish == nil {
		return
	}
	bnd, ok := taskBindingFromContext(ctx)
	if !ok || bnd.AttemptID == "" || bnd.WorkerID != "" {
		return
	}
	// enforce 下由 runSubAgent 在验收门禁之后统一回写，避免“先 done 后拦截”。
	if !force && d.verificationPolicyMode == "enforce" {
		return
	}
	if errors.Is(runErr, errPaused) || errors.Is(runErr, errPartialReturn) {
		return
	}
	status := "done"
	switch {
	case runErr == nil:
		status = "done"
	case errors.Is(runErr, errUnverified), errors.Is(runErr, errVerifyMissing), errors.Is(runErr, errVisualEvidenceMissing):
		status = "unverified"
	case errors.Is(runErr, context.Canceled):
		status = "failed"
	default:
		status = "failed"
	}
	resultID := fmt.Sprintf("res-%s-%08x", subAgentID, fnvHash32(summary))
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	d.attemptFinish(ctx, bnd.AttemptID, status, resultID, errText)
}

// finishClaimIfNeeded 在 runSubAgentOnce 出口把认领结果写回 ClaimService。
// 暂停/部分返回不终结 claim；真正终态才 Complete/Fail。
func (d *Dispatcher) finishClaimIfNeeded(ctx context.Context, subAgentID, summary string, runErr error) {
	d.finishClaim(ctx, subAgentID, summary, runErr, false)
}

func (d *Dispatcher) finishClaimNow(ctx context.Context, subAgentID, summary string, runErr error) {
	d.finishClaim(ctx, subAgentID, summary, runErr, true)
}

func (d *Dispatcher) finishClaim(ctx context.Context, subAgentID, summary string, runErr error, force bool) {
	if d.claimFinish == nil {
		return
	}
	bnd, ok := taskBindingFromContext(ctx)
	if !ok || bnd.WorkerID == "" {
		return
	}
	// enforce 下由 runSubAgent 在验收门禁之后统一回写。
	if !force && d.verificationPolicyMode == "enforce" {
		return
	}
	if errors.Is(runErr, errPaused) || errors.Is(runErr, errPartialReturn) {
		return
	}
	status := "done"
	switch {
	case runErr == nil:
		status = "done"
	case errors.Is(runErr, errUnverified), errors.Is(runErr, errVerifyMissing), errors.Is(runErr, errVisualEvidenceMissing):
		status = "delivered-unverified"
	case errors.Is(runErr, context.Canceled):
		status = "cancelled"
	default:
		status = "failed"
	}
	resultID := fmt.Sprintf("res-%s-%08x", subAgentID, fnvHash32(summary))
	d.claimFinish(ctx, team.ClaimBinding{
		SessionID: bnd.SessionID, TaskID: bnd.TaskID, AttemptID: bnd.AttemptID,
		WorkerID: bnd.WorkerID, AttemptNo: bnd.AttemptNo, PlanVersion: bnd.PlanVersion,
		SpecVersion: bnd.SpecVersion,
	}, status, summary, resultID)
}

// continuationState 单实例的续跑状态（进程内）。
type continuationState struct {
	mu            sync.Mutex
	cp            team.ContinuationCheckpoint
	binding       *team.TaskBinding // 完整绑定快照：熔断终态仍需 Acceptance/Worker/PlanVersion。
	scheduled     bool
	blocked       bool
	blockedReason string
	seq           int
	deadline      time.Time
	digest        string
	noProgress    int
}

// normalization 默认模式（NewDispatcher 用 shadow，bootstrap 按配置覆盖）。
func normalizeAutoResumeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AutoResumeOff:
		return AutoResumeOff
	case AutoResumeEnforce:
		return AutoResumeEnforce
	default:
		return AutoResumeShadow
	}
}

// WithAutoResume 配置自动续跑（B-11）：模式 + 任务级累计次数 + 无进展上限。
func (d *Dispatcher) WithAutoResume(mode string, maxPerTask, noProgressLimit int) *Dispatcher {
	d.autoResumeMode = normalizeAutoResumeMode(mode)
	if maxPerTask > 0 {
		d.autoResumeMaxPerTask = maxPerTask
	}
	if noProgressLimit > 0 {
		d.autoResumeNoProgressLimit = noProgressLimit
	}
	return d
}

// continuationBudget 返回会话级累计续跑计数（root/domain/leaf 共享）。
func (d *Dispatcher) continuationBudget(sessionID string) *atomicCounter {
	v, _ := d.continuationBudgetCnt.LoadOrStore(sessionID, &atomicCounter{})
	return v.(*atomicCounter)
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) Inc() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

func (c *atomicCounter) Load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TryInc 原子占用一次预算；达到上限返回 ok=false。
func (c *atomicCounter) TryInc(max int) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if max > 0 && c.n >= max {
		return c.n, false
	}
	c.n++
	return c.n, true
}

// TryConsumeAutoResume 供主 Agent 复用同一任务级续跑预算（root/domain/leaf 共享）。
func (d *Dispatcher) TryConsumeAutoResume(sessionID string) (int, bool) {
	if d == nil {
		return 0, true
	}
	return d.continuationBudget(sessionID).TryInc(d.autoResumeMaxPerTask)
}

// SetContinuationDeadline 用根任务原始截止时间预置会话 deadline（首次写入生效）。
func (d *Dispatcher) SetContinuationDeadline(sessionID string, deadline time.Time) {
	if d == nil || sessionID == "" || deadline.IsZero() {
		return
	}
	d.continuationDeadlines.LoadOrStore(sessionID, deadline)
}

// ContinuationDeadline 返回会话首次暂停时确定的原始总截止时间（所有派生节点共享）。
// 首次调用按 d.timeout 建立，后续调用不再延后。
func (d *Dispatcher) ContinuationDeadline(sessionID string) time.Time {
	if d == nil || sessionID == "" {
		return time.Time{}
	}
	budget := d.timeout
	if budget <= 0 {
		budget = 30 * time.Minute
	}
	v, _ := d.continuationDeadlines.LoadOrStore(sessionID, time.Now().Add(budget))
	return v.(time.Time)
}

// boundedResumeHistory 构造上下文续跑视图（B-11）：完整 History 仍持久保留，
// 但 context_threshold 恢复时只喂“任务前缀 + 近期尾部”，避免原样重放后立即再触限。
// iteration_limit 不压缩（只开启新迭代切片）。
func boundedResumeHistory(msgs []agent.ReactMessage, cause team.PauseCause) []agent.ReactMessage {
	const maxTokens = 100000
	const minTail = 8
	if cause != team.PauseContextThreshold || len(msgs) <= minTail {
		return msgs
	}
	prefixEnd := 0
	for prefixEnd < len(msgs) && msgs[prefixEnd].Role == "system" {
		prefixEnd++
	}
	firstUser := -1
	for i := prefixEnd; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			firstUser = i
			break
		}
	}
	if firstUser < 0 {
		return msgs
	}
	prefixEnd = firstUser + 1
	for prefixEnd < len(msgs) && msgs[prefixEnd].Role == "system" {
		prefixEnd++
	}
	tailStart := len(msgs) - minTail
	if tailStart < prefixEnd {
		return msgs
	}
	// 避免从孤立 tool 结果起刀（provider 需要 tool_call 配对）。
	for tailStart < len(msgs) && msgs[tailStart].Role == "tool" {
		tailStart++
	}
	compose := func(start int) []agent.ReactMessage {
		out := make([]agent.ReactMessage, 0, prefixEnd+1+len(msgs)-start)
		out = append(out, msgs[:prefixEnd]...)
		out = append(out, agent.ReactMessage{Role: "system", Content: "【续跑视图】为控制上下文已省略中间历史，完整历史仍持久保留；请基于当前任务与近期进度继续，不要重复已完成步骤。"})
		out = append(out, msgs[start:]...)
		return out
	}
	for tailStart > prefixEnd && agent.EstimateMessagesTokens(compose(tailStart)) > maxTokens {
		tailStart++
		for tailStart < len(msgs) && msgs[tailStart].Role == "tool" {
			tailStart++
		}
	}
	if tailStart <= prefixEnd {
		return msgs
	}
	return compose(tailStart)
}

// pauseCauseOf 把 ReAct 触限原因映射到中立暂停原因；空值按上下文阈值兼容旧行为。
func pauseCauseOf(result agent.ReactResult) team.PauseCause {
	if result.PauseCause == "iteration_limit" {
		return team.PauseIterationLimit
	}
	return team.PauseContextThreshold
}

// progressDigest 计算确定性进展摘要：成功工具结果数 + 文件修改 + 末条正文 hash。
// 重复文本/重复计划不产生新证据，digest 不变。
func progressDigest(history []agent.ReactMessage) string {
	files := agent.FilesModifiedFromHistory(history)
	// 用成功工具输出的内容 hash 集合，而不是调用次数：重复相同空输出不算新进展。
	seen := map[string]bool{}
	for _, m := range history {
		if m.Role == "tool" && toolResultSuccess(m.Content) {
			seen[team.HashContextContent(m.Content)] = true
		}
	}
	hashes := make([]string, 0, len(seen))
	for h := range seen {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	last := agent.LastAssistantText(history)
	return team.HashContextContent(strings.Join(files, "\x00") + "|" + strings.Join(hashes, "\x00") + "|" + last)
}

// recordContinuationCheckpoint 记录一次系统暂停并发出 checkpointed 事件。
// taskID/attemptID 来自 C-03 绑定；legacy 为空。
func (d *Dispatcher) recordContinuationCheckpoint(ctx context.Context, agentID, parentID, sid, ownerKind string, cause team.PauseCause, history []agent.ReactMessage, taskID, attemptID string) *continuationState {
	digest := progressDigest(history)
	v, _ := d.continuationStates.LoadOrStore(agentID, &continuationState{})
	st := v.(*continuationState)
	st.mu.Lock()
	cp := team.ContinuationCheckpoint{
		SessionID: sid, RunID: fmt.Sprintf("%s-%d", agentID, time.Now().UnixNano()),
		ParentAgentID: parentID, AgentID: agentID, NodeID: agentID,
		TaskID: taskID, AttemptID: attemptID, OwnerKind: ownerKind, Cause: cause,
		HistoryVersion: int64(len(history)), ProgressDigest: digest,
		ContinuationSeq: st.seq, CreatedAt: time.Now(),
	}
	_ = cp.Normalize()
	// 原始总截止时间按会话共享：root/domain/leaf 首次暂停时建立，之后不再延后。
	if st.deadline.IsZero() {
		st.deadline = d.ContinuationDeadline(sid)
	}
	cp.OriginalDeadline = st.deadline
	if st.digest != "" && st.digest == digest {
		st.noProgress++
	} else {
		st.digest = digest
		st.noProgress = 0
	}
	st.cp = cp
	if b, ok := taskBindingFromContext(ctx); ok {
		bb := b
		st.binding = &bb
	}
	st.blocked = false
	st.blockedReason = ""
	// 新检查点意味着旧 run 已暂停/退出；必须复位 single-flight，否则第二次触限
	// 会命中 scheduled 直接返回，节点永久停在等待自动续跑。
	st.scheduled = false
	st.mu.Unlock()
	d.emitContinuation(sid, agentID, "continuation_checkpointed", cp, map[string]any{
		"owner_kind": ownerKind, "history_version": cp.HistoryVersion, "progress_digest": digest,
	})
	return st
}

// scheduleContinuation 旧 run 退出后决定是否自动续跑。必须在所有运行态清理之后调用。
func (d *Dispatcher) scheduleContinuation(ctx context.Context, sid, agentID string) {
	v, ok := d.continuationStates.Load(agentID)
	if !ok {
		return
	}
	st := v.(*continuationState)
	st.mu.Lock()
	cp := st.cp
	var optBinding *team.TaskBinding
	if st.binding != nil {
		bb := *st.binding
		optBinding = &bb
	}
	mode := d.autoResumeMode
	if mode == "" {
		mode = AutoResumeShadow
	}
	remaining := time.Until(st.deadline)
	st.mu.Unlock()
	if !team.AutoResumable(cp.Cause) {
		d.blockContinuation(sid, agentID, st, "cause_not_resumable")
		return
	}
	if sid != "" && d.isSoftStop(sid) {
		d.blockContinuation(sid, agentID, st, "user_stopped")
		return
	}
	if remaining <= 0 {
		d.blockContinuation(sid, agentID, st, string(team.PauseWallClock))
		return
	}
	if d.teamBudgetExceeded != nil && sid != "" && d.teamBudgetExceeded(sid) {
		d.blockContinuation(sid, agentID, st, string(team.PauseTeamBudget))
		return
	}
	st.mu.Lock()
	noProgress, noProgressLimit := st.noProgress, d.autoResumeNoProgressLimit
	st.mu.Unlock()
	if noProgressLimit > 0 && noProgress >= noProgressLimit {
		d.blockContinuation(sid, agentID, st, "no_progress")
		return
	}
	if mode == AutoResumeOff {
		d.blockContinuation(sid, agentID, st, "auto_resume_off")
		return
	}
	if mode == AutoResumeShadow {
		// shadow 只记录“本会恢复”，保持现有人工暂停链路。
		d.emitContinuation(sid, agentID, "continuation_scheduled_shadow", cp, map[string]any{"remaining_ms": remaining.Milliseconds()})
		st.mu.Lock()
		st.blocked = true
		st.blockedReason = "shadow"
		st.mu.Unlock()
		return
	}
	// enforce：single-flight 先占位，再原子占预算（避免先 Load 后 Inc 的竞态）。
	st.mu.Lock()
	if st.scheduled {
		st.mu.Unlock()
		return
	}
	st.scheduled = true
	st.blocked = false
	st.seq++
	seq := st.seq
	st.mu.Unlock()
	n, ok := d.continuationBudget(sid).TryInc(d.autoResumeMaxPerTask)
	if !ok {
		st.mu.Lock()
		st.scheduled = false
		st.mu.Unlock()
		d.blockContinuation(sid, agentID, st, "auto_resume_max_per_task")
		return
	}
	d.emitContinuation(sid, agentID, "continuation_scheduled", cp, map[string]any{
		"continuation_seq": seq, "task_count": n, "remaining_ms": remaining.Milliseconds(),
	})
	if optBinding == nil {
		if b, ok := taskBindingFromContext(ctx); ok {
			bb := b
			optBinding = &bb
		} else if cp.TaskID != "" {
			optBinding = &team.TaskBinding{SessionID: sid, TaskID: cp.TaskID, AttemptID: cp.AttemptID}
		}
	}
	detached := contDetachedContext(ctx, sid, d.subAgentWorkDirFor(ctx))
	// scheduleContinuation 本身由旧 run 最后一个 defer 调用，其余运行态清理已经完成。
	go d.resumePausedNodeWithOptions(detached, agentID, team.ResumeOptions{
		CheckpointID: cp.CheckpointID, Cause: cp.Cause, RemainingTimeout: remaining,
		ContinuationSeq: seq, Origin: "system_continuation", Binding: optBinding,
	})
}

// blockContinuation 标记不可自动续跑并发审计事件。
// 叶子节点同时通知父并释放未决计数，避免父永久等一个不会自动恢复的节点。
func (d *Dispatcher) blockContinuation(sid, agentID string, st *continuationState, reason string) {
	st.mu.Lock()
	cp := st.cp
	var binding *team.TaskBinding
	if st.binding != nil {
		bb := *st.binding
		binding = &bb
	}
	already := st.blocked
	st.blocked = true
	st.scheduled = false
	st.blockedReason = reason
	st.mu.Unlock()
	if already {
		return // 重复调度不得重复告警
	}
	d.emitContinuation(sid, agentID, "continuation_blocked", cp, map[string]any{"reason": reason})
	if cp.ParentAgentID == "" {
		return
	}
	if cp.OwnerKind == "leaf" {
		d.blockLeafContinuationWithBinding(cp, binding)
		return
	}
	// Domain 无法自动续跑：必须让父知情（否则父会一直等一个不会恢复的节点）。
	if d.mailbox != nil {
		if _, err := d.mailbox.Send(&mailbox.Message{
			From: "dispatcher", To: cp.ParentAgentID, Type: mailbox.MsgInfo,
			Subject: "系统自动续跑已停止: " + cp.AgentID,
			Body:    fmt.Sprintf("【系统通知】子 Agent %s 的自动续跑已停止（原因: %s）。节点仍为暂停态，已保留上下文与检查点；请当轮处置：resume_agent 续跑、cancel_agent 放弃，或重新规划。", cp.AgentID, reason),
		}); err != nil {
			log.Printf("[subagent] continuation blocked notify failed: to=%s err=%v", cp.ParentAgentID, err)
		}
	}
}

// blockLeafContinuation 叶子无自动恢复时按未验证交付收口，父可决定返工或接受。
func (d *Dispatcher) blockLeafContinuation(cp team.ContinuationCheckpoint) {
	d.blockLeafContinuationWithBinding(cp, nil)
}

func (d *Dispatcher) blockLeafContinuationWithBinding(cp team.ContinuationCheckpoint, binding *team.TaskBinding) {
	msg := fmt.Sprintf("子 Agent 触限暂停且无法自动续跑（原因: %s）。已保留上下文与检查点，未完成项需人工决定 resume_agent 或重派。", cp.Cause)
	ctx := tool.WithSessionID(context.Background(), cp.SessionID)
	if binding != nil {
		ctx = withTaskBinding(ctx, *binding)
	} else if cp.TaskID != "" {
		ctx = withTaskBinding(ctx, team.TaskBinding{SessionID: cp.SessionID, TaskID: cp.TaskID, AttemptID: cp.AttemptID})
	}
	domain := ""
	if d.treeFn != nil && cp.SessionID != "" {
		if t := d.treeFn(cp.SessionID); t != nil {
			if n, ok := t.Get(cp.NodeID); ok {
				domain = n.Domain
			}
		}
	}
	var history []agent.ReactMessage
	if d.msgStore != nil {
		history, _ = d.msgStore.LoadMessages(ctx, cp.NodeID)
	}
	result := agent.ReactResult{Text: msg, History: history}
	d.refreshVerificationReport(ctx, cp.ParentAgentID, domain, cp.NodeID, result, errVerifyMissing)
	d.boardUpdate(ctx, cp.ParentAgentID, domain, board.TaskUnverified, msg)
	if d.treeFn != nil && cp.SessionID != "" {
		if t := d.treeFn(cp.SessionID); t != nil {
			if n, ok := t.Get(cp.NodeID); ok && n.Status == orchestrator.StatusPaused {
				d.treeFinishStatus(ctx, cp.NodeID, msg, orchestrator.StatusUnverified, msg)
			}
		}
	}
	d.notifyTerminal(cp.NodeID, cp.ParentAgentID, msg, agent.FilesModifiedFromHistory(history))
	d.trackChildDone(cp.ParentAgentID)
	d.finishClaimNow(ctx, cp.NodeID, msg, errVerifyMissing)
	d.finishDispatchAttemptNow(ctx, cp.NodeID, msg, errVerifyMissing)
}

// continuationPending 判断实例是否已有可自动恢复的待续跑。
func (d *Dispatcher) continuationPending(agentID string) bool {
	v, ok := d.continuationStates.Load(agentID)
	if !ok {
		return false
	}
	st := v.(*continuationState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.scheduled && !st.blocked
}

// clearContinuation 终结后清除状态（防止复活/新 attempt 复用旧检查点）。
func (d *Dispatcher) clearContinuation(agentID string) {
	d.continuationStates.Delete(agentID)
}

// findResumableDuplicate 同父同任务的可恢复实例（B-11 防重派）。
// 有 C-03 绑定时按 task_id 匹配，legacy 按 Domain 匹配。
func (d *Dispatcher) findResumableDuplicate(parentID, domain string, binding *team.TaskBinding) (string, bool) {
	if d.treeFn == nil || parentID == "" {
		return "", false
	}
	sid := parentID
	if i := strings.Index(parentID, "/"); i > 0 {
		sid = parentID[:i]
	}
	t := d.treeFn(sid)
	if t == nil {
		return "", false
	}
	for _, n := range t.Snapshot() {
		if n.ParentID != parentID || n.Status != orchestrator.StatusPaused {
			continue
		}
		v, ok := d.continuationStates.Load(n.ID)
		if !ok {
			continue
		}
		st := v.(*continuationState)
		st.mu.Lock()
		cp := st.cp
		pending := st.scheduled || st.cp.Cause != ""
		st.mu.Unlock()
		if !pending {
			continue
		}
		if binding != nil && binding.TaskID != "" {
			if cp.TaskID == binding.TaskID {
				return n.ID, true
			}
			continue
		}
		if strings.TrimSpace(n.Domain) == strings.TrimSpace(domain) {
			return n.ID, true
		}
	}
	return "", false
}

// contDetachedContext 构造续跑用 ctx：保留会话/工作目录/会话停止传播，但不继承已取消的 run ctx。
func contDetachedContext(ctx context.Context, sid, workdir string) context.Context {
	base := tool.StopContextFrom(ctx)
	if base == nil {
		base = context.Background()
	}
	if sid != "" {
		base = tool.WithSessionID(base, sid)
	}
	if workdir != "" {
		base = tool.WithWorkDir(base, workdir)
	}
	return base
}

// emitContinuation 发出续跑审计事件（best-effort；liveFn 未接线时仅日志）。
func (d *Dispatcher) emitContinuation(sid, agentID, action string, cp team.ContinuationCheckpoint, extra map[string]any) {
	detail := map[string]any{
		"action": action, "session_id": sid, "agent_id": agentID,
		"parent_agent_id": cp.ParentAgentID, "node_id": cp.NodeID,
		"task_id": cp.TaskID, "attempt_id": cp.AttemptID,
		"cause": cp.Cause, "checkpoint_id": cp.CheckpointID,
		"continuation_seq": cp.ContinuationSeq, "owner_kind": cp.OwnerKind,
		"original_deadline": cp.OriginalDeadline,
	}
	for k, v := range extra {
		detail[k] = v
	}
	raw, _ := json.Marshal(detail)
	log.Printf("[subagent] %s: sub=%s cause=%s task=%s %s", action, agentID, cp.Cause, cp.TaskID, string(raw))
	if d.liveFn != nil && sid != "" {
		d.liveFn(sid, agent.LiveEvent{
			Kind: agent.LiveEventContinuation, Agent: cp.OwnerKind,
			AgentID: agentID, Detail: string(raw),
		})
	}
}
