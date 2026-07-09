package dag

import "github.com/blockmemory/agent/backend/pkg/types"

// TopoSort delegates to pkg/types.TopoSort to avoid duplicating the algorithm.
func TopoSort(tasks []*Task) ([]*Task, error) {
	return types.TopoSort(tasks)
}
