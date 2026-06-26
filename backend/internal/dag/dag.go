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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TaskStatus DAG 任务状态。
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"   // 待触发（依赖未满足）
	TaskStatusRunning   TaskStatus = "running"   // 已派发为 session，运行中
	TaskStatusCompleted TaskStatus = "completed" // session 已完成
	TaskStatusFailed    TaskStatus = "failed"    // session 失败
)

// Task DAG 节点。
type Task struct {
	ID         string     `json:"id"`          // 任务 ID（DAG 内唯一）
	Goal       string     `json:"goal"`        // 任务目标，作为 session.goal
	DependsOn  []string   `json:"depends_on"`  // 前置任务 ID 列表
	Status     TaskStatus `json:"status"`      // 当前状态
	SessionID  string     `json:"session_id"`  // 关联 session ID
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DAG 有向无环图：一组带依赖关系的任务 + 可选 cron 调度。
type DAG struct {
	ID        string    `json:"id"`         // DAG 唯一 ID
	Name      string    `json:"name"`       // 人类可读名称
	Cron      string    `json:"cron"`       // 调度表达式（"<N>s" / "" 表示手动触发）
	Tasks     []*Task   `json:"tasks"`      // 任务节点列表
	Enabled   bool      `json:"enabled"`    // 是否启用调度
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasCycle 检测 DAG 是否存在环（拓扑排序失败即有环）。
func (d *DAG) HasCycle() bool {
	_, err := d.TopoSort()
	return err != nil
}

// TopoSort 拓扑排序，返回任务执行顺序；存在环或依赖缺失返回错误。
func (d *DAG) TopoSort() ([]*Task, error) {
	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate task id: %s", t.ID)
		}
		byID[t.ID] = t
	}
	// 校验依赖存在性
	for _, t := range d.Tasks {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("task %s depends on missing %s", t.ID, dep)
			}
		}
	}
	// Kahn 算法
	inDeg := make(map[string]int, len(d.Tasks))
	for _, t := range d.Tasks {
		inDeg[t.ID] = len(t.DependsOn)
	}
	var queue []string
	for id, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	var sorted []*Task
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, byID[id])
		for _, t := range d.Tasks {
			for _, dep := range t.DependsOn {
				if dep == id {
					inDeg[t.ID]--
					if inDeg[t.ID] == 0 {
						queue = append(queue, t.ID)
					}
				}
			}
		}
	}
	if len(sorted) != len(d.Tasks) {
		return nil, fmt.Errorf("cycle detected in dag %s", d.ID)
	}
	return sorted, nil
}

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

// Store DAG 持久化接口（由 store.PostgresStore 实现）。
type Store interface {
	SaveDAG(ctx context.Context, d *DAG) error
	GetDAG(ctx context.Context, id string) (*DAG, error)
	ListDAGs(ctx context.Context) ([]*DAG, error)
	DeleteDAG(ctx context.Context, id string) error
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
	go s.loop(ctx)
}

// Stop 停止调度。
func (s *Scheduler) Stop() { close(s.stop) }

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
func (s *Scheduler) tick(ctx context.Context) {
	dags, err := s.store.ListDAGs(ctx)
	if err != nil || len(dags) == 0 {
		return
	}
	now := time.Now()
	for _, d := range dags {
		if !d.Enabled || d.Cron == "" {
			continue
		}
		interval, err := ParseInterval(d.Cron)
		if err != nil || interval <= 0 {
			continue
		}
		// 简化：若距 updatedAt > interval，则触发
		if now.Sub(d.UpdatedAt) >= interval {
			_ = s.Trigger(ctx, d.ID)
			d.UpdatedAt = now
			_ = s.store.SaveDAG(ctx, d)
		}
	}
	// 推进运行中 DAG
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
// 依赖检测：依赖任务状态均为 completed。
func (s *Scheduler) dispatchReady(ctx context.Context, d *DAG) {
	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		byID[t.ID] = t
	}
	for _, t := range d.Tasks {
		if t.Status != TaskStatusPending {
			continue
		}
		ready := true
		for _, dep := range t.DependsOn {
			if depT, ok := byID[dep]; ok {
				if depT.Status != TaskStatusCompleted {
					ready = false
					break
				}
			} else {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		// 派发
		sid := s.launcher.LaunchSession(t.Goal)
		t.SessionID = sid
		t.Status = TaskStatusRunning
		now := time.Now()
		t.StartedAt = &now
	}
}

// MarkCompleted 标记某 DAG 中某 task 对应的 session 已完成。
// 由 SessionManager 在 session 结束时调用，触发后续依赖任务。
func (s *Scheduler) MarkCompleted(dagID, taskID, sessionID string, success bool) {
	s.mu.Lock()
	d, ok := s.running[dagID]
	s.mu.Unlock()
	if !ok {
		return
	}
	for _, t := range d.Tasks {
		if t.SessionID == sessionID {
			now := time.Now()
			t.FinishedAt = &now
			if success {
				t.Status = TaskStatusCompleted
			} else {
				t.Status = TaskStatusFailed
			}
			break
		}
	}
	// 推进后续依赖
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
		b, _ := json.Marshal(v)
		var cp DAG
		_ = json.Unmarshal(b, &cp)
		out[k] = &cp
	}
	return out
}
