package middleware

// middleware_test.go 验证 TODO #19 Phase 0 核心抽象 + LLM 链中间件：
// 执行顺序（入向 1→2→3、出向逆序）、error 短路、Use 扩展、空链直调终端、
// RetryLLM/CallLLM 行为（重试/退避/取消不重试/Deadline 不重试/空响应不重试）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/blades"
)

type testCtx struct {
	order []string
}

func mwOrder(name string, log *[]string) Middleware[testCtx] {
	return func(next Handler[testCtx]) Handler[testCtx] {
		return func(ctx context.Context, c *testCtx) error {
			*log = append(*log, "enter:"+name)
			err := next(ctx, c)
			*log = append(*log, "exit:"+name)
			return err
		}
	}
}

// TestChain_ExecutionOrder 洋葱顺序：入向 1→2→3，出向 3→2→1。
func TestChain_ExecutionOrder(t *testing.T) {
	var log []string
	chain := New[testCtx]().
		Use(mwOrder("1", &log), mwOrder("2", &log)).
		Use(mwOrder("3", &log)).
		Then(func(ctx context.Context, c *testCtx) error {
			log = append(log, "terminal")
			return nil
		})
	if err := chain(context.Background(), &testCtx{}); err != nil {
		t.Fatalf("chain: %v", err)
	}
	want := []string{"enter:1", "enter:2", "enter:3", "terminal", "exit:3", "exit:2", "exit:1"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Fatalf("order mismatch: got %v want %v", log, want)
	}
}

// TestChain_ErrorShortCircuit 中间件返回错误立即短路（出向仍执行）。
func TestChain_ErrorShortCircuit(t *testing.T) {
	var log []string
	boom := func(next Handler[testCtx]) Handler[testCtx] {
		return func(ctx context.Context, c *testCtx) error {
			log = append(log, "boom")
			return errors.New("boom")
		}
	}
	chain := New[testCtx]().
		Use(mwOrder("1", &log)).
		Use(boom).
		Use(mwOrder("3", &log)).
		Then(func(ctx context.Context, c *testCtx) error {
			log = append(log, "terminal")
			return nil
		})
	err := chain(context.Background(), &testCtx{})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected boom error, got %v", err)
	}
	if strings.Contains(strings.Join(log, ","), "terminal") {
		t.Fatalf("terminal must not run after short-circuit, log=%v", log)
	}
}

// TestChain_EmptyChain 空链直调终端（零开销）。
func TestChain_EmptyChain(t *testing.T) {
	called := false
	chain := New[testCtx]().Then(func(ctx context.Context, c *testCtx) error {
		called = true
		return nil
	})
	if err := chain(context.Background(), &testCtx{}); err != nil {
		t.Fatalf("empty chain: %v", err)
	}
	if !called {
		t.Fatal("terminal should be called directly")
	}
}

// fakeLLMCall 是可编程的 LLM 终端调用。
type fakeLLMCall struct {
	mu      sync.Mutex
	fails   int // 前 fails 次返回错误
	timeout bool // 返回 DeadlineExceeded
	err     error
	calls   int
	delay   time.Duration
}

func (f *fakeLLMCall) Call(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.fails > 0 {
		f.fails--
		if f.timeout {
			return nil, context.DeadlineExceeded
		}
		if f.err != nil {
			return nil, f.err
		}
		return nil, errors.New("transient")
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("ok")}, nil
}

func (f *fakeLLMCall) callsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func runChain(c *LLMCtx, retries int, backoff time.Duration, timeout time.Duration) error {
	return New[LLMCtx]().
		Use(RetryLLM(retries, backoff, nil)).
		Use(CallLLM(timeout)).
		Then(TerminalCall)(context.Background(), c)
}

// TestRetryLLM_RetriesThenSucceeds 瞬时错误重试后成功。
func TestRetryLLM_RetriesThenSucceeds(t *testing.T) {
	f := &fakeLLMCall{fails: 2}
	c := &LLMCtx{Request: &blades.ModelRequest{}, Call: f.Call}
	if err := runChain(c, 3, time.Millisecond, 0); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if c.Resp == nil || c.Resp.Message == nil {
		t.Fatal("resp should be set")
	}
	if got := f.callsCount(); got != 3 {
		t.Fatalf("expected 3 attempts (1+2 retries), got %d", got)
	}
}

// TestRetryLLM_DeadlineNoRetry 单次调用超时不重试（慢推理模型重试风暴防护）。
func TestRetryLLM_DeadlineNoRetry(t *testing.T) {
	f := &fakeLLMCall{fails: 5, timeout: true}
	c := &LLMCtx{Request: &blades.ModelRequest{}, Call: f.Call}
	err := runChain(c, 3, time.Millisecond, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if got := f.callsCount(); got != 1 {
		t.Fatalf("deadline should not retry, got %d attempts", got)
	}
}

// TestRetryLLM_CancelNoRetry 会话取消不重试。
func TestRetryLLM_CancelNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeLLMCall{fails: 5, err: errors.New("cancelled-ish")}
	c := &LLMCtx{Request: &blades.ModelRequest{}, Call: func(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
		cancel()
		return f.Call(ctx, req)
	}}
	err := New[LLMCtx]().
		Use(RetryLLM(3, time.Millisecond, nil)).
		Then(TerminalCall)(ctx, c)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := f.callsCount(); got != 1 {
		t.Fatalf("cancel should stop retries, got %d attempts", got)
	}
}

// TestRetryLLM_EmptyResponseNoRetry 空响应不重试。
func TestRetryLLM_EmptyResponseNoRetry(t *testing.T) {
	call := func(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
		return nil, nil
	}
	c := &LLMCtx{Request: &blades.ModelRequest{}, Call: call}
	err := runChain(c, 3, time.Millisecond, 0)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty response error, got %v", err)
	}
}

// TestCallLLM_Timeout 单次调用超时生效（CallLLM 层）。
func TestCallLLM_Timeout(t *testing.T) {
	slow := func(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return &blades.ModelResponse{Message: blades.AssistantMessage("late")}, nil
		}
	}
	c := &LLMCtx{Request: &blades.ModelRequest{}, Call: slow}
	err := New[LLMCtx]().
		Use(CallLLM(50 * time.Millisecond)).
		Then(TerminalCall)(context.Background(), c)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout, got %v", err)
	}
}
