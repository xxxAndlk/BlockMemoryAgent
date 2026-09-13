package subagent

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestTrackChildDoneInvokesChildDoneFn 验证：注入 WithChildDoneNotify 后，
// trackChildDone 每次递减都会（异步）触发回调并带上父 Agent ID——
// 这是「挂起等子」awaiting_child 会话的唤醒入口。
func TestTrackChildDoneInvokesChildDoneFn(t *testing.T) {
	var got atomic.Value // string
	d := NewDispatcher(nil, nil, nil, nil, nil).
		WithChildDoneNotify(func(parentID string) { got.Store(parentID) })

	// 两个子 Agent，只完成一个（计数不归零，避免触发跨域契约检查的额外 goroutine）。
	d.trackChildStart("parent-1")
	d.trackChildStart("parent-1")
	d.trackChildDone("parent-1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := got.Load(); v != nil {
			if v.(string) != "parent-1" {
				t.Fatalf("childDoneFn parentID=%q, want parent-1", v)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("childDoneFn 未在 2s 内被调用")
}

// TestTrackChildDoneWithoutChildDoneFn 验证：未注入回调时 trackChildDone
// 正常递减且不 panic（nil 安全）。
func TestTrackChildDoneWithoutChildDoneFn(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.trackChildStart("parent-2")
	d.trackChildStart("parent-2")
	d.trackChildDone("parent-2")
	if n := d.PendingChildren("parent-2"); n != 1 {
		t.Fatalf("PendingChildren=%d, want 1", n)
	}
}
