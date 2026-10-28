package store

// long_term_memory_test.go 长期记忆存储语义（2026-10-24 撤回与污染修复 §11.1）：
//   - content_hash 归一化（同内容换排版/标点 hash 一致）；
//   - active 唯一键（同 owner+scope+project+key 只允许一条 active）；
//   - revision 乐观锁（并发会话旧快照提交失败）；
//   - owner / project 隔离；
//   - events 只追加、EventCount 单调。

import (
	"context"
	"testing"
)

func newMemStore() *MemoryLongTermStore { return NewMemoryLongTermStore() }

// TestMemoryContentHashNormalized 同内容换排版/标点/大小写 hash 一致；不同内容不同。
func TestMemoryContentHashNormalized(t *testing.T) {
	a := MemoryContentHash("四足奔跑默认使用单底图 warp 形变。")
	b := MemoryContentHash("四足奔跑默认使用单底图WARP形变")
	c := MemoryContentHash("四足奔跑使用逐帧生成")
	if a != b {
		t.Fatalf("同内容换标点/大小写 hash 应一致: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("不同内容 hash 不应相同")
	}
}

// TestMemoryTextSimilarity 换措辞应给出高相似度，无关内容低相似度。
func TestMemoryTextSimilarity(t *testing.T) {
	base := "四足奔跑默认使用单底图 warp 形变"
	if s := MemoryTextSimilarity(base, "四足奔跑默认使用单底图 warp 变形"); s < 0.6 {
		t.Fatalf("近似表述相似度应 >=0.6，got %.2f", s)
	}
	if s := MemoryTextSimilarity(base, "以后都用中文回复"); s > 0.3 {
		t.Fatalf("无关内容相似度应低，got %.2f", s)
	}
}

// TestMemoryActiveKeyUnique 同 owner+scope+project+key 只允许一条 active。
func TestMemoryActiveKeyUnique(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	base := MemoryItem{Owner: "o", Scope: MemoryScopeUser, MemoryKey: "response.style", Content: "简洁", Status: MemoryStatusActive}
	if _, err := st.InsertItem(ctx, base); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := st.InsertItem(ctx, base); err == nil {
		t.Fatal("同 key 第二条 active 应被拒绝")
	}
	// 非 active 不冲突。
	base.Status = MemoryStatusCandidate
	base.Content = "简洁（候选）"
	if _, err := st.InsertItem(ctx, base); err != nil {
		t.Fatalf("candidate 不应受 active 唯一键约束: %v", err)
	}
}

// TestMemoryOwnerAndProjectIsolation owner/项目隔离：查询不跨租户、不串项目。
func TestMemoryOwnerAndProjectIsolation(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	pkA := NormalizeProjectKey(`D:\WebData\a`)
	pkB := NormalizeProjectKey(`d:/webdata/b`)
	if pkA == pkB {
		t.Fatal("项目键应规范化后仍可区分")
	}
	_, _ = st.InsertItem(ctx, MemoryItem{Owner: "o1", Scope: MemoryScopeProject, ProjectKey: pkA, MemoryKey: "animation.method", Content: "A 项目做法", Status: MemoryStatusActive})
	_, _ = st.InsertItem(ctx, MemoryItem{Owner: "o2", Scope: MemoryScopeProject, ProjectKey: pkA, MemoryKey: "animation.method", Content: "另一个 owner 的做法", Status: MemoryStatusActive})
	_, _ = st.InsertItem(ctx, MemoryItem{Owner: "o1", Scope: MemoryScopeProject, ProjectKey: pkB, MemoryKey: "animation.method", Content: "B 项目做法", Status: MemoryStatusActive})

	got, err := st.ListItems(ctx, MemoryQuery{Owner: "o1", Scope: MemoryScopeProject, ProjectKey: pkA})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Content != "A 项目做法" {
		t.Fatalf("owner=o1 项目= A 应只看到一条自己的条目，got %+v", got)
	}
}

// TestMemoryRevisionOptimisticLock 旧 revision 提交必须失败（不覆盖新决定）。
func TestMemoryRevisionOptimisticLock(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	it, err := st.InsertItem(ctx, MemoryItem{Owner: "", Scope: MemoryScopeUser, MemoryKey: "response.style", Content: "简洁", Status: MemoryStatusActive})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	updated, err := st.SetStatus(ctx, "", it.ID, MemoryStatusQuarantined, it.Revision)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if updated.Revision != it.Revision+1 {
		t.Fatalf("更新后 revision 应 +1，got %d", updated.Revision)
	}
	if _, err := st.SetStatus(ctx, "", it.ID, MemoryStatusRetracted, it.Revision); err != ErrMemoryRevisionConflict {
		t.Fatalf("旧 revision 提交应冲突，got %v", err)
	}
}

// TestMemoryEventsAppendOnly events 只追加：EventCount 单调、事件类型可追溯。
func TestMemoryEventsAppendOnly(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	it, _ := st.InsertItem(ctx, MemoryItem{Owner: "", Scope: MemoryScopeUser, MemoryKey: "response.style", Content: "简洁", Status: MemoryStatusActive})
	for i, et := range []string{MemoryEventCreated, MemoryEventRetracted, MemoryEventRestored} {
		if err := st.AppendEvent(ctx, MemoryEvent{Owner: "", MemoryID: it.ID, EventType: et, Actor: MemoryActorSystem}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	n, err := st.EventCount(ctx, "")
	if err != nil || n != 3 {
		t.Fatalf("EventCount 应为 3，got %d err=%v", n, err)
	}
	events, _ := st.ListEvents(ctx, "", it.ID, 10)
	if len(events) != 3 || events[0].EventType != MemoryEventCreated || events[2].EventType != MemoryEventRestored {
		t.Fatalf("事件应保持追加顺序，got %+v", events)
	}
}

// TestNormalizeProjectKey 项目键规范化：斜杠/大小写/尾斜杠统一。
func TestNormalizeProjectKey(t *testing.T) {
	a := NormalizeProjectKey(`D:\WebData\Demo\`)
	b := NormalizeProjectKey("d:/webdata/demo")
	if a != b {
		t.Fatalf("项目键规范化不一致: %q vs %q", a, b)
	}
	if NormalizeProjectKey("") != "" {
		t.Fatal("空目录应返回空键")
	}
}
