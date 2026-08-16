package plugins

// manager.go 实现插件管理器（设计文档 §4）：
//   - 注册表 map[string]*instance（RWMutex），instance 记录插件 + 状态机；
//   - Enable：Init → Start → 注册工具；失败回滚（已注册工具摘除）；
//   - Disable：先 Unregister 全部工具，再 Stop；
//   - 断线降级：mcp 桥经生命周期回调把状态置 degraded + 自动摘除，重连成功恢复；
//   - Reload：重读 plugins.yaml + 重扫 plugins.d/，差异应用（仿 soul.Loader.Reload）。

import (
	"context"     // 生命周期上下文
	"fmt"         // 错误包装
	"log/slog"    // 结构化日志
	"reflect"     // 配置差异比对
	"strings"     // 错误文案拼接
	"sync"        // 注册表锁

	"github.com/blockmemory/agent/backend/internal/config"  // 插件配置
	"github.com/blockmemory/agent/backend/internal/domain/tool" // 工具挂载目标
	"github.com/blockmemory/agent/backend/internal/plugins/bundle" // bundle 目录包展开
	"github.com/blockmemory/agent/backend/internal/skill" // bundle skill 注入
	"github.com/blockmemory/agent/backend/pkg/types" // Skill 类型
)

// MCPFactory 构造 mcp kind 插件实例。
// 由 bootstrap 注入 mcpbridge.NewFromSettings（plugins 包不直接依赖 MCP SDK）；
// 测试可注入 fake 工厂。
type MCPFactory func(id string, settings map[string]any, deps Deps) (Plugin, error)

// Option 配置 Manager。
type Option func(*Manager)

// WithLogger 注入结构化日志器。
func WithLogger(l *slog.Logger) Option { return func(m *Manager) { m.logger = l } }

// WithConfigDir 设置 plugins.yaml 所在目录（与 config.yaml 同目录惯例）。
func WithConfigDir(dir string) Option { return func(m *Manager) { m.configDir = dir } }

// WithBundlesDir 设置 plugins.d/ 目录（外部插件包扫描目录）。
func WithBundlesDir(dir string) Option { return func(m *Manager) { m.bundlesDir = dir } }

// WithSkillPool 注入 skill 池（bundle 插件包的 SKILL.md 条目注入目标）。
func WithSkillPool(p *skill.Pool) Option { return func(m *Manager) { m.skillPool = p } }

// WithWorkDir 设置 Agent 工作目录（透传 Deps）。
func WithWorkDir(dir string) Option { return func(m *Manager) { m.workDir = dir } }

// WithMCPFactory 注入 mcp kind 插件工厂。
func WithMCPFactory(f MCPFactory) Option { return func(m *Manager) { m.mcpFactory = f } }

// WithConfig 直接注入配置（测试用；跳过文件加载）。
func WithConfig(cfg *config.PluginsConfig) Option { return func(m *Manager) { m.cfg = cfg } }

// instance 是单个插件的运行时记录。
type instance struct {
	id       string
	plugin   Plugin
	manifest Manifest
	config   config.PluginConfig

	lock  sync.Mutex // 串行化 Enable/Disable/生命周期回调对同一实例的操作
	state State
	tools []tool.Tool
	lastErr string

	cancel context.CancelFunc // Enable 时创建的生命周期 ctx（Disable 时取消，桥重连循环据此退出）
}

// Info 是管理 API 输出的插件视图。
type Info struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind"`
	State       State  `json:"state"`
	Enabled     bool   `json:"enabled"`
	Tools       []string `json:"tools,omitempty"`
	URL         string   `json:"url,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	MissingEnv  []string `json:"missing_env,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// Manager 是插件注册表 + 状态机。
type Manager struct {
	registry *tool.Registry

	mu        sync.RWMutex
	instances map[string]*instance
	builtins  map[string]Plugin // 程序化注册的 builtin 插件（kind=builtin）
	cfg       *config.PluginsConfig
	bundleSkills map[string][]*types.Skill // 目录名 → 已注入 skill（Reload 差异摘除）

	configDir  string
	bundlesDir string
	skillPool  *skill.Pool
	logger     *slog.Logger
	workDir    string
	mcpFactory MCPFactory
}

// NewManager 创建插件管理器。
func NewManager(registry *tool.Registry, opts ...Option) *Manager {
	m := &Manager{
		registry:     registry,
		instances:    make(map[string]*instance),
		builtins:     make(map[string]Plugin),
		bundleSkills: make(map[string][]*types.Skill),
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// RegisterBuiltin 程序化注册 builtin kind 插件（编译进二进制的进程内插件）。
func (m *Manager) RegisterBuiltin(p Plugin) {
	if p == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.builtins[p.Manifest().ID] = p
}

// Load 装载配置 + 扫描 bundles，构造实例表（全部停在 registered 态），
// 并自动 enable 配置 enabled: true 的插件。启动期调用；Reload 复用同一实现。
func (m *Manager) Load(ctx context.Context) error {
	desired, err := m.loadDesired()
	if err != nil {
		return err
	}
	m.applyDesired(ctx, desired)
	return nil
}

// Reload 重读 plugins.yaml + 重扫 plugins.d/ 并差异应用（设计文档 §5 管理 API）。
func (m *Manager) Reload(ctx context.Context) error {
	return m.Load(ctx)
}

// loadDesired 构建期望插件表：yaml 条目 + bundle 展开条目（yaml 覆盖 bundle 派生）。
func (m *Manager) loadDesired() (map[string]config.PluginConfig, error) {
	desired := map[string]config.PluginConfig{}

	// 1. plugins.yaml（或注入的配置）：配置目录非空时每次都重读磁盘
	//（Reload 语义：磁盘改动生效；注入配置仅测试用，不设目录）。
	var cfg *config.PluginsConfig
	if m.configDir != "" {
		var err error
		cfg, err = config.LoadPluginsConfig(configPath(m.configDir))
		if err != nil {
			return nil, err
		}
	} else {
		cfg = m.cfg
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	if cfg != nil {
		for id, pc := range cfg.Plugins {
			desired[id] = pc
		}
	}

	// builtin 插件：程序化注册即装载（enabled 缺省 false，需 yaml 显式开启）。
	m.mu.RLock()
	for id := range m.builtins {
		if _, ok := desired[id]; !ok {
			desired[id] = config.PluginConfig{Kind: "builtin"}
		}
	}
	m.mu.RUnlock()

	// 2. plugins.d/ bundle 展开
	if m.bundlesDir != "" {
		bundles, errs := bundle.Scan(m.bundlesDir)
		for _, e := range errs {
			m.logger.Warn("plugins.d 扫描警告", "err", e)
		}
		// 新目录的 skill 先注入（幂等），消失目录的 skill 摘除。
		m.syncBundleSkills(bundles)
		for _, b := range bundles {
			// 目录级启停：plugins.yaml 中 kind=bundle 且键=目录名、enabled=false 时整包禁用。
			if pc, ok := desired[b.Dir]; ok && pc.Kind == "bundle" && !pc.Enabled() {
				continue
			}
			for _, s := range b.Servers {
				id := bundlePluginID(b.Dir, s.Name)
				base := bundleServerSettings(s)
				// yaml 同 ID 条目覆盖 bundle 派生值（如禁用一个 server）。
				if pc, ok := desired[id]; ok {
					merged := mergeSettings(base, pc.Settings)
					pc.Settings = merged
					if pc.Kind == "" {
						pc.Kind = "mcp"
					}
					desired[id] = pc
					continue
				}
				desired[id] = config.PluginConfig{
					Kind:       "mcp",
					EnabledFlag: boolPtr(true), // bundle 插件默认启用
					Settings:   base,
				}
			}
		}
	}
	return desired, nil
}

// applyDesired 差异应用：创建/替换/移除实例，然后 enable 期望运行的插件。
func (m *Manager) applyDesired(ctx context.Context, desired map[string]config.PluginConfig) {
	// 第一轮：收集消失的与配置变更的实例（配置 DeepEqual 比对）。
	m.mu.RLock()
	var toRemove, toReplace []string
	for id, inst := range m.instances {
		pc, ok := desired[id]
		if !ok {
			toRemove = append(toRemove, id)
			continue
		}
		if !reflect.DeepEqual(inst.config, pc) {
			toReplace = append(toReplace, id)
		}
	}
	m.mu.RUnlock()

	// 第二轮：停用并移除（含替换旧实例）。
	for _, id := range append(toRemove, toReplace...) {
		if err := m.Disable(ctx, id); err != nil {
			m.logger.Warn("插件移除失败", "plugin", id, "err", err)
		}
		m.mu.Lock()
		delete(m.instances, id)
		m.mu.Unlock()
	}

	// 第三轮：新建 desired 中缺失的实例（含刚被替换移除的）。
	m.mu.RLock()
	var toCreate []struct {
		id string
		pc config.PluginConfig
	}
	for id, pc := range desired {
		if _, ok := m.instances[id]; !ok {
			toCreate = append(toCreate, struct {
				id string
				pc config.PluginConfig
			}{id, pc})
		}
	}
	m.mu.RUnlock()
	for _, c := range toCreate {
		if err := m.createInstance(c.id, c.pc); err != nil {
			m.logger.Error("插件装载失败", "plugin", c.id, "err", err)
			continue
		}
	}
	// 自动 enable 期望运行的插件（失败仅记录，不阻断启动/重载）。
	for id, pc := range desired {
		if pc.Enabled() {
			if err := m.Enable(ctx, id); err != nil {
				m.logger.Warn("插件自动启用失败", "plugin", id, "err", err)
			}
		}
	}
}

// createInstance 按配置构造插件实例（registered 态）；失败返回错误。
func (m *Manager) createInstance(id string, pc config.PluginConfig) error {
	var plugin Plugin
	var err error
	deps := Deps{Logger: m.logger, Config: pc.Settings, WorkDir: m.workDir}
	switch pc.Kind {
	case "mcp":
		if m.mcpFactory == nil {
			return fmt.Errorf("插件 %q: kind=mcp 但 mcp 工厂未注入（bootstrap 未装配 mcpbridge）", id)
		}
		plugin, err = m.mcpFactory(id, pc.Settings, deps)
		if err != nil {
			return err
		}
	case "builtin":
		p, ok := m.builtins[id]
		if !ok {
			return fmt.Errorf("插件 %q: 未知 builtin 插件（未程序化注册）", id)
		}
		plugin = p
	case "service":
		// Docker 长驻 HTTP 服务（无工具注入），plugins 包内直接构造，无需工厂。
		plugin = newServicePlugin(id, pc.Settings, m.logger)
	default:
		return fmt.Errorf("插件 %q: 未知 kind %q（支持 builtin/mcp/bundle/service）", id, pc.Kind)
	}
	inst := &instance{
		id:       id,
		plugin:   plugin,
		manifest: plugin.Manifest(),
		config:   pc,
		state:    StateRegistered,
	}
	m.mu.Lock()
	m.instances[id] = inst
	m.mu.Unlock()
	return nil
}

// Enable 启用插件：环境检查 → Init → Start → 注册工具；任一步失败回滚。
// 幂等：running 状态直接返回 nil。
func (m *Manager) Enable(ctx context.Context, id string) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	if inst.state == StateRunning {
		return nil // 幂等
	}
	// RequiresEnv 前置检查：缺失拒绝 enable 并给出原因。
	if missing := inst.manifest.MissingEnv(); len(missing) > 0 {
		inst.lastErr = fmt.Sprintf("缺失环境变量: %s", strings.Join(missing, ", "))
		inst.state = StateRegistered
		return fmt.Errorf("启用插件 %q 失败: %s", id, inst.lastErr)
	}
	if err := inst.plugin.Init(ctx, Deps{Logger: m.logger, Config: inst.config.Settings, WorkDir: m.workDir}); err != nil {
		inst.lastErr = err.Error()
		inst.state = StateRegistered
		return fmt.Errorf("初始化插件 %q 失败: %w", id, err)
	}
	inst.state = StateInitialized
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	if err := inst.plugin.Start(lifecycleCtx); err != nil {
		cancel()
		inst.lastErr = err.Error()
		inst.state = StateRegistered
		return fmt.Errorf("启动插件 %q 失败: %w", id, err)
	}
	// 注册工具：冲突预检 + 失败回滚（已注册的摘除，插件 Stop）。
	tools := inst.plugin.Tools()
	for i, t := range tools {
		if m.registry.Has(t.Name()) {
			for j := 0; j < i; j++ {
				m.registry.Unregister(tools[j].Name())
			}
			cancel()
			_ = inst.plugin.Stop(ctx)
			inst.lastErr = fmt.Sprintf("工具名冲突: %q 已被其他工具占用", t.Name())
			inst.state = StateRegistered
			return fmt.Errorf("注册插件 %q 工具失败: %s", id, inst.lastErr)
		}
	}
	for _, t := range tools {
		m.registry.Register(t)
	}
	inst.tools = tools
	inst.cancel = cancel
	inst.state = StateRunning
	inst.lastErr = ""
	m.logger.Info("插件已启用", "plugin", id, "tools", len(tools))
	return nil
}

// Disable 停用插件：先 Unregister 全部工具，再 Stop。幂等。
func (m *Manager) Disable(ctx context.Context, id string) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	if inst.state == StateStopped {
		return nil // 幂等
	}
	for _, t := range inst.tools {
		m.registry.Unregister(t.Name())
	}
	inst.tools = nil
	if inst.cancel != nil {
		inst.cancel()
		inst.cancel = nil
	}
	if err := inst.plugin.Stop(ctx); err != nil {
		m.logger.Warn("插件停止失败", "plugin", id, "err", err)
	}
	inst.state = StateStopped
	inst.lastErr = ""
	m.logger.Info("插件已停用", "plugin", id)
	return nil
}

// onBridgeUp 是 mcp 桥重连成功的回调：重新注册工具并恢复 running。
func (m *Manager) onBridgeUp(id string, tools []tool.Tool) {
	inst := m.getInstance(id)
	if inst == nil {
		return
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	if inst.state != StateRunning && inst.state != StateDegraded {
		return // 已被 Disable，忽略迟到的回调
	}
	for _, t := range tools {
		if m.registry.Has(t.Name()) {
			m.logger.Warn("插件重连工具冲突，跳过", "plugin", id, "tool", t.Name())
			continue
		}
		m.registry.Register(t)
	}
	inst.tools = tools
	inst.state = StateRunning
	inst.lastErr = ""
}

// onBridgeDown 是 mcp 桥断线回调：自动摘除工具并置 degraded。
func (m *Manager) onBridgeDown(id string, err error) {
	inst := m.getInstance(id)
	if inst == nil {
		return
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	if inst.state != StateRunning && inst.state != StateDegraded {
		return
	}
	for _, t := range inst.tools {
		m.registry.Unregister(t.Name())
	}
	inst.tools = nil
	inst.state = StateDegraded
	inst.lastErr = err.Error()
	m.logger.Warn("插件已降级", "plugin", id, "err", err)
}

// List 返回全部插件的信息视图（按 ID 字典序稳定输出）。
func (m *Manager) List() []Info {
	m.mu.RLock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sortStrings(ids)
	out := make([]Info, 0, len(ids))
	for _, id := range ids {
		if info, ok := m.Get(id); ok {
			out = append(out, info)
		}
	}
	return out
}

// Get 返回单个插件的信息视图。
func (m *Manager) Get(id string) (Info, bool) {
	inst := m.getInstance(id)
	if inst == nil {
		return Info{}, false
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	tools := make([]string, 0, len(inst.tools))
	for _, t := range inst.tools {
		tools = append(tools, t.Name())
	}
	manifest := inst.manifest
	return Info{
		ID:          inst.id,
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		Kind:        string(manifest.Kind),
		State:       inst.state,
		Enabled:     inst.config.Enabled(),
		Tools:       tools,
		URL:         manifest.URL,
		Roles:       manifest.Roles,
		MissingEnv:  manifest.MissingEnv(),
		LastError:   inst.lastErr,
	}, true
}

// ToolVisibility 判定插件工具对指定角色的可见性（设计文档 §4.3 动态可见集）。
// 返回 (owned, visible)：owned=true 表示该工具由插件注册（非内置工具）；
// visible 表示 Manifest.Roles 是否允许该角色（running 态才可见）。
// 非插件工具返回 (false, false)——白名单语义由 adapter 自行保持。
func (m *Manager) ToolVisibility(roleID, toolName string) (owned, visible bool) {
	inst := m.getInstanceByTool(toolName)
	if inst == nil {
		return false, false
	}
	inst.lock.Lock()
	defer inst.lock.Unlock()
	if inst.state != StateRunning {
		return true, false // 已停用/降级的插件工具不可见
	}
	return true, inst.manifest.VisibleForRole(roleID)
}

// StopAll 停用全部插件（App.Close 时调用）。
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if err := m.Disable(ctx, id); err != nil {
			m.logger.Warn("关闭插件失败", "plugin", id, "err", err)
		}
	}
	return nil
}

// getInstance 取实例（仅 RLock 保护 map 本身）。
func (m *Manager) getInstance(id string) *instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

// getInstanceByTool 按工具名找持有它的插件实例（RLock 保护 map 遍历）。
func (m *Manager) getInstanceByTool(toolName string) *instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inst := range m.instances {
		for _, t := range inst.tools {
			if t.Name() == toolName {
				return inst
			}
		}
	}
	return nil
}

// syncBundleSkills 注册新目录 skill、摘除消失目录 skill（幂等）。
func (m *Manager) syncBundleSkills(bundles []*bundle.Bundle) {
	if m.skillPool == nil {
		return
	}
	current := make(map[string]bool, len(bundles))
	for _, b := range bundles {
		current[b.Dir] = true
		for _, s := range b.Skills {
			if s != nil && s.SkillID != "" {
				m.skillPool.Register(s)
			}
		}
	}
	m.mu.Lock()
	for dir, skills := range m.bundleSkills {
		if !current[dir] {
			for _, s := range skills {
				if s != nil {
					m.skillPool.Remove(s.SkillID)
				}
			}
			delete(m.bundleSkills, dir)
		}
	}
	for _, b := range bundles {
		if len(b.Skills) > 0 {
			m.bundleSkills[b.Dir] = b.Skills
		}
	}
	m.mu.Unlock()
}

// ---- 小工具 ----

// configPath 从配置目录推导 plugins.yaml 路径。
func configPath(dir string) string {
	if dir == "" {
		return ""
	}
	if dir[len(dir)-1] == '/' || dir[len(dir)-1] == '\\' {
		dir = dir[:len(dir)-1]
	}
	return dir + "/plugins.yaml"
}

// bundlePluginID bundle 展开插件的稳定 ID：bundle/<目录>/<server>。
func bundlePluginID(dir, server string) string {
	return "bundle/" + dir + "/" + server
}

// bundleServerSettings 把 bundle 的 MCP server 描述转成插件 settings。
func bundleServerSettings(s bundle.MCPServer) map[string]any {
	settings := map[string]any{"transport": "stdio"}
	if s.URL != "" {
		settings["transport"] = "http"
		settings["url"] = s.URL
	}
	if s.Command != "" {
		settings["command"] = s.Command
	}
	if len(s.Args) > 0 {
		args := make([]any, 0, len(s.Args))
		for _, a := range s.Args {
			args = append(args, a)
		}
		settings["args"] = args
	}
	if len(s.Env) > 0 {
		env := make(map[string]any, len(s.Env))
		for k, v := range s.Env {
			env[k] = v
		}
		settings["env"] = env
	}
	return settings
}

// mergeSettings 深度合并 base 与 override（override 优先），返回新 map。
func mergeSettings(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = mergeSettings(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
