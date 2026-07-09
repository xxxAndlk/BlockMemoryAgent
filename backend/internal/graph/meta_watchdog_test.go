package graph

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/blockmemory/agent/backend/internal/cmdqueue"
	"github.com/blockmemory/agent/backend/internal/config"
	rtPkg "github.com/blockmemory/agent/backend/internal/runtime"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// emptyRoleConfig 返回一个空的角色配置，避免 NewRoleRegistry 对 nil 解引用。
func emptyRoleConfig(t *testing.T) *pkgconfig.RoleConfigFile {
	t.Helper()
	return &pkgconfig.RoleConfigFile{}
}

// soulPath 返回仓库根目录下的 config/soul.md 路径，供 runtime.New 使用。
func soulPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	// file 位于 backend/internal/graph，上溯三级到项目根
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(root, "config", "soul.md")
}

func TestDrainCommandQueueFeatureFlags(t *testing.T) {
	sessionID := "test-session"
	ctx := context.Background()

	tests := []struct {
		name               string
		interruptEnabled   bool
		queueInjectEnabled bool
		pushInterrupt      bool
		pushEnqueue        bool
		expectInterrupted  bool
		expectEnqueued     bool
	}{
		{
			name:               "both disabled ignores interrupt",
			interruptEnabled:   false,
			queueInjectEnabled: false,
			pushInterrupt:      true,
			expectInterrupted:  false,
		},
		{
			name:               "both disabled ignores enqueue",
			interruptEnabled:   false,
			queueInjectEnabled: false,
			pushEnqueue:        true,
			expectEnqueued:     false,
		},
		{
			name:               "only interrupt enabled handles interrupt",
			interruptEnabled:   true,
			queueInjectEnabled: false,
			pushInterrupt:      true,
			expectInterrupted:  true,
		},
		{
			name:               "only queue inject enabled handles enqueue",
			interruptEnabled:   false,
			queueInjectEnabled: true,
			pushEnqueue:        true,
			expectEnqueued:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := cmdqueue.NewManager()
			rt, err := rtPkg.New(soulPath(t), nil)
			if err != nil {
				t.Fatalf("init runtime: %v", err)
			}
			rt.SetAgentConfig(&config.AgentConfig{
				FeatureTogglesConfig: config.FeatureTogglesConfig{
					InterruptEnabled:   tt.interruptEnabled,
					QueueInjectEnabled: tt.queueInjectEnabled,
				},
			})
			rt.CmdQueue = mgr

			node := NewMetaAgentNode(NewRoleRegistry(emptyRoleConfig(t)), nil, 10, 0)
			node.SetRuntime(rt)

			if tt.pushInterrupt {
				if err := mgr.Enqueue(sessionID, cmdqueue.Item{Content: "新指令", Intent: cmdqueue.IntentInterrupt}); err != nil {
					t.Fatalf("enqueue interrupt: %v", err)
				}
			}
			if tt.pushEnqueue {
				if err := mgr.Enqueue(sessionID, cmdqueue.Item{Content: "追加指令", Intent: cmdqueue.IntentEnqueue}); err != nil {
					t.Fatalf("enqueue enqueue: %v", err)
				}
			}

			state := &types.ThreeLayerState{SessionID: sessionID, ActiveBlocks: make(map[string]*types.SessionBlock)}
			interrupted := node.drainCommandQueue(ctx, state)

			if interrupted != tt.expectInterrupted {
				t.Fatalf("expect interrupted=%v, got %v", tt.expectInterrupted, interrupted)
			}
			if tt.expectEnqueued {
				if len(state.Messages) != 1 || state.Messages[0].Content != "追加指令" {
					t.Fatalf("enqueue 应追加消息，got messages=%v", state.Messages)
				}
			}
			if tt.expectInterrupted {
				if state.DomainGoal != "新指令" {
					t.Fatalf("interrupt 应重置 DomainGoal，got %s", state.DomainGoal)
				}
			}
		})
	}
}

func TestHumanClarifyEnabledRespectsConfig(t *testing.T) {
	rtEnabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtEnabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{HumanClarifyEnabled: true}})

	rtDisabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtDisabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{HumanClarifyEnabled: false}})

	nodeEnabled := NewMetaAgentNode(NewRoleRegistry(emptyRoleConfig(t)), nil, 10, 0)
	nodeEnabled.SetRuntime(rtEnabled)

	nodeDisabled := NewMetaAgentNode(NewRoleRegistry(emptyRoleConfig(t)), nil, 10, 0)
	nodeDisabled.SetRuntime(rtDisabled)

	if !nodeEnabled.humanClarifyEnabled() {
		t.Fatal("配置启用时 humanClarifyEnabled 应返回 true")
	}
	if nodeDisabled.humanClarifyEnabled() {
		t.Fatal("配置关闭时 humanClarifyEnabled 应返回 false")
	}
}
