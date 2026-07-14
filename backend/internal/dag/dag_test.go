package dag

import (
	"context"
	"testing"
	"time"
)

// TestCloneTaskMatchesJSONDeepCopy verifies that cloneTask produces the same
// semantics as the previous JSON Marshal/Unmarshal deep copy, except that nil
// slices are explicitly preserved (which JSON also does, but we make it
// explicit and deterministic).
func TestCloneTaskMatchesJSONDeepCopy(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	cases := []struct {
		name string
		orig *Task
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

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := cloneTask(tc.orig)

			if cp == tc.orig {
				t.Fatal("cloneTask returned the same pointer")
			}
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

			// nil vs empty slice must be preserved exactly.
			if (cp.DependsOn == nil) != (tc.orig.DependsOn == nil) {
				t.Errorf("DependsOn nil-ness changed: got nil=%v, want nil=%v", cp.DependsOn == nil, tc.orig.DependsOn == nil)
			}
			if len(cp.DependsOn) != len(tc.orig.DependsOn) {
				t.Fatalf("DependsOn length mismatch: got %d, want %d", len(cp.DependsOn), len(tc.orig.DependsOn))
			}
			for i := range tc.orig.DependsOn {
				if cp.DependsOn[i] != tc.orig.DependsOn[i] {
					t.Errorf("DependsOn[%d] mismatch: got %q, want %q", i, cp.DependsOn[i], tc.orig.DependsOn[i])
				}
			}

			// Pointer fields: values equal but distinct pointers.
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

// TestCloneDAGMatchesJSONDeepCopy verifies cloneDAG semantics against the
// previous JSON round-trip behavior.
func TestCloneDAGMatchesJSONDeepCopy(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(2 * time.Hour)

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

	if cp == orig {
		t.Fatal("cloneDAG returned the same pointer")
	}
	if cp.ID != orig.ID || cp.Name != orig.Name || cp.Cron != orig.Cron || cp.Enabled != orig.Enabled {
		t.Errorf("scalar fields changed: got %+v, want %+v", cp, orig)
	}
	if !cp.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("CreatedAt mismatch: got %v, want %v", cp.CreatedAt, orig.CreatedAt)
	}
	if !cp.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Errorf("UpdatedAt mismatch: got %v, want %v", cp.UpdatedAt, orig.UpdatedAt)
	}
	if len(cp.Tasks) != len(orig.Tasks) {
		t.Fatalf("Tasks length mismatch: got %d, want %d", len(cp.Tasks), len(orig.Tasks))
	}
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

// TestCloneDAGIndependence ensures mutations on the clone do not affect the
// original DAG.
func TestCloneDAGIndependence(t *testing.T) {
	orig := &DAG{
		ID: "dag-2",
		Tasks: []*Task{
			{ID: "a", Goal: "do a", DependsOn: []string{"b"}},
			{ID: "b", Goal: "do b"},
		},
	}

	cp := cloneDAG(orig)

	// Mutate clone.
	cp.Tasks[0].Goal = "mutated"
	cp.Tasks[0].DependsOn = append(cp.Tasks[0].DependsOn, "c")
	cp.Tasks[1].StartedAt = &time.Time{}
	if len(cp.Tasks) > 0 && cp.Tasks[0].StartedAt != nil {
		cp.Tasks[0].StartedAt = &time.Time{}
	}

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

// TestTopoSortDeterminism verifies that TopoSort is deterministic by sorting
// ready nodes by ID.
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
	ids := make([]string, len(got))
	for i, tsk := range got {
		ids[i] = tsk.ID
	}
	want := []string{"a", "m", "z"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("TopoSort order mismatch at %d: got %v, want %v", i, ids, want)
		}
	}
}

// TestTopoSortErrors checks duplicate IDs, missing dependencies, and cycles.
func TestTopoSortErrors(t *testing.T) {
	t.Run("duplicate id", func(t *testing.T) {
		d := &DAG{Tasks: []*Task{{ID: "a"}, {ID: "a"}}}
		if _, err := d.TopoSort(); err == nil {
			t.Error("expected error for duplicate task id")
		}
	})
	t.Run("missing dependency", func(t *testing.T) {
		d := &DAG{Tasks: []*Task{{ID: "a", DependsOn: []string{"missing"}}}}
		if _, err := d.TopoSort(); err == nil {
			t.Error("expected error for missing dependency")
		}
	})
	t.Run("cycle", func(t *testing.T) {
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

// TestDAGRunnerDispatchesReadyTasks verifies that DAGRunner clones the DAG,
// topologically sorts it, and launches tasks whose dependencies are satisfied.
func TestDAGRunnerDispatchesReadyTasks(t *testing.T) {
	launches := []string{}
	rl := &recordingLauncher{launched: &launches}

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

	if len(launches) != 1 || launches[0] != "goal a" {
		t.Errorf("expected only task a to be launched, got %v", launches)
	}
}

// TestDAGRunnerReturnsTopoError ensures Run returns an error for cyclic DAGs.
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

type recordingLauncher struct {
	launched *[]string
}

func (r *recordingLauncher) LaunchSession(goal string) string {
	if r.launched != nil {
		*r.launched = append(*r.launched, goal)
	}
	return goal + "-session"
}

// TestSchedulerStopWaitsForLoop ensures Stop waits for the loop goroutine to
// exit and can be called multiple times without panic.
func TestSchedulerStopWaitsForLoop(t *testing.T) {
	s := NewScheduler(&fakeStore{}, &fakeLauncher{}, 0)
	s.Start(t.Context())

	// Give loop a moment to start.
	time.Sleep(10 * time.Millisecond)

	s.Stop()

	// WaitGroup should have been decremented; a second Stop must not block
	// forever and must not panic on closed channel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Stop()
	}()

	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("second Stop blocked")
	}
}

type fakeStore struct{}

func (fakeStore) SaveDAG(context.Context, *DAG) error          { return nil }
func (fakeStore) GetDAG(context.Context, string) (*DAG, error) { return nil, nil }
func (fakeStore) ListDAGs(context.Context) ([]*DAG, error)     { return nil, nil }
func (fakeStore) DeleteDAG(context.Context, string) error      { return nil }

type fakeLauncher struct{}

func (fakeLauncher) LaunchSession(string) string { return "" }
