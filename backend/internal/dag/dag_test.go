package dag

import (
	"context" // 测试用上下文
	"strconv" // 伪 sessionID 生成
	"testing" // Go 测试框架
	"time"    // 时间构造与比较
)

// TestCloneTaskMatchesJSONDeepCopy 验证 cloneTask 产生的语义与之前
// JSON 序列化/反序列化深拷贝一致，同时显式保证 nil 切片与空切片被原样保留。
//
// 覆盖场景：
//   - depends_on 为 nil
//   - depends_on 为空切片
//   - depends_on 含多个依赖且时间指针非空
func TestCloneTaskMatchesJSONDeepCopy(t *testing.T) {
	// 构造固定的 UTC 时间，避免时区导致比较失败
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	cases := []struct {
		name string // 用例名称
		orig *Task  // 原始 task
	}{
		{
			name: "nil depends_on",
			orig: &Task{
				ID:        "t1",
				Goal:      "goal one",
				DependsOn: nil,
				Status:    TaskStatusPending,
			},
		},
		{
			name: "empty depends_on",
			orig: &Task{
				ID:        "t2",
				Goal:      "goal two",
				DependsOn: []string{},
				Status:    TaskStatusRunning,
			},
		},
		{
			name: "non-empty depends_on",
			orig: &Task{
				ID:         "t3",
				Goal:       "goal three",
				DependsOn:  []string{"t1", "t2"},
				Status:     TaskStatusCompleted,
				SessionID:  "sess-123",
				StartedAt:  &now,
				FinishedAt: &later,
			},
		},
	}

	// 遍历所有测试用例
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := cloneTask(tc.orig)

			// 深拷贝必须返回不同指针
			if cp == tc.orig {
				t.Fatal("cloneTask returned the same pointer")
			}
			// 标量字段必须保持一致
			if cp.ID != tc.orig.ID {
				t.Errorf("ID mismatch: got %q, want %q", cp.ID, tc.orig.ID)
			}
			if cp.Goal != tc.orig.Goal {
				t.Errorf("Goal mismatch: got %q, want %q", cp.Goal, tc.orig.Goal)
			}
			if cp.Status != tc.orig.Status {
				t.Errorf("Status mismatch: got %q, want %q", cp.Status, tc.orig.Status)
			}
			if cp.SessionID != tc.orig.SessionID {
				t.Errorf("SessionID mismatch: got %q, want %q", cp.SessionID, tc.orig.SessionID)
			}

			// nil 与空切片的 nil-ness 必须原样保留
			if (cp.DependsOn == nil) != (tc.orig.DependsOn == nil) {
				t.Errorf("DependsOn nil-ness changed: got nil=%v, want nil=%v", cp.DependsOn == nil, tc.orig.DependsOn == nil)
			}
			if len(cp.DependsOn) != len(tc.orig.DependsOn) {
				t.Fatalf("DependsOn length mismatch: got %d, want %d", len(cp.DependsOn), len(tc.orig.DependsOn))
			}
			// 逐个元素比较依赖 ID
			for i := range tc.orig.DependsOn {
				if cp.DependsOn[i] != tc.orig.DependsOn[i] {
					t.Errorf("DependsOn[%d] mismatch: got %q, want %q", i, cp.DependsOn[i], tc.orig.DependsOn[i])
				}
			}

			// 指针字段：值相等但指针地址不同
			if (cp.StartedAt == nil) != (tc.orig.StartedAt == nil) {
				t.Errorf("StartedAt nil-ness changed")
			}
			if tc.orig.StartedAt != nil {
				if cp.StartedAt == tc.orig.StartedAt {
					t.Error("StartedAt pointer was not deep copied")
				}
				if !cp.StartedAt.Equal(*tc.orig.StartedAt) {
					t.Errorf("StartedAt value mismatch: got %v, want %v", *cp.StartedAt, *tc.orig.StartedAt)
				}
			}
			if (cp.FinishedAt == nil) != (tc.orig.FinishedAt == nil) {
				t.Errorf("FinishedAt nil-ness changed")
			}
			if tc.orig.FinishedAt != nil {
				if cp.FinishedAt == tc.orig.FinishedAt {
					t.Error("FinishedAt pointer was not deep copied")
				}
				if !cp.FinishedAt.Equal(*tc.orig.FinishedAt) {
					t.Errorf("FinishedAt value mismatch: got %v, want %v", *cp.FinishedAt, *tc.orig.FinishedAt)
				}
			}
		})
	}
}

// TestCloneDAGMatchesJSONDeepCopy 验证 cloneDAG 语义与之前 JSON 往返行为一致。
//
// 校验点：标量字段不变、时间字段相等、每个 Task 都是独立深拷贝。
func TestCloneDAGMatchesJSONDeepCopy(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(2 * time.Hour)

	// 构造包含多种 depends_on 情况的测试 DAG
	orig := &DAG{
		ID:        "dag-1",
		Name:      "test dag",
		Cron:      "30s",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: later,
		Tasks: []*Task{
			{ID: "a", Goal: "do a", DependsOn: nil},
			{ID: "b", Goal: "do b", DependsOn: []string{}},
			{ID: "c", Goal: "do c", DependsOn: []string{"a", "b"}},
		},
	}

	cp := cloneDAG(orig)

	// 必须返回不同指针
	if cp == orig {
		t.Fatal("cloneDAG returned the same pointer")
	}
	// 标量字段必须一致
	if cp.ID != orig.ID || cp.Name != orig.Name || cp.Cron != orig.Cron || cp.Enabled != orig.Enabled {
		t.Errorf("scalar fields changed: got %+v, want %+v", cp, orig)
	}
	// 时间字段必须相等
	if !cp.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("CreatedAt mismatch: got %v, want %v", cp.CreatedAt, orig.CreatedAt)
	}
	if !cp.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Errorf("UpdatedAt mismatch: got %v, want %v", cp.UpdatedAt, orig.UpdatedAt)
	}
	// Task 数量一致
	if len(cp.Tasks) != len(orig.Tasks) {
		t.Fatalf("Tasks length mismatch: got %d, want %d", len(cp.Tasks), len(orig.Tasks))
	}
	// 每个 Task 都是独立深拷贝
	for i, want := range orig.Tasks {
		got := cp.Tasks[i]
		if got == want {
			t.Fatalf("Tasks[%d] is the same pointer", i)
		}
		if got.ID != want.ID || got.Goal != want.Goal {
			t.Errorf("Tasks[%d] scalar fields mismatch: got %+v, want %+v", i, got, want)
		}
		if (got.DependsOn == nil) != (want.DependsOn == nil) {
			t.Errorf("Tasks[%d].DependsOn nil-ness changed: got nil=%v, want nil=%v", i, got.DependsOn == nil, want.DependsOn == nil)
		}
	}
}

// TestCloneDAGIndependence 确保修改克隆后的 DAG 不会影响原始 DAG。
func TestCloneDAGIndependence(t *testing.T) {
	orig := &DAG{
		ID: "dag-2",
		Tasks: []*Task{
			{ID: "a", Goal: "do a", DependsOn: []string{"b"}},
			{ID: "b", Goal: "do b"},
		},
	}

	cp := cloneDAG(orig)

	// 修改副本：变更 Goal、追加依赖、设置时间指针
	cp.Tasks[0].Goal = "mutated"
	cp.Tasks[0].DependsOn = append(cp.Tasks[0].DependsOn, "c")
	cp.Tasks[1].StartedAt = &time.Time{}
	if len(cp.Tasks) > 0 && cp.Tasks[0].StartedAt != nil {
		cp.Tasks[0].StartedAt = &time.Time{}
	}

	// 验证原始对象未被污染
	if orig.Tasks[0].Goal != "do a" {
		t.Errorf("original Goal mutated: got %q", orig.Tasks[0].Goal)
	}
	if len(orig.Tasks[0].DependsOn) != 1 || orig.Tasks[0].DependsOn[0] != "b" {
		t.Errorf("original DependsOn mutated: got %v", orig.Tasks[0].DependsOn)
	}
	if orig.Tasks[1].StartedAt != nil {
		t.Error("original StartedAt mutated")
	}
}

// TestTopoSortDeterminism 验证 TopoSort 按 ID 排序就绪节点，输出结果稳定。
func TestTopoSortDeterminism(t *testing.T) {
	d := &DAG{
		ID: "dag",
		Tasks: []*Task{
			{ID: "z", Goal: "z", DependsOn: []string{"m"}},
			{ID: "a", Goal: "a"},
			{ID: "m", Goal: "m", DependsOn: []string{"a"}},
		},
	}

	got, err := d.TopoSort()
	if err != nil {
		t.Fatalf("TopoSort failed: %v", err)
	}
	// 提取排序后的 ID 列表
	ids := make([]string, len(got))
	for i, tsk := range got {
		ids[i] = tsk.ID
	}
	// 期望顺序：a -> m -> z
	want := []string{"a", "m", "z"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("TopoSort order mismatch at %d: got %v, want %v", i, ids, want)
		}
	}
}

// TestTopoSortErrors 检查重复 ID、缺失依赖与环三种错误场景。
func TestTopoSortErrors(t *testing.T) {
	t.Run("duplicate id", func(t *testing.T) {
		// 两个 task 使用相同 ID，应报错
		d := &DAG{Tasks: []*Task{{ID: "a"}, {ID: "a"}}}
		if _, err := d.TopoSort(); err == nil {
			t.Error("expected error for duplicate task id")
		}
	})
	t.Run("missing dependency", func(t *testing.T) {
		// 依赖指向不存在的 task，应报错
		d := &DAG{Tasks: []*Task{{ID: "a", DependsOn: []string{"missing"}}}}
		if _, err := d.TopoSort(); err == nil {
			t.Error("expected error for missing dependency")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		// a <-> b 形成环，应报错且 HasCycle 返回 true
		d := &DAG{Tasks: []*Task{
			{ID: "a", DependsOn: []string{"b"}},
			{ID: "b", DependsOn: []string{"a"}},
		}}
		if _, err := d.TopoSort(); err == nil {
			t.Error("expected error for cycle")
		}
		if !d.HasCycle() {
			t.Error("HasCycle returned false for cyclic DAG")
		}
	})
}

// TestDAGRunnerDispatchesReadyTasks 验证 DAGRunner 会克隆 DAG、拓扑排序，
// 并仅派发依赖已满足的任务。
func TestDAGRunnerDispatchesReadyTasks(t *testing.T) {
	// 记录被派发的 goal
	launches := []string{}
	rl := &recordingLauncher{launched: &launches}

	// 构造 DAG：a 无依赖，b/c 都依赖 a；本次 runner 只应派发 a
	d := &DAG{
		ID: "runner-dag",
		Tasks: []*Task{
			{ID: "a", Goal: "goal a"},
			{ID: "b", Goal: "goal b", DependsOn: []string{"a"}},
			{ID: "c", Goal: "goal c", DependsOn: []string{"a"}},
		},
	}

	runner := NewDAGRunner(d, rl)
	if err := runner.Run(t.Context()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 只有 task a 被派发
	if len(launches) != 1 || launches[0] != "goal a" {
		t.Errorf("expected only task a to be launched, got %v", launches)
	}
}

// TestDAGRunnerReturnsTopoError 确保 Run 对含环 DAG 返回错误。
func TestDAGRunnerReturnsTopoError(t *testing.T) {
	d := &DAG{
		ID: "runner-cycle",
		Tasks: []*Task{
			{ID: "a", DependsOn: []string{"b"}},
			{ID: "b", DependsOn: []string{"a"}},
		},
	}
	runner := NewDAGRunner(d, &recordingLauncher{})
	if err := runner.Run(t.Context()); err == nil {
		t.Error("expected error for cyclic DAG")
	}
}

// recordingLauncher 是一个测试用的 SessionLauncher 实现，
// 将被派发的 goal 追加到 launched 切片中。
type recordingLauncher struct {
	launched *[]string // 记录所有 LaunchSession 收到的 goal
}

// LaunchSession 记录 goal 并返回 goal + "-session" 作为伪 sessionID。
func (r *recordingLauncher) LaunchSession(goal string) string {
	if r.launched != nil {
		*r.launched = append(*r.launched, goal)
	}
	return goal + "-session"
}

// GetSessionStatus 实现 SessionLauncher 接口；runner 用不到，恒返回未找到。
func (r *recordingLauncher) GetSessionStatus(string) (string, bool) { return "", false }

// TestSchedulerStopWaitsForLoop 验证 Stop 会等待 loop goroutine 退出，
// 且可多次调用而不会 panic 或永久阻塞。
func TestSchedulerStopWaitsForLoop(t *testing.T) {
	s := NewScheduler(&fakeStore{}, &fakeLauncher{}, 0)
	s.Start(t.Context())

	// 给 loop 一点启动时间
	time.Sleep(10 * time.Millisecond)

	s.Stop()

	// WaitGroup 应该已经归零；第二次 Stop 不能永久阻塞，也不能重复关闭 channel panic
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Stop()
	}()

	select {
	case <-done:
		// 符合预期
	case <-time.After(time.Second):
		t.Fatal("second Stop blocked")
	}
}

// fakeStore 是 Store 接口的测试桩，所有方法返回 nil 错误。
type fakeStore struct{}

func (fakeStore) SaveDAG(context.Context, *DAG) error          { return nil }
func (fakeStore) GetDAG(context.Context, string) (*DAG, error) { return nil, nil }
func (fakeStore) ListDAGs(context.Context) ([]*DAG, error)     { return nil, nil }
func (fakeStore) DeleteDAG(context.Context, string) error      { return nil }

// fakeLauncher 是 SessionLauncher 接口的测试桩，总是返回空字符串。
type fakeLauncher struct{}

func (fakeLauncher) LaunchSession(string) string { return "" }

// GetSessionStatus 实现 SessionLauncher 接口；恒返回未找到。
func (fakeLauncher) GetSessionStatus(string) (string, bool) { return "", false }

// ==================== Scheduler reap / cron 测试 ====================

// memStore 是基于内存的 Store 实现，支持列出、读取与回写 DAG，
// 供调度器集成路径（Trigger / tick / reapRunning）测试使用。
type memStore struct {
	dags  map[string]*DAG // id -> DAG 定义（含运行态）
	saves int             // SaveDAG 调用次数
}

// newMemStore 创建内存存储并预置给定 DAG。
func newMemStore(dags ...*DAG) *memStore {
	m := &memStore{dags: make(map[string]*DAG)}
	for _, d := range dags {
		m.dags[d.ID] = d
	}
	return m
}

// SaveDAG 回写 DAG 深拷贝并计数。
func (m *memStore) SaveDAG(_ context.Context, d *DAG) error {
	m.dags[d.ID] = cloneDAG(d)
	m.saves++
	return nil
}

// GetDAG 按 ID 返回 DAG 深拷贝；不存在返回 nil。
func (m *memStore) GetDAG(_ context.Context, id string) (*DAG, error) {
	if d, ok := m.dags[id]; ok {
		return cloneDAG(d), nil
	}
	return nil, nil
}

// ListDAGs 返回全部 DAG 深拷贝。
func (m *memStore) ListDAGs(context.Context) ([]*DAG, error) {
	out := make([]*DAG, 0, len(m.dags))
	for _, d := range m.dags {
		out = append(out, cloneDAG(d))
	}
	return out, nil
}

// DeleteDAG 从内存中删除 DAG。
func (m *memStore) DeleteDAG(_ context.Context, id string) error {
	delete(m.dags, id)
	return nil
}

// stubLauncher 是可控的 SessionLauncher 实现：
// LaunchSession 生成递增 sessionID 并初始置 running；
// 会话状态可通过 setStatus 外部操控，模拟会话终态。
type stubLauncher struct {
	nextID   int               // 递增 sessionID 计数器
	statuses map[string]string // sessionID -> 会话状态
	goals    []string          // 已派发 goal 列表（按派发顺序）
	sessions map[string]string // sessionID -> goal
	fail     bool              // true 时 LaunchSession 返回空串（模拟派发失败）
}

func newStubLauncher() *stubLauncher {
	return &stubLauncher{statuses: map[string]string{}, sessions: map[string]string{}}
}

// LaunchSession 生成伪 sessionID；fail 时返回空串模拟创建失败。
func (l *stubLauncher) LaunchSession(goal string) string {
	if l.fail {
		return ""
	}
	l.nextID++
	sid := "sess-" + strconv.Itoa(l.nextID)
	l.statuses[sid] = "running"
	l.goals = append(l.goals, goal)
	l.sessions[sid] = goal
	return sid
}

// GetSessionStatus 返回可控的会话状态。
func (l *stubLauncher) GetSessionStatus(sid string) (string, bool) {
	st, ok := l.statuses[sid]
	return st, ok
}

// setStatus 修改指定会话的状态（如 completed / error）。
func (l *stubLauncher) setStatus(sid, status string) { l.statuses[sid] = status }

// sessionOf 按 goal 反查其 sessionID。
func (l *stubLauncher) sessionOf(goal string) string {
	for sid, g := range l.sessions {
		if g == goal {
			return sid
		}
	}
	return ""
}

// TestSchedulerReapAdvancesDependencyChain 验证 A→B 依赖链：
// A 会话 completed 后，reap 置 A 完成并派发 B；B 完成后整个 DAG 从 running map 清除。
func TestSchedulerReapAdvancesDependencyChain(t *testing.T) {
	store := newMemStore(&DAG{
		ID:   "chain",
		Name: "chain",
		// 禁用 cron，避免 tick 重新触发干扰断言
		Enabled: false,
		Tasks: []*Task{
			{ID: "a", Goal: "goal a"},
			{ID: "b", Goal: "goal b", DependsOn: []string{"a"}},
		},
	})
	launcher := newStubLauncher()
	s := NewScheduler(store, launcher, time.Second)

	ctx := t.Context()
	if err := s.Trigger(ctx, "chain"); err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}
	// 初始只派发 a
	if len(launcher.goals) != 1 || launcher.goals[0] != "goal a" {
		t.Fatalf("expected only goal a launched, got %v", launcher.goals)
	}

	// a 会话完成，tick 内 reap 应推进 b
	launcher.setStatus(launcher.sessionOf("goal a"), "completed")
	s.tick(ctx)

	snap := s.Snapshot()
	d, ok := snap["chain"]
	if !ok {
		t.Fatal("dag missing from running map after first reap")
	}
	var a, b *Task
	for _, task := range d.Tasks {
		switch task.ID {
		case "a":
			a = task
		case "b":
			b = task
		}
	}
	if a.Status != TaskStatusCompleted {
		t.Errorf("task a status = %q, want completed", a.Status)
	}
	if b.Status != TaskStatusRunning || b.SessionID == "" {
		t.Errorf("task b status = %q session=%q, want running with session", b.Status, b.SessionID)
	}
	if len(launcher.goals) != 2 || launcher.goals[1] != "goal b" {
		t.Errorf("expected goal b launched, got %v", launcher.goals)
	}
	// 状态迁移应已回写存储
	persisted, _ := store.GetDAG(ctx, "chain")
	var pa *Task
	for _, task := range persisted.Tasks {
		if task.ID == "a" {
			pa = task
		}
	}
	if pa.Status != TaskStatusCompleted {
		t.Errorf("persisted task a status = %q, want completed", pa.Status)
	}

	// b 会话完成，整个 DAG 到达终态，应从 running map 删除
	launcher.setStatus(launcher.sessionOf("goal b"), "completed")
	s.tick(ctx)
	if _, ok := s.Snapshot()["chain"]; ok {
		t.Error("finished dag should be removed from running map")
	}
}

// TestSchedulerReapFailureBlocksDownstream 验证 A 会话 error 后，
// A 置 Failed，下游 B 永久保持 pending，不再派发。
func TestSchedulerReapFailureBlocksDownstream(t *testing.T) {
	store := newMemStore(&DAG{
		ID:   "fail-chain",
		Name: "fail-chain",
		Tasks: []*Task{
			{ID: "a", Goal: "goal a"},
			{ID: "b", Goal: "goal b", DependsOn: []string{"a"}},
		},
	})
	launcher := newStubLauncher()
	s := NewScheduler(store, launcher, time.Second)

	ctx := t.Context()
	if err := s.Trigger(ctx, "fail-chain"); err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}
	launcher.setStatus(launcher.sessionOf("goal a"), "error")
	s.tick(ctx)
	// 再来一轮，确认 b 不会被派发
	s.tick(ctx)

	snap := s.Snapshot()
	d, ok := snap["fail-chain"]
	if !ok {
		t.Fatal("dag with pending downstream should stay in running map")
	}
	var a, b *Task
	for _, task := range d.Tasks {
		switch task.ID {
		case "a":
			a = task
		case "b":
			b = task
		}
	}
	if a.Status != TaskStatusFailed {
		t.Errorf("task a status = %q, want failed", a.Status)
	}
	if b.Status != TaskStatusPending {
		t.Errorf("task b status = %q, want pending (blocked by failed dep)", b.Status)
	}
	if len(launcher.goals) != 1 {
		t.Errorf("downstream must not be launched, got %v", launcher.goals)
	}
}

// TestSchedulerReapRemovesFinishedDAG 验证单任务 DAG 完成后从 running map 清除。
func TestSchedulerReapRemovesFinishedDAG(t *testing.T) {
	store := newMemStore(&DAG{
		ID:    "single",
		Name:  "single",
		Tasks: []*Task{{ID: "a", Goal: "goal a"}},
	})
	launcher := newStubLauncher()
	s := NewScheduler(store, launcher, time.Second)

	ctx := t.Context()
	if err := s.Trigger(ctx, "single"); err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}
	if _, ok := s.Snapshot()["single"]; !ok {
		t.Fatal("dag should be in running map after trigger")
	}
	launcher.setStatus(launcher.sessionOf("goal a"), "completed")
	s.tick(ctx)
	if _, ok := s.Snapshot()["single"]; ok {
		t.Error("finished dag should be removed from running map")
	}
}

// TestDispatchEmptySessionIDMarksFailed 验证 LaunchSession 返回空串时
// task 直接标 Failed 并回写存储。
func TestDispatchEmptySessionIDMarksFailed(t *testing.T) {
	store := newMemStore(&DAG{
		ID:    "no-sid",
		Name:  "no-sid",
		Tasks: []*Task{{ID: "a", Goal: "goal a"}},
	})
	launcher := newStubLauncher()
	launcher.fail = true
	s := NewScheduler(store, launcher, time.Second)

	ctx := t.Context()
	if err := s.Trigger(ctx, "no-sid"); err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}
	snap := s.Snapshot()
	d := snap["no-sid"]
	if d == nil || len(d.Tasks) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if d.Tasks[0].Status != TaskStatusFailed {
		t.Errorf("task status = %q, want failed on empty session id", d.Tasks[0].Status)
	}
	// 应已回写存储
	persisted, _ := store.GetDAG(ctx, "no-sid")
	if persisted.Tasks[0].Status != TaskStatusFailed {
		t.Errorf("persisted task status = %q, want failed", persisted.Tasks[0].Status)
	}
}

// TestParseCronStandardAndLegacy 验证标准 5 段 cron 与旧 Ns/Nm/Nh 间隔解析。
func TestParseCronStandardAndLegacy(t *testing.T) {
	// 标准 cron 表达式
	sched, err := ParseCron("0 9 * * *")
	if err != nil || sched == nil {
		t.Fatalf("ParseCron standard expr failed: %v", err)
	}
	// 空串：手动触发，不报错
	sched, err = ParseCron("")
	if err != nil || sched != nil {
		t.Errorf("ParseCron empty = (%v, %v), want (nil, nil)", sched, err)
	}
	// 非法表达式：4 段、非数字、越界值均应报错
	for _, bad := range []string{"* * * *", "not a cron", "61 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) expected error", bad)
		}
	}
	// 旧间隔格式保持原有语义
	for expr, want := range map[string]time.Duration{
		"30s": 30 * time.Second,
		"15m": 15 * time.Minute,
		"24h": 24 * time.Hour,
	} {
		got, err := ParseInterval(expr)
		if err != nil || got != want {
			t.Errorf("ParseInterval(%q) = (%v, %v), want (%v, nil)", expr, got, err, want)
		}
	}
	if got, err := ParseInterval(""); err != nil || got != 0 {
		t.Errorf("ParseInterval(\"\") = (%v, %v), want (0, nil)", got, err)
	}
	// 旧格式非法输入
	for _, bad := range []string{"0s", "xm", "5d"} {
		if _, err := ParseInterval(bad); err == nil {
			t.Errorf("ParseInterval(%q) expected error", bad)
		}
	}
	// 格式识别：旧间隔 vs 标准 cron
	if !isLegacyInterval("30m") {
		t.Error("isLegacyInterval(\"30m\") = false, want true")
	}
	if isLegacyInterval("0 9 * * *") {
		t.Error("isLegacyInterval(\"0 9 * * *\") = true, want false")
	}
}

// TestTickCronTriggerSemantics 验证 tick 的 cron 触发判定：
// 标准 cron 到点触发、旧间隔到期触发、未到点/非法表达式不触发。
func TestTickCronTriggerSemantics(t *testing.T) {
	now := time.Now()
	mk := func(id, cronExpr string, updatedAt time.Time) *DAG {
		return &DAG{
			ID:        id,
			Name:      id,
			Cron:      cronExpr,
			Enabled:   true,
			UpdatedAt: updatedAt,
			Tasks:     []*Task{{ID: "a", Goal: "goal " + id}},
		}
	}
	store := newMemStore(
		mk("cron-due", "* * * * *", now.Add(-2*time.Hour)), // 每分钟触发，早已到点
		mk("cron-not-due", "0 9 * * *", now),               // 下次触发在未来
		mk("legacy-due", "30m", now.Add(-time.Hour)),       // 旧间隔已到期
		mk("legacy-not-due", "30m", now),                   // 旧间隔未到期
		mk("bad-cron", "not a cron", now.Add(-time.Hour)),  // 非法表达式
	)
	launcher := newStubLauncher()
	s := NewScheduler(store, launcher, time.Second)
	s.tick(t.Context())

	launched := map[string]bool{}
	for _, g := range launcher.goals {
		launched[g] = true
	}
	for id, want := range map[string]bool{
		"cron-due":       true,
		"legacy-due":     true,
		"cron-not-due":   false,
		"legacy-not-due": false,
		"bad-cron":       false,
	} {
		if launched["goal "+id] != want {
			t.Errorf("dag %s launched=%v, want %v", id, launched["goal "+id], want)
		}
	}
	// 触发过的 DAG 应回写新的 UpdatedAt，避免同周期重复触发
	d, _ := store.GetDAG(t.Context(), "cron-due")
	if !d.UpdatedAt.After(now.Add(-time.Hour)) {
		t.Errorf("cron-due UpdatedAt not refreshed: %v", d.UpdatedAt)
	}
}
