package main

import (
	"context"
	"fmt"
	"log"

	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

// MockLLMClient 模拟大模型客户端（当无API Key时回退使用）
type MockLLMClient struct{}

func (m *MockLLMClient) Generate(ctx context.Context, prompt string) (string, error) {
	return fmt.Sprintf("模拟LLM响应: %s", prompt[:min(50, len(prompt))]), nil
}

func main() {
	ctx := context.Background()

	// 1. 加载角色配置
	roleCfg, err := config.LoadRoleConfig("config/roles.yaml")
	if err != nil {
		log.Printf("Warning: load role config failed: %v", err)
		// 使用默认配置
		roleCfg = &config.RoleConfigFile{
			MetaAgent: config.MetaAgentConfig{MaxBlocks: 5, SummaryInterval: 3},
			DomainAgent: config.DomainAgentConfig{},
			FixedRoles: []types.RoleDefinition{
				{
					ID: "code_assistant", Name: "代码助手", Type: types.RoleTypeFixed,
					Lifecycle: types.RoleLifecyclePermanent, Description: "代码编写与审查",
					Skills: []string{"代码编写", "代码审查"}, Keywords: []string{"代码", "bug"},
					CanBeCalled: true,
				},
				{
					ID: "ui_assistant", Name: "UI助手", Type: types.RoleTypeFixed,
					Lifecycle: types.RoleLifecyclePermanent, Description: "前端UI实现",
					Skills: []string{"UI修复", "组件开发"}, Keywords: []string{"UI", "样式"},
					CanBeCalled: true,
				},
			},
		}
	}

	fmt.Println("=== 角色配置加载完成 ===")
	fmt.Printf("固定角色数量: %d\n", len(roleCfg.FixedRoles))
	for _, r := range roleCfg.FixedRoles {
		fmt.Printf("  - [%s] %s (类型: %s, 生命周期: %s, 模型: %s)\n",
			r.ID, r.Name, r.Type, r.Lifecycle, r.ModelConfig.Model)
	}
	fmt.Printf("MetaAgent 模型: %s\n", roleCfg.MetaAgent.ModelConfig.Model)
	fmt.Printf("DomainAgent 模型: %s\n", roleCfg.DomainAgent.ModelConfig.Model)

	// 2. 初始化模型工厂（按角色缓存 Eino ChatModel）
	modelFactory := model.NewModelFactory(roleCfg)

	// 尝试预热模型（如果配置了API Key则初始化真实模型，否则回退到Mock）
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Printf("Warning: model warmup failed (可能未配置API Key): %v", err)
		log.Println("回退到 MockLLMClient")
	}

	// 3. 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 3. 构建三层图
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	escalation := &mockThreeLayerNode{name: "EscalationHandler"}
	sinker := &mockThreeLayerNode{name: "Sinker"}

	threeLayerGraph := graph.BuildThreeLayerGraph(metaAgent, escalation, sinker, registry, factory)

	// 4. 启动示例会话
	sessionID := "demo-session-001"
	state := types.NewThreeLayerState(sessionID)
	state.DomainGoal = "修复商城主页穿模问题"

	fmt.Println("\n=== 启动三层架构会话 ===")
	fmt.Printf("会话ID: %s\n", sessionID)
	fmt.Printf("用户目标: %s\n", state.DomainGoal)

	// 5. 执行
	result, err := threeLayerGraph.Invoke(ctx, state)
	if err != nil {
		log.Fatalf("执行失败: %v", err)
	}

	fmt.Println("\n=== 会话执行完成 ===")
	fmt.Printf("最终动作: %s\n", result.NextAction)
	fmt.Printf("会话总结: %s\n", result.SessionSummary)
	fmt.Printf("完成的领域: %v\n", result.CompletedBlocks)

	// 6. 打印所有角色实例
	fmt.Println("\n=== 会话中创建的角色实例 ===")
	for _, inst := range registry.GetInstancesBySession(sessionID) {
		roleDef := registry.GetRoleDef(inst.RoleDefID)
		defName := "unknown"
		if roleDef != nil {
			defName = roleDef.Name
		}
		fmt.Printf("  - [%s] %s (类型: %s, 领域: %s, 状态: %s)\n",
			inst.ID, defName, inst.Type, inst.Domain, inst.Status)
	}
}

// mockThreeLayerNode 模拟三层节点
type mockThreeLayerNode struct {
	name string
}

func (m *mockThreeLayerNode) Name() string {
	return m.name
}

func (m *mockThreeLayerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	fmt.Printf("  [%s] 执行...\n", m.name)
	if m.name == "Sinker" {
		state.NextAction = types.ActionFinish
	} else {
		state.NextAction = types.ActionContinue
	}
	return state, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
