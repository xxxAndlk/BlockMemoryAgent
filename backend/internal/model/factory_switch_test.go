package model

// factory_switch_test.go 覆盖模型动态切换（models.json 注册表版）：
// 切换成功 + 绑定落盘、探测失败 fail-closed、校验（动态角色/未知条目/api_key 空）、
// 同绑定短路、models.json 热更新缓存失效、model_ref/内联合并序、lightweight 回退边。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newSwitchTestFactory 构造带临时 models.json 注册表的工厂。
// 角色覆盖：meta/domain 内联连接参数、code_assistant 纯内联、ref_role 走 model_ref
// （带行为参数验证保留）、lightweight 留空（回退 domain）。
func newSwitchTestFactory(t *testing.T) (*ModelFactory, *config.RegistryStore, string) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:        config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "meta-old", APIKey: "k-meta"}},
		DomainAgent:      config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "domain-old", APIKey: "k-domain"}},
		LightweightModel: types.AgentModelConfig{},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "code", ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "code-old", APIKey: "k-code"}},
			{ID: "ref_role", Name: "ref", ModelConfig: types.AgentModelConfig{ModelRef: "glm-flash", Temperature: 0.5, MaxTokens: 4096}},
		},
	}
	f := NewModelFactory(cfg)
	regPath := filepath.Join(t.TempDir(), "models.json")
	store, err := config.NewRegistryStore(regPath)
	if err != nil {
		t.Fatalf("registry store: %v", err)
	}
	for _, e := range []types.ModelEntry{
		{ID: "glm-flash", Provider: "openai", Model: "glm-5.3-flash", APIKey: "k-glm"},
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4", APIKey: "k-ds"},
	} {
		if err := store.Add(e); err != nil {
			t.Fatalf("add registry entry: %v", err)
		}
	}
	f.SetRegistry(store)
	return f, store, regPath
}

func TestSwitchModelSuccess(t *testing.T) {
	f, _, regPath := newSwitchTestFactory(t)
	probed := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		probed++
		if cfg.Model != "glm-5.3-flash" || cfg.MaxTokens != 1 || cfg.APIKey != "k-glm" {
			t.Fatalf("probe cfg = %+v, want glm-5.3-flash MaxTokens=1", cfg)
		}
		return nil
	}
	got, err := f.SwitchModel(context.Background(), "meta", "glm-flash", "high")
	if err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if got.Model != "glm-5.3-flash" || got.Thinking != "high" {
		t.Fatalf("applied cfg = %+v", got)
	}
	st, err := f.CurrentModelInfo("meta")
	if err != nil {
		t.Fatalf("CurrentModelInfo: %v", err)
	}
	if !st.Bound || st.ModelID != "glm-flash" || st.Model != "glm-5.3-flash" || st.Thinking != "high" {
		t.Fatalf("CurrentModelInfo = %+v", st)
	}
	if probed != 1 {
		t.Fatalf("probe called %d times, want 1", probed)
	}
	disk, err := config.LoadModelsRegistry(regPath)
	if err != nil || disk == nil || disk.RoleBindings["meta"].ModelID != "glm-flash" {
		t.Fatalf("binding 应落盘: %+v err=%v", disk, err)
	}

	// 第二次切换保留既有绑定（合并落盘）。
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { return nil }
	if _, err := f.SwitchModel(context.Background(), "domain", "deepseek", ""); err != nil {
		t.Fatalf("second SwitchModel: %v", err)
	}
	disk, err = config.LoadModelsRegistry(regPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if disk.RoleBindings["meta"].ModelID != "glm-flash" || disk.RoleBindings["domain"].ModelID != "deepseek" {
		t.Fatalf("bindings not merged: %+v", disk.RoleBindings)
	}

	// 切换带 model_ref 的角色：连接参数换、行为参数保留。
	got, err = f.SwitchModel(context.Background(), "ref_role", "deepseek", "")
	if err != nil {
		t.Fatalf("ref_role switch: %v", err)
	}
	if got.Model != "deepseek-v4" || got.Temperature != 0.5 || got.MaxTokens != 4096 {
		t.Fatalf("ref_role 切换后行为参数丢失: %+v", got)
	}
}

func TestSwitchModelProbeFailClosed(t *testing.T) {
	f, _, regPath := newSwitchTestFactory(t)
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		return errors.New("connection refused")
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash", "high"); err == nil {
		t.Fatal("probe failure should return error")
	}
	if st, err := f.CurrentModelInfo("meta"); err != nil || st.Bound {
		t.Fatalf("fail-closed: 绑定不得生效, st=%+v err=%v", st, err)
	}
	if disk, _ := config.LoadModelsRegistry(regPath); disk != nil && len(disk.RoleBindings) > 0 {
		t.Fatalf("fail-closed: 绑定不得落盘, got %+v", disk.RoleBindings)
	}
}

func TestSwitchModelValidation(t *testing.T) {
	f, store, _ := newSwitchTestFactory(t)
	probes := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { probes++; return nil }
	if _, err := f.SwitchModel(context.Background(), "dynamic_helper", "glm-flash", ""); err == nil {
		t.Fatal("dynamic role should be rejected")
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "no-such-model", ""); err == nil {
		t.Fatal("unknown model id should be rejected")
	}
	if err := store.Add(types.ModelEntry{ID: "nokey", Provider: "openai", Model: "m", APIKey: ""}); err != nil {
		t.Fatalf("add nokey entry: %v", err)
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "nokey", ""); err == nil {
		t.Fatal("api_key 为空的条目应拒绝切换")
	}
	if probes != 0 {
		t.Fatalf("校验失败不应探测, probes=%d", probes)
	}
}

// TestSwitchableRolesWhitelist 验证 selectable_roles 白名单：白名单外角色切换/实例覆盖
// 均拒绝且不探测；白名单内角色照常；同绑定短路先于白名单（已绑定的白名单外条目可重申）。
func TestSwitchableRolesWhitelist(t *testing.T) {
	f, store, _ := newSwitchTestFactory(t)
	probes := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { probes++; return nil }
	if err := store.Add(types.ModelEntry{ID: "meta-only", Provider: "openai", Model: "k3", APIKey: "k-k3", SelectableRoles: []string{"meta"}}); err != nil {
		t.Fatalf("add meta-only entry: %v", err)
	}
	// 白名单外角色：SwitchModel 与 SetAgentModel 均拒绝，不探测。
	if _, err := f.SwitchModel(context.Background(), "domain", "meta-only", ""); err == nil {
		t.Fatal("domain 切换到 meta-only 条目应拒绝")
	}
	if _, err := f.SetAgentModel(context.Background(), "s1/domain-1", "domain", "meta-only", ""); err == nil {
		t.Fatal("domain 实例覆盖 meta-only 条目应拒绝")
	}
	// 同绑定短路先于白名单：meta 已绑 meta-only 后重申成功（不探测）。
	if _, err := f.SwitchModel(context.Background(), "meta", "meta-only", ""); err != nil {
		t.Fatalf("meta 切换到 meta-only: %v", err)
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "meta-only", ""); err != nil {
		t.Fatalf("同绑定重申应短路放行: %v", err)
	}
	// 白名单内角色（meta）实例覆盖可用。
	if _, err := f.SetAgentModel(context.Background(), "s1", "meta", "meta-only", ""); err != nil {
		t.Fatalf("meta 实例覆盖 meta-only: %v", err)
	}
	if probes != 3 {
		t.Fatalf("probe called %d times, want 3 (meta 切换 + meta 实例覆盖 + domain 普通条目)", probes)
	}
	// 无白名单条目不受影响。
	if _, err := f.SwitchModel(context.Background(), "domain", "glm-flash", ""); err != nil {
		t.Fatalf("domain 切换普通条目: %v", err)
	}
}

func TestSwitchModelSameBindingShortCircuit(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	calls := 0
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error { calls++; return nil }
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash", ""); err != nil {
		t.Fatalf("first switch: %v", err)
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash", ""); err != nil {
		t.Fatalf("second switch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("same binding should short-circuit, probe called %d times", calls)
	}
	// 同模型但思考档不同：不短路。
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash", "low"); err != nil {
		t.Fatalf("thinking override switch: %v", err)
	}
	if calls != 2 {
		t.Fatalf("thinking 变更应触发探测, probes=%d", calls)
	}
}

func TestHotReloadInvalidatesCache(t *testing.T) {
	f, _, regPath := newSwitchTestFactory(t)

	// 预置缓存：ref_role（model_ref 解析 glm-flash）+ code_assistant（内联）。
	refCfg, err := f.resolveConfig("ref_role")
	if err != nil {
		t.Fatalf("resolve ref_role: %v", err)
	}
	f.mu.Lock()
	f.models["ref_role"] = cachedClient{client: &fakeLLM{resp: "x"}, cfg: refCfg}
	f.models["code_assistant"] = cachedClient{client: &fakeLLM{resp: "x"},
		cfg: types.AgentModelConfig{Provider: "openai", Model: "code-old", APIKey: "k-code"}}
	f.mu.Unlock()

	// 手改 models.json：glm-flash 条目 model 变化 → ref_role 生效配置变化 → 缓存删除。
	newContent := `{"models":[
		{"id":"glm-flash","provider":"openai","model":"glm-5.3-flash-v2","api_key":"k-glm"},
		{"id":"deepseek","provider":"openai","model":"deepseek-v4","api_key":"k-ds"}]}`
	if err := os.WriteFile(regPath, []byte(newContent), 0o600); err != nil {
		t.Fatalf("rewrite models.json: %v", err)
	}
	f.checkRegistryReload()
	f.mu.RLock()
	_, refCached := f.models["ref_role"]
	_, codeCached := f.models["code_assistant"]
	f.mu.RUnlock()
	if refCached {
		t.Fatal("生效配置变化的缓存条目应被删除")
	}
	if !codeCached {
		t.Fatal("内联配置不受注册表影响的缓存应保留")
	}

	// 切换走新条目内容（热更新已生效）。
	f.probeHook = func(ctx context.Context, cfg types.AgentModelConfig) error {
		if cfg.Model != "glm-5.3-flash-v2" {
			t.Fatalf("probe cfg.Model = %q, want 热更新后的 glm-5.3-flash-v2", cfg.Model)
		}
		return nil
	}
	if _, err := f.SwitchModel(context.Background(), "meta", "glm-flash", ""); err != nil {
		t.Fatalf("switch after hot reload: %v", err)
	}
}

func TestResolveConfigMergeOrder(t *testing.T) {
	f, store, _ := newSwitchTestFactory(t)

	// model_ref：连接参数取注册表条目，行为参数保留角色侧。
	cfg, err := f.resolveConfig("ref_role")
	if err != nil {
		t.Fatalf("resolve ref_role: %v", err)
	}
	if cfg.Model != "glm-5.3-flash" || cfg.Provider != "openai" || cfg.APIKey != "k-glm" {
		t.Fatalf("model_ref 连接参数未合并: %+v", cfg)
	}
	if cfg.Temperature != 0.5 || cfg.MaxTokens != 4096 {
		t.Fatalf("行为参数应保留角色侧: %+v", cfg)
	}

	// 内联兜底：无 model_ref 角色照常。
	cfg, err = f.resolveConfig("code_assistant")
	if err != nil || cfg.Model != "code-old" {
		t.Fatalf("inline fallback = %+v err=%v", cfg, err)
	}

	// binding 优先于 model_ref：ref_role 绑定 deepseek。
	if err := store.SetBinding("ref_role", "deepseek", "low"); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	cfg, err = f.resolveConfig("ref_role")
	if err != nil || cfg.Model != "deepseek-v4" || cfg.Thinking != "low" {
		t.Fatalf("binding 应优先: %+v err=%v", cfg, err)
	}
	// CurrentModelInfo 反映绑定来源。
	if st, _ := f.CurrentModelInfo("ref_role"); !st.Bound || st.ModelID != "deepseek" {
		t.Fatalf("CurrentModelInfo = %+v", st)
	}

	// binding 指向已删除条目：fail-open 回落角色自身配置（model_ref）。
	if err := store.SetBinding("code_assistant", "gone-model", ""); err != nil {
		t.Fatalf("SetBinding gone: %v", err)
	}
	cfg, err = f.resolveConfig("code_assistant")
	if err != nil || cfg.Model != "code-old" {
		t.Fatalf("binding fail-open = %+v err=%v", cfg, err)
	}
}

// TestMaxOutputTokensResolution 输出上限解析链：角色侧 max_tokens（>0）优先，
// 其次条目 max_output_tokens，两者皆无保持 0（provider 省略/回退端点默认）。
func TestMaxOutputTokensResolution(t *testing.T) {
	f, store, _ := newSwitchTestFactory(t)
	if err := store.Add(types.ModelEntry{ID: "outmodel", Provider: "openai", Model: "m-out", APIKey: "k-out", MaxOutputTokens: 32000}); err != nil {
		t.Fatalf("add entry: %v", err)
	}
	if err := store.Add(types.ModelEntry{ID: "noout", Provider: "openai", Model: "m-noout", APIKey: "k-noout"}); err != nil {
		t.Fatalf("add entry: %v", err)
	}

	// meta 无角色侧 max_tokens：绑定到条目后取条目值。
	if err := store.SetBinding("meta", "outmodel", ""); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	cfg, err := f.resolveConfig("meta")
	if err != nil {
		t.Fatalf("resolve meta: %v", err)
	}
	if cfg.MaxTokens != 32000 {
		t.Fatalf("条目 max_output_tokens 未生效: %+v", cfg)
	}

	// ref_role 角色侧显式 4096：覆盖条目值。
	if err := store.SetBinding("ref_role", "outmodel", ""); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	cfg, err = f.resolveConfig("ref_role")
	if err != nil {
		t.Fatalf("resolve ref_role: %v", err)
	}
	if cfg.MaxTokens != 4096 {
		t.Fatalf("角色侧 max_tokens 应优先: %+v", cfg)
	}

	// 条目未声明上限：保持 0。
	if err := store.SetBinding("meta", "noout", ""); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	cfg, err = f.resolveConfig("meta")
	if err != nil {
		t.Fatalf("resolve meta: %v", err)
	}
	if cfg.MaxTokens != 0 {
		t.Fatalf("无声明应保持 0: %+v", cfg)
	}
}

func TestModelRefMissingFailsStrict(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "meta-old", APIKey: "k-meta"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "domain-old", APIKey: "k-domain"}},
		FixedRoles: []types.RoleDefinition{
			{ID: "bad_ref", Name: "bad", ModelConfig: types.AgentModelConfig{ModelRef: "gone"}},
		},
	}
	f := NewModelFactory(cfg)
	regPath := filepath.Join(t.TempDir(), "models.json")
	store, err := config.NewRegistryStore(regPath)
	if err != nil {
		t.Fatalf("registry store: %v", err)
	}
	f.SetRegistry(store)
	if _, err := f.GetModel(context.Background(), "bad_ref"); err == nil {
		t.Fatal("model_ref 解析不到应报错（strict startup）")
	}
	if _, err := f.CurrentModelInfo("bad_ref"); err == nil {
		t.Fatal("CurrentModelInfo 同样应报错")
	}
}

func TestLightweightFallbackEdge(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	// lightweight 未配置：回退 domain 配置（非绑定来源）。
	if st, err := f.CurrentModelInfo("lightweight"); err != nil || st.Model != "domain-old" || st.Bound {
		t.Fatalf("lightweight fallback = %+v err=%v", st, err)
	}
	// 配置 model_ref 后独立生效。
	f.cfg.LightweightModel = types.AgentModelConfig{ModelRef: "glm-flash", MaxTokens: 2048}
	cfg, err := f.resolveConfig("lightweight")
	if err != nil || cfg.Model != "glm-5.3-flash" || cfg.MaxTokens != 2048 {
		t.Fatalf("lightweight model_ref = %+v err=%v", cfg, err)
	}
}

func TestSwitchableRoles(t *testing.T) {
	f, _, _ := newSwitchTestFactory(t)
	roles := f.SwitchableRoles()
	want := map[string]bool{"meta": true, "domain": true, "lightweight": true, "code_assistant": true, "ref_role": true}
	if len(roles) != len(want) {
		t.Fatalf("SwitchableRoles = %v", roles)
	}
	for _, r := range roles {
		if !want[r] {
			t.Fatalf("unexpected role %q in %v", r, roles)
		}
	}
}

func TestAddModelGeneratesID(t *testing.T) {
	f, store, regPath := newSwitchTestFactory(t)
	// 无 id：由 model 名 slug 生成。
	if err := f.AddModel(types.ModelEntry{Provider: "openai-chat", Model: "Kimi K3!"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	if _, ok := store.ModelByID("kimi-k3"); !ok {
		t.Fatal("应生成 slug id kimi-k3")
	}
	// 再加同名：追加后缀去重。
	if err := f.AddModel(types.ModelEntry{Provider: "openai-chat", Model: "Kimi K3!"}); err != nil {
		t.Fatalf("AddModel dup: %v", err)
	}
	if _, ok := store.ModelByID("kimi-k3-2"); !ok {
		t.Fatal("重复 model 名应生成 -2 后缀 id")
	}
	disk, err := config.LoadModelsRegistry(regPath)
	if err != nil || len(disk.Models) != 4 {
		t.Fatalf("新增应落盘: %+v err=%v", disk, err)
	}
	// registry 未启用时拒绝。
	bare := NewModelFactory(&config.RoleConfigFile{})
	if err := bare.AddModel(types.ModelEntry{ID: "x", Provider: "p", Model: "m"}); err == nil {
		t.Fatal("无注册表应报错")
	}
}
