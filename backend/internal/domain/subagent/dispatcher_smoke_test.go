package subagent

// dispatcher_smoke_test.go 覆盖域完成机器校验（TODO #56）的 dispatcher 集成链路：
// mock 域写两个 js（一语法错）→ 完成摘要含【机器校验】段 / 错文件触发 smoke_failed 打回。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// writeFileToolCall 构造 WriteFile 工具调用消息（子 Agent 实际写盘，冒烟检查目标来源）。
func writeFileToolCall(path, content string) *blades.Message {
	raw, _ := json.Marshal(map[string]any{"path": path, "content": content})
	return blades.AssistantMessage(
		blades.TextPart{Text: "写入文件。"},
		blades.ToolPart{Name: "WriteFile", Request: string(raw)},
	)
}

// newSmokeTestEnv 构造带共享记忆 + 假冒烟执行器的 Dispatcher 测试环境。
func newSmokeTestEnv(t *testing.T, provider agent.ModelProvider, runnerSpec map[string]string) (*Dispatcher, *mailbox.Mailbox, *tool.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "通用领域纪律"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	store := newTestKVMemory(true)
	toolsReg.SetSharedMemory(store)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: provider}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithSharedMemory(store)
	d.smokeRunner = (&fakeSmokeRunner{byFile: runnerSpec}).run
	d.smokeLookPath = fakeLookPath()
	d.RegisterCallTool(toolsReg)
	return d, mb, toolsReg, dir
}

// writeSpecFor 经 WriteSpec 工具写父 Agent spec（files 为待创建文件，验证 FileList 兜底）。
func writeSpecFor(t *testing.T, toolsReg *tool.Registry, parentID string, files []string) {
	t.Helper()
	ctx := agent.WithAgentID(context.Background(), parentID)
	res, err := toolsReg.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "冒烟检查集成测试",
		"acceptance": []any{"文件语法通过"},
		"files":      files,
	})
	if err != nil || !res.Success {
		t.Fatalf("WriteSpec failed: err=%v res=%+v", err, res)
	}
}

// TestDispatcher_SmokeCheckFail 错文件触发冒烟失败：反馈重试 1 轮仍失败 →
// smoke_failed 打回父 Agent（复用 #43 校验分层路由，不判死）。
func TestDispatcher_SmokeCheckFail(t *testing.T) {
	// 脚本：第一轮 WriteFile 写错文件，第二轮终答；重试轮返回纯文本（不改文件）。
	provider := &scriptedMsgProvider{msgs: []*blades.Message{
		blades.AssistantMessage("start"),
		blades.AssistantMessage("done"),
	}}
	d, mb, toolsReg, dir := newSmokeTestEnv(t, provider, map[string]string{"bad.js": "exit:1"})
	bad := filepath.Join(dir, "bad.js")
	provider.msgs[0] = writeFileToolCall(bad, "var x = ;")
	writeSpecFor(t, toolsReg, "meta", []string{bad})

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写 bad.js",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	body := waitMailboxBody(t, mb, "meta")
	if !strings.Contains(body, "[failure kind=smoke_failed retryable=false]") {
		t.Fatalf("expected smoke_failed marker, got: %q", body)
	}
	for _, want := range []string{"机器校验", "bad.js", "node --check"} {
		if !strings.Contains(body, want) {
			t.Fatalf("failure message missing %q: %q", want, body)
		}
	}
	// 冒烟检查应实际执行过（含重试轮共 2 次）。
	if got := d.parentSpecRecordOf("meta"); got == nil || len(got.files) != 1 {
		t.Fatalf("expected captured parent spec files, got %+v", got)
	}
}

// TestDispatcher_SmokeCheckPass 全绿文件：完成摘要含【机器校验】段（dispatcher 执行标注）。
func TestDispatcher_SmokeCheckPass(t *testing.T) {
	provider := &scriptedMsgProvider{msgs: []*blades.Message{
		blades.AssistantMessage("start"),
		blades.AssistantMessage("done"),
	}}
	_, mb, toolsReg, dir := newSmokeTestEnv(t, provider, nil)
	good := filepath.Join(dir, "good.js")
	provider.msgs[0] = writeFileToolCall(good, "var x = 1")
	writeSpecFor(t, toolsReg, "meta", []string{good})

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写 good.js",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	body := waitMailboxBody(t, mb, "meta")
	for _, want := range []string{"done", "【机器校验】", "非 agent 自述", "通过"} {
		if !strings.Contains(body, want) {
			t.Fatalf("summary missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "smoke_failed") {
		t.Fatalf("pass branch must not contain failure marker: %q", body)
	}
}

// TestDispatcher_SmokeCheckSkipOutsideSpec 子 Agent 写入文件不在 spec.files 范围：
// 冒烟检查跳过（责任归属收敛），摘要无【机器校验】段。
func TestDispatcher_SmokeCheckSkipOutsideSpec(t *testing.T) {
	provider := &scriptedMsgProvider{msgs: []*blades.Message{
		blades.AssistantMessage("start"),
		blades.AssistantMessage("done"),
	}}
	_, mb, toolsReg, dir := newSmokeTestEnv(t, provider, map[string]string{"other.js": "exit:1"})
	outside := filepath.Join(dir, "other.js")
	provider.msgs[0] = writeFileToolCall(outside, "var x = 1")
	writeSpecFor(t, toolsReg, "meta", []string{filepath.Join(dir, "planned.js")})

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写 other.js",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	body := waitMailboxBody(t, mb, "meta")
	if strings.Contains(body, "【机器校验】") || strings.Contains(body, "smoke_failed") {
		t.Fatalf("out-of-spec writes must skip smoke checks, got: %q", body)
	}
}
