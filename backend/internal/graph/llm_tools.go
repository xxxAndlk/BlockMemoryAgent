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
func mergeToolList(skillBrief, defaultTools string) string {
	if skillBrief == "" {
		return defaultTools
	}
	seen := make(map[string]bool)
	var out []string
	for _, line := range strings.Split(skillBrief, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := toolNameFromLine(line)
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, line)
	}
	for _, line := range strings.Split(defaultTools, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := toolNameFromLine(line)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// toolNameFromLine 从 "- WriteFile: 写入文件..." 中提取 "WriteFile"
func toolNameFromLine(line string) string {
	s := strings.TrimPrefix(strings.TrimSpace(line), "-")
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, ":"); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	return ""
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
	var allResults []*ToolResult

	sessionID := ""
	if state != nil {
		sessionID = state.SessionID
	}
	emit := func(ctx context.Context, kind, msg string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg})
		}
	}
	emitDetail := func(ctx context.Context, kind, msg, detail string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg, Detail: detail})
		}
	}
	emit(ctx, "think", "助手开始执行任务: "+task)

	// skillBrief 作为技能简介注入 prompt（实际工具调用走 function calling）。
	skillSection := mergeToolList(skillBrief, "")

	// 系统环境提示：工作目录 / OS / DB 连接 / 运行时，避免 LLM 瞎猜路径或连不上库
	envSection := fmtEnvSection()

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

	contextInfo := ""
	if state != nil && state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("\n当前领域: %s\n领域目标: %s", state.CurrentDomain, state.DomainGoal)
	}
	userMsg := fmt.Sprintf("任务: %s%s\n\n请分析任务并执行。如果需要查看文件或执行命令，请调用工具。", task, contextInfo)

	// mock 路径：无 blades provider，退化为单次 LLM 调用
	if provider == nil {
		emit(ctx, "llm", "无 blades provider（mock 模式），单次 LLM 调用")
		resp, err := llm.Generate(ctx, systemPrompt+"\n\n"+userMsg)
		if err != nil {
			emit(ctx, "error", fmt.Sprintf("LLM 调用失败: %v", err))
			return "", nil
		}
		return resp, nil
	}

	tools := buildBladesTools(executor, progress, sessionID, agentName, &allResults)

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

	emit(ctx, "llm", "启动 blades agent 执行（最多 12 轮 function calling）")
	start := time.Now()
	runner := blades.NewRunner(agent)
	output, err := runner.Run(ctx, blades.UserMessage(userMsg))
	dur := time.Since(start)
	emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] blades agent 完成 dur=%v", agentName, dur.Round(time.Millisecond)), "")

	if err != nil {
		emit(ctx, "error", fmt.Sprintf("blades agent 执行失败: %v", err))
		if len(allResults) > 0 {
			return fmt.Sprintf("agent 执行失败(已完成%d步工具操作): %v", len(allResults), err), allResults
		}
		return "", nil
	}

	finalText := ""
	if output != nil {
		finalText = output.Text()
	}

	// 完成门控：任务要求写文件但无成功 WriteFile 记录 → 返回失败标记触发上层重试
	if taskRequiresWriteFile(task) && !hasWriteFileResult(allResults) {
		emit(ctx, "error", "任务要求写文件但未通过 WriteFile 落盘")
		return "[失败: 任务要求写文件但未调用 WriteFile 落盘] " + finalText, allResults
	}

	return finalText, allResults
}

// maxConsecutiveFailures 同一工具连续失败多少次后强制放弃重试
const maxConsecutiveFailures = 3

// osSpecificHint 兼容保留：旧调用点若仍引用此函数不会断链。新代码请用 fmtEnvSection。
func osSpecificHint() string {
	switch runtime.GOOS {
	case "windows":
		return "操作系统: Windows。打开浏览器用 `start <URL>`；打开文件用 `start <路径>`。" +
			"python3 在 Windows 上通常叫 python；不要使用 xdg-open / open / say 等 macOS/Linux 命令。" +
			"路径分隔符使用反斜杠 \\ 或在命令里用正斜杠 /。"
	case "darwin":
		return "操作系统: macOS。打开浏览器用 `open <URL>`；打开文件用 `open <路径>`。"
	default:
		return "操作系统: Linux/Unix。打开浏览器用 `xdg-open <URL>`。"
	}
}

// taskRequiresWriteFile 判断任务是否要求创建/写入文件。
// 用于"完成门控"：此类任务必须见到成功的 WriteFile 才算完成。
func taskRequiresWriteFile(task string) bool {
	lower := strings.ToLower(task)
	verbs := []string{
		"写", "创建", "生成", "实现", "编写", "开发", "保存", "落盘",
		"write", "create", "generate", "implement", "build", "save",
	}
	fileHints := []string{
		"文件", "代码", "脚本", "程序", "游戏", "页面", "demo", "示例",
		"接口", "配置", "服务", "模块", "库", "工具",
		"file", "code", "script", "program", "game", "page", "server", "api", "config", "module", "lib",
		".go", ".py", ".js", ".ts", ".html", ".css", ".md", ".json", ".yaml", ".yml", ".sql",
	}
	hasVerb := false
	for _, v := range verbs {
		if strings.Contains(lower, v) {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false
	}
	for _, h := range fileHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// hasWriteFileResult 检查工具结果中是否已有成功的 WriteFile
func hasWriteFileResult(results []*ToolResult) bool {
	for _, r := range results {
		if r.Tool == "WriteFile" && r.Success {
			return true
		}
	}
	return false
}

// summarizeToolResult 总结工具结果（避免上下文过长）
func summarizeToolResult(r *ToolResult) string {
	if r.Error != "" {
		out := r.Output
		if len(out) > 400 {
			out = out[:400] + "..."
		}
		if out != "" {
			return fmt.Sprintf("失败: %s\n[输出]\n%s", r.Error, out)
		}
		return fmt.Sprintf("失败: %s", r.Error)
	}
	output := r.Output
	if len(output) > 200 {
		output = output[:200] + "..."
	}
	return output
}

// executeAssistantWithTools 助手使用 blades.Agent + 工具执行任务
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
	if modelFactory == nil {
		return "", nil
	}

	llm, err := modelFactory.GetModel(ctx, roleDef.ID)
	if err != nil {
		return "", nil
	}

	// 取 blades provider；mock 路径（无 API Key）返回错误，传入 nil 触发退化
	provider, _ := modelFactory.GetBladesProvider(ctx, roleDef.ID)

	// 带超时执行：单个 assistant 最多 5 分钟（12 轮 function calling，每轮 LLM 可达 40s+）
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	return executeWithTools(ctx, provider, llm, executor, roleDef, task, state, skillBrief, progress, agentName)
}
