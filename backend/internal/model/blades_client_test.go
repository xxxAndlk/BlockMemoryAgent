package model

// blades_client_test.go 验证 doGenerate 流式优先：
// 2026-08-13 实证 ark /api/coding 对 thinking 模型拒绝非流式长任务请求
// （"streaming is required for operations that may take longer than 10 minutes"），
// 非流式 Generate 全天 400 致 skill 选择/策略决策等辅助调用静默失效。

import (
	"context"
	"errors"
	"testing"

	"github.com/go-kratos/blades"
)

// dualProvider 同时实现 Generate 与 NewStreaming；Generate 响应文本作判别标记。
type dualProvider struct {
	streamFails int
	emptyStream bool
	genCalls    int
}

// Name 满足 blades.ModelProvider 接口。
func (p *dualProvider) Name() string { return "dual" }

// Generate 返回 "from generate" 并计数（若被误调用，测试可检出）。
func (p *dualProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.genCalls++
	return &blades.ModelResponse{Message: blades.AssistantMessage("from generate")}, nil
}

// NewStreaming 前 streamFails 次产出错误流，之后产出成功响应；emptyStream 时无任何产出。
func (p *dualProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		if p.emptyStream {
			return
		}
		if p.streamFails > 0 {
			p.streamFails--
			yield(nil, errors.New("streaming is required for operations that may take longer than 10 minutes"))
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("from stream")}, nil)
	}
}

// TestBladesClientDoGenerate_PrefersStreaming 验证 provider 支持流式时走 NewStreaming 而非 Generate。
func TestBladesClientDoGenerate_PrefersStreaming(t *testing.T) {
	p := &dualProvider{}
	c := &BladesClient{provider: p}
	text, err := c.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("流式路径应成功，got err: %v", err)
	}
	if text != "from stream" {
		t.Fatalf("应返回流式响应，got %q", text)
	}
	if p.genCalls != 0 {
		t.Fatalf("不应调用 Generate，got %d calls", p.genCalls)
	}
}

// TestBladesClientDoGenerate_StreamingError 验证流式错误透传（上层兜底）。
func TestBladesClientDoGenerate_StreamingError(t *testing.T) {
	p := &dualProvider{streamFails: 1}
	c := &BladesClient{provider: p}
	if _, err := c.Generate(context.Background(), "prompt"); err == nil {
		t.Fatal("流式错误应透传")
	}
	if p.genCalls != 0 {
		t.Fatalf("流式出错不应回落 Generate，got %d calls", p.genCalls)
	}
}

// TestBladesClientDoGenerate_EmptyStream 验证无有效产出的空流返回错误。
func TestBladesClientDoGenerate_EmptyStream(t *testing.T) {
	p := &dualProvider{emptyStream: true}
	c := &BladesClient{provider: p}
	if _, err := c.Generate(context.Background(), "prompt"); err == nil {
		t.Fatal("空流应返回错误")
	}
}
