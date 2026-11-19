package team

// logical_task_replay_test.go 覆盖 2026-11-19 再次验收 P0-2 / P1-2：
//   - 所有生产写事件路径都必须「写入后立即重放」一致（快照类带全量合同，迁移类只改状态）；
//   - 结算迁移不得复用 refined（空 payload 快照事件会让重放解码失败）；
//   - 用户接受必须按交付事实选任务，不能取排序数组末尾。

import (
	"context"
	"encoding/json"
	"testing"
)

// assertReplayMatches 写入后立即重放，投影必须与写入方返回的合同完全一致。
func assertReplayMatches(t *testing.T, store LogicalTaskStore, want *LogicalTaskContract) {
	t.Helper()
	events, err := store.ListEvents(context.Background(), want.SessionID, want.LogicalTaskID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("事件底账为空：写入路径没有留下可审计事件")
	}
	for _, ev := range events {
		if IsContractSnapshotEvent(ev.Kind) && ev.Payload == "" {
			t.Fatalf("快照类事件 %s(id=%d) 缺 payload：重放必失败", ev.Kind, ev.EventID)
		}
	}
	got, err := RebuildProjection(want.SessionID, want.LogicalTaskID, events)
	if err != nil {
		t.Fatalf("重放失败（事件底账与投影不一致）：%v", err)
	}
	if got == nil {
		t.Fatal("重放得到空投影")
	}
	if got.Status != want.Status {
		t.Fatalf("重放状态 %q != 写入状态 %q", got.Status, want.Status)
	}
	if got.Revision != want.Revision {
		t.Fatalf("重放 revision %d != %d", got.Revision, want.Revision)
	}
	if got.ContentHash != want.ContentHash {
		t.Fatalf("重放 hash %s != %s", got.ContentHash, want.ContentHash)
	}
}

// TestSettlementTransitionReplays 结算用的 awaiting_verification 事件必须可重放。
func TestSettlementTransitionReplays(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.EnsureTask(ctx, TaskInput{
		SessionID: "s-replay", Goal: "结算事件可重放",
		WriteSet: []ResourceRule{{Kind: RuleKindPath, Value: "src/a.css", Effect: RuleEffectAllow}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaiting, err := m.Transition(ctx, c, TaskStatusAwaitingVerification,
		LogicalEventAwaitingVerification, "session", "session turn settled")
	if err != nil {
		t.Fatalf("结算迁移失败：%v", err)
	}
	assertReplayMatches(t, store, awaiting)

	done, err := m.Transition(ctx, awaiting, TaskStatusDeliveredUnverified, LogicalEventUnverified, "session", "outcome=success")
	if err != nil {
		t.Fatal(err)
	}
	assertReplayMatches(t, store, done)

	accepted, err := m.Accept(ctx, done, "user")
	if err != nil {
		t.Fatal(err)
	}
	assertReplayMatches(t, store, accepted)
}

// TestTransitionWithSnapshotEventCarriesContract 若调用方误用 refined 做迁移，
// payload 仍必须是完整合同（防御性：宁可在写入侧补全，也不产生不可重放事件）。
func TestTransitionWithSnapshotEventCarriesContract(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-snap", Goal: "快照类迁移"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := m.Transition(ctx, c, TaskStatusAwaitingVerification, LogicalEventRefined, "session", "misuse")
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(ctx, c.SessionID, c.LogicalTaskID)
	if err != nil {
		t.Fatal(err)
	}
	var refined LogicalTaskEvent
	for _, ev := range events {
		if ev.Kind == LogicalEventRefined {
			refined = ev
		}
	}
	if refined.Payload == "" {
		t.Fatal("refined 事件必须带全量合同快照")
	}
	var decoded LogicalTaskContract
	if err := json.Unmarshal([]byte(refined.Payload), &decoded); err != nil {
		t.Fatalf("refined payload 必须能解码成合同：%v", err)
	}
	if decoded.Status != TaskStatusAwaitingVerification {
		t.Fatalf("快照里的状态必须是迁移后的状态：%q", decoded.Status)
	}
	assertReplayMatches(t, store, next)
}

// TestApplyEventRejectsEmptySnapshotPayload 空 payload 的快照事件是坏事件，必须显式报错。
func TestApplyEventRejectsEmptySnapshotPayload(t *testing.T) {
	for _, kind := range []string{LogicalEventCreated, LogicalEventRefined} {
		if _, err := ApplyEvent(nil, LogicalTaskEvent{Kind: kind, Payload: ""}); err == nil {
			t.Fatalf("%s 空 payload 必须报错（不得静默跳过）", kind)
		}
	}
}

// TestFindAcceptablePicksByDeliveryFact 接受选择必须按交付事实，不是 revision 排序末尾。
func TestFindAcceptablePicksByDeliveryFact(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)

	// 旧任务：多次 refine，revision 被推到 3，最后被新任务替代（superseded）。
	old, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-accept", Goal: "旧任务"})
	if err != nil {
		t.Fatal(err)
	}
	old, err = m.Refine(ctx, old, TaskInput{SessionID: "s-accept", Goal: "旧任务改一版"})
	if err != nil {
		t.Fatal(err)
	}
	old, err = m.Refine(ctx, old, TaskInput{SessionID: "s-accept", Goal: "旧任务再改一版"})
	if err != nil {
		t.Fatal(err)
	}
	if old.Revision != 3 {
		t.Fatalf("旧任务 revision 应为 3，实际 %d", old.Revision)
	}

	// 新任务：revision=1，正常交付。EnsureTask 会把旧任务 supersede。
	fresh, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-accept", Goal: "新任务"})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := m.Transition(ctx, fresh, TaskStatusAwaitingVerification,
		LogicalEventAwaitingVerification, "session", "settled")
	if err != nil {
		t.Fatal(err)
	}
	delivered, err = m.Transition(ctx, delivered, TaskStatusDeliveredUnverified,
		LogicalEventUnverified, "session", "outcome=success")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.FindAcceptable(ctx, "s-accept")
	if err != nil {
		t.Fatalf("必须能查到可接受任务：%v", err)
	}
	if got.LogicalTaskID != delivered.LogicalTaskID {
		t.Fatalf("可接受任务必须是新交付的 %s，实际 %s（旧任务 revision=%d 更大，不得被选中）",
			delivered.LogicalTaskID, got.LogicalTaskID, old.Revision)
	}
	if !TaskAcceptsUserDecision(got.Status) {
		t.Fatalf("可接受任务状态必须是 verified/delivered-unverified：%s", got.Status)
	}

	// 接受后再查：不再是可接受状态。
	if _, err := m.Accept(ctx, got, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindAcceptable(ctx, "s-accept"); err == nil {
		t.Fatal("已接受的任务不得再被选中")
	}
}

// TestFindAcceptableSkipsFailedAndActive failed / active 任务都不可接受。
func TestFindAcceptableSkipsFailedAndActive(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLogicalTaskStore()
	m := NewTaskManager(store, LogicalTaskModeEnforce)
	c, err := m.EnsureTask(ctx, TaskInput{SessionID: "s-skip", Goal: "失败任务"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindAcceptable(ctx, "s-skip"); err == nil {
		t.Fatal("active 任务不可接受")
	}
	failed, err := m.Transition(ctx, c, TaskStatusFailed, LogicalEventFailed, "session", "outcome=error")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindAcceptable(ctx, "s-skip"); err == nil {
		t.Fatalf("failed 任务不可接受（状态 %s）", failed.Status)
	}
}

// TestTaskAcceptsUserDecision 状态判定表。
func TestTaskAcceptsUserDecision(t *testing.T) {
	accept := []string{TaskStatusVerified, TaskStatusDeliveredUnverified}
	for _, s := range accept {
		if !TaskAcceptsUserDecision(s) {
			t.Fatalf("%s 必须可接受", s)
		}
	}
	reject := []string{TaskStatusDraft, TaskStatusActive, TaskStatusBlocked, TaskStatusAwaitingClarification,
		TaskStatusAwaitingVerification, TaskStatusFailed, TaskStatusCancelled, TaskStatusAccepted, TaskStatusSuperseded}
	for _, s := range reject {
		if TaskAcceptsUserDecision(s) {
			t.Fatalf("%s 不得可接受", s)
		}
	}
}
