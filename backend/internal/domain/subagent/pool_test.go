package subagent

import (
	"context"
	"testing"
	"time"
)

// 上限阻塞 + FIFO 顺序：满池后三个等待者**顺序入队**，放行后按入队序获名额。
func TestExecPoolFIFO(t *testing.T) {
	p := newExecPool(1)
	if err := p.Acquire(context.Background(), "holder"); err != nil {
		t.Fatal(err)
	}
	grantCh := make(chan string, 3)
	// 顺序入队：每个等待者确认入队后再派下一个，消除 goroutine 调度乱序。
	for i, id := range []string{"w1", "w2", "w3"} {
		id := id
		go func() {
			if err := p.Acquire(context.Background(), id); err != nil {
				t.Error(err)
				return
			}
			grantCh <- id // 持名额后上报（limit=1，任一时刻只有一个持有者，无并发写）
			p.Release()
		}()
		// 确认本等待者已入队再派下一个，消除 goroutine 调度乱序。
		deadline := time.Now().Add(2 * time.Second)
		for p.StatsQueued() != i+1 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.StatsQueued() != 3 {
		t.Fatalf("应有 3 个排队者，got %d", p.StatsQueued())
	}
	p.Release() // holder 放行，链式移交
	var got []string
	for i := 0; i < 3; i++ {
		select {
		case id := <-grantCh:
			got = append(got, id)
		case <-time.After(2 * time.Second):
			t.Fatalf("等待者未获名额：got=%v", got)
		}
	}
	if got[0] != "w1" || got[1] != "w2" || got[2] != "w3" {
		t.Fatalf("FIFO 顺序破坏：%v", got)
	}
	if r, q := p.Stats(); r != 0 || q != 0 {
		t.Fatalf("结束后 running/queued 应归零：%d/%d", r, q)
	}
}

// 排队期 ctx 取消：票据摘除，不占名额。
func TestExecPoolCancelWhileQueued(t *testing.T) {
	p := newExecPool(1)
	_ = p.Acquire(context.Background(), "holder")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.Acquire(ctx, "cancelled") }()
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("取消应返回 ctx.Err()")
	}
	p.Release() // holder 归还
	// 名额应可直接获取（取消的票据已摘除，不挡路）。
	got := make(chan error, 1)
	go func() { got <- p.Acquire(context.Background(), "next") }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消的票据挡住了后续 Acquire")
	}
}

// 取消与授予竞态：已授予的票据不因迟到的 cancel 丢名额（调用方 defer Release 归还）。
func TestExecPoolCancelGrantRace(t *testing.T) {
	p := newExecPool(1)
	_ = p.Acquire(context.Background(), "holder")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- p.Acquire(ctx, "racer") }()
	deadline := time.Now().Add(2 * time.Second)
	for p.StatsQueued() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	p.Release() // 授予 racer
	cancel()    // 迟到的取消
	if err := <-got; err != nil {
		t.Fatalf("已授予后取消应视为成功：%v", err)
	}
	p.Release() // racer 归还（不能 panic/阻塞）
}

func TestExecPoolUnlimited(t *testing.T) {
	if newExecPool(0) != nil || newExecPool(-1) != nil {
		t.Fatal("limit<=0 应返回 nil（不限）")
	}
}
