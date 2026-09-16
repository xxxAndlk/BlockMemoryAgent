// Package prompts 内置提示词单一来源。
//
// 全部角色系统提示词（meta/domain、9 个固定叶子角色）以 Go 常量
// 形式编译进二进制，config/roles.yaml 不再承载提示词文本（用户不可见/不可改）；
// 行为参数（temperature/max_tokens/thinking/model_ref 等）仍在 roles.yaml。
// pkg/config.LoadRoleConfig 启动期经 Get 填充各角色 SystemPrompt，
// 未登记的 roleID 报错（strict startup）。
package prompts

import "fmt"

// Version 内置提示词集版本钉（TODO #15 T15）。任何提示词常量的语义性改动
//（措辞微调不算）都应 bump，让启动日志可对照"线上跑的是哪一版提示词"，
// 也方便提示词工程实验（#15 计量）前后归因。bootstrap 启动日志输出该值。
const Version = "20260916-2"

// registry roleID → 内置提示词。键与运行时角色 ID 对齐：
// "meta"/"domain"（MetaAgentConfig/DomainAgentConfig 专用段）、fixed_roles[].id。
// 2026-09-16：chat 角色退役（fast 档改用 doc_assistant），Chat 常量已删。
var registry = map[string]string{
	"meta":            MetaAgent,
	"domain":          DomainAgent,
	"scout":           Scout,
	"light":           Light,
	"code_assistant":  CodeAssistant,
	"ui_assistant":    UIAssistant,
	"prompt_reviewer": PromptReviewer,
	"code_reviewer":   CodeReviewer,
	"test_assistant":  TestAssistant,
	"doc_assistant":   DocAssistant,
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
