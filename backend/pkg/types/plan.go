package types

// 本文件定义 Plan-and-Execute 的计划类型（保留供未来扩展）。
// 当前 ReAct 引擎不再使用 ExecutionPlan，但类型本身无外部依赖，保留以避免
// 一旦恢复计划执行能力时产生循环依赖。

// PlanStepStatus 步骤状态。
type PlanStepStatus string

const (
	PlanStepPending PlanStepStatus = "pending"
	PlanStepRunning PlanStepStatus = "running"
	PlanStepDone    PlanStepStatus = "done"
	PlanStepFailed  PlanStepStatus = "failed"
	PlanStepSkipped PlanStepStatus = "skipped"
)

// PlanStep 计划步骤。
type PlanStep struct {
	Goal      string         // 步骤目标（作为 assistant task 文本）
	ToolHint  string         // 预期工具提示（可选，注入 prompt）
	DependsOn []string       // 依赖的前置步骤目标（可选）
	Status    PlanStepStatus // 执行状态
}

// ExecutionPlan 结构化执行计划。
type ExecutionPlan struct {
	Steps []*PlanStep // 有序步骤列表
}

// PendingGoals 返回尚未完成的步骤目标（按顺序），用于派发助手。
func (p *ExecutionPlan) PendingGoals() []string {
	if p == nil {
		return nil
	}
	var goals []string
	for _, s := range p.Steps {
		if s.Status == PlanStepPending || s.Status == PlanStepRunning {
			goals = append(goals, s.Goal)
		}
	}
	return goals
}

// MarkDone 把指定目标标记为完成（断点续行用）。
func (p *ExecutionPlan) MarkDone(goal string) {
	if p == nil {
		return
	}
	for _, s := range p.Steps {
		if s.Goal == goal {
			s.Status = PlanStepDone
			return
		}
	}
}
