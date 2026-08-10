// Package testserver 封装了原本位于 main.go 的后端装配逻辑，
// 使生产二进制与集成测试都能以进程内方式启动相同的 HTTP mux。
// 它把实际依赖构造委托给 internal/bootstrap，并通过 Deps 暴露真实依赖，
// 方便测试在 HTTP 之外直接访问 store、session manager、runtime 等对象。
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

// EmbedderFactory 创建并注入 embedder；独立成函数便于测试替换为 mock 实现。
//
// 参数：
//   - cfg: 应用配置，包含 PgVector 维度。
//   - roleCfg: 角色配置，包含 embed 相关参数。
//
// 返回：构造好的 embed.Embedder，或构造错误。
var EmbedderFactory = func(cfg *config.Config, roleCfg *pkgconfig.RoleConfigFile) (embed.Embedder, error) {
	return embed.NewEmbedder(roleCfg.Embed, cfg.PgVector.Dimensions)
}

// Deps 保存 BuildHandler 返回的真实后端依赖。测试方可以直接使用这些字段，
// 在仅通过 HTTP 无法完成断言时访问 stores、session manager、runtime、
// model factory 等对象。
type Deps struct {
	Config         *config.Config            // 应用运行时配置
	RoleConfig     *pkgconfig.RoleConfigFile // 角色配置
	Postgres       *store.PostgresStore      // PostgreSQL 存储
	Redis          *store.RedisStore         // Redis 存储
	ModelFactory   *model.ModelFactory       // 模型工厂
	Embedder       embed.Embedder            // 向量嵌入器
	Runtime        *runtime.Runtime          // 运行时聚合器
	SessionManager *server.SessionManager    // HTTP 会话管理器
	Agent          agent.Agent               // ReAct Agent 服务
	DAGScheduler   *dag.Scheduler            // DAG 调度器
	DAGHandler     *server.DAGHandler        // DAG HTTP 处理器
}

// BuildHandler 通过委托 bootstrap.Build 装配后端，并返回根 mux、
// 实时依赖对象以及清理函数。调用方负责 ctx 的取消；返回的 cleanup
// 函数会关闭 stores，应由调用方通过 defer 调用。
//
// 参数：
//   - ctx: 用于初始化的上下文。
//   - cfgPath: 主配置文件路径。
//   - rolePath: 角色配置文件路径。
//   - envPath: 环境变量文件路径。
//   - soulPath: 人格文件路径。
//   - skillPath: 技能文件路径。
//
// 返回：
//   - *http.ServeMux: 装配完成的 HTTP 路由复用器。
//   - *Deps: 真实后端依赖集合。
//   - func(): 清理函数，调用后释放资源。
//   - error: 初始化错误，成功时为 nil。
func BuildHandler(ctx context.Context, cfgPath, rolePath, envPath, soulPath, skillPath string) (*http.ServeMux, *Deps, func(), error) {
	// 严格启动：任意配置文件不存在即失败，明确告知缺失项。
	for path, name := range map[string]string{
		cfgPath:   "config file",
		rolePath:  "roles file",
		envPath:   "env file",
		soulPath:  "soul file",
		skillPath: "skills file",
	} {
		// 路径为空则直接返回错误，避免传入空路径导致后续产生含糊错误。
		if path == "" {
			return nil, nil, nil, fmt.Errorf("%s path is required", name)
		}
		// 检查文件是否存在；os.Stat 出错时返回清晰的缺失提示。
		if _, err := os.Stat(path); err != nil {
			return nil, nil, nil, fmt.Errorf("%s not found: %s", name, path)
		}
	}

	// 委托 bootstrap.Build 完成完整的后端依赖装配。
	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath: cfgPath,
		RolePath:   rolePath,
		EnvPath:    envPath,
		SoulPath:   soulPath,
		ProfilePath: "config/user_profile.md",
		SkillPath:  skillPath,
	})
	if err != nil {
		// 装配失败时直接透传错误；此时资源已由 bootstrap.Build 自行清理。
		return nil, nil, nil, err
	}

	// 将 App 中的依赖映射到 Deps，方便测试按字段访问。
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

	// cleanup 封装 App.Close，供调用方 defer 释放全部后端资源。
	cleanup := func() {
		_ = app.Close()
	}

	// 使用 bootstrap.NewDefaultMux 挂载全部 API 路由。
	mux := bootstrap.NewDefaultMux(app)
	return mux, deps, cleanup, nil
}
