package tool

// ask_user.go 实现 ask_user 工具（TODO #24 人在回路）：
// Agent（meta/domain）在任务执行中主动向用户提问，阻塞等待答复；
// 答复作为工具结果返回发问 Agent（恢复后工具结果入史，ReAct 继续）。
// 与破坏性工具审批（approvalHook，#17 P1）共用同一会话暂停/恢复通道，
// 本工具是通用提问，审批是其一消费者。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AskUserHookFunc 是 ask_user 工具的会话层回调：置 PendingClarify + 暂停会话 +
// 推送问题给用户，阻塞等答复；返回用户原始答复文本。
// 会话取消时返回 ctx 错误；超时未答复返回 ErrAskUserTimeout（工具转"自行决策"）。
type AskUserHookFunc func(ctx context.Context, question string) (string, error)

// ErrAskUserTimeout 用户超时未答复 ask_user 提问。
var ErrAskUserTimeout = errors.New("ask user: timeout waiting for answer")

// askUserTool 实现 ask_user 工具：向用户提出一个问题并等待答复。
// 未注入 hook（服务未接线）时返回未配置错误。
type askUserTool struct {
	hook AskUserHookFunc
	// defaultTimeout 提问默认超时（秒）；<=0 不限。单次调用 timeout_sec 参数覆盖。
	defaultTimeoutSec int
}

// SetAskUserHook 注入 ask_user 工具的会话层回调。
// bootstrap 在 ReactService 装配后调用（agentSvc.AskUserHook()）；nil 时工具返回未配置错误。
func (r *Registry) SetAskUserHook(h AskUserHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["ask_user"].(*askUserTool); ok {
		t.hook = h
	}
}

// SetAskUserTimeoutDefault 设置 ask_user 提问默认超时（秒；<=0 不限）。
// bootstrap 从 config.AskUserTimeoutSec 注入；单次调用 timeout_sec 参数覆盖。
func (r *Registry) SetAskUserTimeoutDefault(sec int) {
	if r == nil {
		return
	}
	if t, ok := r.tools["ask_user"].(*askUserTool); ok {
		t.defaultTimeoutSec = sec
	}
}

// Name 返回工具名称。
func (t *askUserTool) Name() string { return "ask_user" }

// Aliases 返回工具别名列表，当前无别名。
func (t *askUserTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *askUserTool) Description() string {
	return "向用户提出一个问题并等待答复（人在回路）。" +
		"适用场景：需求不明确、关键决策需要用户拍板、信息缺失导致无法继续时主动询问，" +
		"不要自行臆测关键需求。调用会暂停任务等待用户答复；" +
		"参数 question 为要问的问题（简洁、可答、一问答一件事）；" +
		"timeout_sec 可选（>0 时超时未答复返回\"用户未答复，自行决策\"，0=不限，默认 0）。" +
		"答复作为本工具结果返回。"
}

// Execute 执行 ask_user 工具调用。
func (t *askUserTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "ask_user", Error: "ask_user 未接线（会话服务未注入 AskUserHook）"}
	}
	q, _ := args["question"].(string)
	if q == "" {
		return &Result{Tool: "ask_user", Error: "question is required"}
	}
	timeoutSec, _ := args["timeout_sec"].(float64)
	if timeoutSec <= 0 && t.defaultTimeoutSec > 0 {
		timeoutSec = float64(t.defaultTimeoutSec)
	}

	askCtx := ctx
	cancel := func() {}
	if timeoutSec > 0 {
		askCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	}
	defer cancel()

	answer, err := t.hook(askCtx, q)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAskUserTimeout) {
			return &Result{
				Tool:    "ask_user",
				Success: true,
				Output:  "用户未答复，自行决策。",
			}
		}
		// 会话取消等：中止调用（ReAct 循环随 ctx 退出）。
		return &Result{Tool: "ask_user", Error: fmt.Sprintf("提问中断: %v", err)}
	}
	return &Result{
		Tool:    "ask_user",
		Success: true,
		Output:  "用户答复: " + answer,
	}
}
