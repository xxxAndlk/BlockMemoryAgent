package dag

import (
	"fmt"
	"sort"
)

// TopoSort performs a deterministic topological sort of the given tasks using
// Kahn's algorithm (O(V+E)). It returns an error if there are duplicate task
// IDs, missing dependencies, or a cycle. Ready nodes with in-degree zero are
// processed in lexicographic ID order to keep the output deterministic.
func TopoSort(tasks []*Task) ([]*Task, error) {
	byID := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate task id: %s", t.ID)
		}
		byID[t.ID] = t
	}

	// Validate dependency existence.
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("task %s depends on missing %s", t.ID, dep)
			}
		}
	}

	// Compute in-degrees.
	inDeg := make(map[string]int, len(tasks))
	for _, t := range tasks {
		inDeg[t.ID] = len(t.DependsOn)
	}

	// Seed the queue with zero-in-degree nodes, sorted by ID.
	var queue []string
	for id, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)

	var sorted []*Task
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, byID[id])

		// Collect dependents and decrement their in-degrees.
		var ready []string
		for _, t := range tasks {
			for _, dep := range t.DependsOn {
				if dep == id {
					inDeg[t.ID]--
					if inDeg[t.ID] == 0 {
						ready = append(ready, t.ID)
					}
				}
			}
		}
		if len(ready) > 0 {
			sort.Strings(ready)
			queue = append(queue, ready...)
		}
	}

	if len(sorted) != len(tasks) {
		return nil, fmt.Errorf("cycle detected in dag")
	}
	return sorted, nil
}
