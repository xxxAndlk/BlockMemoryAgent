package config

import (
	"fmt"
	"os"

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
//   - MaxBlocks:       单会话最大 SessionBlock 数量，超过将触发压缩或结束。
//   - SummaryInterval: 每 N 步触发一次会话总结，控制上下文膨胀。
type MetaAgentConfig struct {
	ModelConfig     types.AgentModelConfig `yaml:"model_config"`     // 模型配置(提供商/密钥/温度等)
	SystemPrompt    string                 `yaml:"system_prompt"`    // MetaAgent 系统提示词
	MaxBlocks       int                    `yaml:"max_blocks"`       // 单会话最大块数量
	SummaryInterval int                    `yaml:"summary_interval"` // 每N步触发一次会话总结
}

// DomainAgentConfig DomainAgent 共用配置。
//
// 设计意图: 所有 Domain/SubDomain 共享同一份模型配置，简化部署与调参。
type DomainAgentConfig struct {
	ModelConfig types.AgentModelConfig `yaml:"model_config"` // 共享模型配置
}

// DynamicRoleTemplate 动态角色模板。
//
// 由 LLM 在运行时根据当前任务填充 PromptTemplate 实例化出临时角色
// (类型为 domain 或 assistant)，并通过 MaxLifetime 控制其存活时长。
type DynamicRoleTemplate struct {
	ID             string   `yaml:"id"`              // 模板唯一标识
	Name           string   `yaml:"name"`            // 模板名称(展示用)
	Type           string   `yaml:"type"`            // "domain" or "assistant"
	PromptTemplate string   `yaml:"prompt_template"` // 让大模型填充的模板
	Skills         []string `yaml:"skills"`          // 模板预置技能 ID 列表
	MaxLifetime    int      `yaml:"max_lifetime"`    // 最大存活时间（秒）
}

// LoadRoleConfig 从指定路径加载角色配置文件。
//
// 职责:
//   - 读取 YAML 文件并反序列化为 RoleConfigFile。
//   - 解析其中的 ${VAR} / ${VAR:"default"} 环境变量引用。
//   - 为 MaxBlocks/SummaryInterval 等字段填充默认值。
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
	// 读取整个配置文件内容
	data, err := os.ReadFile(path)
	if err != nil {
		// 包装错误便于上层定位
		return nil, fmt.Errorf("read role config: %w", err)
	}

	// 反序列化 YAML 到结构体
	var cfg RoleConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal role config: %w", err)
	}

	// 解析配置中的环境变量引用(如 ${OPENAI_API_KEY})
	cfg.resolveEnvVars()

	// 设置默认值: MaxBlocks 默认 10
	if cfg.MetaAgent.MaxBlocks <= 0 {
		cfg.MetaAgent.MaxBlocks = 10
	}
	// SummaryInterval 默认 5 步
	if cfg.MetaAgent.SummaryInterval <= 0 {
		cfg.MetaAgent.SummaryInterval = 5
	}

	return &cfg, nil
}

// resolveEnvVars 原地解析配置中所有模型字段的 ${VAR} 环境变量引用。
//
// 覆盖范围: MetaAgent / DomainAgent / 每个 FixedRoles 的 APIKey 与 BaseURL。
// 副作用: 直接修改接收者内部字段。
func (c *RoleConfigFile) resolveEnvVars() {
	// MetaAgent 模型配置: 密钥与 BaseURL
	c.MetaAgent.ModelConfig.APIKey = resolveEnv(c.MetaAgent.ModelConfig.APIKey)
	c.MetaAgent.ModelConfig.BaseURL = resolveEnv(c.MetaAgent.ModelConfig.BaseURL)
	// DomainAgent 模型配置: 密钥与 BaseURL
	c.DomainAgent.ModelConfig.APIKey = resolveEnv(c.DomainAgent.ModelConfig.APIKey)
	c.DomainAgent.ModelConfig.BaseURL = resolveEnv(c.DomainAgent.ModelConfig.BaseURL)
	// 轻量模型配置: 密钥与 BaseURL
	c.LightweightModel.APIKey = resolveEnv(c.LightweightModel.APIKey)
	c.LightweightModel.BaseURL = resolveEnv(c.LightweightModel.BaseURL)
	// 逐个固定角色: 密钥与 BaseURL
	for i := range c.FixedRoles {
		c.FixedRoles[i].ModelConfig.APIKey = resolveEnv(c.FixedRoles[i].ModelConfig.APIKey)
		c.FixedRoles[i].ModelConfig.BaseURL = resolveEnv(c.FixedRoles[i].ModelConfig.BaseURL)
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
	// 线性扫描 FixedRoles 切片
	for i := range c.FixedRoles {
		if c.FixedRoles[i].ID == roleID {
			// 返回元素指针，避免拷贝
			return &c.FixedRoles[i]
		}
	}
	return nil
}

// GetFixedRolesByType 按角色类型过滤固定角色。
//
// 参数:
//   - roleType: 目标类型(meta/domain/fixed/dynamic 等)。
//
// 返回: 新切片(可能为空)，不修改内部状态。
func (c *RoleConfigFile) GetFixedRolesByType(roleType types.RoleType) []types.RoleDefinition {
	var result []types.RoleDefinition
	for _, r := range c.FixedRoles {
		// 类型匹配则追加到结果
		if r.Type == roleType {
			result = append(result, r)
		}
	}
	return result
}

// GetDynamicTemplate 按 ID 查找动态角色模板。
//
// 参数:
//   - templateID: 目标模板 ID。
//
// 返回: 命中返回模板指针，未命中返回 nil。
func (c *RoleConfigFile) GetDynamicTemplate(templateID string) *DynamicRoleTemplate {
	for i := range c.DynamicTemplates {
		if c.DynamicTemplates[i].ID == templateID {
			return &c.DynamicTemplates[i]
		}
	}
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
	// 取调用方角色定义
	caller := c.GetFixedRole(callerRoleDefID)
	if caller == nil {
		// 调用方不存在，直接拒绝
		return false
	}
	// 规则2: meta/domain 可调用任意 fixed/dynamic 助手
	if caller.Type == types.RoleTypeMeta || caller.Type == types.RoleTypeDomain {
		callee := c.GetFixedRole(calleeRoleDefID)
		if callee != nil && (callee.Type == types.RoleTypeFixed || callee.Type == types.RoleTypeDynamic) {
			return true
		}
	}
	// 规则3: 检查 Parents 显式声明
	for _, parent := range caller.Parents {
		if parent == calleeRoleDefID {
			return true
		}
	}
	// 默认拒绝
	return false
}
