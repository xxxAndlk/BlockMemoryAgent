// Package bootstrap centralizes the backend dependency wiring that was previously
// duplicated across backend/main.go, backend/cmd/tui/main.go and
// internal/testserver. It produces an App whose exported fields are the stable
// surfaces consumed by HTTP/TUI/test entry points, while keeping graph/runtime
// registry details internal to the package.
package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
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
)

// ConfigPaths groups the file-system locations required to boot the backend.
// All paths are required except SkillPath, which may be empty when no skill pool
// is configured.
type ConfigPaths struct {
	ConfigPath string
	RolePath   string
	EnvPath    string
	SoulPath   string
	SkillPath  string
	// LogWriter is an optional writer for the structured session logger.
	// When nil the logger writes to os.Stderr.
	LogWriter io.Writer
}

// App is the result of bootstrap.Build. Exported fields are the surfaces needed
// by HTTP/TUI/test callers; internal wiring (graph, runtime, registry) is kept
// unexported to prevent upper layers from depending on bootstrap internals.
type App struct {
	Agent                 agent.Agent
	Server                *server.SessionManager
	DAGHandler            *server.DAGHandler
	Runtime               *runtime.Runtime
	Postgres              *store.PostgresStore
	Redis                 *store.RedisStore
	ModelFactory          *model.ModelFactory
	Embedder              embed.Embedder
	Config                *config.Config
	RoleConfig            *pkgconfig.RoleConfigFile
	Graph                 *graph.ThreeLayerGraph
	MemoryCallbackHandler *memory.CallbackHandler
	SnapshotManager       *memory.SnapshotManager
	ContextAssembler      *memory.ContextAssembler
	EpisodeCompressor     *memory.Compressor
	DAGScheduler          *dag.Scheduler
	Logger                *logger.Logger

	// cleanup holds the resources that must be released when the App shuts down.
	cleanup []func() error
}

// Close releases all resources constructed by Build in reverse order.
func (a *App) Close() error {
	var firstErr error
	for i := len(a.cleanup) - 1; i >= 0; i-- {
		if err := a.cleanup[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Build wires the backend with the same initialization order as the original
// main.go/testserver: env -> config -> stores -> models -> graph -> session
// manager -> agent facade. The returned App owns the constructed resources;
// callers must invoke App.Close when shutting down.
func Build(ctx context.Context, paths ConfigPaths) (*App, error) {
	if err := validatePaths(paths); err != nil {
		return nil, err
	}

	if err := config.LoadEnvFile(paths.EnvPath); err != nil {
		return nil, fmt.Errorf("load env file %s: %w", paths.EnvPath, err)
	}

	cfg, err := config.Load(paths.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", paths.ConfigPath, err)
	}

	roleCfg, err := pkgconfig.LoadRoleConfig(paths.RolePath)
	if err != nil {
		return nil, fmt.Errorf("load roles %s: %w", paths.RolePath, err)
	}

	pgStore, err := store.NewPostgresStore(ctx, cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("connect postgres with DSN from %s: %w", paths.ConfigPath, err)
	}
	pgStore.SetEmbeddingDim(cfg.PgVector.Dimensions)
	pgStore.SetSearchBlockMemoryMaxTokens(cfg.Agent.SearchBlockMemoryMaxTokens)

	embedder, err := embed.NewEmbedder(roleCfg.Embed, cfg.PgVector.Dimensions)
	if err != nil {
		pgStore.Close()
		return nil, fmt.Errorf("create embedder: %w", err)
	}
	pgStore.SetEmbedder(embedder)

	if err := ensureSchemas(ctx, pgStore, cfg.PgVector.Dimensions); err != nil {
		pgStore.Close()
		return nil, err
	}

	redisStore, err := store.NewRedisStore(ctx, cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		pgStore.Close()
		return nil, fmt.Errorf("connect redis with addr from %s: %w", paths.ConfigPath, err)
	}

	snapshotMgr := memory.NewSnapshotManager(redisStore, pgStore, &cfg.Agent)
	writeProcessor := memory.NewWriteProcessor(pgStore)
	writeProcessor.SetAgentConfig(&cfg.Agent)
	memoryCallbackHandler := memory.NewCallbackHandler(writeProcessor, snapshotMgr, nil)
	episodeCompressor := memory.NewCompressor(pgStore, &cfg.Agent)
	sessionLogger := logger.New(pgStore, paths.LogWriter)
	contextAssembler := memory.NewContextAssembler(
		memory.NewSimpleWorkspaceReader(),
		&globalKBAdapter{pg: pgStore, embedder: embedder},
		pgStore,
		cfg.Agent.ContextWindow,
	)
	contextAssembler.SetMemoryPolicy(&cfg.Agent.MemoryPolicy)

	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		closeStores(pgStore, redisStore, memoryCallbackHandler)
		return nil, fmt.Errorf("warmup models: %w", err)
	}
	if err := modelFactory.VerifyConnectivity(ctx); err != nil {
		closeStores(pgStore, redisStore, memoryCallbackHandler)
		return nil, fmt.Errorf("verify LLM connectivity: %w", err)
	}

	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	skillPool, err := skill.LoadFromYAML(paths.SkillPath)
	if err != nil {
		closeStores(pgStore, redisStore, memoryCallbackHandler)
		return nil, fmt.Errorf("load skills %s: %w", paths.SkillPath, err)
	}

	rt, err := runtime.New(paths.SoulPath, skillPool)
	if err != nil {
		closeStores(pgStore, redisStore, memoryCallbackHandler)
		return nil, fmt.Errorf("init runtime: %w", err)
	}
	rt.SetAgentConfig(&cfg.Agent)

	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	metaAgent.SetRuntime(rt)
	metaAgent.SetHistoryStore(&pgHistoryAdapter{pg: pgStore})
	escalation := graph.NewEscalationHandlerNode()
	sinker := graph.NewSinkerNode()

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
	if cfg.Agent.DAGEnabled {
		dagScheduler = dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
		dagScheduler.Start(ctx)
	}

	agentSvc := agent.NewService(threeLayerGraph, registry, rt,
		agent.WithSessionManager(sessionMgr),
		agent.WithPostgresStore(pgStore),
		agent.WithModelFactory(modelFactory),
	)

	dagHandler := server.NewDAGHandler(pgStore, dagScheduler)

	app := &App{
		Agent:                 agentSvc,
		Server:                sessionMgr,
		DAGHandler:            dagHandler,
		Runtime:               rt,
		Postgres:              pgStore,
		Redis:                 redisStore,
		ModelFactory:          modelFactory,
		Embedder:              embedder,
		Config:                cfg,
		RoleConfig:            roleCfg,
		Graph:                 threeLayerGraph,
		MemoryCallbackHandler: memoryCallbackHandler,
		SnapshotManager:       snapshotMgr,
		ContextAssembler:      contextAssembler,
		EpisodeCompressor:     episodeCompressor,
		DAGScheduler:          dagScheduler,
		Logger:                sessionLogger,
	}

	app.cleanup = []func() error{
		func() error {
			if dagScheduler != nil {
				dagScheduler.Stop()
			}
			return nil
		},
		memoryCallbackHandler.Close,
		func() error { redisStore.Close(); return nil },
		func() error { pgStore.Close(); return nil },
	}

	return app, nil
}

func validatePaths(paths ConfigPaths) error {
	for path, name := range map[string]string{
		paths.ConfigPath: "config file",
		paths.RolePath:   "roles file",
		paths.EnvPath:    "env file",
		paths.SoulPath:   "soul file",
		paths.SkillPath:  "skills file",
	} {
		if path == "" {
			return fmt.Errorf("%s path is required", name)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s not found: %s", name, path)
		}
	}
	return nil
}

func ensureSchemas(ctx context.Context, pgStore *store.PostgresStore, expectedDim int) error {
	for name, fn := range map[string]func(context.Context, *sql.DB) error{
		"session_history": store.EnsureSessionHistorySchema,
		"session_events":  store.EnsureSessionEventsSchema,
		"session_logs":    store.EnsureSessionLogsSchema,
		"dag":             store.EnsureDAGSchema,
		"memory":          store.EnsureInitialMemorySchema,
	} {
		if err := fn(ctx, pgStore.DB()); err != nil {
			return fmt.Errorf("ensure %s schema: %w", name, err)
		}
	}
	if err := store.ValidateEmbeddingDimension(ctx, pgStore.DB(), expectedDim); err != nil {
		return fmt.Errorf("validate embedding dimension: %w", err)
	}
	return nil
}

func closeStores(pg *store.PostgresStore, redis *store.RedisStore, mch *memory.CallbackHandler) {
	if err := mch.Close(); err != nil {
		slog.Debug("close memory callback handler failed", slog.String("error", err.Error()))
	}
	redis.Close()
	pg.Close()
}
