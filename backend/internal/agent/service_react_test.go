// Package agent 包含 ReactService 的单元测试。
package agent

import (
	// context 用于在测试中传递请求上下文与超时控制。
	"context"
	// os 与 path/filepath 用于在临时目录相关测试中创建/校验目录与文件。
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

	// 预先创建临时目录及文件，让服务在结束时能够观察到并清理它们。
	tempDir := filepath.Join(workDir, ".bma", "tmp", "session-1")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	tempFile := filepath.Join(tempDir, "script.py")
	if err := os.WriteFile(tempFile, []byte("print('temp')"), 0644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}

	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("done"),
		},
	}
	svc := newReactServiceForTest(llm, workDir)

	session, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "test goal"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if session.TempDir != tempDir {
		t.Fatalf("TempDir 期望 %q，got %q", tempDir, session.TempDir)
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
