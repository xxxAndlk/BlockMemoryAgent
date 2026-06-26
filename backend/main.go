package main

// main.go 是 HTTP 服务入口，负责把配置、存储、模型、图、会话管理器、路由、SSE、信号处理串联起来。
// 启动顺序: 配置加载 → 存储初始化 → 角色配置 → 模型工厂预热 → 图构建 → 会话管理器 → 路由注册 → HTTP 监听 → 信号处理优雅关闭。

import (
	"context" // 上下文，用于取消与超时控制
	"flag"    // 命令行参数解析
	"log"     // 日志输出
	"net/http"// HTTP 服务与路由
	"os"      // 文件信息、信号
	"os/signal"// 信号监听
	"strings" // DSN 脱敏时的字符串处理
	"syscall" // SIGINT/SIGTERM 信号常量
	"time"    // 超时时长

	"github.com/blockmemory/agent/backend/internal/config"   // 基础设施配置加载
	"github.com/blockmemory/agent/backend/internal/graph"    // 三层图构建与节点
	"github.com/blockmemory/agent/backend/internal/memory"   // 快照管理器
	"github.com/blockmemory/agent/backend/internal/model"    // 模型工厂
	"github.com/blockmemory/agent/backend/internal/runtime"  // 运行时聚合（看板/邮箱/Skill/Soul/Watchdog）
	"github.com/blockmemory/agent/backend/internal/server"   // HTTP API 与会话管理器
	"github.com/blockmemory/agent/backend/internal/skill"    // Skill 池加载
	"github.com/blockmemory/agent/backend/internal/store"    // Postgres / Redis 存储
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config" // 角色配置（roles.yaml）
	"github.com/blockmemory/agent/backend/pkg/types"         // 公共类型（状态、节点接口）
)

// main 是服务入口。职责: 解析 flag → 装配依赖 → 启动 HTTP → 等待信号优雅关闭。
// 副作用: 打开/关闭 Postgres、Redis 连接；监听端口；注册路由。
func main() {
	// 命令行 flag: 各类配置文件路径，默认值指向仓库内标准位置
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	flag.Parse() // 解析 flag，解析后上述指针才指向实际值

	// 加载 .env 文件: 若存在则把其中 KEY=VALUE 注入进程环境变量
	if _, err := os.Stat(*envPath); err == nil {
		// 文件存在，尝试解析并加载
		if err := config.LoadEnvFile(*envPath); err != nil {
			log.Fatalf("load .env file: %v", err) // 解析失败直接退出
		}
		log.Printf("Loaded environment variables from %s", *envPath)
	} else {
		// 无 .env 文件，回退使用系统环境变量
		log.Printf("No .env file found at %s, using system environment variables", *envPath)
	}

	// 加载基础设施配置（config.yaml: Postgres DSN、pgvector、Redis、HTTP 地址、记忆间隔等）
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err) // 配置加载失败不可恢复
	}

	// 根上下文，cancel 在收到信号时触发，用于通知后台任务退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // 兜底取消

	// 初始化 Postgres 存储层；连接失败直接 fatal，不再降级
	pgStore, err := store.NewPostgresStore(cfg.Postgres.DSN)
	if err != nil {
		log.Fatalf("postgres not available: %v (DSN: %s)", err, redactDSN(cfg.Postgres.DSN))
	}
	defer pgStore.Close() // 关闭连接池
	// 自动应用 session_history 迁移，避免用户忘记跑 002_session_history.sql
	if err := store.EnsureSessionHistorySchema(ctx, pgStore.DB()); err != nil {
		log.Fatalf("ensure session_history schema: %v", err)
	}
	log.Printf("Postgres connected; session_history table ready")

	// 初始化 Redis 存储层；连接失败直接 fatal
	redisStore, err := store.NewRedisStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Fatalf("redis not available: %v (addr=%s)", err, cfg.Redis.Addr)
	}
	defer redisStore.Close() // 关闭连接
	log.Printf("Redis connected at %s", cfg.Redis.Addr)

	// 加载角色配置（roles.yaml: meta_agent/domain_agent/fixed_roles/dynamic_templates）
	roleCfg, err := pkgconfig.LoadRoleConfig(*rolePath)
	if err != nil {
		log.Fatalf("load role config: %v", err) // 角色配置缺失不可恢复
	}

	// 初始化模型工厂: 按角色缓存 blades ModelProvider 实例
	modelFactory := model.NewModelFactory(roleCfg)
	// 预热: 提前创建常用角色模型，缺失 API Key 或连接失败直接 fatal
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Fatalf("model warmup failed: %v", err)
	}

	// 初始化角色注册表（运行期角色实例仓库）和角色工厂（创建动态/固定角色实例）
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// soul.md 必须存在；缺失直接 fatal
	if *soulPath == "" {
		log.Fatalf("soul path is required")
	}
	if _, err := os.Stat(*soulPath); err != nil {
		log.Fatalf("soul file not found: %s (%v)", *soulPath, err)
	}

	// Skill yaml 必须存在；缺失直接 fatal
	if _, err := os.Stat(*skillPath); err != nil {
		log.Fatalf("skill yaml not found: %s (%v)", *skillPath, err)
	}
	skillPool, err := skill.LoadFromYAML(*skillPath)
	if err != nil {
		log.Fatalf("load skill yaml: %v", err)
	}
	log.Printf("Loaded skill pool from %s (count=%d)", *skillPath, len(skillPool.All()))

	// 创建运行时聚合: 看板 / 邮箱 / Watchdog / 人格 / Skill 注册表，统一注入图与节点
	rt := runtime.New(*soulPath, skillPool)
	rt.SetAgentConfig(&cfg.Agent) // 注入 Agent 运行时动态参数（特性2）

	// 构建三层图: MetaAgent 节点 + 升级处理器 + 终止节点
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory) // 注入模型工厂供节点调用 LLM
	metaAgent.SetRuntime(rt)                // 注入运行时聚合（看板/邮箱等）
	// 注入历史存储适配器，让 MetaAgent 能读取过往会话历史
	metaAgent.SetHistoryStore(&pgHistoryAdapter{pg: pgStore})
	escalation := graph.NewEscalationHandlerNode() // 升级仲裁节点
	sinker := &sinkerNode{}                        // 终止节点，强制 Finish
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory) // 全局模型工厂
	builder.SetRuntime(rt)                // 全局运行时
	builder.AddNode(metaAgent)            // 注册 MetaAgent 节点
	builder.AddNode(escalation)           // 注册升级处理节点
	builder.AddNode(sinker)               // 注册终止节点
	threeLayerGraph := builder.Build()    // 构建不可变图

	// 初始化会话管理器: 管理内存中的会话生命周期，连接图与存储
	sessionMgr := server.NewSessionManager(threeLayerGraph, registry)
	sessionMgr.SetPostgresStore(pgStore) // 注入 Postgres 以持久化历史

	// 启动时从 session_history 恢复历史会话到内存，让前端列表不空
	ctxRestore, cancelRestore := context.WithTimeout(ctx, 5*time.Second) // 最多恢复 5 秒
	n := sessionMgr.RestoreSessions(ctxRestore, 50)                      // 最多恢复 50 条
	cancelRestore()                                                       // 释放子上下文
	if n > 0 {
		log.Printf("Restored %d past sessions into in-memory list", n)
	}

	// 快照管理器（Redis 热加载 + Postgres 持久化）
	snapshotMgr := memory.NewSnapshotManager(redisStore, pgStore)

	// API 处理器（web 面板 / TUI 共用），通过 setter 逐步注入依赖
	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(sessionMgr)
	apiHandler.SetRuntime(rt)
	apiHandler.SetStores(pgStore, redisStore)
	apiHandler.SetRoleConfig(roleCfg)
	apiHandler.SetModelFactory(modelFactory)
	apiHandler.SetSnapshotManager(snapshotMgr)

	// 路由注册
	mux := http.NewServeMux()

	// /api/sessions: 列表（GET）与创建（POST）分发
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sessionMgr.HandleListSessions(w, r)
		case http.MethodPost:
			sessionMgr.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// /api/sessions/ 下的子路径（stream/message/board/agents/metrics/watchdog/mailbox）统一由 sessionRouter 分发
	mux.HandleFunc("/api/sessions/", sessionRouter(sessionMgr))

	// 全局状态与指标端点
	mux.HandleFunc("/api/health", apiHandler.HealthHandler)
	mux.HandleFunc("/api/status", apiHandler.StatusHandler)
	mux.HandleFunc("/api/metrics/timeline", apiHandler.TimelineHandler)
	mux.HandleFunc("/api/activity", apiHandler.ActivityHandler)

	// 记忆 / Skill / 文件相关端点
	mux.HandleFunc("/api/snapshot", apiHandler.SnapshotHandler)
	mux.HandleFunc("/api/memory/search", apiHandler.MemorySearchHandler)
	mux.HandleFunc("/api/memory/levels", apiHandler.MemoryLevelsHandler)
	mux.HandleFunc("/api/skills", apiHandler.SkillsHandler)
	mux.HandleFunc("/api/agents/", agentRouter(apiHandler)) // /api/agents/{id}/skills
	mux.HandleFunc("/api/files", apiHandler.FilesHandler)
	mux.HandleFunc("/api/files/content", apiHandler.FileContentHandler)

	// 静态文件: Vue 构建产物在 web/dist/，仅暴露 assets 与 favicon
	fs := http.FileServer(http.Dir("web/dist"))
	mux.Handle("/assets/", fs)
	mux.Handle("/favicon.svg", fs)

	// 首页: Vue SPA，history 路由统一回退 index.html
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/dist/index.html")
	})

	// 启动 HTTP 服务
	addr := cfg.HTTP.Addr
	log.Printf("BlockMemoryAgent starting on http://localhost%s", addr)

	// 构造 http.Server，Handler 指向上面注册好的 mux
	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// 后台 goroutine 监听并服务；非 ErrServerClosed 错误视为致命
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// 等待中断信号（Ctrl+C 或 kill）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh // 阻塞直到收到信号

	// 优雅关闭: 取消上下文并关闭 HTTP 服务
	log.Println("Shutting down...")
	cancel()
	httpServer.Close()
}

// agentRouter 把 /api/agents/{id}/skills 路由到 AgentSkillsHandler。
// 参数: handler — API 处理器，提供 AgentSkillsHandler 方法。
// 返回: http.HandlerFunc，处理单条路由。
// 副作用: 无。
func agentRouter(handler *server.APIHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// 仅匹配以 /skills 结尾的路径，其余返回 404
		if len(path) > len("/api/agents/") && path[len(path)-len("/skills"):] == "/skills" {
			handler.AgentSkillsHandler(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

// sessionRouter 把 /api/sessions/{id} 下的多种子路径分发到对应处理器。
// 覆盖子路径: stream / message / board / agents / metrics / watchdog / mailbox / {id}。
// 参数: mgr — 会话管理器，提供各子路径处理器。
// 返回: http.HandlerFunc，处理上述子路径。
// 副作用: 无。
func sessionRouter(mgr *server.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// /api/sessions/{id}/stream —— SSE 实时事件流
		if len(path) > len("/api/sessions/") && path[len(path)-len("/stream"):] == "/stream" {
			mgr.HandleSessionStream(w, r)
			return
		}

		// /api/sessions/{id}/message —— 给会话发送消息
		if len(path) > len("/api/sessions/") && path[len(path)-len("/message"):] == "/message" {
			mgr.HandleSessionMessage(w, r)
			return
		}

		// /api/sessions/{id}/board —— 任务看板
		if len(path) > len("/api/sessions/") && path[len(path)-len("/board"):] == "/board" {
			mgr.HandleSessionBoard(w, r)
			return
		}

		// /api/sessions/{id}/agents —— 会话内角色实例
		if len(path) > len("/api/sessions/") && path[len(path)-len("/agents"):] == "/agents" {
			mgr.HandleSessionAgents(w, r)
			return
		}

		// /api/sessions/{id}/metrics —— 会话指标
		if len(path) > len("/api/sessions/") && path[len(path)-len("/metrics"):] == "/metrics" {
			mgr.HandleSessionMetrics(w, r)
			return
		}

		// /api/sessions/{id}/watchdog —— 看门狗状态
		if len(path) > len("/api/sessions/") && path[len(path)-len("/watchdog"):] == "/watchdog" {
			mgr.HandleSessionWatchdog(w, r)
			return
		}

		// /api/sessions/{id}/mailbox —— 邮箱事件
		if len(path) > len("/api/sessions/") && path[len(path)-len("/mailbox"):] == "/mailbox" {
			mgr.HandleSessionMailbox(w, r)
			return
		}

		// /api/sessions/{id} —— 单个会话详情
		if len(path) > len("/api/sessions/") {
			mgr.HandleGetSession(w, r)
			return
		}

		// 其余未匹配路径返回 404
		http.NotFound(w, r)
	}
}

// pgHistoryAdapter 把 *store.PostgresStore 适配为 graph.HistoryStore 接口。
// 设计意图: 解耦 MetaAgent 与具体存储实现，便于替换或测试。
type pgHistoryAdapter struct {
	pg *store.PostgresStore // 被包装的 Postgres 存储
}

// RecentSessionHistories 返回最近的会话历史记录，转换为 graph.HistoryEntry。
// 参数: ctx — 上下文；limit — 最多返回条数。
// 返回: 历史条目切片与错误。
// 副作用: 只读查询。
func (a *pgHistoryAdapter) RecentSessionHistories(ctx context.Context, limit int) ([]graph.HistoryEntry, error) {
	recs, err := a.pg.RecentSessionHistories(ctx, limit) // 调用底层存储查询
	if err != nil {
		return nil, err
	}
	// 逐条转换为 graph 包的 HistoryEntry 类型
	out := make([]graph.HistoryEntry, 0, len(recs))
	for _, r := range recs {
		out = append(out, graph.HistoryEntry{
			SessionID:   r.SessionID,
			Goal:        r.Goal,
			Summary:     r.Summary,
			ToolResults: r.ToolResults,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}

// redactDSN 把 DSN 中的密码替换为 ***，仅用于启动日志，避免泄露凭据。
// 参数: dsn — 原始连接字符串。
// 返回: 脱敏后的字符串。
// 副作用: 无。
func redactDSN(dsn string) string {
	// 处理 postgres://user:pass@host/db 形式
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]                 // 协议之后的部分
		if at := strings.Index(rest, "@"); at >= 0 {
			userpass := rest[:at] // user:pass 子串
			if colon := strings.Index(userpass, ":"); colon >= 0 {
				user := userpass[:colon] // 仅保留用户名
				// 重组: 协议 + user + *** + @host/db
				return dsn[:i+3] + user + ":***@" + rest[at+1:]
			}
		}
	}
	return dsn // 无法识别的格式原样返回
}

// sinkerNode 是三层图的终止节点，Invoke 时强制把 NextAction 置为 Finish。
// 设计意图: 提供一个稳定的出口节点，保证状态机能够结束。
type sinkerNode struct{}

// Name 返回节点名称，用于路由与日志。
func (n *sinkerNode) Name() string { return "Sinker" }

// Invoke 把状态的 NextAction 置为 ActionFinish，使状态机循环退出。
// 参数: ctx — 上下文；state — 当前状态。
// 返回: 更新后的状态与 nil 错误。
// 副作用: 修改传入的 state。
func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = types.ActionFinish
	return state, nil
}
