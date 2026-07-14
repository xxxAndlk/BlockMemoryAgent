package types

// 本文件定义 Plan-and-Execute 的计划类型（保留供未来扩展）。
// 当前 ReAct 引擎不再使用 ExecutionPlan，但类型本身无外部依赖，保留以避免
// 一旦恢复计划执行能力时产生循环依赖。

// PlanStepStatus 步骤状态，表示计划单步在生命周期中的所处阶段。
type PlanStepStatus string

const (
	// PlanStepPending 待执行：步骤已创建但尚未开始。
	PlanStepPending PlanStepStatus = "pending"
	// PlanStepRunning 执行中：步骤已被选中并正在执行。
	PlanStepRunning PlanStepStatus = "running"
	// PlanStepDone 已完成：步骤目标达成。
	PlanStepDone PlanStepStatus = "done"
	// PlanStepFailed 失败：步骤执行出错，需要上层决策。
	PlanStepFailed PlanStepStatus = "failed"
	// PlanStepSkipped 已跳过：因前置条件或用户指令而跳过。
	PlanStepSkipped PlanStepStatus = "skipped"
)

// PlanStep 计划步骤：描述一次可独立执行的原子目标。
type PlanStep struct {
	Goal      string         // 步骤目标（作为 assistant task 文本）
	ToolHint  string         // 预期工具提示（可选，注入 prompt）
	DependsOn []string       // 依赖的前置步骤目标（可选）
	Status    PlanStepStatus // 执行状态
}

// ExecutionPlan 结构化执行计划：按依赖关系组织的有序步骤集合。
type ExecutionPlan struct {
	Steps []*PlanStep // 有序步骤列表
}

// PendingGoals 返回尚未完成的步骤目标（按顺序），用于派发助手。
// 参数：无（接收者为 ExecutionPlan 指针）。
// 返回：未完成步骤的 Goal 字符串切片；nil 接收者返回 nil。
func (p *ExecutionPlan) PendingGoals() []string {
	// 防御 nil 接收者：避免空指针解引用。
	if p == nil {
		return nil
	}
	// 预分配结果切片，长度未知但按步骤数上限估算。
	var goals []string
	// 遍历所有步骤，筛选仍处于待执行或执行中的目标。
	for _, s := range p.Steps {
		if s.Status == PlanStepPending || s.Status == PlanStepRunning {
			// 将未完成目标追加到结果。
			goals = append(goals, s.Goal)
		}
	}
	// 返回按原顺序排列的未完成目标列表。
	return goals
}

// MarkDone 把指定目标标记为完成（断点续行用）。
// 参数：goal 要匹配的步骤目标字符串。
// 行为：线性扫描 Steps，首个 Goal 相等的步骤会被置为 PlanStepDone 并立即返回。
func (p *ExecutionPlan) MarkDone(goal string) {
	// 防御 nil 接收者：无操作即可。
	if p == nil {
		return
	}
	// 顺序查找目标步骤。
	for _, s := range p.Steps {
		if s.Goal == goal {
			// 命中后立即更新状态并结束函数，避免继续扫描。
			s.Status = PlanStepDone
			return
		}
	}
}
