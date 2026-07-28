package subagent

// 导入测试与项目依赖包。
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// mockModelFactory 是一个模拟的模型工厂，总是返回同一个 provider。
type mockModelFactory struct {
	// provider 是工厂内部持有的固定提供者实例。
	provider agent.ModelProvider
}

// GetBladesProvider 实现 ModelProviderFactory 接口，忽略 roleID 并返回固定提供者。
func (f *mockModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	// 无论请求哪个角色，都返回工厂中预设的 provider。
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

// mockBlockMemorySearcher 是一个模拟的块记忆检索器，返回固定记录或错误。
type mockBlockMemorySearcher struct {
	// recs 是 SearchBlockMemoryByGoal 返回的固定记录切片。
	recs []*types.KnowledgeRecord
	// err 是 SearchBlockMemoryByGoal 返回的固定错误。
	err error
	// lastSession 记录最近一次调用传入的 sessionID，测试可断言过滤行为。
	lastSession string
}

// SearchBlockMemoryByGoal 实现 BlockMemorySearcher 接口，忽略查询并返回预设结果。
func (m *mockBlockMemorySearcher) SearchBlockMemoryByGoal(ctx context.Context, sessionID, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	m.lastSession = sessionID
	return m.recs, m.err
}

// TestInjectRecalledMemory 验证块记忆召回注入的三种情形：
// 命中时拼接【相关记忆】前缀、无命中与未配置检索器时任务原样返回。
func TestInjectRecalledMemory(t *testing.T) {
	// 情形一：命中两条记忆，任务前应拼入编号记忆段与【当前任务】分隔。
	mock := &mockBlockMemorySearcher{recs: []*types.KnowledgeRecord{
		{Content: "记忆一"},
		{Content: "记忆二"},
	}}
	d := NewDispatcher(nil, nil, nil, nil, nil).
		WithBlockMemorySearcher(mock)
	// 注入 sessionID 到 ctx，验证召回侧按 session 过滤。
	ctx := tool.WithSessionID(context.Background(), "session-42")
	got := d.injectRecalledMemory(ctx, "原始任务")
	want := "【相关记忆】\n1. 记忆一\n2. 记忆二\n\n【当前任务】\n原始任务"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if mock.lastSession != "session-42" {
		t.Fatalf("expected sessionID passed through to searcher, got %q", mock.lastSession)
	}

	// 情形二：检索无命中，任务原样返回。
	d = NewDispatcher(nil, nil, nil, nil, nil).
		WithBlockMemorySearcher(&mockBlockMemorySearcher{})
	if got := d.injectRecalledMemory(context.Background(), "原始任务"); got != "原始任务" {
		t.Fatalf("expected unchanged task, got %q", got)
	}

	// 情形三：未配置检索器，任务原样返回。
	d = NewDispatcher(nil, nil, nil, nil, nil)
	if got := d.injectRecalledMemory(context.Background(), "原始任务"); got != "原始任务" {
		t.Fatalf("expected unchanged task, got %q", got)
	}
}

// TestInjectRecalledMemory_PurePrefixMode 验证 task="" 时返回纯前缀（不含【当前任务】）。
// 对应 ui_assistant-5 任务丢失事故：双前缀注入嵌套【当前任务】使模型把 KV 内容当任务主体。
func TestInjectRecalledMemory_PurePrefixMode(t *testing.T) {
	mock := &mockBlockMemorySearcher{recs: []*types.KnowledgeRecord{{Content: "记忆一"}}}
	d := NewDispatcher(nil, nil, nil, nil, nil).WithBlockMemorySearcher(mock)
	got := d.injectRecalledMemory(context.Background(), "")
	if strings.Contains(got, "【当前任务】") {
		t.Fatalf("pure prefix mode should not include 【当前任务】 marker, got: %q", got)
	}
	if !strings.Contains(got, "【相关记忆】") {
		t.Fatalf("expected 【相关记忆】 prefix, got: %q", got)
	}
}

// TestAssembleTaskWithDualPrefixes_NoNestedMarker 验证 KV + block memory 双前缀拼装后
// 只含一个【当前任务】标记，原始任务位于末尾。回归 ui_assistant-5 嵌套事故。
func TestAssembleTaskWithDualPrefixes_NoNestedMarker(t *testing.T) {
	kv := newTestKVMemory(true)
	_ = kv.Set(context.Background(), "meta:shared", "KV 内容")
	mock := &mockBlockMemorySearcher{recs: []*types.KnowledgeRecord{{Content: "记忆一"}}}
	d := NewDispatcher(nil, nil, nil, nil, nil).
		WithKVMemory(kv).
		WithBlockMemorySearcher(mock)

	// 模拟 runSubAgentOnce 中的拼装逻辑（直接调用两函数的纯前缀模式）。
	var prefixes []string
	if p := d.injectKVMemory(context.Background(), "meta", ""); p != "" {
		prefixes = append(prefixes, p)
	}
	if p := d.injectRecalledMemory(context.Background(), ""); p != "" {
		prefixes = append(prefixes, p)
	}
	task := strings.Join(prefixes, "\n\n") + "\n\n【当前任务】\n" + "原任务"

	// 应只出现一次【当前任务】。
	if c := strings.Count(task, "【当前任务】"); c != 1 {
		t.Fatalf("expected exactly 1 【当前任务】 marker, got %d: %q", c, task)
	}
	// 原任务应位于末尾，KV 内容与相关记忆在其前。
	if !strings.HasPrefix(task, "【共享记忆】") {
		t.Fatalf("expected KV prefix at start, got: %q", task)
	}
	if !strings.Contains(task, "【相关记忆】") {
		t.Fatalf("expected block-memory prefix, got: %q", task)
	}
	if !strings.HasSuffix(task, "原任务") {
		t.Fatalf("expected original task at end, got: %q", task)
	}
}

// TestRoleIDFromAgentID 验证从 Agent 句柄还原角色 ID：
// 子 Agent 句柄去序号；顶层会话 ID（session-N）恒还原为 meta；
// 角色 ID 含连字符时必须保留完整角色名（仅去掉末尾序号）。
func TestRoleIDFromAgentID(t *testing.T) {
	cases := map[string]string{
		"session-1":                  "meta",
		"session-42":                 "meta",
		"meta":                       "meta",
		"session-1/code_assistant-3": "code_assistant",
		"session-2/domain-1":         "domain",
		"session-2/domain-1/code-4":  "code",
		"session-2/my_role-7":        "my_role",
		// 含连字符角色 ID：必须按最后一个 '-' 截断序号，保留完整角色名。
		"session-3/code-assistant-5":              "code-assistant",
		"session-3/search-agent-12":               "search-agent",
		"session-3/domain-1/code-review-bot-2":    "code-review-bot",
		"session-3/search-agent-1/worker-9":       "worker",
	}
	for in, want := range cases {
		if got := roleIDFromAgentID(in); got != want {
			t.Fatalf("roleIDFromAgentID(%q) = %q, want %q", in, got, want)
		}
	}
}

// mockBlockMemorySaver 是一个模拟的块记忆写入器，记录每次 Save 调用并可返回预设错误。
type mockBlockMemorySaver struct {
	// mu 保护 recs，Save 在子 Agent goroutine 中执行，断言在测试 goroutine 中执行。
	mu sync.Mutex
	// recs 依次记录每次 Save 收到的知识记录。
	recs []*types.KnowledgeRecord
	// err 是 Save 返回的预设错误。
	err error
}

// Save 实现 BlockMemorySaver 接口，记录写入内容并返回预设错误。
func (m *mockBlockMemorySaver) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, rec)
	return m.err
}

// savedCount 返回已记录的写入条数（线程安全）。
func (m *mockBlockMemorySaver) savedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

// lastSaved 返回最后一条写入记录；无写入时返回 nil。
func (m *mockBlockMemorySaver) lastSaved() *types.KnowledgeRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recs) == 0 {
		return nil
	}
	return m.recs[len(m.recs)-1]
}

// blockingSaver 是一个阻塞式块记忆写入器：Save 进入后先关闭 entered 通知调用方，
// 再阻塞直至 release 被关闭，用于精确控制 runSubAgent 的执行时序。
type blockingSaver struct {
	// entered 在 Save 首次被调用时关闭（一次性信号）。
	entered chan struct{}
	// release 关闭后 Save 才返回。
	release chan struct{}
}

// Save 实现 BlockMemorySaver 接口，阻塞至 release 通道关闭。
func (m *blockingSaver) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	close(m.entered)
	<-m.release
	return nil
}

// newWriteTestEnv 构造可派发 code_assistant 子 Agent 的测试环境：
// 返回工具注册表与邮箱，并把 saver/enabled 注入 Dispatcher 的块记忆写入闭环。
func newWriteTestEnv(t *testing.T, saver BlockMemorySaver, enabled bool) (*tool.Registry, *mailbox.Mailbox) {
	t.Helper()
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
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithBlockMemorySaver(saver, enabled)
	d.RegisterCallTool(toolsReg)
	return toolsReg, mb
}

// dispatchCodeAssistant 以 meta 身份派发一个 code_assistant 子 Agent，返回其句柄。
func dispatchCodeAssistant(t *testing.T, toolsReg *tool.Registry) string {
	t.Helper()
	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "meta"), "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "write tests",
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	return res.Output
}

// waitForCond 轮询等待条件满足，超时则测试失败。
func waitForCond(t *testing.T, name string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}

// TestRunSubAgent_SavesBlockMemory 验证块记忆写入闭环：
// 开关开启时子 Agent 成功完成后结果摘要被沉淀到块记忆知识库；
// 开关关闭时不写入；写入失败时父 Agent 仍能收到完成通知。
func TestRunSubAgent_SavesBlockMemory(t *testing.T) {
	// 情形一：开关开启，子 Agent 成功后写入一条 block_memory 记录，
	// 内容应包含目标与结果摘要，Meta 携带 goal/domain 标签。
	saver := &mockBlockMemorySaver{}
	toolsReg, _ := newWriteTestEnv(t, saver, true)
	dispatchCodeAssistant(t, toolsReg)
	waitForCond(t, "block memory save", func() bool { return saver.savedCount() > 0 })
	rec := saver.lastSaved()
	if rec.KnowledgeType != enums.KnowledgeTypeBlockMemory {
		t.Fatalf("expected knowledge type %q, got %q", enums.KnowledgeTypeBlockMemory, rec.KnowledgeType)
	}
	if !strings.Contains(rec.Content, "write tests") || !strings.Contains(rec.Content, "done") {
		t.Fatalf("expected content to contain goal and result, got %q", rec.Content)
	}
	if rec.Meta["goal"] != "write tests" {
		t.Fatalf("expected meta goal=write tests, got %v", rec.Meta["goal"])
	}
	if rec.Meta["domain"] != "code_assistant" {
		t.Fatalf("expected meta domain=code_assistant, got %v", rec.Meta["domain"])
	}

	// 情形二：开关关闭，子 Agent 完成后不应产生任何写入。
	saver2 := &mockBlockMemorySaver{}
	toolsReg2, mb2 := newWriteTestEnv(t, saver2, false)
	dispatchCodeAssistant(t, toolsReg2)
	// 先等父邮箱收到完成通知，确保 runSubAgent 已走完成功路径。
	waitForCond(t, "parent notify", func() bool { return len(mb2.Drain("meta")) > 0 })
	if saver2.savedCount() != 0 {
		t.Fatalf("expected no block memory save when disabled, got %d", saver2.savedCount())
	}

	// 情形三：写入失败仅记日志，不影响主流程——父 Agent 仍应收到完成通知。
	saver3 := &mockBlockMemorySaver{err: errors.New("db down")}
	toolsReg3, mb3 := newWriteTestEnv(t, saver3, true)
	dispatchCodeAssistant(t, toolsReg3)
	waitForCond(t, "block memory save attempt", func() bool { return saver3.savedCount() > 0 })
	waitForCond(t, "parent notify after save failure", func() bool { return len(mb3.Drain("meta")) > 0 })
}

// TestRunSubAgent_PurgesMailbox 验证子 Agent 结束后其收件箱被 Purge 清理：
// 利用阻塞式 saver 将 runSubAgent 停在"Run 已结束、defer 未执行"的时刻，
// 此时投递的未读消息必须随 defer 中的 Purge 一并被清除。
func TestRunSubAgent_PurgesMailbox(t *testing.T) {
	saver := &blockingSaver{entered: make(chan struct{}), release: make(chan struct{})}
	toolsReg, mb := newWriteTestEnv(t, saver, true)
	subAgentID := dispatchCodeAssistant(t, toolsReg)

	// 等待 Save 被调用：此时子 Agent Run 已成功结束，不会再 Drain 自己的收件箱，
	// 而 runSubAgent 尚未返回（defer 中的 Purge 尚未执行）。
	select {
	case <-saver.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for block memory save")
	}

	// 向子 Agent 收件箱投递一条未读消息。
	mb.Send(&mailbox.Message{From: "meta", To: subAgentID, Type: mailbox.MsgInfo, Subject: "late"})
	if mb.Count(subAgentID) != 1 {
		t.Fatalf("expected 1 unread message before purge, got %d", mb.Count(subAgentID))
	}

	// 放行 Save，runSubAgent 返回并触发 defer 中的 Purge。
	close(saver.release)
	// Purge 删除整个 inbox 键，该未读消息随之被清理；若未调用 Purge，消息将残留导致超时。
	waitForCond(t, "mailbox purge", func() bool { return mb.Count(subAgentID) == 0 })
}

// TestSendMessageTool 验证 send_message 工具能向目标 Agent 邮箱投递消息，
// 消息携带 ReplyTo=发送方 与调用方指定的 message_type/thread_id。
func TestSendMessageTool(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "x"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterMessagingTool(toolsReg)

	// 以 caller=meta 身份执行 send_message。
	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "meta"), "send_message", map[string]any{
		"to_agent_id":  "session-1/test_assistant-2",
		"subject":      "please verify",
		"body":         "run go test ./...",
		"message_type": "request",
		"thread_id":    "verify-1",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}

	// 校验目标收件箱收到消息，字段保留完整。
	msgs := mb.Drain("session-1/test_assistant-2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in target inbox, got %d", len(msgs))
	}
	m := msgs[0]
	if m.From != "meta" {
		t.Fatalf("From mismatch: got %q", m.From)
	}
	if m.ReplyTo != "meta" {
		t.Fatalf("ReplyTo mismatch: got %q", m.ReplyTo)
	}
	if m.ThreadID != "verify-1" {
		t.Fatalf("ThreadID mismatch: got %q", m.ThreadID)
	}
	if m.Type != mailbox.MsgRequest {
		t.Fatalf("Type mismatch: got %q", m.Type)
	}
}

// TestSendMessageTool_MissingArgs 验证 send_message 工具在缺少必填参数时返回错误。
func TestSendMessageTool_MissingArgs(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "x"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterMessagingTool(toolsReg)

	// 缺少 subject。
	res, _ := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "meta"), "send_message", map[string]any{
		"to_agent_id": "x",
	})
	if res.Success {
		t.Fatal("expected error when subject missing")
	}
	if !strings.Contains(res.Error, "to_agent_id and subject are required") {
		t.Fatalf("unexpected error: %s", res.Error)
	}
}

// TestRegistry_CanCall_PeerPermissions 验证 roles.yaml 中 parents 白名单
// 允许 code_assistant <-> test_assistant 平级互调，支撑验证闭环。
func TestRegistry_CanCall_PeerPermissions(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"test_assistant"}},
			{ID: "test_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}},
		},
	}
	reg := role.NewRegistry(cfg)

	// code_assistant 可调用 test_assistant（平级验证闭环正向）。
	if !reg.CanCall("code_assistant", "test_assistant") {
		t.Fatal("code_assistant should be able to call test_assistant")
	}
	// test_assistant 可调用 code_assistant（平级验证闭环反向：打回修正）。
	if !reg.CanCall("test_assistant", "code_assistant") {
		t.Fatal("test_assistant should be able to call code_assistant")
	}
	// 未在 parents 中的角色不可调用。
	if reg.CanCall("code_assistant", "ui_assistant") {
		t.Fatal("code_assistant should NOT be able to call ui_assistant (not in parents)")
	}
}

// TestDispatcher_PendingChildren 验证父会话终结保护的未决子 Agent 计数：
// 派发时递增、子 Agent 结束时递减，并发出完成信号唤醒 WaitForAnyChild。
func TestDispatcher_PendingChildren(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	// 阻塞式 provider：使子 Agent Run 不立即返回，保证计数窗口可观测。
	saver := &blockingSaver{entered: make(chan struct{}), release: make(chan struct{})}
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "ok"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithBlockMemorySaver(saver, true)
	d.RegisterCallTool(toolsReg)

	if d.PendingChildren("meta") != 0 {
		t.Fatal("initial pending should be 0")
	}

	// 派发一个子 Agent，但用 saver 阻塞在 runSubAgent 末尾，使计数保持 > 0。
	dispatchCodeAssistant(t, toolsReg)

	// 等待 saver 被触发：此时子 Agent 已完成 Run，但 runSubAgent 仍阻塞在 saveBlockMemory。
	select {
	case <-saver.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("saver not entered")
	}

	// 子 Agent Run 已结束，但 runSubAgent 未返回（defer trackChildDone 未执行），计数应为 1。
	if got := d.PendingChildren("meta"); got != 1 {
		t.Fatalf("expected 1 pending child, got %d", got)
	}

	// 在另一 goroutine 中等待任一子 Agent 完成。
	waitDone := make(chan bool, 1)
	go func() { waitDone <- d.WaitForAnyChild("meta", 2*time.Second) }()

	// 放行 saver，runSubAgent 返回，defer trackChildDone 递减计数并发出信号。
	close(saver.release)

	select {
	case ok := <-waitDone:
		if !ok {
			t.Fatal("WaitForAnyChild should return true after child done")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForAnyChild did not return after child completed")
	}

	// 最终计数应归 0。
	waitForCond(t, "pending to reach 0", func() bool { return d.PendingChildren("meta") == 0 })
}

// TestDispatcher_MaxTotalDispatches 验证全局派发总数上限：
// 同一 session 内所有角色派发合计超过 maxTotalDispatches 时被拒绝；
// ResetDispatchCounts（用户新消息）后计数清零可继续派发。
func TestDispatcher_MaxTotalDispatches(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"test_assistant"}, SystemPrompt: "code"},
			{ID: "test_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}, SystemPrompt: "test"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "ok"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	// 设置总数上限为 2：跨角色合计第 3 次派发应被拒绝。
	d.WithMaxTotalDispatches(2)
	d.RegisterCallTool(toolsReg)

	// 以 meta 身份（顶层，agentID 即 sessionID）派发：限额按 session 合计，与角色对无关。
	ctx := agent.WithAgentID(context.Background(), "session-1")
	targets := []string{"code_assistant", "test_assistant"}
	for i, roleID := range targets {
		res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
			"role_id": roleID,
			"task":    "job",
		})
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if !res.Success {
			t.Fatalf("dispatch %d should succeed: %s", i, res.Error)
		}
	}
	// 第 3 次（不同角色）应被总数限额拒绝。
	res, _ := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "job",
	})
	if res.Success {
		t.Fatal("3rd dispatch should be rejected by dispatch total limit")
	}
	if !strings.Contains(res.Error, "dispatch total limit reached") {
		t.Fatalf("unexpected error: %s", res.Error)
	}

	// 用户新消息触发重置后，可继续派发。
	d.ResetDispatchCounts("session-1")
	res, _ = toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "job",
	})
	if !res.Success {
		t.Fatalf("dispatch after reset should succeed: %s", res.Error)
	}
}

// TestDispatcher_KVMemoryInjection 验证 KV 共享记忆注入：子 Agent 派发时读取主 Agent
// 写入的共享记忆并拼到任务前，使被询问协程能看到主 Agent 的关键上下文。
func TestDispatcher_KVMemoryInjection(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})

	// 主 Agent 写入共享记忆（可写实例）。
	kv := newTestKVMemory(true)
	_ = kv.Set(context.Background(), "meta:shared", "project uses Go 1.25")
	// Dispatcher 注入只读视图。
	d.WithKVMemory(kv)

	// 验证只读实例拒绝写入。
	if err := newTestKVMemory(false).Set(context.Background(), "k", "v"); err == nil {
		t.Fatal("read-only KV should reject Set")
	}

	// ExecuteChild 注入 KV 记忆后执行子 Agent，不 panic 即通过。
	_, err := d.ExecuteChild(context.Background(), "meta", "code_assistant", "write a function")
	if err != nil {
		t.Fatalf("ExecuteChild: %v", err)
	}
}

// testKVMemory 是测试用的 KVMemory 实现，支持可写/只读。
type testKVMemory struct {
	items    map[string]string
	writable bool
}

func newTestKVMemory(writable bool) *testKVMemory {
	return &testKVMemory{items: make(map[string]string), writable: writable}
}

func (m *testKVMemory) Get(ctx context.Context, key string) (string, error) {
	return m.items[key], nil
}

func (m *testKVMemory) Set(ctx context.Context, key, value string) error {
	if !m.writable {
		return errors.New("read-only")
	}
	m.items[key] = value
	return nil
}

func (m *testKVMemory) Delete(ctx context.Context, key string) error {
	if !m.writable {
		return errors.New("read-only")
	}
	delete(m.items, key)
	return nil
}

// Keys 返回当前内存中所有键的快照，实现 KVMemoryReader.Keys。
func (m *testKVMemory) Keys(ctx context.Context) []string {
	keys := make([]string, 0, len(m.items))
	for k := range m.items {
		keys = append(keys, k)
	}
	return keys
}

// TestInjectKVMemory_Layer3StaleDetection 验证 Layer 3：KV value 为 SharedEntry JSON 时，
// injectKVMemory 解析后 stat 各 path mtime，不匹配（文件被改）丢弃 KV 降级 fresh read。
// 旧格式（纯字符串）直接用，向后兼容。
func TestInjectKVMemory_Layer3StaleDetection(t *testing.T) {
	// 准备临时文件并记录初始 mtime。
	dir := t.TempDir()
	target := filepath.Join(dir, "stale.go")
	if err := os.WriteFile(target, []byte("v1"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, _ := os.Stat(target)
	origMtime := fi.ModTime().Unix()

	// 构造 dispatcher（不需要真实 provider，只测 injectKVMemory 纯函数行为）。
	d := &Dispatcher{}

	// Case 1: SharedEntry JSON，mtime 匹配 -> 拼接 Content 前缀。
	kv := newTestKVMemory(true)
	entry := sharedEntryMirror{
		Files:   map[string]int64{target: origMtime},
		Content: "stale.go is v1",
	}
	val, _ := json.Marshal(entry)
	_ = kv.Set(context.Background(), "meta:shared", string(val))
	d.WithKVMemory(kv)

	got := d.injectKVMemory(context.Background(), "meta", "do task")
	if !strings.Contains(got, "stale.go is v1") {
		t.Fatalf("expected KV content injected, got: %q", got)
	}
	if !strings.Contains(got, "do task") {
		t.Fatalf("expected task preserved, got: %q", got)
	}

	// Case 2: 文件被改，mtime 不匹配 -> 丢弃 KV，降级 fresh read。
	// 改 mtime：写新内容并 SetModTime 确保 mtime 变化。
	if err := os.WriteFile(target, []byte("v2"), 0644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	// 强制 mtime 与 origMtime 不同（避免同秒写入 mtime 未变）。
	newTime := time.Now().Add(5 * time.Second)
	_ = os.Chtimes(target, newTime, newTime)

	got2 := d.injectKVMemory(context.Background(), "meta", "do task")
	if strings.Contains(got2, "stale.go is v1") {
		t.Fatalf("expected stale KV discarded, got: %q", got2)
	}
	if !strings.Contains(got2, "do task") {
		t.Fatalf("expected task preserved after stale discard, got: %q", got2)
	}
	if strings.Contains(got2, "【共享记忆】") {
		t.Fatalf("expected no shared memory prefix for stale entry, got: %q", got2)
	}

	// Case 3: 旧格式（纯字符串）-> 直接用，不校验 mtime。
	kvOld := newTestKVMemory(true)
	_ = kvOld.Set(context.Background(), "meta:shared", "legacy plain summary")
	dOld := &Dispatcher{}
	dOld.WithKVMemory(kvOld)

	got3 := dOld.injectKVMemory(context.Background(), "meta", "do task")
	if !strings.Contains(got3, "legacy plain summary") {
		t.Fatalf("expected legacy content used as-is, got: %q", got3)
	}
}

// TestInjectKVMemory_NoFilesSkipsStatCheck 验证 Layer 3 边界：SharedEntry.Files 为空时
// 跳过 stat 校验（无 path 需校验），Content 直接拼接。
func TestInjectKVMemory_NoFilesSkipsStatCheck(t *testing.T) {
	d := &Dispatcher{}
	kv := newTestKVMemory(true)
	entry := sharedEntryMirror{Content: "pure conclusion no files"}
	val, _ := json.Marshal(entry)
	_ = kv.Set(context.Background(), "meta:shared", string(val))
	d.WithKVMemory(kv)

	got := d.injectKVMemory(context.Background(), "meta", "do task")
	if !strings.Contains(got, "pure conclusion no files") {
		t.Fatalf("expected content injected when Files empty, got: %q", got)
	}
}

// captureProvider 捕获最后一次 Generate 收到的请求，用于断言系统提示词与任务文本。
type captureProvider struct {
	lastReq *blades.ModelRequest
}

func (m *captureProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.lastReq = req
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

func (m *captureProvider) Name() string { return "capture" }

// TestDispatcher_DomainResponsibilityInjection 验证 role_id="domain" 且携带
// domain+responsibility 时：身份头注入系统提示词（含原通用领域纪律），
// 任务文本带【你的领域】前缀；固定助手不受影响。
func TestDispatcher_DomainResponsibilityInjection(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
			SystemPrompt: "通用领域纪律",
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	cp := &captureProvider{}
	d := NewDispatcher(reg, &mockModelFactory{provider: cp}, toolsReg, nil, agent.NopMemoryPipeline{})

	roleDef := reg.Get("domain")
	if roleDef == nil {
		t.Fatal("domain role not registered")
	}
	_, _, err := d.runSubAgentOnce(context.Background(), "meta", "meta/domain-1", *roleDef,
		"实现 config.js 数值表", "配置", "负责 config.js/index.html/css；禁止碰 js/engine 下文件")
	if err != nil {
		t.Fatalf("runSubAgentOnce: %v", err)
	}
	if cp.lastReq == nil {
		t.Fatal("provider not called")
	}
	// 系统提示词：身份头 + 职责 + 原通用纪律。
	instr := cp.lastReq.Instruction.Text()
	if !strings.Contains(instr, "你是负责【配置】领域的 DomainAgent") {
		t.Fatalf("system prompt missing identity header: %q", instr)
	}
	if !strings.Contains(instr, "负责 config.js/index.html/css") {
		t.Fatalf("system prompt missing responsibility: %q", instr)
	}
	if !strings.Contains(instr, "通用领域纪律") {
		t.Fatalf("system prompt lost base template: %q", instr)
	}
	// 任务文本：带【你的领域】前缀。
	if len(cp.lastReq.Messages) == 0 {
		t.Fatal("no messages in request")
	}
	first := cp.lastReq.Messages[0].Text()
	if !strings.Contains(first, "【你的领域】配置") {
		t.Fatalf("task missing domain prefix: %q", first)
	}
	if !strings.Contains(first, "实现 config.js 数值表") {
		t.Fatalf("task lost original text: %q", first)
	}
}

// TestDispatcher_DomainResponsibilityRequired 验证 role_id="domain" 但缺 responsibility
// 时被硬拒绝：LLM 经常省略该字段导致 DomainAgent 拿到通用 prompt 无职责边界，
// 此处强制报错让 MetaAgent 在下一轮补填。
func TestDispatcher_DomainResponsibilityRequired(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
			SystemPrompt: "通用领域纪律",
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, nil, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, _ := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "domain",
		"domain":  "配置",
		"task":    "实现 config.js",
	})
	if res.Success {
		t.Fatal("expected rejection when role_id=domain lacks responsibility")
	}
	if !strings.Contains(res.Error, "responsibility is required") {
		t.Fatalf("unexpected error: %s", res.Error)
	}
}

