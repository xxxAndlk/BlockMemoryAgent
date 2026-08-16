package tool

// plugin_tools.go 实现插件管理工具组（TODO #51 插件系统 Agent 自安装闭环）：
//   - plugin_search   按关键词检索插件目录（内置精选目录 + 已配置实例 + 远程 registry）；
//   - plugin_install  安装插件（目录条目或手工 manifest）→ 持久化 + 立即生效；
//   - plugin_enable / plugin_disable  热启停已安装插件（包装 Manager 现有 API）；
//   - plugin_list     列出全部插件（状态/工具清单/缺失 env）。
//
// 依赖注入：tool 包不能反向依赖 plugins 包（plugins 已依赖 tool），故定义
// PluginManager 接口 + DTO，由 plugins.ToolManagerAdapter（bootstrap 装配）实现。
// 未接线时工具返回"未接线"错误（与 ask_user/search_knowledge 同款语义）。
//
// 安全门（TODO #51 补齐项 3）：
//   - plugin_install 自标 Destructive()=true → 接入既有审批守卫链（needsApproval），
//     安装动作需用户二次确认（approval hook 未接线时零变化）；
//   - 远程 registry 来源白名单 = plugins.yaml registry_sources（缺省 MCP 官方端点），
//     Manager.Search 只查询白名单内 URL。

import (
	"context" // 工具执行上下文
	"fmt"     // 结果文案
	"strings" // 文案拼接

	"github.com/google/jsonschema-go/jsonschema" // 入参 schema
)

// PluginInfo 是插件信息视图（与 plugins.Info 对应的跨包 DTO）。
type PluginInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind"`
	State       string   `json:"state"`
	Enabled     bool     `json:"enabled"`
	Tools       []string `json:"tools,omitempty"`
	URL         string   `json:"url,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	MissingEnv  []string `json:"missing_env,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

// InstallRequest 是 plugin_install 的安装请求。
// 提供完整 manifest（Kind/transport/command/url/image 等）时按手工安装处理；
// 仅提供 ID 时由 Manager 按目录条目（内置/检索缓存/远程 registry）解析。
type InstallRequest struct {
	// ID 插件 ID（目录条目或手工安装均必填）。
	ID string
	// Name/Description/Version 展示信息覆盖（目录解析时生效）。
	Name        string
	Description string
	Version     string
	// Kind 插件形态：mcp | service；空 = mcp。
	Kind string
	// RequiresEnv 依赖的环境变量（缺失时 enable 拒绝）。
	RequiresEnv []string
	// Roles 可见角色白名单（缺省全角色）。
	Roles []string
	// Settings 完整插件配置（mcp: transport/command/args/url/image/env/destructive...）。
	Settings map[string]any
	// Enabled 安装后是否立即启用（默认 true）。
	Enabled bool
}

// SearchResult 是 plugin_search 的检索结果条目。
type SearchResult struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind"`
	Source      string   `json:"source"` // builtin | mcp-registry | config
	State       string   `json:"state,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

// PluginManager 是 plugin_* 工具依赖的插件管理面（由 plugins.Manager 适配实现）。
type PluginManager interface {
	// List 返回全部插件信息（按 ID 稳定排序）。
	List() []PluginInfo
	// Get 返回单个插件信息。
	Get(id string) (PluginInfo, bool)
	// Enable / Disable 热启停插件。
	Enable(ctx context.Context, id string) error
	Disable(ctx context.Context, id string) error
	// Install 安装插件并立即生效；失败回滚不残留。
	Install(ctx context.Context, req InstallRequest) (PluginInfo, error)
	// Search 检索插件目录（内置 + 已配置 + 远程 registry），返回条目与不可用来源说明。
	Search(ctx context.Context, query string) ([]SearchResult, []string)
}

// SetPluginManager 注入插件管理面（bootstrap 装配 plugins.ToolManagerAdapter 后调用）。
// nil 时 plugin_* 工具返回未配置错误。
func (r *Registry) SetPluginManager(mgr PluginManager) {
	if r == nil {
		return
	}
	r.pluginMgr = mgr
	// 同步注入各工具实例（与 SetAskUserHook 同款模式：工具构造时 mgr 为 nil）。
	if t, ok := r.tools["plugin_search"].(*pluginSearchTool); ok {
		t.mgr = mgr
	}
	if t, ok := r.tools["plugin_install"].(*pluginInstallTool); ok {
		t.mgr = mgr
		t.reg = r
	}
	if t, ok := r.tools["plugin_enable"].(*pluginToggleTool); ok {
		t.mgr = mgr
	}
	if t, ok := r.tools["plugin_disable"].(*pluginToggleTool); ok {
		t.mgr = mgr
	}
	if t, ok := r.tools["plugin_list"].(*pluginListTool); ok {
		t.mgr = mgr
	}
}

// ---- plugin_search ----

// pluginSearchTool 实现 plugin_search：按关键词检索插件目录。
type pluginSearchTool struct{ mgr PluginManager }

// Name 返回工具名称。
func (t *pluginSearchTool) Name() string { return "plugin_search" }

// Aliases 返回工具别名列表，当前无别名。
func (t *pluginSearchTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *pluginSearchTool) Description() string {
	return "检索可安装插件目录：内置精选目录 + 已配置/已安装插件 + 远程 registry（MCP 官方目录等白名单来源）。" +
		"适合用户提出安装某插件时先搜索确认条目 id，再用 plugin_install 安装。query 为关键词（名称/描述子串，可不填列出全部）。仅 MetaAgent 可用。"
}

// InputSchema 返回入参 JSON Schema。
func (t *pluginSearchTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"query": {Type: "string"}},
	}
}

// Execute 执行 plugin_search。
func (t *pluginSearchTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.mgr == nil {
		return &Result{Tool: "plugin_search", Error: "插件管理未接线（服务未注入 PluginManager）"}
	}
	query, _ := args["query"].(string)
	results, notes := t.mgr.Search(ctx, query)
	if len(results) == 0 {
		msg := "未找到匹配插件"
		if len(notes) > 0 {
			msg += "（" + strings.Join(notes, "；") + "）"
		}
		return &Result{Tool: "plugin_search", Success: true, Output: msg}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("插件目录检索结果（%d 条，query=%q）：\n", len(results), query))
	for i, r := range results {
		meta := r.Kind
		switch r.Source {
		case "config":
			meta = "已配置" + r.Kind + "，状态=" + r.State
			if len(r.Tools) > 0 {
				meta += "，工具=" + strings.Join(r.Tools, ",")
			}
		case "mcp-registry":
			meta = "远程目录 " + r.Kind + "（registry: " + r.State + "）"
		default:
			meta = "内置目录 " + r.Kind
		}
		desc := r.Description
		if desc == "" {
			desc = "（无描述）"
		}
		b.WriteString(fmt.Sprintf("  [%d] %s — %s（%s）\n", i+1, r.ID, desc, meta))
	}
	b.WriteString("安装：plugin_install(id=<上述 id>)，已配置条目用 plugin_enable(id=...) 启用。")
	if len(notes) > 0 {
		b.WriteString("\n注意：" + strings.Join(notes, "；"))
	}
	return &Result{Tool: "plugin_search", Success: true, Output: b.String()}
}

// ---- plugin_install ----

// pluginInstallTool 实现 plugin_install：安装插件并立即生效。
// reg 用于安装成功后把新工具自动挂载进调用者作用域（TODO #52："装完当轮即可用"
// 的前提是可见集收窄后调用者已挂载该工具；安装是显式意图，自动挂载天花板内工具）。
type pluginInstallTool struct {
	mgr PluginManager
	reg *Registry
}

// Name 返回工具名称。
func (t *pluginInstallTool) Name() string { return "plugin_install" }

// Aliases 返回工具别名列表，当前无别名。
func (t *pluginInstallTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *pluginInstallTool) Description() string {
	return "安装插件并立即生效（持久化到 plugins.installed.yaml，本工具调用需用户确认）。" +
		"两种用法：① 只传 id（来自 plugin_search 结果，按目录条目安装）；② 手工 manifest：" +
		"id + transport（stdio/http/docker）+ 对应启动配置（stdio 需 command/args，http 需 url，docker 需 image），" +
		"env 传所需环境变量，roles 限定可见角色，destructive=true 使全部工具进审批链。" +
		"enabled 缺省 true（装完即用）。仅 MetaAgent 可用。"
}

// InputSchema 返回入参 JSON Schema。
func (t *pluginInstallTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"id":           {Type: "string"},
			"enabled":      {Type: "boolean"},
			"name":         {Type: "string"},
			"description":  {Type: "string"},
			"transport":    {Type: "string"},
			"command":      {Type: "string"},
			"args":         {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			"url":          {Type: "string"},
			"image":        {Type: "string"},
			"env":          {Type: "object"},
			"roles":        {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			"requires_env": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			"destructive":  {Type: "boolean"},
		},
		Required: []string{"id"},
	}
}

// Destructive 标记安装为敏感操作：恒进审批守卫链（TODO #51 安全门）。
func (t *pluginInstallTool) Destructive() bool { return true }

// Execute 执行 plugin_install。
func (t *pluginInstallTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.mgr == nil {
		return &Result{Tool: "plugin_install", Error: "插件管理未接线（服务未注入 PluginManager）"}
	}
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return &Result{Tool: "plugin_install", Category: ResultCategoryValidationRejected, Error: "plugin_install: id 必填（先 plugin_search 查目录，或提供完整手工 manifest）"}
	}
	req := InstallRequest{
		ID:      id,
		Enabled: true,
	}
	if v, ok := args["enabled"].(bool); ok {
		req.Enabled = v
	}
	if v, ok := args["name"].(string); ok {
		req.Name = v
	}
	if v, ok := args["description"].(string); ok {
		req.Description = v
	}
	if v, ok := args["kind"].(string); ok {
		req.Kind = v
	}
	req.RequiresEnv = stringSliceArg(args, "requires_env")
	req.Roles = stringSliceArg(args, "roles")
	// 手工 manifest 的 settings 组装：出现任一启动配置/覆盖字段即携带；
	// env-only 时仅带 env（适配器走目录条目解析 + env 覆盖）。
	if transport, _ := args["transport"].(string); transport != "" ||
		argNonEmpty(args, "command") || argNonEmpty(args, "url") || argNonEmpty(args, "image") ||
		len(stringSliceArg(args, "args")) > 0 || len(stringMapArg(args, "env")) > 0 {
		settings := map[string]any{}
		if transport != "" {
			settings["transport"] = transport
		}
		if v, _ := args["command"].(string); v != "" {
			settings["command"] = v
		}
		if v := stringSliceArg(args, "args"); len(v) > 0 {
			anyArgs := make([]any, len(v))
			for i, a := range v {
				anyArgs[i] = a
			}
			settings["args"] = anyArgs
		}
		if v, _ := args["url"].(string); v != "" {
			settings["url"] = v
		}
		if v, _ := args["image"].(string); v != "" {
			settings["image"] = v
		}
		if v := stringMapArg(args, "env"); len(v) > 0 {
			settings["env"] = v
		}
		if v, ok := args["destructive"].(bool); ok && v {
			settings["destructive"] = true
		}
		if req.Roles != nil {
			settings["roles"] = req.Roles
		}
		req.Settings = settings
	}
	info, err := t.mgr.Install(ctx, req)
	if err != nil {
		return &Result{Tool: "plugin_install", Category: ResultCategoryExecutionFailed, Error: "安装插件失败: " + err.Error()}
	}
	msg := fmt.Sprintf("插件 %s 安装成功（kind=%s，状态=%s", info.ID, info.Kind, info.State)
	if info.Enabled {
		msg += "，enabled=true"
	}
	if len(info.Tools) > 0 {
		msg += "，工具: " + strings.Join(info.Tools, ", ")
	}
	msg += "）。工具列表下一轮 ReAct 迭代即可调用。"
	// 自动挂载（TODO #52 执行项 1/3）：安装是调用者的显式意图，把新工具的可见部分
	// 直接挂载进调用者作用域（仅天花板内生效，越界项列出并拒绝——如 roles 限定他角色）。
	if t.reg != nil && len(info.Tools) > 0 {
		accepted, rejected := t.reg.MountForScope(scopeKeyFromCtx(ctx), RoleIDFromContext(ctx), info.Tools)
		if len(accepted) > 0 {
			msg += fmt.Sprintf("\n已自动挂载到当前 Agent（本轮即可调用）: %s。", strings.Join(accepted, ", "))
		}
		if len(rejected) > 0 {
			msg += fmt.Sprintf("\n未挂载 %d 个（权限天花板外，需 roles 白名单授权）: %s。", len(rejected), strings.Join(rejected, "；"))
		}
	}
	if len(info.MissingEnv) > 0 {
		msg += "\n注意：缺失环境变量 " + strings.Join(info.MissingEnv, ", ") + "，enable 前需补齐。"
	}
	return &Result{Tool: "plugin_install", Success: true, Output: msg}
}

// ---- plugin_enable / plugin_disable ----

// pluginToggleTool 实现 plugin_enable / plugin_disable 共用逻辑。
type pluginToggleTool struct {
	mgr    PluginManager
	enable bool
}

// Name 返回工具名称。
func (t *pluginToggleTool) Name() string {
	if t.enable {
		return "plugin_enable"
	}
	return "plugin_disable"
}

// Aliases 返回工具别名列表，当前无别名。
func (t *pluginToggleTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *pluginToggleTool) Description() string {
	if t.enable {
		return "启用已安装/已配置的插件（Init → Start → 注册工具，下一轮 ReAct 迭代即可调用）。id 必填。仅 MetaAgent/DomainAgent 可用。"
	}
	return "停用插件（摘除全部工具并停止运行）。id 必填。仅 MetaAgent/DomainAgent 可用。"
}

// InputSchema 返回入参 JSON Schema。
func (t *pluginToggleTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"id": {Type: "string"}},
		Required:   []string{"id"},
	}
}

// Execute 执行 plugin_enable / plugin_disable。
func (t *pluginToggleTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.mgr == nil {
		return &Result{Tool: t.Name(), Error: "插件管理未接线（服务未注入 PluginManager）"}
	}
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return &Result{Tool: t.Name(), Category: ResultCategoryValidationRejected, Error: t.Name() + ": id 必填"}
	}
	var err error
	if t.enable {
		err = t.mgr.Enable(ctx, id)
	} else {
		err = t.mgr.Disable(ctx, id)
	}
	if err != nil {
		return &Result{Tool: t.Name(), Error: err.Error()}
	}
	info, ok := t.mgr.Get(id)
	if !ok {
		return &Result{Tool: t.Name(), Success: true, Output: "插件 " + id + " 操作成功"}
	}
	msg := fmt.Sprintf("插件 %s 已%s（状态=%s", info.ID, map[bool]string{true: "启用", false: "停用"}[t.enable], info.State)
	if len(info.Tools) > 0 {
		msg += "，工具: " + strings.Join(info.Tools, ", ")
	}
	msg += "）"
	if !t.enable && info.LastError != "" {
		msg += "；最近错误: " + info.LastError
	}
	return &Result{Tool: t.Name(), Success: true, Output: msg}
}

// ---- plugin_list ----

// pluginListTool 实现 plugin_list：列出全部插件。
type pluginListTool struct{ mgr PluginManager }

// Name 返回工具名称。
func (t *pluginListTool) Name() string { return "plugin_list" }

// Aliases 返回工具别名列表，当前无别名。
func (t *pluginListTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *pluginListTool) Description() string {
	return "列出全部插件（id/形态/状态/工具清单/缺失 env）。无参数。仅 MetaAgent/DomainAgent 可用。"
}

// InputSchema 返回入参 JSON Schema（无字段）。
func (t *pluginListTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
}

// Execute 执行 plugin_list。
func (t *pluginListTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.mgr == nil {
		return &Result{Tool: "plugin_list", Error: "插件管理未接线（服务未注入 PluginManager）"}
	}
	list := t.mgr.List()
	if len(list) == 0 {
		return &Result{Tool: "plugin_list", Success: true, Output: "当前无插件（plugins.yaml 为空）。需要能力可先 plugin_search 再 plugin_install。"}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("插件列表（%d 个）：\n", len(list)))
	for _, p := range list {
		b.WriteString(fmt.Sprintf("  %s（%s，%s，enabled=%v）", p.ID, p.Kind, p.State, p.Enabled))
		if len(p.Tools) > 0 {
			b.WriteString(" 工具=" + strings.Join(p.Tools, ","))
		}
		if len(p.MissingEnv) > 0 {
			b.WriteString(" 缺env=" + strings.Join(p.MissingEnv, ","))
		}
		if p.LastError != "" {
			b.WriteString(" 最近错误=" + p.LastError)
		}
		b.WriteString("\n")
	}
	return &Result{Tool: "plugin_list", Success: true, Output: b.String()}
}

// ---- 小工具 ----

// stringSliceArg 从 args 提取 []string 参数（兼容 []any）。
func stringSliceArg(args map[string]any, key string) []string {
	var out []string
	switch v := args[key].(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// stringMapArg 从 args 提取 map[string]string 参数（兼容 map[string]any）。
func stringMapArg(args map[string]any, key string) map[string]string {
	out := map[string]string{}
	switch v := args[key].(type) {
	case map[string]string:
		for k, val := range v {
			out[k] = val
		}
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// argNonEmpty 判断 args 中 key 的字符串值是否非空。
func argNonEmpty(args map[string]any, key string) bool {
	v, _ := args[key].(string)
	return strings.TrimSpace(v) != ""
}
