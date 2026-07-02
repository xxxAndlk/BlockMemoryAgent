package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/types"
	"strings"
)

// analyzeTasks 分析领域任务（优先使用LLM，回退到规则）。
//
// 职责：把领域目标拆成 2-4 个可直接用工具执行的子任务。
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（取 DomainGoal 作为分析输入）
//
// 返回：子任务列表；无目标返回 nil；LLM 拆解均为思考类任务时降级为单任务。
//
// DirectExecute 模式：MetaAgent 判定为查询/搜索类简单任务时置 state.DirectExecute=true，
// 此处跳过 LLM 拆解，直接把 goal 作为单任务交给一个 Assistant，避免无谓拆分。
func (n *DomainAgentNode) analyzeTasks(ctx context.Context, state *types.ThreeLayerState) []string {
	// 取领域目标；为空则直接返回 nil
	goal := state.DomainGoal
	if goal == "" {
		return nil
	}

	// DirectExecute 模式：跳过 LLM 拆解，直接派单任务
	if state.DirectExecute {
		n.emit(ctx, "think", "DirectExecute 模式：跳过子任务拆解，直接派发单助手执行")
		return []string{goal}
	}

	// 尝试使用LLM进行任务拆解
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		n.emit(ctx, "llm", "调用 LLM 拆解子任务...")
		// 构造拆解 prompt：强调"可直接用工具执行"，禁止纯思考类子任务
		// 若本次 Invoke 检索到历史相似块记忆，作为参考段注入（特性3）
		memorySection := ""
		if n.recalledMemory != "" {
			memorySection = fmt.Sprintf("\n相关历史块记忆（参考，避免重复劳动）:\n%s\n", n.recalledMemory)
		}
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个任务分析专家。请将以下目标拆解为2-4个独立可执行的子任务。

目标: %s

%s
%s

要求:
- 每个子任务必须是一个可直接用工具执行的动作（如"用 WriteFile 写 X 文件"、"用 RunCommand 运行 Y"、"用 HTTPGet 抓取 Z"）
- 严禁出现"分析/确定/规划/设计/思考/研究/需求/方案"等纯思考类子任务，这类工作应在执行动作中一并完成
- 涉及创建文件的目标，必须有子任务明确写出文件路径与内容来源
- 子任务之间可以有依赖但应尽量并行
- 只输出子任务列表，每行一个，不要编号，不要其他内容
- 任务匹配工具，不要"为了用工具而用工具"：
  * 信息查询/搜索/新闻/行情类目标 → 用 HTTPGet 抓取公开 URL，禁止"写 Python 脚本去搜索"
  * 只有目标明确要求"写代码/生成文件/运行程序"时，才用 WriteFile / RunCommand

示例（好）:
用 HTTPGet 抓取 https://news.example.com/ai 获取最近 AI 新闻
用 WriteFile 把贪吃蛇游戏代码写到 workspace/snake.py
用 RunCommand 运行 python workspace/snake.py 验证

示例（坏，禁止）:
需求分析
设计方案
编写代码
用 WriteFile 写一个 Python 脚本去搜索新闻（应该直接用 HTTPGet）

子任务:`, goal, memorySection, fmtEnvSection()))
		if !timedOut && err == nil && resp != "" {
			// 解析响应为任务列表
			if tasks := parseTaskListFromResp(resp); len(tasks) > 0 {
				n.emit(ctx, "think", fmt.Sprintf("LLM 拆解出 %d 个子任务", len(tasks)))
				return tasks // 返回 LLM 拆解的任务
			}
			// LLM 返回的尽是思考类任务，过滤后为空 → 把整个 goal 作为单任务，
			// 让一个 assistant 用 ReAct 循环完整执行（写文件+运行+验证）。
			n.emit(ctx, "think", "LLM 拆解均为思考类任务，降级为单任务整体执行")
			return []string{goal} // 降级为单任务
		}
		// 超时或失败：推送事件并回退规则
		if timedOut {
			n.emit(ctx, "error", "任务拆解 LLM 调用超时，回退到规则")
			fmt.Printf("[DomainAgent] LLM timeout on task analysis, using rules. %s\n", n.llmTracker.StatsString())
		} else if err != nil {
			n.emitDetail(ctx, "error", "任务拆解 LLM 调用失败: "+err.Error(), "")
		}
	}

	// 规则回退
	return n.analyzeTasksByRules(goal)
}

// parseTaskListFromResp 从LLM响应解析任务列表。
//
// 职责：逐行清洗响应（去前缀/编号），过滤纯思考类任务名。
//
// 参数：
//   - resp：LLM 返回的原始文本
//
// 返回：清洗后的任务列表；过滤后为空则返回 nil（调用方应回退到规则拆解）。
//
// 设计意图：纯思考类任务（需求分析/设计方案/测试验证 等）不产生可执行产物，
// 只会浪费 ReAct 轮数并触发 LLM 超时。
func parseTaskListFromResp(resp string) []string {
	// 纯思考类任务名黑名单
	thinkPatterns := []string{"需求分析", "设计方案", "设计", "分析", "规划", "思考", "研究",
		"确定", "需求", "方案", "测试验证", "验证", "总结", "review"}
	var tasks []string
	for _, line := range strings.Split(resp, "\n") {
		// 去首尾空白
		line = strings.TrimSpace(line)
		// 去除 "- " / "* " 前缀
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		// 去除行首的 "1. " / "2. " 等编号
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = line[idx+2:] // 截掉编号前缀
		}
		// 过短行（<2 字符）视为无效
		if len(line) < 2 {
			continue
		}
		// 过滤纯思考类任务名（整行就是"需求分析"这种短词）
		isThinkOnly := false
		for _, p := range thinkPatterns {
			// 整行匹配思考类关键词
			if line == p || strings.TrimSpace(line) == p {
				isThinkOnly = true
				break
			}
		}
		if isThinkOnly {
			continue // 跳过纯思考类任务
		}
		tasks = append(tasks, line) // 收集有效任务
	}
	return tasks
}

// analyzeTasksByRules 基于规则的任务拆解（回退方案）。
//
// 职责：LLM 不可用时的回退，按目标关键词匹配预设模板。
//
// 参数：
//   - goal：领域目标
//
// 返回：子任务列表；无匹配时把整个 goal 作为单任务。
func (n *DomainAgentNode) analyzeTasksByRules(goal string) []string {
	var tasks []string

	// 按"修复/实现/优化"等关键词匹配模板
	if strings.Contains(goal, "修复") {
		tasks = append(tasks, "分析根因")   // 第1步：根因分析
		tasks = append(tasks, "定位问题代码") // 第2步：定位代码
		tasks = append(tasks, "生成修复方案") // 第3步：生成方案
		tasks = append(tasks, "验证修复")   // 第4步：验证
	} else if strings.Contains(goal, "实现") || strings.Contains(goal, "开发") {
		tasks = append(tasks, "需求分析") // 第1步：需求
		tasks = append(tasks, "设计方案") // 第2步：设计
		tasks = append(tasks, "编写代码") // 第3步：编码
		tasks = append(tasks, "测试验证") // 第4步：测试
	} else if strings.Contains(goal, "优化") {
		tasks = append(tasks, "性能分析") // 第1步：性能分析
		tasks = append(tasks, "识别瓶颈") // 第2步：识别瓶颈
		tasks = append(tasks, "实施优化") // 第3步：实施
	} else {
		// 无匹配：把整个目标作为单任务
		tasks = append(tasks, goal)
	}

	return tasks
}
