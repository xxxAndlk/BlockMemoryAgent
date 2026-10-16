package workflow

// engine.go 量产引擎（开发文档 §5）：一个 run 一个调度循环（goroutine），件间并发 =
// run.concurrency、件内阶段串行；全局活跃 run 闸 max_active_runs（超出入队 pending）。
// 引擎不占用 max_concurrent_sub_agents 池——该池只为升级出来的例外服务。
//
// 纪律：权威全在表（workflow_runs/workflow_units）+ 事件账本，无内存权威；重启经
// Start() 扫 running 恢复（running unit 标回 pending，幂等键不变）。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
)

// Config 是引擎运行参数（接入 config.yaml workflow: 段）。
type Config struct {
	MaxActiveRuns      int           // 全局活跃 run 闸（默认 4）
	DefaultConcurrency int           // 件间并发默认（默认 2）
	BudgetCapDefault   int           // 轻量调用次数默认上限（默认 500）
	JudgeTimeout       time.Duration // 单次评委超时（默认 30s）
}

// Emitter 把工作流事件送入会话事件流（bootstrap 适配 agent.LiveEvent → ForwardLiveEvent）。
// nil 时事件静默丢弃（测试/降级）。
type Emitter func(sessionID, text, detailJSON string)

// ToolDispatcher 是引擎所需的工具调用面（*tool.Registry 满足；测试用假实现）。
type ToolDispatcher interface {
	Dispatch(ctx context.Context, name string, args map[string]any) (*tool.Result, error)
	MountPreApprovedForScope(scope, roleID string, names []string) []string
}

// Engine 是量产引擎本体。
type Engine struct {
	cfg      Config
	store    Store
	tools    ToolDispatcher
	models   LightModel
	embedder Embedder
	escalate Escalator
	trace    TraceLoader
	confirm  tool.WorkflowConfirmHookFunc
	emit     Emitter
	runFinal func(run *Run) // 终态回调（webhook；nil-safe）
	log      *logger.Logger

	stopCtx    context.Context
	stopCancel context.CancelFunc
	stopOnce   sync.Once
	wg         sync.WaitGroup
	startOnce  sync.Once

	mu     sync.Mutex
	active map[string]*runCtl // runID → 调度控制（活跃循环）

	drift *driftTracker

	// 运行期锚点池与发起角色缓存（非权威：重启经 seedAnchorPoolFromUnits 从已采纳单元重建；
	// 角色缓存丢失则 dispatch 走程序化默认放行）。
	anchorsMu sync.Mutex
	anchors   map[anchorKey]string
	runRoles  map[string]string
}

// runCtl 是一个活跃 run 的调度控制（取消传播到在途 Dispatch）。
type runCtl struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewEngine 构造引擎（cfg 零值字段按默认补齐）。
func NewEngine(cfg Config, store Store, tools ToolDispatcher, models LightModel) *Engine {
	if cfg.MaxActiveRuns <= 0 {
		cfg.MaxActiveRuns = 4
	}
	if cfg.DefaultConcurrency <= 0 {
		cfg.DefaultConcurrency = 2
	}
	if cfg.BudgetCapDefault <= 0 {
		cfg.BudgetCapDefault = 500
	}
	if cfg.JudgeTimeout <= 0 {
		cfg.JudgeTimeout = 30 * time.Second
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &Engine{
		cfg:        cfg,
		store:      store,
		tools:      tools,
		models:     models,
		stopCtx:    stopCtx,
		stopCancel: stopCancel,
		active:     make(map[string]*runCtl),
		drift:      newDriftTracker(),
	}
}

// WithEmbedder 注入漂移检测向量器（可缺省，缺省时漂移检测降级跳过）。
func (e *Engine) WithEmbedder(emb Embedder) *Engine { e.embedder = emb; return e }

// WithEscalator 注入升级通道（*subagent.Dispatcher）。
func (e *Engine) WithEscalator(esc Escalator) *Engine { e.escalate = esc; return e }

// WithTraceLoader 注入会话轨迹读取器（workflow_freeze 的数据源）。
func (e *Engine) WithTraceLoader(t TraceLoader) *Engine { e.trace = t; return e }

// WithEmitter 注入事件发射器（会话事件流）。
func (e *Engine) WithEmitter(em Emitter) *Engine { e.emit = em; return e }

// WithRunFinalHook 注入 run 终态回调（webhook notifier；best-effort）。
func (e *Engine) WithRunFinalHook(fn func(run *Run)) *Engine { e.runFinal = fn; return e }

// SetConfirmHook 注入会话层确认回调（预算审批/草案确认；未注入时工具返回未配置错误）。
func (e *Engine) SetConfirmHook(h tool.WorkflowConfirmHookFunc) { e.confirm = h }

// SetLogger 注入结构化日志器（nil 回退标准库 log，照 dag.Scheduler 先例）。
func (e *Engine) SetLogger(l *logger.Logger) { e.log = l }

func (e *Engine) logError(ctx context.Context, msg string, err error) {
	if e.log != nil {
		e.log.Error(ctx, msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}

func (e *Engine) logInfo(ctx context.Context, msg string) {
	if e.log != nil {
		e.log.Info(ctx, msg)
		return
	}
	log.Print(msg)
}

// Start 启动引擎：重启恢复（running unit 标回 pending、running run 重新入调度）+ 兜底巡检。
// 照 dag.Scheduler 的 Start(ctx)/Stop() 生命周期先例。
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		e.wg.Add(2)
		go func() {
			defer e.wg.Done()
			e.kickLoop(ctx)
		}()
		go func() {
			defer e.wg.Done()
			e.recoverRuns(ctx)
		}()
	})
}

// Stop 停止引擎：取消全部在途调度与生成调用并等待 goroutine 退出（幂等）。
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		e.stopCancel()
		e.mu.Lock()
		for _, ctl := range e.active {
			ctl.cancel()
		}
		e.mu.Unlock()
	})
	e.wg.Wait()
}

// kickLoop 兜底巡检：处理边界情况（如入队时额度满、重启恢复竞态）。
func (e *Engine) kickLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stopCtx.Done():
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			e.Kick()
		}
	}
}

// recoverRuns 重启恢复（硬要求：重启不丢批量）。
// running 的 unit 标回 pending（幂等键 idem_key 不变，重跑重执行该 attempt 行，不重不漏）；
// running 的 run 经 Kick 重新入调度。
func (e *Engine) recoverRuns(ctx context.Context) {
	n, err := e.store.ResetRunningUnits(ctx)
	if err != nil {
		e.logError(ctx, "[workflow] 重启恢复：复位 running unit 失败", err)
	} else if n > 0 {
		e.logInfo(ctx, fmt.Sprintf("[workflow] 重启恢复：%d 个中断的 unit 标回 pending 待重跑", n))
	}
	e.Kick()
}

// Kick 尝试把队列中的 run 拉入调度：先恢复 running（重启/续跑），再按 FIFO 提升 pending。
func (e *Engine) Kick() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	e.mu.Lock()
	slots := e.cfg.MaxActiveRuns - len(e.active)
	e.mu.Unlock()
	if slots <= 0 {
		return
	}
	if ids, err := e.store.RunningRunIDs(ctx); err == nil {
		for _, id := range ids {
			if slots <= 0 {
				break
			}
			if e.beginRun(id) {
				slots--
			}
		}
	} else {
		e.logError(ctx, "[workflow] kick 读取 running run 失败", err)
	}
	if slots <= 0 {
		return
	}
	pending, err := e.store.PendingRunIDs(ctx, slots)
	if err != nil {
		e.logError(ctx, "[workflow] kick 读取 pending run 失败", err)
		return
	}
	for _, id := range pending {
		if slots <= 0 {
			break
		}
		ok, err := e.store.UpdateRunStatus(ctx, id, RunStatusPending, RunStatusRunning, "")
		if err != nil || !ok {
			continue
		}
		if e.beginRun(id) {
			slots--
		}
	}
}

// beginRun 为一个 run 启动调度 goroutine（已在调度中返回 false）。
func (e *Engine) beginRun(runID string) bool {
	e.mu.Lock()
	if _, busy := e.active[runID]; busy {
		e.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(e.stopCtx)
	e.active[runID] = &runCtl{ctx: ctx, cancel: cancel}
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.active, runID)
			e.mu.Unlock()
			cancel()
			e.Kick() // 让出额度给排队 run
		}()
		e.scheduleRun(ctx, runID)
	}()
	return true
}

// ---------------------------------------------------------------------------
// 建 run

// CreateSpec 是发起一次量产的入参（workflow_run 工具 / POST /api/workflow/runs）。
type CreateSpec struct {
	SessionID   string // 发起会话（工作区/事件展示锚点）
	Workdir     string // 配方与产物所在工作区（必填）
	RecipeID    string
	RosterRaw   string // 内联 JSON 数组或名册文件路径
	BudgetCap   int    // 0=配置默认
	Concurrency int    // 0=配方覆写/配置默认
	FailFast    *bool  // nil=true
	RoleID      string // 发起角色（工具门预挂/日志；空=顶层未知）
}

// Estimate 是量产前估算（走 ask_user 审批门展示）。
type Estimate struct {
	ItemCount      int `json:"item_count"`
	UnitCount      int `json:"unit_count"`
	GenerateCalls  int `json:"generate_calls"`  // 上限口径（含重摇）
	JudgeCalls     int `json:"judge_calls"`     // 上限口径（含重摇）
	EstimatedCalls int `json:"estimated_calls"` // 合计（=预算 cap 的参照）
	BudgetCap      int `json:"budget_cap"`
}

// EstimateRun 只做加载与估算（不建 run，审批门展示用）。
func (e *Engine) EstimateRun(spec CreateSpec) (*Recipe, []RosterItem, *Estimate, error) {
	if strings.TrimSpace(spec.Workdir) == "" {
		return nil, nil, nil, fmt.Errorf("workdir 必填")
	}
	recipe, err := LoadRecipe(spec.Workdir, spec.RecipeID)
	if err != nil {
		return nil, nil, nil, err
	}
	roster, err := ParseRoster(spec.RosterRaw, spec.Workdir)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateRosterAgainstRecipe(roster, recipe); err != nil {
		return nil, nil, nil, err
	}
	units := 0
	for _, item := range roster {
		units += len(item.StagesFor(recipe.Stages))
	}
	reroll := recipe.RerollLimit()
	est := &Estimate{
		ItemCount:      len(roster),
		UnitCount:      units,
		GenerateCalls:  units * (1 + reroll),
		JudgeCalls:     units * (1 + reroll),
		EstimatedCalls: units * (2 + 2*reroll),
		BudgetCap:      e.budgetCapOf(spec.BudgetCap),
	}
	return recipe, roster, est, nil
}

func (e *Engine) budgetCapOf(v int) int {
	if v > 0 {
		return v
	}
	return e.cfg.BudgetCapDefault
}

// CreateRun 建 run：加载配方 → 展开名册 × 阶段 = units → 建行（快照 recipe_yaml）→
// 预挂生成工具（工具门豁免）→ 入调度。引擎异步执行，立即返回。
func (e *Engine) CreateRun(ctx context.Context, spec CreateSpec) (*Run, *Estimate, error) {
	if strings.TrimSpace(spec.SessionID) == "" {
		return nil, nil, fmt.Errorf("session_id 必填")
	}
	recipe, roster, est, err := e.EstimateRun(spec)
	if err != nil {
		return nil, nil, err
	}
	recipeYAML, err := MarshalRecipe(recipe)
	if err != nil {
		return nil, nil, err
	}
	concurrency := spec.Concurrency
	if concurrency <= 0 {
		concurrency = recipe.Params.Concurrency
	}
	if concurrency <= 0 {
		concurrency = e.cfg.DefaultConcurrency
	}
	failFast := true
	if spec.FailFast != nil {
		failFast = *spec.FailFast
	}
	run := &Run{
		ID:            newRunID(),
		Owner:         "",
		SessionID:     spec.SessionID,
		Workdir:       spec.Workdir,
		RecipeID:      recipe.ID,
		RecipeVersion: recipe.Version,
		RecipeYAML:    string(recipeYAML),
		Roster:        roster,
		Status:        RunStatusPending,
		BudgetCap:     est.BudgetCap,
		Concurrency:   concurrency,
		FailFast:      failFast,
	}
	var units []*Unit
	for _, item := range roster {
		for _, stage := range item.StagesFor(recipe.Stages) {
			units = append(units, newAttemptUnit(run, item, stage, 1))
		}
	}
	if err := e.store.CreateRun(ctx, run, units); err != nil {
		return nil, nil, err
	}
	e.setRunRole(run.ID, spec.RoleID)
	// 生成工具预挂到会话 scope（role_tool_gate 开启部署的豁免通道，语义=配置人工声明放行；
	// 对齐开发文档 §5.7——不在引擎里绕门、不复制门逻辑）。
	if accepted := e.tools.MountPreApprovedForScope(run.SessionID, spec.RoleID, []string{recipe.Tool}); len(accepted) == 0 {
		e.logInfo(ctx, fmt.Sprintf("[workflow] run %s 生成工具 %s 预挂跳过（未注册或已挂）", run.ID, recipe.Tool))
	}
	ev := RunEvent{RunID: run.ID, Status: RunStatusPending,
		Note: fmt.Sprintf("已创建：%d 件 × 阶段 = %d 单元，预算上限 %d 次轻量调用", est.ItemCount, est.UnitCount, run.BudgetCap)}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	e.logInfo(ctx, fmt.Sprintf("[workflow] run %s 创建（recipe=%s v%d, units=%d, cap=%d）", run.ID, recipe.ID, recipe.Version, len(units), run.BudgetCap))
	e.Kick()
	return run, est, nil
}

// newRunID 生成紧凑 run ID（TEXT 主键，≤32 字符便于进事件 Tool 列）。
func newRunID() string {
	return fmt.Sprintf("wf-%d", time.Now().UnixNano()/1e6)
}

// newAttemptUnit 组装一个 attempt 行（id = idem_key = run/item/stage#attempt）。
func newAttemptUnit(run *Run, item RosterItem, stage *Stage, attemptNo int) *Unit {
	idem := fmt.Sprintf("%s/%s/%s#%d", run.ID, item.ID, stage.ID, attemptNo)
	return &Unit{
		ID: idem, RunID: run.ID, Owner: run.Owner,
		ItemID: item.ID, ItemName: item.Name, Stage: stage.ID,
		AttemptNo: attemptNo, Status: UnitStatusPending, IdemKey: idem,
	}
}

// ---------------------------------------------------------------------------
// 运行控制

// ListRuns 运行列表。
func (e *Engine) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	return e.store.ListRuns(ctx, f)
}

// UnitCounts 返回 run 的 cell 口径状态计数（列表页进度展示用）。
func (e *Engine) UnitCounts(ctx context.Context, runID string) (map[string]int, error) {
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, c := range latestCells(units) {
		out[c.Status]++
	}
	return out, nil
}

// RunDetail 是运行详情（名册×阶段网格数据源）。
type RunDetail struct {
	Run   *Run    `json:"run"`
	Units []*Unit `json:"units"`
}

// GetRunDetail 返回 run 行 + 全部 units（attempt 时间线）。
func (e *Engine) GetRunDetail(ctx context.Context, runID string) (*RunDetail, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &RunDetail{Run: run, Units: units}, nil
}

// PauseRun 暂停运行（running → paused；在途 unit 于边界停下）。
func (e *Engine) PauseRun(ctx context.Context, runID string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunStatusRunning {
		return fmt.Errorf("run %s 当前状态 %s，仅 running 可暂停", runID, run.Status)
	}
	ok, err := e.store.UpdateRunStatus(ctx, runID, RunStatusRunning, RunStatusPaused, PauseUser)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %s 状态已被并发迁移", runID)
	}
	e.emitEvent(run.SessionID, RunEvent{RunID: runID, Status: RunStatusPaused, Reason: PauseUser}.Text(), RunEvent{RunID: runID, Status: RunStatusPaused, Reason: PauseUser}.DetailJSON())
	return nil
}

// ResumeRun 续跑（paused → running）；预算暂停需先提额（extraBudget>0）。
func (e *Engine) ResumeRun(ctx context.Context, runID string, extraBudget int) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunStatusPaused {
		return fmt.Errorf("run %s 当前状态 %s，仅 paused 可续跑", runID, run.Status)
	}
	if run.PauseReason == PauseBudgetExceeded && extraBudget <= 0 {
		return fmt.Errorf("run %s 因预算暂停（spent=%d/cap=%d），续跑需先提额", runID, run.SpentEstimate, run.BudgetCap)
	}
	if extraBudget > 0 {
		newCap, err := e.store.RaiseRunBudget(ctx, runID, extraBudget)
		if err != nil {
			return err
		}
		e.logInfo(ctx, fmt.Sprintf("[workflow] run %s 预算提额 → %d", runID, newCap))
	}
	ok, err := e.store.UpdateRunStatus(ctx, runID, RunStatusPaused, RunStatusRunning, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %s 状态已被并发迁移", runID)
	}
	ev := RunEvent{RunID: runID, Status: RunStatusRunning, Note: "续跑"}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	e.Kick()
	return nil
}

// AbortRun 中止运行（pending/running/paused → aborted；在途调度取消）。
func (e *Engine) AbortRun(ctx context.Context, runID string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if IsRunTerminal(run.Status) {
		return fmt.Errorf("run %s 已是终态 %s", runID, run.Status)
	}
	ok, err := e.store.UpdateRunStatus(ctx, runID, run.Status, RunStatusAborted, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %s 状态已被并发迁移", runID)
	}
	e.mu.Lock()
	if ctl, busy := e.active[runID]; busy {
		ctl.cancel()
	}
	e.mu.Unlock()
	ev := RunEvent{RunID: runID, Status: RunStatusAborted}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	e.notifyRunFinal(runID)
	return nil
}

// UnitAction 人审操作：accept / reject（必带理由）/ reroll / skip。
func (e *Engine) UnitAction(ctx context.Context, unitID, action, reason string) error {
	u, err := e.store.GetUnit(ctx, unitID)
	if err != nil {
		return err
	}
	run, err := e.store.GetRun(ctx, u.RunID)
	if err != nil {
		return err
	}
	emit := func(from, to, why string) {
		it := RosterItem{ID: u.ItemID, Name: u.ItemName}
		ev := UnitEvent{RunID: u.RunID, UnitID: u.ID, ItemID: it.ID, ItemName: it.Name, Stage: u.Stage, AttemptNo: u.AttemptNo, From: from, To: to, Reason: why}
		e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	}
	switch action {
	case UnitActionAccept:
		if u.Status != UnitStatusNeedsReview {
			return fmt.Errorf("unit %s 状态 %s，仅 needs_review 可采纳", unitID, u.Status)
		}
		ok, err := e.store.TransitionUnit(ctx, unitID, []string{UnitStatusNeedsReview}, UnitStatusDone, UnitPatch{})
		if err != nil || !ok {
			return fmt.Errorf("采纳失败: %v", err)
		}
		emit(UnitStatusNeedsReview, UnitStatusDone, "人工采纳")
		e.seedAnchorPool(ctx, run, u)
		e.afterCellSettled(ctx, run.ID)
		return nil
	case UnitActionReject:
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("reject 必须带理由（驳回理由是配方演化的输入）")
		}
		ok, err := e.store.TransitionUnit(ctx, unitID, []string{UnitStatusNeedsReview, UnitStatusFailed}, UnitStatusFailed, UnitPatch{RejectReason: &reason})
		if err != nil || !ok {
			return fmt.Errorf("驳回失败（状态不符）: %v", err)
		}
		emit(u.Status, UnitStatusFailed, "驳回："+reason)
		e.afterCellSettled(ctx, run.ID)
		return nil
	case UnitActionSkip:
		ok, err := e.store.TransitionUnit(ctx, unitID, []string{UnitStatusPending, UnitStatusNeedsReview, UnitStatusFailed}, UnitStatusSkipped, UnitPatch{})
		if err != nil || !ok {
			return fmt.Errorf("跳过失败（状态不符）: %v", err)
		}
		emit(u.Status, UnitStatusSkipped, "人审跳过")
		return e.maybeFinalizeRun(ctx, run.ID)
	case UnitActionReroll:
		if u.Status != UnitStatusNeedsReview && u.Status != UnitStatusFailed {
			return fmt.Errorf("unit %s 状态 %s，仅 needs_review/failed 可重摇", unitID, u.Status)
		}
		recipe, _ := ParseRecipe([]byte(run.RecipeYAML))
		var item RosterItem
		for _, it := range run.Roster {
			if it.ID == u.ItemID {
				item = it
				break
			}
		}
		stage := (*Stage)(nil)
		if recipe != nil {
			stage = recipe.StageByID(u.Stage)
		}
		if stage == nil {
			return fmt.Errorf("配方快照缺失阶段 %s，无法重摇", u.Stage)
		}
		next := newAttemptUnit(run, item, stage, u.AttemptNo+1)
		if err := e.store.CreateUnit(ctx, next); err != nil {
			return err
		}
		emit(u.Status, UnitStatusPending, "重摇")
		// run 已终态则复活回 running（续跑通道与断点恢复同一条：只跑非终态 cell）。
		if IsRunTerminal(run.Status) {
			if ok, err := e.store.UpdateRunStatus(ctx, run.ID, run.Status, RunStatusRunning, ""); err != nil || !ok {
				return fmt.Errorf("run 复活失败: %v", err)
			}
		}
		e.Kick()
		return nil
	default:
		return fmt.Errorf("未知 unit 操作: %q", action)
	}
}

// RunSummary 是 workflow_status 的摘要（run 状态 + 单元计数 + 失败原因聚合）。
type RunSummary struct {
	RunID         string         `json:"run_id"`
	Status        string         `json:"status"`
	PauseReason   string         `json:"pause_reason,omitempty"`
	RecipeID      string         `json:"recipe_id"`
	RecipeVersion int            `json:"recipe_version"`
	SessionID     string         `json:"session_id"`
	Counts        map[string]int `json:"unit_counts"` // 按 cell 最新 attempt 的终态计数
	Total         int            `json:"total"`
	FailReasons   []string       `json:"fail_reasons,omitempty"` // 失败原因聚合 topN
	Spent         int            `json:"spent"`
	BudgetCap     int            `json:"budget_cap"`
	CreatedAt     time.Time      `json:"created_at"`
	FinishedAt    *time.Time     `json:"finished_at,omitempty"`
}

// RunSummaryOf 组装运行摘要。
func (e *Engine) RunSummaryOf(ctx context.Context, runID string) (*RunSummary, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return nil, err
	}
	cells := latestCells(units)
	sum := &RunSummary{
		RunID: run.ID, Status: run.Status, PauseReason: run.PauseReason,
		RecipeID: run.RecipeID, RecipeVersion: run.RecipeVersion, SessionID: run.SessionID,
		Counts: map[string]int{}, Total: len(cells),
		Spent: run.SpentEstimate, BudgetCap: run.BudgetCap,
		CreatedAt: run.CreatedAt, FinishedAt: run.FinishedAt,
	}
	reasonCount := map[string]int{}
	for _, c := range cells {
		sum.Counts[c.Status]++
		if c.Status == UnitStatusFailed || c.Status == UnitStatusNeedsReview {
			r := strings.TrimSpace(c.RejectReason)
			if r == "" {
				r = "未通过校验"
			}
			reasonCount[r]++
		}
	}
	for r := range reasonCount {
		sum.FailReasons = append(sum.FailReasons, r)
	}
	// 简单排序：出现次数降序，取 topN。
	for i := 0; i < len(sum.FailReasons); i++ {
		for j := i + 1; j < len(sum.FailReasons); j++ {
			if reasonCount[sum.FailReasons[j]] > reasonCount[sum.FailReasons[i]] {
				sum.FailReasons[i], sum.FailReasons[j] = sum.FailReasons[j], sum.FailReasons[i]
			}
		}
	}
	if len(sum.FailReasons) > 5 {
		sum.FailReasons = sum.FailReasons[:5]
	}
	return sum, nil
}

// latestCells 取每个 (item, stage) 的最新 attempt 行（cell 口径）。
func latestCells(units []*Unit) []*Unit {
	best := map[string]*Unit{}
	for _, u := range units {
		key := u.ItemID + "\x00" + u.Stage
		if cur, ok := best[key]; !ok || u.AttemptNo > cur.AttemptNo {
			best[key] = u
		}
	}
	out := make([]*Unit, 0, len(best))
	for _, u := range best {
		out = append(out, u)
	}
	return out
}

// emitEvent 送事件流（nil 安全）。
func (e *Engine) emitEvent(sessionID, text, detailJSON string) {
	if e.emit == nil || sessionID == "" {
		return
	}
	e.emit(sessionID, text, detailJSON)
}

// notifyRunFinal 终态回调（webhook，best-effort）。
func (e *Engine) notifyRunFinal(runID string) {
	if e.runFinal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	e.runFinal(run)
}
