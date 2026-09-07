package main

// main.go 是 HTTP 服务入口文件，承担以下职责：
//   1. 解析命令行参数，确定各配置文件与前端产物目录的位置；
//   2. 初始化文件日志，将运行期日志按天落盘；
//   3. 调用 bootstrap 包完成依赖装配（数据库、缓存、LLM、路由等）；
//   4. 注册静态资源与 SPA 首页路由；
//   5. 启动 HTTP 服务并监听中断信号，收到信号后优雅关闭。
// 所有可复用的 wiring 逻辑已下沉到 internal/bootstrap 包，保证生产二进制、
// TUI 与集成测试使用同一份初始化路径，避免重复实现导致行为不一致。

import (
	"context"       // 上下文，用于跨 goroutine 传递取消信号与设置超时
	"flag"          // 标准库命令行参数解析
	"fmt"           // 格式化字符串
	"io"            // 日志 writer 接口，用于把 log 输出重定向到文件
	"log"           // 标准库日志；保留以兜底转发未注入 logger 路径的 log 输出
	"net/http"      // HTTP 服务与路由注册
	"os"            // 文件状态、信号、标准错误等
	"os/signal"     // 注册操作系统信号监听器
	"path/filepath" // home 目录与静态资源路径拼接
	"syscall"       // SIGINT/SIGTERM 等信号常量
	"time"          // HTTP 超时与持续时间计算

	"github.com/gin-gonic/gin" // Gin Web 框架（静态资源包装）

	"github.com/blockmemory/agent/backend/internal/bootstrap" // 统一后端依赖装配（wiring）
	"github.com/blockmemory/agent/backend/internal/config"    // 基础设施配置加载与环境变量注入
	"github.com/blockmemory/agent/backend/internal/logger"    // 结构化日志器
	"github.com/blockmemory/agent/backend/internal/logging"   // 日志文件按天分割与静默模式
)

// main 是服务入口函数，按顺序完成：
//  1. 解析 flag；
//  2. 加载 .env 与 config.yaml；
//  3. 初始化文件日志；
//  4. 装配应用上下文与依赖；
//  5. 构造 HTTP 路由并启动监听；
//  6. 等待信号后触发优雅关闭。
//
// 副作用：会打开/关闭 Postgres、Redis 等连接，监听网络端口，注册路由。
func main() {
	// ---- 命令行参数解析 ----
	// flag.String 注册字符串类型命令行参数；第一个参数是 flag 名，第二个是默认值，
	// 第三个是帮助信息。解析后指针才会指向实际传入值。
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	profilePath := flag.String("profile", "config/user_profile.md", "用户画像文件路径（TODO #28）")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	webDistPath := flag.String("web-dist", "web/dist", "前端构建产物目录路径（未显式指定时落到 BMA_HOME/web/dist）")
	flag.Parse() // 解析命令行输入；未解析前 *configPath 等指针仍为默认值

	// ---- 安装目录解析:未显式指定的路径落到 BMA_HOME 下 ----
	home, homeErr := config.HomeDir()
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if homeErr == nil {
		if !explicit["config"] {
			*configPath = filepath.Join(home, "config", "config.yaml")
		}
		if !explicit["roles"] {
			*rolePath = filepath.Join(home, "config", "roles.yaml")
		}
		if !explicit["env"] {
			*envPath = filepath.Join(home, ".env")
		}
		if !explicit["soul"] {
			*soulPath = filepath.Join(home, "config", "soul.md")
		}
		if !explicit["skills"] {
			*skillPath = filepath.Join(home, "config", "skills.yaml")
		}
		if !explicit["profile"] {
			*profilePath = filepath.Join(home, "config", "user_profile.md")
		}
		if !explicit["web-dist"] {
			*webDistPath = filepath.Join(home, "web", "dist")
		}
	}
	// home 解析失败不致命:保留 flag 默认值与既有行为。

	// ---- 启动早期日志器 ----
	// 在配置文件加载之前，任何致命错误都需要落到 stderr；此处使用一个最小配置的
	// zerolog logger，保证启动早期日志格式与运行期一致。
	earlyLogger := logger.NewWithConfig(config.LoggingConfig{Level: "info", Format: "console", Timezone: "Local"}, nil, os.Stderr)
	// home 解析失败时打一行 warning(resolveHome 提示文案),后续仍按 flag 默认值回落 cwd。
	if homeErr != nil {
		earlyLogger.Warn(context.Background(), "安装目录解析失败,回落默认相对路径: "+homeErr.Error())
	}

	// ---- 加载 .env 文件 ----
	// os.Stat 判断文件是否存在；若存在则把其中 KEY=VALUE 注入进程环境变量。
	// bootstrap.Build 内部也会加载一次；此处提前加载是为了让日志路径等配置在
	// 后续 config.Load 之前即可从环境变量读取，保证行为一致。
	if _, err := os.Stat(*envPath); err == nil {
		// 文件存在：调用 LoadEnvFile 逐行解析
		if err := config.LoadEnvFile(*envPath); err != nil {
			// .env 解析失败属于启动期致命错误，直接退出
			earlyLogger.Error(context.Background(), "加载 .env 文件失败", err)
			os.Exit(1)
		}
		earlyLogger.Info(context.Background(), "已加载环境变量: "+*envPath)
	} else {
		// 文件不存在或无法访问：降级使用系统环境变量，保证容器化部署时仍可用
		earlyLogger.Info(context.Background(), fmt.Sprintf("未找到 .env 文件 (%s)，使用系统环境变量", *envPath))
	}

	// ---- 加载基础设施配置 ----
	// config.Load 读取 config.yaml，解析 Postgres DSN、pgvector、Redis、
	// HTTP 地址、记忆间隔等核心运行参数。
	cfg, err := config.Load(*configPath)
	if err != nil {
		// 配置加载失败不可恢复，立即退出
		earlyLogger.Error(context.Background(), "加载配置失败", err)
		os.Exit(1)
	}

	// ---- 初始化文件日志 ----
	// 后台入口按天分割日志到 logs/backend/YYYY-MM-DD.log。
	// silent=false：HTTP 入口不使用 alt-screen，stderr + 文件双写便于开发期实时查看。
	// 失败不 fatal：文件日志缺失时仍用 stderr，保证服务可启动。
	var logWriter io.WriteCloser
	var srvLogger *logger.Logger
	// 日志目录经 BMA_HOME 解析：相对路径落到安装目录下，绝对路径原样使用
	logDir := cfg.Logging.Dir
	if cfg.Logging.Enabled {
		if homeErr == nil {
			logDir = config.ResolveUnderHome(home, logDir)
		}
		// logging.Init 返回一个按天滚动的 io.WriteCloser；EntryBackend 区分入口
		w, err := logging.Init(logging.EntryBackend, logDir, false)
		if err != nil {
			// 初始化失败仅记录警告，保持 stderr 可用
			earlyLogger.Error(context.Background(), "初始化文件日志失败，降级到 stderr", err)
			srvLogger = logger.NewWithConfig(cfg.Logging, nil, os.Stderr)
		} else {
			logWriter = w
			srvLogger = logger.NewWithConfig(cfg.Logging, nil, logWriter)
			defer logWriter.Close() // 进程退出时关闭文件句柄，避免资源泄漏
		}
	} else {
		// 文件日志关闭时统一输出到 stderr
		srvLogger = logger.NewWithConfig(cfg.Logging, nil, os.Stderr)
	}
	srvLogger.Info(context.Background(), fmt.Sprintf("BlockMemoryAgent 后台服务启动中, 日志目录=%s", logDir))

	// ---- 兜底转发标准库 log 输出 ----
	// 业务代码已统一注入 *logger.Logger；少数未注入路径（如测试直接构造的结构体）
	// 仍回退标准库 log。将其输出转发到 srvLogger，避免格式割裂。
	log.SetOutput(srvLogger.StdLogWriter())
	log.SetFlags(0)
	log.SetPrefix("")

	// ---- 构造根上下文 ----
	// context.WithCancel 创建可取消的上下文；cancel 在收到信号时触发，
	// 通知后台任务（如记忆循环）退出。defer cancel() 作为兜底，防止 main 提前返回时泄漏。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- 装配应用依赖 ----
	// bootstrap.Build 统一构造 repository、service、handler、SSE broker 等组件，
	// 返回的 app 对象提供 Close() 用于释放资源。
	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath:  *configPath,  // 基础设施配置路径
		RolePath:    *rolePath,    // 角色配置路径
		EnvPath:     *envPath,     // 环境变量文件路径
		SoulPath:    *soulPath,    // 人格定义文件路径
		ProfilePath: *profilePath, // 用户画像文件路径
		SkillPath:   *skillPath,   // Skill 池 YAML 路径
		LogWriter:   logWriter,    // 文件日志 writer，组件内部可共用
	})
	if err != nil {
		// 依赖装配失败无法继续，退出前 log 已落盘或输出到 stderr
		srvLogger.Error(ctx, "装配依赖失败", err)
		os.Exit(1)
	}
	defer app.Close() // main 返回时释放数据库连接、缓存连接等资源

	// ---- 构造 Gin 路由 ----
	// NewDefaultRouter 已注册全部 API 路由；此处继续注册静态文件与 SPA 首页回退。
	router := bootstrap.NewDefaultRouter(app)

	// 静态文件：Vue 构建产物目录；未显式指定 -web-dist 时已在上文落到 BMA_HOME/web/dist。
	webDist := *webDistPath
	fs := http.FileServer(http.Dir(webDist))
	router.GET("/assets/*filepath", gin.WrapH(fs))    // 静态资源目录（JS/CSS/图片）
	router.GET("/favicon.svg", func(c *gin.Context) { // 站点图标
		c.File(filepath.Join(webDist, "favicon.svg"))
	})

	// SPA 回退：Vue 使用 history 路由，所有未匹配路径统一回退 index.html
	// （含未知 /api 路径，与原标准库路由的 "/" 兜底行为一致）。
	router.NoRoute(func(c *gin.Context) {
		c.File(filepath.Join(webDist, "index.html"))
	})

	// ---- 启动 HTTP 服务 ----
	addr := cfg.HTTP.Addr
	srvLogger.Info(ctx, fmt.Sprintf("BlockMemoryAgent 服务启动: http://localhost%s", addr))

	// 构造 http.Server 实例，Handler 指向上面注册好的 Gin 引擎（实现 http.Handler）。
	// P0-01 修复：显式配置读/写超时，避免慢客户端攻击；IdleTimeout 兜底 120s。
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  time.Duration(cfg.HTTP.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTP.WriteTimeout) * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 在独立 goroutine 中监听并服务；非 ErrServerClosed 的错误视为致命。
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvLogger.Error(ctx, "HTTP 服务错误", err)
			os.Exit(1)
		}
	}()

	// ---- 等待中断信号 ----
	// 注册对 SIGINT（Ctrl+C）和 SIGTERM（kill）的监听，容量 1 避免信号丢失。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh // 阻塞当前 goroutine，直到收到信号

	// ---- 优雅关闭 ----
	// 触发上下文取消，通知后台任务退出；随后优雅关闭 HTTP server。
	srvLogger.Info(ctx, "正在关闭服务...")
	cancel()
	// 优雅关闭：停止接收新连接并等待在途请求完成（避免在途 POST /message 被硬断丢消息）。
	// SSE 长连接不会自行结束，以 3 秒超时封顶后强制关闭兜底；会话中断持久化由
	// defer app.Close() 的 cleanup（Agent.Shutdown 标记中断 + 落库）完成。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		srvLogger.Warn(ctx, "HTTP 连接未在超时内退出，强制关闭: "+err.Error())
		httpServer.Close()
	}
}
