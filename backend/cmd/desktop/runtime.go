//go:build windows

// 自带运行时：包内随附的 PostgreSQL(pgvector) 与 Redis，由外壳当子进程拉起。
//
// 为什么要自带：pgvector 只能跑在 PostgreSQL 里，而让用户机装 Docker Desktop
// 并不符合"装个软件就能用"的常规预期（Postgres.app / pgAdmin 都是自带服务端二进制、
// 当子进程跑）。因此安装包里放一份免安装版 PostgreSQL + pgvector 扩展 + Redis，
// 由外壳负责初始化、拉起、健康检查与收尾——目标机器不需要任何外部依赖。
//
// 布局：
//   <安装目录>\runtime\postgres\            PG 免安装版（share\extension\vector.dll = pgvector）
//   <安装目录>\runtime\redis\               Redis for Windows
//   %LOCALAPPDATA%\BlockMemoryAgent\data\pg     PG 数据目录（initdb 产物，可变大）
//   %LOCALAPPDATA%\BlockMemoryAgent\data\redis  Redis 工作目录
//
// 与开发实例隔离：自带运行时用独立端口（默认 55432 / 56380）与独立数据目录，
// 不碰开发实例那套 Docker 容器与卷。桌面版因此是一份独立"产品实例"。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"syscall"

	"github.com/blockmemory/agent/backend/internal/config"
)

// syscallHideWindow 隐藏子进程窗口（GUI 外壳不能闪黑框）。
var syscallHideWindow = syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}

const (
	// runtimeDirName 安装目录下的自带运行时目录名。
	runtimeDirName = "runtime"
	// defaultPgPort 自带 PostgreSQL 默认端口（避开开发实例的 5432）。
	defaultPgPort = 55432
	// defaultRedisPort 自带 Redis 默认端口（避开开发实例的 6380）。
	defaultRedisPort = 56380
	// embeddedUser 自带实例的超级用户 / 库名。
	embeddedUser = "blockmemory"
	// embeddedDB 自带实例的数据库名。
	embeddedDB = "blockmemory"
)

// instanceConfig 自带实例的身份信息（口令与端口）。
//
// 归属**用户数据目录**而不是安装目录：数据目录（%LOCALAPPDATA%\BlockMemoryAgent\data）
// 是按用户共享的，换安装目录/重装时口令必须跟着数据走——否则新生成的实例口令与
// 已在跑的实例对不上（Redis 认证失败、PG 连不上）。
type instanceConfig struct {
	PgPassword string `json:"pg_password"`
	RedisPass  string `json:"redis_password"`
	PgPort     int    `json:"pg_port"`
	RedisPort  int    `json:"redis_port"`
	// Adopted 标记"这是新建的实例文件"——用于沿用旧版本存在 .env 里的口令。
	Adopted bool `json:"-"`
}

// instanceFile 自带实例的身份文件路径。
func instanceFile() (string, error) {
	dataRoot, err := userDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataRoot, "instance.json"), nil
}

// loadInstance 读实例身份（不存在则新建并落盘）。
func loadInstance() (*instanceConfig, error) {
	path, err := instanceFile()
	if err != nil {
		return nil, err
	}
	cfg := &instanceConfig{}
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, cfg)
		existed = true
	}
	changed := false
	if cfg.PgPassword == "" {
		cfg.PgPassword = randomToken()
		changed = true
	}
	if cfg.RedisPass == "" {
		cfg.RedisPass = randomToken()
		changed = true
	}
	if cfg.PgPort <= 0 {
		cfg.PgPort = defaultPgPort
		changed = true
	}
	if cfg.RedisPort <= 0 {
		cfg.RedisPort = defaultRedisPort
		changed = true
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, err
		}
	}
	cfg.Adopted = !existed
	return cfg, nil
}

// runtimeInfo 自带运行时的生效参数（写回 .env 后端才能连上）。
type runtimeInfo struct {
	DSN        string
	RedisAddr  string
	RedisPass  string
	PgPort     int
	RedisPort  int
	DataDir    string
	UsedEmbedded bool
}

// runtimeAvailable 判断安装包里是否带了自带运行时。
func runtimeAvailable(root string) bool {
	return fileExists(filepath.Join(root, runtimeDirName, "postgres", "bin", "postgres.exe")) &&
		fileExists(filepath.Join(root, runtimeDirName, "redis", "redis-server.exe"))
}

// userDataDir 用户数据目录（%LOCALAPPDATA%\BlockMemoryAgent）。
//
// 装在 LOCALAPPDATA 而不是安装目录：安装目录只放程序，大且持续增长的数据
// （PG 数据目录）另放，符合商业桌面应用的常规分层。
func userDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "BlockMemoryAgent"), nil
}

// ensureEmbeddedRuntime 拉起自带运行时，返回生效的连接参数。
//
// 只在"依赖指向本机"时接管：用户显式配了远程 PostgreSQL/Redis 就不碰。
func ensureEmbeddedRuntime(root string, onStep func(string)) (*runtimeInfo, error) {
	if !runtimeAvailable(root) {
		return nil, fmt.Errorf("安装包未包含自带运行时（缺少 %s）", runtimeDirName)
	}
	step := func(format string, a ...any) {
		if onStep != nil {
			onStep(fmt.Sprintf(format, a...))
		}
	}

	dataRoot, err := userDataDir()
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(dataRoot, "data")
	pgData := filepath.Join(dataDir, "pg")
	redisData := filepath.Join(dataDir, "redis")
	for _, d := range []string{dataRoot, dataDir, pgData, redisData} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	// 口令与端口：归属用户数据目录的实例文件（跟着数据走，换安装目录也一致）。
	inst, err := loadInstance()
	if err != nil {
		return nil, err
	}
	// 兼容早期版本：口令曾存在安装目录的 .env 里，实例文件还没有 → 沿用旧口令，
	// 否则已初始化的数据目录会因为口令对不上而连不上。
	if inst.Adopted {
		envPath := filepath.Join(root, ".env")
		if v := readEnvValue(envPath, "BMA_EMBEDDED_PG_PASSWORD"); v != "" {
			inst.PgPassword = v
		}
		if v := readEnvValue(envPath, "BMA_EMBEDDED_REDIS_PASSWORD"); v != "" {
			inst.RedisPass = v
		}
	}
	envPath := filepath.Join(root, ".env")
	pgPass := inst.PgPassword
	redisPass := inst.RedisPass

	pgPort, err := resolveEmbeddedPort(inst.PgPort, func(port int) bool {
		return probePostgres(fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", embeddedUser, pgPass, port), 2*time.Second) == nil
	})
	if err != nil {
		return nil, err
	}
	inst.PgPort = pgPort
	redisPort, err := resolveEmbeddedPort(inst.RedisPort, func(port int) bool {
		return probeRedis(fmt.Sprintf("127.0.0.1:%d", port), redisPass, 2*time.Second) == nil
	})
	if err != nil {
		return nil, err
	}
	inst.RedisPort = redisPort
	if err := saveInstance(inst); err != nil {
		return nil, err
	}

	// 1) 初始化数据目录（只在首次）。
	if !fileExists(filepath.Join(pgData, "PG_VERSION")) {
		step("初始化内置 PostgreSQL 数据目录（首次运行，约需十几秒）...")
		if err := initPostgres(root, pgData, pgPass); err != nil {
			return nil, err
		}
	}

	// 2) 拉起 PostgreSQL。
	step("启动内置 PostgreSQL ...")
	if err := startPostgres(root, pgData, pgPort); err != nil {
		return nil, err
	}
	step("等待内置 PostgreSQL 就绪 ...")
	adminDSN := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", embeddedUser, pgPass, pgPort)
	if err := waitPostgres(adminDSN, 60*time.Second); err != nil {
		return nil, err
	}

	// 3) 建库 + 装 pgvector 扩展。
	step("准备数据库与 pgvector 扩展 ...")
	if err := ensureDatabase(adminDSN, pgPass); err != nil {
		return nil, err
	}

	// 4) 拉起 Redis。
	step("启动内置 Redis ...")
	if err := startRedis(root, redisData, redisPort, redisPass); err != nil {
		return nil, err
	}
	step("等待内置 Redis 就绪 ...")
	if err := waitRedis(fmt.Sprintf("127.0.0.1:%d", redisPort), redisPass, 30*time.Second); err != nil {
		return nil, err
	}

	info := &runtimeInfo{
		DSN:          fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", embeddedUser, pgPass, pgPort, embeddedDB),
		RedisAddr:    fmt.Sprintf("127.0.0.1:%d", redisPort),
		RedisPass:    redisPass,
		PgPort:       pgPort,
		RedisPort:    redisPort,
		DataDir:      dataDir,
		UsedEmbedded: true,
	}
	// 写回 .env：后端启动时按 .env 连库，不同步就还是旧地址。
	if err := setEnvValue(envPath, "POSTGRES_DSN", info.DSN); err != nil {
		return nil, err
	}
	if err := setEnvValue(envPath, "REDIS_ADDR", info.RedisAddr); err != nil {
		return nil, err
	}
	if err := setEnvValue(envPath, "REDIS_PASSWORD", info.RedisPass); err != nil {
		return nil, err
	}
	return info, nil
}

// initPostgres 用包内 initdb 初始化数据目录。
//
// 编码：优先 UTF8；但中文 Windows 的系统代码页是 936，PostgreSQL for Windows 没有
// 对应编码，initdb 会报 "encoding mismatch"（locale 是 SQL_ASCII）。此时退回
// SQL_ASCII——服务端不做转码，客户端（lib/pq）送来的 UTF-8 字节原样存取，
// 中文内容不受影响（代价是不做大小写/区域感知比较）。
func initPostgres(root, pgData, password string) error {
	bin := filepath.Join(root, runtimeDirName, "postgres", "bin")
	// initdb 需要从密码文件读口令（命令行明文会留在进程列表里）。
	pwFile := filepath.Join(filepath.Dir(pgData), "initdb_pw.txt")
	if err := os.WriteFile(pwFile, []byte(password), 0o600); err != nil {
		return err
	}
	defer os.Remove(pwFile)

	base := []string{
		"-D", pgData,
		"-U", embeddedUser,
		"-A", "scram-sha-256",
		"--pwfile=" + pwFile,
	}
	var lastOut string
	var lastErr error
	for _, enc := range []string{"UTF8", "SQL_ASCII"} {
		args := append(append([]string{}, base...), "-E", enc)
		out, err := runBinary(5*time.Minute, filepath.Join(bin, "initdb.exe"), args...)
		if err == nil {
			return nil
		}
		lastOut, lastErr = out, err
		if !strings.Contains(out, "encoding mismatch") && !strings.Contains(out, "编码") {
			break // 不是编码问题，换编码也没用
		}
	}
	return fmt.Errorf("initdb 失败: %v\n%s", lastErr, tailLines(lastOut, 5))
}

// startPostgres 用 pg_ctl 拉起 PostgreSQL（已在跑就直接复用）。
func startPostgres(root, pgData string, port int) error {
	bin := filepath.Join(root, runtimeDirName, "postgres", "bin")
	logPath := filepath.Join(filepath.Dir(pgData), "postgres.log")
	ctlLog := filepath.Join(filepath.Dir(pgData), "pg_ctl.log")
	// 已在本实例目录跑着就不重复起。
	if out, err := runBinary(30*time.Second, filepath.Join(bin, "pg_ctl.exe"), "-D", pgData, "status"); err == nil && strings.Contains(out, "running") {
		return nil
	}
	args := []string{
		"-D", pgData,
		"-l", logPath,
		"-o", fmt.Sprintf("-p %d -h 127.0.0.1", port),
		"-w", "-t", "60",
		"start",
	}
	// 输出必须落文件而不是管道：pg_ctl 拉起的 postgres 会继承管道句柄，
	// 管道不 EOF 时 CombinedOutput 会永久阻塞（实测卡死 5 分钟）。
	if err := runBinaryToFile(90*time.Second, filepath.Join(bin, "pg_ctl.exe"), ctlLog, args...); err != nil {
		return fmt.Errorf("启动 PostgreSQL 失败: %v\n%s", err, tailLines(readFileTail(ctlLog, 4096), 5))
	}
	return nil
}

// stopPostgres 干净关停 PostgreSQL（避免下次启动走崩溃恢复）。
func stopPostgres(root, pgData string) {
	bin := filepath.Join(root, runtimeDirName, "postgres", "bin")
	ctlLog := filepath.Join(filepath.Dir(pgData), "pg_ctl.log")
	_ = runBinaryToFile(60*time.Second, filepath.Join(bin, "pg_ctl.exe"), ctlLog, "-D", pgData, "-m", "fast", "stop")
}

// waitPostgres 等 PostgreSQL 接受连接。
func waitPostgres(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", dsn)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = db.PingContext(ctx)
			cancel()
			db.Close()
			if err == nil {
				return nil
			}
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("PostgreSQL 未就绪: %v", last)
}

// ensureDatabase 建库并装 pgvector 扩展（幂等）。
func ensureDatabase(adminDSN, password string) error {
	db, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", embeddedDB).Scan(&exists); err != nil {
		return fmt.Errorf("检查数据库失败: %w", err)
	}
	if !exists {
		// CREATE DATABASE 不能带参数，用标识符拼接（embeddedDB 是常量，非用户输入）。
		if _, err := db.Exec("CREATE DATABASE " + embeddedDB); err != nil {
			return fmt.Errorf("建库失败: %w", err)
		}
	}

	appDSN := strings.Replace(adminDSN, "/postgres?", "/"+embeddedDB+"?", 1)
	appDB, err := sql.Open("postgres", appDSN)
	if err != nil {
		return err
	}
	defer appDB.Close()
	if _, err := appDB.Exec("CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("安装 pgvector 扩展失败: %w", err)
	}
	return nil
}

// startRedis 拉起包内 Redis（已在跑就复用）。
func startRedis(root, redisData string, port int, password string) error {
	exe := filepath.Join(root, runtimeDirName, "redis", "redis-server.exe")
	// 已在本机该端口跑着就不重复起。
	if probeRedis(fmt.Sprintf("127.0.0.1:%d", port), password, time.Second) == nil {
		return nil
	}
	args := []string{
		"--port", strconv.Itoa(port),
		"--bind", "127.0.0.1",
		"--requirepass", password,
		"--dir", redisData,
		"--appendonly", "no",
		"--save", "",
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscallHideWindow
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Redis 失败: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// waitRedis 等 Redis 响应 PING。
func waitRedis(addr, password string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := probeRedis(addr, password, 2*time.Second); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("Redis 未就绪: %v", last)
}

// resolveEmbeddedPort 决定自带实例用哪个端口。
//
// 端口记在实例文件里：重启复用同一端口（残留实例还在跑时直接接上）；
// 端口被别人占了才往后找一个空闲的。alive 探针用来判断"占用者是不是我们自己
// 这套实例"（口令对得上就复用，不重复起）。
func resolveEmbeddedPort(preferred int, alive func(int) bool) (int, error) {
	if alive(preferred) {
		return preferred, nil
	}
	for p := preferred; p < preferred+50; p++ {
		if portFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口 %d~%d 都被占用", preferred, preferred+49)
}

// saveInstance 落盘实例身份。
func saveInstance(cfg *instanceConfig) error {
	path, err := instanceFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ensureAllDeps 依赖自举的统一入口：安装包带了自带运行时就用它（目标机免装
// Docker），没带才走 Docker 路径（开发机上没铺 runtime 的旧安装）。
func ensureAllDeps(root string, onStep func(string)) (*DepsReport, error) {
	// .env 载入进程环境：媒体服务等子进程要继承（媒体通道配置只存在于 .env）。
	if envPath := filepath.Join(root, ".env"); fileExists(envPath) {
		_ = config.LoadEnvFile(envPath)
	}
	if !embeddedRuntimeApplies(root) {
		return ensureDeps(root, onStep)
	}
	rep := &DepsReport{Steps: []string{}}
	step := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		rep.Steps = append(rep.Steps, msg)
		if onStep != nil {
			onStep(msg)
		}
	}
	step("使用安装包自带的 PostgreSQL / Redis（无需 Docker）")
	if _, err := ensureEmbeddedRuntime(root, func(s string) { step("%s", s) }); err != nil {
		rep.Error = "启动自带运行时失败：" + err.Error()
		rep.Hint = "自带运行时位于 <安装目录>\\runtime\\；可删掉该目录改用 Docker 依赖"
		return rep, errors.New(rep.Error)
	}
	rep.OK = true
	rep.Infra = "内置 PostgreSQL 与 Redis 已就绪"
	return rep, nil
}

// embeddedRuntimeApplies 判断能否用自带运行时接管：
// 包里带了运行时，且 .env 里的依赖地址指向本机（用户显式配了远程库就不接管）。
func embeddedRuntimeApplies(root string) bool {
	if !runtimeAvailable(root) {
		return false
	}
	env := filepath.Join(root, ".env")
	if dsn := readEnvValue(env, "POSTGRES_DSN"); dsn != "" && !dsnIsLocal(dsn) {
		return false
	}
	if addr := readEnvValue(env, "REDIS_ADDR"); addr != "" && !addrIsLocal(addr) {
		return false
	}
	return true
}

// dsnIsLocal 判断 DSN 是否指向本机。
func dsnIsLocal(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return true
	}
	return isLocalHost(u.Hostname())
}

// addrIsLocal 判断 host:port 是否指向本机。
func addrIsLocal(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return isLocalHost(host)
}

// stopEmbeddedRuntime 收尾：干净关停自带 PostgreSQL，停掉自带 Redis。
func stopEmbeddedRuntime(root string) {
	dataRoot, err := userDataDir()
	if err != nil {
		return
	}
	pgData := filepath.Join(dataRoot, "data", "pg")
	if fileExists(filepath.Join(pgData, "PG_VERSION")) {
		stopPostgres(root, pgData)
	}
	// Redis 是以子进程方式起的：按可执行文件路径精确匹配安装目录下的那个。
	stopProcessByDir(filepath.Join(root, runtimeDirName, "redis"), "redis-server.exe")
}

// readEnvInt 读 .env 里的整数（缺省或非法时用 def）。
func readEnvInt(path, key string, def int) int {
	v := readEnvValue(path, key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// portFree 端口是否空闲。
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// randomToken 生成随机口令。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "bma" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// setEnvValue 写 .env 的某个键（保留其余行与注释）。
func setEnvValue(path, key, value string) error {
	lines := []string{}
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	}
	found := false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(strings.ToUpper(trimmed), strings.ToUpper(key)+"=") {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	out := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o644)
}

// runBinary 执行一个可执行文件并合并输出。
func runBinary(timeout time.Duration, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscallHideWindow
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runBinaryToFile 执行一个可执行文件，输出重定向到文件（不建管道）。
//
// 为什么不用管道：被拉起的服务端进程（如 pg_ctl 拉起的 postgres）会继承管道句柄，
// 父进程退出后管道仍不 EOF，读端会永久阻塞（实测 pg_ctl start 卡死 5 分钟）。
func runBinaryToFile(timeout time.Duration, exe, logPath string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscallHideWindow
	return cmd.Run()
}

// readFileTail 读文件末尾 n 字节（用于把日志尾部塞进错误信息）。
func readFileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) == 0 {
		return ""
	}
	return string(buf)
}
