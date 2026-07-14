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
	"os"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/domain/memory"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
)

// ConfigPaths groups the file-system locations required to boot the backend.
// ConfigPath, RolePath and EnvPath are required. SoulPath and SkillPath are
// optional: an empty SoulPath falls back to an empty Persona, and an empty
// SkillPath falls back to the built-in skill pool.
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
// by HTTP/TUI/test callers; internal wiring is kept unexported to prevent upper
// layers from depending on bootstrap internals.
//
// During the ReAct refactor the old graph-based fields are removed; Runtime is
// retained for soul/skills/watchdog API compatibility.
type App struct {
	Agent        agent.Agent
	Server       *server.SessionManager
	DAGHandler   *server.DAGHandler
	Runtime      *runtime.Runtime
	Postgres     *store.PostgresStore
	Redis        *store.RedisStore
	ModelFactory *model.ModelFactory
	Embedder     embed.Embedder
	Config       *config.Config
	RoleConfig   *pkgconfig.RoleConfigFile
	DAGScheduler *dag.Scheduler
	Logger       *logger.Logger

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
// main.go/testserver: env -> config -> stores -> models -> agent -> session
// manager. The ReAct engine is the canonical execution path. The returned App
// owns the constructed resources; callers must invoke App.Close when
// shutting down.
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

	sessionLogger := logger.New(pgStore, paths.LogWriter)

	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("warmup models: %w", err)
	}
	if err := modelFactory.VerifyConnectivity(ctx); err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("verify LLM connectivity: %w", err)
	}

	var skillPool *skill.Pool
	if paths.SkillPath == "" {
		skillPool = skill.BuiltinPool()
	} else {
		var err error
		skillPool, err = skill.LoadFromYAML(paths.SkillPath)
		if err != nil {
			closeStores(pgStore, redisStore)
			return nil, fmt.Errorf("load skills %s: %w", paths.SkillPath, err)
		}
	}

	// The shared mailbox is used by both Runtime (legacy API compatibility) and
	// the ReAct sub-agent dispatcher.
	sharedMailbox := mailbox.New()

	rt, err := runtime.New(paths.SoulPath, skillPool, runtime.WithMailbox(sharedMailbox))
	if err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("init runtime: %w", err)
	}
	rt.SetAgentConfig(&cfg.Agent)

	// ReAct engine wiring.
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	roleRegistry := role.NewRegistry(roleCfg)
	toolRegistry := tool.NewBuiltinRegistry(workDir, &cfg.Agent, nil)
	memoryPipeline := memory.NewPipeline(memory.NewInMemoryStore())

	subAgentDispatcher := subagent.NewDispatcher(roleRegistry, &reactModelFactory{modelFactory}, toolRegistry, sharedMailbox, memoryPipeline)
	subAgentDispatcher.RegisterCallTool(toolRegistry)

	agentSvc := agent.NewReactService(roleRegistry, modelFactory, toolRegistry, sharedMailbox, memoryPipeline, pgStore)

	sessionMgr := server.NewSessionManager(agentSvc)
	sessionMgr.SetPostgresStore(pgStore)
	sessionMgr.SetModelFactory(modelFactory)

	var dagScheduler *dag.Scheduler
	if cfg.Agent.DAGEnabled {
		dagScheduler = dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
		dagScheduler.Start(ctx)
	}

	dagHandler := server.NewDAGHandler(pgStore, dagScheduler)

	app := &App{
		Agent:        agentSvc,
		Server:       sessionMgr,
		DAGHandler:   dagHandler,
		Runtime:      rt,
		Postgres:     pgStore,
		Redis:        redisStore,
		ModelFactory: modelFactory,
		Embedder:     embedder,
		Config:       cfg,
		RoleConfig:   roleCfg,
		DAGScheduler: dagScheduler,
		Logger:       sessionLogger,
	}

	app.cleanup = []func() error{
		func() error {
			if dagScheduler != nil {
				dagScheduler.Stop()
			}
			return nil
		},
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
	} {
		if path == "" {
			return fmt.Errorf("%s path is required", name)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s not found: %s", name, path)
		}
	}
	// SoulPath 与 SkillPath 可选：空值时 runtime.New 使用空 Persona，skill 使用 BuiltinPool。
	if paths.SoulPath != "" {
		if _, err := os.Stat(paths.SoulPath); err != nil {
			return fmt.Errorf("soul file not found: %s", paths.SoulPath)
		}
	}
	if paths.SkillPath != "" {
		if _, err := os.Stat(paths.SkillPath); err != nil {
			return fmt.Errorf("skills file not found: %s", paths.SkillPath)
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

func closeStores(pg *store.PostgresStore, redis *store.RedisStore) {
	redis.Close()
	pg.Close()
}

// reactModelFactory adapts model.ModelFactory to the narrower agent provider
// factory expected by the sub-agent dispatcher.
type reactModelFactory struct {
	inner *model.ModelFactory
}

func (f *reactModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	return f.inner.GetBladesProvider(ctx, roleID)
}
