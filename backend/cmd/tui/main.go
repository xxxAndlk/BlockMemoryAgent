package main

// cmd/tui is the bubbletea terminal UI entry point. It bootstraps the same
// backend wiring as the HTTP server, then starts a local auth-free HTTP server
// so the TUI input bar can POST to /api/sessions/* and /api/dag/*.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/bootstrap"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/logging"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/tui"
)

func main() {
	// 统一按窄字符计算等宽字体宽度，避免 Windows 终端下 box-drawing / CJK 字符
	// 被 runewidth 误判为宽字符，导致 lipgloss 布局错位或右侧面板溢出问题。
	runewidth.DefaultCondition.EastAsianWidth = false

	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	noAltScreen := flag.Bool("no-alt-screen", false, "禁用 alt-screen（CI 或非 TTY 自动禁用）")
	flag.Parse()

	// 严格启动：任一必需配置文件缺失即失败。
	for path, name := range map[string]string{
		*configPath: "config file",
		*rolePath:   "roles file",
		*envPath:    "env file",
		*soulPath:   "soul file",
		*skillPath:  "skills file",
	} {
		if _, err := os.Stat(path); err != nil {
			log.Fatalf("%s not found: %s", name, path)
		}
	}

	if err := config.LoadEnvFile(*envPath); err != nil {
		log.Fatalf("load .env: %v", err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 日志文件输出（TUI 入口）：按天分割到 logs/tui/YYYY-MM-DD.log
	// bubbletea 用 alt-screen 全屏接管终端，日志若走 stderr 会刷到屏幕上顶乱布局。
	// TUI 必须静默 stderr，始终写文件。即使配置中未启用文件日志，也写入默认目录兜底，
	// 避免日志完全丢入 io.Discard 导致排障无据可查。
	logDir := cfg.Logging.Dir
	if !cfg.Logging.Enabled || logDir == "" {
		logDir = "logs"
	}
	logWriter, err := logging.Init(logging.EntryTUI, logDir, true)
	if err != nil {
		fmt.Println("warning: init file logging:", err)
	} else {
		log.SetOutput(logWriter)
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
		defer logWriter.Close()
	}
	log.Printf("BlockMemoryAgent TUI entry starting, log dir=%s", logDir)

	ctx := context.Background()

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
		// log.Fatalf 不会在终端显示失败原因，用户只看到 "exit status 1"。
		// 这里先 fmt.Fprintln 到 stderr 让终端可见，再 log.Fatal 写文件留痕并退出。
		msg := fmt.Sprintf("启动失败：backend wiring 未通过: %v", err)
		fmt.Fprintln(os.Stderr, msg)
		log.Fatal(msg)
	}
	defer app.Close()

	// Start a local HTTP server so the TUI input bar can POST to /api/sessions/* and /api/dag/*.
	// 这里不直接复用 server.api.go 是因为 TUI 进程内已持有 SessionManager/Graph 实例，
	// 直接走本地 HTTP 比进程内调用更解耦：输入栏只关心 HTTP，便于后续替换为远程后端。
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			app.Server.HandleListSessions(w, r)
		case http.MethodPost:
			app.Server.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Session subresources use Go 1.22 path variables so handlers can read r.PathValue("id").
	mux.HandleFunc("/api/sessions/{id}/stream", app.Server.HandleSessionStream)
	mux.HandleFunc("/api/sessions/{id}/message", app.Server.HandleSessionMessage)
	mux.HandleFunc("/api/sessions/{id}/clarify", app.Server.HandleSessionClarify)
	mux.HandleFunc("/api/sessions/{id}/interrupt", app.Server.HandleSessionInterrupt)
	mux.HandleFunc("/api/sessions/{id}/enqueue", app.Server.HandleSessionEnqueue)
	mux.HandleFunc("/api/sessions/{id}/cancel", app.Server.HandleSessionCancel)
	mux.HandleFunc("/api/sessions/{id}/board", app.Server.HandleSessionBoard)
	mux.HandleFunc("/api/sessions/{id}/agents", app.Server.HandleSessionAgents)
	mux.HandleFunc("/api/sessions/{id}/metrics", app.Server.HandleSessionMetrics)
	mux.HandleFunc("/api/sessions/{id}/watchdog", app.Server.HandleSessionWatchdog)
	mux.HandleFunc("/api/sessions/{id}/topic", app.Server.HandleSessionTopic)
	mux.HandleFunc("/api/sessions/{id}", app.Server.HandleGetSession)

	mux.Handle("/api/dag", app.DAGHandler)
	mux.Handle("/api/dag/", app.DAGHandler)

	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(app.Server)
	mux.HandleFunc("/api/metrics", apiHandler.MetricsHandler)

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"postgres":{"online":true},"redis":{"online":false,"detail":"not configured in TUI"},"llm":{"online":true}}`)
	})

	// 监听 127.0.0.1:0 让内核分配空闲端口，避免与其他进程冲突；端口通过 ln.Addr() 回传给 TUI
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	go func() {
		// 关闭连接时的 "use of closed network connection" 是正常退出，不当作错误
		if err := http.Serve(ln, mux); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("http server: %v", err)
		}
	}()
	httpAddr := "http://" + ln.Addr().String()
	log.Printf("TUI backend listening at %s", httpAddr)

	modelName := app.RoleConfig.MetaAgent.ModelConfig.Model
	model := tui.NewModel(app.Agent, app.DAGHandler, httpAddr, modelName)

	// CI 环境或 stdin 非 TTY 时自动禁用 alt-screen，避免输出被吞或光标异常。
	useAltScreen := !*noAltScreen && os.Getenv("CI") == "" && isatty.IsTerminal(os.Stdin.Fd())
	opts := []tea.ProgramOption{tea.WithMouseCellMotion()}
	if useAltScreen {
		opts = append(opts, tea.WithAltScreen())
	}
	p := tea.NewProgram(model, opts...)
	// panic 恢复：确保异常退出时记录堆栈，bubbletea 自身会恢复终端
	defer func() {
		if r := recover(); r != nil {
			log.Printf("TUI panic recovered: %v (terminal may need reset)", r)
		}
	}()
	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
	// T8 修复：退出前关闭 HTTP listener + 取消运行中会话 ctx。
	// 原实现仅靠 defer pgStore.Close()，未关闭 ln（端口悬挂到进程退出）、
	// 未取消 graph.Invoke goroutine（SSE 流 / goroutine 泄漏到 os.Exit）。
	ln.Close()
	app.Agent.Shutdown(context.Background())
}
