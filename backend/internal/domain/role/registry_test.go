package role

// 导入测试与角色配置相关的包。
import (
	"testing"

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
