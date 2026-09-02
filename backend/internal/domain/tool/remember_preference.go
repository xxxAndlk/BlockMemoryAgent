package tool

// remember_preference.go 实现偏好显式写入工具（TODO #28 双路写入之 a + 2026-09-02 scope 扩展）：
// 用户表达偏好（"记住我偏好 X"）时，MetaAgent 调本工具立即写入，不等会话结束提取。
// scope=user 写用户画像（全局，跨项目）；scope=project 写本项目 .bma/project_preferences.md。

import (
	"context"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// UserProfileHookFunc 是 remember_preference 工具的会话层回调：
// 把偏好文本结构化追加进目标偏好文件（用户画像=偏好小节 / 项目偏好=项目约定小节）。
type UserProfileHookFunc func(ctx context.Context, text string) error

// rememberPreferenceTool 实现 remember_preference 工具。
type rememberPreferenceTool struct {
	userHook    UserProfileHookFunc
	projectHook UserProfileHookFunc
}

// SetUserProfileHook 注入用户画像写入回调（scope=user）。
// bootstrap 接线到 userprofile.Store.Append；nil 时该 scope 返回未配置错误。
func (r *Registry) SetUserProfileHook(h UserProfileHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["remember_preference"].(*rememberPreferenceTool); ok {
		t.userHook = h
	}
}

// SetProjectPreferenceHook 注入项目偏好写入回调（scope=project，2026-09-02 设计 §5）。
// bootstrap 接线到项目偏好 Store.Append("项目约定")；nil 时该 scope 返回未配置错误。
func (r *Registry) SetProjectPreferenceHook(h UserProfileHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["remember_preference"].(*rememberPreferenceTool); ok {
		t.projectHook = h
	}
}

// Name 返回工具名称。
func (t *rememberPreferenceTool) Name() string { return "remember_preference" }

// Aliases 返回工具别名列表，当前无别名。
func (t *rememberPreferenceTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *rememberPreferenceTool) Description() string {
	return "把用户明确表达的偏好写入偏好档案（如「记住我偏好 X」「以后直接改别问」）。" +
		"仅当用户**明确要求记住**或主动陈述稳定偏好时调用，不要臆测。scope=user 写用户画像" +
		"（跨项目全局，已自动注入你的系统提示词）；scope=project 写本项目约定（本项目所有 Agent 生效，" +
		"适用于「本项目用 pnpm」「提交前必须 make lint」等项目级约定）。用户画像全局偏好优先用 user。"
}

// InputSchema 返回 LLM 可见的参数 schema。
func (t *rememberPreferenceTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"text": {
				Type:        "string",
				Description: "要记住的偏好陈述，简洁完整（如「沟通风格: 直接给结论不要铺垫」）",
			},
			"scope": {
				Type:        "string",
				Enum:        []any{"user", "project"},
				Description: "写入范围：user=用户画像（全局，默认）；project=本项目约定",
			},
		},
		Required: []string{"text"},
	}
}

// Execute 执行 remember_preference 工具调用。
func (t *rememberPreferenceTool) Execute(ctx context.Context, args map[string]any) *Result {
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return &Result{Tool: "remember_preference", Error: "text is required"}
	}
	scope, _ := args["scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "user"
	}
	switch scope {
	case "user":
		if t.userHook == nil {
			return &Result{Tool: "remember_preference", Error: "用户画像未接线（服务未注入 UserProfileHook）"}
		}
		if err := t.userHook(ctx, text); err != nil {
			return &Result{Tool: "remember_preference", Error: "写入失败: " + err.Error()}
		}
	case "project":
		if t.projectHook == nil {
			return &Result{Tool: "remember_preference", Error: "项目偏好未接线（服务未注入 ProjectPreferenceHook）"}
		}
		if err := t.projectHook(ctx, text); err != nil {
			return &Result{Tool: "remember_preference", Error: "写入失败: " + err.Error()}
		}
	default:
		return &Result{Tool: "remember_preference", Error: "scope 只能是 user 或 project"}
	}
	return &Result{Tool: "remember_preference", Success: true,
		Output: "已记住(" + scope + "): " + text}
}
