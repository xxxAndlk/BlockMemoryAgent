package graph

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/go-kratos/blades"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// mergeToolList 把 skillBrief（来自 SkillSet.PromptList，以 ToolRef 为工具名）
// 与 defaultTools 合并，按行首工具名去重。skillBrief 行优先保留。
// 两者都空时返回 defaultTools。
//
// 职责：将动态装配的技能简介与默认工具列表合并为一份去重的工具清单，
//   用于在 system prompt 中向 LLM 展示当前可用的工具集合。
// 参数：
//   - skillBrief：来自 SkillSet 的技能简介文本，每行一个工具描述。
//   - defaultTools：兜底的默认工具列表。
// 返回：合并去重后的多行字符串（每行一个工具简介）。
// 副作用：无。
// 并发安全：纯函数，无共享状态。
func mergeToolList(skillBrief, defaultTools string) string {
	// skillBrief 为空时直接返回默认列表，避免空合并产生噪声
	if skillBrief == "" {
		return defaultTools
	}
	// seen 记录已收集的工具名，用于跨两个列表去重
	seen := make(map[string]bool)
	// out 收集去重后的工具描述行
	var out []string
	// 第一轮：遍历 skillBrief，技能简介优先保留
	for _, line := range strings.Split(skillBrief, "\n") {
		line = strings.TrimSpace(line) // 去除首尾空白
		if line == "" {
			continue // 跳过空行
		}
		name := toolNameFromLine(line) // 提取行首工具名
		if name == "" {
			continue // 无法识别工具名的行跳过
		}
		if seen[name] {
			continue // 同名工具已存在，跳过
		}
		seen[name] = true
		out = append(out, line) // 保留该行
	}
	// 第二轮：补充 defaultTools 中未出现过的工具
	for _, line := range strings.Split(defaultTools, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := toolNameFromLine(line)
		if name == "" || seen[name] {
			continue // 无名或已存在则跳过
		}
		seen[name] = true
		out = append(out, line)
	}
	// 用换行拼接为多行字符串返回
	return strings.Join(out, "\n")
}

// toolNameFromLine 从 "- WriteFile: 写入文件..." 中提取 "WriteFile"
//
// 职责：解析一行工具简介文本，提取行首的工具名。
// 参数：
//   - line：形如 "- WriteFile: 写入文件" 的工具描述行。
// 返回：工具名（如 "WriteFile"）；无法解析时返回空串。
// 副作用：无。
// 并发安全：纯函数。
func toolNameFromLine(line string) string {
	// 去除前导 "-" 与空白
	s := strings.TrimPrefix(strings.TrimSpace(line), "-")
	s = strings.TrimSpace(s)
	// 以第一个冒号为分隔，前半部分即工具名
	if idx := strings.Index(s, ":"); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	return "" // 无冒号则无法识别
}

// executeWithTools 使用 blades.Agent + 原生 function-calling 执行任务。
//
// LLM 通过 function calling 决定调用哪些工具，blades.Agent 内部跑 ReAct 循环
// （model.Generate → tool.Handle → 回灌 → 直到无 tool call 或达 maxIterations）。
//
// provider 为 nil 时（mock 路径，无 API Key）退化为单次 llm.Generate，保证
// 服务器无 key 仍可启动。
//
// 支持可选的 skillBrief：当 DomainAgent 已装配 Skill 子集时，将其作为
// "可见技能列表"加入 system prompt（技能仍以文本简介形式提示，不转为
// blades.Tool）。
//
// 职责：助手节点执行任务的核心入口，负责组装 system prompt、构建工具集、
//   驱动 blades Agent 的 ReAct 循环，并在结束时做完成门控校验。
// 参数：
//   - ctx：请求上下文，携带超时与取消信号。
//   - provider：blades 模型提供者；nil 时走 mock 单次调用路径。
//   - llm：底层 LLM 客户端，mock 路径直接使用其 Generate。
//   - executor：本地工具沙箱执行器。
//   - roleDef：角色定义，提供 system prompt 与角色 ID。
//   - task：本次任务描述。
//   - state：三层图当前状态，用于注入领域上下文与 sessionID。
//   - skillBrief：已装配技能的简介文本，注入 system prompt。
//   - progress：进度回调，把每一步动作推给 UI。
//   - agentName：当前 agent 名，用于事件归属。
// 返回：
//   - string：最终输出文本（含失败标记前缀）。
//   - []*ToolResult：本次执行产生的所有工具结果，供上层门控与回放使用。
// 副作用：通过 progress 推送 ProgressEvent；通过 executor 产生文件/命令副作用。
// 并发安全：单次调用安全；多 goroutine 并发调用需各自独立的 state/results。
func executeWithTools(
	ctx context.Context,
	provider blades.ModelProvider,
	llm model.LLMClient,
	executor *ToolExecutor,
	roleDef *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
	skillBrief string,
	progress ProgressCallback,
	agentName string,
) (string, []*ToolResult) {
	// allResults 收集本次 agent 执行期间所有工具调用结果
	var allResults []*ToolResult

	// 从 state 中提取 sessionID，用于进度事件归属
	sessionID := ""
	if state != nil {
		sessionID = state.SessionID
	}
	// emit 闭包：简化进度事件推送（无 detail）
	emit := func(ctx context.Context, kind, msg string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg})
		}
	}
	// emitDetail 闭包：带 detail 的进度事件推送（用于 token 用量、原始输出等）
	emitDetail := func(ctx context.Context, kind, msg, detail string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg, Detail: detail})
		}
	}
	// 推送任务开始事件，便于 UI 显示助手正在思考
	emit(ctx, "think", "助手开始执行任务: "+task)

	// skillBrief 作为技能简介注入 prompt（实际工具调用走 function calling）。
	skillSection := mergeToolList(skillBrief, "")

	// 系统环境提示：工作目录 / OS / DB 连接 / 运行时，避免 LLM 瞎猜路径或连不上库
	envSection := fmtEnvSection()

	// 拼装最终 system prompt：角色 prompt + 工具列表 + 环境信息 + 硬性规则
	systemPrompt := fmt.Sprintf(`%s

你可以使用以下工具（通过 function calling 调用）完成任务。

%s

%s

【硬性规则】
1. 凡任务涉及"创建/写入/生成/实现/编写"文件或代码，必须调用 WriteFile 工具真正落盘，
   禁止只用文字描述代码内容当作完成。代码必须完整、可直接运行，禁止用 pass/占位符/省略号代替实际逻辑。
2. 凡任务涉及"运行/执行/启动"程序，必须调用 RunCommand 工具实际执行，禁止只描述如何运行。
3. 工具未成功执行前，不得宣称任务完成。
4. 工具失败时，输出会包含 stderr/stdout。请阅读失败原因后再决定下一步，
   不要盲目重试同一命令的不同变种。连续两次失败后必须换一种完全不同的方法
   （例如换工具、换路径、放弃当前思路），而不是继续试错。
5. 只有当所有要求的文件已落盘、命令已执行，且无需再调用工具时，才输出最终文字总结。`,
		roleDef.SystemPrompt, skillSection, envSection)

	// 注入当前领域上下文，让 LLM 知道自己处于哪个 domain
	contextInfo := ""
	if state != nil && state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("\n当前领域: %s\n领域目标: %s", state.CurrentDomain, state.DomainGoal)
	}
	// 构造用户消息：任务 + 领域上下文 + 行动指引
	userMsg := fmt.Sprintf("任务: %s%s\n\n请分析任务并执行。如果需要查看文件或执行命令，请调用工具。", task, contextInfo)

	// mock 路径：无 blades provider，退化为单次 LLM 调用
	if provider == nil {
		emit(ctx, "llm", "无 blades provider（mock 模式），单次 LLM 调用")
		// 拼合 system + user 一次性送给 LLM
		resp, err := llm.Generate(ctx, systemPrompt+"\n\n"+userMsg)
		if err != nil {
			emit(ctx, "error", fmt.Sprintf("LLM 调用失败: %v", err))
			return "", nil
		}
		// mock 路径不产生工具结果
		return resp, nil
	}

	// 构造 7 个内置工具的 blades.Tool 集合，结果会写回 allResults
	tools := buildBladesTools(executor, progress, sessionID, agentName, &allResults)

	// 创建 blades Agent：注入模型、系统指令、工具集，限制最多 12 轮迭代
	agent, err := blades.NewAgent(
		agentName,
		blades.WithModel(provider),
		blades.WithInstruction(systemPrompt),
		blades.WithTools(tools...),
		blades.WithMaxIterations(12),
	)
	if err != nil {
		emit(ctx, "error", fmt.Sprintf("创建 blades agent 失败: %v", err))
		return "", nil
	}

	// 启动 blades runner 执行 ReAct 循环
	emit(ctx, "llm", "启动 blades agent 执行（最多 12 轮 function calling）")
	start := time.Now()                              // 记录起始时间用于耗时统计
	runner := blades.NewRunner(agent)                // 创建运行器
	output, err := runner.Run(ctx, blades.UserMessage(userMsg)) // 驱动 Agent 循环
	dur := time.Since(start)                         // 计算总耗时
	// 推送本次 agent 的耗时统计事件
	emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] blades agent 完成 dur=%v", agentName, dur.Round(time.Millisecond)), "")

	// 执行出错处理：若已有部分工具结果，则带上已完成步数返回，便于上层判断
	if err != nil {
		emit(ctx, "error", fmt.Sprintf("blades agent 执行失败: %v", err))
		if len(allResults) > 0 {
			// 已完成部分工具操作，附带步数返回供上层决策
			return fmt.Sprintf("agent 执行失败(已完成%d步工具操作): %v", len(allResults), err), allResults
		}
		return "", nil
	}

	// 提取最终文本输出
	finalText := ""
	if output != nil {
		finalText = output.Text()
	}

	// 完成门控：任务要求写文件但无成功 WriteFile 记录 → 返回失败标记触发上层重试
	if taskRequiresWriteFile(task) && !hasWriteFileResult(allResults) {
		emit(ctx, "error", "任务要求写文件但未通过 WriteFile 落盘")
		// 失败标记前缀便于上层识别并触发重试
		return "[失败: 任务要求写文件但未调用 WriteFile 落盘] " + finalText, allResults
	}

	// 正常完成：返回最终文本与工具结果
	return finalText, allResults
}

// maxConsecutiveFailures 同一工具连续失败多少次后强制放弃重试。
//
// 设计意图：防止 LLM 在某个工具上陷入"失败-重试-再失败"的死循环，
//   达到阈值后通过 SetAction(ActionLoopExit) 强制跳出 blades Agent 循环。
const maxConsecutiveFailures = 3

// osSpecificHint 兼容保留：旧调用点若仍引用此函数不会断链。新代码请用 fmtEnvSection。
//
// 职责：根据当前 GOOS 返回对应的 OS 操作提示（打开浏览器/文件的命令差异）。
// 返回：可读的 OS 提示字符串。
// 副作用：无。
// 并发安全：纯函数。
func osSpecificHint() string {
	switch runtime.GOOS {
	case "windows":
		// Windows：用 start 打开，python3 通常叫 python，避免使用 xdg-open 等
		return "操作系统: Windows。打开浏览器用 `start <URL>`；打开文件用 `start <路径>`。" +
			"python3 在 Windows 上通常叫 python；不要使用 xdg-open / open / say 等 macOS/Linux 命令。" +
			"路径分隔符使用反斜杠 \\ 或在命令里用正斜杠 /。"
	case "darwin":
		// macOS：用 open 打开
		return "操作系统: macOS。打开浏览器用 `open <URL>`；打开文件用 `open <路径>`。"
	default:
		// Linux/Unix：用 xdg-open 打开
		return "操作系统: Linux/Unix。打开浏览器用 `xdg-open <URL>`。"
	}
}

// taskRequiresWriteFile 判断任务是否要求创建/写入文件。
// 用于"完成门控"：此类任务必须见到成功的 WriteFile 才算完成。
//
// 职责：基于关键词匹配判断任务是否涉及文件创建/写入动作。
// 参数：
//   - task：任务描述文本。
// 返回：true 表示该任务必须通过 WriteFile 工具落盘才算完成。
// 副作用：无。
// 并发安全：纯函数。
func taskRequiresWriteFile(task string) bool {
	// 统一小写以做大小写不敏感匹配
	lower := strings.ToLower(task)
	// 动词集合：表示"创建/写入"类动作
	verbs := []string{
		"写", "创建", "生成", "实现", "编写", "开发", "保存", "落盘",
		"write", "create", "generate", "implement", "build", "save",
	}
	// 文件类名词/扩展名集合：表示产出物是文件/代码
	fileHints := []string{
		"文件", "代码", "脚本", "程序", "游戏", "页面", "demo", "示例",
		"接口", "配置", "服务", "模块", "库", "工具",
		"file", "code", "script", "program", "game", "page", "server", "api", "config", "module", "lib",
		".go", ".py", ".js", ".ts", ".html", ".css", ".md", ".json", ".yaml", ".yml", ".sql",
	}
	// 第一步：必须命中至少一个写入类动词
	hasVerb := false
	for _, v := range verbs {
		if strings.Contains(lower, v) {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false // 无写入动词，不算写文件任务
	}
	// 第二步：必须同时命中文件类名词或扩展名
	for _, h := range fileHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// hasWriteFileResult 检查工具结果中是否已有成功的 WriteFile。
//
// 职责：扫描工具结果列表，确认是否存在成功的 WriteFile 调用记录。
// 参数：
//   - results：本次 agent 执行产生的全部工具结果。
// 返回：存在成功 WriteFile 时返回 true。
// 副作用：无。
// 并发安全：只读切片，安全。
func hasWriteFileResult(results []*ToolResult) bool {
	for _, r := range results {
		// 工具名为 WriteFile 且 Success 为 true 才算数
		if r.Tool == "WriteFile" && r.Success {
			return true
		}
	}
	return false
}

// summarizeToolResult 总结工具结果（避免上下文过长）。
//
// 职责：把单个工具结果压缩为一行简短摘要，供回灌给 LLM 或日志展示。
// 参数：
//   - r：工具执行结果。
// 返回：截断后的摘要字符串。
// 副作用：无。
// 并发安全：纯函数。
func summarizeToolResult(r *ToolResult) string {
	// 失败分支：优先展示错误，附带截断后的输出
	if r.Error != "" {
		out := r.Output
		if len(out) > 400 {
			out = out[:400] + "..." // 超长截断，避免上下文膨胀
		}
		if out != "" {
			return fmt.Sprintf("失败: %s\n[输出]\n%s", r.Error, out)
		}
		return fmt.Sprintf("失败: %s", r.Error)
	}
	// 成功分支：截断输出到 200 字符
	output := r.Output
	if len(output) > 200 {
		output = output[:200] + "..."
	}
	return output
}

// executeAssistantWithTools 助手使用 blades.Agent + 工具执行任务。
//
// 职责：助手节点执行任务的对外入口，负责从 ModelFactory 取模型与 blades provider，
//   设置 5 分钟超时，转调 executeWithTools。
// 参数：
//   - ctx：请求上下文。
//   - modelFactory：模型工厂，用于按角色 ID 获取 LLM 客户端与 blades provider。
//   - executor：本地工具沙箱执行器。
//   - roleDef：角色定义。
//   - task：任务描述。
//   - state：三层图状态。
//   - skillBrief：已装配技能简介。
//   - progress：进度回调。
//   - agentName：当前 agent 名。
// 返回：最终文本与工具结果列表；modelFactory 为 nil 或取模型失败时返回空。
// 副作用：通过 progress 推送事件；通过 executor 产生文件/命令副作用。
// 并发安全：单次调用安全；超时通过 context 控制。
func executeAssistantWithTools(
	ctx context.Context,
	modelFactory *model.ModelFactory,
	executor *ToolExecutor,
	roleDef *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
	skillBrief string,
	progress ProgressCallback,
	agentName string,
) (string, []*ToolResult) {
	// 工厂为空直接返回，避免空指针
	if modelFactory == nil {
		return "", nil
	}

	// 按角色 ID 取 LLM 客户端
	llm, err := modelFactory.GetModel(ctx, roleDef.ID)
	if err != nil {
		return "", nil // 取模型失败，静默返回
	}

	// 取 blades provider；mock 路径（无 API Key）返回错误，传入 nil 触发退化
	provider, _ := modelFactory.GetBladesProvider(ctx, roleDef.ID)

	// 带超时执行：单个 assistant 最多 5 分钟（12 轮 function calling，每轮 LLM 可达 40s+）
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	// 转调核心执行函数
	return executeWithTools(ctx, provider, llm, executor, roleDef, task, state, skillBrief, progress, agentName)
}
