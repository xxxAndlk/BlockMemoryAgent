package goal

// manager.go 指令模式管理器：连接 agent 包（DirectiveHook 实现）、调度器
//（goal_reminders 到期扫描）与评估器（轻量模型）。所有决策权威在表，
// 本结构除每会话串行锁外无内存权威。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// Config 是 Manager 的运行参数（零值由 NewManager 收敛默认）。
type Config struct {
	// PollInterval 到期提醒扫描间隔（默认 15s）。
	PollInterval time.Duration
	// MinLoopInterval loop 最小间隔（默认 60s；SetMode 时收敛，防误配 1s 空转烧 token）。
	MinLoopInterval time.Duration
	// JudgeTimeout 单次完成度评估超时（默认 60s）。
	JudgeTimeout time.Duration
}

// JudgeFunc 是轻量模型调用（*model.ModelFactory 满足，照 workflow/judge.go）。
type JudgeFunc func(ctx context.Context, prompt string) (string, error)

// WakeFunc 注入提醒消息并恢复会话推进（bootstrap 接线 ReactService.Send：
// 运行中投邮箱 / 非运行懒恢复续跑同一条路径）。
type WakeFunc func(ctx context.Context, sessionID, text string) error

// NotifyFunc 向会话对话页发纯系统通告（不触发新回合；bootstrap 接线
// ReactService.NotifyUserSystemMessage）。会话不在内存时静默跳过。
type NotifyFunc func(sessionID, text string)

// Manager 管理全部会话的指令模式与回查提醒调度。
type Manager struct {
	cfg    Config
	store  Store
	judge  JudgeFunc
	wake   WakeFunc
	notify NotifyFunc
	logf   func(format string, args ...any)

	idSeq int64 // ID 生成序号（UnixMilli 同毫秒防撞）

	mu     sync.Mutex
	stopCh chan struct{}
	// sessLocks 每会话串行化回合评估 + 模式切换（OnTurnEnd 并发唤醒下同会话不交错）。
	sessLocks sync.Map
}

// NewManager 构造管理器；store 为 nil 时返回 nil（bootstrap 判 PG 不可用）。
func NewManager(cfg Config, store Store, judge JudgeFunc, wake WakeFunc, notify NotifyFunc, logf func(string, ...any)) *Manager {
	if store == nil {
		return nil
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.MinLoopInterval <= 0 {
		cfg.MinLoopInterval = 60 * time.Second
	}
	if cfg.JudgeTimeout <= 0 {
		cfg.JudgeTimeout = 60 * time.Second
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Manager{cfg: cfg, store: store, judge: judge, wake: wake, notify: notify, logf: logf}
}

// sessionLock 取（或建）某会话的串行锁。
func (m *Manager) sessionLock(sessionID string) *sync.Mutex {
	v, _ := m.sessLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Start 启动到期提醒调度循环（幂等）。
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.stopCh:
	default:
		if m.stopCh != nil {
			return // 已启动
		}
	}
	m.stopCh = make(chan struct{})
	go m.loop(ctx, m.stopCh)
}

// Stop 停止调度循环（幂等）。
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopCh != nil {
		close(m.stopCh)
		m.stopCh = nil
	}
}

// loop 调度主循环：定时扫到期提醒 → 认领 → 异步唤醒会话。
func (m *Manager) loop(ctx context.Context, stopCh chan struct{}) {
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			m.fireDue(ctx)
		}
	}
}

// fireDue 认领并触发到期提醒（单发失败不影响其余）。
func (m *Manager) fireDue(ctx context.Context) {
	rems, err := m.store.ClaimDueReminders(ctx, 50)
	if err != nil {
		m.logf("[goal] 扫描到期提醒失败: %v", err)
		return
	}
	for _, r := range rems {
		r := r
		go func() {
			// 唤醒整体超时 2 分钟：会话恢复 + 整轮执行都在其内（Send 本身异步起 goroutine，
			// 此处只等"消息落会话/邮箱"，正常毫秒级）。
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.wake(cctx, r.SessionID, r.Text); err != nil {
				m.logf("[goal] 唤醒会话 %s 失败（提醒 %s）: %v", r.SessionID, r.ID, err)
			}
		}()
	}
}

// ApplyDirective 实现 agent.DirectiveHook：应用/切换会话指令模式（upsert）。
// objective 为空时调用方须已回落（建会话=goal 文本、消息=content），此处仍校验非空。
func (m *Manager) ApplyDirective(ctx context.Context, sessionID string, spec *agent.DirectiveSpec) error {
	if m == nil || spec == nil {
		return fmt.Errorf("指令规格为空")
	}
	if !ValidMode(spec.Mode) {
		return fmt.Errorf("非法指令模式 %q（normal|goal|loop）", spec.Mode)
	}
	objective := strings.TrimSpace(spec.Objective)
	if spec.Mode != ModeNormal && objective == "" {
		return fmt.Errorf("%s 模式需要 objective（目标/指令文本）", spec.Mode)
	}
	var deadline *time.Time
	if strings.TrimSpace(spec.Deadline) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(spec.Deadline))
		if err != nil {
			return fmt.Errorf("deadline 非法（需 RFC3339，如 2026-10-20T09:00:00+08:00）: %w", err)
		}
		deadline = &t
	}
	if spec.Mode == ModeLoop && spec.LoopIntervalSec <= 0 {
		return fmt.Errorf("loop 模式需要 loop_interval_sec > 0")
	}
	lock := m.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	old, err := m.store.GetDirectiveBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("读取既有指令失败: %w", err)
	}
	id := fmt.Sprintf("dir-%d-%d", time.Now().UnixMilli(), atomic.AddInt64(&m.idSeq, 1))
	if old != nil {
		id = old.ID // 一会话一行，保 ID 稳定
	}
	d := &Directive{
		ID:                  id,
		SessionID:           sessionID,
		Mode:                spec.Mode,
		Objective:           objective,
		CompletionCriterion: strings.TrimSpace(spec.CompletionCriterion),
		MaxTurns:            spec.MaxTurns,
		Deadline:            deadline,
		LoopIntervalSec:     spec.LoopIntervalSec,
		LoopUntil:           strings.TrimSpace(spec.LoopUntil),
		Status:              StatusActive,
	}
	if d.Mode == ModeNormal {
		d.Status = StatusStopped
	}
	if d.Mode == ModeLoop && time.Duration(d.LoopIntervalSec)*time.Second < m.cfg.MinLoopInterval {
		d.LoopIntervalSec = int(m.cfg.MinLoopInterval / time.Second)
	}
	if err := m.store.UpsertDirective(ctx, d); err != nil {
		return fmt.Errorf("落库指令失败: %w", err)
	}
	// 模式切换/重激活：清掉旧模式遗留的未触发提醒（如 loop 改 goal 后轮询提醒会乱入）。
	if n, err := m.store.CancelPendingReminders(ctx, sessionID); err != nil {
		m.logf("[goal] 清理旧提醒失败 session=%s: %v", sessionID, err)
	} else if n > 0 {
		m.logf("[goal] 模式切换清理未触发提醒 session=%s n=%d", sessionID, n)
	}
	return nil
}

// Deactivate 实现 agent.DirectiveHook：停止会话指令（切普通/用户停止共用）。
func (m *Manager) Deactivate(sessionID, reason string) {
	if m == nil {
		return
	}
	lock := m.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := m.store.GetDirectiveBySession(ctx, sessionID)
	if err != nil || d == nil || d.Status != StatusActive {
		return
	}
	if ok, err := m.store.FinishDirective(ctx, d.ID, StatusStopped, d.TurnCount, reason); err != nil {
		m.logf("[goal] 停止指令失败 session=%s: %v", sessionID, err)
		return
	} else if !ok {
		return
	}
	if _, err := m.store.CancelPendingReminders(ctx, sessionID); err != nil {
		m.logf("[goal] 停止指令清理提醒失败 session=%s: %v", sessionID, err)
	}
	m.notifySafe(sessionID, fmt.Sprintf("【系统】指令模式已停止（%s）。", reason))
}

// OnTurnEnd 实现 agent.DirectiveHook：回合成功结束后的指令推进。
// status 非 completed（暂停/挂起/出错）时不动作——指令保持 active，
// 待会话恢复或用户下一步操作（暂停态被提醒唤醒后仍回到这里闭环）。
func (m *Manager) OnTurnEnd(sessionID, status, finalText string) {
	if m == nil || status != "completed" {
		return
	}
	lock := m.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, err := m.store.GetDirectiveBySession(ctx, sessionID)
	if err != nil {
		m.logf("[goal] 读取指令失败 session=%s: %v", sessionID, err)
		return
	}
	if d == nil || d.Mode == ModeNormal || d.Status != StatusActive {
		return
	}
	turn := d.TurnCount + 1
	d.TurnCount = turn
	// 预算闸（goal/loop 共用）：硬停止，评估器不再消耗。
	modeLabel := "目标"
	if d.Mode == ModeLoop {
		modeLabel = "循环"
	}
	if d.MaxTurns > 0 && turn >= d.MaxTurns {
		m.finish(ctx, d, StatusFailed, "达到最大轮数预算")
		m.notifySafe(sessionID, fmt.Sprintf("【系统·%s模式】⏸ 已达最大轮数预算，指令停止。可在指令面板加大预算后重新开启。", modeLabel))
		return
	}
	if d.Deadline != nil && time.Now().After(*d.Deadline) {
		m.finish(ctx, d, StatusFailed, "超过截止时间预算")
		m.notifySafe(sessionID, fmt.Sprintf("【系统·%s模式】⏸ 已超过截止时间预算，指令停止。可延长 deadline 后重新开启。", modeLabel))
		return
	}
	switch d.Mode {
	case ModeLoop:
		m.advanceLoop(ctx, d, sessionID, finalText)
	case ModeGoal:
		m.advanceGoal(ctx, d, sessionID, finalText)
	}
}

// advanceLoop loop 推进：判停止条件 → 记轮 → 排下一轮提醒（到点由调度器唤醒）。
func (m *Manager) advanceLoop(ctx context.Context, d *Directive, sessionID, finalText string) {
	reason := ""
	if strings.TrimSpace(d.LoopUntil) != "" {
		hit, r := m.evalLoop(ctx, d, finalText)
		reason = r
		if hit {
			m.finish(ctx, d, StatusComplete, reason)
			m.notifySafe(sessionID, fmt.Sprintf("【系统·循环模式】✅ 停止条件已满足（%s），循环结束，已切回普通模式。", reason))
			return
		}
	}
	if err := m.store.TouchTurn(ctx, d.ID, d.TurnCount, reason); err != nil {
		m.logf("[goal] loop 记轮失败: %v", err)
	}
	runAt := time.Now().Add(time.Duration(d.LoopIntervalSec) * time.Second)
	rem := &Reminder{
		ID:          fmt.Sprintf("rem-%d-%d", time.Now().UnixMilli(), atomic.AddInt64(&m.idSeq, 1)),
		SessionID:   sessionID,
		DirectiveID: d.ID,
		Kind:        ReminderKindInterval,
		Text:        fmt.Sprintf("【系统·循环】第 %d 轮：%s", d.TurnCount+1, d.Objective),
		RunAt:       runAt,
		Status:      ReminderStatusPending,
		CreatedBy:   "system",
	}
	if err := m.store.AddReminder(ctx, rem); err != nil {
		m.logf("[goal] loop 排下一轮失败 session=%s: %v", sessionID, err)
		m.notifySafe(sessionID, "【系统·循环模式】⏸ 排程失败，循环中断："+err.Error())
	}
}

// advanceGoal goal 推进：判达成 → 达成则终局；未达成注入"继续推进"消息续跑。
func (m *Manager) advanceGoal(ctx context.Context, d *Directive, sessionID, finalText string) {
	hit, reason := m.evalGoal(ctx, d, finalText)
	if hit {
		m.finish(ctx, d, StatusComplete, reason)
		m.notifySafe(sessionID, fmt.Sprintf("【系统·目标模式】✅ 目标已达成（%s）。指令模式已切回普通。", reason))
		return
	}
	if err := m.store.TouchTurn(ctx, d.ID, d.TurnCount, reason); err != nil {
		m.logf("[goal] goal 记轮失败: %v", err)
	}
	text := fmt.Sprintf(`【系统·目标模式】第 %d 轮：评估器认为目标尚未达成（%s）。请继续推进目标。
注意：若当前步骤受外部条件阻塞（如开店资质审核、第三方处理需数天），不要空转——用 schedule_followup 工具自行安排回查（delay_minutes=预计等待分钟数，如审核 1-3 个工作日则先排 24 小时后=1440），安排好之后本轮直接结束即可；到点系统会自动注入回查提醒，届时检查进度，未通过则再排下一次。`,
		d.TurnCount, orDefault(reason, "无理由"))
	if err := m.wake(ctx, sessionID, text); err != nil {
		m.logf("[goal] goal 续跑注入失败 session=%s: %v", sessionID, err)
		m.notifySafe(sessionID, "【系统·目标模式】⚠️ 续跑消息注入失败："+err.Error())
	}
}

// finish 终局迁移（乐观）；lastEval 落评估结论文本。
func (m *Manager) finish(ctx context.Context, d *Directive, status, lastEval string) {
	if ok, err := m.store.FinishDirective(ctx, d.ID, status, d.TurnCount, lastEval); err != nil {
		m.logf("[goal] 指令终局落库失败 session=%s: %v", d.SessionID, err)
	} else if !ok {
		m.logf("[goal] 指令终局未命中（已被并发停止）session=%s", d.SessionID)
	}
}

// evalGoal 判定 goal 达成（评委不可用按未达成处理，多跑一轮无害）。
func (m *Manager) evalGoal(ctx context.Context, d *Directive, finalText string) (bool, string) {
	if m.judge == nil {
		return false, "评估器未接线"
	}
	jctx, cancel := context.WithTimeout(ctx, m.cfg.JudgeTimeout)
	defer cancel()
	resp, err := m.judge(jctx, buildGoalEvalPrompt(d, finalText))
	if err != nil {
		m.logf("[goal] 评估调用失败 session=%s: %v", d.SessionID, err)
		return false, "评估器调用失败，按未达成处理"
	}
	res, err := parseEvalOutput(resp, "achieved")
	if err != nil {
		m.logf("[goal] 评估解析失败 session=%s: %v", d.SessionID, err)
		return false, "评估器响应解析失败，按未达成处理"
	}
	return res.Hit, res.Reason
}

// evalLoop 判定 loop 停止条件（评委不可用按未满足处理，循环不误停）。
func (m *Manager) evalLoop(ctx context.Context, d *Directive, finalText string) (bool, string) {
	if m.judge == nil {
		return false, ""
	}
	jctx, cancel := context.WithTimeout(ctx, m.cfg.JudgeTimeout)
	defer cancel()
	resp, err := m.judge(jctx, buildLoopUntilPrompt(d, finalText))
	if err != nil {
		m.logf("[goal] loop 判定调用失败 session=%s: %v", d.SessionID, err)
		return false, ""
	}
	res, err := parseEvalOutput(resp, "met")
	if err != nil {
		m.logf("[goal] loop 判定解析失败 session=%s: %v", d.SessionID, err)
		return false, ""
	}
	return res.Hit, res.Reason
}

// AddFollowup 供 schedule_followup 工具/用户 API 使用：给会话排一条未来回查提醒。
// 返回人类可读摘要。与指令模式正交——普通会话也可用（几天后提醒我检查 X）。
func (m *Manager) AddFollowup(ctx context.Context, sessionID string, delay time.Duration, text, createdBy string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("指令管理器未接线")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("回查内容不能为空")
	}
	if delay < time.Minute {
		return "", fmt.Errorf("回查间隔至少 1 分钟")
	}
	if delay > 30*24*time.Hour {
		return "", fmt.Errorf("回查间隔最多 30 天")
	}
	d, err := m.store.GetDirectiveBySession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	directiveID := ""
	if d != nil {
		directiveID = d.ID
	}
	runAt := time.Now().Add(delay)
	rem := &Reminder{
		ID:          fmt.Sprintf("rem-%d-%d", time.Now().UnixMilli(), atomic.AddInt64(&m.idSeq, 1)),
		SessionID:   sessionID,
		DirectiveID: directiveID,
		Kind:        ReminderKindOnce,
		Text:        "【系统·回查】" + text,
		RunAt:       runAt,
		Status:      ReminderStatusPending,
		CreatedBy:   createdBy,
	}
	if err := m.store.AddReminder(ctx, rem); err != nil {
		return "", err
	}
	return fmt.Sprintf("已安排 %s 回查：%s（会话到点自动注入提醒并恢复推进）",
		runAt.Local().Format("2006-01-02 15:04"), text), nil
}

// GetView 读会话指令 + 未触发提醒（HTTP GET 用）。
func (m *Manager) GetView(ctx context.Context, sessionID string) (*DirectiveView, error) {
	if m == nil {
		return nil, fmt.Errorf("指令管理器未接线")
	}
	d, err := m.store.GetDirectiveBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rems, err := m.store.ListPendingReminders(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return &DirectiveView{Directive: d, PendingReminders: rems}, nil
}

// notifySafe 通知 best-effort（notify 未接线/会话不在内存静默跳过）。
func (m *Manager) notifySafe(sessionID, text string) {
	if m.notify == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.logf("[goal] 系统通告 panic session=%s: %v", sessionID, r)
		}
	}()
	m.notify(sessionID, text)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
