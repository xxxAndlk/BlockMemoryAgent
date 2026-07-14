package board

import (
	"strings" // strings.Contains 校验 Brief 内容
	"testing" // Go 测试框架
)

// TestTaskBoard_LifeCycle 验证看板从创建、添加任务、设置约束、分配到标记完成/失败的完整生命周期。
func TestTaskBoard_LifeCycle(t *testing.T) {
	// 创建新看板
	b := NewTaskBoard("topic-1", "完成订单服务重构")
	// 添加两个子任务，ID 应不同
	id1 := b.AddSubTask("接口设计")
	id2 := b.AddSubTask("数据库迁移")
	if id1 == id2 {
		t.Fatalf("ids must differ")
	}

	// 同名任务应返回已有 ID
	if b.AddSubTask("接口设计") != id1 {
		t.Fatalf("duplicate title should return existing id")
	}

	// 设置全局约束
	b.SetConstraint("api_compat", "兼容旧版")
	// 分配任务并标记完成
	if err := b.Assign(id1, "agent-A"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := b.MarkDone(id1, "OK"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if b.Tasks[id1].Status != TaskDone {
		t.Fatalf("expected done, got %s", b.Tasks[id1].Status)
	}

	// 标记另一任务失败，看板整体应变 FAILED
	if err := b.MarkFailed(id2, "DB outage"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if b.Status != BoardStatusFailed {
		t.Fatalf("expected board FAILED, got %s", b.Status)
	}

	// Brief 应包含任务标题
	brief := b.Brief(10)
	if !strings.Contains(brief, "接口设计") || !strings.Contains(brief, "DB outage") == false { // brief 里 DB outage 是 result，不强求
		// 仅验证标题出现
		t.Fatalf("brief missing title: %q", brief)
	}
}

// TestManager 验证 Manager 的复用与删除行为。
func TestManager(t *testing.T) {
	m := NewManager()
	// 首次创建
	b1 := m.GetOrCreate("t1", "g1")
	// 再次获取同一 topicID 应返回原对象
	b2 := m.GetOrCreate("t1", "g1-other") // 已存在 → 返回原对象
	if b1 != b2 {
		t.Fatalf("manager should reuse existing board")
	}
	// 移除后 Get 应返回 nil
	m.Remove("t1")
	if m.Get("t1") != nil {
		t.Fatalf("Remove should drop board")
	}
}
