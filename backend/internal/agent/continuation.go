package agent

// continuation.go 主 Agent 执行连续性（B-11 进程内）：
// 顶层会话自身触限（上下文阈值/迭代上限）时，暂停后由系统内部唤醒续跑，
// 不要求用户输入“继续”。复用 WakeSuspended 的 wakeInput 通道，但不写用户决策账本、
// 不伪造用户指令；用户停止/审批/澄清/硬预算仍走人工暂停。
//
// 注意：历史中会保留一条系统续跑输入（origin=system_continuation），这是复用现有
// ReAct 续跑入口的取舍；原始任务与用户指令不变。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// 自动续跑模式（与 subagent 同名常量语义一致）。
const (
	autoResumeOff     = "off"
	autoResumeShadow  = "shadow"
	autoResumeEnforce = "enforce"
)

// sessionContinuationState 会话级自动续跑状态。
type sessionContinuationState struct {
	mu              sync.Mutex
	digest          string
	noProgress      int
	seq             int
	deadline        time.Time
	scheduled       bool
	scheduledSeq    int
	scheduledRunSeq int64
	scheduledCause  string
}

type sessionContinuationCounter struct {
	mu sync.Mutex
	n  int
}

func (c *sessionContinuationCounter) inc() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

func (c *sessionContinuationCounter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// SetAutoResumeBudget 复用子 Agent 的同一任务级续跑预算（root/domain/leaf 共享）。
func (s *ReactService) SetAutoResumeBudget(fn func(sessionID string) (int, bool)) {
	s.autoResumeConsume = fn
}

// SetAutoResumeDeadline 复用子 Agent 的会话原始总截止时间（首次暂停建立，不延后）。
func (s *ReactService) SetAutoResumeDeadline(fn func(sessionID string) time.Time) {
	s.autoResumeDeadline = fn
}

// SetAutoResumeSeedDeadline 在会话开始时用根任务原始截止时间预置共享 deadline。
func (s *ReactService) SetAutoResumeSeedDeadline(fn func(sessionID string, deadline time.Time)) {
	s.autoResumeSeed = fn
}

// SetAutoResume 配置主 Agent 自动续跑（bootstrap 按 agent.auto_resume_* 注入）。
func (s *ReactService) SetAutoResume(mode string, maxPerTask, noProgressLimit int) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case autoResumeOff:
		s.autoResumeMode = autoResumeOff
	case autoResumeEnforce:
		s.autoResumeMode = autoResumeEnforce
	default:
		s.autoResumeMode = autoResumeShadow
	}
	if maxPerTask > 0 {
		s.autoResumeMaxPerTask = maxPerTask
	}
	if noProgressLimit > 0 {
		s.autoResumeNoProgressLimit = noProgressLimit
	}
}

// sessionProgressDigest 确定性进展：文件修改 + 成功工具结果数 + 末条正文。
func sessionProgressDigest(history []ReactMessage) string {
	files := FilesModifiedFromHistory(history)
	seen := map[string]bool{}
	for _, m := range history {
		if m.Role != "tool" {
			continue
		}
		var tr ToolResult
		if json.Unmarshal([]byte(m.Content), &tr) == nil && tr.Success {
			seen[fmt.Sprintf("%x", sha256.Sum256([]byte(tr.Tool+"|"+tr.Output)))] = true
		}
	}
	hashes := make([]string, 0, len(seen))
	for h := range seen {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	last := LastAssistantText(history)
	return fmt.Sprintf("%s|%s|%s", strings.Join(files, "\x00"), strings.Join(hashes, "\x00"), last)
}

// tryScheduleSessionAutoResume 在顶层 LimitReached 时开系统续跑。
// 返回 true 表示已接管（调用方不要再 pauseSession）。shadow 只记录，仍走人工暂停。
func (s *ReactService) tryScheduleSessionAutoResume(session *reactInternalSession, result ReactResult) bool {
	if result.PausedOnChild {
		return false
	}
	mode := s.autoResumeMode
	if mode == "" {
		mode = autoResumeShadow
	}
	cause := result.PauseCause
	if cause == "" {
		cause = "context_threshold"
	}
	if cause != "context_threshold" && cause != "iteration_limit" {
		return false
	}
	digest := sessionProgressDigest(result.History)
	stateV, _ := s.sessionContinuations.LoadOrStore(session.ID, &sessionContinuationState{})
	st := stateV.(*sessionContinuationState)
	// 已有待恢复代际时先命中 single-flight；即使预算刚好为 1，也不能把重复触发退回人工暂停。
	st.mu.Lock()
	alreadyScheduled := st.scheduled
	st.mu.Unlock()
	if mode == autoResumeEnforce && alreadyScheduled {
		return true
	}
	var blockedReason string
	st.mu.Lock()
	if st.deadline.IsZero() {
		if s.autoResumeDeadline != nil {
			// 与子 Agent 共享同一会话原始截止时间。
			st.deadline = s.autoResumeDeadline(session.ID)
		} else if s.runtimeCfg.SessionMaxWallClockMin > 0 {
			st.deadline = time.Now().Add(time.Duration(s.runtimeCfg.SessionMaxWallClockMin) * time.Minute)
		}
	}
	if st.digest != "" && st.digest == digest {
		st.noProgress++
	} else {
		st.digest = digest
		st.noProgress = 0
	}
	if st.noProgress > 0 && s.autoResumeNoProgressLimit > 0 && st.noProgress >= s.autoResumeNoProgressLimit {
		blockedReason = "no_progress"
	}
	if !st.deadline.IsZero() && time.Now().After(st.deadline) {
		blockedReason = "wall_clock_exhausted"
	}
	st.mu.Unlock()
	if blockedReason != "" {
		s.emitSessionContinuation(session, "continuation_blocked", map[string]any{
			"cause": cause, "reason": blockedReason,
		})
		return false
	}
	if s.autoResumeConsume == nil && s.autoResumeMaxPerTask > 0 && s.sessionContinuationBudget(session.ID).load() >= s.autoResumeMaxPerTask {
		s.emitSessionContinuation(session, "continuation_blocked", map[string]any{
			"cause": cause, "reason": "auto_resume_max_per_task",
		})
		return false
	}
	if mode == autoResumeShadow {
		s.emitSessionContinuation(session, "continuation_scheduled_shadow", map[string]any{"cause": cause})
		return false
	}
	if mode != autoResumeEnforce {
		return false
	}
	// 用户已停止的会话不续跑。
	if s.stopMarker != nil {
		if q, ok := s.stopMarker.(interface{ IsSoftStop(string) bool }); ok && q.IsSoftStop(session.ID) {
			return false
		}
	}
	// 先占位再消耗预算：重复触发只保留一个待恢复代际，且不得重复扣减共享预算。
	s.store.mu.RLock()
	startSeq := session.runSeq
	s.store.mu.RUnlock()
	st.mu.Lock()
	if st.scheduled {
		st.mu.Unlock()
		return true
	}
	st.seq++
	seq := st.seq
	st.scheduled = true
	st.scheduledSeq = seq
	st.scheduledRunSeq = startSeq
	st.scheduledCause = cause
	st.mu.Unlock()
	var n int
	if s.autoResumeConsume != nil {
		var ok bool
		n, ok = s.autoResumeConsume(session.ID)
		if !ok {
			s.clearScheduledSessionContinuation(session.ID, seq)
			s.emitSessionContinuation(session, "continuation_blocked", map[string]any{
				"cause": cause, "reason": "auto_resume_max_per_task",
			})
			return false
		}
	} else {
		n = s.sessionContinuationBudget(session.ID).inc()
	}
	s.emitSessionContinuation(session, "continuation_scheduled", map[string]any{
		"cause": cause, "continuation_seq": seq, "task_count": n,
	})
	// 只登记暂停态；finalizeSession 在旧 run 清理完成后触发恢复，不靠固定睡眠猜时序。
	s.pauseSession(session, result.History, PauseIterationLimit)
	return true
}

func (s *ReactService) clearScheduledSessionContinuation(sessionID string, seq int) {
	stateV, ok := s.sessionContinuations.Load(sessionID)
	if !ok {
		return
	}
	st := stateV.(*sessionContinuationState)
	st.mu.Lock()
	if st.scheduledSeq == seq {
		st.scheduled = false
	}
	st.mu.Unlock()
}

// claimScheduledSessionContinuation 在旧 run 收尾后原子认领一次系统恢复。
// runSeq + scheduledSeq 双重代际检查使用户恢复／新消息优先，陈旧调度不能覆盖新状态。
func (s *ReactService) claimScheduledSessionContinuation(session *reactInternalSession) (int, string, bool) {
	stateV, ok := s.sessionContinuations.Load(session.ID)
	if !ok {
		return 0, "", false
	}
	st := stateV.(*sessionContinuationState)
	st.mu.Lock()
	if !st.scheduled {
		st.mu.Unlock()
		return 0, "", false
	}
	seq, startSeq, cause := st.scheduledSeq, st.scheduledRunSeq, st.scheduledCause
	st.mu.Unlock()

	s.store.mu.Lock()
	valid := session.runSeq == startSeq && session.Status == enums.SessionStatusAwaitingClarify &&
		session.stopCtx != nil && session.stopCtx.Err() == nil
	if valid {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		session.wakeInput = "【系统续跑】上一轮因上下文/迭代触限暂停，系统已自动继续（origin=system_continuation）。" +
			"请读取当前进度与未完成项继续执行，不要重复询问用户，不要重置任务目标。"
		restartSessionContext(session)
	}
	s.store.mu.Unlock()
	s.clearScheduledSessionContinuation(session.ID, seq)
	if !valid {
		s.emitSessionContinuation(session, "continuation_blocked", map[string]any{
			"cause": cause, "reason": "session_no_longer_resumable",
		})
		return 0, "", false
	}
	s.store.persistHistory(session)
	s.store.persistEvents(session)
	s.emitSessionContinuation(session, "continuation_resumed", map[string]any{
		"cause": cause, "continuation_seq": seq,
	})
	log.Printf("[react] system continuation resume: session=%s cause=%s seq=%d", session.ID, cause, seq)
	return seq, cause, true
}

func (s *ReactService) resumeScheduledSessionContinuation(session *reactInternalSession) {
	if _, _, ok := s.claimScheduledSessionContinuation(session); ok {
		go s.resumeSession(session)
	}
}

func (s *ReactService) sessionContinuationBudget(sessionID string) *sessionContinuationCounter {
	v, _ := s.sessionContinuationBudgetCnt.LoadOrStore(sessionID, &sessionContinuationCounter{})
	return v.(*sessionContinuationCounter)
}

// emitSessionContinuation 落 stats 审计事件，不进入对话正文。
func (s *ReactService) emitSessionContinuation(session *reactInternalSession, action string, extra map[string]any) {
	if session == nil {
		return
	}
	detail := map[string]any{"action": action, "session_id": session.ID, "origin": "system_continuation"}
	for k, v := range extra {
		detail[k] = v
	}
	raw, _ := json.Marshal(detail)
	s.store.addEventDetail(session, eventkind.Stats, "System", "执行连续性", eventkind.Continuation, "", "", "", "", true, string(raw))
}
