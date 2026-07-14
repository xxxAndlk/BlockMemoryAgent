package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeLLM 是一个用于测试的 LLMClient 实现，始终返回预设响应或错误。
type fakeLLM struct {
	resp string // 预设的成功响应
	err  error  // 预设的错误
}

// Generate 实现 LLMClient 接口，直接返回预设值。
func (f *fakeLLM) Generate(ctx context.Context, prompt string) (string, error) {
	// 忽略 ctx 与 prompt，直接返回构造时指定的响应和错误
	return f.resp, f.err
}

// flakyLLM 是一个前 failN 次返回错误、之后返回成功响应的测试替身，
// 用于验证重试逻辑。
type flakyLLM struct {
	resp  string     // 成功后的响应
	failN int        // 前 failN 次返回错误
	calls int        // 累计调用次数
	mu    sync.Mutex // 保护 calls 字段的并发访问
}

// Generate 实现 LLMClient 接口，按调用次数决定返回错误还是成功。
func (f *flakyLLM) Generate(ctx context.Context, prompt string) (string, error) {
	// 加锁保护 calls 计数
	f.mu.Lock()
	// 调用次数加一
	f.calls++
	// 取出当前调用序号
	n := f.calls
	// 解锁，避免阻塞后续操作
	f.mu.Unlock()
	// 若当前序号仍在失败次数范围内，返回构造的错误
	if n <= f.failN {
		return "", fmt.Errorf("transient error #%d", n)
	}
	// 超过失败次数后返回成功响应
	return f.resp, nil
}

// TestCallWithTimeoutInvokesRecordCallback 验证 CallWithTimeout 成功调用后
// 会异步触发 SetRecordCallback 设置的回调，且回调收到的记录字段正确。
func TestCallWithTimeoutInvokesRecordCallback(t *testing.T) {
	// 创建新的追踪器
	tracker := NewLLMCallTracker()

	// 用于同步验证回调结果的互斥锁与变量
	var mu sync.Mutex
	var got CallRecord
	// 带缓冲通道，避免回调 goroutine 阻塞
	called := make(chan struct{}, 1)
	// 设置记录回调
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {
		// 加锁保护 got 变量
		mu.Lock()
		// 保存收到的记录
		got = r
		// 释放锁
		mu.Unlock()
		// 通知主测试 goroutine 回调已触发
		called <- struct{}{}
	})

	// 构造返回 "hello" 的假 LLM
	llm := &fakeLLM{resp: "hello"}
	// 使用空上下文
	ctx := context.Background()
	// 调用被测方法
	resp, err, timedOut := tracker.CallWithTimeout(ctx, llm, "prompt text", "MetaAgent", 5*time.Second, 10*time.Second)
	// 校验不应返回错误
	if err != nil {
		t.Fatalf("CallWithTimeout 不应失败: %v", err)
	}
	// 校验不应超时
	if timedOut {
		t.Fatal("不应超时")
	}
	// 校验响应内容
	if resp != "hello" {
		t.Fatalf("响应应为 hello，got %s", resp)
	}

	// 等待回调被触发，超时则失败
	select {
	case <-called:
		// 回调已触发，继续验证
	case <-time.After(500 * time.Millisecond):
		t.Fatal("record callback 未被调用")
	}

	// 加锁读取回调中保存的记录
	mu.Lock()
	// 函数退出时释放锁
	defer mu.Unlock()
	// 校验 caller 字段
	if got.Caller != "MetaAgent" {
		t.Fatalf("caller 应为 MetaAgent，got %s", got.Caller)
	}
	// 校验 prompt 完整保留
	if got.Prompt != "prompt text" {
		t.Fatalf("prompt 应完整保留，got %s", got.Prompt)
	}
	// 校验 response 正确记录
	if got.Response != "hello" {
		t.Fatalf("response 应为 hello，got %s", got.Response)
	}
}

// TestCallWithTimeoutRetriesThenSucceeds 验证 P0-1：前两次失败第三次成功，
// 最终返回成功响应，且仅产生一条 RecordCall 记录、回调触发一次。
func TestCallWithTimeoutRetriesThenSucceeds(t *testing.T) {
	// 创建新的追踪器
	tracker := NewLLMCallTracker()
	// 同步回调中收到的记录列表
	var mu sync.Mutex
	var records []CallRecord
	// 设置记录回调
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {
		// 加锁保护 records
		mu.Lock()
		// 追加记录
		records = append(records, r)
		// 释放锁
		mu.Unlock()
	})

	// 构造前 2 次失败、第 3 次成功的 flaky LLM
	llm := &flakyLLM{resp: "recovered", failN: 2}
	ctx := context.Background()
	// 调用被测方法
	resp, err, timedOut := tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	// 校验最终成功
	if err != nil {
		t.Fatalf("重试后应成功，got err=%v", err)
	}
	// 校验未超时
	if timedOut {
		t.Fatal("不应超时")
	}
	// 校验响应内容
	if resp != "recovered" {
		t.Fatalf("响应应为 recovered，got %s", resp)
	}
	// 校验底层 LLM 被调用了 3 次
	if llm.calls != 3 {
		t.Fatalf("应调用 3 次，got %d", llm.calls)
	}
	// 回调通过 goroutine 异步触发，轮询等待记录到达
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		// 加锁读取当前记录数
		mu.Lock()
		n := len(records)
		mu.Unlock()
		// 若已收到记录或超过截止时间则退出轮询
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		// 短暂休眠后继续轮询
		time.Sleep(5 * time.Millisecond)
	}
	// 加锁校验记录数
	mu.Lock()
	// 函数退出时释放锁
	defer mu.Unlock()
	// 应仅产生 1 条记录（单次逻辑调用）
	if len(records) != 1 {
		t.Fatalf("应仅 1 条记录，got %d", len(records))
	}
	// 成功记录的错误字段应为 nil
	if records[0].Err != nil {
		t.Fatalf("记录的错误应为 nil，got %v", records[0].Err)
	}
}

// TestCallWithTimeoutAllFailReturnsError 验证 P0-1：3 次全部失败时返回错误，
// 仅 1 条记录。通用错误不再触发 slow mode（Task 4.10 行为修复）。
func TestCallWithTimeoutAllFailReturnsError(t *testing.T) {
	// 创建新的追踪器
	tracker := NewLLMCallTracker()
	// 设置空回调，避免 nil 回调影响流程
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {}) // no-op

	// 构造永远返回通用错误的 flaky LLM
	llm := &flakyLLM{resp: "never", failN: 100}
	ctx := context.Background()

	// 第一次逻辑调用：3 次重试全失败
	_, err, _ := tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	// 校验返回错误
	if err == nil {
		t.Fatal("全部失败应返回错误")
	}
	// 校验重试次数
	if llm.calls != 3 {
		t.Fatalf("应重试 3 次，got %d", llm.calls)
	}
	// 通用错误不应进入 slowMode
	if tracker.ShouldSkipLLM() {
		t.Fatal("单次通用错误不应进入 slowMode")
	}

	// 再连续两次通用错误，累计 3 次后仍不应进入 slowMode
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	// 再次校验 slowMode 未被触发
	if tracker.ShouldSkipLLM() {
		t.Fatal("通用错误不应触发 slowMode")
	}
}

// TestCallWithTimeoutConsecutiveTimeoutsTriggerSlowMode 验证连续超时触发慢速模式。
func TestCallWithTimeoutConsecutiveTimeoutsTriggerSlowMode(t *testing.T) {
	// 创建新的追踪器
	tracker := NewLLMCallTracker()
	// 设置空回调
	tracker.SetRecordCallback(func(ctx context.Context, r CallRecord) {}) // no-op

	// 构造始终返回 DeadlineExceeded 的假 LLM
	llm := &fakeLLM{err: context.DeadlineExceeded}
	ctx := context.Background()

	// 第 1、2 次连续超时
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	// 2 次后不应进入 slowMode
	if tracker.ShouldSkipLLM() {
		t.Fatal("2 次连续超时后不应进入 slowMode")
	}

	// 第 3 次连续超时
	tracker.CallWithTimeout(ctx, llm, "p", "MetaAgent", 5*time.Second, 10*time.Second)
	// 3 次后应进入 slowMode
	if !tracker.ShouldSkipLLM() {
		t.Fatal("3 次连续超时后应进入 slowMode")
	}
}

// TestRetryGenerateBackoffRespectsCancel 验证 retryGenerate 在 ctx 取消时及时退出。
func TestRetryGenerateBackoffRespectsCancel(t *testing.T) {
	// 构造永远失败的 flaky LLM
	llm := &flakyLLM{resp: "x", failN: 100}
	// 创建已取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	// 立即取消上下文
	cancel()
	// 调用被测函数
	_, err, _ := retryGenerate(ctx, llm, "p", 5*time.Second)
	// 校验返回错误
	if err == nil {
		t.Fatal("取消的 ctx 应返回错误")
	}
}

// TestLLMCallTrackerClassifiesErrors 是 Task 4.10 的回归测试：
// 旧行为把 context.Canceled 也计入 timeoutCount 并触发 slow mode；
// 修复后仅 context.DeadlineExceeded 计入超时，取消不再触发慢速模式。
func TestLLMCallTrackerClassifiesErrors(t *testing.T) {
	ctx := context.Background()

	// 子测试 1：context.Canceled 不触发慢速模式
	t.Run("context.Canceled does not trigger slow mode", func(t *testing.T) {
		// 创建新的追踪器
		tracker := NewLLMCallTracker()
		// 设置空回调
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		// 记录 3 次 context.Canceled（旧 bug：这会进入 slow mode）
		for i := 0; i < 3; i++ {
			tracker.RecordCall(ctx, 0, context.Canceled, "test", "", "", "", 0, 0, false)
		}

		// 修复后断言：cancel 不计入 slow mode
		if tracker.ShouldSkipLLM() {
			t.Fatal("修复后：3 次 cancel 不应进入 slow mode")
		}
		// 取统计值
		calls, timeouts, _, _ := tracker.Stats()
		// 调用次数应为 3
		if calls != 3 {
			t.Fatalf("调用次数应为 3，got %d", calls)
		}
		// 超时计数应为 0
		if timeouts != 0 {
			t.Fatalf("超时计数应为 0，got %d", timeouts)
		}
	})

	// 子测试 2：context.DeadlineExceeded 达到阈值后触发慢速模式
	t.Run("context.DeadlineExceeded triggers slow mode after threshold", func(t *testing.T) {
		// 创建新的追踪器
		tracker := NewLLMCallTracker()
		// 设置空回调
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		// 记录 2 次超时
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		// 2 次后不应进入 slow mode
		if tracker.ShouldSkipLLM() {
			t.Fatal("2 次超时后不应进入 slow mode")
		}

		// 第 3 次超时
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		// 3 次连续超时后应进入 slow mode
		if !tracker.ShouldSkipLLM() {
			t.Fatal("3 次连续超时后应进入 slow mode")
		}
	})

	// 子测试 3：nil 错误重置连续计数器
	t.Run("nil error resets consecutive counters", func(t *testing.T) {
		// 创建新的追踪器
		tracker := NewLLMCallTracker()
		// 设置空回调
		tracker.SetRecordCallback(func(context.Context, CallRecord) {})

		// 先记录 2 次超时
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		tracker.RecordCall(ctx, 0, context.DeadlineExceeded, "test", "", "", "", 0, 0, true)
		// 然后记录 1 次成功
		tracker.RecordCall(ctx, 0, nil, "test", "", "", "ok", 0, 0, false)
		// 成功后应退出 slow mode
		if tracker.ShouldSkipLLM() {
			t.Fatal("成功调用后应退出 slow mode")
		}

		// 校验超时计数已清零
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
