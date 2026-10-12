package tool

// evolution_prompt.go 实现 update_evolution_prompt 工具（AI 自维护角色演化准则）：
// 角色把跨任务通用的抽象准则/教训全量写入本角色的演化准则存储，准则逐字注入
// 该角色今后的系统提示词【自进化准则】段，实现经验跨会话自沉淀。
// hook 注入模式同 remember_preference：注册始终发生使 Schema 可见，
// bootstrap 经 SetEvolutionPromptHook 接线；未接线时 Execute 返回"未接线"错误。

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// maxEvolutionPromptRunes 是自进化准则的硬性字符上限（按字符计，超限拒绝）。
const maxEvolutionPromptRunes = 300

// updateEvolutionPromptTool 实现 update_evolution_prompt 工具。
type updateEvolutionPromptTool struct {
	// hook 是准则写入回调（bootstrap 接 RoleEvolutionStore.Upsert）；
	// nil 时工具返回未接线错误。
	hook func(ctx context.Context, roleID, content, sessionID string) error
}

// Name 返回工具名称。
func (t *updateEvolutionPromptTool) Name() string { return "update_evolution_prompt" }

// Aliases 返回工具别名列表，当前无别名。
func (t *updateEvolutionPromptTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *updateEvolutionPromptTool) Description() string {
	return "更新**本角色**的自进化准则（抽象行为准则，会逐字注入你今后的系统提示词）。" +
		"只记录跨任务通用的抽象准则/教训（如「修改代码后必须跑构建验证再交付」），" +
		"不要写具体任务细节或文件路径。全量替换：先读你系统提示词【自进化准则】段的现行内容，" +
		"合并新旧后整体提交。硬性上限 300 字（按字符计），超限会被拒绝。"
}

// InputSchema 返回 LLM 可见的参数 schema。
func (t *updateEvolutionPromptTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"content": {
				Type:        "string",
				Description: "自进化准则全文（全量替换，<=300 字；只写跨任务通用的抽象准则/教训）",
			},
		},
		Required: []string{"content"},
	}
}

// Execute 执行 update_evolution_prompt 工具调用。
// 准则归属按 ctx 携带的角色 ID 定位（顶层与子 Agent 均由 react_agent 注入），
// 缺失角色上下文时拒绝写入，防止准则错挂。
func (t *updateEvolutionPromptTool) Execute(ctx context.Context, args map[string]any) *Result {
	roleID := RoleIDFromContext(ctx)
	if roleID == "" {
		return &Result{Tool: "update_evolution_prompt", Error: "缺少角色上下文，无法定位准则归属"}
	}
	content, _ := args["content"].(string)
	content = strings.TrimSpace(content)
	if content == "" {
		return &Result{Tool: "update_evolution_prompt", Error: "content is required"}
	}
	n := utf8.RuneCountInString(content)
	if n > maxEvolutionPromptRunes {
		return &Result{Tool: "update_evolution_prompt",
			Error: fmt.Sprintf("超过 300 字上限（当前 %d 字），请精简后重试", n)}
	}
	if t.hook == nil {
		return &Result{Tool: "update_evolution_prompt", Error: "演化准则存储未接线"}
	}
	if err := t.hook(ctx, roleID, content, SessionIDFromContext(ctx)); err != nil {
		return &Result{Tool: "update_evolution_prompt", Error: "写入失败: " + err.Error()}
	}
	return &Result{Tool: "update_evolution_prompt", Success: true,
		Output: fmt.Sprintf("已更新本角色自进化准则（%d 字）：\n%s", n, content)}
}
