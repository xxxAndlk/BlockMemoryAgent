package tool

// escalate_gear.go 实现升级档位工具（TODO #14 T7，D-2 留痕）：
// 快速档（chat 角色）接到的任务实为工程任务时，模型调本工具请求升级集群档；
// 会话层 hook 经 askUser 通道推确认卡，用户确认后切档 + 合成种子消息走
// "终态会话收消息→新 run" 既有通道以集群档（meta 全装）重启会话。
// 升级只升不降：本工具只能 fast→cluster；拒绝时维持快速档继续对话。
//
// 与 ask_user 的关系：共用同一会话暂停/恢复通道（pendingClarify + askUser），
// ask_user 是通用提问，本工具是档位升级专用确认（Kind=confirm 选项卡）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// EscalateGearRequest 是 escalate_gear 工具传给会话层 hook 的入参。
type EscalateGearRequest struct {
	// Reason 升级原因（模型写给用户看的一句话，如"需要跨 3 个文件改后端接口"）。
	Reason string
	// TaskBrief 任务简报：升级后集群档应完成的工作（自包含，含验收口径）。
	TaskBrief string
	// Files 模型判断与该任务相关的文件路径（相对工作目录，可空）。
	Files []string
}

// EscalateGearHookFunc 是 escalate_gear 的会话层回调：推确认卡阻塞等用户裁决，
// 确认后切档并安排集群档续跑。返回给模型的结果文本（用户拒绝时为"继续快速档"提示）。
// 会话取消返回 ctx 错误（ReAct 循环随 ctx 退出）。
type EscalateGearHookFunc func(ctx context.Context, req EscalateGearRequest) (string, error)

// escalateGearTool 实现 escalate_gear 工具；未注入 hook 时返回未配置错误。
type escalateGearTool struct {
	hook EscalateGearHookFunc
}

// SetEscalateGearHook 注入 escalate_gear 的会话层回调。
// bootstrap 在 ReactService 装配后调用（agentSvc.EscalateGearHook()）；nil 时工具返回未配置错误。
func (r *Registry) SetEscalateGearHook(h EscalateGearHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["escalate_gear"].(*escalateGearTool); ok {
		t.hook = h
	}
}

// Name 返回工具名称。
func (t *escalateGearTool) Name() string { return "escalate_gear" }

// Aliases 返回工具别名列表，当前无别名。
func (t *escalateGearTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *escalateGearTool) Description() string {
	return "请求升级到集群档（工程模式）。你当前处于快速档（轻量对话），当且仅当用户这轮输入" +
		"实为工程任务（要改代码/建文件/跑命令/多步操作）时调用本工具：传 reason（一句话向用户" +
		"说明为什么要升级）与 task_brief（自包含任务简报：目标、涉及文件、验收标准，集群档" +
		"看不到本次对话历史，全靠这段简报接手）。系统会向用户弹确认卡，确认后自动以集群档" +
		"重启会话（含你的进展摘要与文件清单），用户未确认则继续快速档对话。闲聊/快问快答" +
		"禁止调用。"
}

// InputSchema 返回入参 JSON Schema。
func (t *escalateGearTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"reason":     {Type: "string", Description: "升级原因（一句话，向用户说明为什么这轮需要工程能力）"},
			"task_brief": {Type: "string", Description: "任务简报（自包含，<=500 字）：目标、涉及文件路径、验收标准。集群档以这段简报为种子接手任务"},
			"files": {
				Type:        "array",
				Items:       &jsonschema.Schema{Type: "string"},
				Description: "相关文件路径列表（相对工作目录，可选）：随种子带给集群档",
			},
		},
		Required: []string{"reason", "task_brief"},
	}
}

// Execute 执行 escalate_gear：解析参数并委托会话层 hook。
func (t *escalateGearTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "escalate_gear", Error: "escalate_gear 未接线（会话服务未注入 EscalateGearHook）"}
	}
	reason, _ := args["reason"].(string)
	brief, _ := args["task_brief"].(string)
	if strings.TrimSpace(reason) == "" {
		return &Result{Tool: "escalate_gear", Error: "reason is required（要向用户说明升级原因）"}
	}
	if strings.TrimSpace(brief) == "" {
		return &Result{Tool: "escalate_gear", Error: "task_brief is required（集群档靠这段简报接手任务，不能为空）"}
	}
	var files []string
	if raw, ok := args["files"].([]any); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				files = append(files, strings.TrimSpace(s))
			}
		}
	}
	out, err := t.hook(ctx, EscalateGearRequest{Reason: strings.TrimSpace(reason), TaskBrief: strings.TrimSpace(brief), Files: files})
	if err != nil {
		return &Result{Tool: "escalate_gear", Error: fmt.Sprintf("升级流程中断: %v", err)}
	}
	return &Result{Tool: "escalate_gear", Success: true, Output: out}
}
