package types

// ModelEntry 模型注册表条目：config/models.json `models[]` 中的一套 LLM 连接参数
// （model/api_key/base_url/provider）。设计意图：连接参数与角色解耦——roles.yaml 角色
// 以 model_ref 引用条目，换 key/换端点只改本文件，免重启热更新（mtime 检查）；
// TUI /model 与 Web 选择器的新增模型也落盘到这里。
// 只存连接参数不存行为参数：temperature/max_tokens/thinking 属角色（roles.yaml），
// 思考强度覆盖存 role_bindings[].thinking。
type ModelEntry struct {
	// ID 唯一标识，model_ref 与 role_bindings 以此引用。
	ID string `json:"id"`
	// Name 人类可读名称（展示用），空则回退 ID。
	Name string `json:"name,omitempty"`
	// Provider 模型供应方（openai-chat / openai-responses / anthropic / ollama 等）。
	Provider string `json:"provider"`
	// Model 具体模型名，支持 ${ENV} 引用（加载期展开）。
	Model string `json:"model"`
	// APIKey 调用密钥，支持 ${ENV} 引用；允许为空（切换到该条目时拒绝并提示）。
	APIKey string `json:"api_key,omitempty"`
	// BaseURL 自定义端点，支持 ${ENV} 引用。
	BaseURL string `json:"base_url,omitempty"`
}

// DisplayName 展示名：Name 为空回退 ID。
func (e ModelEntry) DisplayName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}
