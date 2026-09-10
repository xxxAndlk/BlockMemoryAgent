package model

// factory_agent_override_test.go 覆盖实例级模型覆盖（set_agent_model 的工厂层）：
// 覆盖优先于角色绑定、未覆盖回落、探活失败不落覆盖、不落盘、Clear/ClearByPrefix、
// 与角色缓存互相隔离（角色换绑定不影响已覆盖实例，覆盖不影响同角色其他实例）。

import (
	"context"
	"errors"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// agentProviderName 解析实例级 provider 的模型名（provider.Name() 即端点模型名）。
func agentProviderName(t *testing.T, f *ModelFactory, roleID, agentID string) string {
	t.Helper()
	p, err := f.GetBladesProviderForAgent(context.Background(), roleID, agentID)
	if err != nil {
		t.Fatalf("GetBladesProviderForAgent(%s,%s): %v", roleID, agentID, err)
	}
	return p.Name()
}

func TestSetAgentModel_OverridePriorityAndFallback(t *testing.T) {
	f, _, regPath := newSwitchTestFactory(t)
	probes := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		probes++
		if cfg.Model != "glm-5.3-flash" || cfg.MaxTokens != 1 {
			t.Fatalf("probe cfg = %+v, want glm-5.3-flash MaxTokens=1", cfg)
		}
		return nil
	}
	// 未覆盖：实例解析 = 角色级解析。
	if got := agentProviderName(t, f, "domain", "s1/domain-1"); got != "domain-old" {
		t.Fatalf("no override should fall back to role provider, got %q", got)
	}
	// 覆盖后：该实例用新模型，同角色其他实例与角色级解析不变。
	cfg, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "glm-flash", "high")
	if err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}
	if cfg.Model != "glm-5.3-flash" || cfg.Thinking != "high" || cfg.APIKey != "k-glm" {
		t.Fatalf("applied cfg = %+v", cfg)
	}
	if got := agentProviderName(t, f, "domain", "s1/domain-1"); got != "glm-5.3-flash" {
		t.Fatalf("override should win, got %q", got)
	}
	if got := agentProviderName(t, f, "domain", "s1/domain-2"); got != "domain-old" {
		t.Fatalf("sibling instance must be unaffected, got %q", got)
	}
	roleProv, err := f.GetBladesProvider(context.Background(), "domain")
	if err != nil {
		t.Fatalf("role provider: %v", err)
	}
	if roleProv.Name() != "domain-old" {
		t.Fatalf("role-level resolution must be unaffected, got %q", roleProv.Name())
	}
	if probes != 1 {
		t.Fatalf("probe called %d times, want 1", probes)
	}
	// 不落盘：models.json 绑定不得出现。
	if disk, _ := config.LoadModelsRegistry(regPath); disk != nil && len(disk.RoleBindings) > 0 {
		t.Fatalf("agent override must not persist, got %+v", disk.RoleBindings)
	}
	// 清单可见（list_models 的 agent_overrides 段数据源）。
	ovs := f.AgentOverrides()
	if len(ovs) != 1 || ovs[0].AgentID != "s1/domain-1" || ovs[0].RoleID != "domain" ||
		ovs[0].ModelID != "glm-flash" || ovs[0].Model != "glm-5.3-flash" || ovs[0].Thinking != "high" {
		t.Fatalf("AgentOverrides = %+v", ovs)
	}
	// Clear 后回落角色级。
	f.ClearAgentModel("s1/domain-1")
	if got := agentProviderName(t, f, "domain", "s1/domain-1"); got != "domain-old" {
		t.Fatalf("after clear should fall back, got %q", got)
	}
	if len(f.AgentOverrides()) != 0 {
		t.Fatalf("overrides should be empty after clear, got %+v", f.AgentOverrides())
	}
	// 空 agentID 回落角色级（无实例语义的调用点）。
	if got := agentProviderName(t, f, "domain", ""); got != "domain-old" {
		t.Fatalf("empty agentID should fall back, got %q", got)
	}
}

// TestSetAgentModel_ThinkingInherits 验证 thinking 省略时沿用角色当前生效档（含绑定覆盖），
// 不静默回退 roles.yaml。
func TestSetAgentModel_ThinkingInherits(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	// 角色先切到 thinking=low（模拟用户在 TUI 设置的角色档）。
	if _, err := f.SwitchModel(context.Background(), "domain", "deepseek", "low"); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	cfg, err := f.SetAgentModel(context.Background(), "s1/domain-9", "domain", "glm-flash", "")
	if err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}
	if cfg.Thinking != "low" {
		t.Fatalf("thinking should inherit current effective value, got %q", cfg.Thinking)
	}
}

func TestSetAgentModel_ProbeFailClosed(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		return errors.New("connection refused")
	}
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "glm-flash", ""); err == nil {
		t.Fatal("probe failure should return error")
	}
	if len(f.AgentOverrides()) != 0 {
		t.Fatalf("fail-closed: 覆盖不得生效, got %+v", f.AgentOverrides())
	}
	if got := agentProviderName(t, f, "domain", "s1/domain-1"); got != "domain-old" {
		t.Fatalf("fail-closed: 应回落角色级, got %q", got)
	}
}

func TestSetAgentModel_Validation(t *testing.T) {
	f, store, _ := newSwitchTestFactory(t)
	probes := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { probes++; return nil }
	if _, err := f.SetAgentModel(context.Background(), "", "domain", "glm-flash", ""); err == nil {
		t.Fatal("empty agent id should be rejected")
	}
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "dynamic_helper", "glm-flash", ""); err == nil {
		t.Fatal("non-switchable role should be rejected")
	}
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "no-such-model", ""); err == nil {
		t.Fatal("unknown model id should be rejected")
	}
	if err := store.Add(types.ModelEntry{ID: "nokey", Provider: "openai", Model: "m", APIKey: ""}); err != nil {
		t.Fatalf("add nokey entry: %v", err)
	}
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "nokey", ""); err == nil {
		t.Fatal("api_key 为空的条目应拒绝")
	}
	if probes != 0 {
		t.Fatalf("校验失败不应探测, probes=%d", probes)
	}
}

// TestAgentOverride_IsolatedFromRoleSwitch 验证两个方向的隔离：
// 角色级 SwitchModel（含 invalidateChangedClients）不得清掉实例覆盖；
// 实例覆盖不得影响同角色其他实例解析。
func TestAgentOverride_IsolatedFromRoleSwitch(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "glm-flash", ""); err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}
	if _, err := f.SwitchModel(context.Background(), "domain", "deepseek", ""); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if got := agentProviderName(t, f, "domain", "s1/domain-1"); got != "glm-5.3-flash" {
		t.Fatalf("override must survive role switch, got %q", got)
	}
	if got := agentProviderName(t, f, "domain", "s1/domain-2"); got != "deepseek-v4" {
		t.Fatalf("unoverridden instance should follow role binding, got %q", got)
	}
}

func TestClearAgentModelsByPrefix(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	for _, id := range []string{"s1/domain-1", "s1/domain-2", "s2/domain-1"} {
		if _, err := f.SetAgentModel(context.Background(), id, "domain", "glm-flash", ""); err != nil {
			t.Fatalf("SetAgentModel(%s): %v", id, err)
		}
	}
	f.ClearAgentModelsByPrefix("s1/")
	ovs := f.AgentOverrides()
	if len(ovs) != 1 || ovs[0].AgentID != "s2/domain-1" {
		t.Fatalf("prefix clear mismatch: %+v", ovs)
	}
}
