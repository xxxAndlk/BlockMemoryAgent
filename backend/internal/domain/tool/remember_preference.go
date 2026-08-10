package tool

// remember_preference.go 实现用户画像显式写入工具（TODO #28 双路写入之 a）：
// 用户表达偏好（"记住我偏好 X"）时，MetaAgent 调本工具立即写入画像，不等会话结束提取。

import (
	"context"
	"strings"
)

// UserProfileHookFunc 是 remember_preference 工具的会话层回调：
// 把偏好文本结构化追加进用户画像（偏好小节）。
type UserProfileHookFunc func(ctx context.Context, text string) error

// rememberPreferenceTool 实现 remember_preference 工具。
type rememberPreferenceTool struct {
	hook UserProfileHookFunc
}

// SetUserProfileHook 注入用户画像写入回调。
// bootstrap 接线到 userprofile.Store.Append；nil 时工具返回未配置错误。
func (r *Registry) SetUserProfileHook(h UserProfileHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["remember_preference"].(*rememberPreferenceTool); ok {
		t.hook = h
	}
}

// Name 返回工具名称。
func (t *rememberPreferenceTool) Name() string { return "remember_preference" }

// Aliases 返回工具别名列表，当前无别名。
func (t *rememberPreferenceTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *rememberPreferenceTool) Description() string {
	return "把用户明确表达的偏好写入用户画像（如「记住我偏好 X」「以后直接改别问」）。" +
		"仅当用户**明确要求记住**或主动陈述稳定偏好时调用，不要臆测；" +
		"画像已自动注入本 Agent 系统提示词，写入后未来所有会话生效。参数 text 为要记住的偏好陈述。"
}

// Execute 执行 remember_preference 工具调用。
func (t *rememberPreferenceTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "remember_preference", Error: "用户画像未接线（服务未注入 UserProfileHook）"}
	}
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return &Result{Tool: "remember_preference", Error: "text is required"}
	}
	if err := t.hook(ctx, strings.TrimSpace(text)); err != nil {
		return &Result{Tool: "remember_preference", Error: "写入失败: " + err.Error()}
	}
	return &Result{Tool: "remember_preference", Success: true, Output: "已记住: " + strings.TrimSpace(text)}
}
