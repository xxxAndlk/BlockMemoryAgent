package orchestrator

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegisterDefaults(t *testing.T) {
	tr := NewTree()
	tr.Register(Node{ID: "a", Role: "dev"})
	node, ok := tr.Get("a")
	if !ok {
		t.Fatal("node not found after Register")
	}
	if node.Status != StatusRunning {
		t.Errorf("Status = %s, want running", node.Status)
	}
	if node.Started.IsZero() {
		t.Error("Started not defaulted to now")
	}
}

func TestFinishSuccessSetsDone(t *testing.T) {
	tr := NewTree()
	tr.Register(Node{ID: "a"})
	tr.Finish("a", "done text", nil)
	node, _ := tr.Get("a")
	if node.Status != StatusDone {
		t.Errorf("Status = %s, want done", node.Status)
	}
	if node.Summary != "done text" {
		t.Errorf("Summary = %q, want 'done text'", node.Summary)
	}
	if node.Finished.IsZero() {
		t.Error("Finished not set")
	}
}

func TestFinishErrorSetsFailed(t *testing.T) {
	tr := NewTree()
	tr.Register(Node{ID: "a"})
	tr.Finish("a", "partial", errors.New("boom"))
	node, _ := tr.Get("a")
	if node.Status != StatusFailed {
		t.Errorf("Status = %s, want failed", node.Status)
	}
	if node.Err != "boom" {
		t.Errorf("Err = %q, want 'boom'", node.Err)
	}
}

func TestFinishIdempotent(t *testing.T) {
	tr := NewTree()
	tr.Register(Node{ID: "a"})
	tr.Finish("a", "first", nil)
	tr.Finish("a", "second", nil)
	node, _ := tr.Get("a")
	if node.Summary != "first" {
		t.Errorf("Summary = %q, want 'first' (idempotent)", node.Summary)
	}
}

func TestCancelRunningNode(t *testing.T) {
	tr := NewTree()
	cancelled := false
	cancel := func() { cancelled = true }
	tr.Register(Node{ID: "a"})
	tr.SetCancel("a", cancel)
	if !tr.Cancel("a") {
		t.Error("Cancel returned false for running node")
	}
	if !cancelled {
		t.Error("cancel func not invoked")
	}
	node, _ := tr.Get("a")
	if node.Status != StatusCancelled {
		t.Errorf("Status = %s, want cancelled", node.Status)
	}
}

func TestCancelTerminalNodeNoOp(t *testing.T) {
	tr := NewTree()
	called := false
	tr.Register(Node{ID: "a"})
	tr.SetCancel("a", func() { called = true })
	tr.Finish("a", "done", nil)
	if tr.Cancel("a") {
		t.Error("Cancel returned true for terminal node")
	}
	if called {
		t.Error("cancel func called on terminal node")
	}
}

func TestCancelUnknownNode(t *testing.T) {
	tr := NewTree()
	if tr.Cancel("nope") {
		t.Error("Cancel returned true for unknown node")
	}
}

func TestSetCancelSkipsUnregistered(t *testing.T) {
	tr := NewTree()
	tr.SetCancel("ghost", func() {})
	// 不应 panic，不应写入 cancels
	tr.Cancel("ghost")
}

func TestSnapshotIsCopy(t *testing.T) {
	tr := NewTree()
	tr.Register(Node{ID: "a", Role: "r1"})
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	snap[0].Role = "mutated"
	node, _ := tr.Get("a")
	if node.Role != "r1" {
		t.Error("Snapshot modified internal state")
	}
}

func TestSnapshotSortedByStarted(t *testing.T) {
	tr := NewTree()
	t0 := time.Now()
	tr.Register(Node{ID: "a", Started: t0.Add(2 * time.Second)})
	tr.Register(Node{ID: "b", Started: t0.Add(1 * time.Second)})
	tr.Register(Node{ID: "c", Started: t0.Add(3 * time.Second)})
	snap := tr.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	if snap[0].ID != "b" || snap[1].ID != "a" || snap[2].ID != "c" {
		t.Errorf("order = %s,%s,%s; want b,a,c", snap[0].ID, snap[1].ID, snap[2].ID)
	}
}

func TestConcurrentRegisterFinishCancel(t *testing.T) {
	tr := NewTree()
	const n = 100
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "node-" + itoa(i)
			tr.Register(Node{ID: id})
			tr.SetCancel(id, func() {})
			if i%2 == 0 {
				tr.Finish(id, "done", nil)
			} else {
				tr.Cancel(id)
			}
		}(i)
	}
	wg.Wait()
	snap := tr.Snapshot()
	if len(snap) != n {
		t.Errorf("Snapshot len = %d, want %d", len(snap), n)
	}
}

func TestStatusMarshalJSON(t *testing.T) {
	cases := []struct {
		s    Status
		want string
	}{
		{StatusRunning, `"running"`},
		{StatusDone, `"done"`},
		{StatusFailed, `"failed"`},
		{StatusCancelled, `"cancelled"`},
	}
	for _, c := range cases {
		got, err := c.s.MarshalJSON()
		if err != nil {
			t.Errorf("MarshalJSON(%s) err: %v", c.s, err)
		}
		if string(got) != c.want {
			t.Errorf("MarshalJSON(%s) = %s, want %s", c.s, got, c.want)
		}
	}
}

// itoa 避免引入 strconv 仅为此测试。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
