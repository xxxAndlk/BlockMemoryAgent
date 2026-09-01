package config

import (
	"fmt"
	"os"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"gopkg.in/yaml.v3"
)

// RoleConfigFile 角色配置文件根结构。
//
// 职责:
//   - 作为 config/roles.yaml 在内存中的表示，承载 MetaAgent / DomainAgent / 固定角色 / 动态模板四类配置。
//   - 提供按 ID/类型查询角色、以及调用权限矩阵(CanCall)等方法。
//
// 副作用: 无; 仅作为数据载体。
type RoleConfigFile struct {
	// MetaAgent 顶层 MetaAgent 配置，全局唯一。
	MetaAgent MetaAgentConfig `yaml:"meta_agent"`
	// DomainAgent 所有 DomainAgent/SubDomainAgent 共用的配置(模型层共享)。
	DomainAgent DomainAgentConfig `yaml:"domain_agent"`
	// LightweightModel 轻量模型配置，用于历史总结/检索 query 改写等低开销任务。
	// 与 MetaAgent/DomainAgent 模型解耦，可指向更便宜更快的模型（如 deepseek-v4-lite）。
	LightweightModel types.AgentModelConfig `yaml:"lightweight_model"`
	// Embed 文本嵌入模型配置（已从 config.yaml 迁移到 roles.yaml）。
	Embed types.EmbedConfig `yaml:"embed"`
	// FixedRoles 固定角色定义列表，对应 fixed_roles 配置项。
	FixedRoles []types.RoleDefinition `yaml:"fixed_roles"`
	// DynamicTemplates 动态角色生成模板，由 LLM 在运行时按需实例化为临时助手。
	DynamicTemplates []DynamicRoleTemplate `yaml:"dynamic_templates"`
}

// MetaAgentConfig MetaAgent 专属配置。
//
// 字段:
//   - ModelConfig:     模型提供商/密钥/温度等。
//   - SystemPrompt:    系统提示词。
//   - Tools:           工具白名单覆盖；为空时用内置默认（call_sub_agent 等编排工具）。
//                      基准单 Agent 模式用它去掉 call_sub_agent、放开执行类工具。
type MetaAgentConfig struct {
	ModelConfig  types.AgentModelConfig `yaml:"model_config"`  // 模型配置(提供商/密钥/温度等)
	SystemPrompt string                 `yaml:"system_prompt"` // MetaAgent 系统提示词
	Tools        []string               `yaml:"tools"`         // 工具白名单覆盖（空=内置默认编排工具集）
}

// DomainAgentConfig DomainAgent 共用配置。
//
// 设计意图: 所有 Domain/SubDomain 共享同一份模型配置，简化部署与调参。
type DomainAgentConfig struct {
	ModelConfig  types.AgentModelConfig `yaml:"model_config"`  // 共享模型配置
	SystemPrompt string                `yaml:"system_prompt"` // DomainAgent 系统提示词（含 skim/WriteSharedMemory/拆分纪律）
}

// DynamicRoleTemplate 动态角色模板。
//
// 由 LLM 在运行时根据当前任务填充 PromptTemplate 实例化出临时角色
// (类型为 domain 或 assistant)，并通过 MaxLifetime 控制其存活时长。
// 可独立配置 ModelConfig；未配置时动态创建的角色回退到 DomainAgent 模型。
type DynamicRoleTemplate struct {
	ID             string                 `yaml:"id"`              // 模板唯一标识
	Name           string                 `yaml:"name"`            // 模板名称(展示用)
	Type           string                 `yaml:"type"`            // "domain" or "assistant"
	PromptTemplate string                 `yaml:"prompt_template"` // 让大模型填充的模板
	Skills         []string               `yaml:"skills"`          // 模板固定持有技能（按 Name 或 SkillID 匹配池，未知项跳过）
	MaxLifetime    int                    `yaml:"max_lifetime"`    // 最大存活时间（秒）
	ModelConfig    types.AgentModelConfig `yaml:"model_config"`    // 可选：动态角色专用模型配置
}

// LoadRoleConfig 从指定路径加载角色配置文件。
//
// 职责:
//   - 读取 YAML 文件并反序列化为 RoleConfigFile。
//   - 解析其中的 ${VAR} / ${VAR:"default"} 环境变量引用。
//   - 为 Embed 等字段填充默认值。
//
// 参数:
//   - path: roles.yaml 文件路径。
//
// 返回:
//   - *RoleConfigFile: 解析后的配置指针。
//   - error: 读文件或反序列化失败时返回包装错误。
//
// 副作用: 无外部状态修改，仅读取文件与环境变量。
func LoadRoleConfig(path string) (*RoleConfigFile, error) {
	// 读取整个配置文件内容到内存。
	data, err := os.ReadFile(path)
	if err != nil {
		// 包装错误便于上层定位是读文件阶段失败。
		return nil, fmt.Errorf("read role config: %w", err)
	}

	// 反序列化 YAML 到结构体变量 cfg。
	var cfg RoleConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// 包装错误便于上层定位是 YAML 解析阶段失败。
		return nil, fmt.Errorf("unmarshal role config: %w", err)
	}

	// 解析配置中的环境变量引用(如 ${OPENAI_API_KEY})。
	cfg.resolveEnvVars()

	// Embed 默认值: provider 为空时回退 pseudo，避免嵌入模块因空 provider 崩溃。
	if cfg.Embed.Provider == "" {
		cfg.Embed.Provider = "pseudo"
	}
	// BatchSize 默认 1，保持与原有逐条嵌入行为一致。
	if cfg.Embed.BatchSize <= 0 {
		cfg.Embed.BatchSize = 1
	}
	// onnx provider（TODO #41）ModelPath 默认 ./models/<Model>/；
	// 仅 provider=onnx 时填充，openai/local/pseudo 忽略该字段。
	if cfg.Embed.Provider == "onnx" && cfg.Embed.ModelPath == "" && cfg.Embed.Model != "" {
		cfg.Embed.ModelPath = "./models/" + cfg.Embed.Model
	}

	// 返回解析并填充默认值后的配置指针。
	return &cfg, nil
}

// resolveEnvVars 原地解析配置中所有模型字段的 ${VAR} 环境变量引用。
//
// 覆盖范围: MetaAgent / DomainAgent / 每个 FixedRoles 的 APIKey 与 BaseURL。
// 副作用: 直接修改接收者内部字段。
func (c *RoleConfigFile) resolveEnvVars() {
	// MetaAgent 模型配置: 密钥与 BaseURL。
	c.MetaAgent.ModelConfig.APIKey = resolveEnv(c.MetaAgent.ModelConfig.APIKey)
	c.MetaAgent.ModelConfig.BaseURL = resolveEnv(c.MetaAgent.ModelConfig.BaseURL)
	// DomainAgent 模型配置: 密钥与 BaseURL。
	c.DomainAgent.ModelConfig.APIKey = resolveEnv(c.DomainAgent.ModelConfig.APIKey)
	c.DomainAgent.ModelConfig.BaseURL = resolveEnv(c.DomainAgent.ModelConfig.BaseURL)
	// 轻量模型配置: 密钥与 BaseURL。
	c.LightweightModel.APIKey = resolveEnv(c.LightweightModel.APIKey)
	c.LightweightModel.BaseURL = resolveEnv(c.LightweightModel.BaseURL)
	// 文本嵌入模型配置: 密钥与 BaseURL。
	c.Embed.APIKey = resolveEnv(c.Embed.APIKey)
	c.Embed.BaseURL = resolveEnv(c.Embed.BaseURL)
	// 逐个固定角色: 密钥与 BaseURL。
	for i := range c.FixedRoles {
		c.FixedRoles[i].ModelConfig.APIKey = resolveEnv(c.FixedRoles[i].ModelConfig.APIKey)
		c.FixedRoles[i].ModelConfig.BaseURL = resolveEnv(c.FixedRoles[i].ModelConfig.BaseURL)
	}
	// 动态角色模板: 密钥与 BaseURL（P3-4）。
	for i := range c.DynamicTemplates {
		c.DynamicTemplates[i].ModelConfig.APIKey = resolveEnv(c.DynamicTemplates[i].ModelConfig.APIKey)
		c.DynamicTemplates[i].ModelConfig.BaseURL = resolveEnv(c.DynamicTemplates[i].ModelConfig.BaseURL)
	}
}

// GetFixedRole 按 ID 查找固定角色定义。
//
// 参数:
//   - roleID: 目标角色定义 ID。
//
// 返回:
//   - 命中返回角色定义指针(指向内部切片元素，调用方不应长期持有)。
//   - 未命中返回 nil。
func (c *RoleConfigFile) GetFixedRole(roleID string) *types.RoleDefinition {
	// 线性扫描 FixedRoles 切片。
	for i := range c.FixedRoles {
		// ID 匹配则返回该元素的指针，避免拷贝整个结构体。
		if c.FixedRoles[i].ID == roleID {
			// 返回元素指针，调用方需注意切片扩容会使该指针失效。
			return &c.FixedRoles[i]
		}
	}
	// 未命中返回 nil，调用方需做 nil 检查。
	return nil
}

// GetFixedRolesByType 按角色类型过滤固定角色。
//
// 参数:
//   - roleType: 目标类型(meta/domain/fixed/dynamic 等)。
//
// 返回: 新切片(可能为空)，不修改内部状态。
func (c *RoleConfigFile) GetFixedRolesByType(roleType enums.RoleType) []types.RoleDefinition {
	// 初始化空结果切片，保持 nil/empty 语义由 append 决定。
	var result []types.RoleDefinition
	// 遍历所有固定角色，按类型匹配筛选。
	for _, r := range c.FixedRoles {
		// 类型匹配则追加到结果。
		if r.Type == roleType {
			result = append(result, r)
		}
	}
	// 返回新切片，原配置不受影响。
	return result
}

// GetDynamicTemplate 按 ID 查找动态角色模板。
//
// 参数:
//   - templateID: 目标模板 ID。
//
// 返回: 命中返回模板指针，未命中返回 nil。
func (c *RoleConfigFile) GetDynamicTemplate(templateID string) *DynamicRoleTemplate {
	// 线性扫描 DynamicTemplates 切片。
	for i := range c.DynamicTemplates {
		// ID 匹配立即返回指针。
		if c.DynamicTemplates[i].ID == templateID {
			return &c.DynamicTemplates[i]
		}
	}
	// 未命中返回 nil。
	return nil
}

// CanCall 调用权限矩阵: 判断 caller 角色能否调用 callee 角色。
//
// 规则:
//  1. caller 必须存在; 否则拒绝。
//  2. caller 为 meta/domain 时，可调用任意 fixed/dynamic 类型助手。
//  3. 否则检查 callee 是否在 caller.Parents 列表(显式声明的可被调用方)。
//
// 参数:
//   - callerRoleDefID: 调用方角色定义 ID。
//   - calleeRoleDefID: 被调用方角色定义 ID。
//
// 返回: true 允许调用; false 拒绝。
func (c *RoleConfigFile) CanCall(callerRoleDefID, calleeRoleDefID string) bool {
	// 取调用方角色定义，若不存在则无法授权，直接拒绝。
	caller := c.GetFixedRole(callerRoleDefID)
	if caller == nil {
		// 调用方不存在，直接拒绝。
		return false
	}
	// 规则2: meta/domain 可调用任意 fixed/dynamic 助手。
	if caller.Type == enums.RoleTypeMeta || caller.Type == enums.RoleTypeDomain {
		// 查询被调用方角色定义。
		callee := c.GetFixedRole(calleeRoleDefID)
		// 被调用方存在且类型为固定或动态助手时允许调用。
		if callee != nil && (callee.Type == enums.RoleTypeFixed || callee.Type == enums.RoleTypeDynamic) {
			return true
		}
	}
	// 规则3: 检查 Parents 显式声明，命中则允许调用。
	for _, parent := range caller.Parents {
		if parent == calleeRoleDefID {
			return true
		}
	}
	// 默认拒绝：未通过任何授权规则。
	return false
}
