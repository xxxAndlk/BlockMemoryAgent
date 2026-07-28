// Package agent 包含 ReactService 的单元测试。
package agent

import (
	// context 用于在测试中传递请求上下文与超时控制。
	"context"
	// os 与 path/filepath 用于在临时目录相关测试中创建/校验目录与文件。
	// strings 用于断言事件消息内容。
	"strings"
	"os"
	"path/filepath"
	// testing 提供 Go 标准测试框架。
	"testing"
	// time 用于设置轮询超时与休眠间隔。
	"time"

	// enums 提供会话状态等枚举常量。
	"github.com/blockmemory/agent/backend/pkg/enums"
	// blades 提供可编程的模型消息与 provider 接口。
	"github.com/go-kratos/blades"
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
