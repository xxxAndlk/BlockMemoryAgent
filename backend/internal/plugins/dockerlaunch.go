package plugins

// dockerlaunch.go Docker 引擎拉起与降级。
//
// 背景：docker transport 的插件（ui_preview 容器版 / computer_use 沙箱等）依赖本机
// Docker 引擎。引擎没装或没启动时，插件启用会失败，能力直接缺失——而多数情况下
// 只是"Docker Desktop 没开"。
//
// 策略（与 CLAUDE.md 的"降级优先"一致）：
//  1. 启用插件前先探引擎；不可用就**尝试拉起 Docker Desktop**（默认 3 次，
//     每次拉起后等引擎就绪）；仍不可用则该插件降级（能力缺失，不阻断启动）。
//  2. 降级后用户可在应用内点「启动 Docker」重新拉起（POST /api/plugins/docker/start），
//     引擎就绪后自动重载插件，docker 插件随即恢复。
//
// 不做的事：不做后台常驻重试。引擎没起来时每 30 秒重试一次只会刷屏并空耗 CPU
// （实测刷了 26 分钟），改成"用户显式触发"。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DockerLaunchAttempts 一次拉起流程里的尝试次数（用户可感知的上限，别无限试）。
const DockerLaunchAttempts = 3

// dockerEngineWait 单次尝试里等待引擎就绪的时长上限。
// Docker Desktop 冷启动通常 20~60 秒（首次可能更久）。
const dockerEngineWait = 90 * time.Second

// dockerProbeInterval 等待引擎就绪时的轮询间隔。
const dockerProbeInterval = 2 * time.Second

// DockerUnavailableReason 引擎不可用时的标准原因（给用户看，要说清怎么办）。
const DockerUnavailableReason = "Docker 引擎不可用（插件需要 Docker；可点「启动 Docker」自动拉起，或安装并启动 Docker Desktop 后重新启用插件）"

// DockerEngineReady 探测 Docker 引擎（daemon）是否可达。
//
// 用 `docker version --format {{.Server.Version}}`：客户端在、守护进程没起时
// 同样报 "Cannot connect to the Docker daemon"，正是要识别的状态。
func DockerEngineReady(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %s", DockerUnavailableReason, dockerErrTail(out, err))
}

// EnsureDockerEngine 确保 Docker 引擎就绪：不可用时尝试拉起 Docker Desktop，
// 最多 attempts 次（默认 DockerLaunchAttempts）；仍不可用则返回原因。
//
// 每次尝试：探引擎 → 没起就拉起 Docker Desktop → 轮询等引擎就绪。
// 供**用户显式触发**的入口使用（应用内「启动 Docker」按钮），允许慢（每次等 90 秒）。
func EnsureDockerEngine(ctx context.Context, attempts int) error {
	return ensureDockerEngine(ctx, attempts, dockerEngineWait)
}

// EnsureDockerEngineQuick 是插件启用路径用的快速版：同样尝试 3 次，但每次最多等
// 45 秒（Docker Desktop 冷启动 20~60 秒，够用）。插件启用发生在启动预热阶段
// （后台 goroutine），不能为"引擎没开"拖住整个启动；用户显式点「启动 Docker」
// 时走 EnsureDockerEngine（每次等 90 秒，给慢机器留余量）。
func EnsureDockerEngineQuick(ctx context.Context) error {
	return ensureDockerEngine(ctx, DockerLaunchAttempts, 45*time.Second)
}

func ensureDockerEngine(ctx context.Context, attempts int, wait time.Duration) error {
	if attempts <= 0 {
		attempts = DockerLaunchAttempts
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := DockerEngineReady(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if runtime.GOOS != "windows" {
			// 非 Windows 不代管引擎（systemd/launchd 各不相同），只报原因。
			return lastErr
		}
		path, ok := dockerDesktopPath()
		if !ok {
			return fmt.Errorf("%s: 未找到 Docker Desktop（请先安装：https://www.docker.com/products/docker-desktop/）", DockerUnavailableReason)
		}
		if err := startDockerDesktop(path); err != nil {
			lastErr = fmt.Errorf("启动 Docker Desktop 失败: %w", err)
			continue
		}
		if err := waitDockerEngine(ctx, wait); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// dockerDesktopPath 找 Docker Desktop 可执行文件（Windows 常见安装位置）。
func dockerDesktopPath() (string, bool) {
	candidates := []string{
		`C:\Program Files\Docker\Docker\Docker Desktop.exe`,
		`C:\Program Files (x86)\Docker\Docker\Docker Desktop.exe`,
	}
	if local := userLocalAppData(); local != "" {
		candidates = append(candidates, local+`\Docker\Docker Desktop.exe`)
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p, true
		}
	}
	return "", false
}

// startDockerDesktop 拉起 Docker Desktop（不等待它退出）。
//
// 借道 `cmd /c start` 间接启动：Docker Desktop 于是挂在 cmd 之下而不是本进程之下，
// 调用方若用 Wait（会等整棵进程树）就不会被这个常驻 GUI 进程永久阻塞。
func startDockerDesktop(path string) error {
	cmd := exec.Command("cmd", "/c", "start", "", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

// waitDockerEngine 轮询等待引擎就绪。
func waitDockerEngine(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := DockerEngineReady(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dockerProbeInterval):
		}
	}
	return fmt.Errorf("等待 Docker 引擎就绪超时（%s）", dockerErrText(last))
}

// dockerErrTail 从 docker 命令输出里压出一句可读原因（首行）。
func dockerErrTail(out []byte, err error) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		if err != nil {
			return err.Error()
		}
		return "未知错误"
	}
	if i := strings.IndexAny(text, "\r\n"); i > 0 {
		text = text[:i]
	}
	return text
}

// dockerErrText 把错误压成短文案（超时提示用）。
func dockerErrText(err error) string {
	if err == nil {
		return "未知错误"
	}
	text := err.Error()
	if i := strings.Index(text, ": "); i > 0 && len(text) > i+2 {
		// 去掉 "Docker 引擎不可用（…）: " 前缀，只留底层原因。
		return text[i+2:]
	}
	return text
}

// fileExists 判断路径存在且是普通文件。
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// userLocalAppData 返回 %LOCALAPPDATA%（非 Windows 返回空）。
func userLocalAppData() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return os.Getenv("LOCALAPPDATA")
}
