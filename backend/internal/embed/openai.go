package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/httputil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// OpenAIEmbedder 调用 OpenAI 兼容 embedding 端点获取真实向量。
// 支持 provider=openai（官方端点）和 provider=local（本地兼容服务如 ollama / xinference）。
type OpenAIEmbedder struct {
	cfg    types.EmbedConfig
	dim    int
	client *httputil.RetryableClient
}

// NewOpenAIEmbedder 创建 OpenAI 兼容 embedding 客户端。
//
// 参数：
//   - cfg：Provider 需为 openai/local 之一；Model 必填；APIKey 可空（本地服务常无需密钥）。
//   - dim：期望输出维度，用于校验响应向量长度。
//
// 返回：*OpenAIEmbedder；配置缺失时仍返回实例，错误延迟到 Embed 调用时暴露。
func NewOpenAIEmbedder(cfg types.EmbedConfig, dim int) *OpenAIEmbedder {
	if cfg.Model == "" {
		cfg.Model = "text-embedding-3-small"
	}
	return &OpenAIEmbedder{
		cfg: cfg,
		dim: dim,
		client: httputil.NewRetryableClient(httputil.RetryConfig{
			MaxRetries: 3,
			BaseDelay:  500 * time.Millisecond,
			MaxDelay:   5 * time.Second,
		}),
	}
}

// Embed 调用 OpenAI 兼容 /v1/embeddings 接口，返回向量。
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return make([]float32, e.dim), nil
	}
	baseURL := e.baseURL()
	payload := map[string]any{
		"input": text,
		"model": e.cfg.Model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding endpoint returned %d: %s", resp.StatusCode, truncateBytes(respBody, 200))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embedding response: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding response contains no data")
	}
	vec := result.Data[0].Embedding
	if e.dim > 0 && len(vec) != e.dim {
		return nil, fmt.Errorf("embedding dimension mismatch: got %d, want %d", len(vec), e.dim)
	}
	return vec, nil
}

// Dim 返回向量维度。
func (e *OpenAIEmbedder) Dim() int {
	return e.dim
}

// baseURL 返回 OpenAI 兼容端点基础 URL。
func (e *OpenAIEmbedder) baseURL() string {
	if e.cfg.BaseURL != "" {
		return strings.TrimRight(e.cfg.BaseURL, "/")
	}
	return "https://api.openai.com"
}

// embeddingResponse OpenAI /v1/embeddings 响应结构。
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// truncateBytes 截断字节切片并转换为字符串，用于错误信息。
func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
