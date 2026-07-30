package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegisterDefaults(t *testing.T) {
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
	tr.Register(Node{ID: "a"})
	tr.Finish("a", "first", nil)
	tr.Finish("a", "second", nil)
	node, _ := tr.Get("a")
	if node.Summary != "first" {
		t.Errorf("Summary = %q, want 'first' (idempotent)", node.Summary)
	}
}

func TestCancelRunningNode(t *testing.T) {
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
	if tr.Cancel("nope") {
		t.Error("Cancel returned true for unknown node")
	}
}

func TestSetCancelSkipsUnregistered(t *testing.T) {
	tr := NewTree("", nil)
	tr.SetCancel("ghost", func() {})
	// 不应 panic，不应写入 cancels
	tr.Cancel("ghost")
}

func TestSnapshotIsCopy(t *testing.T) {
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
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
	tr := NewTree("", nil)
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

// fakeTreeStore 是测试用的 TreeStore 内存实现,记录 SaveNode 调用并支持 LoadNodes 返回预设数据。
type fakeTreeStore struct {
	saved   map[string]Node // 按 nodeID 索引
	loadErr error
}

func newFakeTreeStore() *fakeTreeStore {
	return &fakeTreeStore{saved: make(map[string]Node)}
}

func (f *fakeTreeStore) SaveNode(_ context.Context, _ string, node Node) error {
	f.saved[node.ID] = node
	return nil
}

func (f *fakeTreeStore) LoadNodes(_ context.Context, _ string) ([]Node, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make([]Node, 0, len(f.saved))
	for _, n := range f.saved {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeTreeStore) DeleteNodesBySession(_ context.Context, _ string) error {
	f.saved = make(map[string]Node)
	return nil
}

// TestTree_PersistOnRegister 验证 Register 后节点写入 store。
func TestTree_PersistOnRegister(t *testing.T) {
	store := newFakeTreeStore()
	tr := NewTree("sess-1", store)
	tr.Register(Node{ID: "n1", Role: "code_assistant", Task: "do X"})

	if _, ok := store.saved["n1"]; !ok {
		t.Fatal("expected node saved to store after Register")
	}
	if store.saved["n1"].Status != StatusRunning {
		t.Errorf("expected saved status running, got %s", store.saved["n1"].Status)
	}
}

// TestTree_PersistOnFinish 验证 Finish 后状态更新写入 store。
func TestTree_PersistOnFinish(t *testing.T) {
	store := newFakeTreeStore()
	tr := NewTree("sess-1", store)
	tr.Register(Node{ID: "n1", Role: "code_assistant"})
	tr.Finish("n1", "done summary", nil)

	if store.saved["n1"].Status != StatusDone {
		t.Errorf("expected saved status done, got %s", store.saved["n1"].Status)
	}
	if store.saved["n1"].Summary != "done summary" {
		t.Errorf("expected saved summary, got %q", store.saved["n1"].Summary)
	}
}

// TestTree_PersistOnCancel 验证 Cancel 后状态更新写入 store。
func TestTree_PersistOnCancel(t *testing.T) {
	store := newFakeTreeStore()
	tr := NewTree("sess-1", store)
	tr.Register(Node{ID: "n1", Role: "code_assistant"})
	tr.SetCancel("n1", func() {})
	tr.Cancel("n1")

	if store.saved["n1"].Status != StatusCancelled {
		t.Errorf("expected saved status cancelled, got %s", store.saved["n1"].Status)
	}
}

// TestTree_LoadFromStore 验证从 store 恢复节点。
func TestTree_LoadFromStore(t *testing.T) {
	store := newFakeTreeStore()
	// 预设一个节点
	store.saved["n1"] = Node{ID: "n1", Role: "code_assistant", Status: StatusDone, Summary: "prior"}

	tr := NewTree("sess-1", store)
	if err := tr.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	node, ok := tr.Get("n1")
	if !ok {
		t.Fatal("expected node loaded from store")
	}
	if node.Summary != "prior" {
		t.Errorf("expected prior summary, got %q", node.Summary)
	}
}

// TestTree_NilStoreNoOp 验证 store 为 nil 时不 panic 且行为正常。
func TestTree_NilStoreNoOp(t *testing.T) {
	tr := NewTree("", nil)
	tr.Register(Node{ID: "n1", Role: "code_assistant"})
	tr.Finish("n1", "done", nil)
	tr.Cancel("n1")
	if err := tr.LoadFromStore(context.Background()); err != nil {
		t.Errorf("expected nil error with nil store, got %v", err)
	}
}

// TestTree_EndCurrentTopic 验证终结话题:取消 Running 节点 + 清内存 + 返回快照 + 删 PG。
func TestTree_EndCurrentTopic(t *testing.T) {
	store := newFakeTreeStore()
	tr := NewTree("sess-1", store)
	tr.Register(Node{ID: "running-1", Role: "code_assistant", Task: "task A"})
	tr.Register(Node{ID: "done-1", Role: "ui_assistant", Task: "task B"})
	tr.Finish("done-1", "done summary", nil)

	cancelCalled := false
	tr.SetCancel("running-1", func() { cancelCalled = true })

	snapshot := tr.EndCurrentTopic()

	// 快照应含两个节点。
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 nodes in snapshot, got %d", len(snapshot))
	}
	// Running 节点应取消且 cancel func 被调用。
	if !cancelCalled {
		t.Error("expected cancel func called for Running node")
	}
	var runningNode *Node
	for i := range snapshot {
		if snapshot[i].ID == "running-1" {
			runningNode = &snapshot[i]
		}
	}
	if runningNode == nil || runningNode.Status != StatusCancelled {
		t.Error("expected running-1 cancelled in snapshot")
	}
	// 内存应清空。
	if nodes := tr.Snapshot(); len(nodes) != 0 {
		t.Errorf("expected tree cleared, got %d nodes", len(nodes))
	}
	// store 应清空(DeleteNodesBySession called)。
	if len(store.saved) != 0 {
		t.Errorf("expected store cleared, got %d nodes", len(store.saved))
	}
}

// TestTree_EndCurrentTopicEmpty 验证空树终结无 panic。
func TestTree_EndCurrentTopicEmpty(t *testing.T) {
	tr := NewTree("sess-1", nil)
	snapshot := tr.EndCurrentTopic()
	if len(snapshot) != 0 {
		t.Errorf("expected empty snapshot, got %d", len(snapshot))
	}
}
