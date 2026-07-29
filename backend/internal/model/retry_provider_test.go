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
