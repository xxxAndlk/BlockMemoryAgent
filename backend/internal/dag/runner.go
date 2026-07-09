package dag

import "context"

// DAGRunner executes a single DAG instance independently of the scheduler's
// cron loop. It clones the DAG, topologically sorts the tasks, and dispatches
// ready tasks via the provided SessionLauncher.
//
// For this iteration the runner is intentionally minimal: the main Scheduler
// still owns the running map and periodic tick. DAGRunner is exposed as a
// reusable building block for ad-hoc or test executions.
type DAGRunner struct {
	dag      *DAG
	launcher SessionLauncher
}

// NewDAGRunner creates a runner for the given DAG and launcher.
func NewDAGRunner(dag *DAG, launcher SessionLauncher) *DAGRunner {
	return &DAGRunner{dag: dag, launcher: launcher}
}

// Run clones the DAG, validates it with a topological sort, and launches all
// tasks whose dependencies are already satisfied.
func (r *DAGRunner) Run(ctx context.Context) error {
	d := cloneDAG(r.dag)

	if _, err := TopoSort(d.Tasks); err != nil {
		return err
	}

	byID := make(map[string]*Task, len(d.Tasks))
	for _, t := range d.Tasks {
		byID[t.ID] = t
		// Treat a fresh runner execution as starting from pending for every task.
		t.Status = TaskStatusPending
	}

	for _, t := range d.Tasks {
		if t.Status != TaskStatusPending {
			continue
		}
		ready := true
		for _, dep := range t.DependsOn {
			if depT, ok := byID[dep]; !ok || depT.Status != TaskStatusCompleted {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		r.launcher.LaunchSession(t.Goal)
	}
	return nil
}
