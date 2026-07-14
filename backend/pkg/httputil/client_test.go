package httputil

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetryableClient_RetriesThenSucceeds 验证重试客户端在服务器连续返回
// 503 时能够按配置重试，并在第三次请求成功后返回 200。
func TestRetryableClient_RetriesThenSucceeds(t *testing.T) {
	// count 记录服务器被请求次数，使用原子操作保证并发安全。
	var count int32
	// 启动一个 httptest 服务器，前两次返回 503，第三次返回 200。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 读取请求体并断言内容，确保重试时请求体被正确复用。
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello" {
			t.Fatalf("unexpected body %q", body)
		}
		// 原子递增请求计数；小于 3 时返回 503 触发重试。
		if atomic.AddInt32(&count, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// 第三次请求返回 200 与响应体。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	// 测试结束后关闭服务器。
	defer server.Close()

	// 构造重试客户端，最大重试 3 次、初始退避 10ms、最大退避 50ms。
	client := NewRetryableClient(RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	})

	// 构造 POST 请求，请求体为 "hello"。
	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	// 执行请求，应自动重试并最终成功。
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 关闭响应体，释放连接。
	defer resp.Body.Close()

	// 断言最终状态码为 200。
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	// 断言服务器恰好收到 3 次请求（1 次初始 + 2 次重试）。
	if atomic.LoadInt32(&count) != 3 {
		t.Fatalf("expected 3 attempts, got %d", count)
	}
}

// TestRetryableClient_RespectsCancel 验证重试客户端会尊重请求的上下文取消信号，
// 在上下文超时时不再继续重试并返回错误。
func TestRetryableClient_RespectsCancel(t *testing.T) {
	// 启动一个慢速服务器，每次响应耗时 100ms，确保触发客户端超时。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	// 测试结束后关闭服务器。
	defer server.Close()

	// 构造重试客户端。
	client := NewRetryableClient(RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	})

	// 创建一个 30ms 后自动超时的上下文。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	// 测试结束后释放上下文资源。
	defer cancel()

	// 构造带上下文的 GET 请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	// 执行请求，预期因上下文超时而失败。
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	// 断言上下文本身已报错（Canceled / DeadlineExceeded）。
	if ctx.Err() == nil {
		t.Fatalf("expected context error, got %v", err)
	}
}
