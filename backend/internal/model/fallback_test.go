// fallback_test.go 备胎链测试（TODO 第15项 T17）：主败切备 / 全败返回最后错误 /
// ctx 取消不切 + 流式首 chunk 前失败降级。provider 全部 fake，不触网。
package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-kratos/blades"
)

// fakeFallbackProvider 可编程 fake provider：fail != nil 时每次调用返回该错误。
type fakeFallbackProvider struct {
	name     string
	fail     error
	genCalls int
}

func (f *fakeFallbackProvider) Name() string { return f.name }

func (f *fakeFallbackProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	f.genCalls++
	if f.fail != nil {
		return nil, f.fail
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("ok:" + f.name)}, nil
}

func (f *fakeFallbackProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		if f.fail != nil {
			yield(nil, f.fail)
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("stream:" + f.name)}, nil)
	}
}

// newTestFallbackProvider 组装主模型 + 一级备胎的包装器；build 调用计数经 buildCalls 观察。
func newTestFallbackProvider(t *testing.T, primary blades.ModelProvider, built map[string]blades.ModelProvider, buildCalls *int, observe FallbackObserver) *fallbackProvider {
	t.Helper()
	return newFallbackProvider("meta", primary, []string{"fb"}, func(roleID, entryID string) (blades.ModelProvider, error) {
		*buildCalls++
		if p, ok := built[entryID]; ok {
			return p, nil
		}
		return nil, errors.New("条目 " + entryID + " 不在模型注册表")
	}, observe)
}

// TestFallbackProvider_PrimaryFailsSwitchesBackup 主模型失败 → 依序切备胎成功，
// 观察者收到一次降级事件（From=主模型名，To=备胎条目 ID）。
func TestFallbackProvider_PrimaryFailsSwitchesBackup(t *testing.T) {
	primary := &fakeFallbackProvider{name: "primary", fail: errors.New("boom: primary down")}
	fb := &fakeFallbackProvider{name: "fb"}
	buildCalls := 0
	var events []FallbackEvent
	p := newTestFallbackProvider(t, primary, map[string]blades.ModelProvider{"fb": fb}, &buildCalls, func(e FallbackEvent) {
		events = append(events, e)
	})

	resp, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if got := resp.Message.Text(); got != "ok:fb" {
		t.Fatalf("resp = %q, want 备胎响应 ok:fb", got)
	}
	if primary.genCalls != 1 || fb.genCalls != 1 {
		t.Fatalf("调用次数 primary=%d fb=%d, want 1/1", primary.genCalls, fb.genCalls)
	}
	if len(events) != 1 || events[0].From != "primary" || events[0].To != "fb" || !strings.Contains(events[0].Err, "primary down") {
		t.Fatalf("降级事件不符: %+v", events)
	}
}

// TestFallbackProvider_AllFailReturnsLastError 全部档位失败：返回最后一次错误，
// 每次降级各回调一次观察者。
func TestFallbackProvider_AllFailReturnsLastError(t *testing.T) {
	primary := &fakeFallbackProvider{name: "primary", fail: errors.New("err-0")}
	fb := &fakeFallbackProvider{name: "fb", fail: errors.New("err-1")}
	buildCalls := 0
	var events []FallbackEvent
	p := newTestFallbackProvider(t, primary, map[string]blades.ModelProvider{"fb": fb}, &buildCalls, func(e FallbackEvent) {
		events = append(events, e)
	})

	_, err := p.Generate(context.Background(), &blades.ModelRequest{})
	if err == nil || err.Error() != "err-1" {
		t.Fatalf("err = %v, want 最后一次错误 err-1", err)
	}
	if len(events) != 1 || events[0].To != "fb" {
		t.Fatalf("降级事件 = %+v, want 恰一次切向 fb", events)
	}
}

// TestFallbackProvider_ContextCanceledNoSwitch ctx 已取消：原样返回错误，
// 不构造备胎、不触发降级回调（换模型救不了已放弃的调用）。
func TestFallbackProvider_ContextCanceledNoSwitch(t *testing.T) {
	primary := &fakeFallbackProvider{name: "primary", fail: context.Canceled}
	fb := &fakeFallbackProvider{name: "fb"}
	buildCalls := 0
	observed := false
	p := newTestFallbackProvider(t, primary, map[string]blades.ModelProvider{"fb": fb}, &buildCalls, func(FallbackEvent) {
		observed = true
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Generate(ctx, &blades.ModelRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if buildCalls != 0 {
		t.Fatalf("备胎不应被构造，buildCalls = %d", buildCalls)
	}
	if observed {
		t.Fatal("ctx 取消不应触发降级回调")
	}
}

// TestFallbackProvider_StreamingFallbackBeforeFirstChunk 流式：首个 chunk 之前失败
// → 切备胎成功；备胎流正常输出（流中失败不降级、直接透传，不在此测）。
func TestFallbackProvider_StreamingFallbackBeforeFirstChunk(t *testing.T) {
	primary := &fakeFallbackProvider{name: "primary", fail: errors.New("stream down")}
	fb := &fakeFallbackProvider{name: "fb"}
	buildCalls := 0
	var events []FallbackEvent
	p := newTestFallbackProvider(t, primary, map[string]blades.ModelProvider{"fb": fb}, &buildCalls, func(e FallbackEvent) {
		events = append(events, e)
	})

	var got []string
	for resp, err := range p.NewStreaming(context.Background(), &blades.ModelRequest{}) {
		if err != nil {
			t.Fatalf("流式意外出错: %v", err)
		}
		got = append(got, resp.Message.Text())
	}
	if len(got) != 1 || got[0] != "stream:fb" {
		t.Fatalf("流式输出 = %v, want [stream:fb]", got)
	}
	if len(events) != 1 || events[0].To != "fb" {
		t.Fatalf("降级事件 = %+v, want 恰一次切向 fb", events)
	}
}
