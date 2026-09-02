package main

// cmd/tui 是 bubbletea 终端 UI 的可执行文件入口。
// 它复用与 HTTP 服务相同的 bootstrap 依赖装配逻辑，然后在本机启动一个无需认证的
// HTTP 服务器，使 TUI 输入栏可以通过 POST 访问 /api/sessions/* 与 /api/dag/* 接口。
// 这种设计让输入栏只关心 HTTP 协议，便于后续把 TUI 对接远程后端。

import (
	"context"  // 上下文，用于 bootstrap 装配与优雅关闭
	"flag"     // 命令行参数解析
	"fmt"      // 格式化输出与字符串拼接
	"log"      // 标准库日志；保留以兜底转发未注入 logger 路径的 log 输出
	"net"      // 监听本地 TCP 端口
	"net/http" // 本地 HTTP 服务
	"os"       // 文件状态、标准错误、环境变量、TTY 检测
	"path/filepath" // 配置路径拼接
	"strings"  // 判断关闭网络连接时的预期错误

	tea "github.com/charmbracelet/bubbletea" // TUI 框架
	"github.com/gin-gonic/gin"               // Gin Web 框架（本地 API 服务）
	"github.com/mattn/go-isatty"             // 检测 stdin 是否为终端
	"github.com/mattn/go-runewidth"          // 等宽字符宽度计算

	"github.com/blockmemory/agent/backend/internal/bootstrap" // 统一后端依赖装配
	"github.com/blockmemory/agent/backend/internal/config"    // 配置与环境变量加载
	"github.com/blockmemory/agent/backend/internal/logger"    // 结构化日志器
	"github.com/blockmemory/agent/backend/internal/logging"   // 文件日志按天分割
	"github.com/blockmemory/agent/backend/internal/server"    // HTTP handler 集合
	"github.com/blockmemory/agent/backend/internal/tui"       // TUI 模型与更新逻辑
)

// main 是 TUI 入口函数，按顺序完成：
//  1. 配置 runewidth 避免东亚字符宽度误判；
//  2. 解析命令行参数并校验必需配置文件存在；
//  3. 加载 .env 与 config.yaml；
//  4. 初始化 TUI 专用文件日志；
//  5. 使用 bootstrap 装配后端依赖；
//  6. 启动本地 HTTP 服务；
//  7. 构造 bubbletea 模型并运行；
//  8. 退出前关闭 listener 并取消运行中会话。
func main() {
	// ---- 等宽字符宽度修正 ----
	// bubbletea/lipgloss 依赖 runewidth 计算字符显示宽度。
	// Windows 终端常把 box-drawing / CJK 字符误判为宽字符，导致布局错位或右侧面板溢出。
	// 统一按窄字符计算等宽字体宽度，保证界面在不同终端下一致。
	runewidth.DefaultCondition.EastAsianWidth = false

	// ---- 命令行参数解析 ----
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	noAltScreen := flag.Bool("no-alt-screen", false, "禁用 alt-screen（CI 或非 TTY 自动禁用）")
	flag.Parse() // 解析命令行输入

	// ---- 安装目录解析:未显式指定的配置路径落到 BMA_HOME 下 ----
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
	}
	// home 解析失败不致命:保留 cwd 相对默认值,由下方文件校验报错提示。

	// ---- 启动早期日志器 ----
	// 配置文件校验失败等早期错误需要落到终端，避免用户只看到 exit status 1。
	earlyLogger := logger.NewWithConfig(config.LoggingConfig{Level: "info", Format: "console", Timezone: "Local"}, nil, os.Stderr)

	// ---- 校验必需配置文件 ----
	// 严格启动：任一必需配置文件缺失即失败，避免运行期因缺配置产生隐式错误。
	for path, name := range map[string]string{
		*configPath: "config file",
		*rolePath:   "roles file",
		*envPath:    "env file",
		*soulPath:   "soul file",
		*skillPath:  "skills file",
	} {
		if _, err := os.Stat(path); err != nil {
			// 文件缺失或不可访问，记录致命错误并退出
			earlyLogger.Error(context.Background(), fmt.Sprintf("%s not found: %s", name, path), err)
			os.Exit(1)
		}
	}

	// 加载 .env 文件，把 KEY=VALUE 注入进程环境变量。
	if err := config.LoadEnvFile(*envPath); err != nil {
		earlyLogger.Error(context.Background(), "load .env", err)
		os.Exit(1)
	}

	// 加载基础设施配置。
	cfg, err := config.Load(*configPath)
	if err != nil {
		earlyLogger.Error(context.Background(), "load config", err)
		os.Exit(1)
	}

	// ---- 初始化文件日志 ----
	// TUI 入口的日志按天分割到 logs/tui/YYYY-MM-DD.log。
	// bubbletea 使用 alt-screen 全屏接管终端，若日志走 stderr 会刷到屏幕上顶乱布局，
	// 因此 TUI 必须静默 stderr，始终写文件。即使配置未启用文件日志，也写入默认目录兜底，
	// 避免日志完全丢弃导致排障无据可查。
	var tuiLogger *logger.Logger
	logDir := cfg.Logging.Dir
	if !cfg.Logging.Enabled || logDir == "" {
		// 配置未启用或目录为空时，使用默认 logs 目录
		logDir = "logs"
	}
	if homeErr == nil {
		logDir = config.ResolveUnderHome(home, logDir)
	}
	// logging.Init 第三个参数 silent=true，表示同时关闭 stderr 输出
	logWriter, err := logging.Init(logging.EntryTUI, logDir, true)
	if err != nil {
		// 初始化失败时向终端打印警告，然后继续使用默认 log 输出
		fmt.Println("warning: init file logging:", err)
		tuiLogger = logger.NewWithConfig(cfg.Logging, nil, os.Stderr)
	} else {
		tuiLogger = logger.NewWithConfig(cfg.Logging, nil, logWriter)
		defer logWriter.Close() // 退出时关闭日志文件句柄
	}
	tuiLogger.Info(context.Background(), fmt.Sprintf("BlockMemoryAgent TUI entry starting, log dir=%s", logDir))

	// ---- 兜底转发标准库 log 输出 ----
	// 业务代码已统一注入 *logger.Logger；少数未注入路径（如测试直接构造的结构体）
	// 仍回退标准库 log。将其输出转发到 tuiLogger，避免 TUI 界面被刷乱。
	log.SetOutput(tuiLogger.StdLogWriter())
	log.SetFlags(0)
	log.SetPrefix("")

	// ---- 构造根上下文 ----
	// bootstrap.Build 需要上下文；当前未设置超时，使用 background。
	ctx := context.Background()

	// ---- 装配后端依赖 ----
	// 使用与 HTTP 服务同一份 wiring，保证 TUI 行为与生产一致。
	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath: *configPath,
		RolePath:   *rolePath,
		EnvPath:    *envPath,
		SoulPath:   *soulPath,
		SkillPath:  *skillPath,
		LogWriter:  logWriter,
	})
	if err != nil {
		// logging.Init(silent=true) 已把 log 输出重定向到日志文件，
		// 日志 Fatal 不会在终端显示失败原因，用户只看到 "exit status 1"。
		// 这里先 fmt.Fprintln 到 stderr 让终端可见，再写文件留痕并退出。
		msg := fmt.Sprintf("启动失败：backend wiring 未通过: %v", err)
		fmt.Fprintln(os.Stderr, msg)
		tuiLogger.Error(ctx, msg, err)
		os.Exit(1)
	}
	defer app.Close() // main 返回时释放数据库、缓存等资源

	// ---- 启动本地 HTTP 服务（Gin）----
	// 这里不直接复用生产路由是因为 TUI 进程内已持有 SessionManager/Graph 实例，
	// 直接走本地 HTTP 比进程内调用更解耦：输入栏只关心 HTTP，便于后续替换为远程后端。
	// 服务仅绑定 127.0.0.1，无需鉴权中间件。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.HandleMethodNotAllowed = true

	// /api 分组：与生产路由同构的会话端点全集（server.RegisterSessionRoutes 共用）。
	api := router.Group("/api")
	server.RegisterSessionRoutes(api, app.Server)

	// DAG 路由：调度器未启用时 handler 自行返回 503。
	app.DAGHandler.RegisterRoutes(api)

	// 通用指标路由：通过 NewAPIHandler 构造，注入 SessionManager
	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(app.Server)
	api.GET("/metrics", apiHandler.MetricsHandler)

	// 健康检查端点：固定返回依赖状态，TUI 内部可用于快速自检
	api.GET("/health", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json",
			[]byte(`{"postgres":{"online":true},"redis":{"online":false,"detail":"not configured in TUI"},"llm":{"online":true}}`))
	})

	// 监听 127.0.0.1:0 让内核分配空闲端口，避免与其他进程冲突；
	// 实际端口通过 ln.Addr() 回传给 TUI。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tuiLogger.Error(context.Background(), "listen", err)
		os.Exit(1)
	}
	// 在独立 goroutine 中服务 HTTP 请求
	go func() {
		// 关闭 listener 时的 "use of closed network connection" 是正常退出信号，不当作错误
		if err := http.Serve(ln, router); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			tuiLogger.Error(context.Background(), "http server", err)
		}
	}()
	httpAddr := "http://" + ln.Addr().String()
	tuiLogger.Info(context.Background(), fmt.Sprintf("TUI backend listening at %s", httpAddr))

	// ---- 构造 TUI 模型 ----
	// 从角色配置中读取模型名称，传递给 TUI 用于标题栏显示
	modelName := app.RoleConfig.MetaAgent.ModelConfig.Model
	model := tui.NewModel(app.Agent, app.DAGHandler, httpAddr, modelName)
	model.SetLogger(app.Logger)

	// ---- 配置 bubbletea 程序选项 ----
	// CI 环境或 stdin 非 TTY 时自动禁用 alt-screen，避免输出被吞或光标异常。
	useAltScreen := !*noAltScreen && os.Getenv("CI") == "" && isatty.IsTerminal(os.Stdin.Fd())
	opts := []tea.ProgramOption{tea.WithMouseCellMotion()} // 启用鼠标单元格移动事件
	if useAltScreen {
		// 终端支持且未显式禁用，则启用 alt-screen 全屏
		opts = append(opts, tea.WithAltScreen())
	}
	p := tea.NewProgram(model, opts...)

	// panic 恢复：确保异常退出时记录堆栈；bubbletea 自身会恢复终端，无需额外处理。
	defer func() {
		if r := recover(); r != nil {
			tuiLogger.Error(context.Background(), fmt.Sprintf("TUI panic recovered: %v (terminal may need reset)", r), nil)
		}
	}()

	// 进入 TUI 主循环；出错则记录并退出
	if _, err := p.Run(); err != nil {
		tuiLogger.Error(context.Background(), "TUI error", err)
		os.Exit(1)
	}

	// ---- 退出清理 ----
	// T8 修复：退出前关闭 HTTP listener 并取消运行中会话上下文。
	// 原实现仅靠 defer pgStore.Close()，未关闭 ln（端口悬挂到进程退出）、
	// 未取消 graph.Invoke goroutine（SSE 流 / goroutine 泄漏到 os.Exit）。
	ln.Close()
	app.Agent.Shutdown(context.Background())
}
