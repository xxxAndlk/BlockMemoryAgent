package board

// takeover_test.go 测试 TODO #73 看板接力认领：
// TakeoverFrom 迁移留痕 + 同名续建自动翻绿不回归 + Done 条目不迁移。

import (
	"strings"
	"testing"
)

func TestTakeoverFrom_MigratesFailedEntries(t *testing.T) {
	b := NewTaskBoard("t1", "g")
	id1 := b.AddSubTask("搭骨架")
	id2 := b.AddSubTask("写逻辑")
	_ = b.SetPlan("", []PlanTask{
		{ID: id1, Title: "搭骨架", Domain: "domain-2"},
		{ID: id2, Title: "写逻辑", Domain: "domain-2"},
	})
	// 旧 domain 失败（永久红场景）。
	_ = b.MarkFailed(id1, "连读守卫终止")
	_ = b.MarkFailed(id2, "超时")

	// 异名接管：迁移到新 domain 并留痕。
	if n := b.TakeoverFrom("domain-2", "续建"); n != 2 {
		t.Fatalf("expected 2 migrated, got %d", n)
	}
	snap := b.Snapshot()
	for _, task := range snap.Tasks {
		if task.Domain != "续建" {
			t.Fatalf("entry %s not migrated, domain=%s", task.ID, task.Domain)
		}
		if task.Status != TaskPending {
			t.Fatalf("migrated entry should be pending, got %s", task.Status)
		}
		if !strings.Contains(task.Title, "曾失败") || !strings.Contains(task.Title, "续建") {
			t.Fatalf("title should carry takeover note, got %q", task.Title)
		}
	}
	// 新 domain 完成整组翻绿（boardAssign/boardUpdate 联动）。
	_ = b.Assign(id1, "sub-1")
	_ = b.MarkDone(id1, "ok")
	_ = b.MarkDone(id2, "ok")
	if b.Snapshot().Status != BoardStatusDone {
		t.Fatalf("board should be DONE after successor completes, got %s", b.Snapshot().Status)
	}
}

func TestTakeoverFrom_SameDomainAndDoneNoop(t *testing.T) {
	b := NewTaskBoard("t1", "g")
	id := b.AddSubTask("任务")
	_ = b.SetPlan("", []PlanTask{{ID: id, Title: "任务", Domain: "d1"}})
	_ = b.MarkDone(id, "done")
	// 同名接管：幂等 0。
	if n := b.TakeoverFrom("d1", "d1"); n != 0 {
		t.Fatalf("same domain should noop, got %d", n)
	}
	// Done 条目不迁移。
	if n := b.TakeoverFrom("d1", "d2"); n != 0 {
		t.Fatalf("done entries should not migrate, got %d", n)
	}
	if b.Snapshot().Tasks[0].Domain != "d1" {
		t.Fatal("done entry domain must stay")
	}
	// 未知旧 domain：0。
	if n := b.TakeoverFrom("ghost", "d2"); n != 0 {
		t.Fatalf("unknown old domain should return 0, got %d", n)
	}
}

func TestBrief_TakeoverHintOnFailed(t *testing.T) {
	b := NewTaskBoard("t1", "g")
	id := b.AddSubTask("任务")
	_ = b.SetPlan("", []PlanTask{{ID: id, Title: "任务", Domain: "ui"}})
	_ = b.MarkFailed(id, "x")
	brief := b.Brief(6)
	if !strings.Contains(brief, "接管提示") || !strings.Contains(brief, "ui") {
		t.Fatalf("brief should hint takeover on failed entries: %q", brief)
	}
	// 无失败条目：无提示。
	b2 := NewTaskBoard("t2", "g")
	id2 := b2.AddSubTask("任务")
	_ = b2.SetPlan("", []PlanTask{{ID: id2, Title: "任务", Domain: "ui"}})
	_ = b2.MarkDone(id2, "ok")
	if strings.Contains(b2.Brief(6), "接管提示") {
		t.Fatal("no hint when no failed entries")
	}
}
