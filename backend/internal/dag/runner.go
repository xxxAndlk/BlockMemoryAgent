package dag

import "context"

// DAGRunner 在调度器的 cron 循环之外独立执行单个 DAG 实例。
//
// 职责：对 DAG 做一次性的克隆、拓扑校验与就绪任务派发。
//
// 设计说明：
//   - 本迭代中 runner 刻意保持最小化，进程级调度仍由 Scheduler 负责（维护 running map 与周期 tick）。
//   - DAGRunner 作为可复用的构建块，供临时执行、测试或未来扩展使用。
//   - 它只派发当前依赖已满足的任务，并不跟踪 session 生命周期。
type DAGRunner struct {
	dag      *DAG            // 原始 DAG 定义，执行前会被深拷贝以保护原始数据
	launcher SessionLauncher // 任务派发器，将 task goal 转为新 session
}

// NewDAGRunner 为指定 DAG 与派发器创建一个运行器。
//
// 参数：
//   - dag：待执行的 DAG 定义；运行器内部会克隆，不会修改该参数。
//   - launcher：SessionLauncher 实现，负责把 task 的 goal 启动为新 session。
//
// 返回：初始化后的 *DAGRunner。
func NewDAGRunner(dag *DAG, launcher SessionLauncher) *DAGRunner {
	return &DAGRunner{dag: dag, launcher: launcher}
}

// Run 克隆 DAG、做拓扑排序校验，并派发出所有依赖已满足的任务。
//
// 执行流程：
//  1. 深拷贝 DAG，避免修改原始定义。
//  2. 调用 TopoSort 校验 DAG 无环且依赖存在；失败立即返回 error。
//  3. 建立 task ID 到 task 的索引 map。
//  4. 将每个 task 重置为 pending 状态，表示本次执行从初始状态开始。
//  5. 遍历 task，对每个 pending 任务检查其 DependsOn 是否全部已完成；若都完成则派发。
//
// 参数：
//   - ctx：上下文；当前实现未直接用到，但保留以兼容 future 的取消/超时扩展。
//
// 返回：拓扑校验失败返回 error；否则返回 nil。
func (r *DAGRunner) Run(ctx context.Context) error {
	// 深拷贝原始 DAG，避免运行期间修改传入的 DAG 实例
	d := cloneDAG(r.dag)

	// 拓扑排序同时完成环检测与依赖完整性校验
	if _, err := TopoSort(d.Tasks); err != nil {
		return err
	}

	// 构建 ID -> Task 索引，便于 O(1) 查询依赖任务状态
	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		byID[t.ID] = t
		// 将本次 runner 执行视为从 pending 开始，因此重置所有任务状态
		t.Status = TaskStatusPending
	}

	// 按遍历顺序派发所有当前依赖已满足的任务
	for _, t := range d.Tasks {
		// 只处理 pending 任务，已派发或已完成任务跳过
		if t.Status != TaskStatusPending {
			continue
		}
		// 检查所有依赖是否都已完成
		ready := true
		for _, dep := range t.DependsOn {
			if depT, ok := byID[dep]; !ok || depT.Status != TaskStatusCompleted {
				ready = false
				break
			}
		}
		// 任一依赖未完成则跳过，等待后续轮次
		if !ready {
			continue
		}
		// 依赖满足：将 task.goal 作为新 session 启动
		r.launcher.LaunchSession(t.Goal)
	}
	return nil
}
