package config

// models_registry_test.go 覆盖 config/models.json 注册表：
// 加载（缺文件/坏 JSON）、${ENV} 展开、Save/Load 往返（含 Windows 原子写覆盖）、
// 校验错误、MaybeReload mtime 热更新（含损坏文件保旧数据）、SetBinding 落盘与删除、
// Add 去重与回滚。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

func writeRegistryFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write registry file: %v", err)
	}
}

func TestLoadModelsRegistryMissing(t *testing.T) {
	f, err := LoadModelsRegistry(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || f != nil {
		t.Fatalf("缺文件应 (nil,nil)，got (%+v, %v)", f, err)
	}
}

func TestLoadModelsRegistryBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	writeRegistryFile(t, path, `{not-json`)
	if _, err := LoadModelsRegistry(path); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestLoadModelsRegistryEnvExpansion(t *testing.T) {
	t.Setenv("BMA_TEST_API_KEY", "key-abc")
	t.Setenv("BMA_TEST_EMPTY", "")
	path := filepath.Join(t.TempDir(), "models.json")
	writeRegistryFile(t, path, `{
		"models": [
			{"id": "a", "provider": "anthropic", "model": "${BMA_TEST_API_KEY}"},
			{"id": "b", "provider": "anthropic", "model": "m2", "api_key": "${BMA_TEST_API_KEY}", "base_url": "${BMA_TEST_UNSET:\"http://dft\"}"},
			{"id": "c", "provider": "anthropic", "model": "m3", "api_key": "${BMA_TEST_EMPTY}"}
		]
	}`)
	f, err := LoadModelsRegistry(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Models[0].Model != "key-abc" {
		t.Fatalf("model 展开错误: %q", f.Models[0].Model)
	}
	if f.Models[1].APIKey != "key-abc" || f.Models[1].BaseURL != "http://dft" {
		t.Fatalf("api_key/base_url 展开错误: %q %q", f.Models[1].APIKey, f.Models[1].BaseURL)
	}
	if f.Models[2].APIKey != "" {
		t.Fatalf("未设置变量应展开为空串，got %q", f.Models[2].APIKey)
	}
}

func TestRegistryValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		file ModelsRegistryFile
	}{
		{"id 空", ModelsRegistryFile{Models: []types.ModelEntry{{Provider: "p", Model: "m"}}}},
		{"id 重复", ModelsRegistryFile{Models: []types.ModelEntry{
			{ID: "x", Provider: "p", Model: "m"}, {ID: "x", Provider: "p", Model: "m2"}}}},
		{"provider 空", ModelsRegistryFile{Models: []types.ModelEntry{{ID: "x", Model: "m"}}}},
		{"model 空", ModelsRegistryFile{Models: []types.ModelEntry{{ID: "x", Provider: "p"}}}},
		{"绑定角色空", ModelsRegistryFile{RoleBindings: map[string]RoleBinding{"": {ModelID: "x"}}}},
		{"绑定 model_id 空", ModelsRegistryFile{RoleBindings: map[string]RoleBinding{"meta": {}}}},
	}
	for _, c := range cases {
		if err := c.file.Validate(); err == nil {
			t.Fatalf("%s: 应报错", c.name)
		}
	}
	// api_key 允许为空（切换时才拒绝）。
	ok := ModelsRegistryFile{Models: []types.ModelEntry{{ID: "x", Provider: "p", Model: "m"}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法条目不应报错: %v", err)
	}
}

func TestRegistrySaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	now := time.Now().UTC().Truncate(time.Second)
	src := &ModelsRegistryFile{
		Models: []types.ModelEntry{
			{ID: "glm", Name: "GLM", Provider: "anthropic", Model: "glm-5.3", APIKey: "k1", BaseURL: "http://b"},
			{ID: "ds", Provider: "openai-chat", Model: "deepseek-v4"},
		},
		RoleBindings: map[string]RoleBinding{
			"meta": {ModelID: "glm", Thinking: "high", AppliedAt: &now},
		},
	}
	// 连续两次 Save 覆盖已存在文件（Windows rename 原子写路径）。
	for i := 0; i < 2; i++ {
		if err := src.Save(path); err != nil {
			t.Fatalf("save #%d: %v", i, err)
		}
	}
	got, err := LoadModelsRegistry(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "glm" || got.Models[0].APIKey != "k1" ||
		got.Models[1].Provider != "openai-chat" {
		t.Fatalf("models 往返不一致: %+v", got.Models)
	}
	b := got.RoleBindings["meta"]
	if b.ModelID != "glm" || b.Thinking != "high" || b.AppliedAt == nil || !b.AppliedAt.Equal(now) {
		t.Fatalf("binding 往返不一致: %+v", b)
	}
}

func TestRegistryStoreMaybeReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	writeRegistryFile(t, path, `{"models":[{"id":"a","provider":"p","model":"m1"}],"role_bindings":{}}`)
	store, err := NewRegistryStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// 未变更：不重载。
	changed, err := store.MaybeReload()
	if changed || err != nil {
		t.Fatalf("未变更应 (false,nil)，got (%v,%v)", changed, err)
	}
	// 手改文件：重载生效。
	writeRegistryFile(t, path, `{"models":[{"id":"a","provider":"p","model":"m1"},{"id":"b","provider":"p","model":"m2"}]}`)
	changed, err = store.MaybeReload()
	if !changed || err != nil {
		t.Fatalf("变更后应 (true,nil)，got (%v,%v)", changed, err)
	}
	if _, ok := store.ModelByID("b"); !ok {
		t.Fatal("重载后新条目应可见")
	}
	// 改坏文件：报错但保留旧数据。
	writeRegistryFile(t, path, `{bad`)
	if _, err := store.MaybeReload(); err == nil {
		t.Fatal("坏文件应报错")
	}
	if _, ok := store.ModelByID("b"); !ok {
		t.Fatal("坏文件不应清空内存数据")
	}
	// 文件删除：清空。
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	changed, err = store.MaybeReload()
	if !changed || err != nil {
		t.Fatalf("删除后应 (true,nil)，got (%v,%v)", changed, err)
	}
	if _, ok := store.ModelByID("a"); ok {
		t.Fatal("删除后条目应清空")
	}
}

func TestRegistryStoreSetBindingPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	store, err := NewRegistryStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// 空注册表写绑定：落盘创建文件。
	if err := store.SetBinding("meta", "glm", "high"); err != nil {
		t.Fatalf("SetBinding: %v", err)
	}
	disk, err := LoadModelsRegistry(path)
	if err != nil || disk == nil {
		t.Fatalf("绑定应落盘: %+v err=%v", disk, err)
	}
	if b := disk.RoleBindings["meta"]; b.ModelID != "glm" || b.Thinking != "high" {
		t.Fatalf("落盘绑定错误: %+v", b)
	}
	// 空注册表（无 models 键）也应能加载：只校验绑定。
	if err := disk.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// 清除绑定。
	if err := store.SetBinding("meta", "", ""); err != nil {
		t.Fatalf("clear binding: %v", err)
	}
	disk, _ = LoadModelsRegistry(path)
	if _, ok := disk.RoleBindings["meta"]; ok {
		t.Fatal("绑定应已删除")
	}
}

func TestRegistryStoreAddDedupAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	store, err := NewRegistryStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	first := types.ModelEntry{ID: "glm", Provider: "p", Model: "m"}
	if err := store.Add(first); err != nil {
		t.Fatalf("add: %v", err)
	}
	// id 冲突报错。
	if err := store.Add(first); err == nil {
		t.Fatal("重复 id 应报错")
	}
	// 非法条目（provider 空）报错且回滚内存追加。
	before := len(store.Models())
	if err := store.Add(types.ModelEntry{ID: "bad", Model: "m"}); err == nil {
		t.Fatal("provider 空应报错")
	}
	if len(store.Models()) != before {
		t.Fatalf("校验失败应回滚，models=%d", len(store.Models()))
	}
	// Models 按 ID 排序返回副本。
	store.Add(types.ModelEntry{ID: "aaa", Provider: "p", Model: "m"})
	models := store.Models()
	if len(models) != 2 || models[0].ID != "aaa" || models[1].ID != "glm" {
		t.Fatalf("Models 应排序: %+v", models)
	}
	// 落盘内容校验：合法 JSON 且含两条目。
	disk, err := LoadModelsRegistry(path)
	if err != nil || len(disk.Models) != 2 {
		t.Fatalf("落盘错误: %+v err=%v", disk, err)
	}
}

func TestRegistryFileJSONShape(t *testing.T) {
	// models.json 顶层键固定为 models/role_bindings，手改兼容性依赖此形状。
	data, err := json.Marshal(ModelsRegistryFile{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe["models"]; !ok {
		t.Fatal("顶层缺 models 键")
	}
}
