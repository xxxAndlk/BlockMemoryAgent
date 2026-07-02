package model

import (
	"context"
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
