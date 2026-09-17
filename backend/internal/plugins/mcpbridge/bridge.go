// Package mcpbridge 将外部 MCP server 桥接为项目内 tool.Tool（设计文档 §3.2）。
//
// 支持三种传输：
//   - stdio：spawn 子进程（command/args/env），经 IOTransport 桥接；
//   - http：streamable HTTP（url）；
//   - docker：以 `docker run -i --rm` 启动容器（image/args/env），stdio 桥接，
//     停止时显式 `docker rm -f` 回收容器（Windows 下 kill CLI 不转发信号）。
//
// 生命周期由 Bridge 统一管理：Start 建立会话并 ListTools 生成工具适配器；
// 连接断开/子进程退出后自动回调 OnDown + 指数退避重连，重连成功重新 ListTools
// 并回调 OnUp。Manager 依赖这两个回调维护插件状态机与工具注册/摘除。
package mcpbridge

import (
	"context"     // 生命周期与调用上下文
	"encoding/json" // 结果序列化
	"fmt"         // 错误包装
	"log/slog"    // 结构化日志
	"net/http"    // HTTP 客户端
	"os"          // 子进程环境
	"os/exec"     // stdio 子进程
	"strings"     // 结果文本拼接
	"sync"        // 桥接内部状态锁
	"time"        // 退避与超时

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/plugins"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 默认单次工具调用超时（MCP 服务端可能长时间无响应，兜底防挂死 Agent 循环）。
const defaultExecTimeout = 120 * time.Second

// Settings 是 plugins.yaml 中 mcp 插件 settings 段的解析结果。
type Settings struct {
	// Transport 传输方式：stdio（默认）| http | docker。
	Transport string
	// Command / Args / Env 仅 stdio/docker 使用：子进程启动命令与环境。
	Command string
	Args    []string
	Env     map[string]string
	// Image 仅 docker 使用：容器镜像（docker run -i --rm <image>）。
	Image string
	// Ports 仅 docker 使用：端口映射（如 "6081:6081"，noVNC 观察口等）。
	Ports []string
	// Volumes 仅 docker 使用：卷映射（如 "D:/data/od-artifacts:/out"）。宿主侧支持
	// ${WORKDIR} 占位符，Init 时展开为 Agent 工作目录绝对路径（随启动目录解析，多开不串）。
	Volumes []string
	// URL 仅 http 使用：MCP streamable HTTP 端点。
	URL string
	// Shared 为 true 时启用跨实例共享容器模式（plugins.yaml shared，仅 docker transport）：
	// 容器以 `docker run -d` 常驻 keeper 运行（容器内无 MCP 业务进程），每个会话经
	// `docker exec -i` 在容器内起独立 MCP server 进程（stdio 桥接）。多个 BMA 实例
	// （多开 TUI）复用同一容器；容器名带 workdir 哈希（同目录复用，异目录隔离卷映射）。
	// Stop 只断开本会话，不回收容器（常驻，显式回收用 docker rm -f）。
	Shared bool
	// ExecCommand 仅 shared docker 使用：每次会话在容器内 exec 的 MCP server 启动命令
	//（argv 列表）。settings.Args 附加在其后（与 docker run 时 CMD 追加在 ENTRYPOINT
	// 后的语义一致，如 ui_preview 的 --headless 等启动参数）。
	ExecCommand []string
	// ContainerEntrypoint 仅 shared docker 使用：`docker run -d` 的 --entrypoint 覆盖
	//（首字段为 --entrypoint 值，其余作为镜像后参数）。缺省 ["sleep","infinity"] keeper：
	// 容器内无常驻业务进程，MCP server 全部由 exec 按会话拉起。
	// computer_use 等需要入口脚本先起后台服务（Xvfb 桌面）的镜像，配合
	// container_env BMA_KEEPER=1 让脚本只起服务不 exec MCP。
	ContainerEntrypoint []string
	// ContainerEnv 仅 shared docker 使用：`docker run -d` 时额外注入的环境变量
	//（仅守护进程可见；Env 会话与守护进程都可见）。
	ContainerEnv map[string]string
	// Destructive 为 true 时全部远端工具标记 Destructive()，接入审批守卫链。
	Destructive bool
	// ToolDescriptionSuffix 附加到每个远端工具描述尾部（plugins.yaml tool_description_suffix）。
	// 用于沙箱类插件提示模型文件系统边界（如 computer_use 容器内写文件宿主不可见）。
	ToolDescriptionSuffix string
	// ImagePassthrough 为 true 时，远端工具返回的 image content 不再只留文本占位符，
	// 而是随 tool.Result.Images 内存透传给多模态模型（视觉回显闭环）。
	// 缺省 false：文本模型（glm-5.3 等）收到 image block 会被端点 400 拒绝，
	// 仅确认模型支持图片输入的插件（如 ui_preview）显式开启。
	ImagePassthrough bool
	// Roles 可见角色白名单（透传到 Manifest，缺省全角色）。
	Roles []string
	// TopLevel 顶层必备能力（透传到 Manifest.TopLevel）：快速/日常档顶层 Agent 启动时自动预挂载。
	TopLevel bool
	// ExecTimeout 单次工具调用超时（默认 120s）。
	ExecTimeout time.Duration
}

// FromSettings 从 settings map 解析配置；缺失字段取默认值。
func FromSettings(settings map[string]any) Settings {
	s := Settings{
		Transport:   "stdio",
		ExecTimeout: defaultExecTimeout,
	}
	if v, ok := settings["transport"].(string); ok && v != "" {
		s.Transport = v
	}
	if v, ok := settings["command"].(string); ok {
		s.Command = v
	}
	if v, ok := settings["args"].([]any); ok {
		for _, a := range v {
			if str, ok := a.(string); ok {
				s.Args = append(s.Args, str)
			}
		}
	}
	if v, ok := settings["env"].(map[string]any); ok {
		s.Env = make(map[string]string, len(v))
		for k, val := range v {
			if str, ok := val.(string); ok {
				s.Env[k] = str
			}
		}
	}
	if v, ok := settings["url"].(string); ok {
		s.URL = v
	}
	if v, ok := settings["shared"].(bool); ok {
		s.Shared = v
	}
	if v, ok := settings["exec_command"].([]any); ok {
		for _, a := range v {
			if str, ok := a.(string); ok {
				s.ExecCommand = append(s.ExecCommand, str)
			}
		}
	}
	if v, ok := settings["container_entrypoint"].([]any); ok {
		for _, a := range v {
			if str, ok := a.(string); ok {
				s.ContainerEntrypoint = append(s.ContainerEntrypoint, str)
			}
		}
	}
	if v, ok := settings["container_env"].(map[string]any); ok {
		s.ContainerEnv = make(map[string]string, len(v))
		for k, val := range v {
			if str, ok := val.(string); ok {
				s.ContainerEnv[k] = str
			}
		}
	}
	if v, ok := settings["image"].(string); ok {
		s.Image = v
	}
	if v, ok := settings["ports"].([]any); ok {
		for _, p := range v {
			if str, ok := p.(string); ok {
				s.Ports = append(s.Ports, str)
			}
		}
	}
	if v, ok := settings["volumes"].([]any); ok {
		for _, vol := range v {
			if str, ok := vol.(string); ok {
				s.Volumes = append(s.Volumes, str)
			}
		}
	}
	if v, ok := settings["destructive"].(bool); ok {
		s.Destructive = v
	}
	if v, ok := settings["tool_description_suffix"].(string); ok {
		s.ToolDescriptionSuffix = v
	}
	if v, ok := settings["image_passthrough"].(bool); ok {
		s.ImagePassthrough = v
	}
	if v, ok := settings["roles"].([]any); ok {
		for _, r := range v {
			if str, ok := r.(string); ok {
				s.Roles = append(s.Roles, str)
			}
		}
	}
	if v, ok := settings["top_level"].(bool); ok {
		s.TopLevel = v
	}
	if v, ok := settings["exec_timeout_sec"].(float64); ok && v > 0 {
		s.ExecTimeout = time.Duration(v * float64(time.Second))
	}
	return s
}

// NewFromSettings 按 settings 构造实现 plugins.Plugin 的 Bridge。
// 供 bootstrap 注入 Manager 的 mcp 工厂使用。
func NewFromSettings(id string, settings map[string]any, deps plugins.Deps) (plugins.Plugin, error) {
	b := New(id, FromSettings(settings), deps.Logger)
	return b, nil
}

// LifecycleHooks 是 Bridge 与 Manager 之间的回调契约。
type LifecycleHooks struct {
	// OnUp 连接就绪、工具列表更新后回调（初始 Start 成功也走此回调）。
	OnUp func(tools []tool.Tool)
	// OnDown 连接断开/子进程退出后回调（工具已被桥接方摘除）。
	OnDown func(err error)
}

// Bridge 是单个 MCP server 的连接桥。
// 并发安全；同一个 Bridge 实例只服务一个插件。
type Bridge struct {
	id       string
	manifest plugins.Manifest
	settings Settings
	logger   *slog.Logger
	hooks    LifecycleHooks

	client *mcp.Client

	mu      sync.Mutex
	session *mcp.ClientSession
	cmd     *exec.Cmd // stdio 子进程（http 传输为 nil）
	tools   []tool.Tool

	// containerName 仅 shared docker 使用：默认会话（启动目录）的容器名。
	containerName string
	// workDir 仅 shared docker 使用：Init 时记录的启动目录（默认会话的工作目录）。
	workDir string
	// volumesRaw ${WORKDIR} 占位符的原始卷模板：settings.Volumes 在 Init 时已按启动目录
	// 展开，异目录会话要重新展开，故原样留一份。
	volumesRaw []string
	// 按工作目录分容器（见 docker.go dockerSharedContainerName）：只在卷里带
	// ${WORKDIR} 时启用——这类插件的 /workspace 必须指向会话自己的工作目录。
	perWorkDir bool
	// extraSessions 异目录会话缓存（key=归一化工作目录）：容器卷在创建时固定，
	// 异目录会话必须用自己容器的 exec 会话，否则读到/写到别人的项目目录。
	extraSessions map[string]*bridgeSession
	// dialMu 串行化异目录会话的建立（docker run/inspect 是秒级操作，不该占着 b.mu）。
	dialMu sync.Mutex

	// lifecycle
	stopCtx    context.Context
	stopCancel context.CancelFunc
	stopped    bool
}

// New 构造 Bridge（不连接；Start 时连接）。
// id 用于日志与 manifest；settings 为解析后的配置。
func New(id string, settings Settings, logger *slog.Logger) *Bridge {
	if logger == nil {
		logger = slog.Default()
	}
	impl := &mcp.Implementation{Name: "blockmemory-agent", Version: "0.1.0"}
	return &Bridge{
		id: id,
		manifest: plugins.Manifest{
			ID:          id,
			Name:        id,
			Kind:        plugins.KindMCP,
			Description: "MCP 外部插件 " + id,
			Roles:       settings.Roles,
			TopLevel:    settings.TopLevel,
		},
		settings: settings,
		logger:   logger,
		client:   mcp.NewClient(impl, &mcp.ClientOptions{Logger: logger}),
	}
}

// Manifest 实现 plugins.Plugin。
func (b *Bridge) Manifest() plugins.Manifest { return b.manifest }

// Init 实现 plugins.Plugin：校验配置，并展开 volumes 中的 ${WORKDIR} 占位符
//（deps.WorkDir 即 bootstrap 的 os.Getwd()，挂载随启动目录解析，支持多目录多开）。
func (b *Bridge) Init(ctx context.Context, deps plugins.Deps) error {
	b.volumesRaw = append([]string(nil), b.settings.Volumes...)
	for i, v := range b.settings.Volumes {
		b.settings.Volumes[i] = plugins.ExpandWorkDir(v, deps.WorkDir)
		// 卷里带占位符 = 该插件必须看到会话的工作目录 → 按目录分容器（见 docker.go）。
		if strings.Contains(b.volumesRaw[i], plugins.WorkDirPlaceholder) {
			b.perWorkDir = true
		}
	}
	b.extraSessions = map[string]*bridgeSession{}
	if b.settings.Shared && b.settings.Transport != "docker" {
		return fmt.Errorf("mcp 插件 %q: shared 仅支持 docker transport（当前 %q）", b.id, b.settings.Transport)
	}
	switch b.settings.Transport {
	case "stdio", "":
		if b.settings.Command == "" {
			return fmt.Errorf("mcp 插件 %q: stdio transport 需要 settings.command", b.id)
		}
	case "http":
		if b.settings.URL == "" {
			return fmt.Errorf("mcp 插件 %q: http transport 需要 settings.url", b.id)
		}
	case "docker":
		if b.settings.Image == "" {
			return fmt.Errorf("mcp 插件 %q: docker transport 需要 settings.image", b.id)
		}
		if b.settings.Shared {
			if len(b.settings.ExecCommand) == 0 {
				return fmt.Errorf("mcp 插件 %q: shared 模式需要 settings.exec_command（容器内 MCP server 启动命令）", b.id)
			}
			b.workDir = deps.WorkDir
			// 按目录分容器时默认容器也带哈希（与异目录会话同一套命名规则，
			// 免得默认目录的容器名与"某会话目录"的容器名撞车或语义不一）。
			if b.perWorkDir {
				b.containerName = dockerSharedContainerName(b.id, deps.WorkDir)
			} else {
				b.containerName = dockerSharedContainerName(b.id, "")
			}
		}
	default:
		return fmt.Errorf("mcp 插件 %q: 未知 transport %q（支持 stdio/http/docker）", b.id, b.settings.Transport)
	}
	return nil
}

// SetLifecycleHooks 注入连接状态回调（Manager 在 Start 前调用）。
func (b *Bridge) SetLifecycleHooks(h LifecycleHooks) { b.hooks = h }

// Start 实现 plugins.Plugin：建立连接并拉取工具列表。
// 成功返回后 Tools() 即返回可用工具；后续断线由内部重连循环接管（经 hooks 通知）。
// 首次连接失败返回错误（Enable 回滚）；Stop 后再次 Start 会重置状态重新连接
//（热插拔 enable-after-disable 语义，Manager 复用同一实例）。
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		// 重置为可启动状态：旧会话/子进程已在 Stop 中清理。
		b.stopped = false
		b.session = nil
		b.cmd = nil
		b.tools = nil
	}
	b.stopCtx, b.stopCancel = context.WithCancel(context.Background())
	lifecycleCtx := b.stopCtx
	b.mu.Unlock()

	session, cmd, tools, err := b.connect(lifecycleCtx)
	if err != nil {
		b.killCmd(cmd)
		return err
	}
	b.mu.Lock()
	b.session = session
	b.cmd = cmd
	b.tools = tools
	b.mu.Unlock()
	// 注意：初始连接成功不触发 OnUp——首次工具注册由 Manager 在 Start 返回后
	// 经 plugin.Tools() 统一执行（冲突预检 + 回滚）；OnUp 仅用于断线重连恢复。
	// 断线监控 + 退避重连循环：session.Wait() 返回即连接关闭
	//（子进程退出会关管道、HTTP 断开会关连接），随后按指数退避重连。
	go b.supervise(session)
	return nil
}

// connect 建立一次会话并列出工具；失败时清理半开资源。
// 返回的 cmd 供调用方在失败/停止时 kill；成功后归 Bridge 持有。
func (b *Bridge) connect(ctx context.Context) (*mcp.ClientSession, *exec.Cmd, []tool.Tool, error) {
	return b.connectWith(ctx, b.containerName, b.workDir, b.settings.Volumes)
}

// connectWith 按给定的容器与卷建立一次会话（默认会话与异目录会话共用）。
func (b *Bridge) connectWith(ctx context.Context, container, workDir string, volumes []string) (*mcp.ClientSession, *exec.Cmd, []tool.Tool, error) {
	transport, cmd, err := b.newTransport(ctx, container, workDir, volumes)
	if err != nil {
		return nil, nil, nil, err
	}
	session, err := b.client.Connect(ctx, transport, nil)
	if err != nil {
		b.killCmd(cmd)
		return nil, nil, nil, fmt.Errorf("mcp connect %s: %w", b.id, err)
	}
	list, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()
		b.killCmd(cmd)
		return nil, nil, nil, fmt.Errorf("mcp list tools %s: %w", b.id, err)
	}
	tools := make([]tool.Tool, 0, len(list.Tools))
	for _, mt := range list.Tools {
		tools = append(tools, b.wrapTool(mt))
	}
	return session, cmd, tools, nil
}

// newTransport 按 settings 构造传输层：
//   - stdio：spawn 子进程，stdin/stdout 管道桥接（stderr 走日志）；
//   - http：streamable HTTP client transport；
//   - docker：docker run 容器，stdio 管道桥接（见 docker.go）。
func (b *Bridge) newTransport(ctx context.Context, container, workDir string, volumes []string) (mcp.Transport, *exec.Cmd, error) {
	switch b.settings.Transport {
	case "http":
		return &mcp.StreamableClientTransport{
			Endpoint: b.settings.URL,
			// 单次 HTTP 请求超时对齐工具调用超时：自托管 scrape/crawl 等长任务
			// 会阻塞在 tools/call 的 POST 上，30s 级别的默认超时会直接掐断。
			HTTPClient:           &http.Client{Timeout: b.settings.ExecTimeout},
			DisableStandaloneSSE: false,
			MaxRetries:           -1, // 重连由本桥退避循环负责，传输层不自行重试
		}, nil, nil
	case "docker":
		return b.newDockerTransport(ctx, container, workDir, volumes)
	default: // stdio
		cmd := exec.CommandContext(ctx, b.settings.Command, b.settings.Args...)
		cmd.Env = os.Environ()
		for k, v := range b.settings.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, fmt.Errorf("mcp stdio stdin: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, fmt.Errorf("mcp stdio stdout: %w", err)
		}
		cmd.Stderr = &stderrWriter{logger: b.logger, id: b.id}
		if err := cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("mcp spawn %q: %w", b.settings.Command, err)
		}
		return &mcp.IOTransport{Reader: stdout, Writer: stdin}, cmd, nil
	}
}

// supervise 监控会话存活并驱动退避重连，直到 Stop。
func (b *Bridge) supervise(session *mcp.ClientSession) {
	// 等待连接关闭（子进程退出/HTTP 断开/服务端关闭）。
	_ = session.Wait()
	b.mu.Lock()
	if b.stopped || b.session != session {
		b.mu.Unlock()
		return // 已停止或已被更新会话取代（不应发生，重连只在同 goroutine 串行）
	}
	b.session = nil
	_ = session.Close()
	b.cmd = nil // 旧子进程已退出（管道关闭触发 Wait 返回）；残留句柄由进程自然回收
	b.tools = nil
	b.mu.Unlock()
	if b.hooks.OnDown != nil {
		b.hooks.OnDown(fmt.Errorf("mcp 连接断开: %s", b.id))
	}

	// 指数退避重连：1s → 2s → 4s … 上限 30s。
	delay := time.Second
	for {
		select {
		case <-b.stopCtx.Done():
			return
		case <-time.After(delay):
		}
		b.logger.Info("mcp 重连", "plugin", b.id, "attempt_delay", delay.String())
		newSession, cmd, newTools, err := b.connect(b.stopCtx)
		if err != nil {
			b.logger.Warn("mcp 重连失败", "plugin", b.id, "err", err)
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			continue
		}
		b.mu.Lock()
		if b.stopped {
			b.mu.Unlock()
			_ = newSession.Close()
			b.killCmd(cmd)
			return
		}
		b.session = newSession
		b.cmd = cmd
		b.tools = newTools
		b.mu.Unlock()
		b.logger.Info("mcp 重连成功", "plugin", b.id, "tools", len(newTools))
		if b.hooks.OnUp != nil {
			b.hooks.OnUp(newTools)
		}
		b.supervise(newSession)
		return
	}
}

// Stop 实现 plugins.Plugin：取消重连循环、关闭会话、杀掉子进程。幂等。
func (b *Bridge) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	session := b.session
	cmd := b.cmd
	b.session = nil
	b.cmd = nil
	b.tools = nil
	if b.stopCancel != nil {
		b.stopCancel()
	}
	b.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
	b.killCmd(cmd)
	b.closeExtraSessions()
	return nil
}

// Tools 实现 plugins.Plugin：返回当前可用工具适配器。
func (b *Bridge) Tools() []tool.Tool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]tool.Tool(nil), b.tools...)
}

// bridgeSession 一个按工作目录建立的 MCP 会话（容器 + exec 子进程）。
type bridgeSession struct {
	session *mcp.ClientSession
	cmd     *exec.Cmd
}

// sessionFor 取本次调用应使用的会话：插件的卷绑定了 ${WORKDIR} 时，按调用 ctx 里的
// 会话工作目录选容器——异目录用自己的容器（卷指向该目录），同目录/无卷走默认会话。
//
// 建立失败退回默认会话（并记日志）：拿到错目录的产物，好过整个插件用不了。
func (b *Bridge) sessionFor(ctx context.Context) *mcp.ClientSession {
	b.mu.Lock()
	def := b.session
	stopped := b.stopped
	b.mu.Unlock()
	if def == nil || !b.perWorkDir || b.settings.Transport != "docker" || !b.settings.Shared {
		return def
	}
	wd := tool.WorkDirFromContext(ctx)
	if wd == "" || normalizeMountPath(wd) == normalizeMountPath(b.workDir) {
		return def
	}
	key := normalizeMountPath(wd)
	b.mu.Lock()
	if s := b.extraSessions[key]; s != nil {
		b.mu.Unlock()
		return s.session
	}
	b.mu.Unlock()

	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	// 双检：等锁期间别的 goroutine 可能已建好。
	b.mu.Lock()
	if s := b.extraSessions[key]; s != nil {
		b.mu.Unlock()
		return s.session
	}
	b.mu.Unlock()
	if stopped {
		return def
	}
	vols := b.volumesFor(wd)
	container := dockerSharedContainerName(b.id, wd)
	dialCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess, cmd, _, err := b.connectWith(dialCtx, container, wd, vols)
	if err != nil {
		b.logger.Warn("异目录 MCP 会话建立失败，回退默认会话（产物将落默认目录）",
			"plugin", b.id, "workdir", wd, "container", container, "err", err)
		return def
	}
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		_ = sess.Close()
		b.killCmd(cmd)
		return def
	}
	b.extraSessions[key] = &bridgeSession{session: sess, cmd: cmd}
	b.mu.Unlock()
	b.logger.Info("异目录 MCP 会话就绪", "plugin", b.id, "workdir", wd, "container", container)
	// 会话断开即从缓存摘掉（下次调用按需重建）；不接退避重连循环——异目录会话是
	// 按需资源，重建成本远低于维护每个目录一套重连状态机。
	go func() {
		_ = sess.Wait()
		b.mu.Lock()
		if cur := b.extraSessions[key]; cur != nil && cur.session == sess {
			delete(b.extraSessions, key)
		}
		b.mu.Unlock()
		_ = sess.Close()
		b.killCmd(cmd)
	}()
	return sess
}

// volumesFor 按工作目录重新展开卷模板（无占位符的卷原样保留）。
func (b *Bridge) volumesFor(workDir string) []string {
	out := make([]string, 0, len(b.volumesRaw))
	for _, v := range b.volumesRaw {
		out = append(out, plugins.ExpandWorkDir(v, workDir))
	}
	return out
}

// closeExtraSessions 关闭全部异目录会话（Stop 时调用）。
func (b *Bridge) closeExtraSessions() {
	b.mu.Lock()
	extras := b.extraSessions
	b.extraSessions = map[string]*bridgeSession{}
	b.mu.Unlock()
	for _, s := range extras {
		_ = s.session.Close()
		b.killCmd(s.cmd)
	}
}

// callTool 执行一次远端工具调用，结果转 *tool.Result。
func (b *Bridge) callTool(ctx context.Context, name string, args map[string]any) *tool.Result {
	session := b.sessionFor(ctx)
	if session == nil {
		return &tool.Result{Tool: name, Error: "MCP 连接不可用（插件已断开，等待重连）", Category: tool.ResultCategoryExecutionFailed}
	}
	callCtx := ctx
	if b.settings.ExecTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, b.settings.ExecTimeout)
		defer cancel()
	}
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// 连接级错误：远端可能已断开，交给 supervise 重连；本次按执行失败回灌。
		return &tool.Result{Tool: name, Error: fmt.Sprintf("MCP 调用失败: %v", err), Category: tool.ResultCategoryExecutionFailed}
	}
	text := renderContent(res.Content)
	if res.IsError {
		return &tool.Result{Tool: name, Success: false, Error: text, Category: tool.ResultCategoryExecutionFailed}
	}
	// MCP 工具输出是不可信外部源（TODO #18-4 T31）：包进 untrusted 围栏再进上下文，
	// 配合提示词【数据围栏纪律】防提示注入；围栏转义防内容自带闭合标记逃逸。
	out := &tool.Result{Tool: name, Success: true, Output: tool.WrapUntrusted("mcp:"+name, text)}
	if b.settings.ImagePassthrough {
		out.Images = extractImages(res.Content)
	}
	return out
}

// 图片透传上限：单次结果最多 4 张、单张 base64 不超过 4MiB。
// 防失控 MCP server 一次回传超大图集撑爆内存与上下文。
const (
	maxPassthroughImages     = 4
	maxPassthroughImageBytes = 4 << 20
)

// extractImages 从 MCP 结果内容块中提取图片（仅 image_passthrough 开启时调用）。
// 超上限的图片静默跳过（Output 文本占位符仍保留其存在痕迹）。
func extractImages(content []mcp.Content) []tool.ResultImage {
	var images []tool.ResultImage
	for _, c := range content {
		v, ok := c.(*mcp.ImageContent)
		if !ok || v.MIMEType == "" || len(v.Data) == 0 {
			continue
		}
		if len(v.Data) > maxPassthroughImageBytes {
			continue
		}
		images = append(images, tool.ResultImage{MIMEType: v.MIMEType, Data: v.Data})
		if len(images) >= maxPassthroughImages {
			break
		}
	}
	return images
}

// renderContent 把 MCP 结果内容块渲染为文本（text 直取；image/audio 标注不展开 base64）。
func renderContent(content []mcp.Content) string {
	if len(content) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range content {
		if i > 0 {
			sb.WriteByte('\n')
		}
		switch v := c.(type) {
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		case *mcp.ImageContent:
			fmt.Fprintf(&sb, "[image %s, %d bytes base64]", v.MIMEType, len(v.Data))
		case *mcp.AudioContent:
			fmt.Fprintf(&sb, "[audio %s, %d bytes base64]", v.MIMEType, len(v.Data))
		case *mcp.EmbeddedResource:
			if v.Resource != nil {
				fmt.Fprintf(&sb, "[resource %s: %s]", v.Resource.URI, v.Resource.Text)
			} else {
				sb.WriteString("[resource]")
			}
		default:
			// 未知内容块：序列化为 JSON 保留信息。
			if b, err := json.Marshal(c); err == nil {
				sb.Write(b)
			}
		}
	}
	return sb.String()
}

// killCmd 终止并回收 stdio/docker 子进程（http 传输 cmd 为 nil）。幂等。
// docker 传输下 CLI 被杀后容器可能残留（Windows 无信号转发），显式 docker rm -f；
// shared 模式容器是跨实例常驻资源，只杀本会话 exec CLI，不回收容器。
func (b *Bridge) killCmd(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	if b.settings.Transport == "docker" && !b.settings.Shared {
		removeDockerContainer(b.logger, dockerContainerName(b.id))
	}
}

// stderrWriter 把子进程 stderr 转发到结构化日志。
type stderrWriter struct {
	logger *slog.Logger
	id     string
}

func (w *stderrWriter) Write(p []byte) (int, error) {
	w.logger.Warn("mcp 子进程 stderr", "plugin", w.id, "output", string(p))
	return len(p), nil
}
