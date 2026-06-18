package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	flag.Parse()

	// 加载 .env 文件
	if _, err := os.Stat(*envPath); err == nil {
		if err := config.LoadEnvFile(*envPath); err != nil {
			log.Fatalf("load .env file: %v", err)
		}
		log.Printf("Loaded environment variables from %s", *envPath)
	} else {
		log.Printf("No .env file found at %s, using system environment variables", *envPath)
	}

	// 加载基础设施配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 初始化存储层
	pgStore, err := store.NewPostgresStore(cfg.Postgres.DSN)
	if err != nil {
		log.Printf("Warning: postgres not available: %v", err)
		log.Printf("  DSN used: %s", redactDSN(cfg.Postgres.DSN))
		log.Printf("  Session history will NOT persist. Set POSTGRES_DSN in .env and apply migrations/*.sql")
		pgStore = nil
	}
	if pgStore != nil {
		defer pgStore.Close()
		// 自动应用 session_history 迁移，避免用户忘记跑 002_session_history.sql
		if err := store.EnsureSessionHistorySchema(ctx, pgStore.DB()); err != nil {
			log.Printf("Warning: ensure session_history schema: %v", err)
		} else {
			log.Printf("Postgres connected; session_history table ready")
		}
	}

	redisStore, err := store.NewRedisStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Printf("Warning: redis not available: %v", err)
		redisStore = nil
	}
	if redisStore != nil {
		defer redisStore.Close()
		log.Printf("Redis connected at %s", cfg.Redis.Addr)
	}

	// 加载角色配置
	roleCfg, err := pkgconfig.LoadRoleConfig(*rolePath)
	if err != nil {
		log.Fatalf("load role config: %v", err)
	}

	// 初始化模型工厂
	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Printf("Warning: model warmup failed: %v", err)
	}

	// 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 加载 Skill 池：优先 yaml 文件，否则回退 BuiltinPool
	var skillPool *skill.Pool
	if _, err := os.Stat(*skillPath); err == nil {
		if p, err := skill.LoadFromYAML(*skillPath); err == nil {
			skillPool = p
			log.Printf("Loaded skill pool from %s (count=%d)", *skillPath, len(p.All()))
		} else {
			log.Printf("load skill yaml failed, fallback to builtin: %v", err)
		}
	}

	// 创建运行时（看板 / 邮箱 / Watchdog / 人格 / Skill 注册表）
	rt := runtime.New(*soulPath, skillPool)

	// 构建三层图
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	metaAgent.SetRuntime(rt)
	if pgStore != nil {
		metaAgent.SetHistoryStore(&pgHistoryAdapter{pg: pgStore})
	}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &sinkerNode{}
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory)
	builder.SetRuntime(rt)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	threeLayerGraph := builder.Build()

	// 初始化会话管理器
	sessionMgr := server.NewSessionManager(threeLayerGraph, registry)
	sessionMgr.SetPostgresStore(pgStore)

	// 启动时从 session_history 恢复历史会话到内存，让前端列表不空
	if pgStore != nil {
		ctxRestore, cancelRestore := context.WithTimeout(ctx, 5*time.Second)
		n := sessionMgr.RestoreSessions(ctxRestore, 50)
		cancelRestore()
		if n > 0 {
			log.Printf("Restored %d past sessions into in-memory list", n)
		}
	} else {
		log.Printf("Postgres unavailable — session history will NOT persist across restarts")
	}

	// 路由
	mux := http.NewServeMux()

	// API
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
	mux.HandleFunc("/api/sessions/", sessionRouter(sessionMgr))

	// 静态文件：Vue 构建产物在 frontend/dist/
	fs := http.FileServer(http.Dir("frontend/dist"))
	mux.Handle("/assets/", fs)
	mux.Handle("/favicon.svg", fs)

	// 首页：Vue SPA（hash 路由，无需服务端回退）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "frontend/dist/index.html")
	})

	// 启动 HTTP 服务
	addr := cfg.HTTP.Addr
	log.Printf("BlockMemoryAgent starting on http://localhost%s", addr)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	cancel()
	httpServer.Close()
}

// sessionRouter 路由 /api/sessions/{id} 和 /api/sessions/{id}/stream
func sessionRouter(mgr *server.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// /api/sessions/{id}/stream
		if len(path) > len("/api/sessions/") && path[len(path)-len("/stream"):] == "/stream" {
			mgr.HandleSessionStream(w, r)
			return
		}

		// /api/sessions/{id}/message
		if len(path) > len("/api/sessions/") && path[len(path)-len("/message"):] == "/message" {
			mgr.HandleSessionMessage(w, r)
			return
		}

		// /api/sessions/{id}/board
		if len(path) > len("/api/sessions/") && path[len(path)-len("/board"):] == "/board" {
			mgr.HandleSessionBoard(w, r)
			return
		}

		// /api/sessions/{id}/agents
		if len(path) > len("/api/sessions/") && path[len(path)-len("/agents"):] == "/agents" {
			mgr.HandleSessionAgents(w, r)
			return
		}

		// /api/sessions/{id}
		if len(path) > len("/api/sessions/") {
			mgr.HandleGetSession(w, r)
			return
		}

		http.NotFound(w, r)
	}
}

// pgHistoryAdapter 把 *store.PostgresStore 适配为 graph.HistoryStore
type pgHistoryAdapter struct {
	pg *store.PostgresStore
}

func (a *pgHistoryAdapter) RecentSessionHistories(ctx context.Context, limit int) ([]graph.HistoryEntry, error) {
	recs, err := a.pg.RecentSessionHistories(ctx, limit)
	if err != nil {
		return nil, err
	}
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

// redactDSN 把 DSN 中的密码替换为 ***，仅用于启动日志
func redactDSN(dsn string) string {
	// 夼理 postgres://user:pass@host/db 形式
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			userpass := rest[:at]
			if colon := strings.Index(userpass, ":"); colon >= 0 {
				user := userpass[:colon]
				return dsn[:i+3] + user + ":***@" + rest[at+1:]
			}
		}
	}
	return dsn
}

// sinkerNode 终止节点
type sinkerNode struct{}

func (n *sinkerNode) Name() string { return "Sinker" }
func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = types.ActionFinish
	return state, nil
}
