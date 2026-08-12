package types

// EmbedConfig 文本嵌入模型配置（P3-3）。
// 支持 pseudo（字符哈希，默认）、openai（OpenAI 兼容端点）、local（本地兼容服务）、
// onnx（进程内 ONNX Runtime 推理，TODO #41）四种 provider。
// 向量维度需与 pgvector.dimensions 一致；provider=pseudo 时忽略 API 相关字段。
type EmbedConfig struct {
	// Provider 嵌入提供方：pseudo | openai | local | onnx。
	// pseudo 使用字符哈希生成伪向量，用于离线测试；openai/local 调用外部嵌入服务；
	// onnx 进程内加载 ONNX 模型权重推理（零 api_key、零外部服务，需 -tags onnx 构建）。
	Provider string `json:"provider" yaml:"provider"`
	// Model 模型名，如 text-embedding-3-small、bge-m3、doubao-embedding、bge-base-zh-v1.5。
	Model string `json:"model" yaml:"model"`
	// APIKey API 密钥（openai/local 需要；pseudo/onnx 忽略）。
	APIKey string `json:"api_key" yaml:"api_key"`
	// BaseURL 自定义端点（OpenAI 兼容格式），用于代理或私有化部署。
	BaseURL string `json:"base_url" yaml:"base_url"`
	// BatchSize 单次批量嵌入条数上限；<=0 时按实现默认值处理。
	BatchSize int `json:"batch_size" yaml:"batch_size"`
	// ModelPath ONNX 模型目录（provider=onnx 专用，TODO #41）：含 model.onnx +
	// tokenizer.json + tokenizer_config.json + vocab.txt；openai/local/pseudo 忽略。
	// 空时用默认 ./models/<Model>/。
	ModelPath string `json:"model_path" yaml:"model_path"`
}
