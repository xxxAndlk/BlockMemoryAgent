package assembly

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeRunner 记录 ExecuteChild 调用并按 task 文本返回预设。
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	scripts map[string]string // key: task text -> output
	err     error
}

type fakeCall struct {
	RoleID string
	Task   string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{scripts: map[string]string{}}
}

func (f *fakeRunner) ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, fakeCall{RoleID: roleID, Task: task})
	if out, ok := f.scripts[task]; ok {
		return out, nil
	}
	return "", nil
}

// staticSplitter 返回预设 DAG。
type staticSplitter struct {
	d *dag.DAG
}

func (s *staticSplitter) Split(ctx context.Context, goal string) (*dag.DAG, error) {
	return s.d, nil
}

// concatAggregator 拼接所有产出。
type concatAggregator struct{}

func (a *concatAggregator) Aggregate(ctx context.Context, goal string, results map[string]string) (string, error) {
	out := ""
	for k, v := range results {
		out += k + ":" + v + ";"
	}
	return out, nil
}

func TestAssembly_ParallelNoDeps(t *testing.T) {
	d := &dag.DAG{
		Tasks: []*types.Task{
			{ID: "t1", Goal: "do 1"},
			{ID: "t2", Goal: "do 2"},
		},
	}
	r := newFakeRunner()
	r.scripts["do 1"] = "out1"
	r.scripts["do 2"] = "out2"
	a := New(&staticSplitter{d: d}, NewRunnerExecutor(r, "code"), &concatAggregator{})

	out, err := a.Run(context.Background(), "domain-1", "build module")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 ExecuteChild calls, got %d", len(r.calls))
	}
	tasks := map[string]bool{"do 1": false, "do 2": false}
	for _, c := range r.calls {
		tasks[c.Task] = true
	}
	if !tasks["do 1"] || !tasks["do 2"] {
		t.Fatalf("expected both tasks executed, got: %+v", r.calls)
	}
	if !contains(out, "t1:out1") || !contains(out, "t2:out2") {
		t.Fatalf("aggregated output missing results: %s", out)
	}
}

func TestAssembly_DependencyOrder(t *testing.T) {
	// t2 depends on t1；t1 必须先完成。
	d := &dag.DAG{
		Tasks: []*types.Task{
			{ID: "t2", Goal: "do 2", DependsOn: []string{"t1"}},
			{ID: "t1", Goal: "do 1"},
		},
	}
	r := newFakeRunner()
	r.scripts["do 1"] = "out1"
	r.scripts["do 2"] = "out2"
	a := New(&staticSplitter{d: d}, NewRunnerExecutor(r, "code"), &concatAggregator{})

	_, err := a.Run(context.Background(), "domain-1", "build module")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(r.calls))
	}
	// t1 应先执行（调用索引 0）。
	if r.calls[0].Task != "do 1" {
		t.Fatalf("expected t1 first, got task: %s", r.calls[0].Task)
	}
	if r.calls[1].Task != "do 2" {
		t.Fatalf("expected t2 second, got task: %s", r.calls[1].Task)
	}
}

func TestAssembly_ExecutorError(t *testing.T) {
	d := &dag.DAG{
		Tasks: []*types.Task{{ID: "t1", Goal: "do 1"}},
	}
	r := newFakeRunner()
	r.err = errors.New("provider down")
	a := New(&staticSplitter{d: d}, NewRunnerExecutor(r, "code"), &concatAggregator{})

	_, err := a.Run(context.Background(), "domain-1", "build module")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "provider down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAssembly_EmptyDAG(t *testing.T) {
	d := &dag.DAG{Tasks: []*types.Task{}}
	r := newFakeRunner()
	a := New(&staticSplitter{d: d}, NewRunnerExecutor(r, "code"), &concatAggregator{})

	out, err := a.Run(context.Background(), "domain-1", "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("expected 0 calls for empty DAG, got %d", len(r.calls))
	}
	if out != "" {
		t.Fatalf("expected empty aggregate, got %q", out)
	}
}

func TestAssembly_CyclicDeps(t *testing.T) {
	// t1 依赖 t2，t2 依赖 t1：死锁。
	d := &dag.DAG{
		Tasks: []*types.Task{
			{ID: "t1", Goal: "do 1", DependsOn: []string{"t2"}},
			{ID: "t2", Goal: "do 2", DependsOn: []string{"t1"}},
		},
	}
	r := newFakeRunner()
	a := New(&staticSplitter{d: d}, NewRunnerExecutor(r, "code"), &concatAggregator{})

	_, err := a.Run(context.Background(), "domain-1", "goal")
	if err == nil {
		t.Fatal("expected error for cyclic deps")
	}
	if !contains(err.Error(), "no runnable tasks") {
		t.Fatalf("unexpected error for cyclic: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
