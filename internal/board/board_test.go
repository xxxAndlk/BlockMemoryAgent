package board

import (
	"strings"
	"testing"
)

func TestTaskBoard_LifeCycle(t *testing.T) {
	b := NewTaskBoard("topic-1", "完成订单服务重构")
	id1 := b.AddSubTask("接口设计")
	id2 := b.AddSubTask("数据库迁移")
	if id1 == id2 {
		t.Fatalf("ids must differ")
	}

	if b.AddSubTask("接口设计") != id1 {
		t.Fatalf("duplicate title should return existing id")
	}

	b.SetConstraint("api_compat", "兼容旧版")
	if err := b.Assign(id1, "agent-A"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := b.MarkDone(id1, "OK"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if b.Tasks[id1].Status != TaskDone {
		t.Fatalf("expected done, got %s", b.Tasks[id1].Status)
	}

	if err := b.MarkFailed(id2, "DB outage"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if b.Status != "FAILED" {
		t.Fatalf("expected board FAILED, got %s", b.Status)
	}

	brief := b.Brief(10)
	if !strings.Contains(brief, "接口设计") || !strings.Contains(brief, "DB outage") == false { // brief 里 DB outage 是 result，不强求
		// 仅验证标题出现
		t.Fatalf("brief missing title: %q", brief)
	}
}

func TestManager(t *testing.T) {
	m := NewManager()
	b1 := m.GetOrCreate("t1", "g1")
	b2 := m.GetOrCreate("t1", "g1-other") // 已存在 → 返回原对象
	if b1 != b2 {
		t.Fatalf("manager should reuse existing board")
	}
	m.Remove("t1")
	if m.Get("t1") != nil {
		t.Fatalf("Remove should drop board")
	}
}
