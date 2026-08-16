package plugins

// install.go 实现插件动态安装（TODO #51 插件系统 Agent 自安装闭环）：
//   - Manager.Install(ctx, manifest, settings, enabled)：校验 → 持久化到
//     plugins.installed.yaml（原子写）→ 复用 createInstance/Enable 立即生效；
//     任一步失败回滚（摘实例 + 移除已写条目），绝不留下半安装状态；
//   - 持久化文件与 plugins.yaml 并列（同目录 plugins.installed.yaml），
//     不覆盖用户手写的 plugins.yaml（注释/编排保留）；loadDesired 合并时
//     plugins.yaml 手工条目优先于 installed 条目；
//   - 并发安全：installMu 串行化安装/回滚；写文件走临时文件 + rename 原子替换。

import (
	"context" // 生命周期上下文
	"fmt"     // 错误包装
	"os"      // 文件读写
	"path/filepath" // 路径拼接
	"regexp"  // 插件 ID 合法性校验
	"strings" // 错误文案

	"github.com/blockmemory/agent/backend/internal/config" // installed 文件解析/合并
	"gopkg.in/yaml.v3"                                    // installed 文件序列化
)

// installedConfigName 是动态安装条目的持久化文件名（与 plugins.yaml 同目录）。
const installedConfigName = "plugins.installed.yaml"

// 插件 ID 合法字符集：字母数字、点、下划线、连字符（容器名/文件路径安全）。
var pluginIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// 可安装的插件形态（builtin 编译进二进制、bundle 需目录包，均不可动态安装）。
func installableKind(k Kind) bool {
	return k == KindMCP || k == KindService
}

// installedConfigPath 推导 installed 文件路径。
func installedConfigPath(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, installedConfigName)
}

// validateInstall 校验安装请求：ID 合法、形态可安装、settings 满足该形态的最小契约。
// 返回错误时不做任何写入。
func validateInstall(manifest Manifest, settings map[string]any) error {
	if !pluginIDPattern.MatchString(manifest.ID) {
		return fmt.Errorf("插件 ID %q 非法（仅允许字母数字与 ._-，长度 1-64）", manifest.ID)
	}
	if strings.HasPrefix(manifest.ID, "bundle/") {
		return fmt.Errorf("插件 ID %q 与 bundle 展开命名空间冲突", manifest.ID)
	}
	if !installableKind(manifest.Kind) {
		return fmt.Errorf("插件形态 %q 不可动态安装（仅支持 mcp/service）", manifest.Kind)
	}
	// 天花板红线（TODO #52 执行项 1）：destructive 插件（工具全进审批链，如 computer_use）
	// 必须显式声明可见角色——缺省/["*"] 全角色授权是权限事故，安装时直接拒绝，绝不停留在文档层。
	if destructive, _ := settings["destructive"].(bool); destructive && !explicitRoles(manifest.Roles) {
		return fmt.Errorf(
			"destructive 插件 %q 必须显式声明可见角色（roles 白名单，禁止缺省或 [\"*\"] 全角色授权；"+
				"例：roles=[\"meta\"]）——权限分配仅人改配置，Agent 不可放权", manifest.ID)
	}
	switch manifest.Kind {
	case KindMCP:
		transport, _ := settings["transport"].(string)
		switch transport {
		case "", "stdio":
			if cmd, _ := settings["command"].(string); strings.TrimSpace(cmd) == "" {
				return fmt.Errorf("mcp 插件 %q: stdio 传输必须提供 settings.command", manifest.ID)
			}
		case "http":
			if u, _ := settings["url"].(string); !strings.HasPrefix(u, "http") {
				return fmt.Errorf("mcp 插件 %q: http 传输必须提供 settings.url", manifest.ID)
			}
		case "docker":
			if img, _ := settings["image"].(string); strings.TrimSpace(img) == "" {
				return fmt.Errorf("mcp 插件 %q: docker 传输必须提供 settings.image", manifest.ID)
			}
		default:
			return fmt.Errorf("mcp 插件 %q: 未知 transport %q（支持 stdio/http/docker）", manifest.ID, transport)
		}
	case KindService:
		if img, _ := settings["image"].(string); strings.TrimSpace(img) == "" {
			return fmt.Errorf("service 插件 %q: 必须提供 settings.image", manifest.ID)
		}
	}
	return nil
}

// Install 动态安装插件：校验 → 持久化 plugins.installed.yaml → 复用
// createInstance/Enable 立即生效。失败回滚（摘实例 + 移除持久化条目）。
// 返回安装后的插件信息视图；已存在同 ID 条目（plugins.yaml 或 installed）拒绝安装。
func (m *Manager) Install(ctx context.Context, manifest Manifest, settings map[string]any, enabled bool) (Info, error) {
	if err := validateInstall(manifest, settings); err != nil {
		return Info{}, err
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()

	// 冲突检查：plugins.yaml / installed / 运行实例任一已有同 ID 即拒绝
	//（覆盖语义易踩坑：settings 深度合并与用户手写条目互相覆盖不可预期）。
	desired, err := m.loadDesired()
	if err != nil {
		return Info{}, fmt.Errorf("读取插件配置失败: %w", err)
	}
	if _, ok := desired[manifest.ID]; ok {
		return Info{}, fmt.Errorf("插件 %q 已存在（plugins.yaml 或已安装），如需改配置请直接编辑 plugins.yaml 后 /api/plugins/reload", manifest.ID)
	}
	if m.getInstance(manifest.ID) != nil {
		return Info{}, fmt.Errorf("插件 %q 已在运行实例表中，拒绝重复安装", manifest.ID)
	}

	// 1. 持久化（原子写：临时文件 + rename）。
	// 落 roles 声明（TODO #52 执行项 1）：安装条目显式声明可见角色（缺省 ["*"]），
	// 重启后 installed yaml → createInstance → 插件 Manifest 的可见性语义与安装时一致，
	// 分配关系可审计——"分配 = 权限声明，仅人改配置"。
	pc := config.PluginConfig{
		Kind:        string(manifest.Kind),
		EnabledFlag: boolPtr(enabled),
		Settings:    mergeSettings(settings, map[string]any{"roles": declaredRoles(manifest, settings)}),
	}
	if err := m.persistInstalled(manifest.ID, pc); err != nil {
		return Info{}, fmt.Errorf("写入插件配置失败: %w", err)
	}

	// 2. 创建实例 + 启用；失败回滚。
	rollback := func(cause error) (Info, error) {
		_ = m.Disable(ctx, manifest.ID)
		m.mu.Lock()
		delete(m.instances, manifest.ID)
		m.mu.Unlock()
		_ = m.dropInstalled(manifest.ID)
		return Info{}, cause
	}
	if err := m.createInstance(manifest.ID, pc); err != nil {
		return rollback(fmt.Errorf("装载插件 %q 失败: %w", manifest.ID, err))
	}
	if enabled {
		if err := m.Enable(ctx, manifest.ID); err != nil {
			return rollback(fmt.Errorf("启用插件 %q 失败（已回滚安装）: %w", manifest.ID, err))
		}
	}
	info, _ := m.Get(manifest.ID)
	m.logger.Info("插件已安装", "plugin", manifest.ID, "kind", manifest.Kind, "enabled", enabled)
	return info, nil
}

// persistInstalled 把条目写入 plugins.installed.yaml（合并已有条目，原子替换）。
// 已存在同 ID 条目时整体覆盖（Install 前已做冲突检查，此处为防御）。
func (m *Manager) persistInstalled(id string, pc config.PluginConfig) error {
	path := installedConfigPath(m.configDir)
	if path == "" {
		return fmt.Errorf("未配置插件配置目录（Manager 未注入 configDir）")
	}
	cfg, err := config.LoadPluginsConfig(path)
	if err != nil {
		return err
	}
	if cfg.Plugins == nil {
		cfg.Plugins = map[string]config.PluginConfig{}
	}
	cfg.Plugins[id] = pc
	return m.writeInstalledFile(path, cfg)
}

// dropInstalled 从 plugins.installed.yaml 移除条目（安装失败回滚 / 未来卸载用）。
func (m *Manager) dropInstalled(id string) error {
	path := installedConfigPath(m.configDir)
	if path == "" {
		return nil
	}
	cfg, err := config.LoadPluginsConfig(path)
	if err != nil {
		return err
	}
	if _, ok := cfg.Plugins[id]; !ok {
		return nil
	}
	delete(cfg.Plugins, id)
	return m.writeInstalledFile(path, cfg)
}

// writeInstalledFile 原子写 installed 文件：临时文件 + rename（防写坏丢配置）。
func (m *Manager) writeInstalledFile(path string, cfg *config.PluginsConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化插件配置失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadInstalled 读取 plugins.installed.yaml（不存在返回空配置）。
func loadInstalled(dir string) (*config.PluginsConfig, error) {
	path := installedConfigPath(dir)
	if path == "" {
		return &config.PluginsConfig{}, nil
	}
	return config.LoadPluginsConfig(path)
}

// explicitRoles 判断 roles 是否为显式授权列表（非空且不含通配符 "*"）。
func explicitRoles(roles []string) bool {
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if r == "*" {
			return false
		}
	}
	return true
}

// declaredRoles 归一化安装的角色声明（TODO #52）：manifest.Roles 优先，
// 其次 settings["roles"]（目录条目/请求覆盖），都缺省时显式写 ["*"]。
// 返回 []any 以匹配 settings 内 roles 的 YAML 反序列化形态（mcpbridge 按 []any 解析）。
func declaredRoles(manifest Manifest, settings map[string]any) []any {
	var roles []string
	if explicitRoles(manifest.Roles) {
		roles = manifest.Roles
	} else if rv, ok := settings["roles"].([]any); ok && len(rv) > 0 {
		for _, r := range rv {
			if s, ok := r.(string); ok && s != "" {
				roles = append(roles, s)
			}
		}
		if !explicitRoles(roles) {
			roles = nil
		}
	}
	if len(roles) == 0 {
		roles = []string{"*"}
	}
	out := make([]any, len(roles))
	for i, r := range roles {
		out[i] = r
	}
	return out
}

// registrySources 返回远程 registry 来源白名单：
// plugins.yaml registry_sources 显式配置优先；空时回退 MCP 官方 registry。
func (m *Manager) registrySources() []string {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg != nil && len(cfg.RegistrySources) > 0 {
		out := make([]string, len(cfg.RegistrySources))
		copy(out, cfg.RegistrySources)
		return out
	}
	return []string{"https://registry.modelcontextprotocol.io/v0/servers"}
}

// Search 聚合检索插件目录（TODO #51 plugin_search）：
//   - 内置精选目录（SearchCatalog）；
//   - 已配置/已安装实例（带当前状态与工具清单，Source=config）；
//   - 远程 registry（仅查询 registrySources 白名单内 URL，失败记 note 不阻断）。
//
// 结果写入 searchCache 供 Install 按 ID 解析安装（install 不重复检索）。
func (m *Manager) Search(ctx context.Context, query string) ([]CatalogEntry, []string) {
	var out []CatalogEntry
	// 1. 内置目录。
	out = append(out, SearchCatalog(query)...)
	// 2. 已配置/已安装实例（按 ID/名称/描述匹配；query 为空全量）。
	q := strings.ToLower(strings.TrimSpace(query))
	m.mu.RLock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if info, ok := m.Get(id); ok {
			if q != "" && !strings.Contains(strings.ToLower(info.ID), q) &&
				!strings.Contains(strings.ToLower(info.Name), q) &&
				!strings.Contains(strings.ToLower(info.Description), q) {
				continue
			}
			out = append(out, CatalogEntry{
				ID:          info.ID,
				Name:        info.Name,
				Version:     info.Version,
				Description: info.Description,
				Kind:        Kind(info.Kind),
				Roles:       info.Roles,
				Source:      sourceConfig,
				State:       string(info.State),
				Tools:       info.Tools,
			})
		}
	}
	// 3. 远程 registry。
	remote, notes := fetchRemoteRegistry(ctx, m.registrySources(), query)
	out = append(out, remote...)
	// 截断输出上限。
	if len(out) > searchResultLimit {
		out = out[:searchResultLimit]
	}
	// 写检索缓存（安装按 ID 解析用）。
	m.searchCacheMu.Lock()
	for _, e := range out {
		if e.Source != sourceConfig {
			m.searchCache[e.ID] = e
		}
	}
	m.searchCacheMu.Unlock()
	return out, notes
}

// resolveCatalogEntry 按 ID 解析可安装条目：内置目录 → 已配置实例（冲突语义）→
// 检索缓存 → 远程 registry 实时查。供 plugin_install 按 search 结果 ID 直接安装。
func (m *Manager) resolveCatalogEntry(ctx context.Context, id string) (CatalogEntry, bool) {
	for _, e := range SearchCatalog(id) {
		if e.ID == id {
			return e, true
		}
	}
	// 已配置/已安装实例：解析命中以便 Install 给出清晰的"已存在"冲突错误。
	if info, ok := m.Get(id); ok {
		return CatalogEntry{
			ID:          info.ID,
			Name:        info.Name,
			Version:     info.Version,
			Description: info.Description,
			Kind:        Kind(info.Kind),
			Roles:       info.Roles,
			Source:      sourceConfig,
			State:       string(info.State),
			Tools:       info.Tools,
		}, true
	}
	m.searchCacheMu.Lock()
	e, ok := m.searchCache[id]
	m.searchCacheMu.Unlock()
	if ok {
		return e, true
	}
	remote, _ := fetchRemoteRegistry(ctx, m.registrySources(), id)
	for _, r := range remote {
		if r.ID == id {
			m.searchCacheMu.Lock()
			m.searchCache[id] = r
			m.searchCacheMu.Unlock()
			return r, true
		}
	}
	return CatalogEntry{}, false
}
