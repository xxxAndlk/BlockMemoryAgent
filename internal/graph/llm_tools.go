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
) (string, []*ToolResult) {
	var allResults []*ToolResult

	skillSection := skillBrief
	if skillSection == "" {
		skillSection = `- ReadFile: 读取文件内容。参数: {"path": "文件路径"}
- WriteFile: 写入文件。参数: {"path": "文件路径", "content": "文件内容"}
- ListDir: 列出目录内容。参数: {"path": "目录路径"}
- RunCommand: 执行shell命令。参数: {"command": "命令", "timeout": 秒数}
- SearchInFiles: 在文件中搜索。参数: {"pattern": "搜索模式", "dir": "目录"}
- HTTPGet: HTTP GET 请求。参数: {"url": "...", "headers": {...}}
- HTTPPost: HTTP POST 请求。参数: {"url": "...", "headers": {...}, "body": {...}}`
	}

	systemPrompt := fmt.Sprintf(`%s

你可以使用以下工具来完成任务:

%s

当你需要调用工具时，输出JSON格式:
{"tool": "工具名", "args": {"参数名": "参数值"}}

如果不需要工具，直接输出最终答案。
每次只调用一个工具，等待结果后再决定下一步。`, roleDef.SystemPrompt, skillSection)

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

		resp, err := llm.Generate(ctx, prompt)
		if err != nil {
			if len(allResults) > 0 {
				return fmt.Sprintf("LLM调用失败(已完成%d步工具操作): %v", len(allResults), err), allResults
			}
			return "", nil // LLM不可用，回退到其他模式
		}

		// 尝试解析工具调用
		toolReq := parseToolCall(resp)
		if toolReq == nil {
			// 不是工具调用：若 LLM 声称"已写/已创建文件"但工具结果里没有 WriteFile，
			// 视为幻觉，强制追加一轮验证，不能直接结束。
			if claimsFileWrite(resp) && !hasWriteFileResult(allResults) {
				prompt += "\n\n[系统提示] 你声称已写文件，但工具执行记录中没有 WriteFile 调用。" +
					"请立即输出 WriteFile 工具调用以真正落盘，禁止用文字描述代替。"
				prompt += "\n用户原始任务: " + task
				// 再给一轮机会
				resp2, err2 := llm.Generate(ctx, prompt)
				if err2 == nil {
					if req2 := parseToolCall(resp2); req2 != nil {
						result := executor.Execute(ctx, req2.Tool, req2.Args)
						allResults = append(allResults, result)
						continue
					}
					// 仍不调用工具：返回带警告的最终结果
					return resp2 + "\n[警告: LLM 声称写文件但未实际调用 WriteFile]", allResults
				}
			}
			// 不是工具调用，视为最终答案
			return resp, allResults
		}

		// 执行工具
		result := executor.Execute(ctx, toolReq.Tool, toolReq.Args)
		allResults = append(allResults, result)
	}

	return "达到最大工具调用轮数", allResults
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

	return executeWithTools(ctx, llm, executor, roleDef, task, state, skillBrief)
}
