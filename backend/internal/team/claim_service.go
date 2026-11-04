package team

// claim_service.go 认领服务与 WorkerSupervisor（C-06，默认关闭）。
// Worker 的取任务动作由运行时代码执行；认领成功后才启动 ReAct/产生 LLM 调用。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ClaimService 统一认领入口（服务端校验资格后调 Store）。
type ClaimService struct {
	Store        ClaimStore
	AllowedRoles map[string]bool
}

// InitPlan 校验并冻结计划。
func (s *ClaimService) InitPlan(ctx context.Context, env PlanEnvelope) error {
	if s == nil || s.Store == nil {
		return fmt.Errorf("claim service not wired")
	}
	if err := NormalizePlan(&env, s.AllowedRoles); err != nil {
		return err
	}
	return s.Store.InitPlan(ctx, env)
}

// ClaimNext 领取该角色最高优先级就绪任务。
func (s *ClaimService) ClaimNext(ctx context.Context, sessionID, roleID, workerID string, lease time.Duration) (*Claim, error) {
	if s == nil || s.Store == nil {
		return nil, ErrNoClaimableTask
	}
	if s.AllowedRoles != nil && !s.AllowedRoles[roleID] {
		return nil, ErrNotEligible
	}
	return s.Store.ClaimNext(ctx, sessionID, roleID, workerID, lease)
}

// Heartbeat 续租。
func (s *ClaimService) Heartbeat(ctx context.Context, b ClaimBinding, lease time.Duration) error {
	return s.Store.Heartbeat(ctx, b, lease)
}

// Complete 写入终态（done/unverified/failed）。
func (s *ClaimService) Complete(ctx context.Context, b ClaimBinding, res ClaimResult) error {
	return s.Store.Complete(ctx, b, res)
}

// Fail 失败/阻塞收口。
func (s *ClaimService) Fail(ctx context.Context, b ClaimBinding, status, errText string) error {
	return s.Store.Fail(ctx, b, status, errText)
}

// Release 释放回 pending。
func (s *ClaimService) Release(ctx context.Context, b ClaimBinding) error {
	return s.Store.Release(ctx, b)
}

// ClaimExecutor 执行认领任务的适配器（subagent.Dispatcher 实现）。
type ClaimExecutor interface {
	DispatchClaimed(ctx context.Context, claim Claim) (agentID string, err error)
}

// WorkerSupervisor 确定性 Worker 控制面（无 LLM 轮询）。
type WorkerSupervisor struct {
	Service  *ClaimService
	Executor ClaimExecutor
	Sessions func() []string
	// Context 按 session 构造执行 ctx（需带 session/workdir/父 agentID）；nil 用扫描 ctx。
	Context           func(sessionID string) context.Context
	Roles             []string
	WorkersPerRole    int
	Lease             time.Duration
	HeartbeatInterval time.Duration
	ScanInterval      time.Duration

	active      sync.Map // attemptID -> ClaimBinding
	activeRoles sync.Map // attemptID -> roleID
	stop        chan struct{}
	stopOnce    sync.Once
	seq         atomic.Int64
	mu          sync.Mutex
	running     int
	roleRunning map[string]int
}

// NewWorkerSupervisor 构造 Supervisor；默认 lease 120s / 心跳 30s / 扫描 5s。
func NewWorkerSupervisor(svc *ClaimService, exec ClaimExecutor, sessions func() []string, roles []string) *WorkerSupervisor {
	return &WorkerSupervisor{
		Service: svc, Executor: exec, Sessions: sessions, Roles: roles,
		WorkersPerRole: 1, Lease: 120 * time.Second, HeartbeatInterval: 30 * time.Second,
		ScanInterval: 5 * time.Second, stop: make(chan struct{}), roleRunning: map[string]int{},
	}
}

// Start 启动扫描与心跳（无 claim 时不调用 LLM）。
func (s *WorkerSupervisor) Start(ctx context.Context) {
	if s == nil || s.Service == nil || s.Executor == nil || s.Sessions == nil {
		return
	}
	go s.scanLoop(ctx)
	go s.heartbeatLoop(ctx)
}

// Stop 停止新认领与心跳。
func (s *WorkerSupervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *WorkerSupervisor) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(s.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce 对全部会话执行一次认领扫描。
func (s *WorkerSupervisor) RunOnce(ctx context.Context) {
	roles := s.Roles
	if len(roles) == 0 {
		roles = []string{"code_assistant", "test_assistant", "scout"}
	}
	limit := s.WorkersPerRole
	if limit <= 0 {
		limit = 1
	}
	for _, sid := range s.Sessions() {
		for _, role := range roles {
			for i := 0; i < limit; i++ {
				// 限制该角色总运行并发，而非每轮扫描数：上一轮已认领仍在跑的也计入。
				s.mu.Lock()
				full := s.roleRunning[role] >= limit
				s.mu.Unlock()
				if full {
					break
				}
				workerID := fmt.Sprintf("worker-%s-%s-%d", sid, role, s.seq.Add(1))
				claim, err := s.Service.ClaimNext(ctx, sid, role, workerID, s.Lease)
				if err != nil {
					if errors.Is(err, ErrNoClaimableTask) {
						break
					}
					log.Printf("[team] claim next failed: session=%s role=%s err=%v", sid, role, err)
					break
				}
				b := ClaimBinding{
					SessionID: claim.SessionID, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
					WorkerID: claim.WorkerID, AttemptNo: claim.AttemptNo,
					PlanVersion: claim.PlanVersion, SpecVersion: claim.SpecVersion,
				}
				s.active.Store(claim.AttemptID, b)
				s.activeRoles.Store(claim.AttemptID, role)
				s.mu.Lock()
				s.running++
				s.roleRunning[role]++
				s.mu.Unlock()
				execCtx := ctx
				if s.Context != nil {
					if c := s.Context(sid); c != nil {
						execCtx = c
					}
				}
				go func(c *Claim, bind ClaimBinding, runCtx context.Context) {
					defer func() {
						s.mu.Lock()
						s.running--
						s.mu.Unlock()
					}()
					if _, derr := s.Executor.DispatchClaimed(runCtx, *c); derr != nil {
						log.Printf("[team] DispatchClaimed failed: session=%s task=%s err=%v", c.SessionID, c.TaskID, derr)
						_ = s.Service.Fail(ctx, bind, TaskBlocked, derr.Error())
						s.releaseRole(c.AttemptID)
						s.active.Delete(c.AttemptID)
					}
				}(claim, b, execCtx)
			}
		}
	}
}

func (s *WorkerSupervisor) heartbeatLoop(ctx context.Context) {
	interval := s.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.active.Range(func(k, v any) bool {
				b, ok := v.(ClaimBinding)
				if !ok {
					s.active.Delete(k)
					return true
				}
				if err := s.Service.Heartbeat(ctx, b, s.Lease); err != nil {
					log.Printf("[team] heartbeat fencing rejected: attempt=%s err=%v", b.AttemptID, err)
					// 租约/所有权已失效：释放角色名额，避免容量永久占满。
					s.releaseRole(b.AttemptID)
					s.active.Delete(k)
				}
				return true
			})
		}
	}
}

// FinishClaim 由 Dispatcher 终态 hook 调用：写事务结果并停止续租。
func (s *WorkerSupervisor) FinishClaim(ctx context.Context, b ClaimBinding, status, summary, resultID string) {
	if s == nil || s.Service == nil {
		return
	}
	if status == TaskCancelled || status == TaskBlocked {
		_ = s.Service.Fail(ctx, b, status, summary)
	} else {
		_ = s.Service.Complete(ctx, b, ClaimResult{Status: status, Summary: summary, ResultID: resultID})
	}
	s.releaseRole(b.AttemptID)
	s.active.Delete(b.AttemptID)
}

// releaseRole 释放该 attempt 占用的角色运行名额（只减一次）。
func (s *WorkerSupervisor) releaseRole(attemptID string) {
	v, ok := s.activeRoles.LoadAndDelete(attemptID)
	if !ok {
		return
	}
	role, _ := v.(string)
	s.mu.Lock()
	if s.roleRunning[role] > 0 {
		s.roleRunning[role]--
	}
	s.mu.Unlock()
}
