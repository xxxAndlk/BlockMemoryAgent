package config

// plugins.go 实现插件配置加载（设计文档 §5）：
//   - PluginsConfig 对应 plugins.yaml（或 config.yaml 的 plugins 段）根节点；
//   - 复用 config.Load 的同目录深合并惯例：config.yaml 的 plugins 段为基底，
//     同目录 plugins.yaml 深度合并覆盖（plugins.yaml 优先）；
//   - 密钥等敏感值只走 ${VAR} / ${VAR:default} 环境变量插值，不落盘明文。

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PluginsConfig 是插件配置根节点（plugins.yaml）。
type PluginsConfig struct {
	// Plugins 按插件 ID 索引的插件条目。
	Plugins map[string]PluginConfig `yaml:"plugins"`
}

// Enabled 返回插件条目是否启用（缺省 false：安全默认，显式开启）。
func (p PluginConfig) Enabled() bool { return p.EnabledFlag != nil && *p.EnabledFlag }

// PluginConfig 单个插件条目。
type PluginConfig struct {
	// Kind 插件形态：builtin | mcp | bundle | service（bundle 用于按目录整包启停插件包；service 为 Docker 长驻 HTTP 服务）。
	Kind string `yaml:"kind"`
	// EnabledFlag 是否随启动自动 enable（nil = false：安全默认，显式开启）。
	EnabledFlag *bool `yaml:"enabled"`
	// Settings 插件专属配置段（mcp 插件：transport/command/args/env/url/destructive/roles 等）。
	Settings map[string]any `yaml:"settings"`
}

// LoadPluginsConfig 读取 plugins.yaml（可选文件）。
// 文件不存在时返回空配置（不报错），使未配置插件的部署零改动。
// 返回值中所有字符串已做 ${VAR} / ${VAR:default} 环境变量插值。
func LoadPluginsConfig(path string) (*PluginsConfig, error) {
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PluginsConfig{}, nil
		}
		return nil, fmt.Errorf("read plugins config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse plugins config %s: %w", path, err)
	}
	return unmarshalPlugins(raw)
}

// MergePluginsConfig 将 src 深度合并到 dst（dst 为基底）：
// 同 ID 插件的 settings 深度合并，src 的标量字段覆盖 dst；src 无该插件时保留 dst。
// 返回合并结果（不修改入参）。
func MergePluginsConfig(dst, src *PluginsConfig) *PluginsConfig {
	out := &PluginsConfig{Plugins: map[string]PluginConfig{}}
	for id, pc := range dst.Plugins {
		out.Plugins[id] = clonePluginConfig(pc)
	}
	for id, pc := range src.Plugins {
		if base, ok := out.Plugins[id]; ok {
			out.Plugins[id] = mergePluginConfig(base, pc)
		} else {
			out.Plugins[id] = clonePluginConfig(pc)
		}
	}
	return out
}

// unmarshalPlugins 从通用 map 构造 PluginsConfig 并做环境变量插值。
func unmarshalPlugins(raw map[string]any) (*PluginsConfig, error) {
	merged, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal plugins config: %w", err)
	}
	cfg := &PluginsConfig{}
	if err := yaml.Unmarshal(merged, cfg); err != nil {
		return nil, fmt.Errorf("parse plugins config: %w", err)
	}
	// 递归插值：settings/command/args/env 中的 ${VAR} 引用全部替换。
	if cfg.Plugins != nil {
		for id, pc := range cfg.Plugins {
			if pc.Settings != nil {
				pc.Settings = resolveEnvInValue(pc.Settings).(map[string]any)
				cfg.Plugins[id] = pc
			}
		}
	}
	return cfg, nil
}

// resolveEnvInValue 递归替换 map/slice/string 中的 ${VAR} 与 ${VAR:"default"} 引用。
func resolveEnvInValue(v any) any {
	switch t := v.(type) {
	case string:
		return resolveEnvWithDefault(t)
	case map[string]any:
		for k, val := range t {
			t[k] = resolveEnvInValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = resolveEnvInValue(val)
		}
		return t
	}
	return v
}

// resolvePluginsPath 从主配置路径推导 plugins.yaml 路径（同目录）。
func resolvePluginsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "plugins.yaml")
}

// LoadPlugins 按主配置同目录惯例加载插件配置：
// config.yaml 的 plugins 段为基底，同目录 plugins.yaml 深度合并覆盖。
// plugins.yaml 缺失时仅返回 config.yaml 中的 plugins 段（可为空）。
func LoadPlugins(configPath string, base *PluginsConfig) (*PluginsConfig, error) {
	fileCfg, err := LoadPluginsConfig(resolvePluginsPath(configPath))
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = &PluginsConfig{}
	}
	return MergePluginsConfig(base, fileCfg), nil
}

// clonePluginConfig 深拷贝插件条目（settings 是嵌套 map，必须拷贝避免共享可变状态）。
func clonePluginConfig(pc PluginConfig) PluginConfig {
	out := pc
	if pc.Settings != nil {
		out.Settings = cloneMap(pc.Settings)
	}
	return out
}

// mergePluginConfig 深度合并两个插件条目：src 覆盖 dst。
func mergePluginConfig(dst, src PluginConfig) PluginConfig {
	out := dst
	if src.Kind != "" {
		out.Kind = src.Kind
	}
	if src.EnabledFlag != nil {
		out.EnabledFlag = src.EnabledFlag
	}
	out.Settings = mergeMapAny(cloneMap(dst.Settings), src.Settings)
	return out
}

// cloneMap 深拷贝 map[string]any（嵌套 map/slice 一并拷贝）。
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneAny(e)
		}
		return out
	}
	return v
}

// mergeMapAny 深度合并 src 到 dst（同键递归，标量覆盖），返回新 map。
func mergeMapAny(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, sv := range src {
		if dv, ok := dst[k]; ok {
			dm, dOk := dv.(map[string]any)
			sm, sOk := sv.(map[string]any)
			if dOk && sOk {
				dst[k] = mergeMapAny(cloneMap(dm), sm)
				continue
			}
		}
		dst[k] = cloneAny(sv)
	}
	return dst
}
