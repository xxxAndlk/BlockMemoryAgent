package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/types"
)

// ToolCallRequest LLM工具调用请求
type ToolCallRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

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

// executeWithTools 使用LLM+工具循环执行任务
// LLM分析任务，决定调用哪些工具，执行后返回结果给LLM继续分析，直到完成
//
// 支持可选的 skillBrief：当 DomainAgent 已装配 Skill 子集时，将其作为
// "可见技能列表"加入 system prompt，避免把全局 ToolExecutor 全部
// 工具集对子 Agent 暴露（v3 §5.3）。
func executeWithTools(
	ctx context.Context,
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

	emit := func(kind, msg string) {
		if progress != nil {
			progress(ProgressEvent{Kind: kind, Agent: agentName, Message: msg})
		}
	}
	emitDetail := func(kind, msg, detail string) {
		if progress != nil {
			progress(ProgressEvent{Kind: kind, Agent: agentName, Message: msg, Detail: detail})
		}
	}
	emit("think", "助手开始执行任务: "+task)

	// 默认工具列表（CamelCase，与 ToolExecutor.Execute case 名一致）。
	// 无论 DomainAgent 是否装配 SkillSet，都保留这套兜底工具，
	// 避免 LLM 在 skillBrief 为空或与 ToolRef 不一致时无工具可用。
	defaultTools := `- ReadFile: 读取文件内容。参数: {"path": "文件路径"}
- WriteFile: 写入文件。参数: {"path": "文件路径", "content": "文件内容"}
- ListDir: 列出目录内容。参数: {"path": "目录路径"}
- RunCommand: 执行shell命令。参数: {"command": "命令", "timeout": 秒数}
- SearchInFiles: 在文件中搜索。参数: {"pattern": "搜索模式", "dir": "目录"}
- HTTPGet: HTTP GET 请求。参数: {"url": "...", "headers": {...}}
- HTTPPost: HTTP POST 请求。参数: {"url": "...", "headers": {...}, "body": {...}}`

	// skillBrief 是 DomainAgent 装配的 Skill 子集（含 ToolRef + 参数 schema）。
	// 合并而非替换默认列表：Skill 子集作为"优先关注"项前置，默认工具作为兜底。
	// 去重以 ToolRef 为准。
	skillSection := mergeToolList(skillBrief, defaultTools)

	systemPrompt := fmt.Sprintf(`%s

你可以使用以下工具来完成任务:

%s

当你需要调用工具时，输出JSON格式:
{"tool": "工具名", "args": {"参数名": "参数值"}}

【硬性规则】
1. 凡任务涉及"创建/写入/生成/实现/编写"文件或代码，必须调用 WriteFile 工具真正落盘，
   禁止只用文字描述代码内容当作完成。代码必须通过 WriteFile 的 content 参数写入磁盘。
2. 凡任务涉及"运行/执行/启动"程序，必须调用 RunCommand 工具实际执行，禁止只描述如何运行。
3. 工具未成功执行前，不得宣称任务完成。
4. 每次只调用一个工具，等待结果后再决定下一步。
5. 只有当所有要求的文件已落盘、命令已执行，且无需再调用工具时，才输出最终文字总结。`, roleDef.SystemPrompt, skillSection)

	contextInfo := ""
	if state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("\n当前领域: %s\n领域目标: %s", state.CurrentDomain, state.DomainGoal)
	}

	userMsg := fmt.Sprintf("任务: %s%s\n\n请分析任务并执行。如果需要查看文件或执行命令，请调用工具。", task, contextInfo)

	// 最多8轮工具调用（提高以容纳验证轮次）
	maxRounds := 8
	for round := 0; round < maxRounds; round++ {
		prompt := systemPrompt + "\n\n" + userMsg
		if len(allResults) > 0 {
			prompt += "\n\n已执行的步骤:\n"
			for i, r := range allResults {
				prompt += fmt.Sprintf("%d. [%s] %s\n", i+1, r.Tool, summarizeToolResult(r))
			}
			prompt += "\n请根据以上结果继续分析，或输出最终结论。"
		}

		emit("llm", fmt.Sprintf("第 %d 轮：调用 LLM 决策下一步...", round+1))
		resp, err := llm.Generate(ctx, prompt)
		if err != nil {
			emit("error", fmt.Sprintf("LLM 调用失败: %v", err))
			if len(allResults) > 0 {
				return fmt.Sprintf("LLM调用失败(已完成%d步工具操作): %v", len(allResults), err), allResults
			}
			return "", nil // LLM不可用，回退到其他模式
		}

		// 尝试解析工具调用
		toolReq := parseToolCall(resp)
		if toolReq == nil {
			// 不是工具调用。
			needWrite := taskRequiresWriteFile(task)
			alreadyWritten := hasWriteFileResult(allResults)

			// 情况 1：LLM 声称已写文件但实际没有 WriteFile 记录 → 幻觉，强制要求落盘
			if claimsFileWrite(resp) && !alreadyWritten {
				emit("think", "LLM 声称已写文件但无 WriteFile 记录，疑似幻觉，强制要求调用工具")
				prompt += "\n\n[系统提示] 你声称已写文件，但工具执行记录中没有 WriteFile 调用。" +
					"请立即输出 WriteFile 工具调用以真正落盘，禁止用文字描述代替。"
				prompt += "\n用户原始任务: " + task
				resp2, err2 := llm.Generate(ctx, prompt)
				if err2 == nil {
					if req2 := parseToolCall(resp2); req2 != nil {
						result := executor.Execute(ctx, req2.Tool, req2.Args)
						allResults = append(allResults, result)
						continue
					}
				}
				return resp + "\n[警告: LLM 声称写文件但未实际调用 WriteFile]", allResults
			}

			// 情况 2：任务要求写文件，但还没成功 WriteFile，且 LLM 想直接给文字答案
			// → 不接受，强制继续要求 WriteFile（除非已是最后一轮）
			if needWrite && !alreadyWritten {
				if round < maxRounds-1 {
					emit("intend", "任务要求写文件但尚未落盘，拒绝文字答案，强制要求调用 WriteFile")
					forcePrompt := prompt + "\n\n[系统提示] 任务要求创建/写入文件，但你尚未调用 WriteFile。" +
						"请立即输出 WriteFile 工具调用，将完整代码写入目标路径。禁止再用文字描述。"
					resp2, err2 := llm.Generate(ctx, forcePrompt)
					if err2 == nil {
						if req2 := parseToolCall(resp2); req2 != nil {
							result := executor.Execute(ctx, req2.Tool, req2.Args)
							allResults = append(allResults, result)
							if result.Success && result.Tool == "WriteFile" {
								emit("think", "WriteFile 成功，任务完成")
								return resp2, allResults
							}
							continue
						}
					}
					// 仍不调用 → 继续下一轮（下一轮还会再逼一次），直至耗尽
					continue
				}
				// 最后一轮仍未写文件 → 返回明确失败标记
				emit("error", "达到最大轮数仍未通过 WriteFile 落盘，任务失败")
				return "[失败: 任务要求写文件但未调用 WriteFile 落盘] " + resp, allResults
			}

			emit("think", "LLM 未调用工具，视为最终答案")
			return resp, allResults
		}

		// 执行工具
		argsStr, _ := json.Marshal(toolReq.Args)
		emitDetail("tool_call", fmt.Sprintf("调用工具 %s", toolReq.Tool), string(argsStr))
		result := executor.Execute(ctx, toolReq.Tool, toolReq.Args)
		allResults = append(allResults, result)
		if result.Success {
			out := result.Output
			if len(out) > 200 {
				out = out[:200] + "..."
			}
			emitDetail("tool_result", fmt.Sprintf("工具 %s 执行成功", toolReq.Tool), out)
		} else {
			emit("error", fmt.Sprintf("工具 %s 执行失败: %s", toolReq.Tool, result.Error))
		}
	}

	return "达到最大工具调用轮数", allResults
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
		"file", "code", "script", "program", "game", "page",
		".go", ".py", ".js", ".ts", ".html", ".css", ".md", ".json",
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

// claimsFileWrite 粗略检测 LLM 文本是否声称已经写入/创建了文件
func claimsFileWrite(resp string) bool {
	lower := strings.ToLower(resp)
	verbs := []string{
		"已写", "已创建", "已生成", "已经写", "已经创建", "已经生成",
		"已保存", "已经保存", "写入完成", "创建完成", "生成完成",
		"written", "created", "generated", "saved",
	}
	for _, v := range verbs {
		if strings.Contains(lower, v) {
			// 同时需要提到文件相关词，避免误报
			if strings.Contains(lower, "文件") || strings.Contains(lower, "file") ||
				strings.Contains(lower, ".go") || strings.Contains(lower, ".py") ||
				strings.Contains(lower, ".js") || strings.Contains(lower, ".ts") ||
				strings.Contains(lower, ".html") || strings.Contains(lower, ".md") {
				return true
			}
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

// parseToolCall 解析LLM输出中的工具调用
func parseToolCall(resp string) *ToolCallRequest {
	resp = strings.TrimSpace(resp)

	// 尝试直接解析
	var req ToolCallRequest
	if err := json.Unmarshal([]byte(extractJSON(resp)), &req); err == nil && req.Tool != "" {
		return &req
	}

	// 尝试提取 ```json ... ``` 块
	jsonStr := extractJSON(resp)
	if jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &req); err == nil && req.Tool != "" {
			return &req
		}
	}

	return nil
}

// summarizeToolResult 总结工具结果（避免上下文过长）
func summarizeToolResult(r *ToolResult) string {
	if r.Error != "" {
		return fmt.Sprintf("失败: %s", r.Error)
	}
	output := r.Output
	if len(output) > 200 {
		output = output[:200] + "..."
	}
	return output
}

// executeAssistantWithTools 助手使用LLM+工具执行任务
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

	// 带超时执行
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	return executeWithTools(ctx, llm, executor, roleDef, task, state, skillBrief, progress, agentName)
}
