package main

// main.go 是 HTTP 服务入口，负责解析命令行参数、初始化文件日志、调用 testserver
// 装配依赖、启动 HTTP 监听、等待信号优雅关闭。所有可复用的 wiring 逻辑已下沉到
// internal/testserver 包，保证生产二进制与集成测试使用同一份初始化路径。

import (
	"context"       // 上下文，用于取消与超时控制
	"flag"          // 命令行参数解析
	"log"           // 日志输出
	"net/http"      // HTTP 服务与路由
	"os"            // 文件信息、信号
	"os/signal"     // 信号监听
	"path/filepath" // 可执行文件相对路径解析
	"syscall"       // SIGINT/SIGTERM 信号常量
	"time"          // HTTP 超时

	"github.com/blockmemory/agent/backend/internal/config"      // 基础设施配置加载
	"github.com/blockmemory/agent/backend/internal/logging"     // 日志文件按天分割
	"github.com/blockmemory/agent/backend/internal/testserver" // 可复用的 wiring 封装
)

// main 是服务入口。职责: 解析 flag → 初始化日志 → 装配依赖 → 启动 HTTP → 等待信号优雅关闭。
// 副作用: 打开/关闭 Postgres、Redis 连接；监听端口；注册路由。
func main() {
	// 命令行 flag: 各类配置文件路径，默认值指向仓库内标准位置
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	webDistPath := flag.String("web-dist", "web/dist", "前端构建产物目录路径（相对路径将基于可执行文件目录解析）")
	flag.Parse() // 解析 flag，解析后上述指针才指向实际值

	// 加载 .env 文件: 若存在则把其中 KEY=VALUE 注入进程环境变量
	// testserver.BuildHandler 内部也会加载一次；此处提前加载是为了让日志路径等配置生效。
	if _, err := os.Stat(*envPath); err == nil {
		if err := config.LoadEnvFile(*envPath); err != nil {
			log.Fatalf("加载 .env 文件失败: %v", err) // 解析失败直接退出
		}
		log.Printf("已加载环境变量: %s", *envPath)
	} else {
		log.Printf("未找到 .env 文件 (%s)，使用系统环境变量", *envPath)
	}

	// 加载基础设施配置（config.yaml: Postgres DSN、pgvector、Redis、HTTP 地址、记忆间隔等）
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err) // 配置加载失败不可恢复
	}

	// 日志文件输出（后台入口）：按天分割到 logs/backend/YYYY-MM-DD.log
	// 失败不 fatal：文件日志缺失时仍用 stderr，保证服务可启动。
	// silent=false：HTTP 入口无 alt-screen，stderr + 文件双写便于开发期实时查看。
	if cfg.Logging.Enabled {
		if _, err := logging.Init(logging.EntryBackend, cfg.Logging.Dir, false); err != nil {
			log.Printf("警告: 初始化文件日志失败: %v (仅输出到 stderr)", err)
		} else {
			defer logging.Close() // 进程退出时关闭文件句柄
		}
	}
	log.Printf("BlockMemoryAgent 后台服务启动中, 日志目录=%s", cfg.Logging.Dir)

	// 根上下文，cancel 在收到信号时触发，用于通知后台任务退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // 兜底取消

	// 使用 testserver 装配与 main.go 完全一致的路由和依赖
	mux, _, cleanup, err := testserver.BuildHandler(ctx, *configPath, *rolePath, *envPath, *soulPath, *skillPath)
	if err != nil {
		log.Fatalf("装配依赖失败: %v", err)
	}
	defer cleanup()

	// 静态文件: Vue 构建产物目录支持相对可执行文件路径解析，避免从其他目录启动时失效
	webDist := resolveWebDistPath(*webDistPath)
	fs := http.FileServer(http.Dir(webDist))
	mux.Handle("/assets/", fs)
	mux.Handle("/favicon.svg", fs)

	// 首页: Vue SPA，history 路由统一回退 index.html
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(webDist, "index.html"))
	})

	// 启动 HTTP 服务
	addr := cfg.HTTP.Addr
	log.Printf("BlockMemoryAgent 服务启动: http://localhost%s", addr)

	// 构造 http.Server，Handler 指向上面注册好的 mux
	// P0-01 修复：配置读/写超时，避免慢客户端攻击；IdleTimeout 兜底 120s。
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.HTTP.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTP.WriteTimeout) * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 后台 goroutine 监听并服务；非 ErrServerClosed 错误视为致命
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务错误: %v", err)
		}
	}()

	// 等待中断信号（Ctrl+C 或 kill）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh // 阻塞直到收到信号

	// 优雅关闭: 取消上下文并关闭 HTTP 服务
	log.Println("正在关闭服务...")
	cancel()
	httpServer.Close()
}

// resolveWebDistPath 解析前端构建产物目录路径。
// 若 path 为绝对路径则原样返回；若为相对路径，则基于当前可执行文件所在目录解析，
// 避免服务从其他工作目录启动时找不到 web/dist。
func resolveWebDistPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		// 无法获取可执行文件路径时，回退到原始相对路径（保持旧行为）
		return path
	}
	// 处理符号链接：取最终实际路径
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	baseDir := filepath.Dir(exe)
	return filepath.Join(baseDir, path)
}
