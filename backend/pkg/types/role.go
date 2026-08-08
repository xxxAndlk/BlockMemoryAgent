package types

import (
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// RoleDefinition 角色定义（配置层面）：来自 roles.yaml 的静态角色模板。
// 描述"角色应当是什么样"，运行时由 ReAct 循环按角色 ID 直接加载使用。
// 该结构体是角色体系的配置侧表达，定义了角色的身份、能力、可调用关系与模型参数。
type RoleDefinition struct {
	// ID 角色定义唯一标识，对应 roles.yaml 中的 id 字段。
	ID string `json:"id" yaml:"id"`
	// Name 人类可读名称，用于日志、TUI 展示与调试。
	Name string `json:"name" yaml:"name"`
	// Type 角色类型，决定实例化路径与生命周期管理策略。
	Type enums.RoleType `json:"type" yaml:"type"`
	// Lifecycle 生命周期策略，控制角色实例的存活时长与回收时机。
	Lifecycle enums.RoleLifecycle `json:"lifecycle" yaml:"lifecycle"`
	// Description 角色职责描述，供 LLM 动态创建角色时参考。
	Description string `json:"description" yaml:"description"`
	// SystemPrompt 系统提示词，执行时注入 Agent 上下文以约束行为。
	SystemPrompt string `json:"system_prompt" yaml:"system_prompt"`
	// Keywords 关键字列表，用于领域匹配、角色筛选与检索召回。
	Keywords []string `json:"keywords" yaml:"keywords"`
	// Skills 技能列表（Skill ID 列表），运行时与 SkillPool 取交集后装配给角色。
	Skills []string `json:"skills" yaml:"skills"`
	// Tools 可用工具 ID 列表，约束该角色可调用的工具集合。
	Tools []string `json:"tools" yaml:"tools"`
	// ModelConfig 模型配置（provider/model/温度等），覆盖或继承全局模型配置。
	ModelConfig AgentModelConfig `json:"model_config" yaml:"model_config"`
	// CanBeCalled 是否可被其他 Agent 调用；false 表示纯调度型角色，不允许被下游调用。
	CanBeCalled bool `json:"can_be_called" yaml:"can_be_called"`
	// Parents 可调用此角色的父角色 ID 列表，显式声明调用关系图。
	Parents []string `json:"parents" yaml:"parents"`
	// Sandbox 角色级写沙箱配置；nil 表示不限制（当前行为：workDir 下任意可写）。
	// 非空 AllowedWritePaths 时，该角色 WriteFile 只能写到这些路径（相对 workDir 解析）下，
	// 用于多 Agent 并发写隔离。dormant opt-in：未配置则零开销跳过。
	Sandbox *RoleSandbox `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
}

// RoleSandbox 角色级写沙箱。AllowedWritePaths 为相对 workDir 的路径前缀列表
// （如 "src/"、artifacts/code/）；空列表等价于不限制。
type RoleSandbox struct {
	// AllowedWritePaths 允许该角色写入的路径前缀列表（相对 workDir，非绝对则 Join workDir）。
	AllowedWritePaths []string `json:"allowed_write_paths" yaml:"allowed_write_paths"`
}

// AgentModelConfig Agent 模型配置：单个角色所需的 LLM 接入参数。
// 用于构造 OpenAI 兼容 SDK 客户端，provider + model 共同决定调用目标。
type AgentModelConfig struct {
	// Provider 模型供应方（如 openai / anthropic / local）。
	Provider string `json:"provider" yaml:"provider"`
	// Model 具体模型名（如 gpt-4o-mini / claude-3-sonnet）。
	Model string `json:"model" yaml:"model"`
	// APIKey 调用密钥；可空则回落到全局环境变量或 provider 默认配置。
	APIKey string `json:"api_key" yaml:"api_key"`
	// BaseURL 自定义 API 端点（兼容 OpenAI 协议的代理或本地服务）。
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Temperature 采样温度，影响输出随机性；0 表示基本确定，越高越随机。
	Temperature float64 `json:"temperature" yaml:"temperature"`
	// MaxTokens 单次响应最大 Token 数，用于控制成本与响应长度。
	MaxTokens int `json:"max_tokens" yaml:"max_tokens"`
}

// SessionBlock 是会话块（DomainAgent 管理）的遗留 API 桥接类型。
// ReAct 重构后内部已不再使用会话块，但 server.Session.State.ActiveBlocks
// 仍对外暴露该形状以保持前端兼容。
type SessionBlock struct {
	// ID 会话块唯一标识。
	ID string `json:"id"`
	// Domain 本块负责的领域名。
	Domain string `json:"domain"`
	// Goal 本块目标描述。
	Goal string `json:"goal"`
}

// ThreeLayerState 是三层图状态机的遗留 API 桥接类型。
// ReAct 重构后内部已不再使用三层状态机，但 server.Session.State 仍对外
// 暴露该 JSON 形状以保持前端兼容。
type ThreeLayerState struct {
	// CurrentDomain 当前活跃领域；由 agent.Session.State 透传。
	CurrentDomain string `json:"current_domain"`
	// ActiveBlocks 当前活跃的会话块列表（仅保留兼容字段）。
	ActiveBlocks map[string]*SessionBlock `json:"active_blocks"`
	// PendingClarify 待处理的人机对话请求；非空表示会话等待用户答复。
	PendingClarify *ClarifyRequest `json:"pending_clarify,omitempty"`
}
