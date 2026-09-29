package subagent

import (
	"testing"
	"time"
)

func TestPendingRequestCompleteByReplyTo(t *testing.T) {
	r := newPendingRequestRegistry()
	r.add(&pendingRequest{MsgID: "m1", From: "a", To: "b", ThreadID: "m1", Deadline: time.Now().Add(time.Minute)})
	if got := r.complete("m1"); got == nil || got.From != "a" {
		t.Fatalf("complete 应命中 m1：%+v", got)
	}
	if got := r.complete("m1"); got != nil {
		t.Fatal("重复销账应幂等返回 nil")
	}
}

func TestPendingRequestMatchAuto(t *testing.T) {
	r := newPendingRequestRegistry()
	old := &pendingRequest{MsgID: "m-old", From: "meta", To: "b", ThreadID: "m-old", Deadline: time.Now().Add(time.Minute)}
	r.add(old)
	time.Sleep(time.Millisecond) // 保证 createdAt 先后
	r.add(&pendingRequest{MsgID: "m-new", From: "meta", To: "b", ThreadID: "m-new", Deadline: time.Now().Add(time.Minute)})
	// b 回复 meta：自动配对最近一条未答 request。
	got := r.matchAuto("b", "meta", "")
	if got == nil || got.MsgID != "m-new" {
		t.Fatalf("自动配对应取最近一条：%+v", got)
	}
	// thread 过滤：带 thread 时不串链。
	if got := r.matchAuto("b", "meta", "m-old"); got == nil || got.MsgID != "m-old" {
		t.Fatalf("thread 过滤失效：%+v", got)
	}
	// 反向（meta 回复 b）不命中。
	if got := r.matchAuto("meta", "b", ""); got != nil {
		t.Fatalf("方向反了不该命中：%+v", got)
	}
}

func TestPendingRequestExpire(t *testing.T) {
	r := newPendingRequestRegistry()
	r.add(&pendingRequest{MsgID: "m-exp", From: "a", To: "b", Deadline: time.Now().Add(-time.Second)})
	r.add(&pendingRequest{MsgID: "m-live", From: "a", To: "b", Deadline: time.Now().Add(time.Hour)})
	expired := r.expire(time.Now())
	if len(expired) != 1 || expired[0].MsgID != "m-exp" {
		t.Fatalf("expire 应只弹超期项：%+v", expired)
	}
	// 已弹出不再重复弹。
	if again := r.expire(time.Now()); len(again) != 0 {
		t.Fatalf("expire 应销账不重复：%+v", again)
	}
}
