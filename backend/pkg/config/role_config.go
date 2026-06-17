package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// RoleConfigFile 角色配置文件
type RoleConfigFile struct {
	MetaAgent        MetaAgentConfig           `yaml:"meta_agent"`
	DomainAgent      DomainAgentConfig         `yaml:"domain_agent"`
	FixedRoles       []types.RoleDefinition    `yaml:"fixed_roles"`
	DynamicTemplates []DynamicRoleTemplate     `yaml:"dynamic_templates"`
}

// MetaAgentConfig 主Agent配置
type MetaAgentConfig struct {
	ModelConfig     types.AgentModelConfig `yaml:"model_config"`
	SystemPrompt    string                 `yaml:"system_prompt"`
	MaxBlocks       int                    `yaml:"max_blocks"`
	SummaryInterval int                    `yaml:"summary_interval"` // 每N步触发一次会话总结
}

// DomainAgentConfig 领域Agent配置（所有DomainAgent共用）
type DomainAgentConfig struct {
	ModelConfig types.AgentModelConfig `yaml:"model_config"`
}

// DynamicRoleTemplate 动态角色模板
type DynamicRoleTemplate struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`         // "domain" or "assistant"
	PromptTemplate string `yaml:"prompt_template"` // 让大模型填充的模板
	Skills      []string `yaml:"skills"`
	MaxLifetime int      `yaml:"max_lifetime"` // 最大存活时间（秒）
}

// LoadRoleConfig 从文件加载角色配置
func LoadRoleConfig(path string) (*RoleConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read role config: %w", err)
	}

	var cfg RoleConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal role config: %w", err)
	}

	// 解析环境变量
	cfg.resolveEnvVars()

	// 设置默认值
	if cfg.MetaAgent.MaxBlocks <= 0 {
		cfg.MetaAgent.MaxBlocks = 10
	}
	if cfg.MetaAgent.SummaryInterval <= 0 {
		cfg.MetaAgent.SummaryInterval = 5
	}

	return &cfg, nil
}

func (c *RoleConfigFile) resolveEnvVars() {
	c.MetaAgent.ModelConfig.APIKey = resolveEnv(c.MetaAgent.ModelConfig.APIKey)
	c.MetaAgent.ModelConfig.BaseURL = resolveEnv(c.MetaAgent.ModelConfig.BaseURL)
	c.DomainAgent.ModelConfig.APIKey = resolveEnv(c.DomainAgent.ModelConfig.APIKey)
	c.DomainAgent.ModelConfig.BaseURL = resolveEnv(c.DomainAgent.ModelConfig.BaseURL)
	for i := range c.FixedRoles {
		c.FixedRoles[i].ModelConfig.APIKey = resolveEnv(c.FixedRoles[i].ModelConfig.APIKey)
		c.FixedRoles[i].ModelConfig.BaseURL = resolveEnv(c.FixedRoles[i].ModelConfig.BaseURL)
	}
}

// GetFixedRole 获取固定角色定义
func (c *RoleConfigFile) GetFixedRole(roleID string) *types.RoleDefinition {
	for i := range c.FixedRoles {
		if c.FixedRoles[i].ID == roleID {
			return &c.FixedRoles[i]
		}
	}
	return nil
}

// GetFixedRolesByType 按类型获取固定角色
func (c *RoleConfigFile) GetFixedRolesByType(roleType types.RoleType) []types.RoleDefinition {
	var result []types.RoleDefinition
	for _, r := range c.FixedRoles {
		if r.Type == roleType {
			result = append(result, r)
		}
	}
	return result
}

// GetDynamicTemplate 获取动态角色模板
func (c *RoleConfigFile) GetDynamicTemplate(templateID string) *DynamicRoleTemplate {
	for i := range c.DynamicTemplates {
		if c.DynamicTemplates[i].ID == templateID {
			return &c.DynamicTemplates[i]
		}
	}
	return nil
}

// CanCall 检查角色A是否可以调用角色B
func (c *RoleConfigFile) CanCall(callerRoleDefID, calleeRoleDefID string) bool {
	caller := c.GetFixedRole(callerRoleDefID)
	if caller == nil {
		return false
	}
	// 如果caller是meta或domain，可以调用任何assistant
	if caller.Type == types.RoleTypeMeta || caller.Type == types.RoleTypeDomain {
		callee := c.GetFixedRole(calleeRoleDefID)
		if callee != nil && (callee.Type == types.RoleTypeFixed || callee.Type == types.RoleTypeDynamic) {
			return true
		}
	}
	// 检查Parents列表
	for _, parent := range caller.Parents {
		if parent == calleeRoleDefID {
			return true
		}
	}
	return false
}
