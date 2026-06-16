package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/blockmemory/agent/internal/config"
	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/internal/server"
	"github.com/blockmemory/agent/internal/store"
	pkgconfig "github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
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
		pgStore = nil
	}
	if pgStore != nil {
		defer pgStore.Close()
	}

	redisStore, err := store.NewRedisStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Printf("Warning: redis not available: %v", err)
		redisStore = nil
	}
	if redisStore != nil {
		defer redisStore.Close()
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

	// 构建三层图
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	escalation := graph.NewEscalationHandlerNode()
	sinker := &sinkerNode{}
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	threeLayerGraph := builder.Build()

	// 初始化会话管理器
	sessionMgr := server.NewSessionManager(threeLayerGraph, registry)

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

	// 静态文件
	fs := http.FileServer(http.Dir("web"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))

	// 首页
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "web/index.html")
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

		// /api/sessions/{id}
		if len(path) > len("/api/sessions/") {
			mgr.HandleGetSession(w, r)
			return
		}

		http.NotFound(w, r)
	}
}

// sinkerNode 终止节点
type sinkerNode struct{}

func (n *sinkerNode) Name() string { return "Sinker" }
func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = types.ActionFinish
	return state, nil
}
