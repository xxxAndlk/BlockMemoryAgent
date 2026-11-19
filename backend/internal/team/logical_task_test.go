package team

// logical_task_test.go WP1 合同/状态机/继承/幂等回归：
// 这些用例对应计划 §5.5 必测场景，且刻意先断言"旧缺陷可复现"的边界：
//   - 空 WriteSet 不得被解释为"全部允许"；
//   - 用户第 20 轮建立的 30px/60px 不变量在第 21 轮"修省略"时仍 active；
//   - 显式撤销只撤销对应约束，其它不变量保留；
//   - 相同 idempotency_key 重放不产生重复合同；
//   - 压缩/重启（RebuildProjection）后合同 hash 逐字不变；
//   - legacy 会话无合同必须返回 ErrLogicalTaskNotFound，不得伪造 accepted。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func fixtureTargets() []SemanticTarget {
	return []SemanticTarget{
		{ID: "hdr-name", File: "web/src/view/distribution/disOrder/disOrder.vue", Language: "vue",
			Kind: "template_node", Anchor: "dis-order-header-item header-item-name"},
		{ID: "css-name", File: "web/src/view/distribution/disOrder/disOrder.vue", Language: "css",
			Kind: "css_declaration", Anchor: ".header-item-name"},
	}
}

func fixtureRules() []ResourceRule {
	return []ResourceRule{
		{Kind: RuleKindPath, Value: "web/src/view/distribution/disOrder/disOrder.vue", Effect: RuleEffectAllow, Source: RuleSourceUser},
	}
}

func gapInvariants() []Invariant {
	return []Invariant{
		{ID: "gap-name-30", Kind: InvariantKindDOMMetric, Status: InvariantActive, Verification: "dom_metric",
			Text:     "商品名称相对商品编码左移 30px",
			Target:   SemanticTarget{ID: "hdr-name", File: "web/src/view/distribution/disOrder/disOrder.vue", Kind: "dom_metric"},
			Expected: json.RawMessage(`{"left_delta_px":30}`)},
		{ID: "gap-time-60", Kind: InvariantKindDOMMetric, Status: InvariantActive, Verification: "dom_metric",
			Text:     "下单时间相对商品名称左移 60px",
			Target:   SemanticTarget{ID: "hdr-time", File: "web/src/view/distribution/disOrder/disOrder.vue", Kind: "dom_metric"},
			Expected: json.RawMessage(`{"left_delta_px":60}`)},
	}
}

// TestNormalizeContract_EmptyWriteSetIsNotAllowAll 空 WriteSet 必须保持为空：
// 计划 §3.2 明确"空 WriteSet = 尚未解析"，任何把空集当全允许的实现在 WP2 会放行越界写入。
func TestNormalizeContract_EmptyWriteSetIsNotAllowAll(t *testing.T) {
	c := &LogicalTaskContract{
		LogicalTaskID: "lt-1", SessionID: "s-1", Revision: 1,
		Goal: "修省略", Status: TaskStatusActive,
	}
	if err := NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	if len(c.WriteSet) != 0 {
		t.Fatalf("empty write set must stay empty, got %+v", c.WriteSet)
	}
	rules := c.WriteSet
	if len(rules) == 0 {
		// 语义断言：空集合不允许任何写入（由 WP2 Authorize 实现，此处锁定字段语义）。
		if containsAllowAll(rules) {
			t.Fatal("empty write set must not expand to allow-all")
		}
	}
}

// containsAllowAll 是守卫：任何 allow:* 或 allow:** 都被视为"全允许"。
func containsAllowAll(rules []ResourceRule) bool {
	for _, r := range rules {
		if r.Effect != RuleEffectAllow {
			continue
		}
		if r.Value == "*" || r.Value == "**" || r.Value == "/" || r.Value == "." {
			return true
		}
	}
	return false
}

// TestNormalizeContract_RejectsNaturalLanguageOnlyInvariant 无法机器验证的不变量
// 必须在 Validate 阶段拒绝（§3.2：只存自然语言不算数）。
func TestNormalizeContract_RejectsNaturalLanguageOnlyInvariant(t *testing.T) {
	c := &LogicalTaskContract{
		LogicalTaskID: "lt-2", SessionID: "s-2", Revision: 1, Status: TaskStatusActive,
		Invariants: []Invariant{{ID: "x", Kind: "natural_language", Expected: json.RawMessage(`{}`)}},
	}
	if err := NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	if err := ValidateContract(c); err == nil {
		t.Fatal("invariant with non-verifiable kind must be rejected")
	}
}

// TestContractHashStableAndStatusIndependent hash 必须排除展示字段与状态推进。
func TestContractHashStableAndStatusIndependent(t *testing.T) {
	mk := func() *LogicalTaskContract {
		return &LogicalTaskContract{
			LogicalTaskID: "lt-h", SessionID: "s-h", Revision: 1, Goal: "改标题",
			Targets:    fixtureTargets(),
			WriteSet:   fixtureRules(),
			Invariants: gapInvariants(),
			Status:     TaskStatusActive,
		}
	}
	a, b := mk(), mk()
	if err := NormalizeContract(a); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeContract(b); err != nil {
		t.Fatal(err)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatal("same content must produce same hash")
	}
	// 状态推进不得改变合同 hash（否则验收证据会被误判过期）。
	b.Status = TaskStatusVerified
	rehashed := ComputeContractHash(b)
	if rehashed != a.ContentHash {
		t.Fatalf("status transition must not change contract hash: %s vs %s", rehashed, a.ContentHash)
	}
	// 目标顺序不敏感（规范化排序）。
	b2 := mk()
	b2.Targets = []SemanticTarget{fixtureTargets()[1], fixtureTargets()[0]}
	if err := NormalizeContract(b2); err != nil {
		t.Fatal(err)
	}
	if b2.ContentHash != a.ContentHash {
		t.Fatal("target order must not affect contract hash")
	}
	// 真正的内容变化必须改变 hash。
	b3 := mk()
	b3.Goal = "改标题为 5em"
	if err := NormalizeContract(b3); err != nil {
		t.Fatal(err)
	}
	if b3.ContentHash == a.ContentHash {
		t.Fatal("goal change must change hash")
	}
}

// TestStatusTransitions 状态机：verified 与 accepted 分离，终态不可回退。
func TestStatusTransitions(t *testing.T) {
	c := &LogicalTaskContract{LogicalTaskID: "lt-s", SessionID: "s-s", Revision: 1, Status: TaskStatusActive}
	if err := AdvanceStatus(c, TaskStatusVerified); err != nil {
		t.Fatalf("active -> verified should be allowed: %v", err)
	}
	if err := AdvanceStatus(c, TaskStatusAccepted); err != nil {
		t.Fatalf("verified -> accepted should be allowed: %v", err)
	}
	if err := AdvanceStatus(c, TaskStatusActive); err == nil {
		t.Fatal("accepted is final: must reject transition back to active")
	}
	// 非法迁移：draft 直达 accepted 必须拒绝（不能跳过验证）。
	d := &LogicalTaskContract{Status: TaskStatusDraft}
	if err := AdvanceStatus(d, TaskStatusAccepted); err == nil {
		t.Fatal("draft -> accepted must be rejected")
	}
}

// TestInheritInvariants_KeepsGapInvariantsForNextTask 第 20 轮不变量在第 21 轮仍 active。
func TestInheritInvariants_KeepsGapInvariantsForNextTask(t *testing.T) {
	prev := &LogicalTaskContract{
		LogicalTaskID: "lt-20", SessionID: "s", Revision: 3, Goal: "加间距",
		WriteSet:   fixtureRules(),
		Invariants: gapInvariants(),
		Status:     TaskStatusDeliveredUnverified,
	}
	res := InheritInvariants(prev, fixtureTargets(), fixtureRules(), "商品名称的长度还没接近下单时间标题就已经省略了，这点修复下")
	if len(res.Inherited) != 2 {
		t.Fatalf("both gap invariants must be inherited, got %+v", res)
	}
	for _, inv := range res.NeedsConfirm {
		t.Fatalf("overlapping file invariants must not need confirmation: %+v", inv)
	}
	if len(res.Revoked) != 0 {
		t.Fatalf("no revocation requested, got %+v", res.Revoked)
	}
}

// TestInheritInvariants_ExplicitRevocationOnlyRevokesMatched 显式撤销只影响对应约束。
func TestInheritInvariants_ExplicitRevocationOnlyRevokesMatched(t *testing.T) {
	prev := &LogicalTaskContract{
		LogicalTaskID: "lt-r", SessionID: "s", Revision: 1,
		Invariants: gapInvariants(), Status: TaskStatusVerified,
	}
	res := InheritInvariants(prev, fixtureTargets(), fixtureRules(), "撤销之前的间距方案，重新做")
	if len(res.Revoked) != 2 {
		t.Fatalf("explicit revocation must revoke inherited invariants, got %+v", res.Revoked)
	}
	if len(res.Inherited) != 0 {
		t.Fatalf("revoked invariants must not also be inherited: %+v", res.Inherited)
	}
}

// TestInheritInvariants_UnrelatedFileGoesToNeedsConfirm 无法判断相关性时保留并标记确认，
// 不得静默丢弃（§3.3 第 5 条）。
func TestInheritInvariants_UnrelatedFileGoesToNeedsConfirm(t *testing.T) {
	prev := &LogicalTaskContract{
		LogicalTaskID: "lt-u", SessionID: "s", Revision: 1,
		Invariants: []Invariant{{
			ID: "other-file", Kind: InvariantKindFileHash, Status: InvariantActive,
			Target:   SemanticTarget{ID: "t", File: "web/src/view/distribution/disOrderHistory/disOrderHistory.vue"},
			Expected: json.RawMessage(`{"sha256":"deadbeef"}`),
		}},
		Status: TaskStatusVerified,
	}
	res := InheritInvariants(prev, fixtureTargets(), fixtureRules(), "修改收货人标题")
	if len(res.Inherited) != 0 {
		t.Fatalf("unrelated invariant must not be inherited silently: %+v", res.Inherited)
	}
	if len(res.NeedsConfirm) != 1 || !res.NeedsConfirm[0].NeedsConfirm {
		t.Fatalf("unrelated invariant must be kept with needs_confirm, got %+v", res)
	}
}

// TestTaskManager_IdempotentCreateReplay 相同幂等键重放不产生重复合同。
func TestTaskManager_IdempotentCreateReplay(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.Create(ctx, TaskInput{SessionID: "s-idem", Goal: "改标题", WriteSet: fixtureRules(), Actor: "goal"})
	if err != nil || c == nil {
		t.Fatalf("create: %v", err)
	}
	// 直接以同一幂等键重放事件：存储层必须幂等（返回 nil 且不新增事件）。
	ev := LogicalTaskEvent{
		SessionID: "s-idem", LogicalTaskID: c.LogicalTaskID, Revision: c.Revision,
		Kind: LogicalEventCreated, Payload: `{}`, IdempotencyKey: "dup-key",
	}
	first, _ := json.Marshal(c)
	_ = first
	if err := store.AppendEvent(ctx, c, ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	before, _ := store.ListEvents(ctx, "s-idem", c.LogicalTaskID)
	if err := store.AppendEvent(ctx, c, ev); err != nil {
		t.Fatalf("replay append: %v", err)
	}
	after, _ := store.ListEvents(ctx, "s-idem", c.LogicalTaskID)
	if len(before) != len(after) {
		t.Fatalf("idempotent replay must not append duplicate events: %d -> %d", len(before), len(after))
	}
}

// TestTaskManager_RefineKeepsRevisionHistory refine 产生新 revision 且旧 revision 事件保留。
func TestTaskManager_RefineKeepsRevisionHistory(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.Create(ctx, TaskInput{SessionID: "s-rev", Goal: "加间距", WriteSet: fixtureRules(), Actor: "goal"})
	if err != nil || c == nil {
		t.Fatalf("create: %v", err)
	}
	prevHash := c.ContentHash
	next, err := m.Refine(ctx, c, TaskInput{
		SessionID: "s-rev", Goal: "30px/60px", Actor: "user_message",
		Invariants: gapInvariants(),
	})
	if err != nil {
		t.Fatalf("refine: %v", err)
	}
	if next.Revision != 2 || next.ParentRevision != 1 {
		t.Fatalf("refine must bump revision: %+v", next)
	}
	if next.ContentHash == prevHash {
		t.Fatal("refine with new invariants must change contract hash")
	}
	if len(next.ActiveInvariants()) != 2 {
		t.Fatalf("refined contract must carry invariants: %+v", next.Invariants)
	}
	events, err := store.ListEvents(ctx, "s-rev", next.LogicalTaskID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	if kinds[LogicalEventCreated] != 1 || kinds[LogicalEventRefined] != 1 {
		t.Fatalf("event ledger must keep both revisions, got %+v", kinds)
	}
}

// TestRebuildProjection_MatchesProjectionAfterRestart 压缩/重启后合同 hash 不变。
func TestRebuildProjection_MatchesProjectionAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.Create(ctx, TaskInput{SessionID: "s-reb", Goal: "任务", WriteSet: fixtureRules()})
	if err != nil || c == nil {
		t.Fatalf("create: %v", err)
	}
	c2, err := m.Refine(ctx, c, TaskInput{SessionID: "s-reb", Invariants: gapInvariants()})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(ctx, "s-reb", c.LogicalTaskID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := RebuildProjection("s-reb", c.LogicalTaskID, events)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt == nil {
		t.Fatal("rebuild returned nil projection")
	}
	if rebuilt.Revision != c2.Revision {
		t.Fatalf("rebuilt revision = %d, want %d", rebuilt.Revision, c2.Revision)
	}
	if got, want := ComputeContractHash(rebuilt), ComputeContractHash(c2); got != want {
		t.Fatalf("rebuilt contract hash drift: got %s want %s", got, want)
	}
	if len(rebuilt.ActiveInvariants()) != 2 {
		t.Fatalf("rebuilt projection must keep invariants: %+v", rebuilt.Invariants)
	}
}

// TestTaskManager_LegacyFallbackNoFakeAccepted 旧会话无合同必须走 fallback，
// 且绝不伪造 accepted。
func TestTaskManager_LegacyFallbackNoFakeAccepted(t *testing.T) {
	ctx := context.Background()
	m := NewTaskManager(NewMemoryLogicalTaskStore(), LogicalTaskModeEnforce)
	c, err := m.ResolveActiveContract(ctx, "legacy-session")
	if !errors.Is(err, ErrLogicalTaskNotFound) {
		t.Fatalf("legacy session must return ErrLogicalTaskNotFound, got c=%+v err=%v", c, err)
	}
	if c != nil {
		t.Fatal("legacy session must not fabricate a contract")
	}
}

// TestTaskManager_OffModeIsNoop off 模式零行为（灰度回退开关）。
func TestTaskManager_OffModeIsNoop(t *testing.T) {
	ctx := context.Background()
	m := NewTaskManager(nil, LogicalTaskModeOff)
	c, err := m.Create(ctx, TaskInput{SessionID: "s-off", Goal: "x"})
	if err != nil || c != nil {
		t.Fatalf("off mode must be no-op, got c=%+v err=%v", c, err)
	}
	if m.Mode() != LogicalTaskModeOff {
		t.Fatalf("mode = %q", m.Mode())
	}
}

// TestNormalizeLogicalTaskMode 非法值回落最保守 shadow。
func TestNormalizeLogicalTaskMode(t *testing.T) {
	cases := map[string]string{
		"":          LogicalTaskModeShadow,
		"off":       LogicalTaskModeOff,
		"SHADOW":    LogicalTaskModeShadow,
		"enforce":   LogicalTaskModeEnforce,
		" enforce ": LogicalTaskModeEnforce,
		"bogus":     LogicalTaskModeShadow,
	}
	for in, want := range cases {
		if got := NormalizeLogicalTaskMode(in); got != want {
			t.Fatalf("NormalizeLogicalTaskMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTaskManager_EnsureTaskCreatesNewAfterTerminal 已完成交付后的新输入默认新任务，
// 并记录 RelatedTaskIDs（§3.3 第 3 条）。
func TestTaskManager_EnsureTaskCreatesNewAfterTerminal(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	first, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-ens", Goal: "任务一", WriteSet: fixtureRules()})
	if err != nil || first == nil {
		t.Fatalf("ensure first: %v", err)
	}
	verified, err := m.Transition(ctx, first, TaskStatusVerified, LogicalEventVerified, "verifier", "L0 pass")
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	second, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-ens", Goal: "任务二",
		WriteSet: fixtureRules(), Targets: fixtureTargets()})
	if err != nil || second == nil {
		t.Fatalf("ensure second: %v", err)
	}
	if second.LogicalTaskID == verified.LogicalTaskID {
		t.Fatal("terminal task must not be refined in place")
	}
	found := false
	for _, id := range second.RelatedTaskIDs {
		if id == verified.LogicalTaskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("new task must relate to previous task, got %+v", second.RelatedTaskIDs)
	}
	// 旧任务投影必须是 superseded，不是被删除。
	old, err := store.GetByID(ctx, "s-ens", verified.LogicalTaskID)
	if err != nil || old == nil {
		t.Fatalf("old task must remain queryable: %v", err)
	}
	if old.Status != TaskStatusSuperseded {
		t.Fatalf("old task status = %q, want superseded", old.Status)
	}
}

// TestTaskManager_EnsureTaskRefinesActive 运行中纠正对未终态任务产生 refine。
func TestTaskManager_EnsureTaskRefinesActive(t *testing.T) {
	ctx := context.Background()
	m := NewTaskManager(NewMemoryLogicalTaskStore(), LogicalTaskModeEnforce)
	first, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-run", Goal: "加间距", WriteSet: fixtureRules()})
	if err != nil || first == nil {
		t.Fatalf("ensure: %v", err)
	}
	second, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-run", Goal: "只修省略", Actor: "mailbox"})
	if err != nil || second == nil {
		t.Fatalf("ensure refine: %v", err)
	}
	if second.LogicalTaskID != first.LogicalTaskID {
		t.Fatalf("running correction must refine same task, got %s -> %s", first.LogicalTaskID, second.LogicalTaskID)
	}
	if second.Revision != first.Revision+1 {
		t.Fatalf("revision must bump: %d -> %d", first.Revision, second.Revision)
	}
}
