// Package testserver encapsulates the backend wiring logic from main.go
// so that both the production binary and integration tests can start the
// same HTTP mux in-process. It delegates the actual dependency construction
// to internal/bootstrap and exposes the live dependencies via Deps.
package testserver

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/bootstrap"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
)

// EmbedderFactory 创建并注入 embedder；独立函数便于测试替换。
var EmbedderFactory = func(cfg *config.Config, roleCfg *pkgconfig.RoleConfigFile) (embed.Embedder, error) {
	return embed.NewEmbedder(roleCfg.Embed, cfg.PgVector.Dimensions)
}

// Deps holds the live backend dependencies returned by BuildHandler. Tests can
// use it to access stores, the session manager, runtime, and the model factory
// directly when HTTP alone is not enough.
type Deps struct {
	Config         *config.Config
	RoleConfig     *pkgconfig.RoleConfigFile
	Postgres       *store.PostgresStore
	Redis          *store.RedisStore
	ModelFactory   *model.ModelFactory
	Embedder       embed.Embedder
	Runtime        *runtime.Runtime
	SessionManager *server.SessionManager
	Agent          agent.Agent
	DAGScheduler   *dag.Scheduler
	DAGHandler     *server.DAGHandler
}

// BuildHandler wires the backend by delegating to bootstrap.Build and returns the
// root mux plus the live dependencies. Callers own ctx cancellation. The
// returned cleanup function closes stores; it should be deferred by callers.
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

	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath: cfgPath,
		RolePath:   rolePath,
		EnvPath:    envPath,
		SoulPath:   soulPath,
		SkillPath:  skillPath,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	deps := &Deps{
		Config:         app.Config,
		RoleConfig:     app.RoleConfig,
		Postgres:       app.Postgres,
		Redis:          app.Redis,
		ModelFactory:   app.ModelFactory,
		Embedder:       app.Embedder,
		Runtime:        app.Runtime,
		SessionManager: app.Server,
		Agent:          app.Agent,
		DAGScheduler:   app.DAGScheduler,
		DAGHandler:     app.DAGHandler,
	}

	cleanup := func() {
		_ = app.Close()
	}

	mux := bootstrap.NewDefaultMux(app)
	return mux, deps, cleanup, nil
}
