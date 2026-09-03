package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// model_overrides.yaml：运行时动态切换角色模型的持久化覆写文件（role → preset_id）。
// 设计意图：切换模型不回写 roles.yaml（会丢注释与 YAML 锚点），而是落这份轻量
// 覆写文件；启动期在 LoadRoleConfig 之后应用，删除本文件即恢复 roles.yaml 原配置。
// 只存 preset_id 不存解析快照：避免密钥明文复制到第二个文件，且预设参数后续
// 调整（如换 base_url）对已切换角色同样生效。

// ModelOverride 单个角色的模型覆写记录。
type ModelOverride struct {
	// PresetID 应用的预设 ID（roles.yaml model_presets[].id）。
	PresetID string `yaml:"preset_id" json:"preset_id"`
	// AppliedAt 应用时间（记录用，不参与逻辑）。
	AppliedAt *time.Time `yaml:"applied_at,omitempty" json:"applied_at,omitempty"`
}

// ModelOverridesFile model_overrides.yaml 的文件结构。
type ModelOverridesFile struct {
	// Overrides roleID → 覆写记录。roleID 取值与 ModelFactory 缓存键一致：
	// "meta"/"domain"/"lightweight" 或 fixed_roles[].id。
	Overrides map[string]ModelOverride `yaml:"overrides" json:"overrides"`
}

// LoadModelOverrides 从指定路径加载覆写文件。
//
// 文件不存在返回 (nil, nil)（视为无覆写）；YAML 损坏返回 error，
// 由调用方决定警告忽略（覆写属可抛弃状态，不应阻断启动）。
func LoadModelOverrides(path string) (*ModelOverridesFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read model overrides %s: %w", path, err)
	}
	var f ModelOverridesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("unmarshal model overrides %s: %w", path, err)
	}
	return &f, nil
}

// Save 原子性较弱地写入覆写文件（直接覆盖，权限 0600）。
func (f *ModelOverridesFile) Save(path string) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal model overrides: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write model overrides %s: %w", path, err)
	}
	return nil
}
