package embed

import (
	"bytes"         // 请求体构造
	"context"       // 上下文传递
	"encoding/json" // JSON 序列化/反序列化
	"fmt"           // 错误格式化
	"io"            // 响应体读取
	"net/http"      // HTTP 方法常量
	"strings"       // 字符串处理
	"time"          // 超时与退避

	"github.com/blockmemory/agent/backend/pkg/httputil" // 可重试 HTTP 客户端
	"github.com/blockmemory/agent/backend/pkg/types"    // EmbedConfig 配置类型
)

// OpenAIEmbedder 调用 OpenAI 兼容 embedding 端点获取真实向量。
// 支持 provider=openai（官方端点）和 provider=local（本地兼容服务如 ollama / xinference）。
type OpenAIEmbedder struct {
	cfg    types.EmbedConfig         // embedding 配置
	dim    int                       // 期望输出维度
	client *httputil.RetryableClient // 可重试 HTTP 客户端
}

// NewOpenAIEmbedder 创建 OpenAI 兼容 embedding 客户端。
//
// 参数：
//   - cfg：Provider 需为 openai/local 之一；Model 必填；APIKey 可空（本地服务常无需密钥）。
//   - dim：期望输出维度，用于校验响应向量长度。
//
// 返回：*OpenAIEmbedder；配置缺失时仍返回实例，错误延迟到 Embed 调用时暴露。
func NewOpenAIEmbedder(cfg types.EmbedConfig, dim int) *OpenAIEmbedder {
	// 若未配置模型，使用 OpenAI 默认 embedding 模型
	if cfg.Model == "" {
		cfg.Model = "text-embedding-3-small"
	}
	// 返回封装后的 embedder
	return &OpenAIEmbedder{
		cfg: cfg,
		dim: dim,
		// 构造可重试 HTTP 客户端：最多 3 次，退避 500ms~5s
		client: httputil.NewRetryableClient(httputil.RetryConfig{
			MaxRetries: 3,
			BaseDelay:  500 * time.Millisecond,
			MaxDelay:   5 * time.Second,
		}),
	}
}

// Embed 调用 OpenAI 兼容 /v1/embeddings 接口，返回向量。
//
// 参数：
//   - ctx: 上下文
//   - text: 待嵌入文本
//
// 返回：
//   - []float32: 嵌入向量
//   - error: 请求/解析/维度校验错误
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// 空文本或仅空白文本直接返回全零向量，避免无意义请求
	if strings.TrimSpace(text) == "" {
		return make([]float32, e.dim), nil
	}
	// 获取基础 URL
	baseURL := e.baseURL()
	// 构造请求 payload
	payload := map[string]any{
		"input": text,
		"model": e.cfg.Model,
	}
	// 序列化为 JSON
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}
	// 构造 POST 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	// 若配置了 API Key，则添加 Authorization 头
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	// 发送请求（内部自动重试）
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	// 函数退出时关闭响应体
	defer resp.Body.Close()

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}
	// 校验 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding endpoint returned %d: %s", resp.StatusCode, truncateBytes(respBody, 200))
	}

	// 解析 JSON 响应
	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embedding response: %w", err)
	}
	// 校验响应中包含 data
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding response contains no data")
	}
	// 取第一条向量
	vec := result.Data[0].Embedding
	// 若指定了维度，则校验向量长度
	if e.dim > 0 && len(vec) != e.dim {
		return nil, fmt.Errorf("embedding dimension mismatch: got %d, want %d", len(vec), e.dim)
	}
	return vec, nil
}

// Dim 返回向量维度。
//
// 返回：int，期望输出维度。
func (e *OpenAIEmbedder) Dim() int {
	return e.dim
}

// baseURL 返回 OpenAI 兼容端点基础 URL。
//
// 返回：去除末尾斜杠后的 BaseURL，未配置则返回官方端点。
func (e *OpenAIEmbedder) baseURL() string {
	// 若配置了 BaseURL，去除末尾斜杠后返回
	if e.cfg.BaseURL != "" {
		return strings.TrimRight(e.cfg.BaseURL, "/")
	}
	// 未配置则使用 OpenAI 官方端点
	return "https://api.openai.com"
}

// embeddingResponse OpenAI /v1/embeddings 响应结构。
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"` // 嵌入向量
		Index     int       `json:"index"`     // 数据索引
	} `json:"data"`
}

// truncateBytes 截断字节切片并转换为字符串，用于错误信息。
//
// 参数：
//   - b: 原始字节切片
//   - max: 最大保留长度
//
// 返回：截断后的字符串。
func truncateBytes(b []byte, max int) string {
	// 长度未超过限制时直接转换
	if len(b) <= max {
		return string(b)
	}
	// 超过限制时截取前 max 字节并附加截断标记
	return string(b[:max]) + "...(truncated)"
}
