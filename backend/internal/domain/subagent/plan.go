package subagent

// plan.go 实现 TODO #22 真·执行计划：
//   - write_plan 工具（meta 白名单）：MetaAgent 拆解任务后写入 board.TaskBoard
//     （目标 + 子任务数组 {id, title, domain, depends_on, acceptance}），校验环依赖/未知依赖；
//   - 派发依赖门：call_sub_agent 派 domain 前查计划，depends_on 未全部 done 拒绝派发；
//   - 完成回写：runSubAgent 成功/失败把对应计划任务 MarkDone/MarkFailed（按 domain 匹配）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
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
	taskID, ok := b.FindByDomain(domain)
	if !ok {
		return "" // 计划未覆盖该领域：放行（零行为变化）
	}
	if b.DependsDone(taskID) {
		return ""
	}
	// 列出未完成的依赖供 LLM 知道等谁。
	snap := b.Snapshot()
	var pending []string
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
	if len(pending) == 0 {
		return fmt.Sprintf("依赖未满足：子任务 %s 的依赖尚未全部完成，请等待前置任务 [mailbox] 回传后再派发", taskID)
	}
	return fmt.Sprintf("依赖未满足：子任务 %s(%s) 依赖 %s 未完成，请等待其 [mailbox] 回传后再派发该领域",
		taskID, tskTitle(snap, taskID), strings.Join(pending, "、"))
}

func tskTitle(snap board.Snapshot, id string) string {
	for _, t := range snap.Tasks {
		if t.ID == id {
			return t.Title
		}
	}
	return id
}

// boardUpdate 把子 Agent 完成/失败状态回写计划任务（按 domain 匹配，TODO #22 Phase 1）。
// 无计划/未匹配静默跳过（零行为变化）。
func (d *Dispatcher) boardUpdate(ctx context.Context, parentID, domain string, done bool, summary string) {
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
	taskID, ok := b.FindByDomain(domain)
	if !ok {
		return
	}
	if done {
		_ = b.MarkDone(taskID, summary)
	} else {
		_ = b.MarkFailed(taskID, summary)
	}
}
