package types

import (
	"fmt"
	"sort"
	"time"
)

// TaskStatus represents the status of a DAG task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// Task is a node in a DAG.
type Task struct {
	ID         string     `json:"id"`
	Goal       string     `json:"goal"`
	DependsOn  []string   `json:"depends_on"`
	Status     TaskStatus `json:"status"`
	SessionID  string     `json:"session_id"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DAG is a directed acyclic graph: a set of dependent tasks plus optional cron scheduling.
type DAG struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Cron      string    `json:"cron"`
	Tasks     []*Task   `json:"tasks"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasCycle reports whether the DAG contains a cycle.
func (d *DAG) HasCycle() bool {
	_, err := TopoSort(d.Tasks)
	return err != nil
}

// TopoSort returns a topologically sorted order of the DAG tasks.
func (d *DAG) TopoSort() ([]*Task, error) {
	return TopoSort(d.Tasks)
}

// TopoSort performs a deterministic topological sort of the given tasks using
// Kahn's algorithm. It returns an error if there are duplicate task IDs, missing
// dependencies, or a cycle. Ready nodes with in-degree zero are processed in
// lexicographic ID order.
func TopoSort(tasks []*Task) ([]*Task, error) {
	byID := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate task id: %s", t.ID)
		}
		byID[t.ID] = t
	}

	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("task %s depends on missing %s", t.ID, dep)
			}
		}
	}

	inDeg := make(map[string]int, len(tasks))
	for _, t := range tasks {
		inDeg[t.ID] = len(t.DependsOn)
	}

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
