// Package dag 实现特性1：基于 DAG（有向无环图）的定时任务流程。
//
// 设计要点：
//   - Task：DAG 节点，含 id / goal / depends_on[] / status / session_id
//   - DAG：一组 Task + cron 表达式 + 名称；持久化到 Postgres dag_jobs 表
//   - Scheduler：进程级后台 goroutine，按 cron 触发 DAG，按依赖关系串/并行
//     派发任务；每个 Task 对应一个 SessionManager.CreateSession 调用
//
// 当前实现：cron 仅支持简单 "every N seconds" 形式（"<N>s"），避免引入
// 完整 cron 库依赖。依赖检测通过轮询 SessionManager 中 session.Status 完成。
package dag

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TaskStatus, Task, and DAG are aliases to pkg/types so that the storage layer
// can depend on the type without importing the dag scheduler package.
type TaskStatus = types.TaskStatus
type Task = types.Task
type DAG = types.DAG

const (
	TaskStatusPending   = types.TaskStatusPending
	TaskStatusRunning   = types.TaskStatusRunning
	TaskStatusCompleted = types.TaskStatusCompleted
	TaskStatusFailed    = types.TaskStatusFailed
)

// ParseInterval 解析 cron 字段为定时间隔。
// 支持 "Ns"（N 秒）、"Nm"（N 分钟）、"Nh"（N 小时）；空串返回 0 表示仅手动触发。
func ParseInterval(cron string) (time.Duration, error) {
	cron = strings.TrimSpace(cron)
	if cron == "" {
		return 0, nil
	}
	if len(cron) < 2 {
		return 0, fmt.Errorf("invalid cron: %s", cron)
	}
	unit := cron[len(cron)-1]
	numStr := cron[:len(cron)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid cron number: %s", cron)
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	}
	return 0, fmt.Errorf("unsupported cron unit: %c", unit)
}

// SessionLauncher 把一个 task.goal 派发为新 session。
// 由 server.SessionManager 实现并注入到 Scheduler。
type SessionLauncher interface {
	LaunchSession(goal string) string // 返回 sessionID
}

// Scheduler DAG 调度器：周期轮询所有启用 DAG，按 cron 触发，按依赖派发任务。
type Scheduler struct {
	store    Store
	launcher SessionLauncher
	mu       sync.Mutex
	running  map[string]*DAG // 正在执行的 DAG 实例（含运行中 task 状态）
	interval time.Duration   // 调度器自身轮询间隔
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewScheduler 创建调度器。interval<=0 时默认 10s。
func NewScheduler(store Store, launcher SessionLauncher, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Scheduler{
		store:    store,
		launcher: launcher,
		running:  make(map[string]*DAG),
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// Start 启动后台调度 goroutine。
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.loop(ctx)
}

// Stop 停止调度并等待后台 goroutine 退出。可安全多次调用。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Trigger 立即触发一次 DAG（忽略 cron）。
// 把 DAG 拷贝一份运行实例，按依赖关系派发可执行任务。
func (s *Scheduler) Trigger(ctx context.Context, id string) error {
	d, err := s.store.GetDAG(ctx, id)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("dag %s not found", id)
	}
	if d.HasCycle() {
		return fmt.Errorf("dag %s has cycle", id)
	}
	// 深拷贝一份运行实例，避免修改 store 返回的原始 DAG。
	d = cloneDAG(d)
	// 重置所有 task 状态为 pending
	for _, t := range d.Tasks {
		t.Status = TaskStatusPending
		t.SessionID = ""
		t.StartedAt = nil
		t.FinishedAt = nil
	}
	s.mu.Lock()
	s.running[id] = d
	s.mu.Unlock()
	s.dispatchReady(ctx, d)
	return nil
}

// loop 调度主循环。
func (s *Scheduler) loop(ctx context.Context) {
	defer s.wg.Done()
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick(ctx)
		}
	}
}

// tick 一次调度轮询：检查 cron 触发 + 推进运行中 DAG 的依赖。
// 触发判定（简化版）：当前时间 - updatedAt >= interval 即触发；trigger 后回写 updatedAt
// 作为下次触发的起点。这避免引入独立 lastFire 字段，但也意味着触发后必须落库。
func (s *Scheduler) tick(ctx context.Context) {
	dags, err := s.store.ListDAGs(ctx)
	if err != nil || len(dags) == 0 {
		return
	}
	now := time.Now()
	for _, d := range dags {
		if !d.Enabled || d.Cron == "" {
			continue // 未启用或仅手动触发的 DAG 跳过 cron 检查
		}
		interval, err := ParseInterval(d.Cron)
		if err != nil || interval <= 0 {
			continue
		}
		// 简化：若距 updatedAt > interval，则触发
		if now.Sub(d.UpdatedAt) >= interval {
			if err := s.Trigger(ctx, d.ID); err != nil {
				log.Printf("[DAG] trigger failed: id=%s err=%v", d.ID, err)
			}
			d.UpdatedAt = now
			if err := s.store.SaveDAG(ctx, d); err != nil { // 持久化新的 updatedAt，避免重复触发
				log.Printf("[DAG] save dag failed: id=%s err=%v", d.ID, err)
			}
		}
	}
	// 推进运行中 DAG：拷贝一份引用后释放锁，再逐个 dispatchReady，避免长持锁
	s.mu.Lock()
	running := make(map[string]*DAG, len(s.running))
	for k, v := range s.running {
		running[k] = v
	}
	s.mu.Unlock()
	for _, d := range running {
		s.dispatchReady(ctx, d)
	}
}

// dispatchReady 把所有依赖已完成的 pending 任务派发为 session。
// 依赖检测：依赖任务状态均为 completed；任一依赖未完成或缺失则保持 pending。
// 派发动作：LaunchSession(goal) 返回 sessionID，写入 task 并置 Running。
func (s *Scheduler) dispatchReady(ctx context.Context, d *DAG) {
	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		byID[t.ID] = t
	}
	for _, t := range d.Tasks {
		if t.Status != TaskStatusPending {
			continue // 已派发或已完成的不再处理
		}
		ready := true
		for _, dep := range t.DependsOn {
			if depT, ok := byID[dep]; ok {
				if depT.Status != TaskStatusCompleted {
					ready = false // 任一依赖未完成，保持 pending
					break
				}
			} else {
				ready = false // 依赖指向不存在的任务，保守不派发
				break
			}
		}
		if !ready {
			continue
		}
		// 派发：把 task.goal 作为新 session 的输入；sessionID 用于后续 MarkCompleted 回调
		sid := s.launcher.LaunchSession(t.Goal)
		t.SessionID = sid
		t.Status = TaskStatusRunning
		now := time.Now()
		t.StartedAt = &now
	}
}

// MarkCompleted 标记某 DAG 中某 task 对应的 session 已完成。
// 由 SessionManager 在 session 结束时调用，触发后续依赖任务。
// 注意：参数 taskID 当前未使用，匹配靠 sessionID 唯一定位 task。
func (s *Scheduler) MarkCompleted(dagID, taskID, sessionID string, success bool) {
	s.mu.Lock()
	d, ok := s.running[dagID]
	s.mu.Unlock()
	if !ok {
		// DAG 已被清理或不在运行中：忽略回调，避免空指针
		return
	}
	for _, t := range d.Tasks {
		if t.SessionID == sessionID {
			now := time.Now()
			t.FinishedAt = &now
			if success {
				t.Status = TaskStatusCompleted
			} else {
				t.Status = TaskStatusFailed // 失败标记让下游依赖永远不会 ready
			}
			break
		}
	}
	// 推进后续依赖：依赖本 task 的 pending 任务，若其他依赖也都完成，会被 dispatchReady 派发
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.dispatchReady(ctx, d)
}

// Snapshot 返回当前运行中 DAG 的拷贝（供 HTTP /api/dag 运行态展示）。
func (s *Scheduler) Snapshot() map[string]*DAG {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*DAG, len(s.running))
	for k, v := range s.running {
		// 深拷贝避免外部修改
		out[k] = cloneDAG(v)
	}
	return out
}

// cloneTask returns a deep copy of a Task. nil slices remain nil, empty slices
// remain empty, and pointer fields are allocated independently.
func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	cp := &Task{
		ID:        t.ID,
		Goal:      t.Goal,
		Status:    t.Status,
		SessionID: t.SessionID,
	}
	if t.DependsOn != nil {
		cp.DependsOn = make([]string, len(t.DependsOn))
		copy(cp.DependsOn, t.DependsOn)
	}
	if t.StartedAt != nil {
		v := *t.StartedAt
		cp.StartedAt = &v
	}
	if t.FinishedAt != nil {
		v := *t.FinishedAt
		cp.FinishedAt = &v
	}
	return cp
}

// cloneDAG returns a deep copy of a DAG, including a deep copy of every Task.
func cloneDAG(d *DAG) *DAG {
	if d == nil {
		return nil
	}
	cp := &DAG{
		ID:        d.ID,
		Name:      d.Name,
		Cron:      d.Cron,
		Enabled:   d.Enabled,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
	if d.Tasks != nil {
		cp.Tasks = make([]*Task, len(d.Tasks))
		for i, t := range d.Tasks {
			cp.Tasks[i] = cloneTask(t)
		}
	}
	return cp
}
