package dag

import (
	"context" // 测试用上下文
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
