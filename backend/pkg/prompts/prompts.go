// Package prompts 内置提示词单一来源。
//
// 全部角色系统提示词（meta/domain、8 个固定叶子角色、2 个动态模板）以 Go 常量
// 形式编译进二进制，config/roles.yaml 不再承载提示词文本（用户不可见/不可改）；
// 行为参数（temperature/max_tokens/thinking/model_ref 等）仍在 roles.yaml。
// pkg/config.LoadRoleConfig 启动期经 Get 填充各角色 SystemPrompt/PromptTemplate，
// 未登记的 roleID 报错（strict startup）。
package prompts

import "fmt"

// registry roleID → 内置提示词。键与运行时角色 ID 对齐：
// "meta"/"domain"（MetaAgentConfig/DomainAgentConfig 专用段）、
// fixed_roles[].id、dynamic_templates[].id。
var registry = map[string]string{
	"meta":               MetaAgent,
	"domain":             DomainAgent,
	"scout":              Scout,
	"light":              Light,
	"code_assistant":     CodeAssistant,
	"ui_assistant":       UIAssistant,
	"prompt_reviewer":    PromptReviewer,
	"code_reviewer":      CodeReviewer,
	"test_assistant":     TestAssistant,
	"doc_assistant":      DocAssistant,
	"domain_template":    DomainTemplate,
	"assistant_template": AssistantTemplate,
}

// Get 返回 roleID 的内置提示词（叶子公共纪律段占位符已展开）。
// 未知 roleID 返回错误，由调用方决定 fail-fast（config loader 转为启动失败）。
func Get(roleID string) (string, error) {
	p, ok := registry[roleID]
	if !ok {
		return "", fmt.Errorf("role %q 提示词未内置（请在 pkg/prompts 登记常量）", roleID)
	}
	return expandLeaf(p), nil
}
