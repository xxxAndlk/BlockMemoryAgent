package subagent

// plan.go 实现 TODO #22 真·执行计划：
//   - write_plan 工具（meta 白名单）：MetaAgent 拆解任务后写入 board.TaskBoard
//     （目标 + 子任务数组 {id, title, domain, depends_on, acceptance}），校验环依赖/未知依赖；
//   - 派发依赖门：call_sub_agent 派 domain 前查计划，depends_on 未全部 done 拒绝派发；
//   - 完成回写：runSubAgent 成功/失败把对应计划任务 MarkDone/MarkFailed（按 domain 匹配）。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// writePlanTool 实现 write_plan 工具：写入/覆盖会话级执行计划。
type writePlanTool struct {
	dispatcher *Dispatcher
}

// RegisterPlanTool 将 write_plan 工具安装到传入的工具注册表中（meta 白名单）。
func (d *Dispatcher) RegisterPlanTool(r *tool.Registry) {
	r.Register(&writePlanTool{dispatcher: d})
}

// Name 返回工具名称。
func (t *writePlanTool) Name() string { return "write_plan" }

// Aliases 返回工具别名列表，当前无别名。
func (t *writePlanTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *writePlanTool) Description() string {
	return "拆解任务后写入执行计划（目标 + 子任务列表）。" +
		"参数 goal 一句话全局目标；tasks 为子任务数组，每项 {id, title, domain, depends_on[], acceptance[]}：" +
		"id 为任务唯一标识（自己命名，字母数字连字符）；domain 为该子任务对应的派发领域（call_sub_agent 的 domain 参数需一致，" +
		"派发依赖门按它校验）；depends_on 为前置依赖的任务 id 列表（依赖未完成时该领域的派发会被拒绝）；" +
		"acceptance 为验收标准列表。" +
		"校验失败（id 重复/引用未知依赖/依赖成环）返回错误不落盘。" +
		"重复调用为全量覆盖：已存在的任务保留状态（重规划不改已完成任务），新增任务追加。"
}

// writePlanInput 是 write_plan 工具的入参结构。
type writePlanInput struct {
	Goal  string        `json:"goal"`
	Tasks []board.PlanTask `json:"tasks"`
}

// Execute 执行 write_plan 工具调用。
func (t *writePlanTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	goal, _ := args["goal"].(string)
	rawTasks, ok := args["tasks"].([]any)
	if !ok || len(rawTasks) == 0 {
		return &tool.Result{Tool: "write_plan", Error: "tasks is required: 非空数组，每项 {id, title, domain, depends_on[], acceptance[]}"}
	}
	tasks := make([]board.PlanTask, 0, len(rawTasks))
	for i, r := range rawTasks {
		m, ok := r.(map[string]any)
		if !ok {
			return &tool.Result{Tool: "write_plan", Error: fmt.Sprintf("tasks[%d] 必须是对象", i)}
		}
		pt := board.PlanTask{}
		pt.ID, _ = m["id"].(string)
		pt.Title, _ = m["title"].(string)
		pt.Domain, _ = m["domain"].(string)
		if pt.ID == "" || pt.Title == "" {
			return &tool.Result{Tool: "write_plan", Error: fmt.Sprintf("tasks[%d]: id 与 title 必填", i)}
		}
		if deps, ok := m["depends_on"].([]any); ok {
			for _, dep := range deps {
				if s, ok := dep.(string); ok && s != "" {
					pt.DependsOn = append(pt.DependsOn, s)
				}
			}
		}
		if accs, ok := m["acceptance"].([]any); ok {
			for _, a := range accs {
				if s, ok := a.(string); ok && s != "" {
					pt.Acceptance = append(pt.Acceptance, s)
				}
			}
		}
		tasks = append(tasks, pt)
	}

	if d.boardFn == nil {
		return &tool.Result{Tool: "write_plan", Error: "board not available"}
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		return &tool.Result{Tool: "write_plan", Error: "missing session context"}
	}
	bd := d.boardFn(sid)
	if bd == nil {
		bd = d.boardFnCreate(sid, goal)
	}
	if bd == nil {
		return &tool.Result{Tool: "write_plan", Error: "board not available for session"}
	}
	if err := bd.SetPlan(goal, tasks); err != nil {
		return &tool.Result{Tool: "write_plan", Error: fmt.Sprintf("计划校验失败: %v", err)}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "已写入计划 %d 个子任务：", len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, tsk := range tasks {
		ids = append(ids, tsk.ID)
	}
	out.WriteString(strings.Join(ids, ", "))
	return &tool.Result{Tool: "write_plan", Success: true, Output: out.String()}
}

// checkDepGate 派发依赖门（TODO #22 Phase 1）：该领域在计划中的子任务存在
// 未完成依赖时拒绝派发。无计划（board nil/空）返回空串零行为变化。
func (d *Dispatcher) checkDepGate(ctx context.Context, parentID, domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" || d.boardFn == nil {
		return ""
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	b := d.boardFn(sid)
	if b == nil {
		return ""
	}
	taskIDs := b.FindAllByDomain(domain)
	if len(taskIDs) == 0 {
		return "" // 计划未覆盖该领域：放行（零行为变化）
	}
	// 一个领域可对应多个子任务：任一任务的依赖未完成即拒绝派发，汇总所有未完成依赖。
	snap := b.Snapshot()
	var pending []string
	firstBlocked := ""
	for _, taskID := range taskIDs {
		if b.DependsDone(taskID) {
			continue
		}
		if firstBlocked == "" {
			firstBlocked = taskID
		}
		for _, tsk := range snap.Tasks {
			if tsk.ID != taskID {
				continue
			}
			for _, dep := range tsk.DependsOn {
				for _, dt := range snap.Tasks {
					if dt.ID == dep && dt.Status != board.TaskDone {
						pending = append(pending, fmt.Sprintf("%s(%s)", dep, dt.Title))
					}
				}
			}
			break
		}
	}
	if len(pending) == 0 {
		return ""
	}
	return fmt.Sprintf("依赖未满足：子任务 %s(%s) 依赖 %s 未完成，请等待其 [mailbox] 回传后再派发该领域",
		firstBlocked, tskTitle(snap, firstBlocked), strings.Join(pending, "、"))
}

func tskTitle(snap board.Snapshot, id string) string {
	for _, t := range snap.Tasks {
		if t.ID == id {
			return t.Title
		}
	}
	return id
}

// depWaiter 一次依赖门拒派的就绪通知登记（轻量排队：只通知不自动派发）。
type depWaiter struct {
	parentID string    // 被拒派的父 Agent（通知目标）
	domain   string    // 被拒派的领域
	reason   string    // 拒派原因原文（含未完成依赖清单，诊断用）
	at       time.Time // 登记时间
}

// registerDepWaiter 登记一次依赖门拒派（同父同域覆盖，幂等）：依赖就绪时经
// notifyDepWaiters 发邮箱通知 + 唤醒挂起会话，替代上级反复烧轮次重试派发。
func (d *Dispatcher) registerDepWaiter(parentID, domain, reason string) {
	if parentID == "" || domain == "" {
		return
	}
	d.depWaiters.Store(parentID+"\x00"+domain, &depWaiter{
		parentID: parentID,
		domain:   domain,
		reason:   reason,
		at:       time.Now(),
	})
}

// notifyDepWaiters 依赖就绪通知：某父名下领域任务终态回写后扫描其 waiter——
// waiter 领域的计划任务依赖已全 done 时，删登记并发邮箱 MsgInfo（From=system，
// drainMailbox 对 From=system 不推活动事件）+ 唤醒挂起（awaiting_child）会话。
// 失败依赖不算了结（DependsDone 只认 done，计划任务失败本就该人工介入），
// 未就绪 waiter 留存待下次回写再查。
func (d *Dispatcher) notifyDepWaiters(ctx context.Context, parentID string) {
	if parentID == "" || d.mailbox == nil || d.boardFn == nil {
		return
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	b := d.boardFn(sid)
	if b == nil {
		return
	}
	d.depWaiters.Range(func(k, v any) bool {
		w, ok := v.(*depWaiter)
		if !ok || w.parentID != parentID {
			return true
		}
		taskIDs := b.FindAllByDomain(w.domain)
		if len(taskIDs) == 0 {
			return true
		}
		for _, id := range taskIDs {
			if !b.DependsDone(id) {
				return true // 仍有未完成依赖，留存待下次回写再查
			}
		}
		d.depWaiters.Delete(k)
		if _, err := d.mailbox.Send(&mailbox.Message{
			From:    "system",
			To:      parentID,
			Type:    mailbox.MsgInfo,
			Subject: "依赖就绪: " + w.domain,
			Body: fmt.Sprintf("【依赖就绪】你派发 %s 曾被依赖门拒绝（%s）。其前置任务已全部完成，现在可以派发了。",
				w.domain, w.reason),
		}); err != nil {
			log.Printf("[dep-gate] 就绪通知发送失败: parent=%s domain=%s err=%v", parentID, w.domain, err)
			return true
		}
		d.wakeSuspendedParent(parentID, "【系统】依赖就绪通知已入邮箱：此前被依赖门拒绝的领域现在可以派发，请查收邮箱。")
		return true
	})
}

// boardUpdate 把子 Agent 完成/失败/未验证状态回写计划任务（按 domain 匹配，TODO #22 Phase 1；
// #60 三态化：status 可为 TaskDone/TaskFailed/TaskUnverified）。
// 一个领域对应多个子任务时整组联动（FindAllByDomain），否则细粒度计划里
// 只有首条任务翻状态、其余永远停在 pending。
// 无计划/未匹配静默跳过（零行为变化）。
func (d *Dispatcher) boardUpdate(ctx context.Context, parentID, domain string, status board.TaskStatus, summary string) {
	domain = strings.TrimSpace(domain)
	if domain == "" || d.boardFn == nil {
		return
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	b := d.boardFn(sid)
	if b == nil {
		return
	}
	for _, taskID := range b.FindAllByDomain(domain) {
		switch status {
		case board.TaskDone:
			_ = b.MarkDone(taskID, summary)
		case board.TaskUnverified:
			_ = b.MarkUnverified(taskID, summary)
		default:
			_ = b.MarkFailed(taskID, summary)
		}
	}
	// 依赖门就绪通知：本领域任务终态落定后，查同父 waiter 是否有领域依赖已全就绪。
	d.notifyDepWaiters(ctx, parentID)
}

// boardAssign 派发回写（TODO #22 Phase 1 补全）：子 Agent 起步即把匹配 domain 的
// 未完成任务置为 in_progress 并记录执行者。此前任务只有完成/失败回写，
// 执行计划面板全程显示 Waiting、总体进度 0%（实证：15 项计划跑了 80 分钟全 Waiting）。
// 已 Done 任务不翻回（重派已完成领域不复活旧状态）；无计划/未匹配静默跳过。
func (d *Dispatcher) boardAssign(ctx context.Context, parentID, domain, assignee string) {
	domain = strings.TrimSpace(domain)
	if domain == "" || d.boardFn == nil {
		return
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	b := d.boardFn(sid)
	if b == nil {
		return
	}
	doneSet := make(map[string]bool)
	for _, t := range b.Snapshot().Tasks {
		if t.Status == board.TaskDone {
			doneSet[t.ID] = true
		}
	}
	for _, taskID := range b.FindAllByDomain(domain) {
		if doneSet[taskID] {
			continue
		}
		_ = b.Assign(taskID, assignee)
	}
}
