package cmdqueue

import (
	"errors"  // 用于 errors.Is 校验
	"testing" // Go 测试框架
)

// TestManager_EnqueueAndDrain 验证入队、Drain 与 HasPending 的基本流程。
func TestManager_EnqueueAndDrain(t *testing.T) {
	mgr := NewManager()
	sessionID := "session-1"

	// 入队第一条：追加意图
	if err := mgr.Enqueue(sessionID, Item{Content: "first", Intent: IntentEnqueue}); err != nil {
		t.Fatalf("enqueue first item: %v", err)
	}
	// 入队第二条：中断意图
	if err := mgr.Enqueue(sessionID, Item{Content: "second", Intent: IntentInterrupt}); err != nil {
		t.Fatalf("enqueue second item: %v", err)
	}

	// Drain 应取出全部两条指令
	items := mgr.Drain(sessionID)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// 校验第一条内容与意图
	if items[0].Content != "first" || items[0].Intent != IntentEnqueue {
		t.Fatalf("first item mismatch: %+v", items[0])
	}
	// 校验第二条内容与意图
	if items[1].Content != "second" || items[1].Intent != IntentInterrupt {
		t.Fatalf("second item mismatch: %+v", items[1])
	}

	// Drain 后队列已空
	if mgr.HasPending(sessionID) {
		t.Fatal("expected no pending items after drain")
	}
}

// TestManager_EnqueueReturnsQueueFull 验证队列达到容量上限后返回 ErrQueueFull。
func TestManager_EnqueueReturnsQueueFull(t *testing.T) {
	mgr := NewManager()
	sessionID := "session-full"

	// 填充队列至默认容量
	for i := 0; i < defaultQueueCap; i++ {
		if err := mgr.Enqueue(sessionID, Item{Content: "fill", Intent: IntentEnqueue}); err != nil {
			t.Fatalf("enqueue item %d: %v", i, err)
		}
	}

	// 超过容量时应返回 ErrQueueFull，而不是无界增长
	err := mgr.Enqueue(sessionID, Item{Content: "overflow", Intent: IntentEnqueue})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	// 队列长度应严格等于容量
	items := mgr.Drain(sessionID)
	if len(items) != defaultQueueCap {
		t.Fatalf("expected %d items, got %d", defaultQueueCap, len(items))
	}
}

// TestQueue_EnqueueRespectsCustomCap 验证 Queue 自身容量上限不受默认容量影响。
func TestQueue_EnqueueRespectsCustomCap(t *testing.T) {
	q := &Queue{cap: 2}

	if err := q.Enqueue(Item{Content: "a"}); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if err := q.Enqueue(Item{Content: "b"}); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if err := q.Enqueue(Item{Content: "c"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
}
