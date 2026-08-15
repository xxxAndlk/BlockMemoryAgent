package logger

// LLMCallRecord 是 LLM 调用日志的数据传输对象，用于替代 Logger.LLMCall 的多个位置参数。
type LLMCallRecord struct {
	Agent           string // 发起调用的 Agent 标识
	Model           string // 使用的模型名称
	Prompt          string // 发送给 LLM 的 prompt 全文
	Response        string // LLM 返回的响应全文
	InputTokens     int    // 输入 token 数
	OutputTokens    int    // 输出 token 数
	CacheHitTokens  int    // 缓存命中 token 数（TODO #40 可观测；provider 按端点语义映射）
	CacheMissTokens int    // 缓存未命中 token 数（TODO #40 可观测）
	LatencyMs       int    // 调用耗时（毫秒）
	Meta            map[string]any // 扩展元数据（如 layer=lightweight 区分轻量调用），写入 session_logs.meta
}
