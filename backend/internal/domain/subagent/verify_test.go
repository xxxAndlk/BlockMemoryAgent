package subagent

// verify_test.go 测试 TODO #43 校验分层 dispatcher 侧：
// resolveVerifyKind 路由、verify_kind 枚举校验、L0 证据扫描（通过/缺证据重试/仍缺 verify_missing）、
// fail-closed unverified 通知、judge 角色解析（交叉模型 + 回退）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// scriptedTextProvider 按调用顺序返回预设文本（耗尽返回 "out of script" 文本）。
type scriptedTextProvider struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (p *scriptedTextProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := "out of script"
	if p.calls < len(p.replies) {
		text = p.replies[p.calls]
	}
	p.calls++
	return &blades.ModelResponse{Message: blades.AssistantMessage(text)}, nil
}
func (p *scriptedTextProvider) Name() string { return "scripted-text" }
func (p *scriptedTextProvider) callsCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// scriptedMsgProvider 按调用顺序返回预设消息（工具调用 + 文本混合；耗尽返回 "out of script"）。
type scriptedMsgProvider struct {
	mu    sync.Mutex
	msgs  []*blades.Message
	calls int
}

func (p *scriptedMsgProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls < len(p.msgs) {
		m := p.msgs[p.calls]
		p.calls++
		return &blades.ModelResponse{Message: m}, nil
	}
	p.calls++
	return &blades.ModelResponse{Message: blades.AssistantMessage("out of script")}, nil
}
func (p *scriptedMsgProvider) Name() string { return "scripted-msg" }

// runCommandToolCall 构造 RunCommand 工具调用消息（command 为验证类命令时 L0 证据可命中）。
func runCommandToolCall(cmd string) *blades.Message {
	raw, _ := json.Marshal(map[string]any{"command": cmd})
	return blades.AssistantMessage(
		blades.TextPart{Text: "运行验证命令。"},
		blades.ToolPart{Name: "RunCommand", Request: string(raw)},
	)
}

// mockRoleModelFactory 按 roleID 返回不同 provider（judge 角色解析测试）；未命中返错。
type mockRoleModelFactory struct {
	byRole map[string]agent.ModelProvider
}

func (f *mockRoleModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	if p, ok := f.byRole[roleID]; ok {
		return p, nil
	}
	return nil, errors.New("no provider for role " + roleID)
}

// newVerifyTestEnv 构造带 code_assistant + prompt_reviewer 固定角色的 Dispatcher 测试环境。
func newVerifyTestEnv(t *testing.T, factory ModelProviderFactory) (*Dispatcher, *mailbox.Mailbox, *tool.Registry) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "通用领域纪律"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
			{ID: "prompt_reviewer", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "review"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, factory, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)
	return d, mb, toolsReg
}

func waitMailboxBody(t *testing.T, mb *mailbox.Mailbox, parentID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := mb.Drain(parentID); len(msgs) > 0 {
			return msgs[0].Body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("parent mailbox got no message")
	return ""
}

// TestResolveVerifyKind 校验分层路由（TODO #43）：显式值优先；auto 按模式/角色推断。
func TestResolveVerifyKind(t *testing.T) {
	cases := []struct {
		verifyKind, roleID, mode, want string
	}{
		{"executable", "domain", "", "executable"},       // 显式优先
		{"rubric", "code_assistant", "react", "rubric"},  // 显式优先
		{"none", "code_assistant", "reflection", "none"}, // 显式优先
		{"", "code_assistant", "reflection", "rubric"},   // auto: reflection → rubric
		{"auto", "code_assistant", "react", "executable"}, // auto: code 角色 → executable
		{"", "test_assistant", "", "executable"},          // auto: 测试角色 → executable
		{"", "code_reviewer", "", "executable"},           // auto: 评审角色 → executable
		{"", "domain", "react", "none"},                   // auto: domain 编排 → none
		{"", "ui_assistant", "", "none"},                  // auto: 其他叶子 → none
		{"", "code_assistant", "plan_execute", "executable"}, // auto: plan_execute 代码角色 → executable
	}
	for _, c := range cases {
		if got := resolveVerifyKind(c.verifyKind, c.roleID, c.mode); got != c.want {
			t.Fatalf("resolveVerifyKind(%q,%q,%q) = %q, want %q", c.verifyKind, c.roleID, c.mode, got, c.want)
		}
	}
}

// TestValidateDispatchArgs_VerifyKindEnum verify_kind 枚举校验：非法值拒绝。
func TestValidateDispatchArgs_VerifyKindEnum(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, agent.NopMemoryPipeline{})
	for _, ok := range []string{"", "auto", "executable", "rubric", "none"} {
		if msg, _ := d.validateDispatchArgs("code_assistant", "t", "", "", ok, ""); msg != "" {
			t.Fatalf("verify_kind %q should pass, got: %s", ok, msg)
		}
	}
	if msg, _ := d.validateDispatchArgs("code_assistant", "t", "", "", "hyper", ""); msg == "" || !strings.Contains(msg, "verify_kind") {
		t.Fatalf("unknown verify_kind should be rejected, got: %q", msg)
	}
}

// TestDispatch_L0ExecutablePasses L0 证据扫描：验证类命令成功执行 → 通过 + 摘要前缀。
func TestDispatch_L0ExecutablePasses(t *testing.T) {
	sp := &scriptedMsgProvider{msgs: []*blades.Message{
		runCommandToolCall("echo verify ok"),
		blades.AssistantMessage("done"),
	}}
	_, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: sp})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "executable",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if body != "【校验:通过(L0 证据)】\ndone" {
		t.Fatalf("expected L0-pass summary, got: %q", body)
	}
}

// TestDispatch_L0MissingEvidence 缺证据 → 重试 1 轮仍缺 → verify_missing 通知父。
func TestDispatch_L0MissingEvidence(t *testing.T) {
	sp := &scriptedTextProvider{replies: []string{"done"}}
	_, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: sp})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "executable",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if !strings.Contains(body, "[failure kind=verify_missing retryable=false]") {
		t.Fatalf("expected verify_missing marker, got: %q", body)
	}
	if !strings.Contains(body, "产出(未验证)") {
		t.Fatalf("expected unverified output attached, got: %q", body)
	}
}

// stubSalvageExtractor 返回固定打捞摘要，用于验证已附产出全文的失败不再追加打捞。
type stubSalvageExtractor struct{}

func (s *stubSalvageExtractor) Extract(ctx context.Context, text, goal, roleID string) ([]string, error) {
	return []string{"SALVAGE-SUMMARY"}, nil
}

// TestDispatch_VerifyMissingSkipsSalvage 回归 08-13 塔防事故：verify_missing 已附产出全文，
// 失败打捞提取器常整段回传同一答案，再追加致正文翻倍。
func TestDispatch_VerifyMissingSkipsSalvage(t *testing.T) {
	sp := &scriptedTextProvider{replies: []string{"done"}}
	d, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: sp})
	d.WithSalvageExtractor(&stubSalvageExtractor{})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "executable",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if !strings.Contains(body, "[failure kind=verify_missing retryable=false]") {
		t.Fatalf("expected verify_missing marker, got: %q", body)
	}
	if strings.Contains(body, "失败打捞") || strings.Contains(body, "SALVAGE-SUMMARY") {
		t.Fatalf("verify_missing 已附产出全文，不应再追加打捞摘要，got: %q", body)
	}
}

// TestDispatch_L0RetryGainsEvidence 缺证据 → 反馈重试轮补上验证命令 → 通过。
func TestDispatch_L0RetryGainsEvidence(t *testing.T) {
	sp := &scriptedMsgProvider{msgs: []*blades.Message{
		blades.AssistantMessage("done"),
		runCommandToolCall("echo verify ok"),
		blades.AssistantMessage("done2"),
	}}
	_, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: sp})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "executable",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if body != "【校验:通过(L0 证据)】\ndone2" {
		t.Fatalf("expected L0-pass after retry, got: %q", body)
	}
}

// TestDispatch_UnverifiedNotifies fail-closed：judge 坏 JSON → unverified 通知父（非静默 pass）。
func TestDispatch_UnverifiedNotifies(t *testing.T) {
	sp := &scriptedTextProvider{replies: []string{"answer"}}
	_, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: sp})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "写文件",
		"mode":    "reflection",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if !strings.Contains(body, "[failure kind=unverified retryable=false]") {
		t.Fatalf("expected unverified marker, got: %q", body)
	}
	if !strings.Contains(body, "answer") {
		t.Fatalf("expected unverified output attached, got: %q", body)
	}
}

// TestJudgeRole_CrossModel 交叉模型 judge：judge 请求打到 prompt_reviewer 的 provider。
func TestJudgeRole_CrossModel(t *testing.T) {
	agentP := &scriptedTextProvider{replies: []string{"answer"}}
	judgeP := &scriptedTextProvider{replies: []string{`{"pass": true, "feedback": ""}`}}
	factory := &mockRoleModelFactory{byRole: map[string]agent.ModelProvider{
		"code_assistant":  agentP,
		"prompt_reviewer": judgeP,
	}}
	d, mb, toolsReg := newVerifyTestEnv(t, factory)
	d.WithJudgeRole("prompt_reviewer")

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "写文件",
		"mode":    "reflection",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if !strings.Contains(body, "【校验:通过(L2 rubric)】") {
		t.Fatalf("expected rubric pass summary, got: %q", body)
	}
	if judgeP.callsCount() != 1 {
		t.Fatalf("judge should hit prompt_reviewer provider once, got %d", judgeP.callsCount())
	}
	if agentP.callsCount() != 1 {
		t.Fatalf("agent provider should only serve the agent run, got %d", agentP.callsCount())
	}
}

// TestJudgeRole_FallbackSameRole judge 角色取不到 → 回退同角色 provider（mock 测试场景不破）。
func TestJudgeRole_FallbackSameRole(t *testing.T) {
	agentP := &scriptedTextProvider{replies: []string{"answer", `{"pass": true, "feedback": ""}`}}
	factory := &mockRoleModelFactory{byRole: map[string]agent.ModelProvider{"code_assistant": agentP}}
	d, mb, toolsReg := newVerifyTestEnv(t, factory)
	d.WithJudgeRole("nonexistent")

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "写文件",
		"mode":    "reflection",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	body := waitMailboxBody(t, mb, "s1")
	if !strings.Contains(body, "【校验:通过(L2 rubric)】") {
		t.Fatalf("expected fallback judge pass, got: %q", body)
	}
	if agentP.callsCount() != 2 {
		t.Fatalf("fallback judge should reuse agent provider (2 calls), got %d", agentP.callsCount())
	}
}
