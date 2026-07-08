package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// analyzeSubTasks 分析子领域任务（优先LLM，失败回退规则）。
//
// 职责：把领域目标拆成 2-4 个可执行的子任务。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（取 DomainGoal 作为分析输入）
//
// 返回：子任务列表；无目标或实例缺失时返回 nil 或单元素列表。
func (n *SubDomainAgentNode) analyzeSubTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		return []string{goal}
	}

	if n.modelFactory != nil {
		if tasks := n.analyzeSubTasksWithLLM(ctx, inst.Domain, goal); len(tasks) > 0 {
			return tasks
		}
	}
	return n.analyzeSubTasksByRules(inst.Domain, goal)
}

// analyzeSubTasksWithLLM 使用LLM分析子领域任务。
//
// 职责：调用领域模型，把目标拆成 2-4 个子任务。
//
// 参数：
//   - ctx：请求上下文
//   - subDomain：子领域名
//   - goal：领域目标
//
// 返回：子任务列表；LLM 不可用或返回空则返回 nil。
func (n *SubDomainAgentNode) analyzeSubTasksWithLLM(ctx context.Context, subDomain, goal string) []string {
	if n.modelFactory == nil {
		return nil
	}

	prompt := fmt.Sprintf(`你是子领域任务分析师。请将以下目标在子领域"%s"中拆解为2-4个具体可执行的子任务。

目标: %s

要求:
- 每个子任务具体且可独立执行
- 只输出子任务列表，每行一个，不要编号，不要其他内容

子任务:`, subDomain, goal)

	caller := "SubDomainAgent/子任务分析"
	resp, callErr, _ := n.CallLLM(ctx, prompt, LLMCallOptions{Caller: caller})
	if callErr != nil || resp == "" {
		return nil
	}

	var tasks []string
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:]
		}
		if len(line) >= 2 {
			tasks = append(tasks, line)
		}
	}
	return tasks
}

// analyzeSubTasksByRules 基于规则分析子任务。
//
// 职责：LLM 不可用时的回退方案，按子领域名关键词匹配预设模板。
//
// 参数：
//   - subDomain：子领域名
//   - goal：领域目标
//
// 返回：子任务列表；无匹配时把整个 goal 作为单任务。
func (n *SubDomainAgentNode) analyzeSubTasksByRules(subDomain, goal string) []string {
	var tasks []string

	if strings.Contains(subDomain, "头部") || strings.Contains(subDomain, "header") {
		tasks = append(tasks, "分析头部组件结构")
		tasks = append(tasks, "检查导航栏样式")
		tasks = append(tasks, "修复头部布局问题")
	} else if strings.Contains(subDomain, "列表") || strings.Contains(subDomain, "list") {
		tasks = append(tasks, "分析列表渲染逻辑")
		tasks = append(tasks, "检查分页组件")
		tasks = append(tasks, "修复列表样式")
	} else if strings.Contains(subDomain, "底部") || strings.Contains(subDomain, "footer") {
		tasks = append(tasks, "检查底部导航")
		tasks = append(tasks, "修复底部样式")
	} else {
		tasks = append(tasks, goal)
	}

	return tasks
}
