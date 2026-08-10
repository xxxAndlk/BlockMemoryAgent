package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// TestVerifyConnectivityRequiresKey 验证 P0-1：无 API key 的角色全部视为未配置，
// 严格启动要求返回聚合错误，不允许跳过或 Mock 启动。
func TestVerifyConnectivityRequiresKey(t *testing.T) {
	// 构造角色配置：三个角色均提供 provider 与 model，但无 APIKey
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "m"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "d"}},
		FixedRoles:  []types.RoleDefinition{{ID: "coder", ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "c"}}},
	}
	// 创建模型工厂
	f := NewModelFactory(cfg)
	// 调用连通性校验，期望返回错误
	if err := f.VerifyConnectivity(context.Background()); err == nil {
		t.Fatal("无 key 应返回错误，禁止跳过启动")
	}
}

// TestVerifyConnectivityFailsOnUnreachable 验证 P0-1：有 key 但后端不可达时返回聚合错误。
func TestVerifyConnectivityFailsOnUnreachable(t *testing.T) {
	// 构造角色配置：MetaAgent 指向一个不可达的本地端口
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			ModelConfig: types.AgentModelConfig{
				Provider: "openai", Model: "m", APIKey: "k", BaseURL: "http://127.0.0.1:0/v1",
			},
		},
	}
	// 创建模型工厂
	f := NewModelFactory(cfg)
	// 使用 60 秒超时的上下文，避免测试无限挂起
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	// 函数退出时取消上下文
	defer cancel()
	// 调用连通性校验，期望因后端不可达返回错误
	if err := f.VerifyConnectivity(ctx); err == nil {
		t.Fatal("不可达后端应返回错误")
	}
}

// TestCallLightweightWithRetryNoEndpoint 验证 P0-1：轻量直连 helper 在无可达后端时返回 err。
func TestCallLightweightWithRetryNoEndpoint(t *testing.T) {
	// 构造角色配置：DomainAgent 指向不可达端口；lightweight 未配置会回退到 domain
	cfg := &config.RoleConfigFile{
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "d", APIKey: "k", BaseURL: "http://127.0.0.1:0/v1"}},
	}
	// 创建模型工厂
	f := NewModelFactory(cfg)
	// 使用 60 秒超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	// 函数退出时取消上下文
	defer cancel()
	// 调用轻量模型重试生成，期望返回错误
	if _, err := f.CallLightweightWithRetry(ctx, "ping"); err == nil {
		t.Fatal("不可达后端应返回错误")
	}
}

// fakeStreamingClient 实现 LLMClient + streamingProvider（BladesClient 同款能力），
// 分块产出文本，用于验证轻量链路流式累积（TODO #33）。
type fakeStreamingClient struct {
	chunks []string
	err    error
	calls  int
}

func (c *fakeStreamingClient) Generate(ctx context.Context, prompt string) (string, error) {
	c.calls++
	return strings.Join(c.chunks, ""), c.err
}

func (c *fakeStreamingClient) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	c.calls++
	return func(yield func(*blades.ModelResponse, error) bool) {
		for _, ch := range c.chunks {
			if !yield(&blades.ModelResponse{Message: blades.AssistantMessage(ch)}, nil) {
				return
			}
		}
		if c.err != nil {
			yield(nil, c.err)
			return
		}
	}
}

// TestRetryStreamGenerate_AccumulatesChunks 流式路径取最后一个累积分块（真实 provider
// 约定"逐块产出累积响应，最后一块为完整响应"）；中途错误走重试，全错返回错误。
func TestRetryStreamGenerate_AccumulatesChunks(t *testing.T) {
	client := &fakeStreamingClient{chunks: []string{
		"已读文件: ",
		"已读文件: config.js\n",
		"已读文件: config.js\n结论: ",
		"已读文件: config.js\n结论: 路径引用错误", // 末块 = 完整累积响应
	}}
	text, err, _ := retryStreamGenerate(context.Background(), client, "prompt", 5*time.Second)
	if err != nil {
		t.Fatalf("stream generate: %v", err)
	}
	if text != "已读文件: config.js\n结论: 路径引用错误" {
		t.Fatalf("expected last cumulative chunk, got: %q", text)
	}
	if client.calls != 1 {
		t.Fatalf("expected single streaming attempt, got %d calls", client.calls)
	}
}

// flakyStreamWrapper 首次 NewStreaming/Generate 调用注入错误，之后委托内层客户端
// （用于验证重试路径：首 attempt 失败 → 退避 → 第二次成功）。
type flakyStreamWrapper struct {
	inner    *fakeStreamingClient
	failedOnce bool
}

func (w *flakyStreamWrapper) Generate(ctx context.Context, prompt string) (string, error) {
	if !w.failedOnce {
		w.failedOnce = true
		return "", errors.New("transient failure")
	}
	return w.inner.Generate(ctx, prompt)
}

func (w *flakyStreamWrapper) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	if !w.failedOnce {
		w.failedOnce = true
		return func(yield func(*blades.ModelResponse, error) bool) {
			yield(nil, errors.New("transient failure"))
		}
	}
	return w.inner.NewStreaming(ctx, req)
}

// TestRetryStreamGenerate_ErrorRetries 首 attempt 出错后重试成功（指数退避路径）。
func TestRetryStreamGenerate_ErrorRetries(t *testing.T) {
	client := &fakeStreamingClient{chunks: []string{"ok"}}
	// 首次调用注入错误：calls=1 时报错，calls>=2 成功。
	flaky := &flakyStreamWrapper{inner: client}
	text, err, _ := retryStreamGenerate(context.Background(), flaky, "prompt", 5*time.Second)
	if err != nil {
		t.Fatalf("should recover after retry: %v", err)
	}
	if text != "ok" {
		t.Fatalf("expected recovered text, got %q", text)
	}
}

// TestRetryStreamGenerate_AllFail 全部 attempt 失败返回最后一次错误。
func TestRetryStreamGenerate_AllFail(t *testing.T) {
	client := &fakeStreamingClient{err: errors.New("endpoint rejected non-streaming")}
	_, err, _ := retryStreamGenerate(context.Background(), client, "prompt", 5*time.Second)
	if err == nil {
		t.Fatal("expected error when all attempts fail")
	}
}

// TestLightweightResolution 轻量模型解析来源：direct（配置了 lightweight_model）/
// fallback-domain（未配置回退 domain），供启动日志排查配置加载漂移（TODO #33）。
func TestLightweightResolution(t *testing.T) {
	cfg := &config.RoleConfigFile{
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "glm-5.2"}},
	}
	f := NewModelFactory(cfg)
	if _, src := f.LightweightResolution(); src != "fallback-domain" {
		t.Fatalf("empty lightweight config should fallback-domain, got %q", src)
	}
	cfg.LightweightModel = types.AgentModelConfig{Provider: "openai", Model: "deepseek-v4-flash"}
	if m, src := f.LightweightResolution(); src != "direct" || m.Model != "deepseek-v4-flash" {
		t.Fatalf("expected direct deepseek-v4-flash, got src=%q model=%q", src, m.Model)
	}
}
