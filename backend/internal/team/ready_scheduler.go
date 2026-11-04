package team

// ready_scheduler.go C-03 确定性 ReadyScheduler（首版）：
// 只消费已批准计划中显式声明 role_id+task 的任务；依赖全部 done 时预留 attempt，
// 再走 Dispatcher 的受控入口。默认关闭（agent.auto_dispatch_enabled=false）。
//
// 不做：Agent 自由认领、Worker 队列、空转 LLM 轮询、绕过权限/预算/审批的旁路。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/board"
)

// AutoTaskSpec 自动调度受控派发入参（冻结任务契约 + 绑定）。
type AutoTaskSpec struct {
	SessionID      string
	PlanVersion    int64
	TaskID         string
	AttemptID      string
	RoleID         string
	Domain         string
	Task           string
	Responsibility string
	Mode           string
	VerifyKind     string
	Risk           string
	ToolsHint      []string
	SkillsHint     []string
	Acceptance     []string
}

// BoundTaskDispatcher 受控派发接口；subagent.Dispatcher 实现。
type BoundTaskDispatcher interface {
	DispatchBoundTask(ctx context.Context, spec AutoTaskSpec) (agentID string, err error)
}

// ReadyScheduler 单进程确定性协调器。
type ReadyScheduler struct {
	Board         func(sessionID string) *board.TaskBoard
	Sessions      func() []string
	Store         AttemptStore
	Dispatcher    BoundTaskDispatcher
	Context       func(sessionID string) context.Context
	AllowedRoles  map[string]bool
	Rescan        time.Duration
	FailurePolicy string // block_dependents（默认）|wait_human
	// MaxAttempts 计划任务未声明 max_attempts 时的默认值（config agent.auto_dispatch_max_attempts）。
	MaxAttempts int
	// ResumeInterrupted 进程重启后是否允许对 interrupted attempt 建新 attempt。
	// 默认 false：保持 blocked，先对账副作用；开启时不重放旧 attempt，而是新建 attempt_no。
	ResumeInterrupted bool

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewReadyScheduler 构造调度器；Rescan<=0 按 5 秒。
func NewReadyScheduler(boardFn func(string) *board.TaskBoard, sessions func() []string, store AttemptStore, disp BoundTaskDispatcher, allowed []string) *ReadyScheduler {
	roles := map[string]bool{}
	if len(allowed) == 0 {
		allowed = []string{"code_assistant", "test_assistant", "scout"}
	}
	for _, r := range allowed {
		roles[strings.TrimSpace(r)] = true
	}
	return &ReadyScheduler{
		Board: boardFn, Sessions: sessions, Store: store, Dispatcher: disp,
		AllowedRoles: roles, Rescan: 5 * time.Second,
		FailurePolicy: "block_dependents", locks: map[string]*sync.Mutex{},
	}
}

// Start 启动低频兜底扫描（事件触发后续接入；扫描零 LLM 调用）。
func (s *ReadyScheduler) Start(ctx context.Context) {
	if s == nil || s.Store == nil || s.Dispatcher == nil || s.Sessions == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(s.Rescan)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, sid := range s.Sessions() {
					if err := s.Reconcile(ctx, sid); err != nil {
						log.Printf("[team] ready scheduler reconcile %s: %v", sid, err)
					}
				}
			}
		}
	}()
}

func (s *ReadyScheduler) sessionLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[sessionID] = m
	}
	return m
}

// Reconcile 对单个 session 做一次确定性调度：读计划 → 找 ready → 预留 attempt → 受控派发。
func (s *ReadyScheduler) Reconcile(ctx context.Context, sessionID string) error {
	if s == nil || s.Board == nil || s.Store == nil || s.Dispatcher == nil {
		return nil
	}
	lk := s.sessionLock(sessionID)
	lk.Lock()
	defer lk.Unlock()

	b := s.Board(sessionID)
	if b == nil {
		return nil
	}
	snap := b.Snapshot()
	if !snap.PlanApproved {
		// 只消费已人工批准的冻结计划；write_plan 本身不触发执行。
		return nil
	}
	done := map[string]bool{}
	attemptsByTask := map[string][]DispatchAttempt{}
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done[t.ID] = true
		}
	}
	attempts, err := s.Store.ListBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list attempts: %w", err)
	}
	for _, a := range attempts {
		attemptsByTask[a.TaskID] = append(attemptsByTask[a.TaskID], a)
	}

	for _, t := range snap.Tasks {
		allPrior := attemptsByTask[t.ID]
		hasInterrupted := false
		for _, a := range allPrior {
			if a.Status == AttemptInterrupted {
				hasInterrupted = true
				break
			}
		}
		if !s.ResumeInterrupted && hasInterrupted {
			// 默认不重放、也不静默挂着：显式 blocked 等人工对账。
			_ = b.MarkBlocked(t.ID, "进程重启后 attempt 状态不确定（interrupted），需人工确认后 resume_interrupted 或重规划")
			continue
		}
		allowedStatus := t.Status == board.TaskPending ||
			(s.ResumeInterrupted && t.Status == board.TaskBlocked && hasInterrupted)
		if !allowedStatus || strings.TrimSpace(t.RoleID) == "" || strings.TrimSpace(t.Task) == "" {
			continue
		}
		if !s.AllowedRoles[t.RoleID] {
			continue
		}
		if !depsDone(t.DependsOn, done) {
			continue
		}
		maxAttempts := t.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = s.MaxAttempts
		}
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		prior := allPrior
		if s.ResumeInterrupted {
			prior = make([]DispatchAttempt, 0, len(allPrior))
			for _, a := range allPrior {
				if a.Status != AttemptInterrupted {
					prior = append(prior, a)
				}
			}
		}
		if len(prior) >= maxAttempts || hasActiveAttempt(prior) {
			continue
		}
		attemptNo := len(allPrior)
		attempt := &DispatchAttempt{
			SessionID: sessionID, PlanVersion: snap.PlanVersion, TaskID: t.ID,
			AttemptNo: attemptNo, Status: AttemptReserved, ParentAgentID: sessionID,
		}
		created, _, err := s.Store.Reserve(ctx, attempt)
		if err != nil {
			return fmt.Errorf("reserve attempt %s: %w", t.ID, err)
		}
		if !created {
			continue
		}
		spec := AutoTaskSpec{
			SessionID: sessionID, PlanVersion: snap.PlanVersion, TaskID: t.ID,
			AttemptID: attempt.AttemptID, RoleID: t.RoleID, Domain: t.Domain,
			Task: t.Task, Responsibility: t.Responsibility, Mode: t.Mode,
			VerifyKind: t.VerifyKind, Risk: t.Risk, ToolsHint: t.ToolsHint,
			SkillsHint: t.SkillsHint, Acceptance: t.Acceptance,
		}
		dispCtx := ctx
		if s.Context != nil {
			if c := s.Context(sessionID); c != nil {
				dispCtx = c
			}
		}
		agentID, derr := s.Dispatcher.DispatchBoundTask(dispCtx, spec)
		now := time.Now()
		if derr != nil {
			attempt.Status = AttemptFailed
			attempt.LastError = derr.Error()
			attempt.FinishedAt = &now
			_ = s.Store.Update(ctx, attempt)
			if s.FailurePolicy == "block_dependents" {
				_ = b.MarkBlocked(t.ID, "自动派发失败: "+derr.Error())
			}
			log.Printf("[team] auto-dispatch failed: session=%s task=%s attempt=%s err=%v", sessionID, t.ID, attempt.AttemptID, derr)
			continue
		}
		attempt.Status = AttemptRunning
		attempt.AgentID = agentID
		attempt.StartedAt = &now
		_ = s.Store.Update(ctx, attempt)
		log.Printf("[team] auto-dispatched: session=%s task=%s role=%s agent=%s", sessionID, t.ID, t.RoleID, agentID)
	}
	return nil
}

func depsDone(deps []string, done map[string]bool) bool {
	for _, d := range deps {
		if !done[d] {
			return false
		}
	}
	return true
}

func hasActiveAttempt(attempts []DispatchAttempt) bool {
	for _, a := range attempts {
		if a.Status == AttemptReserved || a.Status == AttemptRunning {
			return true
		}
	}
	return false
}
