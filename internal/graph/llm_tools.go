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
func executeWithTools(
	ctx context.Context,
	llm model.LLMClient,
	executor *ToolExecutor,
	roleDef *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
) (string, []*ToolResult) {
	var allResults []*ToolResult

	systemPrompt := fmt.Sprintf(`%s

你可以使用以下工具来完成任务:

- ReadFile: 读取文件内容。参数: {"path": "文件路径"}
- WriteFile: 写入文件。参数: {"path": "文件路径", "content": "文件内容"}
- ListDir: 列出目录内容。参数: {"path": "目录路径"}
- RunCommand: 执行shell命令。参数: {"command": "命令", "timeout": 秒数}
- SearchInFiles: 在文件中搜索。参数: {"pattern": "搜索模式", "dir": "目录"}

当你需要调用工具时，输出JSON格式:
{"tool": "工具名", "args": {"参数名": "参数值"}}

如果不需要工具，直接输出最终答案。
每次只调用一个工具，等待结果后再决定下一步。`, roleDef.SystemPrompt)

	contextInfo := ""
	if state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("\n当前领域: %s\n领域目标: %s", state.CurrentDomain, state.DomainGoal)
	}

	userMsg := fmt.Sprintf("任务: %s%s\n\n请分析任务并执行。如果需要查看文件或执行命令，请调用工具。", task, contextInfo)

	// 最多5轮工具调用
	maxRounds := 5
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
			// 不是工具调用，视为最终答案
			return resp, allResults
		}

		// 执行工具
		result := executor.Execute(ctx, toolReq.Tool, toolReq.Args)
		allResults = append(allResults, result)
	}

	return "达到最大工具调用轮数", allResults
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

	return executeWithTools(ctx, llm, executor, roleDef, task, state)
}
