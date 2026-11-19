package model

// factory_registry_autouse_test.go 覆盖 2026-11-18 新装机的模型自动选用规则：
//   - 注册表只有一条模型 → 未绑定/引用不到条目的角色自动用这条（meta 优先）；
//   - 非 meta 角色未设置模型 → 跟随 meta（连接参数跟 meta，行为参数取自己）；
//   - 新增模型时若 meta 尚无可用模型 → 自动绑定 meta（落盘，后续再加模型不丢）；
//   - 空注册表（0 字节 models.json）也要能加载并承载"新增模型"。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newEmptyRegistryFactory 构造"roles.yaml 只写 model_ref、注册表为空"的典型新装机形态。
func newEmptyRegistryFactory(t *testing.T) (*ModelFactory, *config.RegistryStore, string) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{ModelConfig: types.AgentModelConfig{ModelRef: "doubao-seed-evolving", Temperature: 0.1, Thinking: "low"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{ModelRef: "ark-glm-5.3-flash", Temperature: 0.2}},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "code", ModelConfig: types.AgentModelConfig{ModelRef: "ark-deepseek-v4-flash", MaxTokens: 2048}},
		},
	}
	f := NewModelFactory(cfg)
	regPath := filepath.Join(t.TempDir(), "models.json")
	store, err := config.NewRegistryStore(regPath)
	if err != nil {
		t.Fatalf("registry store: %v", err)
	}
	f.SetRegistry(store)
	return f, store, regPath
}

// TestSoleModelAutoSelection 只有一条模型时，meta 与所有未设置模型的角色都自动用它。
func TestSoleModelAutoSelection(t *testing.T) {
	f, store, _ := newEmptyRegistryFactory(t)
	if err := store.Add(types.ModelEntry{ID: "my-model", Provider: "openai-chat", Model: "deepseek-v4", APIKey: "k-1"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	metaCfg, err := f.resolveConfig("meta")
	if err != nil {
		t.Fatalf("sole model should resolve meta: %v", err)
	}
	if metaCfg.Model != "deepseek-v4" || metaCfg.APIKey != "k-1" || metaCfg.Provider != "openai-chat" {
		t.Fatalf("meta cfg = %+v, want sole model", metaCfg)
	}
	// 行为参数仍是 meta 自己的。
	if metaCfg.Temperature != 0.1 || metaCfg.Thinking != "low" {
		t.Fatalf("meta behavior params lost: %+v", metaCfg)
	}
	// 固定角色的 model_ref 解析不到 → 跟随 meta（连接取 meta），行为参数保留自己。
	codeCfg, err := f.resolveConfig("code_assistant")
	if err != nil {
		t.Fatalf("follow meta: %v", err)
	}
	if codeCfg.Model != "deepseek-v4" || codeCfg.APIKey != "k-1" {
		t.Fatalf("code cfg = %+v, want meta connection", codeCfg)
	}
	if codeCfg.MaxTokens != 2048 {
		t.Fatalf("code behavior params lost: %+v", codeCfg)
	}
}

// TestAddModelAutoBindsMeta 空注册表新增第一条模型 → meta 自动绑定并落盘；再加一条不影响。
func TestAddModelAutoBindsMeta(t *testing.T) {
	f, store, regPath := newEmptyRegistryFactory(t)
	if _, err := f.resolveConfig("meta"); err == nil {
		t.Fatal("空注册表时 meta 不应解析成功")
	}
	if err := f.AddModel(types.ModelEntry{Provider: "openai-chat", Model: "my/deepseek", APIKey: "k-1"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	b, ok := store.Binding("meta")
	if !ok || b.ModelID == "" {
		t.Fatalf("meta 应自动绑定: %+v ok=%v", b, ok)
	}
	// 绑定落盘（重启后仍然有效）。
	disk, err := config.LoadModelsRegistry(regPath)
	if err != nil || disk == nil {
		t.Fatalf("load registry: %v %v", disk, err)
	}
	if disk.RoleBindings["meta"].ModelID != b.ModelID {
		t.Fatalf("binding not persisted: %+v", disk.RoleBindings)
	}
	// 再加一条：meta 绑定不变（不会被"最新一条"抢走）。
	if err := f.AddModel(types.ModelEntry{Provider: "anthropic", Model: "second", APIKey: "k-2"}); err != nil {
		t.Fatalf("AddModel second: %v", err)
	}
	if got, _ := store.Binding("meta"); got.ModelID != b.ModelID {
		t.Fatalf("meta binding changed after second add: %+v", got)
	}
	// 两条模型时，非 meta 角色跟随 meta（不是跟随"唯一模型"）。
	if cfgResolved, err := f.resolveConfig("code_assistant"); err != nil || cfgResolved.APIKey != "k-1" {
		t.Fatalf("follow meta after 2 models: %+v err=%v", cfgResolved, err)
	}
}

// TestAddModelKeepsExplicitMetaBinding 显式绑定已存在时不覆盖。
func TestAddModelKeepsExplicitMetaBinding(t *testing.T) {
	f, store, _ := newEmptyRegistryFactory(t)
	if err := store.Add(types.ModelEntry{ID: "chosen", Provider: "openai-chat", Model: "chosen-model", APIKey: "k-chosen"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.SetBinding("meta", "chosen", ""); err != nil {
		t.Fatalf("set binding: %v", err)
	}
	if err := f.AddModel(types.ModelEntry{ID: "other", Provider: "anthropic", Model: "other-model", APIKey: "k-other"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	if b, _ := store.Binding("meta"); b.ModelID != "chosen" {
		t.Fatalf("显式绑定被覆盖: %+v", b)
	}
}

// TestEmptyRegistryFileLoadsAsEmpty 0 字节 models.json 应加载成空注册表（而不是加载失败），
// 并允许新增模型（此前正是这里失败：registry=nil → "模型注册表未启用"）。
func TestEmptyRegistryFileLoadsAsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	store, err := config.NewRegistryStore(path)
	if err != nil {
		t.Fatalf("0 字节注册表不应加载失败: %v", err)
	}
	if len(store.Models()) != 0 {
		t.Fatalf("应为空注册表: %+v", store.Models())
	}
	if err := store.Add(types.ModelEntry{ID: "m1", Provider: "openai-chat", Model: "m1", APIKey: "k"}); err != nil {
		t.Fatalf("空注册表应可新增: %v", err)
	}
}
