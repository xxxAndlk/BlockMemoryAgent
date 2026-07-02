package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/types"
	"sync"
	"time"
)

// matchFixedAssistant 匹配固定助手。
//
// 职责：遍历所有固定助手角色定义，按技能/关键词命中打分，返回最高分者。
//
// 参数：
//   - task：任务文本
//
// 返回：匹配的角色定义；最高分 < 10 返回 nil（视为无匹配，转动态创建）。
// matchFixedAssistant 委托公共实现：按技能/关键词打分匹配固定助手。
func (n *DomainAgentNode) matchFixedAssistant(task string) *types.RoleDefinition {
	return CommonMatchFixedAssistant(n.registry, task)
}

// 职责：为每个任务创建/匹配一个 Assistant，并发执行后收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例（作为 Assistant 的父）
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 并发安全：内部用 sync.Mutex 保护 results map；WaitGroup 等待全部完成。
//
// 注意：当前 Invoke 走串行路径，此函数保留以备并行场景使用。
func (n *DomainAgentNode) dispatchAssistantsParallel(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	results := make(map[string]string) // 结果收集
	var mu sync.Mutex                  // 保护 results 的并发写入
	var wg sync.WaitGroup              // 等待所有 goroutine 完成

	for _, task := range tasks {
		// 为任务创建/匹配 Assistant 实例与角色定义
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			// 创建失败：直接写入错误结果，不进入 goroutine
			mu.Lock()
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			mu.Unlock()
			continue
		}

		// 启动 goroutine 并行执行
		wg.Add(1)
		go func(task string, aInst *types.RoleInstance, aDef *types.RoleDefinition) {
			defer wg.Done()

			// 在 goroutine 内运行助手
			result := n.runAssistant(ctx, state, aInst, aDef, task)

			// 加锁写回结果
			mu.Lock()
			results[task] = result
			mu.Unlock()
		}(task, assistantInst, assistantDef)
	}

	// 等待所有助手完成
	wg.Wait()
	return results
}

// dispatchAssistantsSerial 串行派发助手。
//
// 职责：按顺序为每个任务创建/匹配 Assistant 并执行，收集结果。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - inst：本 Domain 实例
//   - tasks：待处理任务列表
//
// 返回：task -> 结果文本 的映射。
//
// 设计意图：子任务间常有依赖（"启动游戏"依赖"写代码"、"运行 db_check"依赖"写 db_check"），
// 并行会导致后续任务找不到前置产物而反复 ListDir/ReadFile 空转。
// 串行虽慢，但 ReAct 循环能读到前置产物，任务成功率显著提升。
func (n *DomainAgentNode) dispatchAssistantsSerial(ctx context.Context, state *types.ThreeLayerState, inst *types.RoleInstance, tasks []string) map[string]string {
	// 预分配容量，避免 map 扩容
	results := make(map[string]string, len(tasks))
	block := state.ActiveBlocks[state.CurrentBlockID]
	for _, task := range tasks {
		// 为任务创建/匹配 Assistant
		assistantInst, assistantDef := n.createAssistantForTask(ctx, state, inst, task)
		if assistantInst == nil {
			// 创建失败：写入错误结果
			results[task] = fmt.Sprintf("[ERROR] 无法创建助手处理任务: %s", task)
			continue
		}
		// 串行执行：上一个完成后再跑下一个，确保依赖产物可见
		results[task] = n.runAssistant(ctx, state, assistantInst, assistantDef, task)
		// Plan-and-Execute：标记计划步骤完成（断点续行用）
		if block != nil && block.Plan != nil {
			block.Plan.MarkDone(task)
		}
	}
	return results // 返回所有任务的结果
}

// retryWithBackoff 指数退避重试。
//
// 职责：最多重试 maxRetries 次 fn；每次失败后 sleep delay 并翻倍 delay。
//
// 参数：
//   - maxRetries：最大重试次数（含首次执行）
//   - initialDelay：首次失败后的初始退避时长
//   - fn：待重试的函数，返回 nil 视为成功
//
// 返回：最后一次 fn 的错误；全部成功返回 nil。
//
// 并发安全：纯函数，无共享状态。
//
// 用途：Domain/SubDomain 的 runAssistant 用此包装 executeAssistantTask，
// 应对写文件/工具调用的瞬时失败。
func retryWithBackoff(maxRetries int, initialDelay time.Duration, fn func() error) error {
	var err error
	delay := initialDelay
	for i := 0; i < maxRetries; i++ {
		// 执行 fn；成功则立即返回
		if err = fn(); err == nil {
			return nil
		}
		// 非最后一次失败则退避后重试
		if i < maxRetries-1 {
			time.Sleep(delay) // 退避等待
			delay *= 2        // 指数翻倍
		}
	}
	return err // 返回最后一次错误
}
