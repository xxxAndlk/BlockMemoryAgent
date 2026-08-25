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

// TestTaskBoard_MarkUnverified 验证 TODO #60 三态化：MarkUnverified 置 delivered-unverified
// 并记录缺验证原因；全终态无失败但有未验证 → 看板 DELIVERED（非 DONE 非 FAILED）；
// 真失败混入时 FAILED 优先级更高；未知任务报错。
func TestTaskBoard_MarkUnverified(t *testing.T) {
	b := NewTaskBoard("topic-1", "多域交付")
	id1 := b.AddSubTask("引擎")
	id2 := b.AddSubTask("UI")

	if err := b.MarkDone(id1, "OK"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := b.MarkUnverified(id2, "缺验证证据"); err != nil {
		t.Fatalf("mark unverified: %v", err)
	}
	if b.Tasks[id2].Status != TaskUnverified {
		t.Fatalf("expected task delivered-unverified, got %s", b.Tasks[id2].Status)
	}
	if b.Tasks[id2].Result != "缺验证证据" {
		t.Fatalf("expected reason recorded, got %q", b.Tasks[id2].Result)
	}
	if b.Status != BoardStatusDelivered {
		t.Fatalf("expected board DELIVERED, got %s", b.Status)
	}

	// 真失败优先级高于未验证
	if err := b.MarkFailed(id1, "smoke failed"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if b.Status != BoardStatusFailed {
		t.Fatalf("expected board FAILED, got %s", b.Status)
	}

	// 未知任务报错
	if err := b.MarkUnverified("nonexistent", "x"); err == nil {
		t.Fatal("expected error for unknown task")
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

// ---- TODO #22 执行计划扩展测试 ----

// TestSetPlan_Validation 校验：id 空/重复、未知依赖、环依赖均拒绝且不落盘。
func TestSetPlan_Validation(t *testing.T) {
	b := NewTaskBoard("t1", "goal")
	if err := b.SetPlan("g", []PlanTask{{ID: "", Title: "x"}}); err == nil {
		t.Fatal("empty id should be rejected")
	}
	if err := b.SetPlan("g", []PlanTask{
		{ID: "a", Title: "A"},
		{ID: "a", Title: "A2"},
	}); err == nil {
		t.Fatal("duplicate id should be rejected")
	}
	if err := b.SetPlan("g", []PlanTask{
		{ID: "a", Title: "A", DependsOn: []string{"ghost"}},
	}); err == nil {
		t.Fatal("unknown dependency should be rejected")
	}
	if err := b.SetPlan("g", []PlanTask{
		{ID: "a", Title: "A", DependsOn: []string{"b"}},
		{ID: "b", Title: "B", DependsOn: []string{"c"}},
		{ID: "c", Title: "C", DependsOn: []string{"a"}},
	}); err == nil {
		t.Fatal("dependency cycle should be rejected")
	}
	if len(b.Snapshot().Tasks) != 0 {
		t.Fatalf("rejected plans must not be written, got %d tasks", len(b.Snapshot().Tasks))
	}
}

// TestSetPlan_ValidAndMerge 合法计划落盘 + 重规划保留既有状态。
func TestSetPlan_ValidAndMerge(t *testing.T) {
	b := NewTaskBoard("t1", "")
	if err := b.SetPlan("全局目标", []PlanTask{
		{ID: "a", Title: "渲染引擎", Domain: "渲染", Acceptance: []string{"fps>30"}},
		{ID: "b", Title: "配置", Domain: "配置", DependsOn: []string{"a"}},
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	snap := b.Snapshot()
	if snap.Goal != "全局目标" {
		t.Fatalf("goal not set: %s", snap.Goal)
	}
	if len(snap.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(snap.Tasks))
	}
	// 依赖门查询。
	if id, ok := b.FindByDomain("渲染"); !ok || id != "a" {
		t.Fatalf("FindByDomain failed: %q %v", id, ok)
	}
	if !b.DependsDone("a") {
		t.Fatal("a has no deps, DependsDone should be true")
	}
	if b.DependsDone("b") {
		t.Fatal("b depends on a (not done), DependsDone should be false")
	}
	// a 完成 -> b 依赖解除。
	if err := b.MarkDone("a", "ok"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if !b.DependsDone("b") {
		t.Fatal("b DependsDone should be true after a done")
	}
	// 重规划：a 保留 Done，新增 c。
	if err := b.SetPlan("", []PlanTask{
		{ID: "a", Title: "渲染引擎", Domain: "渲染"},
		{ID: "c", Title: "音频", Domain: "音频", DependsOn: []string{"a"}},
	}); err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	snap = b.Snapshot()
	if len(snap.Tasks) != 3 {
		t.Fatalf("re-plan should keep a and add c, got %d tasks", len(snap.Tasks))
	}
	for _, tk := range snap.Tasks {
		if tk.ID == "a" && tk.Status != TaskDone {
			t.Fatalf("re-plan must preserve Done status of existing task, got %v", tk.Status)
		}
	}
}
