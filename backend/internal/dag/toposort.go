package dag

import "github.com/blockmemory/agent/backend/pkg/types"

// TopoSort 将 task 切片委托给 pkg/types.TopoSort 进行拓扑排序。
//
// 职责：作为 dag 包对外的拓扑排序入口，避免在 dag 包与 types 包中重复实现同一算法。
//
// 参数：
//   - tasks：待排序的 Task 指针切片。
//
// 返回：
//   - 拓扑排序后的 Task 指针切片；若存在环、重复 ID 或缺失依赖则返回非 nil error。
func TopoSort(tasks []*Task) ([]*Task, error) {
	return types.TopoSort(tasks)
}
