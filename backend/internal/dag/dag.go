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
	"context" // 上下文，用于取消与超时传递
	"fmt"     // 格式化错误信息
	"log"     // 未注入 logger 时的回退输出
	"strconv" // 字符串与整数转换
	"strings" // 字符串裁剪
	"sync"    // 互斥锁与 WaitGroup
	"time"    // 时间解析与定时器

	"github.com/blockmemory/agent/backend/internal/logger" // 结构化日志器
	"github.com/blockmemory/agent/backend/pkg/types"       // Task / DAG 等公共类型
)

// TaskStatus 是 task 状态类型别名，指向 pkg/types.TaskStatus。
//
// 设计说明：通过别名让存储层可以依赖该类型，而无需反向导入 dag 调度包，
// 从而避免层间循环依赖。
type TaskStatus = types.TaskStatus

// Task 是 DAG 节点类型别名，指向 pkg/types.Task。
type Task = types.Task

// DAG 是有向无环图类型别名，指向 pkg/types.DAG。
type DAG = types.DAG

const (
	// TaskStatusPending 表示任务等待派发。
	TaskStatusPending = types.TaskStatusPending
	// TaskStatusRunning 表示任务已派发，对应的 session 正在执行。
	TaskStatusRunning = types.TaskStatusRunning
	// TaskStatusCompleted 表示任务已成功完成。
	TaskStatusCompleted = types.TaskStatusCompleted
	// TaskStatusFailed 表示任务执行失败，下游依赖将永远无法就绪。
	TaskStatusFailed = types.TaskStatusFailed
)

// ParseInterval 解析 cron 字段为定时间隔。
//
// 支持格式：
//   - "Ns"：N 秒
//   - "Nm"：N 分钟
//   - "Nh"：N 小时
//   - 空串：返回 0，表示仅手动触发
//
// 参数：
//   - cron：待解析的 cron 字符串。
//
// 返回：
//   - 解析成功返回对应的 time.Duration 与 nil error。
//   - 空串返回 0 与 nil error。
//   - 格式错误返回 0 与描述性 error。
func ParseInterval(cron string) (time.Duration, error) {
	cron = strings.TrimSpace(cron)
	// 空串视为手动触发，不报错
	if cron == "" {
		return 0, nil
	}
	// 长度至少为 2，才能容纳数字 + 单位
	if len(cron) < 2 {
		return 0, fmt.Errorf("invalid cron: %s", cron)
	}
	// 最后一位是单位，前面是数字
	unit := cron[len(cron)-1]
	numStr := cron[:len(cron)-1]
	n, err := strconv.Atoi(numStr)
	// 数字解析失败或不是正数均视为非法
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid cron number: %s", cron)
	}
	// 根据单位返回对应 duration
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
//
// 实现说明：由 server.SessionManager 实现并通过构造函数注入 Scheduler。
type SessionLauncher interface {
	// LaunchSession 接收 task 的目标描述，创建并返回新 session 的 ID。
	LaunchSession(goal string) string
}

// Scheduler DAG 调度器：周期轮询所有启用 DAG，按 cron 触发，按依赖派发任务。
//
// 并发说明：
//   - store / launcher 为外部注入依赖，自身不保证并发安全，由调用方维护。
//   - running map 受 mu 保护；Snapshot / Trigger / MarkCompleted / tick 均会访问。
//   - stop / stopOnce 保证 Stop 只关闭一次 stop channel。
//   - wg 等待 loop goroutine 退出。
type Scheduler struct {
	store    Store           // DAG 持久化存储
	launcher SessionLauncher // session 派发器
	log      *logger.Logger  // 结构化日志器，由 SetLogger 注入；nil 时回退标准库 log
	mu       sync.Mutex      // 保护 running map
	running  map[string]*DAG // 正在执行的 DAG 实例（含运行中 task 状态）
	interval time.Duration   // 调度器自身轮询间隔
	stop     chan struct{}   // 关闭以通知 loop 退出
	stopOnce sync.Once       // 保证 stop channel 仅关闭一次
	wg       sync.WaitGroup  // 等待 loop goroutine 结束
}

// NewScheduler 创建调度器。
//
// 参数：
//   - store：DAG 持久化接口实现。
//   - launcher：session 派发接口实现。
//   - interval：调度器轮询间隔；<=0 时默认 10s。
//
// 返回：初始化后的 *Scheduler，running map 与 stop channel 已创建。
func NewScheduler(store Store, launcher SessionLauncher, interval time.Duration) *Scheduler {
	// 非法间隔回退到默认值
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
//
// 参数：
//   - ctx：上下文；loop 会同时监听 ctx.Done() 与 s.stop，任一触发都会退出。
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.loop(ctx)
}

// SetLogger 注入结构化日志器，使调度错误以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log（级别固定 INFO）。
func (s *Scheduler) SetLogger(l *logger.Logger) {
	s.log = l
}

// logError 记录错误类日志；未注入 logger 时回退标准库 log，保持旧行为。
func (s *Scheduler) logError(ctx context.Context, msg string, err error) {
	if s.log != nil {
		s.log.Error(ctx, msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}

// Stop 停止调度并等待后台 goroutine 退出。
//
// 说明：可安全多次调用；第二次及以后会因 stopOnce 而直接等待，不会重复关闭 channel。
func (s *Scheduler) Stop() {
	// 仅首次调用关闭 stop channel
	s.stopOnce.Do(func() { close(s.stop) })
	// 等待 loop goroutine 退出
	s.wg.Wait()
}

// Trigger 立即触发一次 DAG（忽略 cron）。
//
// 执行流程：
//  1. 从 store 读取 DAG。
//  2. 检查是否存在以及是否有环。
//  3. 深拷贝一份运行实例，避免修改 store 返回的原始对象。
//  4. 重置所有 task 状态为 pending。
//  5. 将运行实例放入 running map。
//  6. 立即 dispatchReady，派发出首批无依赖任务。
//
// 参数：
//   - ctx：上下文。
//   - id：DAG 唯一标识。
//
// 返回：DAG 不存在、有环或存储读取失败时返回 error。
func (s *Scheduler) Trigger(ctx context.Context, id string) error {
	d, err := s.store.GetDAG(ctx, id)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("dag %s not found", id)
	}
	// 有环 DAG 不能执行
	if d.HasCycle() {
		return fmt.Errorf("dag %s has cycle", id)
	}
	// 深拷贝一份运行实例，避免修改 store 返回的原始 DAG
	d = cloneDAG(d)
	// 重置所有 task 状态为 pending，清空上一次的 session 与时间信息
	for _, t := range d.Tasks {
		t.Status = TaskStatusPending
		t.SessionID = ""
		t.StartedAt = nil
		t.FinishedAt = nil
	}
	// 放入 running map，开始跟踪其生命周期
	s.mu.Lock()
	s.running[id] = d
	s.mu.Unlock()
	// 立即派发所有当前依赖已满足的任务
	s.dispatchReady(ctx, d)
	return nil
}

// loop 调度主循环。
//
// 说明：在独立 goroutine 中运行，使用 time.Ticker 按 interval 周期执行 tick，
// 直到 stop channel 或 ctx 被取消。
func (s *Scheduler) loop(ctx context.Context) {
	// 通知 Stop() 当前 goroutine 结束
	defer s.wg.Done()
	// 创建周期 ticker
	tick := time.NewTicker(s.interval)
	// 退出前停止 ticker，防止泄漏
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			// 收到停止信号，立即返回
			return
		case <-ctx.Done():
			// 外部上下文取消，立即返回
			return
		case <-tick.C:
			// 每个 tick 检查 cron 触发并推进运行中 DAG
			s.tick(ctx)
		}
	}
}

// tick 一次调度轮询：检查 cron 触发 + 推进运行中 DAG 的依赖。
//
// cron 触发判定（简化版）：
//   - 当前时间 - updatedAt >= interval 即触发。
//   - trigger 后回写 updatedAt 作为下次触发起点。
//   - 这样避免引入独立 lastFire 字段，但也意味着触发后必须落库。
func (s *Scheduler) tick(ctx context.Context) {
	// 列出所有 DAG 定义
	dags, err := s.store.ListDAGs(ctx)
	// 读取失败或无 DAG 时本次 tick 直接结束
	if err != nil || len(dags) == 0 {
		return
	}
	now := time.Now()
	// 遍历每个 DAG，检查是否需要按 cron 触发
	for _, d := range dags {
		// 未启用或仅手动触发的 DAG 跳过 cron 检查
		if !d.Enabled || d.Cron == "" {
			continue
		}
		// 解析 cron 为定时间隔
		interval, err := ParseInterval(d.Cron)
		if err != nil || interval <= 0 {
			continue
		}
		// 简化：若距 updatedAt 超过 interval，则触发该 DAG
		if now.Sub(d.UpdatedAt) >= interval {
			if err := s.Trigger(ctx, d.ID); err != nil {
				s.logError(ctx, fmt.Sprintf("[DAG] trigger failed: id=%s", d.ID), err)
			}
			// 更新 updatedAt 为当前时间，避免同一周期重复触发
			d.UpdatedAt = now
			// 持久化新的 updatedAt，避免进程重启后重复触发
			if err := s.store.SaveDAG(ctx, d); err != nil {
				s.logError(ctx, fmt.Sprintf("[DAG] save dag failed: id=%s", d.ID), err)
			}
		}
	}
	// 推进运行中 DAG：先拷贝一份引用后释放锁，再逐个 dispatchReady，避免长持锁
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
//
// 依赖检测规则：
//   - 依赖任务状态均为 completed 才视为就绪。
//   - 任一依赖未完成或缺失则保持 pending。
//
// 派发动作：调用 LaunchSession(goal) 获得 sessionID，写入 task 并置 Running。
func (s *Scheduler) dispatchReady(ctx context.Context, d *DAG) {
	// 建立 ID -> Task 索引，便于 O(1) 查询依赖状态
	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		byID[t.ID] = t
	}
	// 遍历所有 task，找出可派发的 pending 任务
	for _, t := range d.Tasks {
		// 已派发或已完成的不再处理
		if t.Status != TaskStatusPending {
			continue
		}
		ready := true
		// 逐个检查依赖是否都已完成
		for _, dep := range t.DependsOn {
			if depT, ok := byID[dep]; ok {
				if depT.Status != TaskStatusCompleted {
					// 任一依赖未完成，保持 pending
					ready = false
					break
				}
			} else {
				// 依赖指向不存在的任务，保守不派发
				ready = false
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
//
// 调用时机：由 SessionManager 在 session 结束时调用，触发后续依赖任务。
//
// 注意：参数 taskID 当前未使用，匹配靠 sessionID 唯一定位 task。
//
// 参数：
//   - dagID：DAG 唯一标识。
//   - taskID：任务 ID（保留字段，当前未使用）。
//   - sessionID：由 LaunchSession 返回的 session ID。
//   - success：session 是否成功结束；false 表示失败。
func (s *Scheduler) MarkCompleted(dagID, taskID, sessionID string, success bool) {
	// 在 running map 中查找对应 DAG
	s.mu.Lock()
	d, ok := s.running[dagID]
	s.mu.Unlock()
	if !ok {
		// DAG 已被清理或不在运行中：忽略回调，避免空指针
		return
	}
	// 在 DAG 的任务列表中按 sessionID 找到对应 task
	for _, t := range d.Tasks {
		if t.SessionID == sessionID {
			// 记录完成时间
			now := time.Now()
			t.FinishedAt = &now
			// 根据 success 设置终态
			if success {
				t.Status = TaskStatusCompleted
			} else {
				// 失败标记让下游依赖永远不会 ready
				t.Status = TaskStatusFailed
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
//
// 返回：每个 DAG 都是深拷贝后的独立实例，外部修改不会影响调度器内部状态。
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

// cloneTask 返回 Task 的深拷贝。
//
// 说明：nil 切片保持 nil，空切片保持空，指针字段独立分配内存，
// 确保修改副本不会影响原始对象。
//
// 参数：t 待拷贝的 Task 指针；nil 输入返回 nil。
//
// 返回：深拷贝后的 Task 指针。
func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	// 拷贝标量字段
	cp := &Task{
		ID:        t.ID,
		Goal:      t.Goal,
		Status:    t.Status,
		SessionID: t.SessionID,
	}
	// 深拷贝 DependsOn 切片
	if t.DependsOn != nil {
		cp.DependsOn = make([]string, len(t.DependsOn))
		copy(cp.DependsOn, t.DependsOn)
	}
	// 深拷贝 StartedAt 指针
	if t.StartedAt != nil {
		v := *t.StartedAt
		cp.StartedAt = &v
	}
	// 深拷贝 FinishedAt 指针
	if t.FinishedAt != nil {
		v := *t.FinishedAt
		cp.FinishedAt = &v
	}
	return cp
}

// cloneDAG 返回 DAG 的深拷贝，包括每个 Task 的深拷贝。
//
// 参数：d 待拷贝的 DAG 指针；nil 输入返回 nil。
//
// 返回：深拷贝后的 DAG 指针。
func cloneDAG(d *DAG) *DAG {
	if d == nil {
		return nil
	}
	// 拷贝标量字段
	cp := &DAG{
		ID:        d.ID,
		Name:      d.Name,
		Cron:      d.Cron,
		Enabled:   d.Enabled,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
	// 深拷贝 Tasks 切片及其中每个 Task
	if d.Tasks != nil {
		cp.Tasks = make([]*Task, len(d.Tasks))
		for i, t := range d.Tasks {
			cp.Tasks[i] = cloneTask(t)
		}
	}
	return cp
}
