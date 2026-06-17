package types

import "time"

// Skill 结构化技能对象（v3 §5.1）
//
// 每个 Skill 是一个独立的能力单元，可被任意 DomainAgent / SubDomainAgent
// 在初始化时按领域筛选并装配，避免将全部 Skill 直接暴露给子 Agent
// 造成"上下文噪音"。
type Skill struct {
	// SkillID 全局唯一标识（小写英文+下划线）
	SkillID string `json:"skill_id" yaml:"skill_id"`

	// Name 显示名称
	Name string `json:"name" yaml:"name"`

	// Description 一句话描述（≤30 字），仅这一句话被注入子 Agent 上下文
	Description string `json:"description" yaml:"description"`

	// UsageExample 使用示例（仅在 LLM 选择失败时作为 Few-Shot）
	UsageExample string `json:"usage_example,omitempty" yaml:"usage_example,omitempty"`

	// Domain 所属领域，用于按领域筛选（多领域用","分隔）
	Domain string `json:"domain" yaml:"domain"`

	// ToolRef 绑定的工具实现引用（如 "ReadFile" / "mcp_firecrawl"）
	ToolRef string `json:"tool_ref" yaml:"tool_ref"`

	// Tags 标签，辅助 LLM 与关键字检索
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`

	// Cost 估算每次调用的 Token 消耗（用于预算分配，可选）
	Cost int `json:"cost,omitempty" yaml:"cost,omitempty"`
}

// SkillSet 一次任务装配给某个 DomainAgent 的 Skill 子集
type SkillSet struct {
	OwnerAgent string    `json:"owner_agent"` // 持有者 Agent 实例 ID
	Domain     string    `json:"domain"`
	Skills     []*Skill  `json:"skills"`
	CreatedAt  time.Time `json:"created_at"`
}

// PromptList 输出供 LLM 决策时使用的 "Skill 名:一句话" 列表
//
// 严格遵循 v3 §5.3：LLM 看到的只是 ID + Description，不会被
// 完整定义淹没。
func (s *SkillSet) PromptList() string {
	if s == nil || len(s.Skills) == 0 {
		return "(无可用 Skill)"
	}
	out := ""
	for _, sk := range s.Skills {
		out += "- " + sk.SkillID + ": " + sk.Description + "\n"
	}
	return out
}
