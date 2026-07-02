package model

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestVerifyConnectivitySkipsNoKey 验证 P0-1：无 API key 的角色全部跳过，返回 nil（Mock 模式不阻塞启动）。
func TestVerifyConnectivitySkipsNoKey(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "m"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "d"}},
		FixedRoles:  []types.RoleDefinition{{ID: "coder", ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "c"}}},
	}
	f := NewModelFactory(cfg)
	if err := f.VerifyConnectivity(context.Background()); err != nil {
		t.Fatalf("无 key 应跳过并返回 nil，got %v", err)
	}
}

// TestVerifyConnectivityFailsOnUnreachable 验证 P0-1：有 key 但后端不可达时返回聚合错误。
func TestVerifyConnectivityFailsOnUnreachable(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			ModelConfig: types.AgentModelConfig{
				Provider: "openai", Model: "m", APIKey: "k", BaseURL: "http://127.0.0.1:0/v1",
			},
		},
	}
	f := NewModelFactory(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := f.VerifyConnectivity(ctx); err == nil {
		t.Fatal("不可达后端应返回错误")
	}
}

// TestCallLightweightWithRetryNoEndpoint 验证 P0-1：轻量直连 helper 在无可达后端时返回 err。
func TestCallLightweightWithRetryNoEndpoint(t *testing.T) {
	cfg := &config.RoleConfigFile{
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "d", APIKey: "k", BaseURL: "http://127.0.0.1:0/v1"}},
	}
	f := NewModelFactory(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := f.CallLightweightWithRetry(ctx, "ping"); err == nil {
		t.Fatal("不可达后端应返回错误")
	}
}
