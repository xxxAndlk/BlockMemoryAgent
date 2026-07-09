// Package testserver encapsulates the backend wiring logic from main.go
// so that both the production binary and integration tests can start the
// same HTTP mux in-process. It intentionally keeps the same initialization
// order as main.go: env -> config -> stores -> models -> graph -> session
// manager -> API handler -> mux.
package testserver

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/memory"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// EmbedderFactory 创建并注入 embedder；独立函数便于测试替换。
var EmbedderFactory = func(cfg *config.Config, roleCfg *pkgconfig.RoleConfigFile) (embed.Embedder, error) {
	return embed.NewEmbedder(roleCfg.Embed, cfg.PgVector.Dimensions)
}

// Deps holds the live backend dependencies returned by BuildHandler. Tests can
// use it to access stores, the session manager, runtime, and the model factory
// directly when HTTP alone is not enough.
type Deps struct {
	Config                *config.Config
	RoleConfig            *pkgconfig.RoleConfigFile
	Postgres              *store.PostgresStore
	Redis                 *store.RedisStore
	ModelFactory          *model.ModelFactory
	Embedder              embed.Embedder
	Runtime               *runtime.Runtime
	SessionManager        *server.SessionManager
	Agent                 agent.Agent
	Graph                 *graph.ThreeLayerGraph
	MemoryCallbackHandler *memory.CallbackHandler
	SnapshotManager       *memory.SnapshotManager
	ContextAssembler      *memory.ContextAssembler
	EpisodeCompressor     *memory.Compressor
	DAGScheduler          *dag.Scheduler
	DAGHandler            *server.DAGHandler
}

// BuildHandler wires the backend with the same logic as main.go and returns the
// root mux plus the live dependencies. Callers own ctx cancellation. The
// returned cleanup function closes stores and the memory callback handler; it
// should be deferred by callers.
func BuildHandler(ctx context.Context, cfgPath, rolePath, envPath, soulPath, skillPath string) (*http.ServeMux, *Deps, func(), error) {
	// 严格启动：任意配置文件不存在即失败，明确告知缺失项。
	for path, name := range map[string]string{
		cfgPath:   "config file",
		rolePath:  "roles file",
		envPath:   "env file",
		soulPath:  "soul file",
		skillPath: "skills file",
	} {
		if path == "" {
			return nil, nil, nil, fmt.Errorf("%s path is required", name)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, nil, nil, fmt.Errorf("%s not found: %s", name, path)
		}
	}

	if err := config.LoadEnvFile(envPath); err != nil {
		return nil, nil, nil, fmt.Errorf("load env file %s: %w", envPath, err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config %s: %w", cfgPath, err)
	}

	roleCfg, err := pkgconfig.LoadRoleConfig(rolePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load roles %s: %w", rolePath, err)
	}

	pgStore, err := store.NewPostgresStore(ctx, cfg.Postgres.DSN)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect postgres with DSN from %s: %w", cfgPath, err)
	}
	pgStore.SetEmbeddingDim(cfg.PgVector.Dimensions)
	pgStore.SetSearchBlockMemoryMaxTokens(cfg.Agent.SearchBlockMemoryMaxTokens)

	embedder, err := EmbedderFactory(cfg, roleCfg)
	if err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("create embedder: %w", err)
	}
	pgStore.SetEmbedder(embedder)

	if err := store.EnsureSessionHistorySchema(ctx, pgStore.DB()); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("ensure session_history schema: %w", err)
	}
	if err := store.EnsureSessionEventsSchema(ctx, pgStore.DB()); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("ensure session_events schema: %w", err)
	}
	if err := store.EnsureSessionLogsSchema(ctx, pgStore.DB()); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("ensure session_logs schema: %w", err)
	}
	if err := store.EnsureDAGSchema(ctx, pgStore.DB()); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("ensure dag schema: %w", err)
	}
	if err := store.EnsureInitialMemorySchema(ctx, pgStore.DB()); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("ensure memory schema: %w", err)
	}
	if err := store.ValidateEmbeddingDimension(ctx, pgStore.DB(), cfg.PgVector.Dimensions); err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("validate embedding dimension: %w", err)
	}

	redisStore, err := store.NewRedisStore(ctx, cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		pgStore.Close()
		return nil, nil, nil, fmt.Errorf("connect redis with addr from %s: %w", cfgPath, err)
	}

	snapshotMgr := memory.NewSnapshotManager(redisStore, pgStore, &cfg.Agent)
	writeProcessor := memory.NewWriteProcessor(pgStore)
	writeProcessor.SetAgentConfig(&cfg.Agent)
	memoryCallbackHandler := memory.NewCallbackHandler(writeProcessor, snapshotMgr, nil)
	episodeCompressor := memory.NewCompressor(pgStore, &cfg.Agent)
	sessionLogger := logger.New(pgStore)
	contextAssembler := memory.NewContextAssembler(
		memory.NewSimpleWorkspaceReader(),
		&globalKBAdapter{pg: pgStore, embedder: embedder},
		pgStore,
		cfg.Agent.ContextWindow,
	)
	contextAssembler.SetMemoryPolicy(&cfg.Agent.MemoryPolicy)

	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		pgStore.Close()
		redisStore.Close()
		return nil, nil, nil, fmt.Errorf("warmup models: %w", err)
	}
	// 启动期 LLM 连通性校验：任一已配置角色不可达则装配失败。
	if err := modelFactory.VerifyConnectivity(ctx); err != nil {
		pgStore.Close()
		redisStore.Close()
		return nil, nil, nil, fmt.Errorf("verify LLM connectivity: %w", err)
	}

	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	skillPool, err := skill.LoadFromYAML(skillPath)
	if err != nil {
		pgStore.Close()
		redisStore.Close()
		return nil, nil, nil, fmt.Errorf("load skills %s: %w", skillPath, err)
	}

	rt, err := runtime.New(soulPath, skillPool)
	if err != nil {
		pgStore.Close()
		redisStore.Close()
		return nil, nil, nil, fmt.Errorf("init runtime: %w", err)
	}
	rt.SetAgentConfig(&cfg.Agent)

	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	metaAgent.SetRuntime(rt)
	metaAgent.SetHistoryStore(&pgHistoryAdapter{pg: pgStore})
	escalation := graph.NewEscalationHandlerNode()
	sinker := &sinkerNode{}
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory)
	builder.SetRuntime(rt)
	builder.SetLogger(sessionLogger)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	threeLayerGraph := builder.Build()

	sessionMgr := server.NewSessionManager(threeLayerGraph, registry)
	sessionMgr.SetPostgresStore(pgStore)
	sessionMgr.SetModelFactory(modelFactory)

	threeLayerGraph.SetBlockMemoryStore(&pgBlockMemoryAdapter{pg: pgStore, embedder: embedder, dim: cfg.PgVector.Dimensions})
	threeLayerGraph.SetMemoryCallbackHandler(memoryCallbackHandler)
	threeLayerGraph.SetContextAssembler(contextAssembler)
	threeLayerGraph.SetEpisodeCompressor(episodeCompressor)
	threeLayerGraph.SetAgentSnapshotManager(snapshotMgr)

	var dagScheduler *dag.Scheduler
	if pgStore != nil && cfg.Agent.DAGEnabled {
		dagScheduler = dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
		dagScheduler.Start(ctx)
	}

	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(sessionMgr)
	apiHandler.SetRuntime(rt)
	apiHandler.SetStores(pgStore, redisStore)
	apiHandler.SetRoleConfig(roleCfg)
	apiHandler.SetModelFactory(modelFactory)
	apiHandler.SetSnapshotManager(snapshotMgr)

	mux := http.NewServeMux()

	// 简单 Token 鉴权：默认放行（auth_enabled=false），生产环境应在 config.yaml 启用
	authToken := ""
	publicPaths := []string{"/api/health"}
	if cfg.HTTP.AuthEnabled {
		authToken = cfg.HTTP.AuthToken
	}
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return server.AuthMiddleware(authToken, publicPaths, h)
	}

	mux.HandleFunc("/api/sessions", wrap(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sessionMgr.HandleListSessions(w, r)
		case http.MethodPost:
			sessionMgr.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// Session subresources use Go 1.22 path variables so handlers can read r.PathValue("id").
	mux.HandleFunc("/api/sessions/{id}/stream", wrap(sessionMgr.HandleSessionStream))
	mux.HandleFunc("/api/sessions/{id}/message", wrap(sessionMgr.HandleSessionMessage))
	mux.HandleFunc("/api/sessions/{id}/board", wrap(sessionMgr.HandleSessionBoard))
	mux.HandleFunc("/api/sessions/{id}/agents", wrap(sessionMgr.HandleSessionAgents))
	mux.HandleFunc("/api/sessions/{id}/metrics", wrap(sessionMgr.HandleSessionMetrics))
	mux.HandleFunc("/api/sessions/{id}/logs", wrap(sessionMgr.HandleSessionLogs))
	mux.HandleFunc("/api/sessions/{id}/token-metrics", wrap(sessionMgr.HandleSessionTokenMetrics))
	mux.HandleFunc("/api/sessions/{id}/watchdog", wrap(sessionMgr.HandleSessionWatchdog))
	mux.HandleFunc("/api/sessions/{id}/mailbox", wrap(sessionMgr.HandleSessionMailbox))
	mux.HandleFunc("/api/sessions/{id}/clarify", wrap(sessionMgr.HandleSessionClarify))
	mux.HandleFunc("/api/sessions/{id}/interrupt", wrap(sessionMgr.HandleSessionInterrupt))
	mux.HandleFunc("/api/sessions/{id}/enqueue", wrap(sessionMgr.HandleSessionEnqueue))
	mux.HandleFunc("/api/sessions/{id}/cancel", wrap(sessionMgr.HandleSessionCancel))
	mux.HandleFunc("/api/sessions/{id}/topic", wrap(sessionMgr.HandleSessionTopic))
	mux.HandleFunc("/api/sessions/{id}", wrap(sessionMgr.HandleGetSession))

	mux.HandleFunc("/api/health", apiHandler.HealthHandler)
	mux.HandleFunc("/api/metrics", wrap(apiHandler.MetricsHandler))
	mux.HandleFunc("/api/status", wrap(apiHandler.StatusHandler))
	mux.HandleFunc("/api/metrics/timeline", wrap(apiHandler.TimelineHandler))
	mux.HandleFunc("/api/activity", wrap(apiHandler.ActivityHandler))

	dagHandler := server.NewDAGHandler(pgStore, dagScheduler)
	mux.Handle("/api/dag", wrap(func(w http.ResponseWriter, r *http.Request) {
		dagHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/api/dag/", wrap(func(w http.ResponseWriter, r *http.Request) {
		dagHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/snapshot", wrap(apiHandler.SnapshotHandler))
	mux.HandleFunc("/api/memory/search", wrap(apiHandler.MemorySearchHandler))
	mux.HandleFunc("/api/memory/levels", wrap(apiHandler.MemoryLevelsHandler))
	mux.HandleFunc("/api/memory/eval", wrap(apiHandler.MemoryEvalHandler))
	mux.HandleFunc("/api/skills", wrap(apiHandler.SkillsHandler))
	mux.HandleFunc("/api/agents/{id}/skills", wrap(apiHandler.AgentSkillsHandler))
	mux.HandleFunc("/api/files", wrap(apiHandler.FilesHandler))
	mux.HandleFunc("/api/files/content", wrap(apiHandler.FileContentHandler))

	// Static SPA assets are not wired here; integration tests should hit API
	// endpoints only. The root fallback is omitted to avoid serving web/dist.

	agentSvc := agent.NewService(threeLayerGraph, registry, rt,
		agent.WithPostgresStore(pgStore),
		agent.WithModelFactory(modelFactory),
	)

	deps := &Deps{
		Config:                cfg,
		RoleConfig:            roleCfg,
		Postgres:              pgStore,
		Redis:                 redisStore,
		ModelFactory:          modelFactory,
		Embedder:              embedder,
		Runtime:               rt,
		SessionManager:        sessionMgr,
		Agent:                 agentSvc,
		Graph:                 threeLayerGraph,
		MemoryCallbackHandler: memoryCallbackHandler,
		SnapshotManager:       snapshotMgr,
		ContextAssembler:      contextAssembler,
		EpisodeCompressor:     episodeCompressor,
		DAGScheduler:          dagScheduler,
		DAGHandler:            dagHandler,
	}

	cleanup := func() {
		if dagScheduler != nil {
			dagScheduler.Stop()
		}
		if err := memoryCallbackHandler.Close(); err != nil {
			log.Printf("warning: close memory callback handler: %v", err)
		}
		redisStore.Close()
		pgStore.Close()
	}

	return mux, deps, cleanup, nil
}

type pgBlockMemoryAdapter struct {
	pg       *store.PostgresStore
	embedder embed.Embedder
	dim      int
}

func (a *pgBlockMemoryAdapter) SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string, facts []graph.BlockMemoryFact) error {
	memFacts := make([]memory.Fact, 0, len(facts))
	for _, f := range facts {
		memFacts = append(memFacts, memory.Fact{Key: f.Key, Value: f.Value, Scope: memory.FactScope(f.Scope)})
	}
	rec, err := (&memory.BlockMemoryRecord{
		SessionID: sessionID,
		Domain:    domain,
		Goal:      goal,
		Summary:   summary,
		Facts:     memFacts,
		CreatedAt: time.Now(),
	}).ToKnowledgeRecord(ctx, a.embedder, a.dim)
	if err != nil {
		return fmt.Errorf("convert block memory: %w", err)
	}
	return a.pg.SaveKnowledge(ctx, rec)
}

func (a *pgBlockMemoryAdapter) SearchBlockMemory(ctx context.Context, domain, query string, topK int) (string, error) {
	recs, err := memory.SearchBlockMemory(ctx, a.pg, domain, query, topK)
	if err != nil {
		return "", err
	}
	if len(recs) == 0 {
		return "", nil
	}
	var b strings.Builder
	for i, r := range recs {
		b.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r.Summary))
		if facts := memory.FormatBlockMemoryFacts(r.Facts); facts != "" {
			b.WriteString("facts:\n")
			b.WriteString(facts)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

type globalKBAdapter struct {
	pg       *store.PostgresStore
	embedder embed.Embedder
}

func (a *globalKBAdapter) Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error) {
	emb, err := a.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return a.pg.SearchKnowledge(ctx, emb, topK)
}

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
			MetaMemory:  r.MetaMemory,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}

type sinkerNode struct{}

func (n *sinkerNode) Name() string { return "Sinker" }

func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}
