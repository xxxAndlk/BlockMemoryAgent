package graph

import (
	"context"
	"fmt"
	"log"

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
// 返回：子任务列表；无目标返回 nil；LLM 拆解均为思考类任务时回退为单任务。
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

	// 尝试使用LLM进行任务拆解（用轻量模型 + 短超时，避免 domain_agent 模型 91s 超时）
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		n.emit(ctx, "llm", "调用轻量 LLM 拆解子任务...")
		// 构造拆解 prompt：强调"可直接用工具执行"，禁止纯思考类子任务
		// 若本次 Invoke 检索到历史相似块记忆，作为参考段注入（特性3）
		// 若 blockMemory 可用但未召回任何记忆，强制注入 NO_PRIOR_RECALL 标记，
		// 防止 LLM 幻觉出"上次会话讨论过 X"并编造决策文件（参见塔防 demo 事故）。
		memorySection := ""
		if n.recalledMemory != "" {
			memorySection = fmt.Sprintf("\n相关历史块记忆（参考，避免重复劳动）:\n%s\n", n.recalledMemory)
		} else if n.recallAttempted && n.blockMemory != nil {
			memorySection = "\n[NO_PRIOR_RECALL] 块记忆检索未命中任何历史决策。\n禁止编造\"上次会话/上次讨论过 X\"类内容；如需建立新决策，直接落地并在 Facts 标注 first_session=true。\n禁止用 WriteFile 创建\"决策记录.md\"等文件伪造历史。\n\n"
		}
		resp, err, timedOut := n.callLightweightAs(ctx, "DomainAgent/任务拆解(轻量)", fmt.Sprintf(`你是一个任务分析专家。请将以下目标拆解为2-4个独立可执行的子任务。

目标: %s

%s
%s

要求:
- 至少拆出 2 个子任务，禁止只返回 1 个；若目标本身已是单步动作（如"读文件 X"），仍须补充"验证/运行/读取依赖"等配套子任务
- 每个子任务必须是一个可直接用工具执行的动作（如"用 WriteFile 写 X 文件"、"用 RunCommand 运行 Y"、"用 HTTPGet 抓取 Z"）
- 严禁出现"分析/确定/规划/设计/思考/研究/需求/方案"等纯思考类子任务，这类工作应在执行动作中一并完成
- 涉及创建文件的目标，必须有子任务明确写出文件路径与内容来源；多文件交付目标（如游戏 demo 含 index.html+game.js+config.json+tests）必须每个文件一个子任务
- 子任务之间可以有依赖但应尽量并行；后序任务必须能复用前序任务结论，禁止多个助手重复全量读取同一份代码
- 若目标涉及改造现有项目，应拆为："读取并总结关键文件位置与结构"、"基于前述总结修改具体文件"、"验证修改"; 禁止把"读代码"和"改代码"塞到同一任务里反复探索
- 只输出子任务列表，每行一个，不要编号，不要其他内容
- 任务匹配工具，不要"为了用工具而用工具"：
  * 信息查询/搜索/新闻/行情类目标 → 用 HTTPGet 抓取公开 URL，禁止"写 Python 脚本去搜索"
  * 只有目标明确要求"写代码/生成文件/运行程序"时，才用 WriteFile / RunCommand

示例（好）:
用 SearchInFiles 定位 workspace/tower_game 中金币与波次相关代码，再用 ReadFile 读取关键片段并总结文件路径和函数名
用 WriteFile 修改 workspace/tower_game/game.js 的金币规则为击杀+10、波次完成+20，并添加 10 关关卡配置
用 RunCommand 运行 node -c workspace/tower_game/game.js 验证语法

示例（坏，禁止）:
需求分析
设计方案
编写代码
用 WriteFile 写一个 Python 脚本去搜索新闻（应该直接用 HTTPGet）
用 ReadFile 读取所有游戏文件并修改金币和关卡系统（任务过大，应拆分）
（仅返回 1 个子任务也是禁止的）

子任务:`, goal, memorySection, fmtEnvSection()))
		if !timedOut && err == nil && resp != "" {
			// 解析响应为任务列表
			if tasks := parseTaskListFromResp(resp); len(tasks) > 0 {
				// 强制最少 2 子任务：LLM 只返回 1 个时回退到规则拆解，
				// 避免 DomainAgent 把整个领域压成单 Assistant（参见塔防 demo 事故：
				// 战斗领域只派 1 个 Assistant，射击/AI/弹道/buff/波次全塞一个 prompt）。
				if len(tasks) < 2 && !state.DirectExecute {
					n.emit(ctx, "think", fmt.Sprintf("LLM 仅拆出 %d 个子任务，不足 2 个，回退到规则拆解", len(tasks)))
					return n.analyzeTasksByRules(goal)
				}
				n.emit(ctx, "think", fmt.Sprintf("LLM 拆解出 %d 个子任务", len(tasks)))
				return tasks // 返回 LLM 拆解的任务
			}
			// LLM 返回的尽是思考类任务，过滤后为空 → 把整个 goal 作为单任务，
			// 让一个 assistant 用 ReAct 循环完整执行（写文件+运行+验证）。
			n.emit(ctx, "think", "LLM 拆解均为思考类任务，回退为单任务整体执行")
			return n.analyzeTasksByRules(goal) // 改用规则拆解，避免单任务退化
		}
		// 超时或失败：推送事件并回退规则
		if timedOut {
			n.emit(ctx, "error", "任务拆解 LLM 调用超时，回退到规则")
			log.Printf("[DomainAgent] LLM timeout on task analysis, using rules. %s", n.llmTracker.StatsString())
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
// 职责：LLM 不可用时的回退，按目标关键词匹配可直接用工具执行的子任务模板。
// 避免生成"分析/设计/思考"等纯思考类子任务，防止被 parseTaskListFromResp 过滤后退化回单任务。
//
// 参数：
//   - goal：领域目标
//
// 返回：子任务列表；无匹配时把整个 goal 作为单任务。
func (n *DomainAgentNode) analyzeTasksByRules(goal string) []string {
	var tasks []string

	// 按关键词匹配可直接执行的工具调用描述
	if strings.Contains(goal, "修复") {
		tasks = append(tasks, "用 ReadFile 读取相关文件定位问题代码")
		tasks = append(tasks, "用 WriteFile 修改问题代码完成修复")
		tasks = append(tasks, "用 RunCommand 运行测试或验证命令确认修复生效")
	} else if strings.Contains(goal, "游戏") || strings.Contains(goal, "demo") || strings.Contains(goal, "Demo") {
		// 游戏/演示类目标：按交付物拆分，避免单 Assistant 包揽全部
		tasks = append(tasks, "用 ListDir 查看工作目录结构确定输出路径")
		tasks = append(tasks, "用 WriteFile 创建入口文件（如 index.html）")
		tasks = append(tasks, "用 WriteFile 创建核心逻辑文件（如 game.js 或对应模块）")
		tasks = append(tasks, "用 RunCommand 运行语法检查或测试验证可加载")
	} else if strings.Contains(goal, "实现") || strings.Contains(goal, "开发") || strings.Contains(goal, "编写") {
		tasks = append(tasks, "用 ReadFile 查看现有代码/目录结构确定实现位置")
		tasks = append(tasks, "用 WriteFile 创建或修改文件实现目标功能")
		tasks = append(tasks, "用 RunCommand 运行编译/语法检查/测试验证实现正确")
	} else if strings.Contains(goal, "优化") {
		tasks = append(tasks, "用 ReadFile 读取待优化文件内容")
		tasks = append(tasks, "用 WriteFile 实施优化修改")
		tasks = append(tasks, "用 RunCommand 运行基准测试或验证命令确认优化效果")
	} else if strings.Contains(goal, "查询") || strings.Contains(goal, "查一下") || strings.Contains(goal, "搜索") {
		tasks = append(tasks, "用 HTTPGet 或 ReadFile 获取目标信息并整理回答")
	} else if strings.Contains(goal, "运行") || strings.Contains(goal, "执行") {
		tasks = append(tasks, "用 RunCommand 执行目标命令并收集输出")
	} else {
		// 无匹配：把整个目标作为单任务，交给 Assistant 自行拆解
		tasks = append(tasks, goal)
	}

	return tasks
}
