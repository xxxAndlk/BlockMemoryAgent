package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeLLM 实现 LLMClient，用于测试。
type fakeLLM struct {
	resp string
	err  error
}

func (f *fakeLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return f.resp, f.err
}

// flakyLLM 前 failN 次返回 err，之后返回 resp。用于验证重试。
type flakyLLM struct {
	resp   string
	failN  int
	calls  int
	mu     sync.Mutex
}

func (f *flakyLLM) Generate(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.failN {
		return "", fmt.Errorf("transient error #%d", n)
	}
	return f.resp, nil
}

func TestCallWithTimeoutInvokesRecordCallback(t *testing.T) {
	tracker := NewLLMCallTracker()

	var mu sync.Mutex
	var got CallRecord
	called := make(chan struct{}, 1)
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {
		mu.Lock()
		got = r
		mu.Unlock()
		called <- struct{}{}
	})

	llm := &fakeLLM{resp: "hello"}
	ctx := context.Background()
	resp, err, timedOut := tracker.CallWithTimeout(ctx, llm, "prompt text", "MetaAgent", 5*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("CallWithTimeout 不应失败: %v", err)
	}
	if timedOut {
		t.Fatal("不应超时")
	}
	if resp != "hello" {
		t.Fatalf("响应应为 hello，got %s", resp)
	}

	select {
	case <-called:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("record callback 未被调用")
	}

	mu.Lock()
	defer mu.Unlock()
	if got.Caller != "MetaAgent" {
		t.Fatalf("caller 应为 MetaAgent，got %s", got.Caller)
	}
	if got.Prompt != "prompt text" {
		t.Fatalf("prompt 应完整保留，got %s", got.Prompt)
	}
	if got.Response != "hello" {
		t.Fatalf("response 应为 hello，got %s", got.Response)
	}
}

// TestCallWithTimeoutRetriesThenSucceeds 验证 P0-1：前两次失败第三次成功，
// 最终返回成功响应，且仅产生一条 RecordCall 记录、回调触发一次。
func TestCallWithTimeoutRetriesThenSucceeds(t *testing.T) {
	tracker := NewLLMCallTracker()
	var mu sync.Mutex
	var records []CallRecord
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {
		mu.Lock()
		records = append(records, r)
		mu.Unlock()
	})

	llm := &flakyLLM{resp: "recovered", failN: 2}
	ctx := context.Background()
	resp, err, timedOut := tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("重试后应成功，got err=%v", err)
	}
	if timedOut {
		t.Fatal("不应超时")
	}
	if resp != "recovered" {
		t.Fatalf("响应应为 recovered，got %s", resp)
	}
	if llm.calls != 3 {
		t.Fatalf("应调用 3 次，got %d", llm.calls)
	}
	// 回调通过 goroutine 异步触发，轮询等待
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(records)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("应仅 1 条记录，got %d", len(records))
	}
	if records[0].Err != nil {
		t.Fatalf("记录的错误应为 nil，got %v", records[0].Err)
	}
}

// TestCallWithTimeoutAllFailReturnsError 验证 P0-1：3 次全部失败时返回错误，
// 仅 1 条记录，且连续 3 次逻辑调用失败后进入 slowMode。
func TestCallWithTimeoutAllFailReturnsError(t *testing.T) {
	tracker := NewLLMCallTracker()
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {}) // no-op

	llm := &flakyLLM{resp: "never", failN: 100} // 永远失败
	ctx := context.Background()

	// 第一次逻辑调用：3 次重试全失败
	_, err, _ := tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if err == nil {
		t.Fatal("全部失败应返回错误")
	}
	if llm.calls != 3 {
		t.Fatalf("应重试 3 次，got %d", llm.calls)
	}
	if tracker.ShouldSkipLLM() {
		t.Fatal("单次逻辑失败不应进入 slowMode")
	}

	// 再连续两次逻辑失败，累计 3 次后进入 slowMode
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if !tracker.ShouldSkipLLM() {
		t.Fatal("连续 3 次逻辑失败应进入 slowMode")
	}
}

// TestRetryGenerateBackoffRespectsCancel 验证 retryGenerate 在 ctx 取消时及时退出。
func TestRetryGenerateBackoffRespectsCancel(t *testing.T) {
	llm := &flakyLLM{resp: "x", failN: 100}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消
	_, err, _ := retryGenerate(ctx, llm, "p", 5*time.Second)
	if err == nil {
		t.Fatal("取消的 ctx 应返回错误")
	}
}
