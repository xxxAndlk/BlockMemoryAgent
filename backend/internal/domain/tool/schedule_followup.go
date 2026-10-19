package tool

// schedule_followup.go 实现 schedule_followup 工具：给"当前会话"排一条未来回查提醒，
// 到点由 goal 调度器把提醒文本注入会话并恢复推进（2026-10-18，配合 goal/loop 指令模式）。
//
// 典型场景：目标模式会话推进到"开店资质审核"一步，审核需 1-3 个工作日——Agent 用本工具
// 排 24 小时后回查，本轮直接结束；到点系统注入"【系统·回查】…"，Agent 检查审核结果，
// 未通过则再排下一次。普通会话也可用（几天后提醒我检查 X）。
//
// 工具只做参数解析与基本校验，持久化由注入的 hook 完成（bootstrap 接线 goal.Manager）；
// 会话 ID 从 ctx 取（tool.SessionIDFromContext，Agent 派发工具时已注入）。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// FollowupHookFunc 是 schedule_followup 工具的持久化回调：
// delay 为回查间隔（已由工具侧收敛到 ≥1 分钟），返回人类可读摘要。
type FollowupHookFunc func(ctx context.Context, sessionID string, delayMin int, text string) (string, error)

// scheduleFollowupTool 实现 schedule_followup 工具；未注入 hook 时返回未配置错误。
type scheduleFollowupTool struct {
	hook FollowupHookFunc
}

// SetFollowupHook 注入 schedule_followup 工具的持久化回调。
// bootstrap 在 goal.Manager 就绪后调用；nil 时工具返回未配置错误。
func (r *Registry) SetFollowupHook(h FollowupHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["schedule_followup"].(*scheduleFollowupTool); ok {
		t.hook = h
	}
}

// Name 返回工具名称。
func (t *scheduleFollowupTool) Name() string { return "schedule_followup" }

// Aliases 返回工具别名列表，当前无别名。
func (t *scheduleFollowupTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *scheduleFollowupTool) Description() string {
	return "给当前会话安排一次未来回查：到点系统把回查文本注入本会话并恢复推进（配合目标/循环" +
		"指令模式使用）。典型用法——任务受外部流程阻塞（开店资质审核需数天、第三方处理未知时长）时，" +
		"按外部流程预估安排回查（如审核 1-3 个工作日则先排 24 小时=1440 分钟），安排好之后本轮" +
		"直接结束即可，不要空转等待，也不要自写 sleep/定时脚本；到点检查进度，未通过则再排下一次。" +
		"参数：delay_minutes 多少分钟后回查（必填，≥1）；text 回查时要做的事（必填，写清检查什么、" +
		"未通过时怎么续排）。"
}

// InputSchema 返回入参 JSON Schema。
func (t *scheduleFollowupTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"delay_minutes": {Type: "number", Description: "多少分钟后回查（≥1；审核 1-3 个工作日可先排 1440）"},
			"text":          {Type: "string", Description: "回查时要做的事：检查什么、期望结果、未通过时怎么续排"},
		},
		Required: []string{"delay_minutes", "text"},
	}
}

// Execute 执行 schedule_followup 工具调用：解析参数并委托 hook 持久化。
func (t *scheduleFollowupTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "schedule_followup", Error: "schedule_followup 未接线（goal 管理器未注入 FollowupHook）"}
	}
	sid := SessionIDFromContext(ctx)
	if sid == "" {
		return &Result{Tool: "schedule_followup", Error: "无法确定当前会话（ctx 未注入 session id）"}
	}
	delay, ok := args["delay_minutes"].(float64)
	if !ok || delay < 1 {
		return &Result{Tool: "schedule_followup", Error: "delay_minutes 非法（需 ≥1 的分钟数）"}
	}
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return &Result{Tool: "schedule_followup", Error: "text is required"}
	}
	summary, err := t.hook(ctx, sid, int(delay+0.5), text)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return &Result{Tool: "schedule_followup", Error: fmt.Sprintf("安排回查中断: %v", err)}
		}
		return &Result{Tool: "schedule_followup", Error: err.Error()}
	}
	return &Result{Tool: "schedule_followup", Success: true, Output: summary}
}
