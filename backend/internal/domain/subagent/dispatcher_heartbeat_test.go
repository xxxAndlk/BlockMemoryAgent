package subagent

// dispatcher_heartbeat_test.go 验证心跳巡检主动 kill 假死叶子子 Agent。
import (
	"context"
	"strings"
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

// hangingProvider 模拟 LLM 流式挂起：Generate 阻塞至 release 关闭，忽略 ctx 取消（假死场景）。
// 触发心跳巡检主动 cancel + 兜底 trackChildDone，验证父 Agent 不必空等 sub_agent_timeout。
type hangingProvider struct {
	release chan struct{}
}

func (h *hangingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-h.release
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}
func (h *hangingProvider) Name() string { return "hanging" }

// TestDispatcher_HeartbeatKillsStuckLeaf 验证叶子 Agent 假死（Generate 无返回）时，
// 心跳巡检主动 cancel + notify 父 + 兜底 trackChildDone：父 PendingChildren 归 0 且邮箱收到卡死通知。
func TestDispatcher_HeartbeatKillsStuckLeaf(t *testing.T) {
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
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 测试结束释放悬挂 goroutine，防泄漏

	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithHeartbeatTimeout(200 * time.Millisecond)
	d.RegisterCallTool(toolsReg)
	t.Cleanup(d.ClosePatrol)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "hang",
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected dispatch success, got: %s", res.Error)
	}

	// 等待巡检 kill：父 PendingChildren 应归 0（doneOnce 兜底递减）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.PendingChildren("meta") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.PendingChildren("meta") != 0 {
		t.Fatalf("expected PendingChildren==0 after heartbeat kill, got %d", d.PendingChildren("meta"))
	}

	// 校验邮箱收到卡死通知。
	msgs := mb.Drain("meta")
	if len(msgs) == 0 {
		t.Fatal("expected stuck notify in mailbox, got none")
	}
	if !strings.Contains(msgs[0].Body, "假死") && !strings.Contains(msgs[0].Body, "无活动") {
		t.Fatalf("expected stuck notify body, got: %s", msgs[0].Body)
	}
}

// TestDispatcher_HeartbeatNoPatrolWhenDisabled 验证 heartbeatTimeout<=0 时不启动巡检：
// 假死叶子 Agent 不会被 kill，父 PendingChildren 保持 >0（回归保护：默认关闭不误杀）。
func TestDispatcher_HeartbeatNoPatrolWhenDisabled(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles:  []types.RoleDefinition{{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	// heartbeatTimeout=0 -> 巡检关闭。
	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: release}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{"role_id": "code_assistant", "task": "hang"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// 等待足够巡检周期（若有 bug 启动巡检，此时应已 kill）。
	time.Sleep(300 * time.Millisecond)
	if d.PendingChildren("meta") == 0 {
		t.Fatal("expected PendingChildren>0 when heartbeat disabled (no patrol), got 0")
	}
}
