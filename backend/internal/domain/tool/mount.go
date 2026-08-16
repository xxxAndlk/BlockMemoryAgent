package tool

// mount.go 实现插件工具按需挂载（TODO #52 插件分配给 Agent 的机制）：
//   - 权限模型 = 静态权限天花板 + Agent 天花板内自选（分层混合）：
//     天花板（角色基础工具白名单 ∪ 插件 Manifest.Roles 白名单）由配置声明、Agent 不可改；
//     天花板内 Agent 经 tool_catalog 发现、tool_mount 挂载、tool_unmount 卸载，
//     Schema 注入 = 白名单 ∪（天花板 ∩ 已挂载集），默认收窄为角色基础工具。
//   - 挂载集按 scope（agentID）隔离：MetaAgent 持 sessionID、子 Agent 持 subAgentID，
//     兄弟互不可见；越界挂载（超出天花板）直接拒绝并记日志，绝不静默放行。
//   - 消费方：agent 包 toolRegistryAdapter.Schema() 读取挂载集收窄插件工具可见性；
//     dispatcher 派发前按 tools_hint 预挂载子 Agent scope（TODO #52 执行项 4）。

import (
	"log"
	"strings"
)

// PluginVisibilityFunc 判定插件工具对指定角色的可见性（权限天花板，TODO #52）：
// fn(roleID, toolName) -> (owned, visible)：owned=true 表示该工具由插件注册（非内置工具），
// visible 表示插件 Manifest.Roles 是否允许该角色且插件处于 running 态。
// 由 bootstrap 注入 plugins.Manager.ToolVisibility；nil 时挂载不做天花板校验（不拦截）。
type PluginVisibilityFunc func(roleID, toolName string) (owned, visible bool)

// SetPluginVisibility 注入插件工具角色可见性回调（权限天花板，TODO #52）。
// 由 bootstrap 注入 plugins.Manager.ToolVisibility（与 agent 侧 ToolVisibilityFunc 同源）；
// 供 MountForScope 校验挂载是否越界、tool_catalog 枚举天花板内工具。
// nil 时不拦截挂载（无插件配置的部署零行为变化）。
func (r *Registry) SetPluginVisibility(fn PluginVisibilityFunc) {
	r.pluginVisibility = fn
}

// MountedTools 返回 scope（agentID）当前已挂载的插件工具名快照。
// 供 agent 包 toolRegistryAdapter.Schema() 收窄插件工具可见集（TODO #52 执行项 3）。
// scope 无挂载记录时返回空 map（非 nil，调用方可直接索引）。
func (r *Registry) MountedTools(scope string) map[string]bool {
	r.mountedMu.RLock()
	defer r.mountedMu.RUnlock()
	if scope == "" {
		return map[string]bool{}
	}
	set := r.mounted[scope]
	out := make(map[string]bool, len(set))
	for name := range set {
		out[name] = true
	}
	return out
}

// MountForScope 把插件工具挂载进 scope（agentID）的挂载集，校验权限天花板（TODO #52）：
//   - 工具必须已注册（未注册 → 拒绝）；
//   - 工具必须是插件工具（owned=true）——非插件工具由角色基础白名单管理，无需挂载；
//   - 插件工具必须对该角色可见（visible=true）——越界直接拒绝并记日志；
//   - pluginVisibility 为 nil 时不拦截（无插件配置部署零变化）。
//
// 返回 (accepted, rejected)：rejected 附拒绝原因，调用方（tool_mount / dispatcher tools_hint）
// 原样展示给 LLM；scope 为空（ctx 无 agentID）时全部拒绝（挂载无处落地）。
// 并发安全；幂等（重复挂载同工具 no-op）。
func (r *Registry) MountForScope(scope, roleID string, names []string) (accepted, rejected []string) {
	if scope == "" {
		rejected = make([]string, 0, len(names))
		for _, n := range names {
			rejected = append(rejected, n+"（无法定位调用者作用域）")
		}
		return nil, rejected
	}
	r.mountedMu.Lock()
	defer r.mountedMu.Unlock()
	if r.mounted == nil {
		r.mounted = make(map[string]map[string]bool)
	}
	set := r.mounted[scope]
	if set == nil {
		set = make(map[string]bool)
		r.mounted[scope] = set
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if reason := r.mountDeniedReason(name, roleID); reason != "" {
			rejected = append(rejected, name+"（"+reason+"）")
			log.Printf("[tool] mount denied: scope=%s role=%s tool=%s reason=%s", scope, roleID, name, reason)
			continue
		}
		if !set[name] {
			set[name] = true
			log.Printf("[tool] mount: scope=%s role=%s tool=%s", scope, roleID, name)
		}
		accepted = append(accepted, name)
	}
	return accepted, rejected
}

// mountDeniedReason 返回挂载拒绝原因；空串表示允许挂载。
// 调用方持 mountedMu 写锁；r.tools 由 r.mu 保护，读时加读锁（防热插拔 Register/Unregister 并发）。
func (r *Registry) mountDeniedReason(name, roleID string) string {
	r.mu.RLock()
	_, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "工具未注册"
	}
	if r.pluginVisibility == nil {
		return "" // 无插件可见性配置：不拦截（天花板语义由配置缺失自然退化为不限制）
	}
	owned, visible := r.pluginVisibility(roleID, name)
	if !owned {
		return "非插件工具，由角色基础白名单管理，无需挂载"
	}
	if !visible {
		return "超出角色权限天花板（插件 roles 白名单不允许该角色）"
	}
	return ""
}

// UnmountTools 从 scope 的挂载集移除插件工具（收窄可见集，无需天花板校验——卸载是收缩操作）。
// 幂等；scope 无记录时 no-op。
func (r *Registry) UnmountTools(scope string, names []string) {
	if scope == "" {
		return
	}
	r.mountedMu.Lock()
	defer r.mountedMu.Unlock()
	set := r.mounted[scope]
	if set == nil {
		return
	}
	for _, name := range names {
		delete(set, name)
		log.Printf("[tool] unmount: scope=%s tool=%s", scope, name)
	}
	if len(set) == 0 {
		delete(r.mounted, scope)
	}
}
