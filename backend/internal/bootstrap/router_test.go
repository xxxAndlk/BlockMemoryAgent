package bootstrap

import (
	"context" // 接口签名占位
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin" // Gin Web 框架（测试路由）

	"github.com/blockmemory/agent/backend/internal/agent"               // agent.Agent 接口
	"github.com/blockmemory/agent/backend/internal/board"               // 看板快照类型
	"github.com/blockmemory/agent/backend/internal/config"              // 应用配置
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // Agent 树节点类型
	"github.com/blockmemory/agent/backend/internal/server"              // SessionManager / DAGHandler
	"github.com/blockmemory/agent/backend/internal/userprofile"         // 用户画像类型
)

// mockAgentForRouter 是构造 SessionManager 所需的最小 agent.Agent 桩。
// 仅用于路由注册测试，不承载业务行为：全部方法返回零值。
type mockAgentForRouter struct{}

func (mockAgentForRouter) CreateSession(context.Context, agent.CreateRequest) (*agent.Session, error) {
	return nil, nil
}
func (mockAgentForRouter) ResumeSession(context.Context, string, agent.ResumeRequest) (*agent.Session, error) {
	return nil, nil
}
func (mockAgentForRouter) Send(context.Context, string, agent.Message) error { return nil }
func (mockAgentForRouter) Stream(context.Context, string) (<-chan agent.Event, error) {
	return nil, nil
}
func (mockAgentForRouter) Query(context.Context, string, agent.Query) (agent.Result, error) {
	return agent.Result{}, nil
}
func (mockAgentForRouter) Control(context.Context, string, agent.ControlCommand) error { return nil }
func (mockAgentForRouter) List(context.Context, agent.Filter) ([]*agent.Session, error) {
	return nil, nil
}
func (mockAgentForRouter) Get(context.Context, string) (*agent.Session, error) {
	return nil, nil
}
func (mockAgentForRouter) DeleteSession(context.Context, string) error { return nil }
func (mockAgentForRouter) ListAgents(context.Context, string) ([]agent.AgentInstance, error) {
	return nil, nil
}
func (mockAgentForRouter) Tree(context.Context, string) ([]orchestrator.Node, error) {
	return nil, nil
}
func (mockAgentForRouter) CancelAgent(context.Context, string, string) error { return nil }
func (mockAgentForRouter) PauseAgent(context.Context, string, string) error  { return nil }
func (mockAgentForRouter) MessageAgent(context.Context, string, string, string) error {
	return nil
}
func (mockAgentForRouter) Board(context.Context, string) (*board.Snapshot, error) {
	return nil, nil
}
func (mockAgentForRouter) Profile(context.Context) (*userprofile.Profile, error) {
	return nil, nil
}
func (mockAgentForRouter) SaveProfile(context.Context, string) error { return nil }
func (mockAgentForRouter) ProjectPreferences(context.Context) (*userprofile.Profile, error) {
	return &userprofile.Profile{}, nil
}
func (mockAgentForRouter) SaveProjectPreferences(context.Context, string) error { return nil }
func (mockAgentForRouter) Shutdown(context.Context) error            { return nil }
func (mockAgentForRouter) SummarizeTaskTitle(context.Context, string) string {
	return ""
}

// TestNewDefaultRouterRegistersWithoutPanic 验证生产路由注册不触发 Gin 通配符冲突 panic。
// 重点覆盖同层级"静态段 + 参数段"场景（/api/dag/running vs /api/dag/:id、
// /api/plugins/reload vs /api/plugins/:id、/api/metrics/timeline vs /api/metrics），
// 这类冲突在 Gin 中是注册期 panic，必须在测试中钉住。
// 同时验证：鉴权关闭时请求直接放行、静态段优先于参数段命中。
func TestNewDefaultRouterRegistersWithoutPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &App{
		Config:     &config.Config{HTTP: config.HTTPConfig{AuthEnabled: false}},
		Server:     server.NewSessionManager(mockAgentForRouter{}),
		DAGHandler: server.NewDAGHandler(nil, nil), // scheduler nil：请求返回 503，但注册必须成功
	}

	router := NewDefaultRouter(app) // 注册期 panic 直接令测试失败

	// 健康检查端点：公开访问，返回 200。
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/health = %d", rec.Code)
	}

	// /api/dag/running：调度器未启用时应返回 503 而非 404/panic
	// （证明静态段未被参数段 :id 吞掉）。
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dag/running", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/api/dag/running = %d, want 503", rec.Code)
	}

	// 会话集合端点可达（mock agent 返回空列表）。
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/sessions = %d, body=%s", rec.Code, rec.Body.String())
	}
}
