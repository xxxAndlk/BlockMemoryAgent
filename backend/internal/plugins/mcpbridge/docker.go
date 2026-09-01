// docker.go 实现 docker 传输的两种容器形态：
//   - legacy（shared=false）：`docker run -i --rm`，容器生命周期 = 会话生命周期，
//     停止时显式 `docker rm -f`（Windows 下 kill CLI 不转发信号）；
//   - shared（shared=true）：`docker run -d` 常驻 keeper 容器（容器内无 MCP 业务
//     进程），每个会话经 `docker exec -i` 在容器内起独立 MCP server 进程做 stdio
//     桥接。多个 BMA 实例（多开 TUI）复用同一容器；Stop 只断本会话不回收容器。
//
// 生命周期要点（两种形态共用）：
//   - legacy 容器名带进程号后缀、shared 容器名带 workdir 哈希后缀，见各自函数注释；
//   - 启动前 best-effort `docker rm -f` 同名容器，防上次崩溃残留导致 name 冲突；
//   - 只经 -e KEY=VALUE 显式传递 settings.env，不继承宿主全部环境。
package mcpbridge

import (
	"context"
	"crypto/sha256"
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

// dockerSharedContainerName 共享容器名：插件 id + workdir sha256 前 8 位十六进制。
// 同目录多开实例 → 同名容器 → 复用；不同目录实例 → 不同容器，${WORKDIR} 卷映射
// 互不干扰（保留原 pid 后缀方案的防互杀语义：不同名容器各实例只管理自己的）。
func dockerSharedContainerName(pluginID, workDir string) string {
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
	sum := sha256.Sum256([]byte(workDir))
	fmt.Fprintf(&sb, "-%x", sum[:4])
	return sb.String()
}

// dockerRunArgsShared 构造 shared 模式 `docker run -d` 参数（不含 "docker" 本身）：
// 常驻 keeper 容器（--entrypoint 覆盖，缺省 sleep infinity，容器内无常驻业务进程，
// MCP server 全部由 exec 按会话拉起），无 --rm/-i，settings.Args 不追加
//（那是 exec 会话的 MCP server 参数，见 dockerExecArgs）。
func dockerRunArgsShared(name string, s Settings) []string {
	args := []string{"run", "-d", "--name", name}
	args = append(args, "--add-host", "host.docker.internal:host-gateway")
	for _, p := range s.Ports {
		args = append(args, "-p", p)
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", v)
	}
	keys := make([]string, 0, len(s.Env)+len(s.ContainerEnv))
	for k := range s.Env {
		keys = append(keys, k)
	}
	for k := range s.ContainerEnv {
		if _, ok := s.Env[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := s.Env[k]; ok {
			args = append(args, "-e", k+"="+v)
			continue
		}
		args = append(args, "-e", k+"="+s.ContainerEnv[k])
	}
	entrypoint := s.ContainerEntrypoint
	if len(entrypoint) == 0 {
		entrypoint = []string{"sleep", "infinity"}
	}
	// docker --entrypoint 只接受单值：首字段作 entrypoint，其余作镜像后参数。
	args = append(args, "--entrypoint", entrypoint[0])
	args = append(args, s.Image)
	args = append(args, entrypoint[1:]...)
	return args
}

// dockerExecArgs 构造 shared 模式会话的 `docker exec` 参数（不含 "docker" 本身）：
// 每个客户端会话在容器内起独立 MCP server 进程；settings.Args 附加在
// exec_command 之后（对齐 docker run 时 CMD 追加在 ENTRYPOINT 后的语义）。
func dockerExecArgs(container string, s Settings) []string {
	argv := append([]string{"exec", "-i", container}, s.ExecCommand...)
	return append(argv, s.Args...)
}

// dockerInspectRunning 查询容器运行状态与镜像；容器不存在/查询失败返回 (false, "")。
func dockerInspectRunning(logger *slog.Logger, name string) (running bool, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		"{{.State.Running}} {{.Config.Image}}", name).Output()
	if err != nil {
		// 容器不存在是常态（首次启动），不算错误。
		logger.Debug("docker inspect", "container", name, "err", err)
		return false, ""
	}
	return parseDockerInspectRunning(string(out))
}

// parseDockerInspectRunning 解析 `docker inspect -f '{{.State.Running}} {{.Config.Image}}'` 输出。
func parseDockerInspectRunning(out string) (bool, string) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return false, ""
	}
	return fields[0] == "true", fields[1]
}

// ensureSharedContainer 确保共享容器在运行且镜像匹配。
// 容器不在/已停止 → rm 残留后 `docker run -d` 重建；镜像不匹配（配置改了）→ 重建。
// 并发竞态：另一实例同时重建撞名失败时重新 inspect，running 且镜像一致即放行。
func (b *Bridge) ensureSharedContainer(ctx context.Context) error {
	running, image := dockerInspectRunning(b.logger, b.containerName)
	if running && image == b.settings.Image {
		return nil
	}
	if running {
		b.logger.Warn("共享容器镜像不匹配，重建", "plugin", b.id, "container", b.containerName,
			"running_image", image, "want_image", b.settings.Image)
	}
	removeDockerContainer(b.logger, b.containerName)
	args := dockerRunArgsShared(b.containerName, b.settings)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		if r, img := dockerInspectRunning(b.logger, b.containerName); r && img == b.settings.Image {
			b.logger.Info("共享容器已被并发实例创建，复用", "plugin", b.id, "container", b.containerName)
			return nil
		}
		return fmt.Errorf("mcp docker run -d %q: %w (%s)", b.settings.Image, err, strings.TrimSpace(string(out)))
	}
	b.logger.Info("共享容器已启动", "plugin", b.id, "container", b.containerName)
	return nil
}

// newSharedDockerTransport 确保共享 keeper 容器在运行，然后 `docker exec -i` 在
// 容器内起本会话独立的 MCP server 进程并返回 stdio 桥接传输。
// exec CLI 退出只结束本会话，容器（其他实例的会话）不受影响。
func (b *Bridge) newSharedDockerTransport(ctx context.Context) (mcp.Transport, *exec.Cmd, error) {
	if err := b.ensureSharedContainer(ctx); err != nil {
		return nil, nil, err
	}
	args := dockerExecArgs(b.containerName, b.settings)
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp docker exec stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp docker exec stdout: %w", err)
	}
	cmd.Stderr = &stderrWriter{logger: b.logger, id: b.id}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("mcp docker exec %q: %w", strings.Join(b.settings.ExecCommand, " "), err)
	}
	return &mcp.IOTransport{Reader: stdout, Writer: stdin}, cmd, nil
}

// newDockerTransport 启动 docker 容器并返回 stdio 桥接传输：
// shared 模式走常驻 keeper 容器 + exec 会话（见 newSharedDockerTransport）；
// legacy 模式 `docker run -i --rm` 容器即会话。
func (b *Bridge) newDockerTransport(ctx context.Context) (mcp.Transport, *exec.Cmd, error) {
	if b.settings.Shared {
		return b.newSharedDockerTransport(ctx)
	}
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
