// Package agent 包含 ReactService 的单元测试。
package agent

import (
	// context 用于在测试中传递请求上下文与超时控制。
	"context"
	// os 与 path/filepath 用于在临时目录相关测试中创建/校验目录与文件。
	// strings 用于断言事件消息内容。
	"strings"
	"sync"
	"os"
	"path/filepath"
	// testing 提供 Go 标准测试框架。
	"testing"
	// time 用于设置轮询超时与休眠间隔。
	"time"

	// enums 提供会话状态等枚举常量。
	"github.com/blockmemory/agent/backend/pkg/enums"
	// eventkind 提供事件类型常量（user_message / system 等）。
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	// blades 提供可编程的模型消息与 provider 接口。
	"github.com/go-kratos/blades"

	// board 提供任务看板（TODO #36 输入补全的绑定数据源）。
	"github.com/blockmemory/agent/backend/internal/board"
	// orchestrator 提供 Agent 树结构,供话题切换测试 Register 节点。
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// mockReactModelProvider 是一个可编程的 blades.ModelProvider，用于 ReactService 测试。
// 它按顺序返回预设的模型响应，当预设耗尽后返回默认的 "done" 消息。
type mockReactModelProvider struct {
	responses []*blades.Message // responses 预设的模型响应队列
	calls     int               // calls 记录 Generate 已被调用的次数
	// block 非空时，第二次及之后 Generate 调用阻塞到该 channel 被关闭。
	// 用于让测试在会话结束前完成 tempDir 准备工作，避免 race。
	block <-chan struct{}
}

// Generate 实现 blades.ModelProvider 接口，按顺序返回预设响应。
//
// 参数:
//
//	ctx - 请求上下文，控制超时与取消；
//	req - 模型请求，测试中暂未使用。
//
// 返回值:
//
//	*blades.ModelResponse - 包含下一条预设消息的响应；
//	error                - 本 mock 实现始终返回 nil。
func (m *mockReactModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 第二次及之后调用，若 block 非空则等待关闭信号，让测试有机会在会话结束前建 tempDir。
	if m.calls >= 1 && m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 如果已调用次数达到预设响应数量，则返回默认结束消息，避免测试死循环。
	if m.calls >= len(m.responses) {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	// 取出当前索引对应的响应，并将调用计数加一。
	resp := m.responses[m.calls]
	m.calls++
	return &blades.ModelResponse{Message: resp}, nil
}

// Name 返回 mock provider 的名称，用于日志与调试。
func (m *mockReactModelProvider) Name() string { return "mock-react" }

// TestReactService_CreateAndGet 验证创建会话后可以通过 Get 接口查询到最终状态。
func TestReactService_CreateAndGet(t *testing.T) {
	// 准备只返回一条 "hello world" 消息的 mock provider。
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("hello world"),
		},
	}
	// 构造被测服务，使用临时目录作为工作目录。
	svc := newReactServiceForTest(llm, t.TempDir())
	// 使用空上下文调用 CreateSession。
	ctx := context.Background()

	// 创建会话。
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "test goal"})
	// 创建失败则立即终止测试。
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	// 会话 ID 必须非空。
	if created.ID == "" {
		t.Fatal("expected session ID")
	}
	// 目标字段应原样返回。
	if created.Goal != "test goal" {
		t.Errorf("Goal = %q, want %q", created.Goal, "test goal")
	}
	// 新建会话状态应为 running。
	if created.Status != string(enums.SessionStatusRunning) {
		t.Errorf("Status = %q, want %q", created.Status, string(enums.SessionStatusRunning))
	}

	// 轮询最多 2 秒，等待会话执行完成。
	deadline := time.Now().Add(2 * time.Second)
	var got *Session
	for time.Now().Before(deadline) {
		// 查询最新会话快照。
		got, err = svc.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		// 已完成则提前退出轮询。
		if got.Status == string(enums.SessionStatusCompleted) {
			break
		}
		// 未结束则短暂休眠，避免 CPU 空转。
		time.Sleep(10 * time.Millisecond)
	}
	// 校验返回的会话 ID 与目标。
	if got.ID != created.ID {
		t.Errorf("Get ID = %q, want %q", got.ID, created.ID)
	}
	if got.Goal != "test goal" {
		t.Errorf("Get Goal = %q, want %q", got.Goal, "test goal")
	}
	// 最终结果应等于 mock provider 返回的文本。
	if got.Result != "hello world" {
		t.Errorf("Result = %q, want %q", got.Result, "hello world")
	}
}

// TestReactService_CreateSessionEmitsUserMessageEvent 验证新建会话把首条消息（goal）
// 落成 user_message 事件：Web 聊天面板只从事件流渲染用户气泡与首回合容器，缺该事件时
// 首条消息仅存在于标题（goal）永不展示、首轮流式文本无处渲染（2026-09-26 实证）。
// 事件顺序固定：会话启动 → user_message（runSession 同 goroutine 顺序落，无交错）。
func TestReactService_CreateSessionEmitsUserMessageEvent(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("hello"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "完成TODO.md中的任务"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// 轮询最多 2 秒，等待会话执行完成（终态后事件流稳定）。
	deadline := time.Now().Add(2 * time.Second)
	var got *Session
	for time.Now().Before(deadline) {
		got, err = svc.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if got.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 首事件应为「会话启动」锚点，紧随其后应为 goal 的 user_message 事件。
	if len(got.Events) < 2 {
		t.Fatalf("expected >=2 events (启动锚点 + user_message), got %d", len(got.Events))
	}
	if got.Events[0].Type != eventkind.System || !strings.HasPrefix(got.Events[0].Message, "会话启动") {
		t.Errorf("events[0] = {%q %q}, want system 会话启动", got.Events[0].Type, got.Events[0].Message)
	}
	um := got.Events[1]
	if um.Type != eventkind.UserMessage {
		t.Errorf("events[1].Type = %q, want %q", um.Type, eventkind.UserMessage)
	}
	if um.Agent != "User" {
		t.Errorf("events[1].Agent = %q, want User", um.Agent)
	}
	if um.Message != "完成TODO.md中的任务" {
		t.Errorf("events[1].Message = %q, want goal 原文", um.Message)
	}
}

// TestReactService_ListSessions 验证 List 接口能正确返回多个会话并支持 Limit 过滤。
func TestReactService_ListSessions(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("first result"),
			blades.AssistantMessage("second result"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	// 创建第一个会话。
	_, err := svc.CreateSession(ctx, CreateRequest{Goal: "first"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	// 创建第二个会话。
	_, err = svc.CreateSession(ctx, CreateRequest{Goal: "second"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// 等待两个会话都完成，最多 2 秒。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := svc.List(ctx, Filter{})
		if len(all) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 不带过滤时应返回两个会话。
	all, err := svc.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("len(List) = %d, want 2", len(all))
	}

	// 限制返回 1 条时应只返回 1 个会话。
	limited, err := svc.List(ctx, Filter{Limit: 1})
	if err != nil {
		t.Fatalf("List with limit error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("len(List with limit) = %d, want 1", len(limited))
	}
}

// TestReactService_LaunchSessionCompletes 验证 LaunchSession 能异步启动并完成会话。
func TestReactService_LaunchSessionCompletes(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("launched result"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())

	// 启动会话。
	id := svc.LaunchSession("launch goal")
	if id == "" {
		t.Fatal("expected session ID")
	}

	// 轮询等待会话完成。
	deadline := time.Now().Add(2 * time.Second)
	var completed bool
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(context.Background(), id)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			// 校验最终结果。
			if snap.Result != "launched result" {
				t.Errorf("Result = %q, want %q", snap.Result, "launched result")
			}
			completed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !completed {
		t.Fatal("launched session did not complete")
	}
}

// TestReactService_SendMessage 验证向已完成会话发送消息后可再次得到回复。
func TestReactService_SendMessage(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("initial"),
			blades.AssistantMessage("reply to message"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "test goal"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// 等待初始运行完成。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 发送跟进消息。
	if err := svc.Send(ctx, created.ID, Message{Role: "user", Content: "follow up"}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	// 等待会话再次完成并产生预期结果。
	deadline = time.Now().Add(2 * time.Second)
	var final *Session
	for time.Now().Before(deadline) {
		final, _ = svc.Get(ctx, created.ID)
		if final != nil && final.Status == string(enums.SessionStatusCompleted) && final.Result == "reply to message" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("session not found after Send")
	}
	if final.Result != "reply to message" {
		t.Errorf("Result = %q, want %q", final.Result, "reply to message")
	}
	if len(final.Messages) < 3 {
		t.Errorf("expected at least 3 messages, got %d", len(final.Messages))
	}
}

// TestReactService_PromptEnhance_AppendsCompletion 开启补全时 Send 的续跑意图消息
// 被附加【系统补全】段（意图标签 + 看板失败任务绑定）；原文保留在【用户原始指令】段。
func TestReactService_PromptEnhance_AppendsCompletion(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("initial"),
			blades.AssistantMessage("resumed"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	svc.SetPromptEnhance(true)

	// 造带失败任务的看板（模拟"自检被 loop guard 三连败终止"）。
	b := board.NewTaskBoard("s1", "做塔防")
	if err := b.SetPlan("做塔防", []board.PlanTask{{ID: "t1", Title: "自检", Domain: "自检", Acceptance: []string{"a"}}}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if err := b.MarkFailed("t1", "loop guard 三连败终止"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	svc.SetBoard(func(string) *board.TaskBoard { return b })

	ctx := context.Background()
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "做塔防"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等待初始运行完成。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 发送续跑意图的短指令（事故场景原文）。
	if err := svc.Send(ctx, created.ID, Message{Role: "user", Content: "重新执行"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 等待会话再次完成，检查用户消息是否被补全。
	deadline = time.Now().Add(2 * time.Second)
	var final *Session
	for time.Now().Before(deadline) {
		final, _ = svc.Get(ctx, created.ID)
		if final != nil && final.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("session not found after Send")
	}
	var lastUser string
	for _, m := range final.Messages {
		if m.Role == string(enums.ChatRoleUser) {
			lastUser = m.Content
		}
	}
	if !strings.Contains(lastUser, "【系统补全】") || !strings.Contains(lastUser, "意图: 续跑") {
		t.Fatalf("enhanced message should carry 系统补全 + 续跑 intent, got: %q", lastUser)
	}
	if !strings.Contains(lastUser, "自检") || !strings.Contains(lastUser, "loop guard 三连败终止") {
		t.Fatalf("failed task should be bound, got: %q", lastUser)
	}
	if !strings.Contains(lastUser, "【用户原始指令】\n重新执行") {
		t.Fatalf("original text must be preserved verbatim, got: %q", lastUser)
	}
}

// TestReactService_PromptEnhance_DisabledPassthrough 关闭补全时 Send 原样直通（零行为变化）。
func TestReactService_PromptEnhance_DisabledPassthrough(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("initial"),
			blades.AssistantMessage("resumed"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	svc.SetPromptEnhance(false)

	ctx := context.Background()
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := svc.Send(ctx, created.ID, Message{Role: "user", Content: "继续做"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var final *Session
	for time.Now().Before(deadline) {
		final, _ = svc.Get(ctx, created.ID)
		if final != nil && final.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("session not found after Send")
	}
	for _, m := range final.Messages {
		if m.Role == string(enums.ChatRoleUser) && strings.Contains(m.Content, "继续做") {
			if strings.Contains(m.Content, "【系统补全】") {
				t.Fatalf("disabled enhance should pass through verbatim, got: %q", m.Content)
			}
			return
		}
	}
	t.Fatal("user message not found")
}

// TestReactService_CleansTempDirOnCompletion 验证会话完成后会清理其临时目录。
func TestReactService_CleansTempDirOnCompletion(t *testing.T) {
	workDir := t.TempDir()

	// mock LLM 第一轮调 ListDir（让会话持续到工具执行），第二轮返回 done 触发结束+cleanup。
	// 用 block channel 阻塞第二轮，让测试有机会在会话结束前建 tempDir+文件，race-free。
	block := make(chan struct{})
	toolCallMsg := &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "ListDir", Request: `{"path":"."}`},
		},
	}
	llm := &mockReactModelProvider{
		responses: []*blades.Message{toolCallMsg, blades.AssistantMessage("done")},
		block:     block,
	}
	svc := newReactServiceForTest(llm, workDir)

	session, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "test goal"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	tempDir := session.TempDir
	// 等会话完成第一轮 LLM 调用（避免第一轮还没跑就放行 block）。
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if llm.calls >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 在会话运行期间建 tempDir + 文件，让 cleanup 在会话结束时能命中并删除。
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	tempFile := filepath.Join(tempDir, "script.py")
	if err := os.WriteFile(tempFile, []byte("print('temp')"), 0644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	// 放行第二轮 LLM 调用，会话进入结束流程 -> cleanup 删 tempDir。
	close(block)
	// 确认 TempDir 路径含 session- 前缀（跨重启唯一格式校验）。
	if !strings.Contains(tempDir, "session-") {
		t.Fatalf("TempDir 应含 session- 前缀, got %q", tempDir)
	}

	// 轮询等待会话结束并确认临时目录被删除。
	deadline := time.Now().Add(2 * time.Second)
	cleaned := false
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(context.Background(), session.ID)
		if snap != nil && snap.Status != string(enums.SessionStatusRunning) {
			if _, err := os.Stat(tempDir); os.IsNotExist(err) {
				cleaned = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cleaned {
		t.Fatalf("会话完成后临时目录应被删除，但仍存在: %s", tempDir)
	}
	if _, err := os.Stat(tempFile); !os.IsNotExist(err) {
		t.Fatalf("会话完成后临时文件应被删除，但仍存在: %s", tempFile)
	}
}

// TestReactService_SessionIDUniqueAcrossInstances 验证两次独立创建的 ReactService
// 生成的 sessionID 不冲突。旧实现 "session-N" 重启回 1，block-memory 按 session_id
// 召回旧 session 数据污染新 session。
func TestReactService_SessionIDUniqueAcrossInstances(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{blades.AssistantMessage("done")},
	}
	// 实例 1
	svc1 := newReactServiceForTest(llm, t.TempDir())
	s1, err := svc1.CreateSession(context.Background(), CreateRequest{Goal: "first"})
	if err != nil {
		t.Fatalf("svc1 CreateSession: %v", err)
	}
	// 实例 2（模拟进程重启）
	svc2 := newReactServiceForTest(llm, t.TempDir())
	s2, err := svc2.CreateSession(context.Background(), CreateRequest{Goal: "second"})
	if err != nil {
		t.Fatalf("svc2 CreateSession: %v", err)
	}
	// 两个实例的 sessionID 必须不同，否则跨实例数据隔离失效。
	if s1.ID == s2.ID {
		t.Fatalf("两个 ReactService 实例生成相同 sessionID: %s", s1.ID)
	}
	// 都应含 session- 前缀且格式为 session-<bootEpoch>-<seq>。
	if !strings.HasPrefix(s1.ID, "session-") || !strings.HasPrefix(s2.ID, "session-") {
		t.Fatalf("sessionID 应含 session- 前缀, got %q / %q", s1.ID, s2.ID)
	}
}

// TestReactService_PausesOnIterationLimit 验证达到最大轮数上限时会话进入
// awaiting_clarify（暂停待续）而非 error，且用户发送消息后可从进度续跑直至完成。
func TestReactService_PausesOnIterationLimit(t *testing.T) {
	// 模型持续请求工具调用（触发轮数上限），恢复后给出最终答案。
	toolCallMsg := &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "ListDir", Request: `{"path":"."}`},
		},
	}
	llm := &mockReactModelProvider{
		responses: []*blades.Message{toolCallMsg, toolCallMsg, toolCallMsg, blades.AssistantMessage("final answer")},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	svc.SetRuntimeConfig(ReactRuntimeConfig{MaxIterations: 2})
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "long task"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// 轮询等待会话进入暂停待续状态（而不是 error）。
	deadline := time.Now().Add(3 * time.Second)
	paused := false
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusAwaitingClarify) {
			paused = true
			break
		}
		if snap != nil && snap.Status == string(enums.SessionStatusError) {
			t.Fatalf("达到轮数上限不应进入 error 状态: %s", snap.Result)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !paused {
		t.Fatal("会话未进入暂停待续状态")
	}

	// 校验暂停提示事件已记录。
	snap, _ := svc.Get(ctx, created.ID)
	found := false
	for _, ev := range snap.Events {
		if strings.Contains(ev.Message, "已达最大轮数上限") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("应记录'已达最大轮数上限'暂停事件")
	}

	// 用户发送"继续"后应从进度续跑并最终完成。
	if err := svc.Send(ctx, created.ID, Message{Role: "user", Content: "继续"}); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			if snap.Result != "final answer" {
				t.Fatalf("续跑结果 = %q, want %q", snap.Result, "final answer")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("续跑后会话未完成")
}

// TestReactService_SwitchTopic 验证话题切换:终结旧 Agent 树 + 更新 ActiveTopicID。
func TestReactService_SwitchTopic(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{blades.AssistantMessage("done")},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "first topic"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// 在树上 Register 一个 Running 节点(模拟派发的子 Agent)。
	tree := svc.TreeFor(created.ID)
	tree.Register(orchestrator.Node{ID: "sub-1", Role: "code_assistant", Task: "do X"})

	// 切换话题。
	switched, err := svc.SwitchTopic(ctx, created.ID, "second topic", "do Y")
	if err != nil {
		t.Fatalf("SwitchTopic error: %v", err)
	}
	if switched.ActiveTopicID != "1" {
		t.Errorf("expected ActiveTopicID=1, got %q", switched.ActiveTopicID)
	}
	if switched.Goal != "do Y" {
		t.Errorf("expected Goal=do Y, got %q", switched.Goal)
	}

	// 旧节点应被取消(Running -> Cancelled),树清空。
	if nodes := tree.Snapshot(); len(nodes) != 0 {
		t.Errorf("expected tree cleared after topic switch, got %d nodes", len(nodes))
	}

	// 再次切换:topic ID 递增。
	switched2, err := svc.SwitchTopic(ctx, created.ID, "third", "")
	if err != nil {
		t.Fatalf("second SwitchTopic error: %v", err)
	}
	if switched2.ActiveTopicID != "2" {
		t.Errorf("expected ActiveTopicID=2, got %q", switched2.ActiveTopicID)
	}
}

// TestReactService_SwitchTopicNotFound 验证会话不存在时返回 ErrSessionNotFound。
func TestReactService_SwitchTopicNotFound(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	ctx := context.Background()
	_, err := svc.SwitchTopic(ctx, "nope", "name", "")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

// recallMemStore 是 SharedMemoryStore 的内存测试实现,支持 Set/Get/Delete/Keys。
type recallMemStore struct {
	data map[string]string
}

func newRecallMemStore() *recallMemStore { return &recallMemStore{data: map[string]string{}} }

func (s *recallMemStore) Set(_ context.Context, k, v string) error    { s.data[k] = v; return nil }
func (s *recallMemStore) Get(_ context.Context, k string) (string, error) { return s.data[k], nil }
func (s *recallMemStore) Delete(_ context.Context, k string) error   { delete(s.data, k); return nil }
func (s *recallMemStore) Keys(_ context.Context) []string {
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	return out
}

// TestReactService_RecallTopicSummaries 验证按 session 前缀召回旧话题摘要:
// 跳过当前话题、跳过其他 session 的同 ID 摘要(防跨 session 污染)。
func TestReactService_RecallTopicSummaries(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	store := newRecallMemStore()
	svc.SetSharedMemoryStore(store)
	ctx := context.Background()

	// session-A 的两个旧话题 + 当前话题(2),session-B 的同 ID 话题(跨 session 污染源)。
	store.data["topic:session-A:1:summary"] = "【话题 1 摘要】旧任务A"
	store.data["topic:session-A:2:summary"] = "【话题 2 摘要】当前话题A"
	store.data["topic:session-B:1:summary"] = "【话题 1 摘要】他处任务B"

	got := svc.recallTopicSummaries(ctx, "session-A", "2")
	if strings.Contains(got, "当前话题A") {
		t.Errorf("不应召回当前话题摘要, got: %s", got)
	}
	if strings.Contains(got, "他处任务B") {
		t.Errorf("不应召回其他 session 的摘要(跨 session 污染), got: %s", got)
	}
	if !strings.Contains(got, "旧任务A") {
		t.Errorf("应召回旧话题摘要, got: %s", got)
	}
}

// TestReactService_InjectTopicRecall 验证旧话题摘要注入 input 前,且同一话题只注入一次。
func TestReactService_InjectTopicRecall(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	store := newRecallMemStore()
	svc.SetSharedMemoryStore(store)
	ctx := context.Background()

	// 构造一个内存会话:activeTopicID="2",recalledTopicID=""(未注入过)。
	sess := &reactInternalSession{ID: "s1", Goal: "g", activeTopicID: "2"}
	store.data["topic:s1:1:summary"] = "【话题 1 摘要】前置结论"

	out := svc.injectTopicRecall(ctx, sess, "继续")
	if !strings.Contains(out, "前置结论") || !strings.Contains(out, "继续") {
		t.Fatalf("应注入旧话题摘要并保留原 input, got: %s", out)
	}
	if sess.recalledTopicID != "2" {
		t.Fatalf("recalledTopicID 应更新为 2, got %q", sess.recalledTopicID)
	}

	// 第二次调用同一话题:不应重复注入(返回原 input)。
	out2 := svc.injectTopicRecall(ctx, sess, "再来")
	if strings.Contains(out2, "前置结论") {
		t.Fatalf("同一话题不应重复注入摘要, got: %s", out2)
	}
	if !strings.Contains(out2, "再来") {
		t.Fatalf("应保留原 input, got: %s", out2)
	}
}

// TestLoopConfigByRole 验证按角色返回的上下文 token 阈值：
// TokenBudgetPerRole 显式配置覆盖默认 150000（含显式 meta:0=不限制）；未列出角色用默认。
func TestLoopConfigByRole(t *testing.T) {
	cfg := ReactRuntimeConfig{TokenBudgetPerRole: map[string]int{
		"domain":         50000,
		"meta":           0,
		"code_assistant": 20000,
	}}
	cases := map[string]int{
		"domain":         50000,
		"meta":           0, // 显式设 0 -> 不限制(override 默认)
		"code_assistant": 20000,
		"ui_assistant":   150000, // 未在 map 中 -> 默认 150000
		"unknown_role":   150000, // 默认
	}
	for role, want := range cases {
		got := cfg.LoopConfigByRole(role).TokenBudget
		if got != want {
			t.Errorf("LoopConfigByRole(%q).TokenBudget = %d, want %d", role, got, want)
		}
	}
}

// TestLoopConfigByRoleDefault 验证未注入 TokenBudgetPerRole 时按默认 150000 全角色。
func TestLoopConfigByRoleDefault(t *testing.T) {
	cfg := ReactRuntimeConfig{}
	for _, role := range []string{"domain", "meta", "code_assistant", "unknown_role"} {
		if got := cfg.LoopConfigByRole(role).TokenBudget; got != 150000 {
			t.Errorf("%s default = %d, want 150000", role, got)
		}
	}
	// ContextTokenBudget 显式配置覆盖默认。
	if got := (ReactRuntimeConfig{ContextTokenBudget: 200000}).LoopConfigByRole("unknown").TokenBudget; got != 200000 {
		t.Errorf("ContextTokenBudget default = %d, want 200000", got)
	}
}

// TestLoopConfigByRoleOverride 验证显式配置覆盖默认。
func TestLoopConfigByRoleOverride(t *testing.T) {
	cfg := ReactRuntimeConfig{TokenBudgetPerRole: map[string]int{
		"domain": 30000, // 覆盖默认 150000
	}}
	if got := cfg.LoopConfigByRole("domain").TokenBudget; got != 30000 {
		t.Errorf("override domain = %d, want 30000", got)
	}
	// 保留 LoopConfig() 的其他字段（MaxIterations 等）。
	if lc := cfg.LoopConfigByRole("domain"); lc.MaxIterations == 0 {
		t.Error("LoopConfigByRole 应继承 LoopConfig() 的 MaxIterations 默认值")
	}
}

// mockPausedDomainResumer 记录 ResumePaused 调用，返回预设 result/err。
type mockPausedDomainResumer struct {
	result ReactResult
	err    error
	calls  int
	nodeID string
}

func (m *mockPausedDomainResumer) ResumePaused(_ context.Context, pausedNodeID string) (ReactResult, error) {
	m.calls++
	m.nodeID = pausedNodeID
	return m.result, m.err
}

// setSessionPausedOnChild 把会话置为 PausedOnChild 并在树上注册一个 Paused domain 节点，
// 模拟"子领域 Agent 触达 token 上限暂停"现场。返回注入的 paused 节点 ID。
func setSessionPausedOnChild(t *testing.T, svc *ReactService, sessionID string) string {
	t.Helper()
	pausedNodeID := sessionID + "/domain-1"
	svc.store.mu.Lock()
	sess, ok := svc.store.sessions[sessionID]
	if !ok {
		svc.store.mu.Unlock()
		t.Fatalf("session %s not found", sessionID)
	}
	sess.Status = enums.SessionStatusPausedOnChild
	// 重建可取消 ctx，供 resumeSession 使用。
	ctx, cancel := context.WithCancel(context.Background())
	sess.ctx = ctx
	sess.cancelFn = cancel
	svc.store.mu.Unlock()
	tr := svc.TreeFor(sessionID)
	tr.Register(orchestrator.Node{ID: pausedNodeID, ParentID: sessionID, Role: "domain", Status: orchestrator.StatusRunning, Started: time.Now()})
	tr.Pause(pausedNodeID, "token budget exhausted")
	return pausedNodeID
}

// waitForStatus 轮询直至会话状态匹配 want，超时失败。
func waitForStatus(t *testing.T, svc *ReactService, sessionID string, want enums.SessionStatus, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(context.Background(), sessionID)
		if snap != nil && snap.Status == string(want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("会话未进入 %s 状态", name)
}

// TestSendMessageRoutesToPausedDomain 验证 PausedOnChild 态发消息优先恢复 earliest paused domain：
// resumeDispatcher.ResumePaused 被调用 + 完成后 MetaAgent 经 resumeSession 整合结果。
func TestSendMessageRoutesToPausedDomain(t *testing.T) {
	llm := &mockReactModelProvider{responses: []*blades.Message{
		blades.AssistantMessage("init"),
		blades.AssistantMessage("整合完毕：ok"),
	}}
	svc := newReactServiceForTest(llm, t.TempDir())
	resumer := &mockPausedDomainResumer{result: ReactResult{Text: "domain done"}}
	svc.SetPausedDomainResumer(resumer)

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等初始 runSession 完成，避免其覆盖即将设置的 PausedOnChild 态。
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "initial completed")
	pausedID := setSessionPausedOnChild(t, svc, created.ID)

	if err := svc.sendMessage(context.Background(), created.ID, "继续"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	// 完成后 MetaAgent resumeSession 接管整合 -> 会话 Completed。
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "completed")
	if resumer.calls != 1 {
		t.Errorf("ResumePaused calls = %d, want 1 (should route to paused domain)", resumer.calls)
	}
	if resumer.nodeID != pausedID {
		t.Errorf("ResumePaused nodeID = %q, want %q", resumer.nodeID, pausedID)
	}
}

// TestResumePausedDomainRePause 验证 domain resume 再触限（LimitReached）时回退 PausedOnChild。
func TestResumePausedDomainRePause(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	resumer := &mockPausedDomainResumer{result: ReactResult{LimitReached: true}}
	svc.SetPausedDomainResumer(resumer)

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "initial completed")
	setSessionPausedOnChild(t, svc, created.ID)

	if err := svc.sendMessage(context.Background(), created.ID, "继续"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	// 再触限 -> 回退 PausedOnChild 等下次"继续"。
	waitForStatus(t, svc, created.ID, enums.SessionStatusPausedOnChild, "paused_on_child (re-pause)")
	if resumer.calls != 1 {
		t.Errorf("ResumePaused calls = %d, want 1", resumer.calls)
	}
}

// TestResumePausedDomainCompletes 验证 domain resume 完成后 MetaAgent resumeSession 整合结果。
func TestResumePausedDomainCompletes(t *testing.T) {
	llm := &mockReactModelProvider{responses: []*blades.Message{
		blades.AssistantMessage("init"),
		blades.AssistantMessage("最终整合答案"),
	}}
	svc := newReactServiceForTest(llm, t.TempDir())
	resumer := &mockPausedDomainResumer{result: ReactResult{Text: "domain done"}}
	svc.SetPausedDomainResumer(resumer)

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "initial completed")
	setSessionPausedOnChild(t, svc, created.ID)

	if err := svc.sendMessage(context.Background(), created.ID, "继续"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "completed")
	snap, _ := svc.Get(context.Background(), created.ID)
	if !strings.Contains(snap.Result, "最终整合答案") {
		t.Errorf("result = %q, want contain '最终整合答案'", snap.Result)
	}
}

// TestFindEarliestPausedDomain 验证按 Started 最早返回 Paused domain 节点 ID。
func TestFindEarliestPausedDomain(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tr := svc.TreeFor(created.ID)
	t0 := time.Now()
	// 后注册的更早 Started，应被选中。
	tr.Register(orchestrator.Node{ID: "late", ParentID: created.ID, Role: "domain", Status: orchestrator.StatusRunning, Started: t0.Add(2 * time.Second)})
	tr.Pause("late", "x")
	tr.Register(orchestrator.Node{ID: "early", ParentID: created.ID, Role: "domain", Status: orchestrator.StatusRunning, Started: t0.Add(1 * time.Second)})
	tr.Pause("early", "x")
	// 非 domain 的 Paused 节点应被忽略。
	tr.Register(orchestrator.Node{ID: "other", ParentID: created.ID, Role: "code_assistant", Status: orchestrator.StatusRunning, Started: t0})
	tr.Pause("other", "x")

	got := svc.findEarliestPausedDomain(created.ID)
	if got != "early" {
		t.Errorf("findEarliestPausedDomain = %q, want 'early'", got)
	}
}

// TestCancelPausedOnChildSession 验证 paused_on_child 态会话可被取消：
// 暂停态没有运行中的 goroutine，但必须允许用户退出暂停死锁
// （回归：旧实现拒绝非 running 态取消，会话无任何逃生通道）。
func TestCancelPausedOnChildSession(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "initial completed")
	setSessionPausedOnChild(t, svc, created.ID)

	if err := svc.cancel(context.Background(), created.ID); err != nil {
		t.Fatalf("cancel paused_on_child session should succeed, got: %v", err)
	}
	snap, _ := svc.Get(context.Background(), created.ID)
	if snap.Status != string(enums.SessionStatusError) {
		t.Errorf("status = %q, want %q (cancelled)", snap.Status, enums.SessionStatusError)
	}
}

// TestCancelAwaitingClarifySession 验证 awaiting_clarify 态会话同样可被取消。
func TestCancelAwaitingClarifySession(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "initial completed")
	svc.store.mu.Lock()
	if sess, ok := svc.store.sessions[created.ID]; ok {
		sess.Status = enums.SessionStatusAwaitingClarify
	}
	svc.store.mu.Unlock()

	if err := svc.cancel(context.Background(), created.ID); err != nil {
		t.Fatalf("cancel awaiting_clarify session should succeed, got: %v", err)
	}
	snap, _ := svc.Get(context.Background(), created.ID)
	if snap.Status != string(enums.SessionStatusError) {
		t.Errorf("status = %q, want %q (cancelled)", snap.Status, enums.SessionStatusError)
	}
}

// TestCancelSession_CascadesTree 验证 TODO #25-2 会话取消级联：
// cancel() 遍历权威树取消 Running/Paused 节点。
func TestCancelSession_CascadesTree(t *testing.T) {
	svc := newReactServiceForTest(&deadlineProvider{}, t.TempDir())
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 手动注册两个在跑子节点（模拟派发中的子 Agent）。
	tr := svc.TreeFor(created.ID)
	if tr == nil {
		t.Fatal("TreeFor returned nil")
	}
	tr.Register(orchestrator.Node{ID: created.ID + "/domain-1", ParentID: created.ID, Role: "domain", Status: orchestrator.StatusRunning})
	tr.Register(orchestrator.Node{ID: created.ID + "/code_assistant-1", ParentID: created.ID, Role: "code_assistant", Status: orchestrator.StatusRunning})

	if err := svc.cancel(context.Background(), created.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, n := range tr.Snapshot() {
		if n.Status != orchestrator.StatusCancelled {
			t.Fatalf("node %s should be Cancelled after session cancel, got status=%v", n.ID, n.Status)
		}
	}
}

// TestWallClock_ExpiryTerminatesSession 验证 TODO #25-4 全局墙钟：
// 会话超过时限未终止 -> 级联取消节点 + 会话置 error"超全局时限"。
func TestWallClock_ExpiryTerminatesSession(t *testing.T) {
	svc := newReactServiceForTest(&blockingLLMProvider{}, t.TempDir())
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tr := svc.TreeFor(created.ID)
	tr.Register(orchestrator.Node{ID: created.ID + "/domain-1", ParentID: created.ID, Role: "domain", Status: orchestrator.StatusRunning})

	sess := svc.store.snapshotSessionByID(created.ID)
	svc.startWallClock(sess, 200*time.Millisecond)

	// 墙钟先发布会话终态、再级联取消树节点（setSessionError 的"首次错误胜出"要求
	// 时限文案不能被随后 ctx 取消触发的 runSession 收尾覆写），两步之间存在微秒级
	// 窗口——轮询等树收敛，不能"看到 error 立即断言"（Linux 容器实测 4/30 挂）。
	deadline := time.Now().Add(5 * time.Second)
	var stuckNode string
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(context.Background(), created.ID)
		if snap.Status != string(enums.SessionStatusError) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !strings.Contains(snap.Result, "超全局时限") {
			t.Fatalf("expected wall-clock error text, got: %s", snap.Result)
		}
		// 树节点应被级联取消（级联滞后于终态发布，等其收敛）。
		stuckNode = ""
		for _, n := range tr.Snapshot() {
			if n.Status != orchestrator.StatusCancelled {
				stuckNode = n.ID
			}
		}
		if stuckNode == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session did not terminate within wall clock deadline (stuck node: %s)", stuckNode)
}

// secondCallGateProvider 包装 mock：第二次 Generate 进入时发信号（随后被 mock 的
// block 卡住），供测试精确等待"工具回合已完成、下一次 LLM 调用在飞"的时刻；
// 同时捕获每次请求的完整消息列表，供断言续跑后模型第一眼能看到全部前文。
type secondCallGateProvider struct {
	inner   *mockReactModelProvider
	entered chan struct{}
	once    sync.Once

	mu      sync.Mutex
	lastReq *blades.ModelRequest
}

func (p *secondCallGateProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if p.inner.calls >= 1 {
		p.once.Do(func() { close(p.entered) })
	}
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	return p.inner.Generate(ctx, req)
}

func (p *secondCallGateProvider) Name() string { return "second-call-gate" }

// requestContains 报告最近一次的 LLM 请求消息里是否含指定子串。
func (p *secondCallGateProvider) requestContains(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastReq == nil {
		return false
	}
	for _, m := range p.lastReq.Messages {
		if strings.Contains(m.Text(), sub) {
			return true
		}
	}
	return false
}

// TestCancelMidRun_PreservesHistoryForResume 回归 2026-09-20 线上事故（会话
// session-1789909779862275000-f6d467e3-6，日志 2026-09-20.log）：
// 用户首轮要求"提取全部学习文档示例代码"，MetaAgent 提交计划被驳回，用户终止
// 会话后补发"rag_qa_bot文件夹与RAG实战课程，RAG实战指南文件不用管，修改记录与
// README也不用"——执行却只做被剔除的那几个文件夹/文件，意图完全做反。
// 根因：runSession/resumeSession 的错误分支不把 result.History 提交到
// session.History（只有成功/暂停分支提交），用户终止即丢弃全部前文——
// 续跑时 History 为空（线上实测 msgs=3/hist=301），模型只看到当条补充消息。
// 本测试使用与线上一致的用户输入串，复刻"工具回合在飞时被终止→补发消息续跑"全程。
func TestCancelMidRun_PreservesHistoryForResume(t *testing.T) {
	block := make(chan struct{})
	llm := &secondCallGateProvider{
		inner: &mockReactModelProvider{
			responses: []*blades.Message{
				{
					Role: blades.RoleAssistant,
					Parts: []blades.Part{
						blades.ToolPart{Name: "ListDir", Request: string(mustJSON(map[string]any{"path": "."}))},
					},
				},
			},
			block: block,
		},
		entered: make(chan struct{}),
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	// 与线上一致的首轮输入（2026-09-20.log 22:04 会话原话）。
	goal := "查看文件夹下的学习文档，其中有大量示例代码。你新建一个文件夹，里面提取出所有文档中的示例代码，每一段完整代码示例一个文件。你要全部提取出来的同时验证是否能跑跑不起来修复文档中代码再提取出文件。一个个完整代码示例一个文件，一节内容一个文件夹，一个章内容一个大文件夹分层级，方便学习时找到例子"
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: goal})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等第二次 LLM 调用进入阻塞（第一次工具调用回合已累积进循环内 history）。
	select {
	case <-llm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("2nd llm call did not enter Generate")
	}
	// 用户终止（复刻事故：计划被驳回、修订中点终止）。
	if err := svc.cancel(context.Background(), created.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusError, "cancelled")

	// 修复点：error 终态后 History 必须已提交（修复前为 nil，续跑丢光前文）。
	// cancel() 先翻终态、ReAct 收尾 goroutine 滞后——轮询等 History 落库。
	var hist []ReactMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		svc.store.mu.Lock()
		hist = make([]ReactMessage, len(svc.store.sessions[created.ID].History))
		copy(hist, svc.store.sessions[created.ID].History)
		svc.store.mu.Unlock()
		if len(hist) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(hist) == 0 {
		t.Fatal("cancelled session should keep accumulated History, got empty")
	}
	contains := func(sub string) bool {
		for _, m := range hist {
			if strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}
	if !contains(goal) {
		t.Fatal("History lost original goal after cancel")
	}

	// 事故续跑路径：终止后补发与线上一致的剔除消息。
	followUp := "rag_qa_bot文件夹与RAG实战课程，RAG实战指南文件不用管，修改记录与README也不用"
	close(block)
	if err := svc.enqueue(context.Background(), created.ID, followUp); err != nil {
		t.Fatalf("enqueue after cancel: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusCompleted, "resumed completed")

	svc.store.mu.Lock()
	hist = make([]ReactMessage, len(svc.store.sessions[created.ID].History))
	copy(hist, svc.store.sessions[created.ID].History)
	svc.store.mu.Unlock()
	if !contains(goal) || !contains(followUp) {
		t.Fatalf("resumed run lost context: goal=%v follow-up=%v", contains(goal), contains(followUp))
	}
	// 决定性断言：续跑后的第一次 LLM 请求必须同时携带原始任务与补充消息——
	// 模型第一眼就能看到全部前文，而不是只看到当条消息（线上事故形态）。
	if !llm.requestContains(goal) || !llm.requestContains(followUp) {
		t.Fatalf("resumed first LLM request missing context: goal=%v follow-up=%v",
			llm.requestContains(goal), llm.requestContains(followUp))
	}
}

// blockingLLMProvider Generate 阻塞直到 ctx 取消：模拟长跑会话（供墙钟测试保持 running 态）。
type blockingLLMProvider struct{}

func (p *blockingLLMProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *blockingLLMProvider) Name() string { return "blocking" }

// TestFinalizeThinking_Dedupe 验证 think 事件去重：
// 思考流与答复流交错时每个 LLMDelta 触发一次 finalize，累积式思考文本会产生
// 数十条内容相同的 think 事件（实证同一条"等待回传"叙事日志重复 40+ 行）。
// 相同文本只落一条，不同文本（新一轮思考）再落。
func TestFinalizeThinking_Dedupe(t *testing.T) {
	svc := newReactServiceForTest(nil, t.TempDir())
	sess := &reactInternalSession{ID: "s1", ctx: context.Background()}
	svc.store.mu.Lock()
	svc.store.sessions["s1"] = sess
	svc.store.mu.Unlock()

	ev := LiveEvent{Agent: "代码助手", AgentID: "s1/code_assistant-1"}
	sess.ThinkingText = "等待子 Agent 回传结果。"
	svc.finalizeThinking(sess, ev)
	svc.finalizeThinking(sess, ev)
	svc.finalizeThinking(sess, ev)
	if len(sess.Events) != 1 {
		t.Fatalf("相同思考文本应只落 1 条 think 事件，got %d", len(sess.Events))
	}
	if sess.ThinkingText != "" {
		t.Fatalf("finalize 后思考缓存应清空，got %q", sess.ThinkingText)
	}
	// 新一轮不同思考文本应再落一条。
	sess.ThinkingText = "现在开始实现丧尸绘制分支。"
	svc.finalizeThinking(sess, ev)
	if len(sess.Events) != 2 {
		t.Fatalf("不同思考文本应再落 1 条，got %d", len(sess.Events))
	}
}
