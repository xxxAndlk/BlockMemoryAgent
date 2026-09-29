package subagent

// idle_pool.go 实现 DomainAgent 热驻留（TODO：Domain 热驻 + 复用权重）。
//
// 开启（bootstrap 按 config domain_hot_resident_enabled 注入）后：
//   - domain 派发走 runDomainSupervisor 常驻 goroutine：任务完成/用户软停止转入
//     Idle（goroutine park 等复用），不销毁；执行失败才销毁（Failed 由块记忆打捞承接）。
//   - 复用入口 call_sub_agent(reuse_agent_id=X)：槽 idle 则唤醒+注入新任务；
//     busy 则入 taskQueue（执行完当前任务后自动出队）。
//   - Idle 加权 TTL：effectiveTTL = min(BaseTTL + reuseCount*Extend, MaxTTL)。
//     enterIdle 即武装倒计时（任务完成≈父收到回传时就开始计时，避免完成后无限期热存
//     占内存）；复用/用户直连唤醒时停表且 reuseCount+1，本轮完成回 idle 按新权重重新武装满额。
//   - TTL 到期=冷驻（非终态 Done）：实例销毁释放资源，树节点保持 Idle 作可复活
//     注册表条目；reuse_agent_id 命中冷驻节点时 restoreColdSlot 从 agent_messages
//     重建槽（dormant，无 supervisor，首个唤醒序列惰性启动）续跑历史上下文。
//   - 会话级挂起（触限暂停全树）：sessionSuspendState 广播 wake channel，叶子与
//     domain 的 SuspendGate.Park 阻塞其上；ResumeSessionAgents close 广播唤醒。
//   - 心跳/墙钟豁免：Idle/挂起时 activity.Delete（scanStuck Range 不到）；
//     domain 墙钟为 slot timer（挂起时 Stop 存剩余 wallRemain，恢复重挂；并发池
//     过闸/出队才挂载，排队不计预算），非 ctx deadline。
//
// 关闭（默认）时所有路径零变化：dispatchOne 不分流，supervisor 不启动。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// idleRosterFileWindow 是 roster/复用守卫取"近期写入文件"的时间窗口。
const idleRosterFileWindow = 24 * time.Hour

// domainHotConfig 热驻留参数，由 bootstrap 从 config 注入。
type domainHotConfig struct {
	Enabled        bool
	BaseTTL        time.Duration // Idle 基础寿命
	ExtendPerReuse time.Duration // 每次复用延长量
	MaxTTL         time.Duration // 加权倒计时上限
	MaxPerSession  int           // 单 session 热驻上限（超限 LRU 淘毁最旧 idle）
	TaskQueueLen   int           // 忙碌 domain 新任务缓冲上限
}

// DomainHotConfig 是 domainHotConfig 的导出别名，bootstrap 注入用。
type DomainHotConfig = domainHotConfig

// domainOpKind 是 supervisor ops channel 的操作类型。
type domainOpKind int

const (
	opNewTask  domainOpKind = iota // 新任务（复用派发）
	opResume                       // 挂起唤醒（"继续"或任意用户消息的 wake-all）
	opDestroy                      // 销毁（TTL 到期/硬取消/话题切换/失败收口）
)

// domainOp 是发给 supervisor goroutine 的单条指令。
type domainOp struct {
	kind      domainOpKind
	task      string             // opNewTask：任务正文（已拼前缀）
	wallClock time.Duration      // opNewTask：本次派发级墙钟
	resumeMsg string             // opResume：续跑输入（默认"继续"）
	images    []tool.ResultImage // opNewTask：本轮用户图片（Alt+V 粘贴，带外穿透；仅内存不持久化）
	byTTL     bool               // opDestroy：TTL 到期触发（冷驻语义：destroySlot 跳过树 Finish，节点保 Idle）
}

// queuedTask 是忙碌 domain 缓冲的新任务（PendingChildren 已挂账，销毁时须补偿递减）。
type queuedTask struct {
	task      string
	wallClock time.Duration
	images    []tool.ResultImage // 本轮用户图片（同 domainOp.images）
}

// slotState 是 domainSlot 的生命周期状态。
type slotState int

const (
	slotRunning slotState = iota // 执行任务中（含挂起中 suspended=true 嵌套）
	slotIdle                     // 任务完结，park 等复用或 TTL 到期
	slotDestroyed                // 已销毁
)

// domainSlot 单个热驻 domain 的全部状态。
// ops 是 supervisor goroutine 的唯一指令入口（缓冲 8）；其余可变字段由 mu 保护。
type domainSlot struct {
	mu             sync.Mutex
	id             string
	sessionID      string
	// workDir 会话级工作目录（S2）：槽创建时从派发 ctx 捕获，会话内固定；
	// 每任务 ctx 经 tool.WithWorkDir 重注入，空=进程默认。
	workDir        string
	// stopCtx 会话级中断传播基底（TODO 第10④）：任务 ctx 以它为 WithCancel 基底，
	// 会话 Stop/cancel 即刻取消执行中任务；复用派发时随新 runCtx 刷新（restartSessionContext
	// 会重建，旧值已取消不能沿用）。
	stopCtx        context.Context
	parentID       string
	domain         string
	responsibility string
	agent          *agent.ReActAgent // 热实例，跨任务复用（systemPrompt 含冻结 responsibility 头）
	history        []agent.ReactMessage

	state      slotState
	suspended  bool // running 期间被会话级挂起（Park 阻塞中）
	reuseCount int
	taskQueue  []queuedTask
	// dormant 冷恢复重建槽标记（restoreColdSlot）：无 supervisor 常驻 goroutine，
	// 首个唤醒序列（dispatchToIdleSlot/wakeIdleWithTask）惰性启动并清零。
	dormant bool

	wallRemain time.Duration       // 挂起时冻结的剩余墙钟
	wallTimer  *time.Timer         // 墙钟 timer（到期 cancel 当前任务 ctx）
	wallFired  bool                // 墙钟 timer 已到期（Canceled 分支区分墙钟取消与外部硬取消）
	childReported bool             // 当前任务的父未决计数已递减（destroySlot 兜底防双递减）
	cancelTask context.CancelFunc  // 当前任务 ctx cancel
	taskCtx    context.Context     // 当前任务 ctx（挂起判定用）
	// cancelPending: 取消/暂停指令到达时任务 ctx 尚未绑定（派发 Register 与 runDomainTask
	// 建 ctx 之间的窗口），任务入口见标记即刻收口（暂停→park / 硬取消→销毁）。
	cancelPending bool

	// pendingTask: 任务尚未执行即 park 时暂存的任务文本/墙钟/图片，resume 时原样续跑
	// （该路径无 history，"继续"会丢失原任务）。
	pendingTask      string
	pendingWallClock time.Duration
	pendingImages    []tool.ResultImage
	ttlArmed   bool                // TTL 是否已武装（enterIdle 完成即武装）
	ttlTimer   *time.Timer         // 加权倒计时（武装后非 nil）
	ttlDeadline time.Time          // 武装时的到期时刻（IdleLeft 计算用）
	idleSince  time.Time

	// gear 进 Idle 时固化的会话档位（TODO #14 T22）：隐式复用解析按它裁决——
	// 新派发会话档位与槽档位不符时不隐式接管（用户刚把会话切到快速档，不该吃
	// 到集群档攒下的热驻上下文继续跑重活）。空串=未接线/存量槽，按匹配放行。
	gear string

	ops chan domainOp
}

// domainPool 按 sessionID 维护热驻 domain 槽，挂在 Dispatcher 上。
type domainPool struct {
	mu     sync.Mutex
	bySess map[string]map[string]*domainSlot
	cfg    domainHotConfig
}

func newDomainPool(cfg domainHotConfig) *domainPool {
	return &domainPool{bySess: make(map[string]map[string]*domainSlot), cfg: cfg}
}

// sessionSuspendState 会话级挂起广播（叶子+domain 共用）。
// suspended=true 期间 SuspendGate.Park 阻塞在 wake 上；ResumeSession close(wake) 广播唤醒后重建。
type sessionSuspendState struct {
	mu        sync.Mutex
	suspended bool
	wake      chan struct{}
}

func (s *sessionSuspendState) isSuspended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// suspend 置挂起态并重建 wake channel（旧 wake 已 close 的场景）。
func (s *sessionSuspendState) suspend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		s.suspended = true
		s.wake = make(chan struct{})
	}
}

// resume 解除挂起并 close(wake) 广播唤醒所有 Park 中的 goroutine。
func (s *sessionSuspendState) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		s.suspended = false
		if s.wake != nil {
			close(s.wake)
			s.wake = nil
		}
	}
}

// slotSuspendGate 实现 agent.SuspendGate：挂起检查 + Park 在会话 wake channel 上。
// gate 只在会话挂起时阻塞；销毁/取消经任务 ctx 取消（Park 同时 select ctx.Done）。
type slotSuspendGate struct {
	d    *Dispatcher
	sid  string
}

func (g *slotSuspendGate) Park(ctx context.Context) error {
	st := g.d.suspendState(g.sid)
	if st == nil || !st.isSuspended() {
		return nil
	}
	st.mu.Lock()
	var wake chan struct{}
	if st.suspended {
		wake = st.wake
	}
	st.mu.Unlock()
	if wake == nil {
		return nil
	}
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// suspendState 取或创建 session 的挂起状态。
func (d *Dispatcher) suspendState(sessionID string) *sessionSuspendState {
	if sessionID == "" {
		return nil
	}
	if v, ok := d.suspendStates.Load(sessionID); ok {
		return v.(*sessionSuspendState)
	}
	st := &sessionSuspendState{}
	v, loaded := d.suspendStates.LoadOrStore(sessionID, st)
	if loaded {
		return v.(*sessionSuspendState)
	}
	return st
}

// WithDomainHotResident 注入热驻配置。Enabled=false（默认零值）时所有路径零变化。
func (d *Dispatcher) WithDomainHotResident(cfg domainHotConfig) *Dispatcher {
	d.hotCfg = cfg
	if cfg.Enabled {
		d.pool = newDomainPool(cfg)
	}
	return d
}

// hotEnabled 报告热驻是否开启。
func (d *Dispatcher) hotEnabled() bool {
	return d.hotCfg.Enabled && d.pool != nil
}

// slotPaused 报告热驻槽是否已进入暂停落地态（任务收尾完成、park 等唤醒）：
// pause_agent 工具据此确认暂停已生效（随后 resume_agent 才能稳定命中）。
func (d *Dispatcher) slotPaused(sessionID, id string) bool {
	if !d.hotEnabled() {
		return false
	}
	s := d.pool.slot(sessionID, id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == slotRunning && s.suspended
}

// slot 按会话+ID 取热驻槽。
func (p *domainPool) slot(sessionID, id string) *domainSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bySess[sessionID][id]
}

// store 注册槽。
func (p *domainPool) store(s *domainSlot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.bySess[s.sessionID]
	if !ok {
		m = make(map[string]*domainSlot)
		p.bySess[s.sessionID] = m
	}
	m[s.id] = s
}

// remove 摘除槽，返回是否存在。
func (p *domainPool) remove(sessionID, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.bySess[sessionID]
	if !ok {
		return false
	}
	if _, ok := m[id]; !ok {
		return false
	}
	delete(m, id)
	if len(m) == 0 {
		delete(p.bySess, sessionID)
	}
	return true
}

// idleSlots 返回 session 的全部非销毁槽快照（按 idleSince 升序，LRU 淘汰用）。
func (p *domainPool) idleSlots(sessionID string) []*domainSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*domainSlot
	for _, s := range p.bySess[sessionID] {
		s.mu.Lock()
		if s.state == slotIdle {
			out = append(out, s)
		}
		s.mu.Unlock()
	}
	// 插入排序：idleSince 升序（最旧在前）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].idleSince.Before(out[j-1].idleSince); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// slots 返回 session 的全部非销毁槽快照（任意状态）。
func (p *domainPool) slots(sessionID string) []*domainSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*domainSlot
	for _, s := range p.bySess[sessionID] {
		s.mu.Lock()
		if s.state != slotDestroyed {
			out = append(out, s)
		}
		s.mu.Unlock()
	}
	return out
}

// rearmSlotActivity 重建槽的心跳监控条目：enterIdle/挂起收尾 Delete activity（巡检豁免）后，
// 唤醒路径（reuse-wake / resume / 出队缓冲任务）统一在 runDomainTask 入口重建，
// 保证执行期假死仍可被巡检发现。
func (d *Dispatcher) rearmSlotActivity(id string) {
	d.activity.Store(id, newEvidence())
}

// enterIdle 转入 Idle：记录 idleSince 并立即武装加权 TTL——任务完成（父已收到回传）
// 即开始销毁倒计时，不再无限期热存等用户下一条消息；复用/直连唤醒路径在唤醒时停表，
// 本轮完成回到这里按新权重重新武装满额（对话/复用刷新寿命）。心跳豁免=activity.Delete。
func (d *Dispatcher) enterIdle(s *domainSlot, summary string) {
	s.mu.Lock()
	if s.state != slotRunning {
		s.mu.Unlock()
		return
	}
	killFn := s.destroyFnLocked()
	s.mu.Unlock()

	// P0-2c 树先行：树翻 Idle 必须先于槽状态翻转——复用临界区（dispatchToIdleSlot/
	// WakeIdleWithMessage）持 s.mu 复核"槽 Idle ⇒ 树 Idle"并同临界区完成树 Wake，
	// 消除"槽已 Idle 树仍 Running"导致的 t.Wake 假失败窗口（旧顺序下该窗口会让
	// 合法复用被误拒或树卡 Idle）。
	if d.treeFn != nil {
		if t := d.treeFn(s.sessionID); t != nil {
			t.Idle(s.id, summary, killFn)
		}
	}

	s.mu.Lock()
	if s.state != slotRunning {
		// 窗口内槽被复用唤醒/销毁：树状态由该路径自纠（复用完成重新 enterIdle；
		// 销毁路径 Finish），此处不再翻转。
		s.mu.Unlock()
		return
	}
	s.state = slotIdle
	s.idleSince = time.Now()
	// T22 档位固化：进 Idle 时记录会话当前档位，隐式复用解析按它裁决
	//（回调未接线/会话不存在时空串=复用守卫放行）。
	if d.sessionGearFn != nil {
		s.gear = d.sessionGearFn(s.sessionID)
	}
	s.ttlArmed = false
	if s.ttlTimer != nil {
		s.ttlTimer.Stop()
		s.ttlTimer = nil
	}
	s.mu.Unlock()

	d.activity.Delete(s.id)
	d.armTTL(s)
	log.Printf("[subagent] IDLE: sub=%s domain=%s reuse=%d (hot-resident)", s.id, s.domain, s.reuseCount)
}

// destroyFnLocked 返回槽的取消闭包（须持 s.mu 调用），供树 SetCancel 绑定（Register 首绑、
// Idle 换绑）。语义随槽状态分流：
//   - Idle：投递 opDestroy 销毁（TTL/硬取消空闲槽）；
//   - Running 且任务 ctx 已绑定：直接 cancel 任务 ctx——supervisor 正在跑任务，opDestroy
//     只能排队等任务自然结束，既停不住执行又会在暂停落地后把槽销毁（2026-09-10 实证：
//     pause_agent 早于任务 ctx 绑定到达时，cancel 打空后 opDestroy 残留，park 即被销毁）；
//   - Running 且任务 ctx 未绑定（派发 Register 与 runDomainTask 建 ctx 之间的窗口）：
//     登记 cancelPending，任务入口即刻收口。
func (s *domainSlot) destroyFnLocked() context.CancelFunc {
	return func() {
		s.mu.Lock()
		if s.state == slotRunning {
			if s.cancelTask != nil {
				cancel := s.cancelTask
				s.mu.Unlock()
				cancel()
				return
			}
			s.cancelPending = true
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		select {
		case s.ops <- domainOp{kind: opDestroy}:
		default:
			// ops 满时丢弃：supervisor park 循环必消费，正常不达。
		}
	}
}

// gearOf 读取槽固化档位（mu 保护；enterIdle 写入）。
func (s *domainSlot) gearOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gear
}

// armTTL 武装加权倒计时（幂等：已武装不重复）。
func (d *Dispatcher) armTTL(s *domainSlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != slotIdle || s.ttlArmed {
		return
	}
	ttl := d.slotTTLLocked(s)
	s.ttlArmed = true
	s.ttlDeadline = time.Now().Add(ttl)
	slot := s
	s.ttlTimer = time.AfterFunc(ttl, func() {
		slot.ops <- domainOp{kind: opDestroy, byTTL: true}
	})
	log.Printf("[subagent] IDLE TTL armed: sub=%s ttl=%v reuse=%d", s.id, ttl, s.reuseCount)
}

// slotTTLLocked 计算当前加权 TTL（须持 s.mu 调用）。
func (d *Dispatcher) slotTTLLocked(s *domainSlot) time.Duration {
	ttl := d.hotCfg.BaseTTL + time.Duration(s.reuseCount)*d.hotCfg.ExtendPerReuse
	if ttl > d.hotCfg.MaxTTL {
		ttl = d.hotCfg.MaxTTL
	}
	return ttl
}

// ArmIdleTTLs 武装 session 全部 idle domain 的加权倒计时。
// enterIdle 已改为完成即武装，本接口保留兼容（armTTL 幂等，已武装不重复）。
func (d *Dispatcher) ArmIdleTTLs(sessionID string) {
	if !d.hotEnabled() || sessionID == "" {
		return
	}
	for _, s := range d.pool.slots(sessionID) {
		d.armTTL(s)
	}
}

// IdleRoster 返回 session 的可复用 domain 清单（实现 agent.IdleRosterProvider）：
// 池内热驻槽 + 树中 Status==Idle 但池内无槽的冷驻节点（TTL 到期销毁的可复活注册表
// 条目，Cold=true）——冷驻不可见则 MetaAgent 不会发起复用，冷恢复通路即成死路。
func (d *Dispatcher) IdleRoster(sessionID string) []agent.IdleDomainInfo {
	if !d.hotEnabled() || sessionID == "" {
		return nil
	}
	var out []agent.IdleDomainInfo
	for _, s := range d.pool.slots(sessionID) {
		s.mu.Lock()
		if s.state == slotDestroyed {
			s.mu.Unlock()
			continue
		}
		info := agent.IdleDomainInfo{
			AgentID:     s.id,
			Domain:      s.domain,
			ReuseCount:  s.reuseCount,
			Busy:        s.state == slotRunning,
			Resp:        truncateRunes(strings.TrimSpace(s.responsibility), 60),
		}
		if files := d.recentWrittenFiles(s.id, idleRosterFileWindow); len(files) > 0 {
			n := len(files)
			if n > 3 {
				n = 3
			}
			for _, f := range files[:n] {
				info.WrittenFiles = append(info.WrittenFiles, filepath.Base(f))
			}
		}
		if s.state == slotIdle {
			// LastTask/LastSummary 从最近 history 提取。
			info.LastSummary = truncateRunes(agent.LastAssistantText(s.history), 200)
		}
		if s.ttlArmed {
			if left := time.Until(s.ttlDeadline); left > 0 {
				info.IdleLeft = left
			}
		}
		// LastTask 取首个 user 消息（最近任务目标）。
		for i := len(s.history) - 1; i >= 0; i-- {
			if s.history[i].Role == "user" {
				info.LastTask = truncateRunes(s.history[i].Content, 100)
				break
			}
		}
		s.mu.Unlock()
		out = append(out, info)
	}
	// 冷驻条目：树节点 Status==Idle 但池内无槽（TTL 到期已销毁实例）。只取树元数据
	//（无 history 读取，保持清单廉价）；复用命中时 dispatchToIdleSlot 才冷恢复。
	if d.treeFn != nil {
		if t := d.treeFn(sessionID); t != nil {
			for _, n := range t.Snapshot() {
				if n.Role != "domain" || n.Status != orchestrator.StatusIdle {
					continue
				}
				if d.pool.slot(sessionID, n.ID) != nil {
					continue // 热驻槽已在上方列出
				}
				out = append(out, agent.IdleDomainInfo{
					AgentID:  n.ID,
					Domain:   n.Domain,
					LastTask: truncateRunes(n.Task, 100),
					Cold:     true,
				})
			}
		}
	}
	return out
}

// SlotAlive 报告指定 domain 的热驻槽是否存活（池内存在且未销毁）——实现
// agent.IdleRosterProvider 的扩展方法，ReactService.ListAgents 据此给树节点
// 实例补 Hot 字段（热驻=true；冷驻/无槽/热驻未开启=false）。
func (d *Dispatcher) SlotAlive(sessionID, agentID string) bool {
	if !d.hotEnabled() || sessionID == "" || agentID == "" {
		return false
	}
	s := d.pool.slot(sessionID, agentID)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != slotDestroyed
}

// runDomainSupervisor 热驻 domain 的常驻 goroutine 主循环：
// 执行任务 → 按结果分类收尾 → park（select ops / 已武装的 TTL 由 ops 到期投递）。
// 唯一退出路径：opDestroy（TTL 到期/硬取消/话题切换/失败收口/进程关闭）。
// 任务完成后优先出队缓冲任务（忙碌时复用派发入队的），无任务才 park。
func (d *Dispatcher) runDomainSupervisor(s *domainSlot, firstTask string, firstWallClock time.Duration, firstImages []tool.ResultImage) {
	task, wc, imgs := firstTask, firstWallClock, firstImages
	for {
		outcome := d.runDomainTask(s, task, wc, imgs)
		switch outcome {
		case domainTaskDone, domainTaskStopped:
			// 成功/软停止：收尾已在 runDomainTask 内完成（notify + tree.Idle + trackChildDone + enterIdle）。
			// 优先出队缓冲任务（挂账已在入队时 trackChildStart；出队执行不再重复挂账）。
			if q, ok := s.dequeueNext(); ok {
				task, wc, imgs = q.task, q.wallClock, q.images
				// 唤醒槽（Idle→Running）继续执行队头任务。
				// P0-2f（2026-09-18 实测审查发现）：出队续跑必须同时解除 TTL 武装——
				// enterIdle 刚武装的倒计时原样遗留，任务执行期间 TTL 到期投 opDestroy，
				// 收尾 park 即销毁槽（队列剩余任务连带静默丢弃）。与 dispatchToIdleSlot
				// idle 分支的解除序列（停表+nil+ttlArmed=false）对齐。
				s.mu.Lock()
				s.state = slotRunning
				if s.ttlTimer != nil {
					s.ttlTimer.Stop()
					s.ttlTimer = nil
				}
				s.ttlArmed = false
				cancelRef := s.cancelTask
				s.mu.Unlock()
				if d.treeFn != nil {
					if t := d.treeFn(s.sessionID); t != nil {
						t.Wake(s.id, cancelRef)
					}
				}
				continue
			}
		case domainTaskSuspended:
			// 触限/挂起：SaveMessages + tree.Pause 已在 runDomainTask 内完成。
			// PendingChildren 保持 >0（不 trackChildDone），MetaAgent 检测 Paused 置会话暂停。
			// 等用户消息唤醒（ResumeSessionAgents 经 ops 发 opResume 续跑）。
		case domainTaskFailed, domainTaskDestroyed:
			// 失败：收尾已完成（salvage/notify + treeFinish Failed + trackChildDone）。
			// 销毁：清理已完成。
			d.destroySlot(s, "failed")
			return
		}

		// park：等下一条指令。TTL 到期也经 ops（AfterFunc 投 opDestroy）。
		op := <-s.ops
		switch op.kind {
		case opDestroy:
			// TTL 到期走冷驻语义（destroySlot 跳过树 Finish，节点保 Idle 可复活）。
			if op.byTTL {
				d.destroySlot(s, "ttl")
			} else {
				d.destroySlot(s, "destroy")
			}
			return
		case opResume:
			// 挂起唤醒："继续"续跑当前任务（history 在内存，budget 由 Assemble 独立轮估）。
			// 恢复轮不重复带图（不持久化语义）：历史里首条 user 消息已带过本任务的图。
			task, wc, imgs = op.resumeMsg, s.resumeWallClock(), nil
			// 任务未执行即 park（暂停早于任务启动）：原任务文本从未进入 history，
			// 用暂存的任务原文续跑而非"继续"。
			s.mu.Lock()
			if s.pendingTask != "" {
				task, wc, imgs = s.pendingTask, s.pendingWallClock, s.pendingImages
				s.pendingTask, s.pendingWallClock, s.pendingImages = "", 0, nil
			}
			s.mu.Unlock()
			// 挂起前树已 Pause，恢复置回 Running。
			s.mu.Lock()
			s.state = slotRunning
			cancelRef := s.cancelTask
			s.mu.Unlock()
			if d.treeFn != nil {
				if t := d.treeFn(s.sessionID); t != nil {
					t.Resume(s.id, cancelRef)
				}
			}
		case opNewTask:
			task, wc, imgs = op.task, op.wallClock, op.images
			// 唤醒后槽回到 Running（enterIdle 的前置检查依赖）。
			s.mu.Lock()
			s.state = slotRunning
			s.mu.Unlock()
		}
	}
}

// resumeWallClock 返回挂起前冻结的剩余墙钟（无则 0=不限制）。
func (s *domainSlot) resumeWallClock() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.wallRemain
	s.wallRemain = 0
	return r
}

// destroySlot 清理槽资源：树终态由调用路径决定（TTL 到期=冷驻，节点保 Idle 作可复活
// 注册表条目；硬取消=已 Cancelled；失败=已 Failed）。队列中已挂账任务补偿递减
//（PendingChildren 对称性）。
func (d *Dispatcher) destroySlot(s *domainSlot, reason string) {
	s.mu.Lock()
	if s.state == slotDestroyed {
		s.mu.Unlock()
		return
	}
	s.state = slotDestroyed
	queued := s.taskQueue
	s.taskQueue = nil
	if s.ttlTimer != nil {
		s.ttlTimer.Stop()
		s.ttlTimer = nil
	}
	if s.wallTimer != nil {
		s.wallTimer.Stop()
		s.wallTimer = nil
	}
	childPending := !s.childReported // 执行中任务尚未递减父未决计数（外部硬取消等静默路径）
	s.childReported = true
	s.mu.Unlock()

	// 兜底：执行中任务未经正常收口（成功/失败/墙钟分支都会递减）即被销毁时，
	// 此处补偿递减 + 回告父——否则父终结保护 wait loop 永久空等（同队列补偿语义）。
	// 必须在 subMeta.Delete 之前做：递减经 subMeta.doneOnce 与巡检 kill 的兜底互斥。
	if childPending {
		d.trackChildDoneOnce(s)
		d.notify(s.parentID, s.id, "子 Agent 被强制销毁（硬取消），任务未回传；父未决计数已兜底递减。", nil)
	}
	d.pool.remove(s.sessionID, s.id)
	d.running.Delete(s.id)
	d.activity.Delete(s.id)
	d.subMeta.Delete(s.id)
	d.lastWrites.Delete(s.id)
	d.heldSkills.Delete(s.id)
	if d.mailbox != nil {
		d.purgeMailboxWithNotice(s.id, "热驻槽已销毁")
	}
	// 队列中已 trackChildStart 的任务补偿递减（销毁时永不执行）。
	for range queued {
		d.trackChildDone(s.parentID)
	}
	// P0-2c 排空滞留指令：复用派发（dispatchToIdleSlot/WakeIdleWithMessage）与销毁竞态时，
	// opNewTask 可能已入通道但排在在飞 opDestroy（TTL AfterFunc/destroyFnLocked 投递）
	// 之后——supervisor 消费销毁指令后退出，通道永不再被读取，任务静默丢失且父未决
	// 计数挂账永不递减。此处逐项补偿递减并回告父 Agent。
	drainedNewTasks := 0
	drainLoop:
	for {
		select {
		case op := <-s.ops:
			if op.kind == opNewTask {
				drainedNewTasks++
			}
		default:
			break drainLoop
		}
	}
	for i := 0; i < drainedNewTasks; i++ {
		d.trackChildDone(s.parentID)
	}
	if drainedNewTasks > 0 {
		d.notify(s.parentID, s.id, fmt.Sprintf("子 Agent 销毁时丢弃 %d 个竞态投递的复用任务（父未决计数已补偿递减），如需继续请重新派发。", drainedNewTasks), nil)
	}
	// 树收尾：TTL 到期=冷驻——跳过 Finish，节点保持 Idle 作可复活注册表条目
	//（reuse_agent_id 命中时 restoreColdSlot 从 agent_messages 重建槽续跑）；
	// 其余路径（失败已 Failed/硬取消已 Cancelled）Finish 幂等 no-op，调用兜底。
	if reason != "ttl" && d.treeFn != nil && s.sessionID != "" {
		if t := d.treeFn(s.sessionID); t != nil {
			t.Finish(s.id, "idle expired (TTL)", nil)
		}
	}
	// 槽终结即回收实例级模型覆盖（暂停 park 不销毁槽，覆盖跨 resume 存活）。
	d.clearAgentModel(s.id)
	log.Printf("[subagent] SLOT DESTROYED: sub=%s domain=%s reason=%s reuse=%d", s.id, s.domain, reason, s.reuseCount)
}

// domainTaskOutcome 是 runDomainTask 的结果分类。
type domainTaskOutcome int

const (
	domainTaskDone      domainTaskOutcome = iota // 成功完成（已进 Idle）
	domainTaskSuspended                          // 触限/会话挂起后 Pause（等唤醒）
	domainTaskStopped                            // 用户软停止（已进 Idle）
	domainTaskFailed                             // 失败（销毁）
	domainTaskDestroyed                          // 硬取消（销毁）
)

// runDomainTask 执行单个 domain 任务：构造/复用 ReActAgent、前缀注入、驱动引擎、
// 按结果分类收尾（含 notify 父 + 树状态 + trackChildDone + 记忆沉淀）。
// images 为本轮用户图片（Alt+V 粘贴，带外穿透）：注入 taskCtx 挂到本任务首条
// user 消息，并随 domain 自身的工具执行 ctx 递归穿透给其派发的叶子 Agent。
func (d *Dispatcher) runDomainTask(s *domainSlot, task string, wallClock time.Duration, images []tool.ResultImage) domainTaskOutcome {
	// 每任务 ctx：stopCtx 基底（TODO 第10④，会话 Stop 即刻取消执行中任务；
	// 缺省回退 Background）+ sessionID；墙钟由 slot timer 驱动（可挂起停表），
	// 过闸（出队）才挂载——并发池排队不计预算。
	ctx := context.Background()
	if s.stopCtx != nil {
		ctx = s.stopCtx
	}
	if s.sessionID != "" {
		ctx = tool.WithSessionID(ctx, s.sessionID)
	}
	// 每会话工作目录（S2）：ctx 由 Background 重建，从槽位捕获值显式重注入（空 no-op）。
	ctx = tool.WithWorkDir(ctx, s.workDir)
	if len(images) > 0 {
		ctx = agent.WithUserImages(ctx, images)
	}
	taskCtx, cancelTask := context.WithCancel(ctx)
	defer cancelTask()

	s.mu.Lock()
	s.taskCtx = taskCtx
	s.cancelTask = cancelTask
	s.childReported = false // 新任务重置：上一任务的递减标记不得污染 destroySlot 兜底判定
	s.mu.Unlock()

	// 巡检兜底换绑（2026-08-26 panic 修复）：dispatchHotDomain 注册的 subMeta.cancel 为 nil
	//（任务 ctx 彼时尚未创建），此处每任务换绑真实 cancel--假死 kill 时巡检才能真正取消
	// 执行中的任务 ctx（ctx 取消 -> 下方 Canceled 分支 -> domainTaskDestroyed 销毁槽）。
	// 换绑 = Store 新实例（subAgentMeta 字段无锁被巡检并发读，禁止原地改写）；上一任务的
	// cancel 已由其 defer 触发，巡检拿到旧实例再调用是无害 no-op。
	if v, ok := d.subMeta.Load(s.id); ok {
		if m, ok2 := v.(*subAgentMeta); ok2 {
			d.subMeta.Store(s.id, &subAgentMeta{cancel: cancelTask, parentID: m.parentID, sessionID: m.sessionID, wallClock: wallClock})
		}
	}
	// 活动重建：enterIdle/挂起收尾会 Delete activity（巡检豁免），唤醒路径（reuse-wake/
	// resume/出队缓冲任务）统一在任务入口重建，执行期假死仍可被巡检发现
	//（此前唤醒分支只 Load 刷新，enterIdle Delete 后必落空--复用任务心跳永久失明）。
	d.rearmSlotActivity(s.id)

	// 排队不计墙钟（P1"出队才计墙钟"在热驻路径的补齐）：入口不再挂 timer——并发池
	// 排队期它会把等待时长烧进预算；排队超预算时 timer 更会在 runDomainEngine 的 gate
	// 排队中触发 cancelTask -> Acquire 出局 -> wallFired 分支，给从未执行过一轮的任务
	// 误报墙钟失败。此处仅停残留 timer + 重置触发标志（保留原 armWallClock 头部的
	// 入场 bookkeeping：gate-abort/外部取消的 Canceled 分类依赖 wallFired=false）；
	// timer 改由 runDomainEngine 过闸（出队）成功后 armWallClock 挂载，任务以全额预算
	// 起跑。有效值解析（0 回退 d.timeout）随 arm 一并后移——pendingWallClock 暂存原值，
	// 恢复时经 armWallClock 同样解析，口径等价。
	resetSlotWallClock(s)

	// 树节点保持 Running + 绑任务 cancel（Wake 已由派发方完成；首任务 Register 时已绑）。
	if d.treeFn != nil {
		if t := d.treeFn(s.sessionID); t != nil {
			t.SetCancel(s.id, cancelTask)
		}
	}

	// 早到指令收口：Register 与建 ctx 之间到达的取消/暂停（destroyFn 无 ctx 可 cancel，
	// 登记 cancelPending）——任务不进入执行，按暂停标记/硬取消分流。
	s.mu.Lock()
	pending := s.cancelPending
	s.cancelPending = false
	s.mu.Unlock()
	if pending {
		if d.isPauseRequested(s.id) {
			d.ClearPauseNode(s.id)
			// 任务尚未执行：暂存任务文本，resume 时原样续跑（无 history，"继续"会丢任务）。
			s.mu.Lock()
			s.suspended = true
			s.pendingTask = task
			s.pendingWallClock = wallClock
			s.pendingImages = images
			s.mu.Unlock()
			if d.treeFn != nil && s.sessionID != "" {
				if t := d.treeFn(s.sessionID); t != nil {
					t.Pause(s.id, "manual pause")
				}
			}
			d.activity.Delete(s.id)
			log.Printf("[subagent] MANUAL-PAUSED: sub=%s domain=%s (pre-execution, awaiting resume)", s.id, s.domain)
			return domainTaskSuspended
		}
		log.Printf("[subagent] CANCELLED: sub=%s domain=%s (pre-execution)", s.id, s.domain)
		return domainTaskDestroyed
	}

	// 会话级 logger 挂 ctx（同 runSubAgent）。
	if d.log != nil && s.sessionID != "" {
		ctx = logger.NewContext(ctx, d.log.WithSession(s.sessionID).WithAgent(s.agentName()))
	}

	started := time.Now()
	result, err := d.runDomainEngine(s, taskCtx, cancelTask, task, wallClock)
	files := agent.FilesModifiedFromHistory(result.History)
	duration := time.Since(started)

	// 任务终结统一停表：残留 wallTimer 会在任务完结（含 DONE 转 Idle 热驻）后空放触发
	//（2026-08-28 实证：01:20 DONE 的 domain-2 在 02:38 被残留 timer 打出 WALL CLOCK）。
	// 挂起分支的冻结剩余逻辑本就不依赖 timer（任务 ctx 无 deadline，wallRemain 恒 0），不受影响。
	stopWallClock(s)

	if errors.Is(err, errPaused) {
		log.Printf("[subagent] PAUSED: sub=%s domain=%s duration=%s (token budget, hot-resident awaiting resume)", s.id, s.domain, duration)
		// 触限挂起：SaveMessages 安全网 + tree.Pause（可恢复语义，MetaAgent 检测链零改动）。
		d.saveSlotMessages(s, result.History)
		if d.treeFn != nil && s.sessionID != "" {
			if t := d.treeFn(s.sessionID); t != nil {
				t.Pause(s.id, "token budget exhausted")
			}
		}
		// 冻结墙钟剩余（恢复时重挂）。
		s.mu.Lock()
		if s.wallTimer != nil {
			s.wallTimer.Stop()
			s.wallTimer = nil
			if s.taskCtx != nil {
				if dl, ok := s.taskCtx.Deadline(); ok {
					if r := time.Until(dl); r > 0 {
						s.wallRemain = r
					}
				}
			}
		}
		s.suspended = true
		s.mu.Unlock()
		// 挂起期间心跳豁免：等用户"继续"可远超心跳阈值（2026-08-26 实证 panic 链：
		// 挂起槽的 activity 残留被巡检判假死 -> killStuckSubAgent）。恢复执行时
		// runDomainTask 入口重建。
		d.activity.Delete(s.id)
		// 挂起全树（叶子经 SuspendGate 在下个检查点 park；在飞的跑完或完成回灌）。
		d.SuspendSession(s.sessionID)
		// P0-2b：父会话若正处于挂起等子（awaiting_child），暂停事件无任何唤醒源
		// （无 notify/无递减）——显式唤醒让父走 PausedChildChecker 转 paused_on_child。
		d.pokeParent(s.parentID)
		d.wakeSuspendedParent(s.parentID, "【系统】子 Agent "+s.id+" 触达 token 上限已暂停，请检测并处置（可换模型续跑或 cancel 放弃）。")
		return domainTaskSuspended
	}
	if errors.Is(err, context.Canceled) {
		sid := tool.SessionIDFromContext(taskCtx)
		if sid != "" && d.isSoftStop(sid) {
			// 用户软停止：SaveMessages 安全网 + 转 Idle（可续跑可复用）。
			d.saveSlotMessages(s, result.History)
			partial := truncateRunes(agent.LastAssistantText(result.History), 500)
			d.boardUpdate(taskCtx, s.parentID, s.domain, board.TaskFailed, truncateRunes(partial, 300))
			d.notifyTerminal(s.id, s.parentID, "子 Agent 已被用户停止，当前任务中断；成果已保留，热驻待复用。\n"+partial, files)
			d.trackChildDoneOnce(s)
			d.enterIdle(s, "user stop: "+partial)
			log.Printf("[subagent] SOFT-STOP IDLE: sub=%s domain=%s duration=%s", s.id, s.domain, duration)
			return domainTaskStopped
		}
		// 墙钟到期：armWallClock 的 timer cancel 到这里与外部硬取消同形（context.Canceled），
		// 用 wallFired 区分。墙钟走失败收口（翻看板 + treeFinish + notify 父 + 递减未决计数）
		// 而非硬取消的静默销毁——静默销毁不减 PendingChildren、邮箱无消息，父终结保护
		// wait loop 永久空等（2026-08-28 实证 domain-3 超时销毁后 MetaAgent 卡死"等待回传"）。
		s.mu.Lock()
		wallFired := s.wallFired
		s.mu.Unlock()
		if wallFired {
			d.wallClockWrapUp(s, taskCtx, result, files, duration)
			return domainTaskDestroyed
		}
		// 手动单节点暂停（TODO 第9⑥/10③ 审计面）：PauseAgent 标记后 StopRunning 到这里。
		// 同触限挂起收尾（SaveMessages + tree.Pause + 心跳豁免），槽保留 park 等唤醒
		//（既有 resume/继续通路可续）；不 SuspendSession——手动暂停仅针对该 domain，
		// 会话其余任务照跑。
		if d.isPauseRequested(s.id) {
			d.ClearPauseNode(s.id)
			d.saveSlotMessages(s, result.History)
			if d.treeFn != nil && s.sessionID != "" {
				if t := d.treeFn(s.sessionID); t != nil {
					t.Pause(s.id, "manual pause")
				}
			}
			s.mu.Lock()
			s.suspended = true
			s.mu.Unlock()
			d.activity.Delete(s.id)
			// P0-2b：同触限挂起路径——父若挂起等子需显式唤醒当轮处置。
			d.pokeParent(s.parentID)
			d.wakeSuspendedParent(s.parentID, "【系统】子 Agent "+s.id+" 已按指令暂停，请查收邮箱处置。")
			log.Printf("[subagent] MANUAL-PAUSED: sub=%s domain=%s duration=%s (hot-resident awaiting resume)", s.id, s.domain, duration)
			return domainTaskSuspended
		}
		// 硬取消：树状态由取消方（Tree.Cancel）已置 Cancelled，此处仅销毁槽
		//（父未决计数由 destroySlot 兜底递减）。
		log.Printf("[subagent] CANCELLED: sub=%s domain=%s duration=%s", s.id, s.domain, duration)
		return domainTaskDestroyed
	}
	if err != nil {
		partial := ""
		if result.History != nil {
			partial = truncateRunes(agent.LastAssistantText(result.History), 500)
		}
		log.Printf("[subagent] FAIL: sub=%s domain=%s duration=%s err=%v partial=%q", s.id, s.domain, duration, err, truncateRunes(partial, 200))
		salvage := d.salvageFailure(taskCtx, s.parentID, s.id, s.slotRoleDef(), s.domain, result, partial)
		kind := failureKindOf(taskCtx, err)
		// 双调修复（TODO #24 批二②）：failText 一次计算复用，此前 888/900 两行各调一次。
		failText := formatSubAgentFailure(taskCtx, err, result, d.effectiveTimeout(s.id), partial)
		msg := hotResidentFailureNotice(kind, failText, salvage)
		// 状态语义三态化：缺验证证据非失败——树落 delivered-unverified、看板标黄。
		boardSt, treeStatus := failureStatusTri(kind)
		d.boardUpdate(taskCtx, s.parentID, s.domain, boardSt, truncateRunes(msg, 300))
		d.treeFinishStatus(taskCtx, s.id, partial, treeStatus, failText)
		d.notifyTerminal(s.id, s.parentID, msg, files)
		d.trackChildDoneOnce(s)
		return domainTaskFailed
	}

	// 成功：boardUpdate + tree.Idle + saveBlockMemory + notify + trackChildDone + 进 Idle。
	log.Printf("[subagent] DONE: sub=%s domain=%s duration=%s result_len=%d", s.id, s.domain, duration, len(result.Text))
	// 终态全量落 PG（编排页对话视图权威源）：热驻槽同样适用——否则任务完成后
	// 对话页只剩热层（24h 后过期即空白），与暂停/软停分支的 saveSlotMessages 口径不一致。
	d.saveSlotMessages(s, result.History)
	d.boardUpdate(taskCtx, s.parentID, s.domain, board.TaskDone, result.Text)
	summary := result.Text
	if result.VerifyNote != "" {
		summary = fmt.Sprintf("【校验:通过(%s)】\n%s", result.VerifyNote, result.Text)
	}
	d.saveBlockMemory(taskCtx, s.id, "domain", s.parentID, s.domain, s.lastTaskGoal(), result.Text, blockOutcomeSuccess, files, result.History)
	d.notifyTerminal(s.id, s.parentID, summary, files)
	d.trackChildDoneOnce(s)
	d.enterIdle(s, summary)
	return domainTaskDone
}

// trackChildDoneOnce 热驻 domain 任务的父未决计数递减入口：经 subMeta.doneOnce 与
// 巡检 kill 的兜底递减互斥（防双递减），并标记槽 childReported 供 destroySlot 兜底判定。
// subMeta 缺失（极端时序）回退直接递减。
func (d *Dispatcher) trackChildDoneOnce(s *domainSlot) {
	if v, ok := d.subMeta.Load(s.id); ok {
		if m, ok2 := v.(*subAgentMeta); ok2 {
			m.doneOnce.Do(func() { d.trackChildDone(s.parentID) })
			s.mu.Lock()
			s.childReported = true
			s.mu.Unlock()
			return
		}
	}
	d.trackChildDone(s.parentID)
	s.mu.Lock()
	s.childReported = true
	s.mu.Unlock()
}

// wallClockWrapUp 热驻 domain 墙钟到期的失败收口：翻看板 + 树终态 Failed + notify 父 +
// 递减父未决计数，附部分产出与失败打捞。此前墙钟取消与外部硬取消共用静默销毁路径，
// 父 Agent 永远等不到回执（2026-08-28 实证 domain-3 撞 2h 墙钟后对话栏卡死）。
// 收口后槽仍走销毁（任务 ctx 已死不可续），父收到超时回执可自行决定重派/收口。
func (d *Dispatcher) wallClockWrapUp(s *domainSlot, taskCtx context.Context, result agent.ReactResult, files []string, duration time.Duration) {
	partial := ""
	if result.History != nil {
		partial = truncateRunes(agent.LastAssistantText(result.History), 500)
	}
	budget := d.effectiveTimeout(s.id)
	msg := failureMarker(FailureKindTimeout, false) + "\n" +
		fmt.Sprintf("子 Agent 墙钟预算耗尽（上限 %v，已执行 %v），已被强制收口；产出未完成，请重派或基于已有成果收口。%s",
			budget, duration, partialSuffix(partial))
	if salvage := d.salvageFailure(taskCtx, s.parentID, s.id, s.slotRoleDef(), s.domain, result, partial); salvage != "" {
		msg += "\n\n" + salvagePrefixMarker + salvage
	}
	d.boardUpdate(taskCtx, s.parentID, s.domain, board.TaskFailed, truncateRunes(msg, 300))
	d.treeFinishStatus(taskCtx, s.id, partial, orchestrator.StatusFailed, fmt.Sprintf("wall clock budget %v exhausted", budget))
	d.notifyTerminal(s.id, s.parentID, msg, files)
	d.trackChildDoneOnce(s)
	log.Printf("[subagent] WALL CLOCK WRAP-UP: sub=%s domain=%s duration=%s budget=%v (parent notified, pending decremented)",
		s.id, s.domain, duration, budget)
}

// runDomainEngine 构造/复用 ReActAgent 并驱动引擎。
// ReActAgent 实例跨任务复用（存 s.agent）：systemPrompt 含冻结 responsibility 头；
// 每次任务前重挂 running。实例新建（freshBuild）且槽内带 history 时（冷恢复重建槽：
// dormant 唤醒/会话 resume，history 来自 agent_messages）以 RunWithHistory 携带种子
// 续上下文（同 ResumePaused 的 msgs 续跑口径）；热驻复用与全新首派走原 runEngine 路径。
func (d *Dispatcher) runDomainEngine(s *domainSlot, taskCtx context.Context, cancelTask context.CancelFunc, task string, wallClock time.Duration) (agent.ReactResult, error) {
	s.mu.Lock()
	agentInst := s.agent
	s.mu.Unlock()

	freshBuild := false
	if agentInst == nil {
		sub, err := d.buildDomainAgent(s)
		if err != nil {
			return agent.ReactResult{}, err
		}
		s.mu.Lock()
		s.agent = sub
		s.mu.Unlock()
		agentInst = sub
		freshBuild = true
	}
	d.running.Store(s.id, agentInst)

	// 并发池准入（P1）：热驻槽只在真正跑任务时占名额，park/idle 不占。
	// 墙钟传 0——槽任务墙钟由 slot timer 管理（dispatchOne :3317-3318 注释），
	// 且过闸（出队）后才挂载：排队不烧预算（下方 armWallClock）。
	ev := d.activityEvidenceFor(s.id)
	runCtx, runCancel, release, gateErr := d.enterExecGate(taskCtx, ev, s.id, 0)
	if gateErr != nil {
		// gate-abort 无 timer 可残留（任务入口仅 reset 未 arm）；runDomainTask 的
		// Canceled 分类读 wallFired=false，外部取消（软停止/暂停/硬取消）语义保持正确。
		d.running.Delete(s.id)
		return agent.ReactResult{}, gateErr
	}
	defer runCancel()
	defer release()

	// 出队此刻才挂槽任务墙钟：任务以全额预算起跑，排队时长不计入（任务入口
	// resetSlotWallClock 已停残留并复位标志；armWallClock 内部再停一次为幂等兜底）。
	// 预警阶梯同步从执行起点计时。runCtx==taskCtx（gate 墙钟传 0），timer 到期
	// cancelTask 即断引擎，走 runDomainTask 的 wallFired 收口。
	d.armWallClock(s, taskCtx, cancelTask, wallClock)

	var result agent.ReactResult
	var err error
	if freshBuild {
		s.mu.Lock()
		seed := s.history
		s.mu.Unlock()
		if len(seed) > 0 {
			result, err = agentInst.RunWithHistory(runCtx, task, seed)
		} else {
			result, err = d.runEngine(runCtx, agentInst, s.id, "domain", "", "", task)
		}
	} else {
		result, err = d.runEngine(runCtx, agentInst, s.id, "domain", "", "", task)
	}
	if err != nil {
		return result, err
	}

	// 保存终态 history（Idle/挂起安全网 + 复用续跑种子）。
	s.mu.Lock()
	s.history = result.History
	s.mu.Unlock()
	d.running.Delete(s.id)

	if result.LimitReached {
		return result, errPaused
	}
	return result, nil
}

// buildDomainAgent 构造热驻 domain 的 ReActAgent（首任务一次）：
// 复用 runSubAgentOnce 装配链（roleDef 覆写/responsibility 头/uptake/live 转发），
// 额外注入 SuspendGate。
func (d *Dispatcher) buildDomainAgent(s *domainSlot) (*agent.ReActAgent, error) {
	roleDef := d.registry.Get("domain")
	if roleDef == nil {
		return nil, fmt.Errorf("domain role not found")
	}
	if n := domainAgentDisplayName(s.domain, ""); n != "" {
		roleDef.Name = n
	}
	// responsibility 头注入（同 runSubAgentOnce，冻结进实例 systemPrompt）。
	if header := domainResponsibilityHeader(s.domain, s.responsibility); header != "" {
		roleDef.SystemPrompt = roleDef.SystemPrompt + "\n\n" + header
	}
	// 运行时身份块注入（同 runSubAgentOnce，2026-09-21）：槽创建时点（首派）的兄弟名册
	// 冻结进系统提示词；后续复用时的兄弟变化经 mailbox 消息 From 头兜底（提示词已注明）。
	roleDef.SystemPrompt = roleDef.SystemPrompt + "\n\n" +
		d.runtimeIdentityBlock(s.sessionID, parentIDOfAgentID(s.id), s.id)

	// 首次解析仅作建槽期 fail-fast 与初值；WithProviderFunc 使槽存活期内每次 LLM
	// 调用按当前绑定重解析——否则 set_role_model/TUI 切换对热驻槽永久不可见。
	// 解析走实例级（agent 覆盖 > 角色绑定）：set_agent_model 对该槽换档后下次调用即生效。
	provider, err := d.providerForAgent(context.Background(), "domain", s.id)
	if err != nil {
		return nil, fmt.Errorf("get model: %w", err)
	}
	providerFn := func(ctx context.Context) (agent.ModelProvider, error) {
		return d.providerForAgent(ctx, "domain", s.id)
	}
	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}
	// 拓扑名册实时刷新（2026-09-21 可见性矩阵，热驻路径补齐）：槽的系统提示词创建时
	// 冻结（身份块首注快照），复用续作不再重注——每轮【拓扑名册更新】是本路径唯一的
	// 拓扑更新通道。seeded=创建时点名册，此后每轮 diff（任何变化注入全量当前名册）。
	// 黑板摄取（兄弟产出）此处不挂——热驻路径该缺口为旧账（仅非热驻路径），
	// 本轮只补名册刷新，不扩散行为面。
	{
		liveRoster := func() []rosterEntry {
			out := d.rosterEntries(s.sessionID, parentIDOfAgentID(s.id), s.id)
			out = append(out, d.childEntries(s.sessionID, s.id)...)
			return out
		}
		seeded := map[string]bool{}
		for _, e := range liveRoster() {
			seeded[e.id] = true
		}
		mem = newSiblingUptakePipeline(mem, nil, s.sessionID, parentIDOfAgentID(s.id), s.domain, s.id).
			WithRoster(liveRoster, seeded)
	}
	// 系统提示词工作目录按会话解析(终审修复,同 dispatcher 派发路径):槽创建时已从派发 ctx
	// 捕获会话 workDir,优先使用;空串回落工具注册表默认目录。
	wd := s.workDir
	if wd == "" {
		wd = d.subAgentWorkDir()
	}
	sub := agent.NewReActAgent(s.id, *roleDef, provider, agent.NewToolRegistryAdapterForRole(d.tools, s.id, roleDef.Tools, roleDef.ID, d.pluginVisibility)).
		WithProviderFunc(providerFn).
		WithMailbox(d.mailbox).
		WithMemory(mem).
		WithLoopConfig(d.loopConfigFor("domain")).
		WithWorkDir(wd).
		WithSkillBlock(d.skillBlockFor(s.id, roleDef)).
		WithPendingChildrenChecker(d).
		WithSuspendGate(&slotSuspendGate{d: d, sid: s.sessionID})

	// 心跳检活：同 runSubAgentOnce（dispatcher.go）注入活动上报回调。此前热驻 domain
	// 漏注入，touchActivity 全程 no-op（工具 30s 保活 tick / 流式 delta 均失效），
	// activity 只在任务入口刷新一次——domain 直接执行任何 >10min 长工具即被巡检误判
	// 假死 killed（2026-08-27 实证：3 个 domain 死于验证阶段长工具执行中）。
	// Load-per-call 而非捕获指针：enterIdle/挂起收尾会 Delete activity、
	// rearmSlotActivity 每任务重建新证据条目，闭包捕获旧指针会写进已废弃条目。
	sub = sub.WithActivityReporter(d.activityReporterFn(s.id))
	// 消息热层（编排页对话视图）：热驻 domain 是 config 默认主路径（domain_hot_resident_enabled
	// 默认 true），漏注入会让用户在编排页面对最常看的 DomainAgent 看到空白对话。
	if d.msgLogger != nil {
		sub = sub.WithMessageLogger(d.msgLogger)
	}
	if d.log != nil {
		sub = sub.WithLogger(d.log.WithSession(s.sessionID).WithAgent(roleDef.Name))
	}
	if d.liveFn != nil && s.sessionID != "" {
		forwarder := d.liveFn
		sid := s.sessionID
		sub = sub.WithLiveEvents(func(ev agent.LiveEvent) {
			d.recordFileWrite(s.id, ev)
			d.recordRecentActivity(s.id, ev)
			forwarder(sid, ev)
		})
	}
	// 心跳：domain 注册活动证据（等子期间靠后代冒泡保活，同 runSubAgentOnce 语义）。
	d.activity.Store(s.id, newEvidence())
	return sub, nil
}

// armWallClock 启动 slot 墙钟 timer：到期 cancelTask（软停止/硬收口由任务 ctx 分支处理）。
// 由 runDomainEngine 在并发池过闸（出队）成功后调用——排队不计墙钟，任务以全额预算起跑。
// 返回有效墙钟（0=不限制）。同时挂 50%/75%/90% 递进预警（wallClockWarnLadder）——此前热驻
// domain 完全没有预警（仅到期杀），而 config 默认 domain_hot_resident_enabled=true 即主路径，
// 渲染领域 Agent 两小时零预警撞墙即此路径（2026-08-28 实证）。taskCtx 结束预警 goroutine 即退出。
func (d *Dispatcher) armWallClock(s *domainSlot, taskCtx context.Context, cancelTask context.CancelFunc, wallClock time.Duration) time.Duration {
	if wallClock <= 0 {
		wallClock = d.timeout
	}
	if wallClock <= 0 {
		return 0
	}
	s.mu.Lock()
	if s.wallTimer != nil {
		s.wallTimer.Stop()
	}
	s.wallFired = false // 新任务重置触发标志（上一任务的残留不得污染本任务判定）
	s.mu.Unlock()
	s.wallTimer = time.AfterFunc(wallClock, func() {
		// 先置标志再 cancel：runDomainTask 的 Canceled 分支靠 wallFired 区分
		// 墙钟到期（走失败收口回告父）与外部硬取消（静默销毁）。
		s.mu.Lock()
		s.wallFired = true
		s.mu.Unlock()
		log.Printf("[subagent] WALL CLOCK exceeded: sub=%s domain=%s budget=%v", s.id, s.domain, wallClock)
		cancelTask()
	})
	wallClockWarnLadder(d.mailbox, s.id, wallClock, taskCtx.Done())
	return wallClock
}

// stopWallClock 停掉当前任务的墙钟 timer（任务终结时调用）。不停表则残留 timer 会在
// 任务完结（含 DONE 转 Idle 热驻）后空放触发，误打 WALL CLOCK 日志并 cancel 已死 ctx
//（2026-08-28 实证：01:20 DONE 的 domain-2 在 02:38 被残留 timer 触发）。
func stopWallClock(s *domainSlot) {
	s.mu.Lock()
	if s.wallTimer != nil {
		s.wallTimer.Stop()
		s.wallTimer = nil
	}
	s.mu.Unlock()
}

// resetSlotWallClock 任务入场的墙钟 bookkeeping（不挂载 timer）：停残留 timer 并重置
// 触发标志——每任务以 wallFired=false 起步，gate-abort/外部取消的 Canceled 分类不受
// 上一任务残留污染。timer 挂载推迟到 runDomainEngine 过闸（出队）后（排队不计墙钟）。
func resetSlotWallClock(s *domainSlot) {
	s.mu.Lock()
	if s.wallTimer != nil {
		s.wallTimer.Stop()
		s.wallTimer = nil
	}
	s.wallFired = false
	s.mu.Unlock()
}

// saveSlotMessages 落盘 history 安全网（脱离已取消 ctx + 10s 超时，同软停止分支）。
func (d *Dispatcher) saveSlotMessages(s *domainSlot, history []agent.ReactMessage) {
	if d.msgStore == nil || s.sessionID == "" || history == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.msgStore.SaveMessages(ctx, s.id, s.sessionID, history); err != nil {
		log.Printf("[subagent] slot save messages failed: sub=%s err=%v", s.id, err)
	}
}

// agentName 返回槽的展示名。
func (s *domainSlot) agentName() string {
	if s.domain != "" {
		return truncateRunes(s.domain, 16) + "领域Agent"
	}
	return "DomainAgent"
}

// slotRoleDef 返回槽的冻结 roleDef（失败路径 salvageFailure 签名需要）。
func (s *domainSlot) slotRoleDef() types.RoleDefinition {
	rd := types.RoleDefinition{ID: "domain", Name: s.agentName()}
	return rd
}

// lastTaskGoal 返回最近任务目标（块记忆 goal 标签用）。
func (s *domainSlot) lastTaskGoal() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.history) - 1; i >= 0; i-- {
		if s.history[i].Role == "user" {
			return truncateRunes(s.history[i].Content, 200)
		}
	}
	return s.domain
}

// SuspendSession 挂起 session 全部子 Agent（触限暂停波及全树）：
// 置挂起态后，叶子与 domain 的 SuspendGate.Park 在下个检查点阻塞；
// 在飞 LLM/工具调用跑完（有界超时）自然到检查点。Idle domain 的 TTL 冻结。
func (d *Dispatcher) SuspendSession(sessionID string) {
	if sessionID == "" {
		return
	}
	st := d.suspendState(sessionID)
	if st == nil {
		return
	}
	st.suspend()
	// 冻结 idle 槽的 TTL（挂起期间不倒计时）。
	if d.hotEnabled() {
		for _, s := range d.pool.slots(sessionID) {
			s.mu.Lock()
			if s.state == slotIdle && s.ttlTimer != nil {
				s.ttlTimer.Stop()
				s.ttlTimer = nil
				// 记剩余量，恢复时重挂。
				s.ttlDeadline = time.Now() // 暂以当前时间占位，resume 重算
				// P0-2g（2026-09-18 实测审查发现）：冻结必须连 ttlArmed 一起复位——
				// 原实现只停表，armTTL 的"已武装"幂等守卫令 ResumeSessionAgents
				// 的重挂调用永远被拒，槽退出 TTL 治理永久驻留。
				s.ttlArmed = false
			}
			s.mu.Unlock()
		}
	}
	log.Printf("[subagent] SESSION SUSPENDED: session=%s (all agents parking)", sessionID)
}

// RestoreSessionDomains 会话恢复中间层（TODO #21④）：进程重启后 resume 会话时，
// 树中未终态（Paused/Idle/Running）或终态不久（Done/Failed/Cancelled/Unverified 且
// finished 在 30min 内）的 domain 实例，按 (sessionID, agentID) 从 agent_messages 取回
// history、树元数据经 TreeFor→LoadNodes 已恢复，重建热驻槽（state=Idle park 等复用/唤醒）。
// 三层连续体中间层：热驻池（分钟级 LRU）与领域档案（跨会话永久级，#17）之间的
// "会话 resume 级"实例+上下文恢复（KC 语义：恢复会话还原子 Agent 实例状态与上下文历史）。
// 边界（TODO#14 不破）：只还原落库数据已够的会话历史——agent_messages 无行不建空壳槽
// （冷复活走 #17 档案匹配），不新增持久化面；已在池的槽（同进程 resume 热驻未死）跳过。
// best-effort：单实例失败仅记日志。返回重建槽数。
func (d *Dispatcher) RestoreSessionDomains(sessionID string) int {
	if sessionID == "" || !d.hotEnabled() || d.treeFn == nil || d.msgStore == nil {
		return 0
	}
	t := d.treeFn(sessionID)
	if t == nil {
		return 0
	}
	const recentTerminalWindow = 30 * time.Minute
	now := time.Now()
	restored := 0
	for _, n := range t.Snapshot() {
		if n.Role != "domain" {
			continue
		}
		switch n.Status {
		case orchestrator.StatusRunning, orchestrator.StatusPaused, orchestrator.StatusIdle:
			// 未终态：实例+上下文都该回温。
		case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled, orchestrator.StatusUnverified:
			// 终态不久：仍可作热驻续建种子（复用上下文继续追问）；太久远交 #17 冷复活。
			if n.Finished.IsZero() || now.Sub(n.Finished) > recentTerminalWindow {
				continue
			}
		default:
			continue
		}
		s := d.restoreColdSlot(sessionID, n)
		if s == nil {
			continue
		}
		d.armTTL(s)
		restored++
		s.mu.Lock()
		msgN := len(s.history)
		s.mu.Unlock()
		log.Printf("[subagent] RESTORE DOMAIN SLOT: sub=%s domain=%s msgs=%d status=%s", n.ID, n.Domain, msgN, n.Status)
	}
	return restored
}

// restoreColdSlot 按树节点+agent_messages 重建冷驻 Idle 槽（TTL 冷驻/会话 resume 共用）：
// Role==domain、池内无槽、LoadMessages 非空才建（无 history 不建空壳，返回 nil）；
// 节点存在性/状态资格/终态时间窗由调用方过滤（RestoreSessionDomains 的未终态+
// 终态 30min 窗口；dispatchToIdleSlot 的冷驻 Idle 复用）。建槽后 pool.store +
// 树 SetCancel 绑销毁句柄，不 armTTL——由调用方决定（会话 resume 补 armTTL 保持原语义；
// 复用派发由紧接的唤醒序列接管）。
// 重建槽 dormant（无 supervisor 常驻 goroutine），首个唤醒序列惰性启动。
// 邮箱 Reopen：TTL 销毁路径已 Purge（closed 标记），不重开则唤醒后回传/直连死信。
func (d *Dispatcher) restoreColdSlot(sessionID string, n orchestrator.Node) *domainSlot {
	if sessionID == "" || n.ID == "" || !d.hotEnabled() || d.msgStore == nil {
		return nil
	}
	if n.Role != "domain" {
		return nil
	}
	if d.pool.slot(sessionID, n.ID) != nil {
		return nil // 热驻未死（同进程 resume），已有实例不重建
	}
	msgs, err := d.msgStore.LoadMessages(context.Background(), n.ID)
	if err != nil {
		log.Printf("[subagent] restore domain: load messages failed: sub=%s err=%v", n.ID, err)
		return nil
	}
	if len(msgs) == 0 {
		// 落库数据不够（无 history）：不建空壳槽，冷复活走 #17 档案匹配。
		return nil
	}
	s := &domainSlot{
		id:        n.ID,
		sessionID: sessionID,
		parentID:  n.ParentID,
		domain:    strings.TrimSpace(n.Domain),
		history:   msgs,
		state:     slotIdle,
		idleSince: time.Now(),
		dormant:   true,
		ops:       make(chan domainOp, 8),
	}
	d.pool.store(s)
	if d.treeFn != nil {
		if t := d.treeFn(sessionID); t != nil {
			// 树绑定销毁句柄（重建槽的 TTL/硬取消出口），与 dispatchHotDomain 首绑同语义。
			t.SetCancel(n.ID, s.destroyFnLocked())
		}
	}
	if d.mailbox != nil {
		d.mailbox.Reopen(n.ID)
	}
	return s
}

// ResumeSessionAgents 唤醒 session 全部挂起 Agent：
// close wake 广播 + Paused 树节点 Resume（绑槽任务 cancel）+ 恢复冻结的 idle TTL。
func (d *Dispatcher) ResumeSessionAgents(sessionID string) {
	if sessionID == "" {
		return
	}
	// 会话恢复中间层（TODO #21④）：进程重启后先重建未终态/终态不久 domain 槽，
	// 再走既有唤醒广播——重建槽同样被下方 armTTL/opResume 扫到，零特判。
	d.RestoreSessionDomains(sessionID)
	st := d.suspendState(sessionID)
	if st == nil {
		return
	}
	st.resume()

	if !d.hotEnabled() {
		return
	}
	for _, s := range d.pool.slots(sessionID) {
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if state == slotIdle {
			// 恢复冻结 TTL：重新武装满额加权寿命。
			d.armTTL(s)
		}
	}
	// 挂起中的 running 槽经 ops 发 opResume（"继续"续跑当前任务）。
	for _, s := range d.pool.slots(sessionID) {
		s.mu.Lock()
		suspended := s.suspended
		s.suspended = false
		s.mu.Unlock()
		if suspended {
			select {
			case s.ops <- domainOp{kind: opResume, resumeMsg: "继续"}:
			default:
			}
		}
	}
	log.Printf("[subagent] SESSION RESUMED: session=%s (all agents waking)", sessionID)
}

// DestroyAllIdle 销毁全部热驻槽（进程 Shutdown 优雅退出）。
func (d *Dispatcher) DestroyAllIdle() {
	if !d.hotEnabled() {
		return
	}
	d.pool.mu.Lock()
	var all []*domainSlot
	for sid := range d.pool.bySess {
		for _, s := range d.pool.bySess[sid] {
			all = append(all, s)
		}
	}
	d.pool.mu.Unlock()
	for _, s := range all {
		select {
		case s.ops <- domainOp{kind: opDestroy}:
		default:
		}
	}
}

// dispatchToIdleSlot 复用派发入口（call_sub_agent reuse_agent_id 参数）：
//   - 槽 idle：Wake（树 Idle→Running）+ opNewTask；reuseCount+1；TTL 重置满额。
//   - 槽 busy（running/挂起）：入 taskQueue（挂账 trackChildStart），当前任务完成后自动出队。
//   - 槽不存在但树节点为冷驻（Role==domain && Status==Idle）：restoreColdSlot 从
//     agent_messages 重建 dormant 槽，落入同一 idle 唤醒序列（惰性启动 supervisor）。
//   - 槽不存在且不可冷恢复/已销毁：返回错误提示新建。
// 返回 subAgentID 与错误结果。
func (d *Dispatcher) dispatchToIdleSlot(ctx context.Context, parentID, reuseAgentID, task string, wallClock time.Duration, mode, verifyKind string) (string, *tool.Result) {
	sessionID := sessionIDFromAgentID(parentID)
	s := d.pool.slot(sessionID, reuseAgentID)
	if s == nil && d.treeFn != nil {
		// 冷驻恢复（TTL 到期销毁后节点保 Idle 的可复活注册表条目）：池内无槽时
		// 查树节点，Role==domain && Status==Idle 且有 history 则重建槽。
		// 重建在持锁外完成，下方 idle 唤醒序列持锁复核状态，竞态安全。
		if t := d.treeFn(sessionID); t != nil {
			if n, ok := t.Get(reuseAgentID); ok && n.Role == "domain" && n.Status == orchestrator.StatusIdle {
				s = d.restoreColdSlot(sessionID, n)
				if s != nil {
					log.Printf("[subagent] COLD RESTORE: sub=%s domain=%s (reuse dispatch)", n.ID, n.Domain)
				}
			}
		}
	}
	if s == nil {
		return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s not found（已销毁或不存在），请用 role_id=domain 新建", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
	}
	// 校验槽归属（跨会话误用防御）。
	if tool.SessionIDFromContext(ctx) != "" && tool.SessionIDFromContext(ctx) != s.sessionID {
		return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s belongs to another session", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
	}

	// stopCtx 刷新（TODO 第10④）：会话恢复后 stopCtx 已重建（旧值随上次 Stop 取消），
	// 复用/入队前换绑本次 runCtx 携带的新基底，防新任务继承已取消 context 秒死。
	// 忙碌槽当前任务不受影响（其 taskCtx 已派生），仅作用于后续任务。
	if sc := tool.StopContextFrom(ctx); sc != nil {
		s.mu.Lock()
		s.stopCtx = sc
		s.mu.Unlock()
	}

	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	// 本轮用户图片（Alt+V 粘贴）：从父 ctx 带外取出，随任务捎带给该 domain
	//（本任务首条 user 消息挂图；忙碌入队则随 queuedTask 缓冲）。
	imgs := agent.UserImagesFromContext(ctx)

	// 忙碌入队闭包：slotIdle 竞态复核发现槽已转 Running 时与 default 分支同语义复用。
	// P0-2d（2026-09-18 集群档实测审查发现，严重）：挂账 trackChildStart 与入队必须
	// 同事务——原实现先挂账（s.mu 外）后入队，destroySlot 排空队列并补偿递减（s.mu 外，
	// idle_pool.go destroySlot 尾部）若先执行，本任务的补偿 -1 先于挂账 +1，父未决
	// 计数永久 +1（终结保护空等）。现整段持 s.mu：锁内复核状态（销毁即拒，不挂账）→
	// 容量复核 → 入队 → 同事务挂账。destroy 先持锁则见 destroyed 拒绝；后持锁则队列
	// 含本任务且挂账已完成，其补偿 -1 与本 +1 平账。
	enqueue := func() (string, *tool.Result) {
		taskText := d.buildReuseTask(ctx, s, task)
		s.mu.Lock()
		if s.state == slotDestroyed {
			s.mu.Unlock()
			return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s 已被销毁（TTL/取消竞态），请用 role_id=domain 新建", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
		}
		if len(s.taskQueue) >= d.hotCfg.TaskQueueLen {
			s.mu.Unlock()
			return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s 任务队列已满（%d），请稍后重派或新建", reuseAgentID, len(s.taskQueue)), Category: tool.ResultCategoryValidationRejected}
		}
		s.taskQueue = append(s.taskQueue, queuedTask{task: taskText, wallClock: wallClock, images: imgs})
		qlen := len(s.taskQueue)
		d.trackChildStart(parentID)
		s.mu.Unlock()
		log.Printf("[subagent] QUEUE: sub=%s domain=%s queued=%d (busy, will run after current task)", s.id, s.domain, qlen)
		// 任务台账登记：忙碌入队同样记"进行中"（备注队列位置），任务执行后由 notify 收口。
		d.ledger.RecordDispatch(s.sessionID, parentID, s.id, s.domain, task, fmt.Sprintf("入队第%d位", qlen))
		d.writeCheckpoint(s.id, checkpointPhaseReuse)
		return s.id + "（忙碌中，任务已入队，当前任务完成后执行）", nil
	}

	switch state {
	case slotDestroyed:
		return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s already destroyed，请用 role_id=domain 新建", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
	case slotIdle:
		// 唤醒：树 Idle→Running + reuseCount+1 + TTL 重置满额 + opNewTask。
		// P0-2c 竞态收口（整个分支持 s.mu）：读状态与投递 opNewTask 必须原子——否则槽可在
		// 期间被 TTL/硬取消销毁（opDestroy 先被 supervisor 消费 → 槽销毁退出 → opNewTask
		// 滞留死通道），父未决计数挂账永不递减、任务静默丢失。持锁投递为 non-blocking
		//（default 分支），不与 supervisor 的 s.mu 形成死锁；op 仍可能落在在飞 opDestroy
		// 之后，由 destroySlot 排空通道兜底补偿。
		s.mu.Lock()
		if s.state != slotIdle {
			// 读取状态后槽被销毁/转 Running：销毁拒绝；Running 落入队（同 busy 语义）。
			st := s.state
			s.mu.Unlock()
			if st == slotDestroyed {
				return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s 已被销毁（取消/TTL 到期竞态），请用 role_id=domain 新建", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
			}
			return enqueue()
		}
		s.reuseCount++
		if s.ttlTimer != nil {
			s.ttlTimer.Stop()
			s.ttlTimer = nil
		}
		s.ttlArmed = false
		// 任务文本注入前缀（共享记忆 + 召回），responsibility 用槽内冻结值。
		taskText := d.buildReuseTask(ctx, s, task)
		cancelRef := s.cancelTask
		// 树 Idle→Running 与槽状态复核同临界区（enterIdle 已改为树先行，"槽 Idle ⇒ 树
		// Idle"不变量成立）：Wake 失败=状态失步，拒绝并回滚 reuseCount，不投递 op。
		if d.treeFn != nil {
			if t := d.treeFn(s.sessionID); t != nil {
				if !t.Wake(s.id, cancelRef) {
					s.reuseCount--
					s.mu.Unlock()
					return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s 树节点非空闲（状态失步），请用 role_id=domain 新建", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
				}
			}
		}
		// P0-2d：未决计数与 opNewTask 投递必须同事务（同持 s.mu 先计数后投递）——
		// destroySlot 排空滞留 op 的补偿递减在 s.mu 外执行，若计数留在投递后（锁外），
		// 销毁可先补偿 -1 后计数 +1，父未决永久挂账。default 分支回滚计数并补偿递减。
		d.trackChildStart(parentID)
		// dormant（冷恢复重建槽）：无 supervisor 常驻 goroutine，opNewTask 无消费者——
		// 锁内置 Running + 清 dormant + 排空死信期滞留 op（RestoreSessionDomains 重建槽
		// 武装过 TTL，timer 与停表竞态可能在飞的 opDestroy），解锁后惰性启动
		// supervisor 以本任务为首任务直入。state=slotRunning 同时令并发复用落入队分支。
		dormant := s.dormant
		if dormant {
			s.dormant = false
			s.state = slotRunning
		drainLoop:
			for {
				select {
				case <-s.ops:
				default:
					break drainLoop
				}
			}
		} else {
			select {
			case s.ops <- domainOp{kind: opNewTask, task: taskText, wallClock: wallClock, images: imgs}:
			default:
				// ops 满（异常）：回滚计数与未决挂账。
				s.reuseCount--
				s.mu.Unlock()
				d.trackChildDone(parentID)
				return "", &tool.Result{Error: fmt.Sprintf("reuse_agent_id %s 指令通道满，请稍后重试", reuseAgentID), Category: tool.ResultCategoryValidationRejected}
			}
		}
		s.mu.Unlock()
		if dormant {
			// 与 dispatchHotDomain 同口径：巡检元数据（runDomainTask 入口换绑真实 cancel）+
			// panic 兜底，supervisor 常驻 goroutine 惰性启动（首任务即本次复用任务）。
			d.subMeta.Store(s.id, &subAgentMeta{parentID: s.parentID, sessionID: s.sessionID, wallClock: wallClock})
			d.ensurePatrol()
			go func() {
				defer d.recoverSlotPanic(s)
				d.runDomainSupervisor(s, taskText, wallClock, imgs)
			}()
		}
		// 活动恢复（心跳豁免解除）：enterIdle 已 Delete 旧条目，Load 必落空，须重建。
		d.rearmSlotActivity(s.id)
		log.Printf("[subagent] REUSE: sub=%s domain=%s reuse=%d task_len=%d", s.id, s.domain, s.reuseCount, len(task))
		// 任务台账登记：续建复用新开一条任务记录（台账按任务粒度，热驻槽跨任务不累加）。
		d.ledger.RecordDispatch(s.sessionID, parentID, s.id, s.domain, task, fmt.Sprintf("续建#%d", s.reuseCount))
		d.writeCheckpoint(s.id, checkpointPhaseReuse)
		return s.id, nil
	default: // slotRunning（含挂起）
		return enqueue()
	}
}

// WakeIdleWithMessage 用户直连唤醒热驻 idle 槽（编排页对话面板，service MessageAgent
// 的 StatusIdle 分支调用；实现 agent.AgentMessenger 接口）。
// 唤醒序列照搬 dispatchToIdleSlot 的 idle 分支（reuseCount++ / 停 TTL / Wake /
// trackChildStart / rearmSlotActivity / opNewTask / 台账），差异点：
//   - 任务文本 = "【用户直连消息】"+用户原文（对齐 ReviveWithMessage 种子口径）——
//     这是用户对该 Agent 的对话而非主 Agent 派发的新任务，不拼 buildReuseTask 前缀；
//   - 邮箱通知父 Agent（MsgInfo，对齐 ReviveWithMessage 的父感知口径）并 pokeParent。
// 槽不存在/已销毁 → ErrAgentNotDirectable；槽 running → ErrAgentBusy。
func (d *Dispatcher) WakeIdleWithMessage(agentID, content string) error {
	taskText := "【用户直连消息】\n" + content
	if err := d.wakeIdleWithTask(agentID, taskText, "用户直连", "用户直连唤醒热驻 Agent"); err != nil {
		return err
	}
	log.Printf("[subagent] USER-WAKE: sub=%s msg_len=%d", agentID, len(content))
	return nil
}

// WakeIdleForMail 兄弟 Agent 邮件唤醒热驻 idle 槽（2026-09-21 问答面闭环修复）。
// 实证：domain-2 向 idle 槽 domain-1 发出 request 后 5 分钟零回复——消息躺在邮箱里
// 无人 drain（idle 槽只被用户直连/复用派发唤醒，send_message 投递不触发唤醒），
// 「答别人：当轮必须回复」纪律因收件方根本不在循环里而落空。本方法在 request/
// escalate 命中 idle 槽时复用 idle 唤醒序列，任务文本为邮箱处置提示：槽续答先
// drain 邮箱看到提问并 reply，无其他待办则自然回 idle。
// 唤醒是尽力而为：失败（槽已销毁/忙碌）时消息仍在邮箱，复用时可见（现状兜底）。
func (d *Dispatcher) WakeIdleForMail(agentID, askerID, subject string) error {
	taskText := "【邮箱请求】Agent " + askerID + " 向你提问（request 消息已在你的邮箱）。\n" +
		"本轮只处置该提问：查看邮箱，用 send_message(to_agent_id=对方id, message_type=reply, " +
		"reply_to=来信注入头里的 id, thread_id 沿用) 回复，" +
		"给结论+关键依据+文件路径；回复后若无其他待办即结束。"
	if err := d.wakeIdleWithTask(agentID, taskText, "邮箱请求", "Agent "+askerID+" 邮件提问唤醒"); err != nil {
		return err
	}
	log.Printf("[subagent] MAIL-WAKE: sub=%s asker=%s subject=%q", agentID, askerID, truncateRunes(subject, 60))
	return nil
}

// wakeIdleWithTask 是 WakeIdleWithMessage / WakeIdleForMail 共享的 idle 唤醒序列：
// 槽 idle 校验 → reuseCount+1 / 停 TTL / 树 Idle→Running（失败回滚计数）→
// trackChildStart + opNewTask（P0-2d 同事务）→ rearmSlotActivity → 台账 + 父感知。
// ledgerVerb 入账备注动词（"用户直连#N"/"邮箱请求#N"），parentNote 父感知邮件正文前缀。
func (d *Dispatcher) wakeIdleWithTask(agentID, taskText, ledgerVerb, parentNote string) error {
	if !d.hotEnabled() {
		return fmt.Errorf("%w: 热驻未开启", agent.ErrAgentNotDirectable)
	}
	s := d.pool.slot(sessionIDFromAgentID(agentID), agentID)
	if s == nil {
		return fmt.Errorf("%w: 热驻槽 %s 不存在或已销毁", agent.ErrAgentNotDirectable, agentID)
	}
	s.mu.Lock()
	if s.state != slotIdle {
		st := s.state
		s.mu.Unlock()
		if st == slotRunning {
			return fmt.Errorf("%w: 目标 Agent 正在执行任务", agent.ErrAgentBusy)
		}
		return fmt.Errorf("%w: 热驻槽 %s 已销毁", agent.ErrAgentNotDirectable, agentID)
	}
	// idle 分支（同 dispatchToIdleSlot）：reuseCount+1 + TTL 重置 + 树 Idle→Running + opNewTask。
	// P0-2c：状态复核→树 Wake→op 投递整段持 s.mu（竞态与回滚口径同 dispatchToIdleSlot）。
	s.reuseCount++
	if s.ttlTimer != nil {
		s.ttlTimer.Stop()
		s.ttlTimer = nil
	}
	s.ttlArmed = false
	cancelRef := s.cancelTask
	if d.treeFn != nil {
		if t := d.treeFn(s.sessionID); t != nil {
			if !t.Wake(s.id, cancelRef) {
				s.reuseCount--
				s.mu.Unlock()
				return fmt.Errorf("%w: 热驻槽 %s 树节点非空闲（状态失步）", agent.ErrAgentNotDirectable, agentID)
			}
		}
	}
	// P0-2d：未决计数与 opNewTask 投递同事务（口径同 dispatchToIdleSlot idle 分支）；
	// default 分支回滚计数并补偿递减。
	d.trackChildStart(s.parentID)
	// dormant（冷恢复重建槽）：无 supervisor 常驻 goroutine——锁内置 Running + 清
	// dormant + 排空死信期滞留 op，解锁后惰性启动 supervisor（首任务即本次任务）。
	// 口径同 dispatchToIdleSlot idle 分支的 dormant 处理。
	dormant := s.dormant
	if dormant {
		s.dormant = false
		s.state = slotRunning
	drainLoop:
		for {
			select {
			case <-s.ops:
			default:
				break drainLoop
			}
		}
	} else {
		select {
		case s.ops <- domainOp{kind: opNewTask, task: taskText}:
		default:
			s.reuseCount--
			s.mu.Unlock()
			d.trackChildDone(s.parentID)
			return fmt.Errorf("%w: 热驻槽 %s 指令通道满，请稍后重试", agent.ErrAgentNotDirectable, agentID)
		}
	}
	s.mu.Unlock()
	if dormant {
		d.subMeta.Store(s.id, &subAgentMeta{parentID: s.parentID, sessionID: s.sessionID})
		d.ensurePatrol()
		go func() {
			defer d.recoverSlotPanic(s)
			d.runDomainSupervisor(s, taskText, 0, nil)
		}()
	}

	// 父未决计数已在上方临界区内与 opNewTask 投递同事务挂账（P0-2d），
	// 完成时 trackChildDoneOnce 配对递减。
	// 活动恢复（心跳豁免解除）：enterIdle 已 Delete 旧条目，须重建。
	d.rearmSlotActivity(s.id)
	// 任务台账登记：直连/邮件唤醒单独备注，区别于主 Agent 续建派发。
	d.ledger.RecordDispatch(s.sessionID, s.parentID, s.id, s.domain, truncateRunes(taskText, 80), fmt.Sprintf("%s#%d", ledgerVerb, s.reuseCount))
	d.writeCheckpoint(s.id, checkpointPhaseReuse)
	// 父感知（对齐 ReviveWithMessage）：邮件通知父"等其回传，勿重复派发同领域任务"。
	if s.parentID != "" && d.mailbox != nil {
		_, _ = d.mailbox.Send(&mailbox.Message{
			From: "dispatcher", To: s.parentID, Type: mailbox.MsgInfo,
			Subject: "热驻 Agent 唤醒",
			Body:    fmt.Sprintf("%s：%s，等待其回传，勿重复派发同领域任务。", parentNote, s.id),
		})
		d.pokeParent(s.parentID)
	}
	return nil
}

// buildReuseTask 拼装复用任务文本：项目自述 + 共享前缀 + 召回前缀 + 领域标签 + 任务正文。
func (d *Dispatcher) buildReuseTask(ctx context.Context, s *domainSlot, task string) string {
	var prefixes []string
	// 项目自述（TODO 第10项⑦）：与 runSubAgentOnce 首派同口径，置前缀首位。
	if brief := d.projectBriefPrefix(ctx); brief != "" {
		prefixes = append(prefixes, brief)
	}
	if sp := d.buildSharedPrefix(ctx, s.parentID, s.domain); sp != "" {
		prefixes = append(prefixes, sp)
	}
	if bm, _ := d.injectScopedRecall(ctx, s.parentID, s.domain, task, ""); bm != "" {
		prefixes = append(prefixes, bm)
	}
	if label := strings.TrimSpace(s.domain); label != "" {
		task = "【你的领域】" + textutil.TruncateRunes(label, 16, "…") + "\n" + task
	}
	// 派发前上下文预算（TODO #22②）：超 fork 硬顶转 isolated（前缀全弃不硬灌）。
	prefixes, task, _ = capSpawnPrefixes(prefixes, task)
	if len(prefixes) > 0 {
		return strings.Join(prefixes, "\n\n") + "\n\n【当前任务】\n" + task
	}
	return task
}

// enqueueNext 弹出队列中下一任务（当前任务完成后调用，返回 ok=false 表示空）。
func (s *domainSlot) dequeueNext() (queuedTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.taskQueue) == 0 {
		return queuedTask{}, false
	}
	t := s.taskQueue[0]
	s.taskQueue = s.taskQueue[1:]
	return t, true
}

// dispatchHotDomain 热驻模式的全新 domain 派发（dispatchOne 分流入口）：
// 建槽 + 树 Register/绑定 cancel + 心跳元数据 + 启动 supervisor 常驻 goroutine。
// 前缀注入（共享记忆/召回/领域标签/salvage）复用 runDomainTask 路径的任务拼装——
// 此处只拼首任务前缀（与 runSubAgentOnce 等价），后续复用任务走 buildReuseTask。
// PendingChildren 挂账在 dispatchOne 已完成（trackChildStart）。
func (d *Dispatcher) dispatchHotDomain(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, domain, task, responsibility string, wallClock time.Duration, taskBrief string, started time.Time) (string, *tool.Result) {
	sid := tool.SessionIDFromContext(ctx)

	// 首任务前缀拼装（共享记忆 + 召回 + 领域标签 + salvage 由 dispatchOne 已做 salvage）。
	taskText := d.buildReuseTask(ctx, &domainSlot{parentID: parentID, domain: domain, sessionID: sid}, task)

	s := &domainSlot{
		id:             subAgentID,
		sessionID:      sid,
		workDir:        tool.WorkDirFromContext(ctx),
		stopCtx:        tool.StopContextFrom(ctx),
		parentID:       parentID,
		domain:         strings.TrimSpace(domain),
		responsibility: responsibility,
		state:          slotRunning,
		ops:            make(chan domainOp, 8),
	}
	// 树 Register + 绑定槽销毁句柄（首任务 cancel 由 runDomainTask 内 SetCancel 换绑）。
	if d.treeFn != nil && sid != "" {
		if t := d.treeFn(sid); t != nil {
			t.Register(orchestrator.Node{
				ID:       subAgentID,
				ParentID: parentID,
				Role:     "domain",
				Domain:   domain,
				Task:     taskBrief,
				Started:  started,
				Status:   orchestrator.StatusRunning,
			})
			t.SetCancel(subAgentID, s.destroyFnLocked())
		}
	}
	// 任务台账登记（热驻新建路径，与 dispatchOne 非热路径并列）。
	d.ledger.RecordDispatch(sid, parentID, subAgentID, domain, taskBrief, "")
	d.writeCheckpoint(subAgentID, checkpointPhaseDispatch)
	// 心跳元数据：subMeta 供巡检兜底；activity 由 buildDomainAgent 注册。
	d.subMeta.Store(subAgentID, &subAgentMeta{parentID: parentID, sessionID: sid, wallClock: wallClock})
	d.ensurePatrol()

	// LRU 上限：超 MaxPerSession 时淘毁最旧 idle（销毁经 ops 串行，安全）。
	if idles := d.pool.idleSlots(sid); len(idles) >= d.hotCfg.MaxPerSession {
		for i := 0; i <= len(idles)-d.hotCfg.MaxPerSession; i++ {
			old := idles[i]
			log.Printf("[subagent] IDLE LRU EVICT: sub=%s domain=%s (max %d)", old.id, old.domain, d.hotCfg.MaxPerSession)
			select {
			case old.ops <- domainOp{kind: opDestroy}:
			default:
			}
		}
	}

	d.pool.store(s)
	log.Printf("[subagent] dispatch: parent=%s sub=%s role=domain domain=%s task=%q (hot-resident)", parentID, subAgentID, domain, taskBrief)
	// P1-3 panic 兜底：supervisor 内任何 panic（SDK 空指针/工具 panic）不得崩掉整个
	// 后端带走全部在跑会话——recover 后走无锁清理（panic 可能持 s.mu 未放，禁碰槽锁），
	// 父未决计数补偿与树收尾对齐 destroySlot 语义。
	go func() {
		defer d.recoverSlotPanic(s)
		d.runDomainSupervisor(s, taskText, wallClock, agent.UserImagesFromContext(ctx))
	}()
	return subAgentID, nil
}

// recoverSlotPanic 热驻 supervisor goroutine 的 panic 兜底（defer 调用，无 panic 时
// 零开销直接返回）。刻意不触碰 s.mu：panic 展开点可能仍持有槽锁，加锁即死锁；
// 只做 sync.Map/pool 级无锁清理 + 父补偿，槽从池摘除防泄漏。
func (d *Dispatcher) recoverSlotPanic(s *domainSlot) {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("[subagent] PANIC RECOVERED: supervisor sub=%s domain=%s panic=%v", s.id, s.domain, r)
	var doneOnce *sync.Once
	if v, ok := d.subMeta.LoadAndDelete(s.id); ok {
		if m, ok2 := v.(*subAgentMeta); ok2 {
			doneOnce = &m.doneOnce
		}
	}
	d.pool.remove(s.sessionID, s.id)
	d.running.Delete(s.id)
	d.activity.Delete(s.id)
	d.lastWrites.Delete(s.id)
	d.heldSkills.Delete(s.id)
	if d.mailbox != nil {
		d.purgeMailboxWithNotice(s.id, "异常终止")
	}
	d.panicNotifyParent(s.parentID, s.id, s.sessionID, doneOnce)
}
