package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// mockReactModelProvider is a programmable blades.ModelProvider for ReactService tests.
type mockReactModelProvider struct {
	responses []*blades.Message
	calls     int
}

func (m *mockReactModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if m.calls >= len(m.responses) {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	resp := m.responses[m.calls]
	m.calls++
	return &blades.ModelResponse{Message: resp}, nil
}

func (m *mockReactModelProvider) Name() string { return "mock-react" }

func TestReactService_CreateAndGet(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("hello world"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "test goal"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected session ID")
	}
	if created.Goal != "test goal" {
		t.Errorf("Goal = %q, want %q", created.Goal, "test goal")
	}
	if created.Status != string(enums.SessionStatusRunning) {
		t.Errorf("Status = %q, want %q", created.Status, string(enums.SessionStatusRunning))
	}

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
	if got.ID != created.ID {
		t.Errorf("Get ID = %q, want %q", got.ID, created.ID)
	}
	if got.Goal != "test goal" {
		t.Errorf("Get Goal = %q, want %q", got.Goal, "test goal")
	}
	if got.Result != "hello world" {
		t.Errorf("Result = %q, want %q", got.Result, "hello world")
	}
}

func TestReactService_ListSessions(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("first result"),
			blades.AssistantMessage("second result"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())
	ctx := context.Background()

	_, err := svc.CreateSession(ctx, CreateRequest{Goal: "first"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	_, err = svc.CreateSession(ctx, CreateRequest{Goal: "second"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Wait briefly for sessions to be created; they complete quickly with the mock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := svc.List(ctx, Filter{})
		if len(all) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	all, err := svc.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("len(List) = %d, want 2", len(all))
	}

	limited, err := svc.List(ctx, Filter{Limit: 1})
	if err != nil {
		t.Fatalf("List with limit error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("len(List with limit) = %d, want 1", len(limited))
	}
}

func TestReactService_LaunchSessionCompletes(t *testing.T) {
	llm := &mockReactModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("launched result"),
		},
	}
	svc := newReactServiceForTest(llm, t.TempDir())

	id := svc.LaunchSession("launch goal")
	if id == "" {
		t.Fatal("expected session ID")
	}

	deadline := time.Now().Add(2 * time.Second)
	var completed bool
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(context.Background(), id)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
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

	// Wait for initial run to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := svc.Send(ctx, created.ID, Message{Role: "user", Content: "follow up"}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

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

func TestReactService_CleansTempDirOnCompletion(t *testing.T) {
	workDir := t.TempDir()

	// Pre-create the temp directory so the service can clean it up.
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
