package tool

// workflow_hook.go 定义工作流量产工具（workflow_run 预算审批 / workflow_freeze 配方草案确认）
// 的会话层确认回调类型。与 escalate_gear 的 hook 同范式：工具只持有 hook 字段，
// 会话层（bootstrap 注入 ReactService.WorkflowConfirmHook()）经 askUser 通道推确认卡阻塞等用户裁决。
//
// 类型放 domain/tool 而非 workflow 包：agent 包（hook 实现方）不依赖 internal/workflow
//（workflow 不得 import agent，反向也应避免），hook 是双方共同消费的最小契约。

import "context"

// WorkflowConfirmOption 是确认卡的一个选项。
type WorkflowConfirmOption struct {
	ID          string // 选项 ID（推荐 confirm / reject，答复经 resolveApproval 裁决）
	Label       string // 按钮文案
	Description string // 补充说明（可为空）
}

// WorkflowConfirmRequest 是工作流工具的确认卡入参。
type WorkflowConfirmRequest struct {
	// Title 卡片场景标题（进 ClarifyRequest.Context，如"工作流量产审批"）。
	Title string
	// Question 正文（用户可见；workflow_freeze 会把 YAML 草案原文放这里）。
	Question string
	// Options 选项列表（首个通常为确认）。为空时按 confirm/reject 两选项兜底。
	Options []WorkflowConfirmOption
	// EditHint 非空时提示用户可直接回复修改稿（freeze 草案确认用），附在正文尾。
	EditHint string
}

// WorkflowConfirmResult 是用户的裁决结果。
type WorkflowConfirmResult struct {
	// Approved 是否选择确认（resolveApproval 裁决；自由文本按保守拒绝）。
	Approved bool
	// Answer 用户答复原文（freeze 场景可能是用户手改的 YAML 全文）。
	Answer string
}

// WorkflowConfirmHookFunc 是工作流工具的会话层回调：推确认卡阻塞等用户裁决。
// 会话取消/超时返回 ctx 错误（工具侧按"未确认"处理，不建 run / 不落盘）。
type WorkflowConfirmHookFunc func(ctx context.Context, req WorkflowConfirmRequest) (WorkflowConfirmResult, error)
