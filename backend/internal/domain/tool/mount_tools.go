package tool

// mount_tools.go 实现工具目录与按需挂载工具组（TODO #52 执行项 2/3）：
//   - tool_catalog   只读：列出当前角色权限天花板内的全部插件工具（名称 + 一句话描述，
//     不含 schema，按插件分组），并标注本 Agent 已挂载项——Agent 借此"知道自己能用什么"，
//     替代全量 schema 注入（上下文膨胀的根因）；
//   - tool_mount     把插件工具挂载进当前 Agent 的可见集（下一轮 Schema 纳入）；
//     挂载仅在天花板内生效，越界直接拒绝并说明（权限天花板不可由 Agent 突破）；
//   - tool_unmount   卸载已挂载工具（收窄可见集，收缩操作无需天花板校验）。
//
// 权限模型（分层混合）：天花板 = roles.yaml 角色基础白名单 ∪ 插件 Manifest.Roles 白名单，
// 由人改配置声明，Agent 不可改；天花板内 Agent 自主选用。Schema 注入 =
// 角色基础工具 +（天花板 ∩ 已挂载集），默认即收窄。

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// ---- tool_catalog ----

// toolCatalogTool 实现 tool_catalog：枚举当前角色天花板内的插件工具。
type toolCatalogTool struct{ reg *Registry }

// Name 返回工具名称。
func (t *toolCatalogTool) Name() string { return "tool_catalog" }

// Aliases 返回工具别名列表，当前无别名。
func (t *toolCatalogTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *toolCatalogTool) Description() string {
	return "列出当前角色权限天花板内可用的全部插件工具（名称 + 一句话描述，不含入参 schema），按插件分组并标注已挂载项。" +
		"插件工具默认不注入本 Agent 上下文（避免 schema 膨胀）：先用本工具发现，再用 tool_mount 挂载，下一轮迭代即对当前 Agent 可见。"
}

// InputSchema 返回入参 JSON Schema（无字段）。
func (t *toolCatalogTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
}

// Execute 执行 tool_catalog。
func (t *toolCatalogTool) Execute(ctx context.Context, args map[string]any) *Result {
	r := t.reg
	if r.pluginMgr == nil {
		return &Result{Tool: "tool_catalog", Error: "插件管理未接线（服务未注入 PluginManager）"}
	}
	roleID := RoleIDFromContext(ctx)
	scope := scopeKeyFromCtx(ctx)
	mounted := r.MountedTools(scope)
	// 配置预挂载的顶层必备工具（settings.top_level）不在角色天花板的也要列出（已挂 ✓）：
	// 它们已在 Schema 里，catalog 漏列会让模型误判"没有这个能力"。
	pre := r.PreMountedTools(scope)

	var b strings.Builder
	b.WriteString("当前角色权限天花板内的插件工具（✓=已挂载，下一轮即对本 Agent 可见；空=未挂载）：\n")
	anyInCeiling := false
	for _, p := range r.pluginMgr.List() {
		var lines []string
		for _, tn := range p.Tools {
			owned, visible := true, true
			if r.pluginVisibility != nil {
				owned, visible = r.pluginVisibility(roleID, tn)
			}
			if !pre[tn] && (!owned || !visible) {
				continue
			}
			mark := " "
			if mounted[tn] {
				mark = "✓"
			}
			lines = append(lines, fmt.Sprintf("  %s %s — %s", mark, tn, t.describe(tn)))
		}
		if len(lines) == 0 {
			continue
		}
		anyInCeiling = true
		title := p.Name
		if title == "" {
			title = p.ID
		}
		fmt.Fprintf(&b, "【插件 %s】%s（%s）\n%s\n", p.ID, title, p.State, strings.Join(lines, "\n"))
	}
	if !anyInCeiling {
		b.WriteString("（无）——本角色没有可用的插件工具。\n")
	}
	b.WriteString("规则：天花板 = roles.yaml 角色基础白名单 ∪ 插件 roles 白名单（仅人改配置，Agent 不可改）；" +
		"越界挂载会被 tool_mount 拒绝。发现需要的工具后调 tool_mount(tools=[...]) 挂载。")
	return &Result{Tool: "tool_catalog", Success: true, Output: b.String()}
}

// describe 取已注册工具的一句话描述；未注册（竞态摘除）或未实现 SchemaSource 时给占位文案。
func (t *toolCatalogTool) describe(name string) string {
	r := t.reg
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "（已摘除）"
	}
	src, ok := tool.(SchemaSource)
	if !ok {
		return "（无描述）"
	}
	desc := src.Description()
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = desc[:i]
	}
	if desc == "" {
		return "（无描述）"
	}
	return desc
}

// ---- tool_mount ----

// toolMountTool 实现 tool_mount：挂载插件工具（天花板内生效）。
type toolMountTool struct{ reg *Registry }

// Name 返回工具名称。
func (t *toolMountTool) Name() string { return "tool_mount" }

// Aliases 返回工具别名列表，当前无别名。
func (t *toolMountTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *toolMountTool) Description() string {
	return "把插件工具挂载进当前 Agent 的可见集，下一轮迭代即对当前 Agent 可见（入参 schema 注入上下文）。" +
		"挂载仅在本角色权限天花板（插件 roles 白名单）内生效：越界工具直接拒绝并说明原因，不会静默放行。" +
		"先用 tool_catalog 查天花板内工具清单，再决定挂载哪些；不再需要时用 tool_unmount 卸载。"
}

// InputSchema 返回入参 JSON Schema。
func (t *toolMountTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"tools":  {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "要挂载的插件工具名列表（来自 tool_catalog）"},
			"reason": {Type: "string", Description: "挂载原因（可选，便于审计）"},
		},
		Required: []string{"tools"},
	}
}

// Execute 执行 tool_mount。
func (t *toolMountTool) Execute(ctx context.Context, args map[string]any) *Result {
	r := t.reg
	names := stringSliceArg(args, "tools")
	if len(names) == 0 {
		return &Result{Tool: "tool_mount", Category: ResultCategoryValidationRejected, Error: "tool_mount: tools 必填（非空工具名列表，来自 tool_catalog）"}
	}
	roleID := RoleIDFromContext(ctx)
	scope := scopeKeyFromCtx(ctx)
	accepted, rejected := r.MountForScope(scope, roleID, names)
	var b strings.Builder
	if len(accepted) > 0 {
		fmt.Fprintf(&b, "已挂载 %d 个工具，下一轮迭代即对当前 Agent 可见：%s。", len(accepted), strings.Join(accepted, ", "))
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "\n拒绝 %d 个（权限天花板外，无法挂载）：%s。", len(rejected), strings.Join(rejected, "；"))
	}
	return &Result{Tool: "tool_mount", Success: true, Output: b.String()}
}

// ---- tool_unmount ----

// toolUnmountTool 实现 tool_unmount：卸载已挂载工具。
type toolUnmountTool struct{ reg *Registry }

// Name 返回工具名称。
func (t *toolUnmountTool) Name() string { return "tool_unmount" }

// Aliases 返回工具别名列表，当前无别名。
func (t *toolUnmountTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *toolUnmountTool) Description() string {
	return "把已挂载的插件工具从当前 Agent 可见集卸载，下一轮迭代起不再注入该工具 schema（收窄上下文）。" +
		"不再需要某插件工具时用它卸载；之后仍可随时 tool_mount 重新挂载。"
}

// InputSchema 返回入参 JSON Schema。
func (t *toolUnmountTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"tools": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "要卸载的插件工具名列表"},
		},
		Required: []string{"tools"},
	}
}

// Execute 执行 tool_unmount。
func (t *toolUnmountTool) Execute(ctx context.Context, args map[string]any) *Result {
	r := t.reg
	names := stringSliceArg(args, "tools")
	if len(names) == 0 {
		return &Result{Tool: "tool_unmount", Category: ResultCategoryValidationRejected, Error: "tool_unmount: tools 必填（非空工具名列表）"}
	}
	scope := scopeKeyFromCtx(ctx)
	r.UnmountTools(scope, names)
	return &Result{Tool: "tool_unmount", Success: true, Output: fmt.Sprintf("已卸载 %d 个工具，下一轮迭代起不再对当前 Agent 可见：%s。", len(names), strings.Join(names, ", "))}
}
