package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/internal/memory"
	"github.com/blockmemory/agent/internal/server"
	"github.com/blockmemory/agent/internal/store"
)

func main() {
	var (
		postgresDSN = flag.String("postgres", "postgres://user:pass@localhost/blockmemory?sslmode=disable", "PostgreSQL DSN")
		redisAddr   = flag.String("redis", "localhost:6379", "Redis address")
		redisPass   = flag.String("redis-pass", "", "Redis password")
		redisDB     = flag.Int("redis-db", 0, "Redis DB")
		httpAddr    = flag.String("http", ":8080", "HTTP server address")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 初始化存储层
	pgStore, err := store.NewPostgresStore(*postgresDSN)
	if err != nil {
		log.Printf("Warning: postgres not available: %v", err)
		pgStore = nil
	}
	if pgStore != nil {
		defer pgStore.Close()
	}

	redisStore, err := store.NewRedisStore(*redisAddr, *redisPass, *redisDB)
	if err != nil {
		log.Printf("Warning: redis not available: %v", err)
		redisStore = nil
	}
	if redisStore != nil {
		defer redisStore.Close()
	}

	// 初始化广播器
	broadcaster := server.NewTUIBroadcaster()

	// 初始化记忆控制器
	var writeProcessor *memory.WriteProcessor
	var snapshotMgr *memory.SnapshotManager
	if pgStore != nil {
		writeProcessor = memory.NewWriteProcessor(pgStore)
		snapshotMgr = memory.NewSnapshotManager(redisStore, pgStore)
	}

	_ = memory.NewCallbackHandler(writeProcessor, snapshotMgr, broadcaster)

	// 初始化 Agent 注册表
	registry := graph.NewSimpleRegistry()
	// 注册示例 Agent
	registry.Register(&graph.AgentInfo{
		ID:          "ui_agent_homepage",
		Name:        "UI Homepage Agent",
		Description: "处理首页 UI 相关问题",
		ModuleID:    "ui",
		Keywords:    []string{"ui", "homepage", "page", "frontend", "界面", "首页"},
	})
	registry.Register(&graph.AgentInfo{
		ID:          "cart_agent",
		Name:        "Cart Agent",
		Description: "处理购物车相关问题",
		ModuleID:    "cart",
		Keywords:    []string{"cart", "shopping", "buy", "购物车", "购买"},
	})

	// 初始化工作区 (使用 Redis)
	var workspaceClient graph.WorkspaceClient = redisStore
	var workspaceWriter graph.WorkspaceWriter = redisStore
	var workspaceUpdater graph.WorkspaceUpdater = redisStore

	// 构建 Graph
	router := graph.NewRouterNode(registry, workspaceClient)
	validator := graph.NewValidatorNode()
	workspaceNode := graph.NewWorkspaceUpdaterNode(workspaceUpdater)
	escalation := graph.NewEscalationHandlerNode()

	// Agent 节点工厂
	agentFactory := func(agentID string) graph.Node {
		var assembler graph.ContextAssembler
		if snapshotMgr != nil {
			// TODO: 创建实际的 ContextAssembler
			assembler = nil
		}
		return graph.NewAgentExecutorNode(agentID, assembler, snapshotMgr, workspaceWriter)
	}

	// 创建默认 Agent Executor (会被动态替换)
	agentExecutor := agentFactory("default")

	// 构建图
	compiledGraph, err := graph.BuildDefaultGraph(
		router,
		agentExecutor,
		validator,
		workspaceNode,
		escalation,
		graph.NewSinkerNode(redisStore),
	)
	if err != nil {
		log.Fatalf("build graph: %v", err)
	}

	// 初始化 API 处理器
	apiHandler := server.NewAPIHandler(broadcaster)
	if snapshotMgr != nil {
		apiHandler.SetSnapshotManager(snapshotMgr)
	}

	// 启动 HTTP 服务器
	go startHTTPServer(ctx, *httpAddr, broadcaster, apiHandler)

	// 启动示例话题
	go func() {
		time.Sleep(2 * time.Second)
		state := graph.NewState("T-fix-overlap", "修复商城主页穿模")
		log.Printf("Starting topic: %s", state.TopicID)

		result, err := compiledGraph.Invoke(ctx, state)
		if err != nil {
			log.Printf("Graph execution error: %v", err)
			return
		}
		log.Printf("Topic completed: %s, final action: %s", result.TopicID, result.NextAction)
	}()

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	cancel()
}

func startHTTPServer(ctx context.Context, addr string, broadcaster *server.TUIBroadcaster, apiHandler *server.APIHandler) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tui/stream", broadcaster.SSEHandler)
	mux.HandleFunc("/api/tui/retrieve", apiHandler.RetrieveHandler)
	mux.HandleFunc("/api/tui/event/resolve", apiHandler.EventResolveHandler)
	mux.HandleFunc("/api/tui/snapshot/inspect", apiHandler.SnapshotInspectHandler)
	mux.HandleFunc("/api/tui/graph/pause", apiHandler.GraphPauseHandler)
	mux.HandleFunc("/api/tui/graph/resume", apiHandler.GraphResumeHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("HTTP server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
