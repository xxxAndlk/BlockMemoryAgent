package workflow

// executor.go 调度与单位执行（开发文档 §5.2/§5.3/§5.4）。
//
// 单位执行序列：合成提示词+锚点 → 调生成工具 → 程序校验（免费）→ 模型评委（便宜）→
// 重摇 / 采纳 / 标 needs_review；采纳件按 stage.anchor_out 写回运行期锚点池。
// 调度：件间并发 = run.concurrency（信号量），件内阶段串行（锚点传播依赖）；
// 全部边界检查 run 状态（paused/aborted/failed 在阶段/attempt 边界停下）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// cellRef 是一个待执行格（件, 阶段, 最新 attempt 行）。
type cellRef struct {
	item  RosterItem
	stage *Stage
	unit  *Unit
}

// setRunRole 缓存 run 的发起角色（重启后丢失 → 空，dispatch 走程序化默认放行）。
func (e *Engine) setRunRole(runID, roleID string) {
	if roleID == "" {
		return
	}
	e.anchorsMu.Lock()
	if e.runRoles == nil {
		e.runRoles = map[string]string{}
	}
	e.runRoles[runID] = roleID
	e.anchorsMu.Unlock()
}

func (e *Engine) runRole(runID string) string {
	e.anchorsMu.Lock()
	defer e.anchorsMu.Unlock()
	return e.runRoles[runID]
}

// scheduleRun 一个 run 的调度循环（由 beginRun 启动，独占 runID）。
func (e *Engine) scheduleRun(ctx context.Context, runID string) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		e.logError(ctx, fmt.Sprintf("[workflow] 调度 run %s 读取失败", runID), err)
		return
	}
	if IsRunTerminal(run.Status) {
		return
	}
	recipe, err := ParseRecipe([]byte(run.RecipeYAML))
	if err != nil {
		e.failRun(ctx, run, PauseCircuitBreaker, "配方快照损坏："+err.Error())
		return
	}
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		e.logError(ctx, fmt.Sprintf("[workflow] 调度 run %s 读 unit 失败", runID), err)
		return
	}
	// 锚点池重建（重启恢复：从已采纳的 anchor_out 单元恢复运行期锚点）。
	e.seedAnchorPoolFromUnits(run, recipe, units)

	// 收集待执行 cell：名册顺序 × 阶段顺序；终态 cell 跳过（done 不重跑，不重不漏）。
	byItem := map[string][]cellRef{}
	var order []string
	for _, item := range run.Roster {
		for _, stage := range item.StagesFor(recipe.Stages) {
			latest := latestOf(units, item.ID, stage.ID)
			if latest == nil || IsUnitTerminal(latest.Status) {
				continue
			}
			if _, seen := byItem[item.ID]; !seen {
				order = append(order, item.ID)
			}
			byItem[item.ID] = append(byItem[item.ID], cellRef{item: item, stage: stage, unit: latest})
		}
	}
	if len(order) == 0 {
		e.maybeFinalizeRun(ctx, runID)
		return
	}

	conc := run.Concurrency
	if conc <= 0 {
		conc = e.cfg.DefaultConcurrency
	}
	sem := make(chan struct{}, maxInt(1, conc))
	var wg sync.WaitGroup
	for _, itemID := range order {
		group := byItem[itemID]
		wg.Add(1)
		go func(g []cellRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, c := range g {
				if ctx.Err() != nil {
					return
				}
				// 阶段边界闸：run 不在 running（paused/aborted/failed）即停。
				if st := e.runStatus(ctx, runID); st != RunStatusRunning {
					return
				}
				e.executeCell(ctx, run, recipe, c.item, c.stage, c.unit)
			}
		}(group)
	}
	wg.Wait()
	e.maybeFinalizeRun(ctx, runID)
}

// executeCell 执行一个 cell 的全部重摇序列（attempt 循环）。
func (e *Engine) executeCell(ctx context.Context, run *Run, recipe *Recipe, item RosterItem, stage *Stage, cur *Unit) {
	attempt := cur
	for {
		if ctx.Err() != nil {
			return
		}
		// claim：pending → running（乐观迁移；失败=已被并发/人审挪走）。
		ok, err := e.store.TransitionUnit(ctx, attempt.ID, []string{UnitStatusPending}, UnitStatusRunning, UnitPatch{ItemName: item.Name})
		if err != nil {
			e.logError(ctx, fmt.Sprintf("[workflow] claim unit %s 失败", attempt.ID), err)
			return
		}
		if !ok {
			return
		}
		e.emitUnit(run, attempt, UnitStatusPending, UnitStatusRunning, "", item)
		out := e.runAttempt(ctx, run, recipe, item, stage, attempt)
		switch out.Status {
		case UnitStatusDone, UnitStatusNeedsReview:
			e.afterCellSettled(ctx, run.ID)
			return
		case UnitStatusFailed:
			if out.Rerollable {
				attempt = newAttemptUnit(run, item, stage, attempt.AttemptNo+1)
				if err := e.store.CreateUnit(ctx, attempt); err != nil {
					e.logError(ctx, fmt.Sprintf("[workflow] 建重摇 attempt 失败 %s", attempt.ID), err)
					e.afterCellSettled(ctx, run.ID)
					return
				}
				e.emitUnit(run, attempt, "", UnitStatusPending, "重摇", item)
				continue
			}
			// 重摇耗尽：升级通道（配方声明 escalate 段时派真子 Agent）。
			e.maybeEscalate(ctx, run, recipe, item, stage, attempt)
			e.afterCellSettled(ctx, run.ID)
			return
		default:
			return
		}
	}
}

// attemptOutcome 是一次 attempt 的结算结果。
type attemptOutcome struct {
	Status     string // done | needs_review | failed
	Rerollable bool   // failed 且还有重摇额度
}

// settleCtx 是结算写路径的 ctx：解除上游取消（abort/停机时在途 attempt 仍需把状态落回表，
// 否则 unit 永远停在 running——底账一致性优先于取消传播），带 5s 超时兜底。
func settleCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// runAttempt 执行一次 attempt（合成 → 生成 → 程序校验 → 评委 → 结算）。
func (e *Engine) runAttempt(ctx context.Context, run *Run, recipe *Recipe, item RosterItem, stage *Stage, unit *Unit) attemptOutcome {
	deliv := recipe.EffectiveDeliverable(stage)
	attemptsRemain := unit.AttemptNo <= recipe.RerollLimit()
	settleFail := func(reason string, cr CheckResultJSON, artPath string) attemptOutcome {
		crj := MarshalCheckResult(cr)
		patch := UnitPatch{RejectReason: &reason}
		if crj != "" {
			patch.CheckResult = &crj
		}
		if artPath != "" {
			patch.ArtifactPath = &artPath
		}
		wctx, cancel := settleCtx(ctx)
		ok, err := e.store.TransitionUnit(wctx, unit.ID, []string{UnitStatusRunning}, UnitStatusFailed, patch)
		cancel()
		if err != nil || !ok {
			e.logError(ctx, fmt.Sprintf("[workflow] 结算 unit %s=failed 失败", unit.ID), err)
		}
		e.emitUnit(run, unit, UnitStatusRunning, UnitStatusFailed, reason, item)
		return attemptOutcome{Status: UnitStatusFailed, Rerollable: attemptsRemain}
	}
	settleReview := func(reason string, cr CheckResultJSON, artPath string) attemptOutcome {
		crj := MarshalCheckResult(cr)
		patch := UnitPatch{RejectReason: &reason}
		if crj != "" {
			patch.CheckResult = &crj
		}
		if artPath != "" {
			patch.ArtifactPath = &artPath
		}
		wctx, cancel := settleCtx(ctx)
		ok, err := e.store.TransitionUnit(wctx, unit.ID, []string{UnitStatusRunning}, UnitStatusNeedsReview, patch)
		cancel()
		if err != nil || !ok {
			e.logError(ctx, fmt.Sprintf("[workflow] 结算 unit %s=needs_review 失败", unit.ID), err)
		}
		e.emitUnit(run, unit, UnitStatusRunning, UnitStatusNeedsReview, reason, item)
		return attemptOutcome{Status: UnitStatusNeedsReview}
	}

	// 1. 合成（提示词 + 锚点参数）。
	anchors := e.resolveAnchors(run, recipe, item, stage)
	synth, err := SynthArgs(recipe, item, stage, anchors, unit.AttemptNo, run.Workdir)
	if err != nil {
		return settleFail("合成失败："+err.Error(), CheckResultJSON{Error: err.Error()}, "")
	}

	// 2. 生成（ctx 带 session/workdir/stop，程序化路径经 tool.Registry.Dispatch）。
	result, derr := e.tools.Dispatch(e.dispatchCtx(ctx, run, recipe), recipe.Tool, synth.Args)
	e.spend(ctx, run.ID, 1)
	if derr != nil {
		return settleFail(fmt.Sprintf("生成工具调度失败：%v", derr), CheckResultJSON{Error: derr.Error()}, "")
	}
	if result == nil || !result.Success {
		msg := "生成失败"
		if result != nil && strings.TrimSpace(result.Error) != "" {
			msg = "生成失败：" + result.Error
		}
		return settleFail(msg, CheckResultJSON{Error: msg}, "")
	}
	art := synth.OutputPath
	if p := artifactPathFromResult(result); p != "" {
		art = normalizeRelPath(p) // 工具回显路径为准（save_as 约定下应与合成一致）
		synth.OutputPath = art
	}

	// 3. 程序校验（免费）。
	prog := CheckProgram(run.Workdir, synth, deliv, recipe.ProgramChecks())
	cr := CheckResultJSON{Program: &prog}
	if !prog.Pass {
		reason := "程序校验未过：" + progSummary(prog)
		if attemptsRemain {
			return settleFail(reason, cr, art)
		}
		if itemPass(prog, "exists") {
			return settleReview(reason+"（重摇耗尽）", cr, art)
		}
		return settleFail(reason+"（重摇耗尽，无产物）", cr, art)
	}

	// 4. 模型评委（便宜；程序过才叫）。
	if len(recipe.Check.Judge) > 0 {
		judge := JudgeUnit(ctx, e.models, e.cfg.JudgeTimeout, recipe, item, stage, synth)
		e.spend(ctx, run.ID, 1)
		cr.Judge = &judge
		if judge.Err != "" {
			// 评委不可用（调用失败/解析失败）：不重摇（重摇治不了评委），直接人审。
			return settleReview("评委不可用："+judge.Err, cr, art)
		}
		if !judge.Pass {
			reason := "评委未通过：" + judgeSummary(judge)
			if attemptsRemain {
				return settleFail(reason, cr, art)
			}
			return settleReview(reason+"（重摇耗尽）", cr, art)
		}
	}

	// 5. 采纳：漂移检测（best-effort）→ 落 unit → 锚点传播。
	if d := e.drift.Observe(ctx, e.embedder, stage.ID+"/"+recipe.ID, synth.PromptText); d != nil {
		cr.Drift = d
	}
	crj := MarshalCheckResult(cr)
	patch := UnitPatch{ArtifactPath: &art}
	if crj != "" {
		patch.CheckResult = &crj
	}
	wctx, cancel := settleCtx(ctx)
	ok, err := e.store.TransitionUnit(wctx, unit.ID, []string{UnitStatusRunning}, UnitStatusDone, patch)
	cancel()
	if err != nil || !ok {
		e.logError(ctx, fmt.Sprintf("[workflow] 采纳 unit %s 失败", unit.ID), err)
		return attemptOutcome{Status: UnitStatusFailed}
	}
	e.emitUnit(run, unit, UnitStatusRunning, UnitStatusDone, "采纳", item)
	if stage.AnchorOut {
		e.putAnchor(run.ID, item.ID, stage.ID, art)
	}
	return attemptOutcome{Status: UnitStatusDone}
}

// ---------------------------------------------------------------------------
// 调度边界：预算 / 熔断 / 终态

// afterCellSettled 在一个 cell 到达终态后做边界检查：预算闸 → 熔断 → 终态收口。
func (e *Engine) afterCellSettled(ctx context.Context, runID string) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil || run == nil {
		return
	}
	if run.Status == RunStatusRunning {
		// 预算闸：spent 达上限 → 暂停（pause_reason=budget_exceeded），resume 需提额。
		if run.SpentEstimate >= run.BudgetCap {
			if ok, err := e.store.UpdateRunStatus(ctx, runID, RunStatusRunning, RunStatusPaused, PauseBudgetExceeded); err == nil && ok {
				ev := RunEvent{RunID: runID, Status: RunStatusPaused, Reason: PauseBudgetExceeded,
					Note: fmt.Sprintf("已消耗 %d/%d 次轻量调用，续跑需提额", run.SpentEstimate, run.BudgetCap)}
				e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
				e.logInfo(ctx, "[workflow] "+ev.Text())
				return
			}
		}
		// 早期熔断（fail_fast）：前 5 个到达终态的 cell 中失败（failed/needs_review）≥3 →
		// run failed（结论"配方疑似有问题，回打样期"，不把整批烧完——设计报告 §5.4 制度化）。
		if run.FailFast {
			if firstFive := e.firstTerminalCells(ctx, runID, 5); len(firstFive) >= 5 {
				unsucc := 0
				for _, c := range firstFive {
					if c.Status == UnitStatusFailed || c.Status == UnitStatusNeedsReview {
						unsucc++
					}
				}
				if unsucc >= 3 {
					e.failRun(ctx, run, PauseCircuitBreaker,
						fmt.Sprintf("早期熔断：前 5 个完成单元中 %d 个未通过，配方疑似有问题（回打样期修配方）", unsucc))
					return
				}
			}
		}
	}
	e.maybeFinalizeRun(ctx, runID)
}

// firstTerminalCells 返回按终态时间升序的前 n 个终态 cell。
func (e *Engine) firstTerminalCells(ctx context.Context, runID string, n int) []*Unit {
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return nil
	}
	var terminal []*Unit
	for _, c := range latestCells(units) {
		if IsUnitTerminal(c.Status) {
			terminal = append(terminal, c)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].UpdatedAt.Before(terminal[j].UpdatedAt) })
	if len(terminal) > n {
		terminal = terminal[:n]
	}
	return terminal
}

// maybeFinalizeRun 全部 cell 终态且无 running/pending → run done（从 running/paused 迁移）。
func (e *Engine) maybeFinalizeRun(ctx context.Context, runID string) error {
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return err
	}
	for _, c := range latestCells(units) {
		if !IsUnitTerminal(c.Status) {
			return nil
		}
	}
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunStatusRunning && run.Status != RunStatusPaused {
		return nil
	}
	ok, err := e.store.UpdateRunStatus(ctx, runID, run.Status, RunStatusDone, "")
	if err != nil || !ok {
		return err
	}
	counts := map[string]int{}
	for _, c := range latestCells(units) {
		counts[c.Status]++
	}
	ev := RunEvent{RunID: runID, Status: RunStatusDone,
		Note: fmt.Sprintf("完成 %d / 待审 %d / 失败 %d / 跳过 %d", counts[UnitStatusDone], counts[UnitStatusNeedsReview], counts[UnitStatusFailed], counts[UnitStatusSkipped])}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	e.logInfo(ctx, "[workflow] "+ev.Text())
	e.dropRunState(runID)
	e.notifyRunFinal(runID)
	return nil
}

// failRun 把 run 置 failed（熔断/快照损坏）。
func (e *Engine) failRun(ctx context.Context, run *Run, reason, note string) {
	ok, err := e.store.UpdateRunStatus(ctx, run.ID, RunStatusRunning, RunStatusFailed, reason)
	if err != nil || !ok {
		return
	}
	ev := RunEvent{RunID: run.ID, Status: RunStatusFailed, Reason: reason, Note: note}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
	e.logInfo(ctx, "[workflow] "+ev.Text())
	e.dropRunState(run.ID)
	e.notifyRunFinal(run.ID)
}

// runStatus 读取 run 当前状态（边界闸）。
func (e *Engine) runStatus(ctx context.Context, runID string) string {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return ""
	}
	return run.Status
}

// ---------------------------------------------------------------------------
// 锚点池 / 消耗 / 分发 ctx

// anchorKey 运行期锚点池键（run + 件 + 阶段）。
type anchorKey struct {
	runID  string
	itemID string
	stage  string
}

func (e *Engine) putAnchor(runID, itemID, stageID, path string) {
	e.anchorsMu.Lock()
	defer e.anchorsMu.Unlock()
	if e.anchors == nil {
		e.anchors = map[anchorKey]string{}
	}
	e.anchors[anchorKey{runID: runID, itemID: itemID, stage: stageID}] = path
}

// resolveAnchors 解析锚点池：静态配方锚点（槽位替换）+ 该件运行期锚点（前置阶段采纳件）。
func (e *Engine) resolveAnchors(run *Run, recipe *Recipe, item RosterItem, stage *Stage) map[string]string {
	out := make(map[string]string, len(recipe.Anchors)+len(stage.AnchorIn))
	for name, tpl := range recipe.Anchors {
		out[name] = expandTemplate(tpl, item, stage.ID)
	}
	e.anchorsMu.Lock()
	for k, v := range e.anchors {
		if k.runID == run.ID && k.itemID == item.ID {
			out[k.stage] = v // 运行期锚点（该件已采纳的前置阶段）覆盖同名静态锚点
		}
	}
	e.anchorsMu.Unlock()
	return out
}

// seedAnchorPoolFromUnits 从已采纳单元重建锚点池（重启恢复路径）。
func (e *Engine) seedAnchorPoolFromUnits(run *Run, recipe *Recipe, units []*Unit) {
	for _, u := range units {
		if u.Status != UnitStatusDone || u.ArtifactPath == "" {
			continue
		}
		if st := recipe.StageByID(u.Stage); st != nil && st.AnchorOut {
			e.putAnchor(run.ID, u.ItemID, u.Stage, u.ArtifactPath)
		}
	}
}

// seedAnchorPool 采纳/复活路径下补种一个 unit 的锚点（UnitAction accept）。
func (e *Engine) seedAnchorPool(ctx context.Context, run *Run, u *Unit) {
	recipe, err := ParseRecipe([]byte(run.RecipeYAML))
	if err != nil || recipe == nil {
		return
	}
	if st := recipe.StageByID(u.Stage); st != nil && st.AnchorOut && u.ArtifactPath != "" {
		e.putAnchor(run.ID, u.ItemID, u.Stage, u.ArtifactPath)
	}
}

// dropRunState 清理 run 的内存态（锚点池/角色缓存）。
func (e *Engine) dropRunState(runID string) {
	e.anchorsMu.Lock()
	for k := range e.anchors {
		if k.runID == runID {
			delete(e.anchors, k)
		}
	}
	delete(e.runRoles, runID)
	e.anchorsMu.Unlock()
}

// spend 记录并累加消耗估算（best-effort）。
func (e *Engine) spend(ctx context.Context, runID string, delta int) {
	if delta <= 0 {
		return
	}
	if _, err := e.store.AddRunSpent(ctx, runID, delta); err != nil {
		e.logError(ctx, fmt.Sprintf("[workflow] run %s 记录消耗失败", runID), err)
	}
}

// dispatchCtx 组装生成工具调用的 ctx（session/workdir 定位工作区容器；
// agentID=会话 ID 使工具门 scope 与会话顶层一致；roleID 供门/预挂豁免判定）。
func (e *Engine) dispatchCtx(ctx context.Context, run *Run, recipe *Recipe) context.Context {
	c := tool.WithSessionID(ctx, run.SessionID)
	c = tool.WithAgentID(c, run.SessionID)
	c = tool.WithWorkDir(c, run.Workdir)
	c = tool.WithStopContext(c, e.stopCtx)
	if role := e.runRole(run.ID); role != "" {
		c = tool.WithRoleID(c, role)
	}
	return c
}

// emitUnit 发 unit 状态迁移事件。
func (e *Engine) emitUnit(run *Run, u *Unit, from, to, reason string, item RosterItem) {
	ev := UnitEvent{
		RunID: run.ID, UnitID: u.ID, ItemID: u.ItemID, ItemName: item.Name,
		Stage: u.Stage, AttemptNo: u.AttemptNo, From: from, To: to, Reason: reason,
	}
	e.emitEvent(run.SessionID, ev.Text(), ev.DetailJSON())
}

// ---------------------------------------------------------------------------
// 小工具

func latestOf(units []*Unit, itemID, stage string) *Unit {
	var best *Unit
	for _, u := range units {
		if u.ItemID != itemID || u.Stage != stage {
			continue
		}
		if best == nil || u.AttemptNo > best.AttemptNo {
			best = u
		}
	}
	return best
}

func itemPass(prog ProgramResult, key string) bool {
	for _, it := range prog.Items {
		if it.Key == key {
			return it.Pass
		}
	}
	return false
}

func progSummary(prog ProgramResult) string {
	var fails []string
	for _, it := range prog.Items {
		if !it.Pass {
			fails = append(fails, it.Key+": "+it.Reason)
		}
	}
	return truncateStr(strings.Join(fails, "；"), 300)
}

func judgeSummary(j JudgeResult) string {
	var fails []string
	for _, it := range j.Items {
		if !it.Pass {
			fails = append(fails, it.Item+": "+it.Reason)
		}
	}
	if len(fails) == 0 {
		return j.Err
	}
	return truncateStr(strings.Join(fails, "；"), 300)
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// artifactPathFromResult 从工具结果提取产物路径：优先 Result.Artifacts（ShowArtifact 登记），
// 回退解析 Output 里的 JSON（MCP 工具的输出约定带 path 字段，可能是 `<untrusted_data>` 围栏文本）。
func artifactPathFromResult(result *tool.Result) string {
	if result == nil {
		return ""
	}
	for _, a := range result.Artifacts {
		if strings.TrimSpace(a.Path) != "" {
			return a.Path
		}
	}
	text := result.Output
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &payload); err != nil {
		return ""
	}
	if p, ok := payload["path"].(string); ok {
		return p
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
