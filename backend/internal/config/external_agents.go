package config

// external_agents.go 实现外部 Agent CLI 注册表配置（doc/计划_外部Agent调用.md §4.2）：
//   - external_agents.yaml 与 config.yaml 同目录，缺文件返回空配置（零行为变化）；
//   - ${VAR} / ${VAR:default} 环境变量插值复用 plugins.go 的 resolveEnvInValue；
//   - 归一化：空 command 条目丢弃+日志、timeout_sec<=0 回落 1800、
//     args 无 {{task}} 占位符时补占位符为最后一个 argv（工具执行时渲染为任务文本）。

import (
	"fmt"
	"log/slog"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// ExternalAgentDefaultTimeoutSec 单调用默认墙钟上限（秒）；timeout_sec<=0 时回落。
const ExternalAgentDefaultTimeoutSec = 1800

// ExternalAgentTaskPlaceholder 任务占位符：执行时替换为任务文本（独立 argv 元素，不经 shell）。
const ExternalAgentTaskPlaceholder = "{{task}}"

// ExternalAgentsConfig 是外部 Agent 注册表根节点（external_agents.yaml）。
type ExternalAgentsConfig struct {
	// Agents 按注册 ID 索引的外部 Agent 条目。
	Agents map[string]ExternalAgentEntry `yaml:"agents"`
}

// ExternalAgentEntry 单个外部 Agent CLI 条目。
type ExternalAgentEntry struct {
	// Enabled 是否启用；false 条目不进工具描述与执行清单。
	Enabled bool `yaml:"enabled"`
	// Command 可执行命令（PATH 查找）。
	Command string `yaml:"command"`
	// Args 参数模板；{{task}} 占位符在执行时渲染为任务文本。
	Args []string `yaml:"args"`
	// TimeoutSec 单调用墙钟上限（秒）；<=0 回落 ExternalAgentDefaultTimeoutSec。
	TimeoutSec int `yaml:"timeout_sec"`
	// Description 人类可读描述（管理页展示）。
	Description string `yaml:"description"`
}

// LoadExternalAgentsConfig 读取 external_agents.yaml（可选文件）。
// 文件不存在时返回空配置（不报错），使未配置外部 Agent 的部署零改动。
// 返回值已做环境变量插值与归一化（见 normalizeExternalAgents）。
func LoadExternalAgentsConfig(path string) (*ExternalAgentsConfig, error) {
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ExternalAgentsConfig{}, nil
		}
		return nil, fmt.Errorf("read external agents config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse external agents config %s: %w", path, err)
	}
	// 递归插值（unmarshalPlugins 同款三段式）：强类型 struct 无法走 resolveEnvInValue，
	// 先 map 插值再回灌 yaml 二次反序列化。
	resolveEnvInValue(raw)
	merged, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal external agents config: %w", err)
	}
	cfg := &ExternalAgentsConfig{}
	if err := yaml.Unmarshal(merged, cfg); err != nil {
		return nil, fmt.Errorf("parse external agents config: %w", err)
	}
	normalizeExternalAgents(cfg)
	return cfg, nil
}

// normalizeExternalAgents 就地归一化：空 command 丢弃+日志、timeout_sec<=0 回落默认、
// args 无 {{task}} 占位符时补占位符为最后一个 argv（执行时渲染为任务文本，语义同
// 计划书 §4.2「无占位符时任务文本追加为最后一个 argv」，只是把追加动作提前到加载期，
// 使工具侧统一为单一渲染路径）。
func normalizeExternalAgents(cfg *ExternalAgentsConfig) {
	if cfg == nil || len(cfg.Agents) == 0 {
		return
	}
	out := make(map[string]ExternalAgentEntry, len(cfg.Agents))
	for id, e := range cfg.Agents {
		if e.Command == "" {
			slog.Warn("external agent entry dropped: empty command", slog.String("id", id))
			continue
		}
		if e.TimeoutSec <= 0 {
			e.TimeoutSec = ExternalAgentDefaultTimeoutSec
		}
		if !slices.Contains(e.Args, ExternalAgentTaskPlaceholder) {
			e.Args = append(e.Args, ExternalAgentTaskPlaceholder)
		}
		out[id] = e
	}
	cfg.Agents = out
}
