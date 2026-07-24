// Package assembly 实现领域 Agent 任务拆解编排器：把复杂目标拆为互不干涉的小任务，
// 每个小任务用干净上下文的子 Agent 执行（压缩 Token、对抗上下文污染），全部完成后聚合返回。
//
// 设计意图：对应 TODO 第七项"Agent执行任务，先拆解任务，拆解为不会干涉的单元任务后，
// 一个单元任务使用一个干净上下文的Agent编码助手执行"。与 verifyloop 共享"干净上下文子 Agent
// 派发"理念，但语义不同：
//   - verifyloop：单产出 -> 验证 -> 修正 -> 统一测试（串行修正循环）
//   - assembly：复杂目标 -> 拆解 -> 并行/依赖执行小任务 -> 聚合（拆解聚合）
//
// 抽象分层：
//   - Splitter：把目标拆为小任务列表（含依赖关系，用 DAG 表达）。
//   - Executor：执行单个小任务，返回产出文本。默认实现经 Dispatcher.ExecuteChild
//     同步派发干净上下文子 Agent。
//   - Aggregator：把所有小任务产出聚合为最终结果。
//
// 小任务间依赖用 internal/dag 的 DAG 表达：Splitter 返回 DAG，Assembly 按拓扑序执行，
// 依赖完成后才执行下游任务，保证"运行一个任务依赖于另一个任务"的形式。
package assembly

import (
	"context"
	"fmt"
	"sync"

	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Runner 抽象执行子 Agent 的能力，由 *subagent.Dispatcher 实现（ExecuteChild 方法）。
// 与 verifyloop.Runner 同构，复用 subagent.Dispatcher 即可。
type Runner interface {
	// ExecuteChild 同步执行一个子 Agent 并返回其最终答复文本。
	ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error)
}

// Splitter 把复杂目标拆为小任务 DAG。
// 实现方决定拆解策略：LLM 拆解、规则拆解、人工预设等。
// 返回的 DAG 节点 ID 即小任务 ID，Task.Goal 为小任务描述，Task.DependsOn 表达依赖。
type Splitter interface {
	// Split 根据原始目标返回小任务 DAG。
	Split(ctx context.Context, goal string) (*dag.DAG, error)
}

// Executor 执行单个小任务，返回产出文本。
// 默认实现经 Runner.ExecuteChild 派发干净上下文子 Agent；可替换为本地执行、MCP 调用等。
type Executor interface {
	// Execute 执行一个小任务。
	// parentID 为上级 Agent ID（领域 Agent），供 Runner 派发子 Agent 时标识 caller。
	// task 为小任务描述（含背景、目标、验收标准）。
	Execute(ctx context.Context, parentID string, task types.Task) (string, error)
}

// Aggregator 把所有小任务产出聚合为最终结果。
// 实现方决定聚合策略：LLM 汇总、模板拼接、按依赖层级合并等。
type Aggregator interface {
	// Aggregate 接收原始目标与小任务->产出映射，返回聚合后的最终结果。
	Aggregate(ctx context.Context, goal string, results map[string]string) (string, error)
}

// Assembly 是领域任务拆解编排器。零值不可用，须通过 New 构造。
// 驱动流程：Splitter.Split -> 按拓扑序 Executor.Execute -> Aggregator.Aggregate。
type Assembly struct {
	splitter  Splitter
	executor  Executor
	aggregator Aggregator
}

// New 创建编排器，三接口必须非 nil。
func New(splitter Splitter, executor Executor, aggregator Aggregator) *Assembly {
	return &Assembly{splitter: splitter, executor: executor, aggregator: aggregator}
}

// Run 驱动拆解-执行-聚合流程，阻塞至全部完成或出错。
// ctx 取消时立即返回已完成的产出与取消错误。
func (a *Assembly) Run(ctx context.Context, parentID, goal string) (string, error) {
	// 步骤 1：拆解。
	d, err := a.splitter.Split(ctx, goal)
	if err != nil {
		return "", fmt.Errorf("split: %w", err)
	}
	if d == nil || len(d.Tasks) == 0 {
		// 无小任务：直接聚合空结果。
		return a.aggregator.Aggregate(ctx, goal, map[string]string{})
	}

	// 步骤 2：按拓扑序执行，依赖完成后才执行下游（支持并行无依赖任务）。
	results, err := a.executeDAG(ctx, parentID, d)
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}

	// 步骤 3：聚合。
	aggregated, err := a.aggregator.Aggregate(ctx, goal, results)
	if err != nil {
		return "", fmt.Errorf("aggregate: %w", err)
	}
	return aggregated, nil
}

// executeDAG 按拓扑序执行 DAG 中所有小任务，依赖完成后才执行下游。
// 无依赖的任务可并行执行（受依赖图约束）；任一任务出错则取消未完成任务并返回错误。
func (a *Assembly) executeDAG(ctx context.Context, parentID string, d *dag.DAG) (map[string]string, error) {
	results := make(map[string]string, len(d.Tasks))
	var mu sync.Mutex
	// done 标记每个任务是否已完成（成功或失败）。
	done := make(map[string]bool, len(d.Tasks))
	// errCh 收集第一个错误，用 once 保证只记第一个。
	errCh := make(chan error, 1)
	var errOnce sync.Once

	// ready 检查任务的所有依赖是否已完成。
	ready := func(t types.Task) bool {
		for _, dep := range t.DependsOn {
			if !done[dep] {
				return false
			}
		}
		return true
	}

	// 并发控制：用 semaphore 限制并行度（默认按 DAG 节点数，即无限制并行；
	// 调用方可通过 ctx 取消控制）。
	sem := make(chan struct{}, len(d.Tasks))
	var wg sync.WaitGroup

	// 执行单个任务。
	execOne := func(t types.Task) {
		defer wg.Done()
		defer func() { <-sem }()
		if ctx.Err() != nil {
			return
		}
		out, err := a.executor.Execute(ctx, parentID, t)
		mu.Lock()
		done[t.ID] = true
		if err != nil {
			errOnce.Do(func() {
				errCh <- fmt.Errorf("task %s: %w", t.ID, err)
			})
			mu.Unlock()
			return
		}
		results[t.ID] = out
		mu.Unlock()
	}

	// 调度循环：重复扫描直到所有任务完成或出错。
	remaining := make(map[string]types.Task, len(d.Tasks))
	for _, t := range d.Tasks {
		remaining[t.ID] = *t
	}
	for len(remaining) > 0 {
		// 检查是否已有错误。
		select {
		case err := <-errCh:
			return results, err
		default:
		}
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		progressed := false
		for id, t := range remaining {
			if !ready(t) {
				continue
			}
			delete(remaining, id)
			progressed = true
			wg.Add(1)
			sem <- struct{}{}
			go execOne(t)
		}
		if !progressed {
			// 无任务可推进：等已执行任务完成或出错。
			if len(remaining) == len(d.Tasks) {
				// 所有任务都未 ready 且无在飞任务 -> 依赖图有环或初始依赖缺失。
				return results, fmt.Errorf("no runnable tasks (cyclic or missing deps): %d remaining", len(remaining))
			}
			// 等待在飞任务完成。
			wg.Wait()
			// 检查错误。
			select {
			case err := <-errCh:
				return results, err
			default:
			}
		}
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return results, err
	default:
	}
	return results, nil
}

// RunnerExecutor 是 Executor 的默认实现：经 Runner.ExecuteChild 派发干净上下文子 Agent。
type RunnerExecutor struct {
	runner Runner
	roleID string
}

// NewRunnerExecutor 创建默认 Executor。runner 提供 ExecuteChild 能力；
// roleID 为执行小任务的子 Agent 角色（如 code_assistant）。
func NewRunnerExecutor(runner Runner, roleID string) *RunnerExecutor {
	return &RunnerExecutor{runner: runner, roleID: roleID}
}

// Execute 实现 Executor 接口：派发子 Agent 执行单个小任务。
func (e *RunnerExecutor) Execute(ctx context.Context, parentID string, task types.Task) (string, error) {
	return e.runner.ExecuteChild(ctx, parentID, e.roleID, task.Goal)
}

// Compile-time assertions.
var (
	_ Executor = (*RunnerExecutor)(nil)
)
