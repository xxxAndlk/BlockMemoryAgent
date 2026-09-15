package server

import (
	"context"       // 测试用上下文
	"encoding/json" // wire 载荷构造
	"fmt"           // 构造会话 ID
	"net/http"      // HTTP handler 测试
	"net/http/httptest"
	"os"       // 写临时假视频文件
	"path/filepath" // 临时视频路径
	"strings" // 请求体构造
	"sync"    // 并发保护 mock 数据
	"testing" // 测试框架
	"time"    // 时间戳

	"github.com/gin-gonic/gin" // Gin Web 框架（测试路由）

	"github.com/blockmemory/agent/backend/internal/agent" // agent 门面接口
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // Agent 树节点类型
	"github.com/blockmemory/agent/backend/internal/userprofile"         // 看板快照类型
	"github.com/blockmemory/agent/backend/pkg/enums"                    // 会话状态与角色枚举
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
// 参数 ctx：上下文；filter：过滤条件（按 Status 过滤，Limit>0 时截断）。
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
	// 与真实门面一致：Limit 语义是"最多返回这么多条"。
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
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

// DeleteSession 硬删除测试会话（从 mock 存储摘除）。
// 返回值：不存在时返回 ErrSessionNotFound。
func (m *mockAgentForServer) DeleteSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return agent.ErrSessionNotFound
	}
	delete(m.sessions, sessionID)
	return nil
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

// PauseAgent 暂停 domain 支路，测试实现返回 nil。
func (m *mockAgentForServer) PauseAgent(ctx context.Context, sessionID, instID string) error {
	return nil
}

// MessageAgent 用户直连，测试实现返回 nil（成功）。
func (m *mockAgentForServer) MessageAgent(ctx context.Context, sessionID, instID, content string) (bool, error) {
	return false, nil
}

// Profile 返回用户画像，测试实现返回空。
func (m *mockAgentForServer) Profile(ctx context.Context) (*userprofile.Profile, error) {
	return &userprofile.Profile{}, nil
}

// SaveProfile 覆盖画像，测试实现为空操作。
func (m *mockAgentForServer) SaveProfile(ctx context.Context, content string) error { return nil }

// ProjectPreferences 返回项目偏好，测试实现返回空。
func (m *mockAgentForServer) ProjectPreferences(ctx context.Context) (*userprofile.Profile, error) {
	return &userprofile.Profile{}, nil
}

// SaveProjectPreferences 覆盖项目偏好，测试实现为空操作。
func (m *mockAgentForServer) SaveProjectPreferences(ctx context.Context, content string) error {
	return nil
}

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

// capturingAgent 包装 mockAgentForServer，捕获 Send 收到的消息供断言。
type capturingAgent struct {
	mockAgentForServer
	mu       sync.Mutex
	sent     []agent.Message
	sendSess []string
}

func (c *capturingAgent) Send(ctx context.Context, sessionID string, msg agent.Message) error {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.sendSess = append(c.sendSess, sessionID)
	c.mu.Unlock()
	return nil
}

// TestHandleSessionMessageWithImages 验证 POST /api/sessions/{id}/message 的
// images 字段：解码 agent.WireImage -> Message.Images 透传；超限张数 400。
func TestHandleSessionMessageWithImages(t *testing.T) {
	facade := newTestAgent(t)
	cap := &capturingAgent{mockAgentForServer: *newMockAgentForServer()}
	// 建一个会话供 Get 返回。
	sess, err := facade.CreateSession(context.Background(), agent.CreateRequest{Goal: "img"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_ = sess
	// capturingAgent 持有独立 sessions map，直接把会话注册进去。
	created, _ := cap.mockAgentForServer.CreateSession(context.Background(), agent.CreateRequest{Goal: "img"})

	mgr := NewSessionManager(cap)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/sessions/:id/message", mgr.HandleSessionMessage)

	// wire 载荷由 json.Marshal(tool.ResultImage) 生成：Data 为 base64 ASCII，
	// encoding/json 对 []byte 再做一层 base64（双端对称，解出即还原）。
	wantImg := agent.WireImage{MIMEType: "image/png", Data: []byte("aVBobw==")}
	imgJSON, _ := json.Marshal(wantImg)
	body := `{"content":"按这张图实现","images":[` + string(imgJSON) + `]}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+created.ID+"/message", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.sent) != 1 {
		t.Fatalf("Send 次数 = %d, want 1", len(cap.sent))
	}
	msg := cap.sent[0]
	if msg.Content != "按这张图实现" {
		t.Fatalf("content = %q", msg.Content)
	}
	if len(msg.Images) != 1 || msg.Images[0].MIMEType != "image/png" || string(msg.Images[0].Data) != "aVBobw==" {
		t.Fatalf("images = %+v", msg.Images)
	}

	// 超限 5 张：400。
	over := `{"content":"x","images":[`
	for i := 0; i < 5; i++ {
		if i > 0 {
			over += ","
		}
		over += `{"mime_type":"image/png","data":"aQ=="}`
	}
	over += `]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/sessions/"+created.ID+"/message", strings.NewReader(over))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("超限 status = %d, want 400", rec2.Code)
	}
}

// TestHandleSessionMessageWithVideos 验证 POST /api/sessions/{id}/message 的
// videos 字段：合法路径透传 Message.Videos；扩展名白名单/超数量/路径不存在 400。
func TestHandleSessionMessageWithVideos(t *testing.T) {
	facade := newTestAgent(t)
	cap := &capturingAgent{mockAgentForServer: *newMockAgentForServer()}
	created, _ := cap.mockAgentForServer.CreateSession(context.Background(), agent.CreateRequest{Goal: "vid"})
	_ = facade

	mgr := NewSessionManager(cap)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/sessions/:id/message", mgr.HandleSessionMessage)

	// 合法：假 .mp4（ParseWireVideos 只校验存在/大小，不解析内容）→ 透传。
	valid := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(valid, make([]byte, 10), 0o644); err != nil {
		t.Fatalf("写假视频: %v", err)
	}
	body := `{"content":"分析这个视频","videos":[{"path":` + jsonString(t, valid) + `}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+created.ID+"/message", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cap.mu.Lock()
	if len(cap.sent) != 1 {
		t.Fatalf("Send 次数 = %d, want 1", len(cap.sent))
	}
	if len(cap.sent[0].Videos) != 1 || cap.sent[0].Videos[0].Path != valid || cap.sent[0].Videos[0].MIMEType != "video/mp4" {
		t.Fatalf("videos 透传不符: %+v", cap.sent[0].Videos)
	}
	cap.mu.Unlock()

	// 非法：扩展名白名单 / 超 2 个 / 文件不存在 / 相对路径 → 400。
	pj := jsonString(t, valid)
	for name, b := range map[string]string{
		"bad-ext":       `{"content":"x","videos":[{"path":"C:/nope.exe"}]}`,
		"over-limit":    `{"content":"x","videos":[{"path":` + pj + `},{"path":` + pj + `},{"path":` + pj + `}]}`,
		"missing-file":  `{"content":"x","videos":[{"path":"C:/definitely_missing_x9.mp4"}]}`,
		"relative-path": `{"content":"x","videos":[{"path":"relative.mp4"}]}`,
	} {
		req2 := httptest.NewRequest(http.MethodPost, "/api/sessions/"+created.ID+"/message", strings.NewReader(b))
		rec2 := httptest.NewRecorder()
		r.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec2.Code)
		}
	}
}

// jsonString 把字符串编码为 JSON 字符串字面量（处理 Windows 路径反斜杠转义）。
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
