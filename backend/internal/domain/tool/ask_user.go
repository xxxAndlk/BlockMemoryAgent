package tool

// ask_user.go 实现 ask_user 工具（TODO #24 人在回路，#53 结构化选项）：
// Agent（meta/domain）在任务执行中主动向用户提问，阻塞等待答复；
// 答复作为工具结果返回发问 Agent（恢复后工具结果入史，ReAct 继续）。
// 与破坏性工具审批（approvalHook，#17 P1）共用同一会话暂停/恢复通道，
// 本工具是通用提问，审批是其一消费者。
//
// 结构化选项（TODO #53）：方向不明确时模型应传 options（2-N 个方向）让用户
// 点选，而非开放式提问；纯确认场景可不传（hook 侧按 Kind=confirm 生成
// 确认/拒绝两选项）。multi_select=true 时用户可多选，逗号/空格分隔回传。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// AskUserOption 是 ask_user 工具的一个结构化选项（TODO #53）。
type AskUserOption struct {
	ID          string // ID 选项唯一标识
	Label       string // Label 选项展示文本
	Description string // Description 选项补充说明（可选）
}

// AskUserQuestion 是批量提问模式（questions 数组）中的单个问题，
// 字段与顶层单题入参一致；顶层 question/options/multi_select 保留向后兼容。
type AskUserQuestion struct {
	Question    string
	Options     []AskUserOption
	MultiSelect bool
}

// AskUserOptions 是 ask_user 工具传给 hook 的单题入参（TODO #53）。
// Options 为空 = 纯自由文本提问（旧行为）；MultiSelect=true 时用户可多选。
// Detail 为附加长上下文（如 submit_plan 的计划全文）：落对话区事件流展示，
// 不进问答面板（面板只显示 question + 选项）。批量模式在 Execute 侧拆成
// 多次单题 hook 调用，hook 签名不变。
type AskUserOptions struct {
	Options     []AskUserOption
	MultiSelect bool
	Detail      string
}

// AskUserHookFunc 是 ask_user 工具的会话层回调：置 PendingClarify + 暂停会话 +
// 推送问题给用户，阻塞等答复；返回用户原始答复文本。
// 会话取消时返回 ctx 错误；超时未答复返回 ErrAskUserTimeout（工具转"自行决策"）。
type AskUserHookFunc func(ctx context.Context, question string, opts AskUserOptions) (string, error)

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
	return "向用户提问并等待答复（人在回路）。推荐批量模式：questions 一次问齐 2-5 个" +
		"关键分叉（选错要返工、缺失无法定案的决策；不问琐碎，用户已明确的禁止再问），" +
		"答复按题号汇总返回——提问是最省时间的行动。说人话，每个选项讲清对结果的影响。" +
		"参数：questions 每项 {question,options,multi_select}；options 每项 {id,label,description}" +
		"（label 简短可点选，给候选不开放式让用户打字；multi_select=true 可多选）。" +
		"detail 可选：长上下文完整展示在对话区；timeout_sec 可选（>0 时超时未答复返回" +
		"\"自行决策\"，默认 0 不限）。"
}

// InputSchema 返回入参 JSON Schema（TODO #53 结构化选项）。
func (t *askUserTool) InputSchema() *jsonschema.Schema {
	optionSchema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{
			"id":          {Type: "string", Description: "选项唯一标识（如 dark/light）"},
			"label":       {Type: "string", Description: "选项展示文本（简短可点选）"},
			"description": {Type: "string", Description: "选项补充说明（可选）"},
		},
		Required: []string{"id", "label"},
	}
	optionArraySchema := &jsonschema.Schema{
		Type:        "array",
		Items:       optionSchema,
		Description: "结构化选项（2-N 个方向，方向不明确时必填；缺省=自由文本提问）",
	}
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"question":     {Type: "string", Description: "要问的问题（简洁、可答、一问答一件事）"},
			"options":      optionArraySchema,
			"multi_select": {Type: "boolean", Description: "是否允许多选（默认 false=单选）"},
			"timeout_sec":  {Type: "number", Description: "超时秒数（>0 时每题超时未答复自行决策，0=不限）"},
			"questions": {
				Type: "array",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"question":     {Type: "string", Description: "要问的问题"},
						"options":      optionArraySchema,
						"multi_select": {Type: "boolean", Description: "是否允许多选（默认 false）"},
					},
					Required: []string{"question"},
				},
				Description: "批量模式：2-5 个问题一次问齐（新任务开工前澄清必用），题目逐个呈现，答复按题号汇总返回",
			},
			"detail": {Type: "string", Description: "附加长上下文（如计划全文）：完整展示在对话区供用户滚动查看，问答面板只显示 question 与选项；question 写短引导语即可"},
		},
		Required: []string{"question"},
	}
}

// maxAskUserBatch 批量提问单次调用的问题数上限。
const maxAskUserBatch = 5

// parseAskUserQuestions 解析批量模式 questions 数组；为空时回退顶层单题字段
// （question/options/multi_select 包装成单元素列表），两条路径归一。
// question 与 questions 同时存在时 questions 优先。
func parseAskUserQuestions(args map[string]any) []AskUserQuestion {
	var qs []AskUserQuestion
	if raw, ok := args["questions"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			q, _ := m["question"].(string)
			if q == "" {
				continue
			}
			qs = append(qs, AskUserQuestion{
				Question:    q,
				Options:     parseOptionList(m["options"]),
				MultiSelect: boolOf(m["multi_select"]),
			})
		}
	}
	if len(qs) == 0 {
		if q, _ := args["question"].(string); q != "" {
			qs = []AskUserQuestion{{
				Question:    q,
				Options:     parseOptionList(args["options"]),
				MultiSelect: boolOf(args["multi_select"]),
			}}
		}
	}
	if len(qs) > maxAskUserBatch {
		qs = qs[:maxAskUserBatch]
	}
	return qs
}

// parseOptionList 解析 options 数组字段（[]any 或 []map[string]any 两种形态）。
func parseOptionList(v any) []AskUserOption {
	raw, ok := v.([]any)
	if !ok {
		if arr, ok2 := v.([]map[string]any); ok2 {
			raw = make([]any, 0, len(arr))
			for _, m := range arr {
				raw = append(raw, m)
			}
		}
	}
	var opts []AskUserOption
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		label, _ := m["label"].(string)
		if id == "" || label == "" {
			continue
		}
		desc, _ := m["description"].(string)
		opts = append(opts, AskUserOption{ID: id, Label: label, Description: desc})
	}
	return opts
}

// boolOf 宽松取布尔字段；缺失或类型不符返回 false。
func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// Execute 执行 ask_user 工具调用：批量模式逐题调 hook，答案按题号汇总。
func (t *askUserTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "ask_user", Error: "ask_user 未接线（会话服务未注入 AskUserHook）"}
	}
	questions := parseAskUserQuestions(args)
	if len(questions) == 0 {
		return &Result{Tool: "ask_user", Error: "question is required"}
	}
	timeoutSec, _ := args["timeout_sec"].(float64)
	if timeoutSec <= 0 && t.defaultTimeoutSec > 0 {
		timeoutSec = float64(t.defaultTimeoutSec)
	}
	detail, _ := args["detail"].(string)

	var sb strings.Builder
	sb.WriteString("用户答复:")
	for i, q := range questions {
		askCtx := ctx
		cancel := func() {}
		if timeoutSec > 0 {
			askCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		}
		answer, err := t.hook(askCtx, q.Question, AskUserOptions{Options: q.Options, MultiSelect: q.MultiSelect, Detail: detail})
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAskUserTimeout) {
				// 单题超时不中断批量：记未答继续，由模型对该题自行决策。
				// 不回显问题原文：问题里可能含"需要修改"等裁决词，回显会污染下游裁决。
				fmt.Fprintf(&sb, "\n%d. 答复: 用户未答复，自行决策。", i+1)
				continue
			}
			// 会话取消等：中止调用（ReAct 循环随 ctx 退出）。
			return &Result{Tool: "ask_user", Error: fmt.Sprintf("提问中断: %v", err)}
		}
		fmt.Fprintf(&sb, "\n%d. 答复: %s", i+1, answer)
	}
	return &Result{
		Tool:    "ask_user",
		Success: true,
		Output:  sb.String(),
	}
}
