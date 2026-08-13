package model

// retry_provider_test.go 验证 retryProvider 的重试行为：
// - 瞬时错误重试到成功
// - 全部失败返回最后一次错误
// - ctx.Canceled 不重试
// - 空响应视为失败触发重试

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-kratos/blades"
)

// fakeProvider 是可编程的 blades.ModelProvider，按预设队列返回响应或错误。
type fakeProvider struct {
	responses []*blades.ModelResponse // 成功响应队列
	errors    []error                  // 错误队列，按 calls 顺序消费
	calls     int                      // 调用计数
	name      string                   // provider 名称
}

// Generate 按调用次数返回预设的响应或错误。
func (p *fakeProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.calls++
	if p.calls <= len(p.errors) && p.errors[p.calls-1] != nil {
		return nil, p.errors[p.calls-1]
	}
	if p.calls <= len(p.responses) {
		return p.responses[p.calls-1], nil
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("default")}, nil
}

// Name 返回 provider 名称。
func (p *fakeProvider) Name() string { return p.name }

// NewStreaming 满足 blades.ModelProvider 接口，测试中不使用流式路径，返回错误。
func (p *fakeProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		yield(nil, errors.New("streaming not supported in fakeProvider"))
	}
}

// TestRetryProvider_TransientErrorRetried 验证瞬时错误重试后成功。
func TestRetryProvider_TransientErrorRetried(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			errors.New("503 service unavailable"),
			errors.New("429 rate limited"),
			nil, // 第三次成功
		},
		responses: []*blades.ModelResponse{
			nil, nil,
			{Message: blades.AssistantMessage("ok")},
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err != nil {
		t.Fatalf("expected success after retries, got err: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected non-nil response")
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 calls (2 retries + success), got %d", inner.calls)
	}
}

// TestRetryProvider_AllFailuresReturned 验证全部失败后返回错误且包含重试次数信息。
func TestRetryProvider_AllFailuresReturned(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			errors.New("503 service unavailable"),
			errors.New("502 bad gateway"),
			errors.New("500 internal server error"),
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if resp != nil {
		t.Fatal("expected nil response on failure")
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 calls, got %d", inner.calls)
	}
	if !strings.Contains(err.Error(), "after 3 retries") {
		t.Fatalf("error should mention retry count, got: %v", err)
	}
	if !strings.Contains(err.Error(), "500 internal server error") {
		t.Fatalf("error should wrap last failure, got: %v", err)
	}
}

// TestRetryProvider_CtxCanceledNotRetried 验证 ctx 取消时不重试。
func TestRetryProvider_CtxCanceledNotRetried(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			context.Canceled,
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 提前取消
	_, err := p.Generate(ctx, &blades.ModelRequest{})
	if err == nil {
		t.Fatal("expected error on canceled ctx")
	}
	// ctx 已取消时首次调用前就返回，calls 应为 0
	if inner.calls != 0 {
		t.Fatalf("expected 0 calls on canceled ctx, got %d", inner.calls)
	}
}

// TestRetryProvider_EmptyResponseRetried 验证空响应（err=nil 但 resp=nil）触发重试。
func TestRetryProvider_EmptyResponseRetried(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			nil, // 第一次：err=nil 但 resp=nil（空响应）
			nil, // 第二次：同上
			nil, // 第三次：同上
		},
		responses: []*blades.ModelResponse{
			{Message: nil}, // resp 非 nil 但 Message=nil
			nil,            // resp=nil
			{Message: blades.AssistantMessage("ok")},
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err != nil {
		t.Fatalf("expected success after retrying empty responses, got: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected non-nil response with message")
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 calls, got %d", inner.calls)
	}
}

// TestRetryProvider_SuccessFirstTry 验证首次成功不重试。
func TestRetryProvider_SuccessFirstTry(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		responses: []*blades.ModelResponse{
			{Message: blades.AssistantMessage("ok")},
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected non-nil response")
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 call (no retry on success), got %d", inner.calls)
	}
}

// TestRetryProvider_BackoffDoesntBlockForever 验证重试退避总时长有界，
// 不会因过长的退避阻塞主循环。
func TestRetryProvider_BackoffDoesntBlockForever(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			errors.New("503"),
			errors.New("503"),
			errors.New("503"),
		},
	}
	// 使用短退避避免测试等待过久
	orig := providerRetryInitialBackoff
	providerRetryInitialBackoff = time.Millisecond
	defer func() { providerRetryInitialBackoff = orig }()

	p := &retryProvider{inner: inner, name: "fake"}
	start := time.Now()
	_, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	dur := time.Since(start)
	// 退避：1ms + 2ms = 3ms（第 3 次失败不退避），总时长应 < 100ms
	if dur > 100*time.Millisecond {
		t.Fatalf("retry backoff too long: %v", dur)
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 calls, got %d", inner.calls)
	}
}

// TestWrapWithRetry_NilInner 验证 nil inner 返回 nil。
func TestWrapWithRetry_NilInner(t *testing.T) {
	if got := wrapWithRetry(nil, "test"); got != nil {
		t.Fatalf("expected nil for nil inner, got %v", got)
	}
}

// TestWrapWithRetry_WrapsProvider 验证 wrapWithRetry 返回 retryProvider 实例。
func TestWrapWithRetry_WrapsProvider(t *testing.T) {
	inner := &fakeProvider{name: "fake"}
	wrapped := wrapWithRetry(inner, "fake")
	if wrapped == nil {
		t.Fatal("expected non-nil wrapped provider")
	}
	rp, ok := wrapped.(*retryProvider)
	if !ok {
		t.Fatal("expected *retryProvider type")
	}
	if rp.name != "fake" {
		t.Fatalf("expected name 'fake', got %q", rp.name)
	}
}


// TestRetryProvider_ClientErrorNotRetried 验证 4xx 客户端错误（请求本身非法）不重试：
// 实证 domain 的 400 配对错误被完整重发 3 次才放弃，白等 3 倍拒绝耗时。
func TestRetryProvider_ClientErrorNotRetried(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			errors.New(`POST "https://api.example.com/v1/messages": 400 Bad Request {"error":{"type":"invalid_request_error","message":"tool_call_ids did not have response messages"}}`),
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	_, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if inner.calls != 1 {
		t.Fatalf("4xx 应快速失败不重试，got %d calls", inner.calls)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("应透传原始 4xx 错误，got: %v", err)
	}
}

// TestIsNonRetryableErr 验证 4xx 标记判定：4xx（除 408/429）不可重试，5xx/429/网络错误可重试。
func TestIsNonRetryableErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`POST "https://x/v1/messages": 400 Bad Request {"error":{"type":"invalid_request_error"}}`, true},
		{"401 Unauthorized", false}, // 无空格边界不匹配 " 401 "，按可重试处理（保守）
		{" 401 ", true},
		{" 403 ", true},
		{" 404 ", true},
		{" 422 ", true},
		{"invalid_request_error: bad tool use", true},
		{" 408 request timeout", false},
		{" 429 rate limited", false},
		{" 500 internal error", false},
		{" 503 service unavailable", false},
		{"connection refused", false},
	}
	for _, c := range cases {
		if got := isNonRetryableErr(errors.New(c.msg)); got != c.want {
			t.Errorf("isNonRetryableErr(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	if isNonRetryableErr(nil) {
		t.Error("nil error 应判定为可重试（false）")
	}
}

// clampFakeProvider 在 fakeProvider 基础上实现 maxTokensClamper：
// ClampMaxTokensOnError 恒返回 true（模拟 max_tokens 超限已钳制），
// NewStreaming 前 streamFails 次产出错误流，之后产出成功流。
type clampFakeProvider struct {
	fakeProvider
	clamped     bool
	streamFails int
}

// ClampMaxTokensOnError 记录调用并声称修复成功。
func (p *clampFakeProvider) ClampMaxTokensOnError(err error) bool {
	p.clamped = true
	return true
}

// NewStreaming 可编程流：前 streamFails 次产出 400 错误，之后产出成功响应。
func (p *clampFakeProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		if p.streamFails > 0 {
			p.streamFails--
			yield(nil, errors.New(`POST "https://ark.cn-beijing.volces.com/api/coding/v1/messages": 400 Bad Request {"error":{"code":"InvalidParameter","message":"The parameter `+"`max_tokens`"+` specified in the request is not valid: integer above maximum value, expected a value <= 32768, but got 65536 instead."}}`))
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("ok")}, nil)
	}
}

// TestRetryProvider_MaxTokensClampRetried 验证 4xx 中 max_tokens 超限被 provider 自愈后
// 继续重试而非快速失败（2026-08-13 代码助手 65536>32768 全挂的根因场景）。
func TestRetryProvider_MaxTokensClampRetried(t *testing.T) {
	inner := &clampFakeProvider{fakeProvider: fakeProvider{
		name: "fake",
		errors: []error{
			errors.New(`POST "https://ark.cn-beijing.volces.com/api/coding/v1/messages": 400 Bad Request {"error":{"code":"InvalidParameter","message":"The parameter ` + "`max_tokens`" + ` specified in the request is not valid: integer above maximum value, expected a value <= 32768, but got 65536 instead."}}`),
			nil,
		},
		responses: []*blades.ModelResponse{
			nil,
			{Message: blades.AssistantMessage("ok")},
		},
	}}
	orig := providerRetryInitialBackoff
	providerRetryInitialBackoff = time.Millisecond
	defer func() { providerRetryInitialBackoff = orig }()

	p := &retryProvider{inner: inner, name: "fake"}
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err != nil {
		t.Fatalf("钳制后重试应成功，got err: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected non-nil response")
	}
	if !inner.clamped {
		t.Fatal("应调用 ClampMaxTokensOnError 自愈")
	}
	if inner.calls != 2 {
		t.Fatalf("钳制后应重试一次成功，got %d calls", inner.calls)
	}
}

// TestRetryProvider_MaxTokensClampStreaming 验证流式路径同样自愈：
// 首错（未产出增量）时钳制并重试，成功后正常产出增量。
func TestRetryProvider_MaxTokensClampStreaming(t *testing.T) {
	inner := &clampFakeProvider{fakeProvider: fakeProvider{name: "fake"}, streamFails: 1}
	orig := providerRetryInitialBackoff
	providerRetryInitialBackoff = time.Millisecond
	defer func() { providerRetryInitialBackoff = orig }()

	p := &retryProvider{inner: inner, name: "fake"}
	got := 0
	for resp, err := range p.NewStreaming(context.Background(), &blades.ModelRequest{}) {
		if err != nil {
			t.Fatalf("钳制后流式重试应成功，got err: %v", err)
		}
		if resp != nil && resp.Message != nil {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("应产出 1 个成功响应，got %d", got)
	}
	if !inner.clamped {
		t.Fatal("应调用 ClampMaxTokensOnError 自愈")
	}
}

// TestRetryProvider_ClientErrorWithoutClampNotRetried 验证未实现自愈接口的 provider
// 保持原有 4xx 快速失败行为不变。
func TestRetryProvider_ClientErrorWithoutClampNotRetried(t *testing.T) {
	inner := &fakeProvider{
		name: "fake",
		errors: []error{
			errors.New(`POST "https://ark.cn-beijing.volces.com/api/coding/v1/messages": 400 Bad Request {"error":{"code":"InvalidParameter","message":"The parameter ` + "`max_tokens`" + ` specified in the request is not valid: integer above maximum value, expected a value <= 32768, but got 65536 instead."}}`),
		},
	}
	p := &retryProvider{inner: inner, name: "fake"}
	_, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if inner.calls != 1 {
		t.Fatalf("无自愈能力时应快速失败不重试，got %d calls", inner.calls)
	}
}
