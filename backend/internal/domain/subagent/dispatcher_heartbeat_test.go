package subagent

// dispatcher_heartbeat_test.go 验证心跳巡检主动 kill 假死叶子子 Agent。
import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
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

// TestDispatcher_PingActivityPreventsKill 验证等待用户答复期间的保活探针（PingActivity）：
// 活动时间被持续刷新时巡检不判假死；停止 ping 后超阈值才 kill。
// 回归场景：子 Agent 阻塞在审批/提问等用户答复，期间无 LLM/工具活动（2026-08-18
// 三次误杀均卡在此状态），会话层周期性 PingActivity 保活。
func TestDispatcher_PingActivityPreventsKill(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles:  []types.RoleDefinition{{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()

	d := NewDispatcher(reg, &mockModelFactory{provider: &hangingProvider{release: make(chan struct{})}}, toolsReg, mb, agent.NopMemoryPipeline{}).
		WithHeartbeatTimeout(150 * time.Millisecond)
	d.ensurePatrol()
	t.Cleanup(d.ClosePatrol)

	id := "sess-1/code_assistant-1"
	killed := make(chan struct{})
	var cancelOnce sync.Once
	d.activity.Store(id, new(atomic.Int64))
	d.subMeta.Store(id, &subAgentMeta{
		parentID:  "meta",
		sessionID: "sess-1",
		cancel:    func() { cancelOnce.Do(func() { close(killed) }) },
	})

	// 模拟保活：每 50ms ping 一次，持续 400ms（> 2× 阈值），期间不得被杀。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-killed:
			t.Fatal("保活期间不应被心跳 kill")
		default:
		}
		d.PingActivity(id)
		time.Sleep(50 * time.Millisecond)
	}

	// 停止保活：超阈值后巡检应 kill。
	select {
	case <-killed:
		// 预期：停止 ping 后被巡检判定假死。
	case <-time.After(3 * time.Second):
		t.Fatal("停止保活后应被心跳 kill")
	}
}
