package flow

// engine.go 流程引擎：图解释执行（逐节点走，branch 决定下一跳，节点间串行）。
// 纪律照 workflow 引擎先例：权威全在表（flow_runs/flow_node_runs），无内存权威；
// 重启经 Start() 恢复（running 的 node_run 标回 pending、running 的 run 重走调度循环，
// 已完成的节点经 node_run 状态幂等跳过）；approval 节点把 run 挂到 waiting_approval，
// 经 DecideNode 裁决后恢复。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
)

// Config 是引擎运行参数（接入 config.yaml flow: 段）。
type Config struct {
	MaxActiveRuns int           // 全局活跃 run 闸（默认 8；flow 节点轻量，闸比 workflow 宽）
	NodeTimeout   time.Duration // 单节点执行超时（llm/tool 调用，默认 120s）
}

// ModelSource 是 llm 节点的模型面（*model.ModelFactory 满足；测试用假实现）。
type ModelSource interface {
	// GetModel 按角色 ID 取该角色模型绑定的客户端。
	GetModel(ctx context.Context, roleID string) (model.LLMClient, error)
	// GetLightweightModel 取轻量模型客户端（llm 节点 model 留空时用）。
	GetLightweightModel(ctx context.Context) (model.LLMClient, error)
}

// ToolDispatcher 是 tool 节点的工具面（*tool.Registry 满足；测试用假实现）。
type ToolDispatcher interface {
	Dispatch(ctx context.Context, name string, args map[string]any) (*tool.Result, error)
}

// Engine 是流程引擎本体。
type Engine struct {
	cfg    Config
	store  Store
	tools  ToolDispatcher
	models ModelSource
	log    *logger.Logger

	stopCtx    context.Context
	stopCancel context.CancelFunc
	stopOnce   sync.Once
	wg         sync.WaitGroup
	startOnce  sync.Once

	mu     sync.Mutex
	active map[string]*runCtl // runID → 调度控制（活跃循环）
}

// runCtl 是一个活跃 run 的调度控制（取消传播到在途调用）。
type runCtl struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewEngine 构造引擎（cfg 零值字段按默认补齐）。
func NewEngine(cfg Config, store Store, tools ToolDispatcher, models ModelSource) *Engine {
	if cfg.MaxActiveRuns <= 0 {
		cfg.MaxActiveRuns = 8
	}
	if cfg.NodeTimeout <= 0 {
		cfg.NodeTimeout = 120 * time.Second
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
	}
}

// SetLogger 注入结构化日志器（nil 回退标准库 log，照 workflow/dag 先例）。
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

// Start 启动引擎：重启恢复（running 的 node_run 标回 pending + running 的 run 重走）。
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.recoverRuns(ctx)
		}()
	})
}

// Stop 停止引擎：取消全部在途执行并等待 goroutine 退出（幂等）。
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

// recoverRuns 重启恢复：running 节点标回 pending；running 的 run 重新入调度
// （walk 幂等：已 success/waiting/failed 的 node_run 直接跳过）。
func (e *Engine) recoverRuns(ctx context.Context) {
	n, err := e.store.ResetRunningNodes(ctx)
	if err != nil {
		e.logError(ctx, "[flow] 重启恢复：复位 running 节点失败", err)
	} else if n > 0 {
		e.logInfo(ctx, fmt.Sprintf("[flow] 重启恢复：%d 个中断节点标回 pending", n))
	}
	ids, err := e.store.RunningRunIDs(ctx)
	if err != nil {
		e.logError(ctx, "[flow] 重启恢复：读取 running run 失败", err)
		return
	}
	for _, id := range ids {
		e.beginRun(id)
	}
	e.Kick() // pending 队列可能因重启遗留未提升
}

// ---------------------------------------------------------------------------
// 建 run

// CreateSpec 是发起一次流程执行的入参（POST /api/flow/runs）。
type CreateSpec struct {
	Workdir   string         // flow 定义所在工作区（必填）
	FlowID    string         // flow 定义 id（必填）
	Inputs    map[string]any // start 节点输入
	SessionID string         // 发起会话（可选锚点）
}

// CreateRun 建 run：加载 flow → 快照 → 校验 start 必填输入 → 建行（全部 node_run
// pending）→ 入调度。引擎异步执行，立即返回。
func (e *Engine) CreateRun(ctx context.Context, spec CreateSpec) (*Run, error) {
	if strings.TrimSpace(spec.Workdir) == "" {
		return nil, fmt.Errorf("workdir 必填")
	}
	if strings.TrimSpace(spec.FlowID) == "" {
		return nil, fmt.Errorf("flow_id 必填")
	}
	flow, err := LoadFlow(spec.Workdir, spec.FlowID)
	if err != nil {
		return nil, err
	}
	if err := validateRunInputs(flow, spec.Inputs); err != nil {
		return nil, err
	}
	snapshot, err := MarshalFlow(flow)
	if err != nil {
		return nil, err
	}
	run := &Run{
		ID:          newRunID(),
		FlowID:      flow.ID,
		FlowName:    flow.Name,
		FlowVersion: flow.Version,
		FlowJSON:    string(snapshot),
		Inputs:      spec.Inputs,
		Status:      RunStatusPending, // 全局活跃闸内排队，Kick 提升为 running
		SessionID:   spec.SessionID,
		Workdir:     spec.Workdir,
	}
	nodeRuns := make([]*NodeRun, 0, len(flow.Nodes))
	for _, n := range flow.Nodes {
		nodeRuns = append(nodeRuns, &NodeRun{
			ID:       nodeRunID(run.ID, n.ID),
			RunID:    run.ID,
			NodeID:   n.ID,
			NodeType: n.Type,
			Status:   NodeStatusPending,
		})
	}
	if err := e.store.CreateRun(ctx, run, nodeRuns); err != nil {
		return nil, err
	}
	e.logInfo(ctx, fmt.Sprintf("[flow] run %s 创建（flow=%s v%d, nodes=%d）", run.ID, flow.ID, flow.Version, len(nodeRuns)))
	e.Kick()
	return run, nil
}

// validateRunInputs 校验 start 必填输入齐全。
func validateRunInputs(flow *Flow, inputs map[string]any) error {
	start := (*Node)(nil)
	for _, n := range flow.Nodes {
		if n.Type == NodeTypeStart {
			start = n
			break
		}
	}
	if start == nil {
		return fmt.Errorf("flow %s 缺少 start 节点", flow.ID)
	}
	for _, spec := range start.paramSpecs("inputs") {
		if spec.Required {
			v, ok := inputs[spec.Key]
			if !ok || v == nil || (spec.Type == "string" && strings.TrimSpace(stringify(v)) == "") {
				return fmt.Errorf("缺少必填输入 %q（start.inputs）", spec.Key)
			}
		}
	}
	return nil
}

// newRunID 生成紧凑 run ID（TEXT 主键，≤32 字符）。
func newRunID() string {
	return fmt.Sprintf("fl-%d", time.Now().UnixNano()/1e6)
}

// nodeRunID 生成 node_run 行 ID（runID/nodeID）。
func nodeRunID(runID, nodeID string) string {
	return fmt.Sprintf("%s/%s", runID, nodeID)
}

// ---------------------------------------------------------------------------
// 运行控制

// RunFilter 是运行列表过滤条件（零值=全部）。
type RunFilter struct {
	FlowID string
	Status string
	Limit  int
}

// ListRuns 运行列表。
func (e *Engine) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	return e.store.ListRuns(ctx, f)
}

// RunDetail 是运行详情（调试面板数据源）。
type RunDetail struct {
	Run       *Run       `json:"run"`
	NodeRuns  []*NodeRun `json:"node_runs"`
}

// GetRunDetail 返回 run 行 + 全部 node_run。
func (e *Engine) GetRunDetail(ctx context.Context, runID string) (*RunDetail, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	nrs, err := e.store.ListNodeRuns(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &RunDetail{Run: run, NodeRuns: nrs}, nil
}

// AbortRun 中止运行（running/waiting_approval → aborted；在途执行取消）。
func (e *Engine) AbortRun(ctx context.Context, runID string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if IsRunTerminal(run.Status) {
		return fmt.Errorf("run %s 已是终态 %s", runID, run.Status)
	}
	ok, err := e.store.UpdateRunStatus(ctx, runID, run.Status, RunStatusAborted, nil, "")
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
	e.logInfo(ctx, fmt.Sprintf("[flow] run %s 已中止", runID))
	return nil
}

// DecideNode 裁决 approval 节点：approved → 节点成功（产出 approved/comment）继续走；
// rejected → 节点失败，run 失败。仅 waiting 状态可裁决。
func (e *Engine) DecideNode(ctx context.Context, runID, nodeID string, approved bool, comment string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunStatusWaitingApprove {
		return fmt.Errorf("run %s 状态 %s，不在等待裁决", runID, run.Status)
	}
	id := nodeRunID(runID, nodeID)
	nr, err := e.store.GetNodeRun(ctx, id)
	if err != nil {
		return err
	}
	if nr.Status != NodeStatusWaiting {
		return fmt.Errorf("节点 %s 状态 %s，不在等待裁决", nodeID, nr.Status)
	}
	if approved {
		if err := e.store.FinishNodeRun(ctx, id, NodeStatusSuccess, nil, map[string]any{
			"approved": true, "comment": comment,
		}, ""); err != nil {
			return err
		}
		// waiting_approval → running（乐观迁移；被并发 abort 则不恢复）。
		if ok, err := e.store.UpdateRunStatus(ctx, runID, RunStatusWaitingApprove, RunStatusRunning, nil, ""); err != nil {
			return err
		} else if !ok {
			return nil // run 已被并发迁移（如 abort），裁决记录保留但不恢复。
		}
		e.logInfo(ctx, fmt.Sprintf("[flow] run %s 节点 %s 裁决通过，继续执行", runID, nodeID))
		e.Kick()
		return nil
	}
	// 拒绝 = 终局：节点失败、run failed（落拒绝理由），剩余 pending 剪枝。
	reason := strings.TrimSpace(comment)
	if reason == "" {
		reason = "人工拒绝"
	}
	if err := e.store.FinishNodeRun(ctx, id, NodeStatusFailed, nil, map[string]any{
		"approved": false, "comment": comment,
	}, reason); err != nil {
		return err
	}
	if _, err := e.store.UpdateRunStatus(ctx, runID, RunStatusWaitingApprove, RunStatusFailed, nil, reason); err != nil {
		return err
	}
	e.logInfo(ctx, fmt.Sprintf("[flow] run %s 节点 %s 裁决拒绝：%s", runID, nodeID, reason))
	e.skipRemaining(ctx, runID)
	return nil
}

// Kick 尝试把队列中的 run 拉入调度：先恢复 running（重启/续跑），再按 FIFO 提升
// pending（全局活跃闸：超出的 run 保持排队）。
func (e *Engine) Kick() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	e.mu.Lock()
	slots := e.cfg.MaxActiveRuns - len(e.active)
	e.mu.Unlock()
	if slots <= 0 {
		return
	}
	// 先恢复 running（重启/恢复），再按 FIFO 提升 pending。
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
		e.logError(ctx, "[flow] kick 读取 running run 失败", err)
	}
	if slots <= 0 {
		return
	}
	pending, err := e.store.PendingRunIDs(ctx, slots)
	if err != nil {
		e.logError(ctx, "[flow] kick 读取 pending run 失败", err)
		return
	}
	for _, id := range pending {
		if slots <= 0 {
			break
		}
		ok, err := e.store.UpdateRunStatus(ctx, id, RunStatusPending, RunStatusRunning, nil, "")
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
		}()
		e.walkRun(ctx, runID)
	}()
	return true
}

// ---------------------------------------------------------------------------
// 图解释执行

// walkRun 是 run 的调度循环：每轮从表里读 node_run 权威状态，挑一个依赖全 success 的
// pending 节点执行；answer 成功即 run done；无候选节点（走不通/分支剪枝）时剩余
// pending 统一标 skipped 并按是否有 answer 成功定终态。approval 挂起时 run 置
// waiting_approval 并退出循环，等 DecideNode 恢复。
func (e *Engine) walkRun(ctx context.Context, runID string) {
	for {
		run, err := e.store.GetRun(ctx, runID)
		if err != nil {
			e.logError(ctx, fmt.Sprintf("[flow] run %s 读取失败", runID), err)
			return
		}
		if IsRunTerminal(run.Status) {
			return
		}
		flow, err := ParseFlow([]byte(run.FlowJSON))
		if err != nil {
			e.failRun(ctx, run, fmt.Errorf("快照解析失败: %w", err))
			return
		}
		nrs, err := e.store.ListNodeRuns(ctx, runID)
		if err != nil {
			e.logError(ctx, fmt.Sprintf("[flow] run %s 读取节点失败", runID), err)
			return
		}
		statusByNode := make(map[string]string, len(nrs))
		for _, nr := range nrs {
			statusByNode[nr.NodeID] = nr.Status
		}

		// 挑候选：按 flow.Nodes 顺序第一个 pending 且依赖全 success 的节点。
		var next *Node
		for _, n := range flow.Nodes {
			if statusByNode[n.ID] != NodeStatusPending {
				continue
			}
			if depsSuccess(n, statusByNode) {
				next = n
				break
			}
		}
		if next == nil {
			e.finalizeRun(ctx, run, statusByNode)
			return
		}
		cont, err := e.execNode(ctx, run, next, statusByNode)
		if err != nil {
			e.failRun(ctx, run, fmt.Errorf("节点 %s: %w", next.ID, err))
			return
		}
		if !cont {
			return // approval 挂起 / 引擎停止
		}
	}
}

// depsSuccess 依赖节点全部 success 才算就绪。
func depsSuccess(n *Node, statusByNode map[string]string) bool {
	for _, d := range n.DependsOn {
		if statusByNode[d] != NodeStatusSuccess {
			return false
		}
	}
	return true
}

// execNode 执行一个节点并落 node_run 状态。返回 cont=false 表示循环应退出
// （approval 挂起：run 已置 waiting_approval；answer 完成：run 已置 done）。
// inputs 记录解析后的实际入参（调试面板 trace）。
func (e *Engine) execNode(ctx context.Context, run *Run, n *Node, statusByNode map[string]string) (bool, error) {
	// 组运行时变量作用域：全部 success 节点的产出。
	sc, err := e.buildScope(ctx, run.ID)
	if err != nil {
		return false, err
	}
	id := nodeRunID(run.ID, n.ID)
	if err := e.store.StartNodeRun(ctx, id); err != nil {
		return false, err
	}
	// 节点执行带超时与取消（abort/stop 传播）。
	nctx, cancel := context.WithTimeout(toolDispatchCtx(ctx, run), e.cfg.NodeTimeout)
	defer cancel()

	var inputs map[string]any
	var outputs map[string]any
	var execErr error
	waiting := false
	switch n.Type {
	case NodeTypeStart:
		inputs = map[string]any{"inputs": run.Inputs}
		outputs = evalStart(n, run.Inputs)
	case NodeTypeLLM:
		prompt, rerr := resolveString(n.configString("prompt"), sc)
		if rerr != nil {
			execErr = rerr
			break
		}
		inputs = map[string]any{"prompt": prompt}
		if system := n.configString("system"); system != "" {
			if sys, serr := resolveString(system, sc); serr == nil {
				inputs["system"] = sys
			}
		}
		outputs, execErr = e.evalLLM(nctx, n, prompt)
	case NodeTypeTool:
		args, rerr := resolveValue(n.configMap("args"), sc)
		if rerr != nil {
			execErr = rerr
			break
		}
		argMap, _ := args.(map[string]any)
		inputs = map[string]any{"args": argMap}
		outputs, execErr = e.evalTool(nctx, run, n.configString("tool"), argMap)
	case NodeTypeBranch:
		next, berr := evalBranch(n, sc)
		inputs = map[string]any{}
		if berr != nil {
			execErr = berr
		} else {
			// 未选中分支的即时目标标 skipped（更深的剪枝由终态统一标）。
			inputs["next"] = next
			outputs = map[string]any{"_next": next}
			e.skipBranchTargets(ctx, run.ID, n, next, statusByNode)
		}
	case NodeTypeApproval:
		question, _ := resolveString(n.configString("question"), sc)
		inputs = map[string]any{"question": question}
		// 挂起：node_run → waiting，run → waiting_approval，退出循环等裁决。
		if execErr = e.store.FinishNodeRun(ctx, id, NodeStatusWaiting, inputs, nil, ""); execErr == nil {
			_, execErr = e.store.UpdateRunStatus(ctx, run.ID, run.Status, RunStatusWaitingApprove, nil, "")
		}
		waiting = execErr == nil
	case NodeTypeAnswer:
		inputs = map[string]any{"outputs": n.configMap("outputs")}
		outputs, execErr = evalAnswer(n, sc)
	}

	if execErr != nil {
		_ = e.store.FinishNodeRun(ctx, id, NodeStatusFailed, inputs, nil, execErr.Error())
		return false, execErr
	}
	if waiting {
		return false, nil
	}
	if err := e.store.FinishNodeRun(ctx, id, NodeStatusSuccess, inputs, outputs, ""); err != nil {
		return false, err
	}

	// answer 成功即终态：输出落 run 并结束。
	if n.Type == NodeTypeAnswer {
		_, err := e.store.UpdateRunStatus(ctx, run.ID, run.Status, RunStatusDone, outputs, "")
		if err != nil {
			return false, err
		}
		e.logInfo(ctx, fmt.Sprintf("[flow] run %s 完成（answer=%s）", run.ID, n.ID))
		// 剩余 pending 标 skipped。
		e.skipRemaining(ctx, run.ID)
		return false, nil
	}
	return true, nil
}

// buildScope 从 node_run 表重建运行时变量作用域（success 节点的产出）。
func (e *Engine) buildScope(ctx context.Context, runID string) (scope, error) {
	nrs, err := e.store.ListNodeRuns(ctx, runID)
	if err != nil {
		return nil, err
	}
	sc := scope{}
	for _, nr := range nrs {
		if nr.Status == NodeStatusSuccess && len(nr.Outputs) > 0 {
			out := make(map[string]any, len(nr.Outputs))
			for k, v := range nr.Outputs {
				if k == "_next" {
					continue // branch 的路由标记不进变量表
				}
				out[k] = v
			}
			sc[nr.NodeID] = out
		}
	}
	return sc, nil
}

// skipBranchTargets 把 branch 未选中 case 的即时目标节点标 skipped（pending 才标）。
// skipBranchTargets 把 branch 未选中路径的即时目标标 skipped（pending 才标）：
// 全部 case 目标 + default 目标里除 chosen 外的都剪枝（更深的剪枝由终态统一标）。
func (e *Engine) skipBranchTargets(ctx context.Context, runID string, n *Node, chosen string, statusByNode map[string]string) {
	targets := map[string]bool{}
	for _, c := range n.branchCases() {
		targets[c.Next] = true
	}
	if def := strings.TrimSpace(n.configString("default")); def != "" {
		targets[def] = true
	}
	delete(targets, chosen) // 选中路径保留
	for t := range targets {
		if statusByNode[t] == NodeStatusPending {
			if err := e.store.FinishNodeRun(ctx, nodeRunID(runID, t), NodeStatusSkipped, nil, nil, "分支未选中"); err != nil {
				e.logError(ctx, fmt.Sprintf("[flow] run %s 剪枝标记失败", runID), err)
			}
			statusByNode[t] = NodeStatusSkipped
		}
	}
}

// skipRemaining 把 run 里剩余 pending 节点统一标 skipped。
func (e *Engine) skipRemaining(ctx context.Context, runID string) {
	nrs, err := e.store.ListNodeRuns(ctx, runID)
	if err != nil {
		e.logError(ctx, fmt.Sprintf("[flow] run %s 读取剪枝节点失败", runID), err)
		return
	}
	for _, nr := range nrs {
		if nr.Status == NodeStatusPending {
			if err := e.store.FinishNodeRun(ctx, nr.ID, NodeStatusSkipped, nil, nil, "run 已终结"); err != nil {
				e.logError(ctx, fmt.Sprintf("[flow] run %s 剪枝标记失败", runID), err)
			}
		}
	}
}

// finalizeRun 无候选节点时定终态：answer 成功即返回（execNode 内已处理），走到这里
// 说明图走不通（分支剪枝后无就绪节点/图未连到 answer），run failed；剩余 pending 标 skipped。
func (e *Engine) finalizeRun(ctx context.Context, run *Run, statusByNode map[string]string) {
	e.failRun(ctx, run, fmt.Errorf("没有可执行节点（分支走不通或图未连到 answer）"))
}

// failRun 把 run 置 failed 并落错误原因，剩余 pending 标 skipped。
func (e *Engine) failRun(ctx context.Context, run *Run, cause error) {
	if _, err := e.store.UpdateRunStatus(ctx, run.ID, run.Status, RunStatusFailed, nil, cause.Error()); err != nil {
		e.logError(ctx, fmt.Sprintf("[flow] run %s 置失败失败", run.ID), err)
	}
	e.logError(ctx, fmt.Sprintf("[flow] run %s 失败", run.ID), cause)
	e.skipRemaining(ctx, run.ID)
}

// toolDispatchCtx 组装 tool 节点调用的 ctx（session/workdir 定位工作区容器，
// 照 workflow.dispatchCtx 先例；flow 无角色上下文，roleID 不注入）。
func toolDispatchCtx(ctx context.Context, run *Run) context.Context {
	c := tool.WithSessionID(ctx, run.SessionID)
	c = tool.WithAgentID(c, run.SessionID)
	c = tool.WithWorkDir(c, run.Workdir)
	return c
}
