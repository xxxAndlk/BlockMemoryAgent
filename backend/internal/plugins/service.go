// service.go 实现 kind=service 插件：Docker 化的长驻 HTTP 服务（无 MCP 工具）。
//
// 与 mcp 插件的差异：mcp 插件以 `docker run -i` 起容器做 stdio 桥接、工具经
// MCP ListTools 动态注册；service 插件以 `docker run -d` 起长驻容器，不注入
// 任何工具，只把生命周期（启动/健康探针/回收）挂上插件状态机，因此同样经
// /api/plugins/enable|disable|reload 热插拔。典型用途：Open Design UI 设计台
// 这类自带 Web UI 的服务。
//
// 生命周期要点（与 mcpbridge/docker.go 同一套约定）：
//   - 启动前 best-effort `docker rm -f` 同名容器，防上次崩溃残留导致 name 冲突；
//   - 容器名带进程号后缀（bma-plugin-svc-<id>-<pid>），避免同机多个 BMA 实例
//     （tui.exe 与 headless 后端）互相清场；
//   - 停止即显式 `docker rm -f`，幂等；
//   - 只经 -e KEY=VALUE 显式传递 settings.env，不继承宿主全部环境。
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// serviceStartTimeoutDefault 健康探针默认等待上限。
const serviceStartTimeoutDefault = 60 * time.Second

// serviceSettings 是 plugins.yaml 中 service 插件 settings 段的解析结果。
type serviceSettings struct {
	// Image 容器镜像（必填，docker run -d <image>）。
	Image string
	// Args 追加在镜像后的容器启动参数。
	Args []string
	// Env 显式传入容器的环境变量。
	Env map[string]string
	// Ports 端口映射（如 "127.0.0.1:7456:7456"）。
	Ports []string
	// Volumes 卷映射（如 "bma-open-design-data:/app/.od"）。
	Volumes []string
	// URL 展示用访问地址（写入 Manifest，供前端/TUI 展示入口）。
	URL string
	// HealthURL 就绪探针（宿主侧地址）；空则容器起来即视为 running。
	HealthURL string
	// RequiresEnv 依赖的环境变量名（透传 Manifest，缺失时 Enable 拒绝）。
	RequiresEnv []string
	// StartTimeout 健康探针等待上限（默认 60s）。
	StartTimeout time.Duration
}

// serviceSettingsFromMap 从 settings map 解析配置；缺失字段取默认值。
func serviceSettingsFromMap(settings map[string]any) serviceSettings {
	s := serviceSettings{StartTimeout: serviceStartTimeoutDefault}
	if v, ok := settings["image"].(string); ok {
		s.Image = v
	}
	if v, ok := settings["url"].(string); ok {
		s.URL = v
	}
	if v, ok := settings["health_url"].(string); ok {
		s.HealthURL = v
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
	if v, ok := settings["ports"].([]any); ok {
		for _, p := range v {
			if str, ok := p.(string); ok {
				s.Ports = append(s.Ports, str)
			}
		}
	}
	if v, ok := settings["volumes"].([]any); ok {
		for _, p := range v {
			if str, ok := p.(string); ok {
				s.Volumes = append(s.Volumes, str)
			}
		}
	}
	if v, ok := settings["requires_env"].([]any); ok {
		for _, e := range v {
			if str, ok := e.(string); ok {
				s.RequiresEnv = append(s.RequiresEnv, str)
			}
		}
	}
	if v, ok := settings["start_timeout_sec"].(float64); ok && v > 0 {
		s.StartTimeout = time.Duration(v * float64(time.Second))
	}
	return s
}

// serviceContainerName 由插件 id 派生容器名（净化规则与 mcpbridge 一致），
// 追加进程号后缀防多实例互杀（见 mcpbridge/docker.go dockerContainerName 注释）。
func serviceContainerName(pluginID string) string {
	var sb strings.Builder
	sb.WriteString("bma-plugin-svc-")
	for _, r := range pluginID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	fmt.Fprintf(&sb, "-%d", os.Getpid())
	return sb.String()
}

// serviceRunArgs 构造 `docker run -d` 参数（不含 "docker" 本身）；纯函数便于测试。
func serviceRunArgs(name string, s serviceSettings) []string {
	args := []string{"run", "-d", "--name", name}
	// 容器内访问宿主服务（与其他插件容器一致的约定）。
	args = append(args, "--add-host", "host.docker.internal:host-gateway")
	for _, p := range s.Ports {
		args = append(args, "-p", p)
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", v)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	// map 迭代顺序不稳定，排序保证 args 可重现（便于测试与排障）。
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	args = append(args, s.Image)
	args = append(args, s.Args...)
	return args
}

// servicePlugin 是 Docker 长驻 HTTP 服务插件（实现 Plugin 接口，Tools 恒为空）。
type servicePlugin struct {
	id        string
	manifest  Manifest
	settings  serviceSettings
	logger    *slog.Logger
	container string
}

// newServicePlugin 按 settings 构造 service 插件实例。
func newServicePlugin(id string, settings map[string]any, logger *slog.Logger) Plugin {
	if logger == nil {
		logger = slog.Default()
	}
	s := serviceSettingsFromMap(settings)
	return &servicePlugin{
		id: id,
		manifest: Manifest{
			ID:          id,
			Name:        id,
			Kind:        KindService,
			Description: "Docker 服务插件 " + id,
			URL:         s.URL,
			RequiresEnv: s.RequiresEnv,
		},
		settings:  s,
		logger:    logger,
		container: serviceContainerName(id),
	}
}

// Manifest 实现 Plugin。
func (p *servicePlugin) Manifest() Manifest { return p.manifest }

// Init 实现 Plugin：校验配置（image 必填）。
func (p *servicePlugin) Init(_ context.Context, _ Deps) error {
	if p.settings.Image == "" {
		return fmt.Errorf("service 插件 %q: 需要 settings.image", p.id)
	}
	return nil
}

// Start 实现 Plugin：拉起长驻容器并等待健康探针就绪。
// 失败时回收已起容器并返回错误（Enable 回滚）。
func (p *servicePlugin) Start(ctx context.Context) error {
	// 上次异常退出可能残留同名容器（进程被强杀时 pid 后缀会变，但同进程
	// enable-after-disable / 重连路径仍可能撞上），先清场。
	p.removeContainer()
	args := serviceRunArgs(p.container, p.settings)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("service 插件 %q docker run 失败: %w (%s)", p.id, err, strings.TrimSpace(string(out)))
	}
	p.logger.Info("service 容器已启动", "plugin", p.id, "container", p.container)
	if p.settings.HealthURL == "" {
		return nil
	}
	if err := p.waitHealthy(ctx); err != nil {
		p.removeContainer()
		return err
	}
	return nil
}

// waitHealthy 轮询健康探针直到 2xx 或超时/ctx 取消。
func (p *servicePlugin) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(p.settings.StartTimeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.settings.HealthURL, nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					p.logger.Info("service 健康探针就绪", "plugin", p.id, "url", p.settings.HealthURL)
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service 插件 %q 健康探针超时（%s 未就绪，%s）", p.id, p.settings.HealthURL, p.settings.StartTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop 实现 Plugin：显式回收容器。幂等。
func (p *servicePlugin) Stop(_ context.Context) error {
	p.removeContainer()
	return nil
}

// removeContainer best-effort 删除容器（不存在/失败均静默，仅记 debug 日志）。
func (p *servicePlugin) removeContainer() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", p.container).CombinedOutput(); err != nil {
		// 容器不存在是常态（首次启动/已回收），不算错误。
		p.logger.Debug("docker rm -f", "container", p.container, "output", strings.TrimSpace(string(out)))
	}
}

// Tools 实现 Plugin：service 插件不注入工具。
func (p *servicePlugin) Tools() []tool.Tool { return nil }
