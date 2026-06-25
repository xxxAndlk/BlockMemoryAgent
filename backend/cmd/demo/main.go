package main

// cmd/demo 是 CLI 三层流演示入口: 加载角色配置 → 构建三层图 → 启动示例会话 → 打印角色实例。
// 不依赖数据库/HTTP，用于快速验证图与角色注册表是否工作。

import (
	"context" // 上下文
	"fmt"     // 标准输出
	"log"     // 警告日志

	"github.com/blockmemory/agent/backend/internal/graph" // 三层图构建
	"github.com/blockmemory/agent/backend/internal/model" // 模型工厂
	"github.com/blockmemory/agent/backend/pkg/config"     // 角色配置加载
	"github.com/blockmemory/agent/backend/pkg/types"      // 公共类型
)

// MockLLMClient 模拟大模型客户端（当无API Key时回退使用）
type MockLLMClient struct{}

// Generate 返回固定格式的模拟响应，仅用于演示无真实 LLM 时的回退路径。
func (m *MockLLMClient) Generate(ctx context.Context, prompt string) (string, error) {
	return fmt.Sprintf("模拟LLM响应: %s", prompt[:min(50, len(prompt))]), nil
}

// main 是演示入口: 装配三层图并执行一次示例会话，最后打印角色实例。
// 副作用: 控制台输出；无外部资源写入。
func main() {
	ctx := context.Background()

	// 1. 加载角色配置: 失败时使用内置默认配置，保证演示可运行
	roleCfg, err := config.LoadRoleConfig("config/roles.yaml")
	if err != nil {
		log.Printf("Warning: load role config failed: %v", err)
		// 使用默认配置
		roleCfg = &config.RoleConfigFile{
			MetaAgent:    config.MetaAgentConfig{MaxBlocks: 5, SummaryInterval: 3},
			DomainAgent:  config.DomainAgentConfig{},
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

	// 打印角色配置概览
	fmt.Println("=== 角色配置加载完成 ===")
	fmt.Printf("固定角色数量: %d\n", len(roleCfg.FixedRoles))
	for _, r := range roleCfg.FixedRoles {
		fmt.Printf("  - [%s] %s (类型: %s, 生命周期: %s, 模型: %s)\n",
			r.ID, r.Name, r.Type, r.Lifecycle, r.ModelConfig.Model)
	}
	fmt.Printf("MetaAgent 模型: %s\n", roleCfg.MetaAgent.ModelConfig.Model)
	fmt.Printf("DomainAgent 模型: %s\n", roleCfg.DomainAgent.ModelConfig.Model)

	// 2. 初始化模型工厂（按角色缓存 blades ModelProvider）
	modelFactory := model.NewModelFactory(roleCfg)

	// 尝试预热模型（如果配置了API Key则初始化真实模型，否则回退到Mock）
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Printf("Warning: model warmup failed (可能未配置API Key): %v", err)
		log.Println("回退到 MockLLMClient")
	}

	// 3. 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 3. 构建三层图: 用 mock 节点充当升级处理器与终止节点
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

	// 5. 执行: 驱动状态机直至完成
	result, err := threeLayerGraph.Invoke(ctx, state)
	if err != nil {
		log.Fatalf("执行失败: %v", err)
	}

	// 打印最终结果
	fmt.Println("\n=== 会话执行完成 ===")
	fmt.Printf("最终动作: %s\n", result.NextAction)
	fmt.Printf("会话总结: %s\n", result.SessionSummary)
	fmt.Printf("完成的领域: %v\n", result.CompletedBlocks)

	// 6. 打印所有角色实例: 展示运行期动态创建的角色层级
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

// mockThreeLayerNode 模拟三层节点，用于演示中替代真实的升级处理器与终止节点。
type mockThreeLayerNode struct {
	name string // 节点名称
}

// Name 返回节点名称。
func (m *mockThreeLayerNode) Name() string {
	return m.name
}

// Invoke 打印执行日志，并根据节点名称决定 NextAction。
// Sinker 节点强制 Finish，其他节点继续，保证状态机能够收尾。
func (m *mockThreeLayerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	fmt.Printf("  [%s] 执行...\n", m.name)
	if m.name == "Sinker" {
		state.NextAction = types.ActionFinish
	} else {
		state.NextAction = types.ActionContinue
	}
	return state, nil
}
