// pool.go 实现子 Agent 全局并发池（2026-09-28 P1 资源治理）：限制同时在跑的子
// Agent 总数，超额派发 FIFO 排队而非拒绝（拒绝会与模型重试叠加成拒绝循环）。
//
// 职责边界：池只做准入控制（Acquire/Release/Stats）——子 Agent 生命周期权威仍是
// orchestrator.Tree（注册/取消/巡检/落 PG），池不维护任何 Agent 状态、不参与
// 取消语义（Acquire 等待期 ctx 取消即摘票据退出，树态由取消方收口）。
//
// 不变量：waitQ 非空 ⇒ sem 满。Release 时若有排队者，名额直接移交队首（不还桶），
// 保证 FIFO 且不被新到 Acquire 的 fast path 插队。
//
// 接受有界（2026-09-28 终审 #5）：名额由持名额 goroutine 的 defer Release 归还；
// goroutine 挂死在无视 ctx 的调用里时名额随之滞留至进程重启（killStuckSubAgent
// 会打日志暴露此口径）。不做强收——强收后迟到的 Release 会超发名额，比滞留更危险。
package subagent

import (
	"container/list" // FIFO 票据队列
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// execPoolQueueWarn 排队超此时长打 warn 日志（可观测性最低限度，不加 HTTP 端点）。
const execPoolQueueWarn = 2 * time.Minute

// execTicket 一张排队票据：granted 标记与 close(ch) 构成授予信号，均在 p.mu 下读写。
type execTicket struct {
	ch      chan struct{}
	granted bool
	elem    *list.Element
}

// execPool 并发名额池。limit<=0 时构造返回 nil（调用方 nil 检查即"不限"）。
type execPool struct {
	sem     chan struct{} // 容量=limit 的名额桶
	mu      sync.Mutex    // 保护 waitQ 与 ticket.granted
	waitQ   *list.List    // *execTicket FIFO
	queued  atomic.Int64  // 当前排队数（Stats/测试用）
	running atomic.Int64  // 当前持名额数（Stats/测试用）
}

// newExecPool 创建池；limit<=0 返回 nil 表示不限并发（旧行为）。
func newExecPool(limit int) *execPool {
	if limit <= 0 {
		return nil
	}
	return &execPool{sem: make(chan struct{}, limit), waitQ: list.New()}
}

// Acquire 获取一个执行名额：有空位直通；满则 FIFO 排队等 Release 移交。
// 排队期 ctx 取消：票据摘除返回 ctx.Err()（若取消与授予竞态且授予在先，视为成功——
// 调用方必须 defer Release 归还）。
// id 仅用于排队超时 warn 日志。
//
// 锁纪律（2026-09-29 评审修订）：fast-path 试探与入队必须在同一段 p.mu 临界区内
// 完成——否则"试探失败（sem 满）→ 释放锁前持有者 Release 见 waitQ 空走还桶 → 本
// 调用方再入队"的交错会让票据永久等不到移交（名额空挂、巡检因 queued 豁免不杀，
// 挂到会话取消为止）。临界区内：waitQ 空且 sem 有位 → 直通；否则入队——入队时
// sem 恒满，且并发的 Release 必在锁后看到队首票据走移交，无遗失唤醒。
func (p *execPool) Acquire(ctx context.Context, id string) error {
	// fast path：有空位直通（waitQ 非空时 sem 恒满，不会插队）。
	p.mu.Lock()
	if p.waitQ.Len() == 0 {
		select {
		case p.sem <- struct{}{}:
			p.mu.Unlock()
			p.running.Add(1)
			return nil
		default:
		}
	}
	t := &execTicket{ch: make(chan struct{})}
	t.elem = p.waitQ.PushBack(t)
	p.queued.Add(1)
	p.mu.Unlock()
	warn := time.AfterFunc(execPoolQueueWarn, func() {
		log.Printf("[subagent] exec-pool QUEUE-WAIT: sub=%s 排队超 %s 未获名额", id, execPoolQueueWarn)
	})
	defer warn.Stop()
	select {
	case <-t.ch:
		p.running.Add(1)
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		if t.granted {
			// 授予在先：名额已归本调用方，照常成功（迟到的取消不丢名额）。
			p.mu.Unlock()
			p.running.Add(1)
			return nil
		}
		p.waitQ.Remove(t.elem)
		p.queued.Add(-1)
		p.mu.Unlock()
		return ctx.Err()
	}
}

// Release 归还名额：有排队者直接移交队首（FIFO），否则还桶。
// 锁纪律：Front 判定与还桶（<-p.sem）同处一段 p.mu 临界区——非阻塞 select 可安全
// 在锁内执行；若还桶放出锁外，"判空 → 新 Acquire 入队 → 再还桶"交错会让队首票据
// 空挂（sem 已有位但无人移交）。
func (p *execPool) Release() {
	p.running.Add(-1)
	p.mu.Lock()
	if el := p.waitQ.Front(); el != nil {
		t := el.Value.(*execTicket)
		p.waitQ.Remove(el)
		p.queued.Add(-1)
		t.granted = true
		close(t.ch) // 名额移交队首（桶内占用不归还）
		p.mu.Unlock()
		return
	}
	select {
	case <-p.sem:
	default:
		// 配对失衡（Release 多于 Acquire）防呆：不阻塞、打日志。
		log.Printf("[subagent] exec-pool RELEASE-UNDERFLOW（Acquire/Release 未配对）")
	}
	p.mu.Unlock()
}

// Stats 返回当前持名额数与排队数。
func (p *execPool) Stats() (running, queued int) {
	return int(p.running.Load()), int(p.queued.Load())
}

// StatsQueued 返回排队数（测试便捷方法）。
func (p *execPool) StatsQueued() int {
	return int(p.queued.Load())
}

// enterExecGate 并发池准入 + 墙钟挂载（2026-09-28 P1"出队才计墙钟"）：
//   - concPool==nil（不限）：直通，墙钟立即挂载（旧行为）；
//   - 排队等待期间置 evidence.queued（巡检豁免），出队才挂墙钟——排队不烧预算。
// 返回 runCtx（挂好墙钟的执行 ctx）、runCancel（墙钟释放）、release（名额归还，
// 调用方 defer）、err（排队期 ctx 取消——硬取消由取消方收口树态与父通知，会话软停止
// 由调用方 settleGateAbort 代收口；调用方走"未执行"分支直接收尾，不调 runSubAgent）。
func (d *Dispatcher) enterExecGate(baseCtx context.Context, ev *activityEvidence, subAgentID string, wallClock time.Duration) (context.Context, context.CancelFunc, func(), error) {
	runCancel := context.CancelFunc(func() {})
	release := func() {}
	if d.concPool == nil {
		runCtx := baseCtx
		if wallClock > 0 {
			runCtx, runCancel = context.WithTimeout(baseCtx, wallClock)
		}
		return runCtx, runCancel, release, nil
	}
	if ev != nil {
		ev.markQueued()
	}
	if err := d.concPool.Acquire(baseCtx, subAgentID); err != nil {
		if ev != nil {
			ev.clearQueued(time.Now().UnixNano())
		}
		return baseCtx, runCancel, release, err
	}
	if ev != nil {
		ev.clearQueued(time.Now().UnixNano())
	}
	runCtx := baseCtx
	if wallClock > 0 {
		runCtx, runCancel = context.WithTimeout(baseCtx, wallClock)
	}
	return runCtx, runCancel, d.concPool.Release, nil
}
