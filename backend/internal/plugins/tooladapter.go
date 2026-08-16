package plugins

// tooladapter.go 把 plugins.Manager 适配为 tool.PluginManager（TODO #51）：
// tool 包不能反向依赖 plugins 包（plugins 已依赖 tool），DTO 转换集中在此处，
// bootstrap 装配后经 tool.Registry.SetPluginManager 注入 plugin_* 工具。
//
// 安装解析（Install）：请求只带 ID 时按「内置目录 → 检索缓存 → 远程 registry 实时查」
// 解析目录条目；请求带 transport/command/url/image 等启动配置时按手工 manifest 安装；
// 目录条目可被请求中的 name/description/env/roles 等字段覆盖。

import (
	"context" // 生命周期上下文
	"strings" // 启动配置判定

	"github.com/blockmemory/agent/backend/internal/domain/tool" // 工具包 DTO 与接口
)

// ToolManagerAdapter 是 Manager 的 tool.PluginManager 适配器。
type ToolManagerAdapter struct {
	m *Manager
}

// NewToolManagerAdapter 构造适配器。
func NewToolManagerAdapter(m *Manager) *ToolManagerAdapter {
	return &ToolManagerAdapter{m: m}
}

// List 实现 tool.PluginManager。
func (a *ToolManagerAdapter) List() []tool.PluginInfo {
	infos := a.m.List()
	out := make([]tool.PluginInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, toToolInfo(i))
	}
	return out
}

// Get 实现 tool.PluginManager。
func (a *ToolManagerAdapter) Get(id string) (tool.PluginInfo, bool) {
	info, ok := a.m.Get(id)
	if !ok {
		return tool.PluginInfo{}, false
	}
	return toToolInfo(info), true
}

// Enable 实现 tool.PluginManager。
func (a *ToolManagerAdapter) Enable(ctx context.Context, id string) error {
	return a.m.Enable(ctx, id)
}

// Disable 实现 tool.PluginManager。
func (a *ToolManagerAdapter) Disable(ctx context.Context, id string) error {
	return a.m.Disable(ctx, id)
}

// Install 实现 tool.PluginManager：解析目录条目或手工 manifest 后交给 Manager.Install。
func (a *ToolManagerAdapter) Install(ctx context.Context, req tool.InstallRequest) (tool.PluginInfo, error) {
	manifest, settings, err := a.resolveInstall(ctx, req)
	if err != nil {
		return tool.PluginInfo{}, err
	}
	info, err := a.m.Install(ctx, manifest, settings, req.Enabled)
	if err != nil {
		return tool.PluginInfo{}, err
	}
	return toToolInfo(info), nil
}

// Search 实现 tool.PluginManager。
func (a *ToolManagerAdapter) Search(ctx context.Context, query string) ([]tool.SearchResult, []string) {
	entries, notes := a.m.Search(ctx, query)
	out := make([]tool.SearchResult, 0, len(entries))
	for _, e := range entries {
		out = append(out, tool.SearchResult{
			ID:          e.ID,
			Name:        e.Name,
			Version:     e.Version,
			Description: e.Description,
			Kind:        string(e.Kind),
			Source:      e.Source,
			State:       e.State,
			Tools:       e.Tools,
		})
	}
	return out, notes
}

// resolveInstall 解析安装请求：
//   - 请求自带启动配置（transport/command/url/image）→ 手工 manifest；
//   - 否则按 ID 解析目录条目（内置 → 检索缓存 → 远程 registry），
//     请求中的 name/description/env/destructive 等覆盖条目默认值；
//   - 两者皆无 → 报错提示先 plugin_search。
func (a *ToolManagerAdapter) resolveInstall(ctx context.Context, req tool.InstallRequest) (Manifest, map[string]any, error) {
	manual := hasLaunchConfig(req.Settings)
	if !manual {
		entry, ok := a.m.resolveCatalogEntry(ctx, req.ID)
		if !ok {
			return Manifest{}, nil, &installResolveError{req.ID}
		}
		settings := cloneSettings(entry.Settings)
		if req.Roles != nil {
			settings["roles"] = req.Roles
		}
		manifest := Manifest{
			ID:          entry.ID,
			Name:        entry.Name,
			Version:     entry.Version,
			Description: entry.Description,
			Kind:        entry.Kind,
			RequiresEnv: entry.RequiresEnv,
			Roles:       entry.Roles,
		}
		if req.Name != "" {
			manifest.Name = req.Name
		}
		if req.Description != "" {
			manifest.Description = req.Description
		}
		if req.Version != "" {
			manifest.Version = req.Version
		}
		if req.RequiresEnv != nil {
			manifest.RequiresEnv = req.RequiresEnv
		}
		// 请求 env/destructive 覆盖目录条目默认值（调用方可补全/替换）。
		if s := req.Settings; s != nil {
			if env, ok := s["env"]; ok {
				settings["env"] = env
			}
			if d, ok := s["destructive"]; ok {
				settings["destructive"] = d
			}
		}
		return manifest, settings, nil
	}
	kind := KindMCP
	if req.Kind != "" {
		kind = Kind(req.Kind)
	}
	manifest := Manifest{
		ID:          req.ID,
		Name:        req.Name,
		Description: req.Description,
		Version:     req.Version,
		Kind:        kind,
		RequiresEnv: req.RequiresEnv,
		Roles:       req.Roles,
	}
	if manifest.Name == "" {
		manifest.Name = req.ID
	}
	return manifest, req.Settings, nil
}

// hasLaunchConfig 判断 settings 是否含启动配置（transport/command/url/image 任一非空）。
func hasLaunchConfig(settings map[string]any) bool {
	for _, k := range []string{"transport", "command", "url", "image"} {
		if v, ok := settings[k].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// installResolveError 是目录解析失败的哨兵错误（提示先搜索）。
type installResolveError struct{ id string }

func (e *installResolveError) Error() string {
	return "未找到插件 " + e.id + " 的目录条目，且未提供启动配置。请先 plugin_search 确认 id，或提供完整 manifest（transport + command/url/image）"
}

// toToolInfo 转换 plugins.Info → tool.PluginInfo。
func toToolInfo(i Info) tool.PluginInfo {
	return tool.PluginInfo{
		ID:          i.ID,
		Name:        i.Name,
		Version:     i.Version,
		Description: i.Description,
		Kind:        i.Kind,
		State:       string(i.State),
		Enabled:     i.Enabled,
		Tools:       i.Tools,
		URL:         i.URL,
		Roles:       i.Roles,
		MissingEnv:  i.MissingEnv,
		LastError:   i.LastError,
	}
}

// cloneSettings 深拷贝 settings map（防共享可变状态）。
func cloneSettings(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAnyValue(v)
	}
	return out
}

func cloneAnyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneSettings(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneAnyValue(e)
		}
		return out
	}
	return v
}
