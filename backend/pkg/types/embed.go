package types

// EmbedConfig 文本嵌入模型配置（P3-3）。
// 支持 pseudo（字符哈希，默认）、openai（OpenAI 兼容端点）、local（本地兼容服务）三种 provider。
// 向量维度需与 pgvector.dimensions 一致；provider=pseudo 时忽略 API 相关字段。
type EmbedConfig struct {
	// Provider 嵌入提供方：pseudo | openai | local
	Provider string `json:"provider" yaml:"provider"`
	// Model 模型名，如 text-embedding-3-small、bge-m3、doubao-embedding
	Model string `json:"model" yaml:"model"`
	// APIKey API 密钥（openai/local 需要）
	APIKey string `json:"api_key" yaml:"api_key"`
	// BaseURL 自定义端点（OpenAI 兼容格式）
	BaseURL string `json:"base_url" yaml:"base_url"`
	// BatchSize 单次批量嵌入条数上限；<=0 时按实现默认值
	BatchSize int `json:"batch_size" yaml:"batch_size"`
}
