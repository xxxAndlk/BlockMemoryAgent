package board

import (
	"errors"
	"testing"
)

// fakeStore 内存实现 board.Store，记录调用次数。
type fakeStore struct {
	snap    Snapshot
	found   bool
	saveErr error
	saveCnt int
	loadCnt int
}

func (f *fakeStore) LoadBoard(topicID string) (Snapshot, bool, error) {
	f.loadCnt++
	return f.snap, f.found, nil
}

func (f *fakeStore) SaveBoard(snap Snapshot) error {
	f.saveCnt++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.snap = snap
	f.found = true
	return nil
}

// mutator 触发写穿：SetPlan/Assign/MarkDone 后 store 收到最新快照。
func TestBoardPersistWriteThrough(t *testing.T) {
	fs := &fakeStore{}
	m := NewManager().WithStore(fs)
	b := m.GetOrCreate("sess-1", "goal")
	if err := b.SetPlan("goal", []PlanTask{{ID: "t1", Title: "任务一", Domain: "后端"}, {ID: "t2", Title: "任务二", Domain: "测试", DependsOn: []string{"t1"}}}); err != nil {
		t.Fatal(err)
	}
	if fs.saveCnt == 0 {
		t.Fatal("SetPlan 未触发持久化")
	}
	_ = b.Assign("t1", "sess-1/domain-1")
	_ = b.MarkDone("t1", "done")
	if fs.saveCnt < 3 {
		t.Fatalf("Assign/MarkDone 应各触发一次持久化：%d", fs.saveCnt)
	}
	if fs.snap.Tasks[0].Status != TaskDone {
		t.Fatalf("快照应含最新状态：%+v", fs.snap.Tasks[0])
	}
	// 持久化失败不影响内存行为（fail-open）。
	fs.saveErr = errors.New("pg down")
	if err := b.MarkBlocked("t2", "等依赖"); err != nil {
		t.Fatalf("持久化失败不应影响看板操作：%v", err)
	}
	fs.saveErr = nil
}

// 重启恢复：新 Manager（空内存）经 GetOrCreate 从 store 惰性重建，状态/依赖/seq 完整。
func TestBoardRestoreFromStore(t *testing.T) {
	fs := &fakeStore{}
	m1 := NewManager().WithStore(fs)
	b1 := m1.GetOrCreate("sess-1", "goal")
	_ = b1.SetPlan("goal", []PlanTask{{ID: "t1", Title: "任务一", Domain: "后端"}, {ID: "t2", Title: "任务二", Domain: "测试", DependsOn: []string{"t1"}}})
	_ = b1.MarkDone("t1", "ok")
	autoID := b1.AddSubTask("自动编号任务") // 占用 seq

	// 模拟重启：新 Manager，同一 store。
	m2 := NewManager().WithStore(fs)
	b2 := m2.GetOrCreate("sess-1", "ignored")
	if !b2.DependsDone("t2") {
		t.Fatal("t1 已 done，t2 依赖应就绪")
	}
	got := b2.Snapshot()
	if got.Status != BoardStatusInProgress || len(got.Tasks) != 3 {
		t.Fatalf("恢复快照不符：%+v", got)
	}
	// seq 恢复：新自动编号不与恢复的 autoID 冲突。
	newID := b2.AddSubTask("再自动编号")
	if newID == autoID {
		t.Fatalf("seq 未恢复，ID 冲突：%s", newID)
	}
}

// 无 store 时纯内存（旧行为不变）。
func TestManagerWithoutStore(t *testing.T) {
	m := NewManager()
	b := m.GetOrCreate("sess-x", "g")
	_ = b.SetPlan("g", []PlanTask{{ID: "t1", Title: "x", Domain: "d"}})
	if !b.DependsDone("t1") {
		t.Fatal("纯内存行为回归")
	}
}

// mapFakeStore 按 topicID 区分的内存 store（GetRestored 缺失语义测试用：
// fakeStore 对任意 topic 都返回同一快照，无法验证"未知 topic 不恢复"）。
type mapFakeStore struct {
	snaps   map[string]Snapshot
	loadErr error
}

func (f *mapFakeStore) LoadBoard(topicID string) (Snapshot, bool, error) {
	if f.loadErr != nil {
		return Snapshot{}, false, f.loadErr
	}
	s, ok := f.snaps[topicID]
	return s, ok, nil
}

func (f *mapFakeStore) SaveBoard(snap Snapshot) error {
	if f.snaps == nil {
		f.snaps = make(map[string]Snapshot)
	}
	f.snaps[snap.TopicID] = snap
	return nil
}

// GetRestored：内存 miss 时从 store 惰性恢复（依赖状态/seq 完整、恢复即写穿），
// 但不新建——未知 topic / 读取失败返回 nil；Get 仍不恢复（行为不变）。
func TestManagerGetRestored(t *testing.T) {
	fs := &mapFakeStore{}
	m1 := NewManager().WithStore(fs)
	b1 := m1.GetOrCreate("sess-1", "goal")
	_ = b1.SetPlan("goal", []PlanTask{{ID: "t1", Title: "任务一", Domain: "后端"}, {ID: "t2", Title: "任务二", Domain: "测试", DependsOn: []string{"t1"}}})
	_ = b1.MarkDone("t1", "ok")
	autoID := b1.AddSubTask("自动编号任务") // 占用 seq

	// 模拟重启：新 Manager（空内存），同一 store。
	m2 := NewManager().WithStore(fs)

	// Get 不触发恢复（旧行为不变）。
	if got := m2.Get("sess-1"); got != nil {
		t.Fatalf("Get 不应触发恢复，got %+v", got.Snapshot())
	}

	// GetRestored 恢复：依赖状态与 seq 完整。
	b2 := m2.GetRestored("sess-1")
	if b2 == nil {
		t.Fatal("GetRestored 应从 store 恢复看板")
	}
	if !b2.DependsDone("t2") {
		t.Fatal("t1 已 done，恢复后 t2 依赖应就绪")
	}
	if got := b2.Snapshot(); got.Status != BoardStatusInProgress || len(got.Tasks) != 3 {
		t.Fatalf("恢复快照不符：%+v", got)
	}
	if newID := b2.AddSubTask("再自动编号"); newID == autoID {
		t.Fatalf("seq 未恢复，ID 冲突：%s", newID)
	}
	// 恢复即登记内存：后续 Get 命中同一实例。
	if m2.Get("sess-1") != b2 {
		t.Fatal("恢复后 Get 应命中同一实例")
	}
	// 恢复出的看板已注入 persistFn：后续变更写穿 store。
	if _ = b2.MarkDone("t2", "ok"); fs.snaps["sess-1"].Tasks[1].Status != TaskDone {
		t.Fatalf("恢复看板变更应写穿 store：%+v", fs.snaps["sess-1"].Tasks[1])
	}

	// 未知 topic：返回 nil 且不创建。
	if got := m2.GetRestored("sess-unknown"); got != nil {
		t.Fatalf("未知 topic 应返回 nil，got %+v", got.Snapshot())
	}
	if got := m2.Get("sess-unknown"); got != nil {
		t.Fatal("GetRestored 不得创建缺失看板")
	}

	// 读取失败：返回 nil 且不创建（fail-open）。
	fs.loadErr = errors.New("pg down")
	if got := m2.GetRestored("sess-err"); got != nil {
		t.Fatalf("读取失败应返回 nil，got %+v", got.Snapshot())
	}
	if got := m2.Get("sess-err"); got != nil {
		t.Fatal("读取失败不得创建看板")
	}
	fs.loadErr = nil
}
