// process.go 实现 kind=process 插件：宿主机长驻进程（无工具注入）。
//
// 与 service 插件（Docker 容器）同一思路、不同载体：service 以 docker run -d
// 起长驻容器，process 以 exec.Cmd 直接在宿主起长驻进程（如 Node 脚本
// login.mjs/bridge.mjs 这类不便容器化的桥接程序）。同样不注入任何工具，
// 只把生命周期（spawn/健康探针/崩溃自动重启/杀进程树）挂上插件状态机，
// 经 /api/plugins/enable|disable|reload 热插拔。
//
// 生命周期要点：
//   - Start：spawn 子进程，stdout/stderr 逐行转发到插件日志（带插件 ID 前缀）；
//     配了 health_url 时阻塞探针至 2xx（ready_timeout 上限），超时杀进程报错；
//   - 监视 goroutine 独占 cmd.Wait：非 Stop 触发的退出在 auto_restart（默认开）
//     下指数退避重启（1s 翻倍至 60s 上限；进程稳定运行 30s 以上后重置退避）；
//   - Stop 幂等：标记停止 → 杀进程树（Windows 用 taskkill /T /F 整树杀，
//     其他平台 Process.Kill 单进程）→ 等监视 goroutine 退出；
//   - 状态语义与 service 对齐：Start 探针通过即 running（运行期崩溃由
//     auto_restart 兜底，Manager 侧不接 degraded 回调，与 service 先例一致）。
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// process 插件默认值。
const (
	// processReadyTimeoutDefault 健康探针默认等待上限。
	processReadyTimeoutDefault = 30 * time.Second
	// processRestartBackoffInit 崩溃重启初始退避。
	processRestartBackoffInit = 1 * time.Second
	// processRestartBackoffMax 崩溃重启退避上限。
	processRestartBackoffMax = 60 * time.Second
	// processStableRunDuration 进程存活超过该时长视为稳定运行，重置退避。
	processStableRunDuration = 30 * time.Second
)

// processSettings 是 plugins.yaml / bma-plugin.json 中 process 插件 settings 段的解析结果。
type processSettings struct {
	// Command 可执行文件（必填，如 "node"）。
	Command string
	// Args 启动参数。
	Args []string
	// Env 追加到进程环境的变量（叠加在宿主环境之上）。
	Env map[string]string
	// Cwd 工作目录；bundle 展开时 ${PLUGIN_DIR} 已替换为插件包绝对路径，
	// 为空则继承 BMA 进程工作目录。
	Cwd string
	// HealthURL 就绪探针（GET 2xx 视为健康）；空则进程起来即视为 running。
	HealthURL string
	// AutoRestart 崩溃自动重启（默认 true）。
	AutoRestart bool
	// ReadyTimeout 健康探针等待上限（默认 30s）。
	ReadyTimeout time.Duration

	// 展示元数据（bundle 展开注入 / yaml 手填），写入 Manifest。
	Name        string
	Version     string
	Description string
}

// processSettingsFromMap 从 settings map 解析配置；缺失字段取默认值。
func processSettingsFromMap(settings map[string]any) processSettings {
	s := processSettings{
		AutoRestart:  true,
		ReadyTimeout: processReadyTimeoutDefault,
	}
	if v, ok := settings["command"].(string); ok {
		s.Command = v
	}
	// args 兼容 []any（yaml/json 反序列化）与 []string（程序化构造）两种形态。
	switch v := settings["args"].(type) {
	case []any:
		for _, a := range v {
			if str, ok := a.(string); ok {
				s.Args = append(s.Args, str)
			}
		}
	case []string:
		s.Args = append(s.Args, v...)
	}
	switch v := settings["env"].(type) {
	case map[string]any:
		s.Env = make(map[string]string, len(v))
		for k, val := range v {
			if str, ok := val.(string); ok {
				s.Env[k] = str
			}
		}
	case map[string]string:
		s.Env = v
	}
	if v, ok := settings["cwd"].(string); ok {
		s.Cwd = v
	}
	if v, ok := settings["health_url"].(string); ok {
		s.HealthURL = v
	}
	if v, ok := settings["auto_restart"].(bool); ok {
		s.AutoRestart = v
	}
	if v, ok := settings["ready_timeout"].(float64); ok && v > 0 {
		s.ReadyTimeout = time.Duration(v * float64(time.Second))
	}
	if v, ok := settings["name"].(string); ok {
		s.Name = v
	}
	if v, ok := settings["version"].(string); ok {
		s.Version = v
	}
	if v, ok := settings["description"].(string); ok {
		s.Description = v
	}
	return s
}

// processPlugin 是宿主机长驻进程插件（实现 Plugin 接口，Tools 恒为空）。
type processPlugin struct {
	id       string
	manifest Manifest
	settings processSettings
	logger   *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd         // 当前子进程（重启时替换）
	outW    *processLogWriter // 当前 stdout 转发器（Wait 后冲刷残行）
	errW    *processLogWriter // 当前 stderr 转发器
	stopCh  chan struct{}     // Stop 信号（关闭即停止，打断退避等待）
	doneCh  chan struct{}     // 监视 goroutine 退出信号
	running bool              // 是否处于 Start~Stop 生命周期内
	starts  int               // 累计 spawn 次数（测试观察重启用）
}

// newProcessPlugin 按 settings 构造 process 插件实例。
func newProcessPlugin(id string, settings map[string]any, logger *slog.Logger) Plugin {
	if logger == nil {
		logger = slog.Default()
	}
	s := processSettingsFromMap(settings)
	name := s.Name
	if name == "" {
		name = id
	}
	desc := s.Description
	if desc == "" {
		desc = "宿主进程插件 " + id
	}
	return &processPlugin{
		id: id,
		manifest: Manifest{
			ID:          id,
			Name:        name,
			Version:     s.Version,
			Description: desc,
			Kind:        KindProcess,
		},
		settings: s,
		logger:   logger,
	}
}

// Manifest 实现 Plugin。
func (p *processPlugin) Manifest() Manifest { return p.manifest }

// Init 实现 Plugin：校验配置（command 必填）。
func (p *processPlugin) Init(_ context.Context, _ Deps) error {
	if strings.TrimSpace(p.settings.Command) == "" {
		return fmt.Errorf("process 插件 %q: 需要 settings.command", p.id)
	}
	return nil
}

// Start 实现 Plugin：spawn 子进程并起监视 goroutine；
// 配了 health_url 时阻塞至探针就绪（失败杀进程报错，Enable 回滚）。
func (p *processPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil // 幂等（Manager 已挡重复 Enable，此处防御）
	}
	p.running = true
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	if err := p.spawn(); err != nil {
		p.mu.Lock()
		p.running = false
		close(p.doneCh)
		p.mu.Unlock()
		return err
	}
	go p.monitor()
	if p.settings.HealthURL != "" {
		if err := p.waitHealthy(ctx); err != nil {
			// 探针超时：杀进程并禁止重启（对齐 service 失败回收语义）。
			_ = p.Stop(context.Background())
			return err
		}
	}
	return nil
}

// spawn 拉起一次子进程（Start 与崩溃重启共用）。
func (p *processPlugin) spawn() error {
	cmd := exec.Command(p.settings.Command, p.settings.Args...)
	if p.settings.Cwd != "" {
		cmd.Dir = p.settings.Cwd
	}
	// 叠加宿主环境（settings.env 同名键后置覆盖）。
	env := os.Environ()
	keys := make([]string, 0, len(p.settings.Env))
	for k := range p.settings.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 排序保证环境构造可重现（便于测试与排障）
	for _, k := range keys {
		env = append(env, k+"="+p.settings.Env[k])
	}
	cmd.Env = env
	outW := &processLogWriter{logger: p.logger, id: p.id}
	errW := &processLogWriter{logger: p.logger, id: p.id, stderr: true}
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process 插件 %q 启动失败: %w", p.id, err)
	}
	p.mu.Lock()
	p.cmd = cmd
	p.outW = outW
	p.errW = errW
	p.starts++
	p.mu.Unlock()
	p.logger.Info("process 进程已启动", "plugin", p.id, "pid", cmd.Process.Pid,
		"command", p.settings.Command, "args", p.settings.Args)
	return nil
}

// monitor 独占 cmd.Wait：非 Stop 触发的退出按 auto_restart 指数退避重启。
// 退出时关闭 doneCh（Stop 据此等待收尾）。
func (p *processPlugin) monitor() {
	defer func() {
		p.mu.Lock()
		done := p.doneCh
		p.mu.Unlock()
		close(done)
	}()
	backoff := processRestartBackoffInit
	for {
		p.mu.Lock()
		cmd := p.cmd
		outW, errW := p.outW, p.errW
		p.mu.Unlock()
		if cmd == nil {
			return
		}
		startAt := time.Now()
		waitErr := cmd.Wait()
		// 冲刷日志残行（进程未以换行结尾的输出）。
		outW.flush()
		errW.flush()

		p.mu.Lock()
		running := p.running
		starts := p.starts
		p.mu.Unlock()
		if !running {
			return // Stop 触发的退出，不重启
		}
		if !p.settings.AutoRestart {
			p.logger.Warn("process 进程已退出（auto_restart 关闭，不重启）",
				"plugin", p.id, "err", waitErr)
			return
		}
		// 稳定运行一段时间后崩溃视为偶发，退避从头计。
		if time.Since(startAt) > processStableRunDuration {
			backoff = processRestartBackoffInit
		}
		p.logger.Warn("process 进程意外退出，退避后重启",
			"plugin", p.id, "err", waitErr, "backoff", backoff, "starts", starts)
		select {
		case <-p.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < processRestartBackoffMax {
			backoff *= 2
			if backoff > processRestartBackoffMax {
				backoff = processRestartBackoffMax
			}
		}
		p.mu.Lock()
		stillRunning := p.running
		p.mu.Unlock()
		if !stillRunning {
			return
		}
		if err := p.spawn(); err != nil {
			p.logger.Warn("process 进程重启失败，继续退避", "plugin", p.id, "err", err)
		}
	}
}

// waitHealthy 轮询健康探针直到 2xx 或超时/ctx 取消（与 service 探针同款）。
func (p *processPlugin) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(p.settings.ReadyTimeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.settings.HealthURL, nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					p.logger.Info("process 健康探针就绪", "plugin", p.id, "url", p.settings.HealthURL)
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process 插件 %q 健康探针超时（%s 未就绪，%s）", p.id, p.settings.HealthURL, p.settings.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop 实现 Plugin：标记停止 → 杀进程树 → 等监视 goroutine 收尾。幂等。
func (p *processPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return nil // 幂等
	}
	p.running = false
	close(p.stopCh)
	cmd := p.cmd
	done := p.doneCh
	p.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		killProcessTree(p.logger, p.id, cmd)
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// killProcessTree 杀进程及其整棵子进程树：
// Windows 用 taskkill /T /F（node 起的子进程一并收掉）；其他平台 Process.Kill
// （未设进程组隔离，保持跨平台编译简单，树杀仅 Windows 目标平台保证）。
func killProcessTree(logger *slog.Logger, pluginID string, cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	if runtime.GOOS == "windows" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
		if err != nil {
			// 进程已退出是常态（崩溃后 Stop），不算错误。
			logger.Debug("taskkill", "plugin", pluginID, "pid", pid, "output", strings.TrimSpace(string(out)))
		}
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		logger.Debug("process kill", "plugin", pluginID, "pid", pid, "err", err)
	}
}

// Tools 实现 Plugin：process 插件不注入工具。
func (p *processPlugin) Tools() []tool.Tool { return nil }

// processLogWriter 把子进程 stdout/stderr 按行转发到插件日志（带插件 ID 前缀）。
// Write 线程安全由 exec 保证（每个流单 goroutine 写入）；残行由 flush 冲刷。
type processLogWriter struct {
	logger *slog.Logger
	id     string
	stderr bool
	buf    []byte
}

// Write 按换行切分，逐行打日志；不足一行的残片留在缓冲区。
func (w *processLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.logLine(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush 冲刷缓冲区残行（进程退出后调用）。
func (w *processLogWriter) flush() {
	if len(w.buf) > 0 {
		w.logLine(string(w.buf))
		w.buf = nil
	}
}

// logLine 输出一行子进程日志（stderr 走 Warn，stdout 走 Info）。
func (w *processLogWriter) logLine(line string) {
	line = strings.TrimRight(line, "\r")
	if w.stderr {
		w.logger.Warn("process 输出", "plugin", w.id, "stream", "stderr", "line", line)
	} else {
		w.logger.Info("process 输出", "plugin", w.id, "stream", "stdout", "line", line)
	}
}
