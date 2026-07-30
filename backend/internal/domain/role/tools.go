package role

// tools.go 提供 create_role / list_roles 两个运行时角色管理工具。
// 仅暴露给 MetaAgent（registry.go 中 meta 角色的 Tools 白名单显式列出），
// 让 MetaAgent 在长任务中按需注册新角色而不必重启进程。
//
// 设计与 call_sub_agent 一致：工具实现 Tool 接口（Name/Aliases/Execute），
// 通过 role.Registry.RegisterTools 注入到 tool.Registry；
// tool.Registry.Schema() 检测 r.tools["create_role"|"list_roles"] 并用
// tools.NewFunc 暴露给 LLM。Description 动态生成，便于 LLM 理解参数含义。
//
// 不做模板渲染：LLM 直接传结构化字段（id/name/system_prompt/...），
// Registry.Register 原样落 dynamic map。

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// createRoleTool 实现 create_role 工具：运行时注册一个动态角色。
type createRoleTool struct {
	registry *Registry
}

// Name 返回工具标准名称 create_role。
func (t *createRoleTool) Name() string { return "create_role" }

// Aliases 返回 create_role 的别名。
func (t *createRoleTool) Aliases() []string { return []string{"register_role"} }

// Description 返回 LLM 可见描述。
func (t *createRoleTool) Description() string {
	return "运行时注册一个新的动态角色（Dynamic 类型），注册后立即可被 call_sub_agent 派发。" +
		"用于任务需要 yaml 中未预定义的专门助手时（如某特定框架的代码审查员）。" +
		"参数：id（唯一，不可为 meta/domain）、name、system_prompt（必填）、description、" +
		"tools（该角色可用的工具名列表）、can_be_called（默认 true，false 表示纯调度型不可被调用）、" +
		"parents（可调用此角色的父角色 ID 列表，默认空表示任何编排者可调用）。" +
		"注册成功后该角色在当前进程存活；进程重启不保留（roles.yaml 才持久化）。"
}

// Execute 执行 create_role 工具调用。
func (t *createRoleTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	if t.registry == nil {
		return &tool.Result{Tool: "create_role", Error: "role registry not configured"}
	}

	id, _ := args["id"].(string)
	name, _ := args["name"].(string)
	systemPrompt, _ := args["system_prompt"].(string)
	description, _ := args["description"].(string)

	if id == "" || name == "" || systemPrompt == "" {
		return &tool.Result{Tool: "create_role", Error: "id, name, system_prompt are required"}
	}

	// 解析 tools 列表（兼容 []any / []string）。
	var toolList []string
	switch v := args["tools"].(type) {
	case []string:
		toolList = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				toolList = append(toolList, s)
			}
		}
	}

	// 解析 parents 列表（兼容 []any / []string）。
	var parents []string
	switch v := args["parents"].(type) {
	case []string:
		parents = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				parents = append(parents, s)
			}
		}
	}

	// can_be_called 默认 true。
	canBeCalled := true
	if v, ok := args["can_be_called"].(bool); ok {
		canBeCalled = v
	}

	role := &types.RoleDefinition{
		ID:           id,
		Name:         name,
		Type:         enums.RoleTypeDynamic,
		Description:  description,
		SystemPrompt: systemPrompt,
		Tools:        toolList,
		CanBeCalled:  canBeCalled,
		Parents:      parents,
	}

	if err := t.registry.Register(role); err != nil {
		return &tool.Result{Tool: "create_role", Error: err.Error()}
	}

	return &tool.Result{
		Tool:    "create_role",
		Success: true,
		Output:  fmt.Sprintf("role %q registered (tools=%d, can_be_called=%v)", id, len(toolList), canBeCalled),
	}
}

// listRolesTool 实现 list_roles 工具：列出当前所有角色（静态 + 动态）。
type listRolesTool struct {
	registry *Registry
}

// Name 返回工具标准名称 list_roles。
func (t *listRolesTool) Name() string { return "list_roles" }

// Aliases 返回 list_roles 的别名。
func (t *listRolesTool) Aliases() []string { return []string{"list_role"} }

// Description 返回 LLM 可见描述。
func (t *listRolesTool) Description() string {
	return "列出当前所有可用角色（meta/domain 内置 + 固定助手 + 运行时注册的动态角色）。" +
		"无参数。返回每个角色的 id/name/type/can_be_called/description/tools 摘要。" +
		"用于 create_role 前查看现有角色避免 ID 冲突，或决策派发目标时了解可调用角色清单。"
}

// Execute 执行 list_roles 工具调用。
func (t *listRolesTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	if t.registry == nil {
		return &tool.Result{Tool: "list_roles", Error: "role registry not configured"}
	}
	roles := t.registry.List()
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, map[string]any{
			"id":            r.ID,
			"name":          r.Name,
			"type":          string(r.Type),
			"can_be_called": r.CanBeCalled,
			"description":   r.Description,
			"tools":         r.Tools,
		})
	}
	b, _ := json.Marshal(out)
	return &tool.Result{
		Tool:    "list_roles",
		Success: true,
		Output:  string(b),
	}
}

// RegisterTools 把 create_role 与 list_roles 工具安装到传入的工具注册表。
// bootstrap 在构造 toolRegistry 后调用，使两个工具对 LLM 可见。
// 角色注册表通过 closure 持有引用，工具执行时直接读写 dynamic 层。
func (r *Registry) RegisterTools(tr *tool.Registry) {
	if r == nil || tr == nil {
		return
	}
	tr.Register(&createRoleTool{registry: r})
	tr.Register(&listRolesTool{registry: r})
}
