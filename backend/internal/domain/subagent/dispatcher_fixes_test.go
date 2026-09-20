package subagent

import (
	"sync"
	"testing"
	"time"
)

// TestClosePatrol_ConcurrentIdempotent 修复 F1：并发/重复 ClosePatrol 不得 panic
//（close of closed channel），patrol goroutine 必须真正退出（持引用而非重读 nil 字段）。
func TestClosePatrol_ConcurrentIdempotent(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.heartbeatTimeout = 20 * time.Millisecond
	d.ensurePatrol()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.ClosePatrol()
		}()
	}
	wg.Wait()

	// 重复调用为空操作。
	d.ClosePatrol()
}

// TestWaitForAnyChild_CrossWaveNoFalseWakeup 修复 F2：上一波完成残留的合并信号
// 在 count<=0 短路时被清掉，下一波新派发的首次等待不得被旧信号立即唤醒空转。
// 语义契约：返回 true = "请调用方重查条件"（waitForChildren 自行复核计数与邮箱），
// 超时 false = 期间无任何信号。
func TestWaitForAnyChild_CrossWaveNoFalseWakeup(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	p := "session-1/meta"

	// 第一波：两个子完成，期间无等待方——notify 缓冲里残留 1 个合并信号。
	d.trackChildStart(p)
	d.trackChildStart(p)
	d.trackChildDone(p)
	d.trackChildDone(p)

	// 短路入口（count<=0）应清掉残留信号并返回 true。
	if !d.WaitForAnyChild(p, 50*time.Millisecond) {
		t.Fatal("全部完成后 WaitForAnyChild 应返回 true")
	}

	// 第二波：新派发一个子。若残留信号未清，此处会被立即唤醒返回 true（空转）；
	// 修复后应阻塞至超时返回 false，真实完成后再返回 true。
	d.trackChildStart(p)
	start := time.Now()
	if d.WaitForAnyChild(p, 250*time.Millisecond) {
		t.Fatal("新子仍在飞，残留信号导致误唤醒")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("应阻塞至超时，实际仅等 %v", elapsed)
	}

	// 真实完成后立即返回。
	d.trackChildDone(p)
	if !d.WaitForAnyChild(p, 100*time.Millisecond) {
		t.Fatal("子完成后应返回 true")
	}
}

// TestTrackChildDone_NeverNegative 修复 F3②：重复/过量补偿不得把计数打穿到负数。
func TestTrackChildDone_NeverNegative(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	p := "session-1/meta"

	d.trackChildStart(p)
	d.trackChildDone(p)
	d.trackChildDone(p) // 模拟第三路径/doneOnce 失守的重复补偿
	if c := d.getOrCreatePending(p).count.Load(); c != 0 {
		t.Fatalf("计数下限保护失效，count=%d", c)
	}
}

// TestPurgeSession_ClearsPending 修复 F3①：硬删除会话必须回收 pending 计数条目，
// 不得永久驻留 sync.Map。
func TestPurgeSession_ClearsPending(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.trackChildStart("session-9/meta")
	d.trackChildStart("session-9/meta/domain-1")
	d.trackChildStart("session-10/meta")

	d.PurgeSession("session-9", []string{"session-9/meta", "session-9/meta/domain-1"})

	if _, ok := d.pending.Load("session-9/meta"); ok {
		t.Fatal("PurgeSession 应回收 session-9/meta 的 pending 条目")
	}
	if _, ok := d.pending.Load("session-9/meta/domain-1"); ok {
		t.Fatal("PurgeSession 应回收节点级 pending 条目")
	}
	if _, ok := d.pending.Load("session-10/meta"); !ok {
		t.Fatal("其他会话的 pending 条目不得误删")
	}
}
