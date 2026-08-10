package server

import (
	"context" // 测试用上下文
	"fmt"     // 构造会话 ID
	"sync"    // 并发保护 mock 数据
	"testing" // 测试框架
	"time"    // 时间戳

	"github.com/blockmemory/agent/backend/internal/agent"                // agent 门面接口
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/userprofile"                // 看板快照类型
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // Agent 树节点类型
	"github.com/blockmemory/agent/backend/pkg/enums"                     // 会话状态与角色枚举
)

// mockAgentForServer 是一个最小化的 agent.Agent 实现，
// 用于在不连接真实 ReAct 引擎或图的情况下测试 HTTP 适配层。
type mockAgentForServer struct {
	mu       sync.RWMutex              // 保护 sessions 并发访问
	sessions map[string]*agent.Session // 会话 ID -> 会话对象
	seq      int                       // 自增 ID 序列号
}

// newMockAgentForServer 创建并初始化一个 mock Agent。
// 返回值：*mockAgentForServer。
func newMockAgentForServer() *mockAgentForServer {
	return &mockAgentForServer{sessions: make(map[string]*agent.Session)}
}

// CreateSession 创建一个新的测试会话。
// 参数 ctx：上下文；req：创建请求。
// 返回值：创建的会话与错误。
func (m *mockAgentForServer) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("session-%d", m.seq)
	s := &agent.Session{
		ID:        id,
		Goal:      req.Goal,
		Status:    string(enums.SessionStatusCompleted),
		Result:    "done",
		StartedAt: time.Now(),
		Messages: []agent.Message{
			{Role: string(enums.ChatRoleUser), Content: req.Goal, Timestamp: time.Now()},
		},
	}
	m.sessions[id] = s
	return s, nil
}

// ResumeSession 恢复会话，测试实现直接返回 Get 结果。
func (m *mockAgentForServer) ResumeSession(ctx context.Context, sessionID string, req agent.ResumeRequest) (*agent.Session, error) {
	return m.Get(ctx, sessionID)
}

// Send 发送消息，测试实现为空操作。
func (m *mockAgentForServer) Send(ctx context.Context, sessionID string, msg agent.Message) error {
	return nil
}

// Stream 返回事件流，测试实现返回 nil。
func (m *mockAgentForServer) Stream(ctx context.Context, sessionID string) (<-chan agent.Event, error) {
	return nil, nil
}

// Query 通用查询，测试实现返回空结果。
func (m *mockAgentForServer) Query(ctx context.Context, sessionID string, q agent.Query) (agent.Result, error) {
	return agent.Result{}, nil
}

// Control 控制命令，测试实现为空操作。
func (m *mockAgentForServer) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error {
	return nil
}

// List 按过滤条件列出会话。
// 参数 ctx：上下文；filter：过滤条件（目前仅按 Status 过滤）。
// 返回值：会话指针切片与错误。
func (m *mockAgentForServer) List(ctx context.Context, filter agent.Filter) ([]*agent.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*agent.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if filter.Status != "" && s.Status != filter.Status {
			continue // 状态不匹配则跳过
		}
		out = append(out, s)
	}
	return out, nil
}

// Get 根据 ID 获取会话。
// 返回值：会话指针；不存在时返回 ErrSessionNotFound。
func (m *mockAgentForServer) Get(ctx context.Context, sessionID string) (*agent.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, agent.ErrSessionNotFound
	}
	return s, nil
}

// ListAgents 列出会话中的 Agent 实例，测试实现返回 nil。
func (m *mockAgentForServer) ListAgents(ctx context.Context, sessionID string) ([]agent.AgentInstance, error) {
	return nil, nil
}

// Tree 返回 Agent 树快照，测试实现返回 nil。
func (m *mockAgentForServer) Tree(ctx context.Context, sessionID string) ([]orchestrator.Node, error) {
	return nil, nil
}

// CancelAgent 取消子 Agent，测试实现返回 nil。
func (m *mockAgentForServer) CancelAgent(ctx context.Context, sessionID, instID string) error {
	return nil
}

// Profile 返回用户画像，测试实现返回空。
func (m *mockAgentForServer) Profile(ctx context.Context) (*userprofile.Profile, error) { return &userprofile.Profile{}, nil }

// SaveProfile 覆盖画像，测试实现为空操作。
func (m *mockAgentForServer) SaveProfile(ctx context.Context, content string) error { return nil }

// Shutdown 关闭 Agent，测试实现为空操作。
func (m *mockAgentForServer) Shutdown(ctx context.Context) error { return nil }

// SummarizeTaskTitle 总结任务标题，测试实现直接返回原值。
func (m *mockAgentForServer) SummarizeTaskTitle(ctx context.Context, title string) string {
	return title
}

// newTestAgent 构造一个用于测试的 agent.Agent 实例。
// 参数 t：测试对象。
// 返回值：agent.Agent。
func newTestAgent(t *testing.T) agent.Agent {
	t.Helper()
	return newMockAgentForServer()
}

// TestSessionManagerDelegatesCreateAndGet 测试创建会话与按 ID 获取是否正确委托。
func TestSessionManagerDelegatesCreateAndGet(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	session, err := mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "adapter test"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	got := mgr.GetSession(session.ID)
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.ID != session.ID {
		t.Errorf("GetSession ID = %q, want %q", got.ID, session.ID)
	}
	if got.Goal != "adapter test" {
		t.Errorf("GetSession Goal = %q, want %q", got.Goal, "adapter test")
	}
}

// TestSessionManagerListSessions 测试 ListSessions 返回所有会话。
func TestSessionManagerListSessions(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	_, _ = mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "first"})
	_, _ = mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "second"})

	sessions := mgr.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("ListSessions len = %d, want 2", len(sessions))
	}
}

// TestSessionManagerLaunchSession 测试 LaunchSession 能成功返回会话 ID 并最终完成。
func TestSessionManagerLaunchSession(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	id := mgr.LaunchSession("launch test")
	if id == "" {
		t.Fatal("LaunchSession returned empty id")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := mgr.GetSession(id)
		if s != nil && s.Status != enums.SessionStatusRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("LaunchSession did not complete")
}

// Board 返回看板快照，测试实现返回 nil（回退树合成）。
func (m *mockAgentForServer) Board(ctx context.Context, sessionID string) (*board.Snapshot, error) {
	return nil, nil
}
