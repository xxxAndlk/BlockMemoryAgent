package logger

// LLMCallRecord 是 LLM 调用日志的数据传输对象，用于替代 Logger.LLMCall 的多个位置参数。
type LLMCallRecord struct {
	Agent        string
	Model        string
	Prompt       string
	Response     string
	InputTokens  int
	OutputTokens int
	LatencyMs    int
}
