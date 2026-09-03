package types

// ModelPreset 模型预设：roles.yaml `model_presets[]` 中的一套可切换 LLM 接入参数。
// 设计意图：运行时动态切换角色模型（TUI /api/models 弹窗、Web settings 页）的候选清单。
// 切换只改 ModelFactory 的 override，不回写 roles.yaml；重启后由 model_overrides.yaml
// （role → preset_id）在启动期重新应用。
type ModelPreset struct {
	// ID 预设唯一标识，model_overrides.yaml 以此引用。
	ID string `json:"id" yaml:"id"`
	// Name 人类可读名称（展示用），空则回退 ID。
	Name string `json:"name" yaml:"name"`
	// Provider 模型供应方（openai / anthropic 等，与 AgentModelConfig.Provider 同义）。
	Provider string `json:"provider" yaml:"provider"`
	// Model 具体模型名。
	Model string `json:"model" yaml:"model"`
	// APIKey 调用密钥，支持 ${ENV} 引用（resolveEnvVars 统一展开）。
	APIKey string `json:"api_key,omitempty" yaml:"api_key,omitempty"`
	// BaseURL 自定义端点，支持 ${ENV} 引用。
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Temperature 采样温度；0=未配置（沿用端点默认）。
	Temperature float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	// MaxTokens 单次响应最大 Token 数；0=未配置。
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	// Thinking 思考模式（off/low/medium/high），语义同 AgentModelConfig.Thinking。
	Thinking string `json:"thinking,omitempty" yaml:"thinking,omitempty"`
}

// ToAgentModelConfig 转换为工厂可消费的 AgentModelConfig。
func (p ModelPreset) ToAgentModelConfig() AgentModelConfig {
	return AgentModelConfig{
		Provider:    p.Provider,
		Model:       p.Model,
		APIKey:      p.APIKey,
		BaseURL:     p.BaseURL,
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
		Thinking:    p.Thinking,
	}
}
