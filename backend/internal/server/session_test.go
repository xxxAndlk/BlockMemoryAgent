package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/graph"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeMetaAgentForCleanup 是一个立即结束的 MetaAgent 节点，用于会话清理测试。
type fakeMetaAgentForCleanup struct{}

func (n *fakeMetaAgentForCleanup) Name() string { return "MetaAgent" }
func (n *fakeMetaAgentForCleanup) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.SessionSummary = "done"
	state.NextAction = enums.ActionFinish
	return state, nil
}

type fakeSinkerForCleanup struct{}

func (n *fakeSinkerForCleanup) Name() string { return "Sinker" }
func (n *fakeSinkerForCleanup) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}

// TestSessionCleansTempDirOnCompletion 验证：会话正常完成后，其临时目录被自动清理。
func TestSessionCleansTempDirOnCompletion(t *testing.T) {
	workDir := t.TempDir()

	cfg := &pkgconfig.RoleConfigFile{}
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(&fakeMetaAgentForCleanup{})
	builder.AddNode(graph.NewEscalationHandlerNode())
	builder.AddNode(&fakeSinkerForCleanup{})
	g := builder.Build()

	mgr := NewSessionManager(g, registry)
	// 覆盖 workDir 为测试目录，使临时目录落在可控位置
	mgr.workDir = workDir

	session := mgr.CreateSession(context.Background(), "test goal")
	tempDir := session.TempDir
	if tempDir == "" {
		t.Fatal("新建会话应分配 TempDir")
	}

	// 在临时目录下创建一个文件，模拟 Agent 产生的临时产物
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	tempFile := filepath.Join(tempDir, "script.py")
	if err := os.WriteFile(tempFile, []byte("print('temp')"), 0644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}

	// 等待会话完成且临时目录被清理
	deadline := time.Now().Add(2 * time.Second)
	cleaned := false
	for time.Now().Before(deadline) {
		snap := mgr.SnapshotSession(session.ID)
		if snap != nil && snap.Status != enums.SessionStatusRunning {
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

// TestSessionTempDirLocation 验证：新建会话的 TempDir 位于预期位置。
func TestSessionTempDirLocation(t *testing.T) {
	workDir := t.TempDir()

	cfg := &pkgconfig.RoleConfigFile{}
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(&fakeMetaAgentForCleanup{})
	builder.AddNode(graph.NewEscalationHandlerNode())
	builder.AddNode(&fakeSinkerForCleanup{})
	g := builder.Build()

	mgr := NewSessionManager(g, registry)
	mgr.workDir = workDir

	session := mgr.CreateSession(context.Background(), "location test")
	expected := filepath.Join(workDir, ".bma", "tmp", session.ID)
	if session.TempDir != expected {
		t.Fatalf("TempDir 期望 %q，got %q", expected, session.TempDir)
	}
}
