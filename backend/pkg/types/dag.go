package types

import (
	"fmt"
	"sort"
	"time"
)

// TaskStatus 表示 DAG 任务节点的状态。
type TaskStatus string

const (
	// TaskStatusPending 待执行：任务已创建但尚未开始调度。
	TaskStatusPending TaskStatus = "pending"
	// TaskStatusRunning 运行中：任务已被调度并正在执行。
	TaskStatusRunning TaskStatus = "running"
	// TaskStatusCompleted 已完成：任务已成功结束。
	TaskStatusCompleted TaskStatus = "completed"
	// TaskStatusFailed 失败：任务执行过程中出错。
	TaskStatusFailed TaskStatus = "failed"
)

// Task 是 DAG 中的一个节点，描述一个带依赖关系的最小执行单元。
type Task struct {
	ID         string     `json:"id"`                    // 任务唯一标识
	Goal       string     `json:"goal"`                  // 任务目标描述
	DependsOn  []string   `json:"depends_on"`            // 依赖的任务 ID 列表
	Status     TaskStatus `json:"status"`                // 当前状态
	SessionID  string     `json:"session_id"`            // 所属会话 ID
	StartedAt  *time.Time `json:"started_at,omitempty"`  // 开始执行时间，仅开始后非空
	FinishedAt *time.Time `json:"finished_at,omitempty"` // 完成时间，仅完成后非空
}

// DAG 是有向无环图：一组相互依赖的任务，以及可选的 cron 调度配置。
type DAG struct {
	ID        string    `json:"id"`         // DAG 唯一标识
	Name      string    `json:"name"`       // DAG 名称
	Cron      string    `json:"cron"`       // cron 表达式，用于周期调度
	Tasks     []*Task   `json:"tasks"`      // 任务节点列表
	Enabled   bool      `json:"enabled"`    // 是否启用
	CreatedAt time.Time `json:"created_at"` // 创建时间
	UpdatedAt time.Time `json:"updated_at"` // 最后更新时间
}

// HasCycle 报告该 DAG 是否包含环。
// 内部通过拓扑排序实现：若排序失败且原因是存在环，则返回 true。
func (d *DAG) HasCycle() bool {
	// 调用包级 TopoSort 对 DAG 的所有任务排序。
	_, err := TopoSort(d.Tasks)
	// 只要有错误（重复 ID、缺失依赖或环）都视为"存在环"，返回 true。
	return err != nil
}

// TopoSort 返回 DAG 任务按拓扑顺序排序后的结果。
// 若图中存在环或依赖异常，则返回错误。
func (d *DAG) TopoSort() ([]*Task, error) {
	// 委托给包级 TopoSort 实现复用。
	return TopoSort(d.Tasks)
}

// TopoSort 使用 Kahn 算法对给定任务进行确定性拓扑排序。
// 若出现重复任务 ID、缺失依赖或环，则返回错误。
// 入度为 0 的就绪节点按 ID 字典序处理，保证输出顺序稳定。
func TopoSort(tasks []*Task) ([]*Task, error) {
	// 建立 ID 到任务对象的映射，便于按 ID 快速查找。
	byID := make(map[string]*Task, len(tasks))
	// 遍历任务列表填充映射，同时检测重复 ID。
	for _, t := range tasks {
		// 若 ID 已存在，说明配置重复，立即返回错误。
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate task id: %s", t.ID)
		}
		// 将任务存入映射。
		byID[t.ID] = t
	}

	// 校验每个任务的依赖是否都在当前任务集合中，避免指向不存在的任务。
	for _, t := range tasks {
		// 遍历当前任务的所有依赖 ID。
		for _, dep := range t.DependsOn {
			// 依赖 ID 不在映射中，返回缺失依赖错误。
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("task %s depends on missing %s", t.ID, dep)
			}
		}
	}

	// 计算每个任务的入度（依赖数量）。
	inDeg := make(map[string]int, len(tasks))
	for _, t := range tasks {
		// 入度即 DependsOn 切片长度。
		inDeg[t.ID] = len(t.DependsOn)
	}

	// 初始化就绪队列：收集所有入度为 0 的任务 ID。
	var queue []string
	for id, deg := range inDeg {
		// 入度为 0 表示没有前置依赖，可立即执行。
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	// 按 ID 字典序排序队列，保证确定性输出。
	sort.Strings(queue)

	// sorted 保存最终拓扑排序结果。
	var sorted []*Task
	// 当队列非空时持续处理。
	for len(queue) > 0 {
		// 取出队列头部 ID。
		id := queue[0]
		// 移除已取出的元素。
		queue = queue[1:]
		// 将对应任务追加到结果。
		sorted = append(sorted, byID[id])

		// ready 暂存本次处理完成后新变为入度 0 的任务 ID。
		var ready []string
		// 遍历所有任务，寻找依赖中包含当前 id 的后置任务。
		for _, t := range tasks {
			// 检查该任务的所有依赖。
			for _, dep := range t.DependsOn {
				// 若某任务依赖当前处理的任务，则将其入度减一。
				if dep == id {
					inDeg[t.ID]--
					// 入度降为 0 表示该任务就绪，加入待排序列表。
					if inDeg[t.ID] == 0 {
						ready = append(ready, t.ID)
					}
				}
			}
		}
		// 若本轮有新增就绪任务，按字典序排序后追加到队列尾部。
		if len(ready) > 0 {
			sort.Strings(ready)
			queue = append(queue, ready...)
		}
	}

	// 若排序结果数量不等于原始任务数，说明存在环导致无法处理全部节点。
	if len(sorted) != len(tasks) {
		return nil, fmt.Errorf("cycle detected in dag")
	}
	// 返回稳定的拓扑排序结果。
	return sorted, nil
}
