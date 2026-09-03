package model

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func newSwitchTestFactory(t *testing.T) (*ModelFactory, string) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:        config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "meta-old", APIKey: "k-meta"}},
		DomainAgent:      config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "domain-old", APIKey: "k-domain"}},
		LightweightModel: types.AgentModelConfig{},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "code", ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "code-old", APIKey: "k-code"}},
		},
	}
	f := NewModelFactory(cfg)
	overridesPath := filepath.Join(t.TempDir(), "model_overrides.yaml")
	f.SetModelPresets([]types.ModelPreset{
		{ID: "glm-flash", Provider: "openai", Model: "glm-5.3-flash", APIKey: "k-glm"},
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4", APIKey: "k-ds"},
	}, overridesPath)
	return f, overridesPath
}

func TestSwitchModelSuccess(t *testing.T) {
	f, overridesPath := newSwitchTestFactory(t)
	probed := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		probed++
		if cfg.Model != "glm-5.3-flash" || cfg.MaxTokens != 1 {
			t.Fatalf("probe cfg = %+v, want glm-5.3-flash MaxTokens=1", cfg)
		}
		return nil
	}
	got, err := f.SwitchModel(context.Background(), "meta", "glm-flash")
	if err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if got.Model != "glm-5.3-flash" {
		t.Fatalf("applied model = %q", got.Model)
	}
	provider, model, presetID, overridden := f.CurrentModelInfo("meta")
	if !overridden || presetID != "glm-flash" || model != "glm-5.3-flash" || provider != "openai" {
		t.Fatalf("CurrentModelInfo = (%s,%s,%s,%v)", provider, model, presetID, overridden)
	}
	if probed != 1 {
		t.Fatalf("probe called %d times, want 1", probed)
	}
	file, err := config.LoadModelOverrides(overridesPath)
	if err != nil || file == nil || file.Overrides["meta"].PresetID != "glm-flash" {
		t.Fatalf("overrides file = %+v err=%v", file, err)
	}

	// 第二次切换保留既有覆写
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	if _, err := f.SwitchModel(context.Background(), "domain", "deepseek"); err != nil {
		t.Fatalf("second SwitchModel: %v", err)
	}
	file, err = config.LoadModelOverrides(overridesPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if file.Overrides["meta"].PresetID != "glm-flash" || file.Overrides["domain"].PresetID != "deepseek" {
		t.Fatalf("overrides not merged: %+v", file.Overrides)
	}
}

func TestSwitchModelProbeFailClosed(t *testing.T) {
	f, overridesPath := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		return errors.New("connection refused")
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash"); err == nil {
		t.Fatal("probe failure should return error")
	}
	if _, _, _, overridden := f.CurrentModelInfo("meta"); overridden {
		t.Fatal("fail-closed: override must not be applied")
	}
	if file, _ := config.LoadModelOverrides(overridesPath); file != nil {
		t.Fatalf("fail-closed: overrides file must not be written, got %+v", file)
	}
}

func TestSwitchModelValidation(t *testing.T) {
	f, _ := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	if _, err := f.SwitchModel(context.Background(), "dynamic_helper", "glm-flash"); err == nil {
		t.Fatal("dynamic role should be rejected")
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "no-such-preset"); err == nil {
		t.Fatal("unknown preset should be rejected")
	}
}

func TestSwitchModelSamePresetShortCircuit(t *testing.T) {
	f, _ := newSwitchTestFactory(t)
	calls := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { calls++; return nil }
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash"); err != nil {
		t.Fatalf("first switch: %v", err)
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash"); err != nil {
		t.Fatalf("second switch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("same preset should short-circuit, probe called %d times", calls)
	}
}

func TestApplyStartupOverrides(t *testing.T) {
	f, _ := newSwitchTestFactory(t)
	file := &config.ModelOverridesFile{Overrides: map[string]config.ModelOverride{
		"meta":     {PresetID: "deepseek"},
		"obsolete": {PresetID: "gone"},
	}}
	skipped := f.ApplyStartupOverrides(file)
	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want 1 entry", skipped)
	}
	if _, _, presetID, overridden := f.CurrentModelInfo("meta"); !overridden || presetID != "deepseek" {
		t.Fatalf("startup override not applied: presetID=%q overridden=%v", presetID, overridden)
	}
	// 未覆写角色走原解析
	if _, model, _, overridden := f.CurrentModelInfo("code_assistant"); overridden || model != "code-old" {
		t.Fatalf("code_assistant = %s overridden=%v, want code-old false", model, overridden)
	}
	// nil/空文件无操作
	if skipped := f.ApplyStartupOverrides(nil); skipped != nil {
		t.Fatalf("nil file should skip nothing, got %v", skipped)
	}
}

func TestSwitchableRolesAndCurrentInfoFallback(t *testing.T) {
	f, _ := newSwitchTestFactory(t)
	roles := f.SwitchableRoles()
	want := map[string]bool{"meta": true, "domain": true, "lightweight": true, "code_assistant": true}
	if len(roles) != len(want) {
		t.Fatalf("SwitchableRoles = %v", roles)
	}
	for _, r := range roles {
		if !want[r] {
			t.Fatalf("unexpected role %q in %v", r, roles)
		}
	}
	// lightweight 无独立配置回退 domain
	if _, model, _, overridden := f.CurrentModelInfo("lightweight"); overridden || model != "domain-old" {
		t.Fatalf("lightweight fallback = %s overridden=%v, want domain-old false", model, overridden)
	}
}
