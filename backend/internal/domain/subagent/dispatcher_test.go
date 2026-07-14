package subagent

// 导入测试与项目依赖包。
import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// mockProvider 是一个模拟的模型提供者，每次 Generate 调用都返回固定的 assistant 消息。
type mockProvider struct {
	// text 是 Generate 返回的固定文本内容。
	text string
}

// Generate 实现 agent.ModelProvider 接口，返回包含固定文本的模型响应。
// 参数 ctx 为调用上下文；req 为模型请求，本模拟实现忽略请求内容。
func (m *mockProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 直接返回固定 assistant 消息，不依赖输入请求。
	return &blades.ModelResponse{Message: blades.AssistantMessage(m.text)}, nil
}

// Name 返回模拟提供者的名称标识。
func (m *mockProvider) Name() string { return "mock" }

// mockModelFactory 是一个模拟的模型工厂，总是返回同一个 mockProvider。
type mockModelFactory struct {
	// provider 是工厂内部持有的固定提供者实例。
	provider *mockProvider
}

// GetBladesProvider 实现 ModelProviderFactory 接口，忽略 roleID 并返回固定提供者。
func (f *mockModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	// 无论请求哪个角色，都返回工厂中预设的 mockProvider。
	return f.provider, nil
}

// TestDispatcher_RegisterAndCall 验证 Dispatcher 注册 call_sub_agent 工具后，
// 可以通过工具调度成功创建并运行子 Agent，最终收到子 Agent 完成的邮箱通知。
func TestDispatcher_RegisterAndCall(t *testing.T) {
	// 构造角色配置：包含 MetaAgent、DomainAgent 与一个可调用的固定角色 code_assistant。
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	// 创建角色注册表、工具注册表与邮箱。
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	// 创建 Dispatcher 并将 call_sub_agent 工具注册到工具注册表。
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	// 构造携带父 Agent ID 的上下文，模拟 meta 代理调用子代理。
	ctx := agent.WithAgentID(context.Background(), "meta")
	// 调度 call_sub_agent 工具，请求 code_assistant 角色执行任务。
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "write tests",
	})
	// 校验调度未返回错误。
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	// 校验工具调用成功。
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	// 校验返回的子 Agent ID 非空。
	if res.Output == "" {
		t.Fatal("expected non-empty sub-agent id")
	}

	// 等待异步运行的子 Agent 完成，并通过邮箱向父 Agent 投递摘要。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		//  drain 父 Agent "meta" 邮箱中的消息。
		msgs := mb.Drain("meta")
		// 若收到消息，校验第一条消息正文为 "done"。
		if len(msgs) > 0 {
			if msgs[0].Body != "done" {
				t.Fatalf("expected summary 'done', got %q", msgs[0].Body)
			}
			// 校验通过，结束测试。
			return
		}
		// 未收到消息则短暂等待后重试。
		time.Sleep(10 * time.Millisecond)
	}
	// 超过截止时间仍未收到消息，测试失败。
	t.Fatal("timed out waiting for sub-agent mailbox message")
}

// TestDispatcher_CannotCallUncallable 验证当固定角色未声明 CanBeCalled 时，
// Dispatcher 应拒绝调用并返回失败结果。
func TestDispatcher_CannotCallUncallable(t *testing.T) {
	// 构造一个固定角色 code_assistant，但显式设置为不可调用。
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: false},
		},
	}
	// 创建依赖对象，邮箱与记忆管道在此测试中不需要实际功能。
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, nil, nil)
	// 注册 call_sub_agent 工具。
	d.RegisterCallTool(toolsReg)

	// 构造携带父 Agent ID 的上下文。
	ctx := agent.WithAgentID(context.Background(), "meta")
	// 尝试调用不可调用的角色。
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "write tests",
	})
	// 期望 err 非 nil 或 res.Success 为 false，否则说明权限校验失效。
	if err == nil && res.Success {
		t.Fatal("expected failure for non-callable role")
	}
}
