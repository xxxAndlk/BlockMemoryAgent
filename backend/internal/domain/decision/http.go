package decision

// http.go 外部 HTTP provider（TypeSafe Jev 兼容面，默认关）。
// 立场：学范式不接死外部 API——provider 可插拔，默认走 LLM 兜底；
// 外部服务仅在 config.yaml decision: 节显式配置时启用，失败自动回退 LLM 兜底。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPProvider 调外部决策服务（POST JSON：Request 形态 → Response 形态）。
// TypeSafe 兼容口径：answers[].key/value/score/confidence 字段名一致；
// 服务侧不认识的字段忽略，缺 key 的答案按无效丢弃（与 LLM provider 同一校验）。
type HTTPProvider struct {
	URL     string
	APIKey  string
	Timeout time.Duration
	Client  *http.Client
	// Fallback 外部失败时的兜底（通常挂 LLMProvider）；nil 则错误上抛。
	Fallback Provider
}

// NewHTTPProvider 构造。url 为空时报错（调用方应只在显式配置后构造）。
func NewHTTPProvider(url, apiKey string, timeout time.Duration, fallback Provider) *HTTPProvider {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &HTTPProvider{
		URL:      url,
		APIKey:   apiKey,
		Timeout:  timeout,
		Client:   &http.Client{Timeout: timeout},
		Fallback: fallback,
	}
}

// Decide POST 决策请求；网络/解析失败回退 Fallback（LLM 兜底），
// 双失败才上抛错误——调用方 fail-open 回退现状行为。
func (p *HTTPProvider) Decide(ctx context.Context, req Request) (Response, error) {
	if p == nil || p.URL == "" {
		return Response{}, fmt.Errorf("decision http provider: url not configured")
	}
	resp, err := p.post(ctx, req)
	if err == nil {
		return resp, nil
	}
	if p.Fallback != nil {
		return p.Fallback.Decide(ctx, req)
	}
	return Response{}, err
}

// post 单次 HTTP 往返。响应按 LLM 同一校验口径过滤无效答案。
func (p *HTTPProvider) post(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("decision http marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("decision http build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	httpResp, err := p.Client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("decision http do: %w", err)
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("decision http read: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("decision http status %d: %s", httpResp.StatusCode, compactRunes(string(raw), 200))
	}
	var out decisionLLMOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("decision http parse: %w", err)
	}
	return filterHTTPAnswers(out, req), nil
}

// filterHTTPAnswers 按 Key 对齐 + 原语校验（与 parseDecisionResponse 同一防幻觉口径）。
// 外部服务可信度高于 LLM 但仍不放松候选闭集——结构性防线不看来源。
func filterHTTPAnswers(out decisionLLMOutput, req Request) Response {
	byKey := make(map[string]int, len(out.Answers))
	for i, a := range out.Answers {
		byKey[a.Key] = i
	}
	var resp Response
	for _, q := range req.Questions {
		i, ok := byKey[q.Key]
		if !ok {
			continue
		}
		raw := out.Answers[i]
		ans := Answer{
			Key:        q.Key,
			Point:      q.Point,
			Primitive:  q.Primitive,
			Value:      raw.Value,
			Score:      clamp01(raw.Score),
			Confidence: clamp01(raw.Confidence),
		}
		if !answerValid(q, ans) {
			continue
		}
		resp.Answers = append(resp.Answers, ans)
	}
	return resp
}
