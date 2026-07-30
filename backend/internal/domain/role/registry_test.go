package role

// 导入测试与角色配置相关的包。
import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestRegistry_Get_MetaAndFixed 验证 Registry.Get 对内置角色与固定角色的查询行为。
func TestRegistry_Get_MetaAndFixed(t *testing.T) {
	// 构造一个包含 MetaAgent 与两个固定角色的测试配置。
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta prompt",
			ModelConfig:  types.AgentModelConfig{Model: "meta-model"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
			{ID: "ui_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}},
		},
	}
	// 使用测试配置创建角色注册表。
	r := NewRegistry(cfg)

	// 查询内置 meta 角色，校验其系统提示词与配置一致。
	meta := r.Get("meta")
	if meta == nil || meta.SystemPrompt != "meta prompt" {
		t.Fatalf("unexpected meta role: %+v", meta)
	}

	// 查询固定角色 code_assistant，校验其 ID 正确。
	code := r.Get("code_assistant")
	if code == nil || code.ID != "code_assistant" {
		t.Fatalf("unexpected code role: %+v", code)
	}

	// 查询不存在的角色，应返回 nil。
	if r.Get("missing") != nil {
		t.Fatal("expected nil for missing role")
	}
}

// TestRegistry_CanCall 验证角色间的调用权限判断逻辑。
func TestRegistry_CanCall(t *testing.T) {
	// 构造测试配置：包含可调用角色、带父角色的可调用角色以及不可调用角色。
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
			{ID: "ui_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}},
			{ID: "private", Type: enums.RoleTypeFixed, CanBeCalled: false},
		},
	}
	// 创建角色注册表。
	r := NewRegistry(cfg)

	// 校验 meta 可以调用声明为 callable 的固定角色 code_assistant。
	if !r.CanCall("meta", "code_assistant") {
		t.Error("meta should be able to call callable fixed role")
	}
	// 校验 meta 不能调用未声明 callable 的固定角色 private。
	if r.CanCall("meta", "private") {
		t.Error("meta should not be able to call non-callable fixed role")
	}
	// 校验 ui_assistant 通过 Parents 字段获得调用 code_assistant 的权限。
	if !r.CanCall("ui_assistant", "code_assistant") {
		t.Error("ui_assistant parent permission should allow calling code_assistant")
	}
	// 校验 code_assistant 没有反向调用 ui_assistant 的权限。
	if r.CanCall("code_assistant", "ui_assistant") {
		t.Error("code_assistant should not be able to call ui_assistant without parent")
	}
}

// TestRegistry_RegisterDynamic 验证运行时注册动态角色并立即被 Get/CanCall/CallableFixedRoles 发现。
func TestRegistry_RegisterDynamic(t *testing.T) {
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
		},
	}
	r := NewRegistry(cfg)

	// 初始状态只有 1 个固定角色可调用。
	if got := len(r.CallableFixedRoles()); got != 1 {
		t.Fatalf("expected 1 callable role, got %d", got)
	}

	// 注册一个动态角色。
	dyn := &types.RoleDefinition{
		ID:           "doc_reviewer",
		Name:         "Doc Reviewer",
		Type:         enums.RoleTypeDynamic,
		SystemPrompt: "review docs",
		CanBeCalled:  true,
	}
	if err := r.Register(dyn); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// Get 应能找到动态角色。
	got := r.Get("doc_reviewer")
	if got == nil || got.Name != "Doc Reviewer" {
		t.Fatalf("dynamic role not found: %+v", got)
	}

	// CanCall：meta 应可调用动态角色。
	if !r.CanCall("meta", "doc_reviewer") {
		t.Error("meta should be able to call callable dynamic role")
	}

	// CallableFixedRoles 应包含动态角色。
	callable := r.CallableFixedRoles()
	found := false
	for _, c := range callable {
		if c.ID == "doc_reviewer" {
			found = true
		}
	}
	if !found {
		t.Error("dynamic role missing from CallableFixedRoles")
	}

	// List 应包含 meta + domain + 1 fixed + 1 dynamic。
	if got := len(r.List()); got != 4 {
		t.Errorf("expected 4 roles in List, got %d", got)
	}
}

// TestRegistry_RegisterValidation 验证 Register 的参数校验。
func TestRegistry_RegisterValidation(t *testing.T) {
	r := NewRegistry(&config.RoleConfigFile{})

	cases := []struct {
		name string
		role *types.RoleDefinition
		want string
	}{
		{
			name: "empty id",
			role: &types.RoleDefinition{ID: "", Name: "X", Type: enums.RoleTypeDynamic, SystemPrompt: "p"},
			want: "id is required",
		},
		{
			name: "reserved meta",
			role: &types.RoleDefinition{ID: "meta", Name: "X", Type: enums.RoleTypeDynamic, SystemPrompt: "p"},
			want: "reserved",
		},
		{
			name: "reserved domain",
			role: &types.RoleDefinition{ID: "domain", Name: "X", Type: enums.RoleTypeDynamic, SystemPrompt: "p"},
			want: "reserved",
		},
		{
			name: "wrong type fixed",
			role: &types.RoleDefinition{ID: "r1", Name: "X", Type: enums.RoleTypeFixed, SystemPrompt: "p"},
			want: "only dynamic type",
		},
		{
			name: "empty prompt",
			role: &types.RoleDefinition{ID: "r2", Name: "X", Type: enums.RoleTypeDynamic, SystemPrompt: "  "},
			want: "system_prompt is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := r.Register(c.role)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected error %q, got %v", c.want, err)
			}
		})
	}
}

// TestRegistry_RegisterDuplicate 验证 ID 冲突拒绝。
func TestRegistry_RegisterDuplicate(t *testing.T) {
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
		},
	}
	r := NewRegistry(cfg)

	// 第一次注册成功。
	dyn := &types.RoleDefinition{ID: "dyn1", Name: "D1", Type: enums.RoleTypeDynamic, SystemPrompt: "p"}
	if err := r.Register(dyn); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	// 重复注册同 ID（dynamic 层）失败。
	if err := r.Register(dyn); err == nil {
		t.Error("duplicate dynamic register should fail")
	}
	// 与固定角色 ID 冲突失败。
	dyn2 := &types.RoleDefinition{ID: "code_assistant", Name: "X", Type: enums.RoleTypeDynamic, SystemPrompt: "p"}
	if err := r.Register(dyn2); err == nil {
		t.Error("register colliding with fixed role should fail")
	}
}

// TestRegistry_Unregister 验证注销动态角色。
func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry(&config.RoleConfigFile{})
	dyn := &types.RoleDefinition{ID: "dyn1", Name: "D1", Type: enums.RoleTypeDynamic, SystemPrompt: "p"}
	if err := r.Register(dyn); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if r.Get("dyn1") == nil {
		t.Fatal("dynamic role not found after register")
	}
	if err := r.Unregister("dyn1"); err != nil {
		t.Fatalf("unregister failed: %v", err)
	}
	if r.Get("dyn1") != nil {
		t.Error("dynamic role should be nil after unregister")
	}
	// 再次注销失败。
	if err := r.Unregister("dyn1"); err == nil {
		t.Error("unregister missing role should fail")
	}
}

// TestRegistry_RegisterTools 验证 RegisterTools 注入后 create_role/list_roles 可被 Dispatch 调用。
func TestRegistry_RegisterTools(t *testing.T) {
	r := NewRegistry(&config.RoleConfigFile{})
	tr := tool.NewBuiltinRegistry("", nil, nil)
	r.RegisterTools(tr)

	// list_roles 应返回 meta + domain（即使 cfg 为空，meta/domain 仍合成）。
	res, _ := tr.Dispatch(context.Background(), "list_roles", map[string]any{})
	if !res.Success {
		t.Fatalf("list_roles failed: %s", res.Error)
	}
	if !strings.Contains(res.Output, "meta") || !strings.Contains(res.Output, "domain") {
		t.Errorf("list_roles output missing meta/domain: %s", res.Output)
	}

	// create_role 注册一个动态角色。
	res, _ = tr.Dispatch(context.Background(), "create_role", map[string]any{
		"id":            "test_role",
		"name":          "Test Role",
		"system_prompt": "test prompt",
		"description":   "test desc",
		"tools":         []any{"ReadFile", "WriteFile"},
	})
	if !res.Success {
		t.Fatalf("create_role failed: %s", res.Error)
	}

	// 验证角色已注册。
	got := r.Get("test_role")
	if got == nil || got.Name != "Test Role" {
		t.Fatalf("role not registered: %+v", got)
	}
	if got.Type != enums.RoleTypeDynamic {
		t.Errorf("expected dynamic type, got %q", got.Type)
	}
	if len(got.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(got.Tools))
	}
}
