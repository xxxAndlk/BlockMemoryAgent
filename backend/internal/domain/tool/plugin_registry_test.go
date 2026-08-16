package tool

// plugin_registry_test.go 覆盖热插拔插件对注册表的改造点（设计文档 §4.2）：
//   - 动态注册工具（实现 SchemaSource）出现在 Schema 且顺序稳定；
//   - Unregister 后 Dispatch 返回 unknown tool、Schema 不再暴露；
//   - 并发 Register/Unregister/Dispatch/Schema 无竞态（-race 下运行）。

import (
	"context"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// fakePluginTool 模拟 MCP 桥生成的插件工具：实现 tool.Tool + SchemaSource + Destructive。
type fakePluginTool struct {
	name string
}

func (f *fakePluginTool) Name() string      { return f.name }
func (f *fakePluginTool) Aliases() []string { return []string{"alias_" + f.name} }
func (f *fakePluginTool) Execute(ctx context.Context, args map[string]any) *Result {
	return &Result{Tool: f.name, Success: true, Output: "plugin-ok:" + f.name}
}
func (f *fakePluginTool) Description() string            { return "插件测试工具 " + f.name }
func (f *fakePluginTool) InputSchema() *jsonschema.Schema { return nil }
func (f *fakePluginTool) Destructive() bool               { return f.name == "danger" }

// TestDynamicToolSchemaExposure 验证：注册实现 SchemaSource 的动态工具后，
// Schema() 出现该工具且描述来自 Description()；未实现 SchemaSource 的注册工具不暴露。
func TestDynamicToolSchemaExposure(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	names := map[string]bool{}
	for _, s := range r.Schema() {
		names[s.Name()] = true
	}
	if names["ask_user"] {
		t.Fatal("ask_user 未实现 SchemaSource，不应出现在 Schema（保持既有行为）")
	}

	// 注册两个动态工具（顺序固定）。
	r.Register(&fakePluginTool{name: "web_search"})
	r.Register(&fakePluginTool{name: "research"})
	schema := r.Schema()
	var got []string
	for _, s := range schema {
		if s.Name() == "web_search" || s.Name() == "research" {
			got = append(got, s.Name())
			if s.Description() != "插件测试工具 "+s.Name() {
				t.Fatalf("动态工具描述应透传 Description()，got %q", s.Description())
			}
		}
	}
	if len(got) != 2 || got[0] != "web_search" || got[1] != "research" {
		t.Fatalf("动态工具应按注册顺序暴露，got %v", got)
	}

	// 派发动态工具走统一 Dispatch 路径。
	res, err := r.Dispatch(context.Background(), "web_search", map[string]any{"q": "x"})
	if err != nil || !res.Success || res.Output != "plugin-ok:web_search" {
		t.Fatalf("动态工具派发失败: res=%+v err=%v", res, err)
	}
}

// TestUnregister 验证：Unregister 摘除工具与别名，Dispatch/Schema 不再可见。
func TestUnregister(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&fakePluginTool{name: "web_search"})
	if !r.Has("web_search") {
		t.Fatal("注册后 Has 应为 true")
	}
	if !r.Unregister("web_search") {
		t.Fatal("Unregister 应返回 true")
	}
	if r.Has("web_search") {
		t.Fatal("摘除后 Has 应为 false")
	}
	// Dispatch 走别名也被摘除。
	if _, err := r.Dispatch(context.Background(), "alias_web_search", nil); err == nil {
		t.Fatal("摘除后别名派发应报 unknown tool")
	}
	if _, err := r.Dispatch(context.Background(), "web_search", nil); err == nil {
		t.Fatal("摘除后派发应报 unknown tool")
	}
	for _, s := range r.Schema() {
		if s.Name() == "web_search" {
			t.Fatal("摘除后 Schema 不应包含 web_search")
		}
	}
	// 重复摘除幂等。
	if r.Unregister("web_search") {
		t.Fatal("重复 Unregister 应返回 false")
	}
}

// TestDestructiveDynamicToolNeedsApproval 验证：动态工具自标 Destructive 时
// 经审批链（needsApproval 恒 true，与目录无关）。
func TestDestructiveDynamicToolNeedsApproval(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&fakePluginTool{name: "danger"})
	approved := false
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		approved = true
		return true, nil
	})
	if _, err := r.Dispatch(context.Background(), "danger", map[string]any{}); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !approved {
		t.Fatal("Destructive 动态工具应触发审批钩子")
	}
}

// TestRegistryConcurrentRegisterUnregisterDispatch 在 -race 下验证注册表并发安全：
// 并发注册/摘除/派发/Schema 互不干扰，最终一致性由调用方语义保证。
func TestRegistryConcurrentRegisterUnregisterDispatch(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "plugin_" + string(rune('a'+n%26)) + string(rune('0'+n))
			for j := 0; j < 50; j++ {
				r.Register(&fakePluginTool{name: name})
				_, _ = r.Dispatch(context.Background(), name, map[string]any{})
				_ = r.Schema()
				r.Unregister(name)
			}
		}(i)
	}
	// 并发读 Schema/Dispatch 内置工具。
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = r.Schema()
				_, _ = r.Dispatch(context.Background(), "ReadFile", map[string]any{"path": "x"})
			}
		}()
	}
	wg.Wait()
}
