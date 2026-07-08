package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/soul"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// recordingTracker 包装 model.LLMCallTracker，记录 CallWithTimeout 收到的关键参数。
type recordingTracker struct {
	*model.LLMCallTracker
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	caller string
	prompt string
	soft   time.Duration
	hard   time.Duration
	llm    model.LLMClient
}

func (r *recordingTracker) CallWithTimeout(ctx context.Context, llm model.LLMClient, prompt, caller string, soft, hard time.Duration) (string, error, bool) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{caller: caller, prompt: prompt, soft: soft, hard: hard, llm: llm})
	r.mu.Unlock()
	return r.LLMCallTracker.CallWithTimeout(ctx, llm, prompt, caller, soft, hard)
}

func (r *recordingTracker) Calls() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

// assertTemperatureWrapped 验证传入的 LLM 被 routingTemperature 包装。
func assertTemperatureWrapped(t *testing.T, llm model.LLMClient) {
	t.Helper()
	wrapped, ok := llm.(*temperatureWrappedLLM)
	if !ok {
		t.Fatalf("expected *temperatureWrappedLLM, got %T", llm)
	}
	if wrapped.temperature != routingTemperature {
		t.Errorf("wrapped temperature = %v, want %v", wrapped.temperature, routingTemperature)
	}
}

// newMockModelFactory 构造一个指向 mock HTTP 服务器的 ModelFactory，
// meta/domain/lightweight 三个角色均可用。
func newMockModelFactory(t *testing.T, serverURL string) *model.ModelFactory {
	t.Helper()
	cfg := &pkgconfig.RoleConfigFile{
		MetaAgent: pkgconfig.MetaAgentConfig{
			ModelConfig: types.AgentModelConfig{
				Provider: "openai",
				Model:    "gpt-test",
				APIKey:   "test-key",
				BaseURL:  fmt.Sprintf("%s/v1", serverURL),
			},
		},
		DomainAgent: pkgconfig.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{
				Provider: "openai",
				Model:    "gpt-test",
				APIKey:   "test-key",
				BaseURL:  fmt.Sprintf("%s/v1", serverURL),
			},
		},
		LightweightModel: types.AgentModelConfig{
			Provider: "openai",
			Model:    "gpt-test",
			APIKey:   "test-key",
			BaseURL:  fmt.Sprintf("%s/v1", serverURL),
		},
	}
	return model.NewModelFactory(cfg)
}

// newMockLLMServer 启动一个返回固定 OpenAI 格式补全的 HTTP 测试服务器。
func newMockLLMServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 0,
			"model":   "gpt-test",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "mock response",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 10,
				"total_tokens":      20,
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode mock response: %v", err)
		}
	}))
}

// newTestSoulLoader 从临时 soul.md 文件构造 soul.Loader。
func newTestSoulLoader(t *testing.T, content string) *soul.Loader {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write soul.md: %v", err)
	}
	loader := soul.NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatalf("load soul.md: %v", err)
	}
	return loader
}

// newTestBaseAgentNode 构造一个用于 CallLLM 测试的 BaseAgentNode。
func newTestBaseAgentNode(t *testing.T, mf *model.ModelFactory) (*BaseAgentNode, *recordingTracker, *[]ProgressEvent) {
	t.Helper()
	tracker := &recordingTracker{LLMCallTracker: model.NewLLMCallTracker()}
	var events []ProgressEvent
	var mu sync.Mutex
	node := &BaseAgentNode{
		modelFactory: mf,
		llmTracker:   tracker, // recordingTracker 实现 llmCallTracker
		progress: func(ctx context.Context, ev ProgressEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
		agentLabel: func() string { return "TestAgent" },
	}
	return node, tracker, &events
}

// TestCallLLM_MetaAgent_DefaultTimeouts 验证 MetaAgent 调用使用默认 30s/90s。
func TestCallLLM_MetaAgent_DefaultTimeouts(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "MetaAgent", UseMetaModel: true})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 30*time.Second {
		t.Errorf("MetaAgent default soft timeout = %v, want 30s", calls[0].soft)
	}
	if calls[0].hard != 90*time.Second {
		t.Errorf("MetaAgent default hard timeout = %v, want 90s", calls[0].hard)
	}
}

// TestCallLLM_MetaAgent_AgentCfgOverride 验证 MetaAgent 默认 30s/90s 可被 AgentCfg 覆盖。
func TestCallLLM_MetaAgent_AgentCfgOverride(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)
	node.rt = &runtime.Runtime{
		AgentCfg: &config.AgentConfig{LLMSoftTimeoutSec: 10, LLMHardTimeoutSec: 20},
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "MetaAgent", UseMetaModel: true})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 10*time.Second {
		t.Errorf("MetaAgent AgentCfg soft timeout = %v, want 10s", calls[0].soft)
	}
	if calls[0].hard != 20*time.Second {
		t.Errorf("MetaAgent AgentCfg hard timeout = %v, want 20s", calls[0].hard)
	}
}

// TestCallLLM_MetaAgent_SoulInjection 验证 MetaAgent InjectSoul=true 时 prompt 被注入 soul 内容。
func TestCallLLM_MetaAgent_SoulInjection(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)
	node.rt = &runtime.Runtime{
		Soul: newTestSoulLoader(t, "你是一位乐于助人的 AI 助手。"),
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "user prompt", LLMCallOptions{Caller: "MetaAgent", UseMetaModel: true, InjectSoul: true})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].prompt, "你是一位乐于助人的 AI 助手。") {
		t.Errorf("MetaAgent prompt should contain soul content, got: %s", calls[0].prompt)
	}
	if !strings.Contains(calls[0].prompt, "user prompt") {
		t.Errorf("MetaAgent prompt should contain original prompt, got: %s", calls[0].prompt)
	}
}

// TestCallLLM_DomainAgent_DefaultTimeouts 验证 DomainAgent 调用使用默认 30s/90s 并可被 AgentCfg 覆盖。
func TestCallLLM_DomainAgent_DefaultTimeouts(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "DomainAgent/任务拆解"})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 30*time.Second || calls[0].hard != 90*time.Second {
		t.Errorf("DomainAgent default timeouts = %v/%v, want 30s/90s", calls[0].soft, calls[0].hard)
	}
}

// TestCallLLM_DomainAgent_AgentCfgOverride 验证 DomainAgent AgentCfg 覆盖。
func TestCallLLM_DomainAgent_AgentCfgOverride(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)
	node.rt = &runtime.Runtime{
		AgentCfg: &config.AgentConfig{LLMSoftTimeoutSec: 5, LLMHardTimeoutSec: 8},
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "DomainAgent/任务拆解"})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 5*time.Second || calls[0].hard != 8*time.Second {
		t.Errorf("DomainAgent AgentCfg timeouts = %v/%v, want 5s/8s", calls[0].soft, calls[0].hard)
	}
}

// TestCallLLM_DomainAgent_NoSoulInjection 验证 DomainAgent 即使配置了 soul 也不注入。
func TestCallLLM_DomainAgent_NoSoulInjection(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)
	node.rt = &runtime.Runtime{
		Soul: newTestSoulLoader(t, "人格内容"),
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "user prompt", LLMCallOptions{Caller: "DomainAgent/任务拆解"})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if strings.Contains(calls[0].prompt, "人格内容") {
		t.Errorf("DomainAgent prompt should NOT contain soul content by default")
	}
}

// TestCallLLM_DomainLightweight_HardcodedTimeouts 验证 Domain callLightweightAs 使用硬编码 15s/25s，
// 且不受 AgentCfg 覆盖。
func TestCallLLM_DomainLightweight_HardcodedTimeouts(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	tracker := &recordingTracker{LLMCallTracker: model.NewLLMCallTracker()}
	domain := &DomainAgentNode{BaseAgentNode: BaseAgentNode{
		modelFactory: mf,
		llmTracker:   tracker,
		agentLabel:   func() string { return "DomainAgent" },
	}}
	domain.rt = &runtime.Runtime{
		AgentCfg: &config.AgentConfig{LLMSoftTimeoutSec: 100, LLMHardTimeoutSec: 200},
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = domain.callLightweightAs(ctx, "DomainAgent/任务拆解(轻量)", "prompt")

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 15*time.Second {
		t.Errorf("Domain lightweight soft timeout = %v, want 15s", calls[0].soft)
	}
	if calls[0].hard != 25*time.Second {
		t.Errorf("Domain lightweight hard timeout = %v, want 25s", calls[0].hard)
	}
}

// TestCallLLM_DomainLightweight_FallbackUsesDefaultTimeouts 验证轻量模型不可用时，
// Domain callLightweightAs 回退到领域模型并使用 30s/90s 默认超时（允许 AgentCfg 覆盖）。
func TestCallLLM_DomainLightweight_FallbackUsesDefaultTimeouts(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	// 只配置 meta/domain，不配置 lightweight，让 GetLightweightModel 因缺少 API key 失败。
	cfg := &pkgconfig.RoleConfigFile{
		MetaAgent: pkgconfig.MetaAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "gpt-test", APIKey: "test-key", BaseURL: fmt.Sprintf("%s/v1", server.URL)},
		},
		DomainAgent: pkgconfig.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "gpt-test", APIKey: "test-key", BaseURL: fmt.Sprintf("%s/v1", server.URL)},
		},
		// LightweightModel 配置为缺少 API key，使 GetLightweightModel 返回 error。
		LightweightModel: types.AgentModelConfig{
			Provider: "openai",
			Model:    "gpt-test",
			APIKey:   "",
			BaseURL:  fmt.Sprintf("%s/v1", server.URL),
		},
	}
	mf := model.NewModelFactory(cfg)
	tracker := &recordingTracker{LLMCallTracker: model.NewLLMCallTracker()}
	domain := &DomainAgentNode{BaseAgentNode: BaseAgentNode{
		modelFactory: mf,
		llmTracker:   tracker,
		agentLabel:   func() string { return "DomainAgent" },
	}}
	domain.rt = &runtime.Runtime{
		AgentCfg: &config.AgentConfig{LLMSoftTimeoutSec: 7, LLMHardTimeoutSec: 11},
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = domain.callLightweightAs(ctx, "DomainAgent/任务拆解(轻量)", "prompt")

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 7*time.Second {
		t.Errorf("fallback soft timeout = %v, want 7s (AgentCfg override)", calls[0].soft)
	}
	if calls[0].hard != 11*time.Second {
		t.Errorf("fallback hard timeout = %v, want 11s (AgentCfg override)", calls[0].hard)
	}
}

// TestCallLLM_SubDomainAgent_DefaultTimeouts 验证 SubDomainAgent 调用使用默认 30s/90s 并可被 AgentCfg 覆盖。
func TestCallLLM_SubDomainAgent_DefaultTimeouts(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)
	node.rt = &runtime.Runtime{
		AgentCfg: &config.AgentConfig{LLMSoftTimeoutSec: 12, LLMHardTimeoutSec: 18},
	}

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "SubDomainAgent/任务拆解"})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].soft != 12*time.Second || calls[0].hard != 18*time.Second {
		t.Errorf("SubDomainAgent timeouts = %v/%v, want 12s/18s", calls[0].soft, calls[0].hard)
	}
}

// TestCallLLM_EmitsProgressEventsInOrder 验证 CallLLM 按 prompt → token_usage → llm_response 顺序推送事件。
func TestCallLLM_EmitsProgressEventsInOrder(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, _, eventsPtr := newTestBaseAgentNode(t, mf)

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{Caller: "TestAgent"})

	events := *eventsPtr
	if len(events) < 3 {
		t.Fatalf("expected at least 3 progress events, got %d: %+v", len(events), events)
	}
	if events[0].Kind != "prompt" {
		t.Errorf("first event kind = %q, want prompt", events[0].Kind)
	}
	if events[1].Kind != "token_usage" {
		t.Errorf("second event kind = %q, want token_usage", events[1].Kind)
	}
	if events[2].Kind != "llm_response" {
		t.Errorf("third event kind = %q, want llm_response", events[2].Kind)
	}
}

// TestCallLLM_MetaLightweight_UsesRoutingTemperature 验证 MetaAgent 轻量调用使用 0 温度包装。
func TestCallLLM_MetaLightweight_UsesRoutingTemperature(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)
	node, tracker, _ := newTestBaseAgentNode(t, mf)

	ctx := WithSessionID(context.Background(), "s1")
	_, _, _ = node.CallLLM(ctx, "prompt", LLMCallOptions{
		Caller:      "MetaAgent/路由判定(轻量)",
		Lightweight: true,
		Temperature: &routingTemperature,
	})

	calls := tracker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].caller != "MetaAgent/路由判定(轻量)" {
		t.Errorf("caller = %q, want MetaAgent/路由判定(轻量)", calls[0].caller)
	}
	assertTemperatureWrapped(t, calls[0].llm)
}

// newTestMetaAgentNode 构造一个用于测试 MetaAgent 调用行为的 MetaAgentNode。
func newTestMetaAgentNode(t *testing.T, mf *model.ModelFactory) (*MetaAgentNode, *recordingTracker) {
	t.Helper()
	tracker := &recordingTracker{LLMCallTracker: model.NewLLMCallTracker()}
	n := NewMetaAgentNode(nil, nil, 5, 0)
	n.modelFactory = mf
	n.llmTracker = tracker
	n.progress = func(ctx context.Context, ev ProgressEvent) {}
	return n, tracker
}

// TestCallLLM_MetaAgent_NonLightweight_UsesRoutingTemperature 验证 MetaAgent 非轻量调用
//（analyzeDomains / executeDirectAnswer / finalizeSession）默认被 routingTemperature 包装。
func TestCallLLM_MetaAgent_NonLightweight_UsesRoutingTemperature(t *testing.T) {
	server := newMockLLMServer(t)
	defer server.Close()
	mf := newMockModelFactory(t, server.URL)

	ctx := WithSessionID(context.Background(), "s1")

	t.Run("executeDirectAnswer", func(t *testing.T) {
		n, tracker := newTestMetaAgentNode(t, mf)
		_, _ = n.executeDirectAnswer(ctx, &types.ThreeLayerState{DomainGoal: "你好"})
		calls := tracker.Calls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(calls))
		}
		assertTemperatureWrapped(t, calls[0].llm)
	})

	t.Run("analyzeDomains", func(t *testing.T) {
		n, tracker := newTestMetaAgentNode(t, mf)
		_ = n.analyzeDomains(ctx, &types.ThreeLayerState{DomainGoal: "开发一个商城系统"})
		calls := tracker.Calls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(calls))
		}
		assertTemperatureWrapped(t, calls[0].llm)
	})

	t.Run("finalizeSession", func(t *testing.T) {
		n, tracker := newTestMetaAgentNode(t, mf)
		n.finalizeSession(ctx, &types.ThreeLayerState{SessionSummary: "已完成"})
		calls := tracker.Calls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(calls))
		}
		assertTemperatureWrapped(t, calls[0].llm)
	})
}
