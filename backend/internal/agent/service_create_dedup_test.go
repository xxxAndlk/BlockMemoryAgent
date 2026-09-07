// Package agent CreateSession 防重复提交幂等闸测试。
// 背景：web 端重载页面/新标签页重发同一目标时，本地无 activeSession 会再次走
// 建会话接口，实证（2026-09-07）产生两个并行执行同一任务的 session 白跑 1 小时。
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// blockedProvider 阻塞首次 Generate 直到 release 关闭，保证首个会话保持 running。
type blockedProvider struct{ release chan struct{} }

func (p blockedProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-p.release
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}
func (p blockedProvider) Name() string { return "blocked" }

// newDedupTestService 构造最小可用的 ReactService。
func newDedupTestService(t *testing.T, provider ModelProvider) *ReactService {
	t.Helper()
	roleRegistry := role.NewRegistry(&pkgconfig.RoleConfigFile{})
	toolRegistry := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	s.testProvider = provider
	s.store.workDir = t.TempDir()
	return s
}

// TestCreateSession_DedupsIdenticalRunningGoal 验证：同 goal 重复建会话返回既有
// running 会话；goal 归一化 trim 生效；非 running 态放行新建；不同 goal 不受影响。
func TestCreateSession_DedupsIdenticalRunningGoal(t *testing.T) {
	release := make(chan struct{})
	s := newDedupTestService(t, blockedProvider{release: release})
	defer close(release)

	ctx := context.Background()

	first, err := s.CreateSession(ctx, CreateRequest{Goal: "  做个塔防游戏  "})
	if err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}

	// 同 goal（trim 归一化）：返回既有会话而非新建。
	second, err := s.CreateSession(ctx, CreateRequest{Goal: "做个塔防游戏"})
	if err != nil {
		t.Fatalf("second CreateSession: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected duplicate to return existing session %s, got new %s", first.ID, second.ID)
	}

	// 不同 goal：正常新建。
	other, err := s.CreateSession(ctx, CreateRequest{Goal: "写一份调研报告"})
	if err != nil {
		t.Fatalf("other CreateSession: %v", err)
	}
	if other.ID == first.ID {
		t.Fatalf("different goal must create new session, got %s", other.ID)
	}

	// 既有会话不再是 running：放行新建（awaiting_clarify/paused/error 同理放行）。
	s.store.mu.Lock()
	if internal := s.store.sessions[first.ID]; internal != nil {
		internal.Status = enums.SessionStatusError
	}
	s.store.mu.Unlock()
	after, err := s.CreateSession(ctx, CreateRequest{Goal: "做个塔防游戏"})
	if err != nil {
		t.Fatalf("after-error CreateSession: %v", err)
	}
	if after.ID == first.ID {
		t.Fatalf("non-running session must not dedup, got %s", after.ID)
	}
}

// TestFindRunningDuplicateSession_PicksNewest 验证：多个同 goal running 会话取最新。
// 直塞 store 构造确定性状态（createSession 会启动 runSession goroutine，引入竞态）。
func TestFindRunningDuplicateSession_PicksNewest(t *testing.T) {
	s := newDedupTestService(t, nil)

	now := time.Now()
	a := &reactInternalSession{ID: "sess-a", Goal: "同目标", Status: enums.SessionStatusRunning, StartedAt: now.Add(-2 * time.Second)}
	b := &reactInternalSession{ID: "sess-b", Goal: "同目标", Status: enums.SessionStatusRunning, StartedAt: now}
	done := &reactInternalSession{ID: "sess-done", Goal: "同目标", Status: enums.SessionStatusCompleted, StartedAt: now.Add(time.Second)}

	s.store.mu.Lock()
	s.store.sessions[a.ID] = a
	s.store.sessions[b.ID] = b
	s.store.sessions[done.ID] = done
	s.store.mu.Unlock()

	got := s.store.findRunningDuplicateSession("同目标")
	if got == nil || got.ID != b.ID {
		t.Fatalf("expected newest running session sess-b, got %+v", got)
	}
}
