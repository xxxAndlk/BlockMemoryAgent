package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestLoadModelOverridesMissingFile(t *testing.T) {
	f, err := LoadModelOverrides(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if f != nil {
		t.Fatalf("missing file should return nil file, got %+v", f)
	}
}

func TestModelOverridesSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_overrides.yaml")
	now := time.Now().UTC().Truncate(time.Second)
	src := &ModelOverridesFile{Overrides: map[string]ModelOverride{
		"meta":   {PresetID: "glm-flash", AppliedAt: &now},
		"domain": {PresetID: "deepseek-v4"},
	}}
	if err := src.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadModelOverrides(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Overrides) != 2 {
		t.Fatalf("want 2 overrides, got %d", len(got.Overrides))
	}
	if got.Overrides["meta"].PresetID != "glm-flash" {
		t.Fatalf("meta preset = %q, want glm-flash", got.Overrides["meta"].PresetID)
	}
	if got.Overrides["meta"].AppliedAt == nil || !got.Overrides["meta"].AppliedAt.Equal(now) {
		t.Fatalf("meta applied_at = %v, want %v", got.Overrides["meta"].AppliedAt, now)
	}
	if got.Overrides["domain"].AppliedAt != nil {
		t.Fatalf("domain applied_at should be nil, got %v", got.Overrides["domain"].AppliedAt)
	}
}

func TestLoadModelOverridesCorruptYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("overrides: [broken"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadModelOverrides(path); err == nil {
		t.Fatal("corrupt yaml should return error")
	}
}

func TestResolveEnvVarsForPresets(t *testing.T) {
	t.Setenv("BMA_TEST_PRESET_KEY", "sk-test-123")
	t.Setenv("BMA_TEST_PRESET_URL", "http://preset.example.com")
	cfg := &RoleConfigFile{
		ModelPresets: []types.ModelPreset{
			{ID: "a", APIKey: "${BMA_TEST_PRESET_KEY}", BaseURL: "${BMA_TEST_PRESET_URL}"},
			{ID: "b", APIKey: "${BMA_TEST_MISSING:defkey}"},
		},
	}
	cfg.resolveEnvVars()
	if cfg.ModelPresets[0].APIKey != "sk-test-123" {
		t.Fatalf("preset a api_key = %q, want sk-test-123", cfg.ModelPresets[0].APIKey)
	}
	if cfg.ModelPresets[0].BaseURL != "http://preset.example.com" {
		t.Fatalf("preset a base_url = %q", cfg.ModelPresets[0].BaseURL)
	}
	if cfg.ModelPresets[1].APIKey != "defkey" {
		t.Fatalf("preset b api_key = %q, want defkey (default fallback)", cfg.ModelPresets[1].APIKey)
	}
}

func TestGetModelPreset(t *testing.T) {
	cfg := &RoleConfigFile{ModelPresets: []types.ModelPreset{{ID: "glm-flash", Model: "glm-5.3-flash"}}}
	p, ok := cfg.GetModelPreset("glm-flash")
	if !ok || p.Model != "glm-5.3-flash" {
		t.Fatalf("GetModelPreset hit failed: %+v ok=%v", p, ok)
	}
	if _, ok := cfg.GetModelPreset("nope"); ok {
		t.Fatal("GetModelPreset miss should return false")
	}
}
