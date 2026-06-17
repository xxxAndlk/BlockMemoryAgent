package graph

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/internal/runtime"
	"github.com/blockmemory/agent/internal/skill"
	pkgconfig "github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

// minimalRoleConfig 构造一个不依赖 yaml 的最小角色配置（mock 模式跑得通）
func minimalRoleConfig() *pkgconfig.RoleConfigFile {
	return &pkgconfig.RoleConfigFile{
		MetaAgent: pkgconfig.MetaAgentConfig{
			MaxBlocks:       4,
			SummaryInterval: 1,
			ModelConfig:     types.AgentModelConfig{Provider: "openai", Model: "mock"}, // APIKey 空 → mockClient
		},
		DomainAgent: pkgconfig.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{
				ID:          "code_assistant",
				Name:        "代码助手",
				Type:        types.RoleTypeFixed,
				Lifecycle:   types.RoleLifecyclePermanent,
				Skills:      []string{"代码"},
				Keywords:    []string{"代码", "code"},
				CanBeCalled: true,
			},
		},
	}
}

// TestThreeLayerGraph_RuntimeWired 验证：开一次 Session 后，
// Runtime.Boards 已有该会话的看板、Watchdog 至少记录一次决策。
func TestThreeLayerGraph_RuntimeWired(t *testing.T) {
	cfg := minimalRoleConfig()
	registry := NewRoleRegistry(cfg)
	// 不用真实模型工厂；MetaAgent 在没有 modelFactory 时也能跑（走规则回退）
	factory := NewRoleFactory(registry, nil, cfg)

	rt := runtime.New("", skill.BuiltinPool())

	meta := NewMetaAgentNode(registry, factory, cfg.MetaAgent.MaxBlocks, cfg.MetaAgent.SummaryInterval)
	meta.SetRuntime(rt)
	escalation := NewEscalationHandlerNode()
	sinker := &fakeSinker{}

	builder := NewThreeLayerGraphBuilder(registry, factory)
	builder.SetRuntime(rt)
	builder.AddNode(meta)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	g := builder.Build()

	state := types.NewThreeLayerState("test-session")
	state.DomainGoal = "修复商城首页穿模问题"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := g.Invoke(ctx, state); err != nil {
		t.Fatalf("graph invoke: %v", err)
	}

	if rt.Boards.Get("test-session") == nil {
		t.Fatalf("expected TaskBoard for session to be created")
	}
	// Watchdog 至少做过一次评估
	if len(rt.Watchdog.History()) == 0 {
		t.Fatalf("expected watchdog to record at least one decision")
	}
}

type fakeSinker struct{}

func (n *fakeSinker) Name() string { return "Sinker" }
func (n *fakeSinker) Invoke(ctx context.Context, s *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	s.NextAction = types.ActionFinish
	return s, nil
}
