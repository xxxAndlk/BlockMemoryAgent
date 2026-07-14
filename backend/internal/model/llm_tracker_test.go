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
	resp  string
	failN int
	calls int
	mu    sync.Mutex
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
// 仅 1 条记录。通用错误不再触发 slow mode（Task 4.10 行为修复）。
func TestCallWithTimeoutAllFailReturnsError(t *testing.T) {
	tracker := NewLLMCallTracker()
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {}) // no-op

	llm := &flakyLLM{resp: "never", failN: 100} // 永远返回通用错误
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
		t.Fatal("单次通用错误不应进入 slowMode")
	}

	// 再连续两次通用错误，累计 3 次后仍不应进入 slowMode
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if tracker.ShouldSkipLLM() {
		t.Fatal("通用错误不应触发 slowMode")
	}
}

// TestCallWithTimeoutConsecutiveTimeoutsTriggerSlowMode 验证连续超时触发慢速模式。
func TestCallWithTimeoutConsecutiveTimeoutsTriggerSlowMode(t *testing.T) {
	tracker := NewLLMCallTracker()
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {}) // no-op

	llm := &fakeLLM{err: context.DeadlineExceeded}
	ctx := context.Background()

	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if tracker.ShouldSkipLLM() {
		t.Fatal("2 次连续超时后不应进入 slowMode")
	}

	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	if !tracker.ShouldSkipLLM() {
		t.Fatal("3 次连续超时后应进入 slowMode")
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

// TestLLMCallTrackerClassifiesErrors 是 Task 4.10 的回归测试：
// 旧行为把 context.Canceled 也计入 timeoutCount 并触发 slow mode；
// 修复后仅 context.DeadlineExceeded 计入超时，取消不再触发慢速模式。
func TestLLMCallTrackerClassifiesErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("context.Canceled does not trigger slow mode", func(t *testing.T) {
		tracker := NewLLMCallTracker()
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		// 记录 3 次 context.Canceled（旧 bug：这会进入 slow mode）
		for i := 0; i < 3; i++ {
			tracker.RecordCall(ctx, 0, context.Canceled, "test", "", "", "", 0, 0, false)
		}

		// 修复后断言：cancel 不计入 slow mode
		if tracker.ShouldSkipLLM() {
			t.Fatal("修复后：3 次 cancel 不应进入 slow mode")
		}
		calls, timeouts, _, _ := tracker.Stats()
		if calls != 3 {
			t.Fatalf("调用次数应为 3，got %d", calls)
		}
		if timeouts != 0 {
			t.Fatalf("超时计数应为 0，got %d", timeouts)
		}
	})

	t.Run("context.DeadlineExceeded triggers slow mode after threshold", func(t *testing.T) {
		tracker := NewLLMCallTracker()
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		if tracker.ShouldSkipLLM() {
			t.Fatal("2 次超时后不应进入 slow mode")
		}

		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		if !tracker.ShouldSkipLLM() {
			t.Fatal("3 次连续超时后应进入 slow mode")
		}
	})

	t.Run("nil error resets consecutive counters", func(t *testing.T) {
		tracker := NewLLMCallTracker()
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		tracker.RecordCall(ctx, 0, nil, "test", "", "", "ok", 0, 0, false)
		if tracker.ShouldSkipLLM() {
			t.Fatal("成功调用后应退出 slow mode")
		}

		_, timeouts, _, _ := tracker.Stats()
		if timeouts != 0 {
			t.Fatalf("成功调用后超时计数应清零，got %d", timeouts)
		}

		// 再次 1 次超时不应重新进入 slow mode
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		if tracker.ShouldSkipLLM() {
			t.Fatal("成功调用后仅 1 次超时不应进入 slow mode")
		}
	})
}
