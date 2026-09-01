package types

// Skill 结构化技能对象。
//
// 来源三类：skills.yaml 工具别名（ToolRef 绑定已有工具）、
// 主流 Agent 工具约定目录的 SKILL.md（.claude/.codex/.agents/.cursor/.gemini/.agent）、
// 插件 bundle 注入。Skill 内容按渐进披露下发：仅元数据（Name+Description）
// 进系统提示，正文经 load_skill 工具按需获取。
type Skill struct {
	// SkillID 全局唯一标识（小写英文+下划线）。
	SkillID string `json:"skill_id" yaml:"skill_id"`

	// Name 显示名称（SKILL.md frontmatter name；派发/加载按此匹配）。
	Name string `json:"name" yaml:"name"`

	// Description 一句话描述，唯一被注入系统提示的内容。
	Description string `json:"description" yaml:"description"`

	// UsageExample 使用示例（yaml 工具别名 skill 的 few-shot 兜底）。
	UsageExample string `json:"usage_example,omitempty" yaml:"usage_example,omitempty"`

	// Domain 所属领域（多领域用","分隔）。
	Domain string `json:"domain" yaml:"domain"`

	// ToolRef 绑定的工具实现引用（如 "ReadFile"）；SKILL.md 类技能为空。
	ToolRef string `json:"tool_ref" yaml:"tool_ref"`

	// Tags 标签。
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`

	// Cost 估算每次调用的 Token 消耗（可选）。
	Cost int `json:"cost,omitempty" yaml:"cost,omitempty"`

	// Source 技能来源：yaml | dir | bundle | builtin。
	Source string `json:"source,omitempty" yaml:"source,omitempty"`

	// Path SKILL.md 文件绝对路径（dir/bundle 来源），用于 load_skill 附带资源清单。
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Content SKILL.md 正文全文（dir/bundle 来源），load_skill 按需返回。
	Content string `json:"content,omitempty" yaml:"content,omitempty"`
}
