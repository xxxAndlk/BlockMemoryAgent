package graph

import (
	"context"
	"fmt"
	"sync"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// dispatchAssistantsParallel 并行调度助手。
//
// 职责：为每个任务创建/匹配一个 Assistant，并发执行后收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 SubDomain 实例（作为 Assistant 的父）
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 并发安全：内部用 sync.Mutex 保护 results map；WaitGroup 等待全部完成。
func (n *SubDomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, task := range tasks {
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			mu.Lock()
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(task string, aInst *types.RoleInstance, aDef *types.RoleDefinition) {
			defer wg.Done()
			result := n.runAssistant(ctx, state, aInst, aDef, task)
			mu.Lock()
			results[task] = result
			mu.Unlock()
		}(task, assistantInst, assistantDef)
	}

	wg.Wait()
	return results
}
