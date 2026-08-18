// docker transport：以 `docker run -i --rm` 容器方式运行 MCP server，
// stdin/stdout 管道桥接（等价于 stdio，只是子进程是 docker CLI）。
//
// 生命周期要点：
//   - 启动前 best-effort `docker rm -f` 同名容器，防上次崩溃残留导致 name 冲突；
//   - 停止时除 kill CLI 外必须显式 `docker rm -f`（Windows 下 kill 进程不转发信号，
//     容器会脱离 CLI 继续运行）；
//   - 只经 -e KEY=VALUE 显式传递 settings.env，不继承宿主全部环境。
package mcpbridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dockerContainerName 由插件 id 派生容器名（docker 仅允许 [a-zA-Z0-9_.-]，
// bundle 插件 id 形如 bundle/<dir>/<server> 含 /，需净化）。
// 追加进程号后缀：tui.exe 与 headless 后端是同机两个独立 BMA 实例（各自经
// bootstrap 装配自己的插件管理器），固定容器名会导致两边重启清场时互相
// docker rm -f 对方的容器（实测表现为秒级重连 churn）。带后缀后各实例只
// 管理自己的容器；代价是进程被强杀时会残留带旧 pid 的孤儿容器，需手动清理
//（docker ps -a --filter name=bma-plugin-）。
func dockerContainerName(pluginID string) string {
	var sb strings.Builder
	sb.WriteString("bma-plugin-")
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

// dockerRunArgs 构造 `docker run` 参数（不含 "docker" 本身）。
func dockerRunArgs(pluginID string, s Settings) (name string, args []string) {
	name = dockerContainerName(pluginID)
	args = []string{"run", "-i", "--rm", "--name", name}
	// 容器内访问宿主服务（如自托管 firecrawl 栈发布在 localhost:3002）。
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
	return name, args
}

// newDockerTransport 启动 docker 容器并返回 stdio 桥接传输。
func (b *Bridge) newDockerTransport(ctx context.Context) (mcp.Transport, *exec.Cmd, error) {
	name, args := dockerRunArgs(b.id, b.settings)
	// 上次异常退出可能残留同名容器（--rm 在 CLI 被 kill 时不生效），先清场。
	removeDockerContainer(b.logger, name)

	cmd := exec.CommandContext(ctx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp docker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp docker stdout: %w", err)
	}
	cmd.Stderr = &stderrWriter{logger: b.logger, id: b.id}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("mcp docker run %q: %w", b.settings.Image, err)
	}
	return &mcp.IOTransport{Reader: stdout, Writer: stdin}, cmd, nil
}

// removeDockerContainer best-effort 删除容器（不存在/失败均静默，仅记 debug 日志）。
func removeDockerContainer(logger *slog.Logger, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
		// 容器不存在是常态（首次启动/已被 --rm 回收），不算错误。
		logger.Debug("docker rm -f", "container", name, "output", strings.TrimSpace(string(out)))
	}
}
