package bootstrap

// external_agents.go 接线外部 Agent 注册表（doc/计划_外部Agent调用.md §4.4）：
// 路径推导 + config 条目 → 工具侧注册条目转换。

import (
	"path/filepath"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// externalAgentConfigPath 从主配置路径推导 external_agents.yaml（同目录惯例，plugins.yaml 同款）。
func externalAgentConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "external_agents.yaml")
}

// externalAgentDefsFromConfig 把 config 条目转换为工具侧注册条目（深拷贝 Args 防共享）。
func externalAgentDefsFromConfig(cfg *config.ExternalAgentsConfig) []tool.ExternalAgentDef {
	if cfg == nil || len(cfg.Agents) == 0 {
		return nil
	}
	defs := make([]tool.ExternalAgentDef, 0, len(cfg.Agents))
	for id, e := range cfg.Agents {
		defs = append(defs, tool.ExternalAgentDef{
			ID:          id,
			Enabled:     e.Enabled,
			Command:     e.Command,
			Args:        append([]string(nil), e.Args...),
			TimeoutSec:  e.TimeoutSec,
			Description: e.Description,
		})
	}
	return defs
}
