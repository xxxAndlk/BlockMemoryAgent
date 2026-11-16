//go:build windows

// 依赖自举：让桌面版做到「双击即用」。
//
// 背景：后端是严格启动——PostgreSQL/Redis 连不上直接退出（backend/main.go 装配失败即
// os.Exit(1)），而 pgvector 扩展只能跑在容器里，于是基础设施住在 Docker 中。
// 旧版打包把整个 docker/ 目录排除在包外，外壳只管拉起后端：用户必须先手工开
// Docker Desktop、手工 compose up，桌面版才有可能起得来——那不叫打包。
//
// 本文件负责把这条链路补齐：探测依赖 → 必要时拉起 Docker Desktop → compose up
// postgres/redis → 等就绪；失败一律给出「可读原因 + 可执行动作」，不出现
// “90 秒未就绪”这种无信息量的提示。
//
// 依赖实现只此一份（Go）：脚本启动器 tools/app/launch.ps1 通过
// `BMA-Desktop.exe --ensure-deps` 复用，避免 PowerShell/Go 双实现漂移。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"        // PostgreSQL 驱动（driver 名 "postgres"）
	"github.com/redis/go-redis/v9" // Redis 客户端

	"github.com/blockmemory/agent/backend/internal/config" // 基础设施配置解析（与后端同源）
)

const (
	// composeFileName 包内编排文件名（与仓库 docker/docker-compose.yml 同名同内容）。
	composeFileName = "docker-compose.yml"
	// dockerDesktopURL 未安装 Docker Desktop 时的引导地址。
	dockerDesktopURL = "https://www.docker.com/products/docker-desktop/"
	// defaultPostgresDSN 与包内 compose 的凭据一致（.env.example 同值）。
	defaultPostgresDSN = "postgres://blockmemory:blockmemory_dev@localhost:5432/blockmemory?sslmode=disable"
	defaultRedisAddr   = "localhost:6380"
)

// DepsReport 依赖自举结果。写 app\deps.json 供脚本启动器与外部调用方读取。
type DepsReport struct {
	OK      bool     `json:"ok"`
	Infra   string   `json:"infra"`
	Media   string   `json:"media"`
	Steps   []string `json:"steps"`
	Error   string   `json:"error"`
	Hint    string   `json:"hint"`
	LogPath string   `json:"log_path"`
}

// depTargets 依赖目标地址（来自安装目录的 .env）。
type depTargets struct {
	pgDSN      string
	pgHost     string
	pgPort     string
	redisAddr  string
	redisPass  string
	pgLocal    bool
	redisLocal bool
}

// resolveDepTargets 解析后端将要使用的依赖地址。
//
// 走与后端同一套解析链：先加载 .env（.env 优先级高于 shell 环境变量），再由
// config.yaml 的 ${VAR:默认} 插值算出最终 DSN / Redis 地址。这样"探测用的地址"
// 与"后端实际用的地址"必然一致——不会出现探测通过、后端仍连不上的错位。
// config.yaml 读不到时退化为直接读 .env。
func resolveDepTargets(root string) depTargets {
	envPath := filepath.Join(root, ".env")
	if _, err := os.Stat(envPath); err == nil {
		_ = config.LoadEnvFile(envPath)
	}

	t := depTargets{}
	if cfg, err := config.Load(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.pgDSN = cfg.Postgres.DSN
		t.redisAddr = cfg.Redis.Addr
		t.redisPass = cfg.Redis.Password
	}
	if t.pgDSN == "" {
		t.pgDSN = readEnvValue(envPath, "POSTGRES_DSN")
	}
	if t.redisAddr == "" {
		t.redisAddr = readEnvValue(envPath, "REDIS_ADDR")
	}
	if t.redisPass == "" {
		t.redisPass = readEnvValue(envPath, "REDIS_PASSWORD")
	}
	if t.pgDSN == "" {
		t.pgDSN = defaultPostgresDSN
	}
	if t.redisAddr == "" {
		t.redisAddr = defaultRedisAddr
	}
	// PG：postgres://user:pass@host:port/db?sslmode=...
	if u, err := url.Parse(t.pgDSN); err == nil && u.Host != "" {
		t.pgHost = u.Hostname()
		t.pgPort = u.Port()
	}
	if t.pgHost == "" {
		t.pgHost = "localhost"
	}
	if t.pgPort == "" {
		t.pgPort = "5432"
	}
	// Redis：host:port
	rh, rp, err := net.SplitHostPort(t.redisAddr)
	if err != nil {
		rh, rp = t.redisAddr, "6379"
	}
	if rh == "" {
		rh = "localhost"
	}
	t.redisAddr = net.JoinHostPort(rh, rp)
	t.pgLocal = isLocalHost(t.pgHost)
	t.redisLocal = isLocalHost(rh)
	return t
}

// isLocalHost 判断目标是否在本机（决定要不要拉本地 docker 起依赖）。
func isLocalHost(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// probePostgres 用真实驱动 ping：能区分「库没起」与「库起着但账号/库名不对」，
// 后者不该再去起 docker。
func probePostgres(dsn string, timeout time.Duration) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return db.PingContext(ctx)
}

// probeRedis 真实 ping（带口令）：TCP 通但认证失败同样算未就绪。
func probeRedis(addr, password string, timeout time.Duration) error {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: password})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Ping(ctx).Err()
}

// ensureDeps 把基础设施拉起来。返回 nil 表示已就绪（可直接起后端）。
//
// onStep 用于把过程回传给进度窗（可为 nil）。
func ensureDeps(root string, onStep func(string)) (*DepsReport, error) {
	rep := &DepsReport{Steps: []string{}}
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		rep.Steps = append(rep.Steps, msg)
		if onStep != nil {
			onStep(msg)
		}
	}

	targets := resolveDepTargets(root)

	pgErr := probePostgres(targets.pgDSN, 3*time.Second)
	redisErr := probeRedis(targets.redisAddr, targets.redisPass, 3*time.Second)
	if pgErr == nil && redisErr == nil {
		rep.OK = true
		rep.Infra = "PostgreSQL 与 Redis 已就绪"
		return rep, nil
	}
	step("依赖未就绪：PostgreSQL %s；Redis %s", errText(pgErr), errText(redisErr))

	// 远程托管的依赖不在本机起容器，直接报真实原因。
	if !targets.pgLocal || !targets.redisLocal {
		rep.Error = fmt.Sprintf("远程依赖不可用：PostgreSQL %s；Redis %s", errText(pgErr), errText(redisErr))
		rep.Hint = "检查 .env 的 POSTGRES_DSN / REDIS_ADDR 是否指向正确的实例"
		return rep, errors.New(rep.Error)
	}

	composeFile := filepath.Join(root, "docker", composeFileName)
	if !fileExists(composeFile) {
		rep.Error = "安装目录缺少 docker\\" + composeFileName + "（安装包不完整）"
		rep.Hint = "重新运行 bma-app.ps1 install -Target <安装目录> 补齐程序载荷"
		return rep, errors.New(rep.Error)
	}

	// 1) docker 引擎：不可用则拉起 Docker Desktop 并等它就绪。
	if err := dockerEngineReady(10 * time.Second); err != nil {
		step("Docker 引擎未就绪，正在启动 Docker Desktop ...")
		path, ok := startDockerDesktop()
		if !ok {
			rep.Error = "未找到 Docker Desktop（桌面版用它提供 PostgreSQL/pgvector 与 Redis）"
			rep.Hint = "请先安装 Docker Desktop：" + dockerDesktopURL
			return rep, errors.New(rep.Error)
		}
		step("已启动 %s，等待引擎就绪 ...", path)
		if err := waitDockerEngine(3 * time.Minute); err != nil {
			rep.Error = "Docker 引擎启动超时：" + err.Error()
			rep.Hint = "手动打开 Docker Desktop 等到就绪后重试；首次启动可能需要接受许可协议"
			return rep, errors.New(rep.Error)
		}
	}
	step("Docker 引擎就绪")

	// 2) 拉起容器（首次运行要拉镜像，耐心等）。
	step("启动 PostgreSQL / Redis 容器（首次运行需拉取镜像，请耐心等待）...")
	out, err := composeUp(composeFile, 15*time.Minute, "postgres", "redis")
	if err != nil {
		rep.Error = "启动 PostgreSQL / Redis 失败：" + tailLines(out, 3)
		rep.Hint = "检查 Docker 是否正常运行、端口 5432 / 6380 是否被其他程序占用"
		return rep, errors.New(rep.Error)
	}
	step("容器已拉起，等待数据库就绪 ...")

	if err := waitDepsReady(targets, 2*time.Minute); err != nil {
		rep.Error = "PostgreSQL / Redis 未在超时内就绪：" + err.Error()
		rep.Hint = "查看 docker ps 与容器日志（docker logs blockmemory-postgres）"
		return rep, errors.New(rep.Error)
	}

	rep.OK = true
	rep.Infra = "PostgreSQL 与 Redis 已就绪"
	step("依赖就绪")
	return rep, nil
}

// waitDepsReady 轮询直到两个依赖都可用。
func waitDepsReady(t depTargets, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		pgErr := probePostgres(t.pgDSN, 3*time.Second)
		redisErr := probeRedis(t.redisAddr, t.redisPass, 3*time.Second)
		if pgErr == nil && redisErr == nil {
			return nil
		}
		last = fmt.Errorf("PostgreSQL %s；Redis %s", errText(pgErr), errText(redisErr))
		time.Sleep(2 * time.Second)
	}
	if last == nil {
		last = errors.New("超时")
	}
	return last
}

// dockerEngineReady 检查 docker 引擎（daemon）是否可用。
func dockerEngineReady(timeout time.Duration) error {
	out, err := runDocker(timeout, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("%v（%s）", err, tailLines(out, 2))
	}
	return nil
}

// waitDockerEngine 轮询等待引擎就绪（Docker Desktop 冷启动要几十秒）。
func waitDockerEngine(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := dockerEngineReady(5 * time.Second); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(3 * time.Second)
	}
	if last == nil {
		last = errors.New("超时")
	}
	return last
}

// dockerDesktopCandidates Docker Desktop 可执行文件的常见安装位置。
func dockerDesktopCandidates() []string {
	paths := []string{
		`C:\Program Files\Docker\Docker\Docker Desktop.exe`,
		`C:\Program Files (x86)\Docker\Docker\Docker Desktop.exe`,
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		paths = append(paths, filepath.Join(local, "Docker", "Docker Desktop.exe"))
	}
	return paths
}

// startDockerDesktop 拉起 Docker Desktop（不等待它退出）。
//
// 借道 `cmd /c start` 间接启动：Docker Desktop 于是挂在 cmd 之下而不是本进程之下——
// 调用方若用 Start-Process -Wait（会等整棵进程树）就不会被这个常驻 GUI 进程永久阻塞。
func startDockerDesktop() (string, bool) {
	for _, p := range dockerDesktopCandidates() {
		if !fileExists(p) {
			continue
		}
		cmd := exec.Command("cmd", "/c", "start", "", p)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
		if err := cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return p, true
		}
	}
	return "", false
}

// composeUp 调起 compose：优先 v2 插件（docker compose），失败回退 v1（docker-compose）。
func composeUp(composeFile string, timeout time.Duration, services ...string) (string, error) {
	tail := append([]string{"up", "-d"}, services...)
	if out, err := runDocker(timeout, append([]string{"compose", "-f", composeFile}, tail...)...); err == nil {
		return out, nil
	}
	return runDockerBinary(timeout, "docker-compose", append([]string{"-f", composeFile}, tail...)...)
}

// ensureMedia 拉起媒体栈：open_design daemon + minimax-media 桥 + 两个插件镜像。
//
// 纯 best-effort：媒体镜像体积大、构建慢（Playwright 镜像），放不进启动关键路径；
// 失败只落日志 + 返回摘要，不阻断主流程（与插件系统的降级语义一致）。
//
// 返回值区分"无需变更"与"已更新"——后者才需要通知后端换 keeper（重载会掐断在途会话）。
func ensureMedia(root string, out io.Writer) string {
	composeFile := filepath.Join(root, "docker", composeFileName)
	if !fileExists(composeFile) {
		return "未安装媒体栈（缺少 docker\\" + composeFileName + "）"
	}

	// 插件镜像缺失时构建（后台较慢：open-design-mcp 需 npm install，ui-preview 拉 Playwright 镜像）。
	images := []struct{ tag, ctx string }{
		{"bma/open-design-mcp:local", filepath.Join(root, "docker", "open-design-mcp")},
		{"bma/ui-preview-mcp:local", filepath.Join(root, "docker", "ui-preview-mcp")},
	}
	changed := false
	for _, img := range images {
		if imageExists(img.tag) {
			continue
		}
		if _, err := os.Stat(img.ctx); err != nil {
			continue
		}
		changed = true
		fmt.Fprintf(out, "[media] 构建镜像 %s ...\n", img.tag)
		if o, err := runDocker(20*time.Minute, "build", "-t", img.tag, img.ctx); err != nil {
			fmt.Fprintf(out, "[media] 构建 %s 失败: %v\n%s\n", img.tag, err, tailLines(o, 3))
		}
	}
	if !containerRunning("bma-open-design") || !containerRunning("bma-minimax-media") {
		changed = true
	}
	if !changed {
		return "媒体栈已就绪"
	}

	fmt.Fprintln(out, "[media] 拉起 open_design / minimax-media ...")
	o, err := composeUp(composeFile, 20*time.Minute, "open_design", "minimax-media")
	if err != nil {
		fmt.Fprintf(out, "[media] 启动失败: %v\n%s\n", err, tailLines(o, 3))
		return "媒体栈启动失败（不影响主流程）"
	}
	return "媒体栈已更新"
}

// imageExists 判断本地镜像是否存在。
func imageExists(tag string) bool {
	_, err := runDocker(10*time.Second, "image", "inspect", tag)
	return err == nil
}

// containerRunning 判断容器是否在跑（按名字过滤）。
func containerRunning(name string) bool {
	out, err := runDocker(10*time.Second, "ps", "-q", "--filter", "name="+name)
	return err == nil && strings.TrimSpace(out) != ""
}

// runDocker 执行 docker 命令并合并标准输出/错误。
func runDocker(timeout time.Duration, args ...string) (string, error) {
	return runDockerBinary(timeout, "docker", args...)
}

// runDockerBinary 执行指定可执行文件（docker / docker-compose）。
func runDockerBinary(timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

// writeDepsReport 把结果写成 JSON（供 --ensure-deps 的调用方读取）。
func writeDepsReport(path string, rep *DepsReport) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// errText 把错误转成简短可读文本。
func errText(err error) string {
	if err == nil {
		return "正常"
	}
	return err.Error()
}

// tailLines 取文本末尾 n 行（用于把 docker 的长输出压成错误摘要）。
func tailLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.Join(lines, " | ")
	}
	return strings.Join(lines[len(lines)-n:], " | ")
}
