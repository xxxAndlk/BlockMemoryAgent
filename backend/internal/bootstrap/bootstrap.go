// Package bootstrap 集中管理后端依赖装配逻辑。
//
// 此前该逻辑分散在 backend/main.go、backend/cmd/tui/main.go 与
// internal/testserver 中；本包统一构建一个 App 实例，其导出字段是
// HTTP/TUI/测试入口需要消费的稳定接口，而 graph/runtime registry 等
// 内部细节保持未导出，防止上层依赖 bootstrap 内部实现。
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

// ConfigPaths 汇总启动后端所需的文件系统路径。
//
// 其中 ConfigPath、RolePath、EnvPath 为必填项；SoulPath 与 SkillPath 为
// 可选项：SoulPath 为空时回退到空 Persona，SkillPath 为空时回退到内置
// 技能池。
type ConfigPaths struct {
	ConfigPath string // 主配置文件路径（如 config/config.yaml）
	RolePath   string // 角色配置文件路径（如 config/roles.yaml）
	EnvPath    string // 环境变量文件路径（如 backend/.env）
	SoulPath   string // 人格文件路径（如 config/soul.md），可为空
	SkillPath  string // 技能文件路径（如 config/skills.yaml），可为空
	// LogWriter 是会话结构化日志的可选输出目标；为 nil 时日志写入 os.Stderr。
	LogWriter io.Writer
}

// App 是 bootstrap.Build 的返回结果。
//
// 导出字段是 HTTP/TUI/测试调用方需要的对外接口；内部装配细节保持未导出，
// 避免上层代码直接依赖 bootstrap 的内部实现。
//
// 在 ReAct 重构过程中，旧的 graph 相关字段已被移除；Runtime 字段保留，
// 用于兼容 soul/skills/watchdog 等 API。
type App struct {
	Agent        agent.Agent               // ReAct Agent 服务，负责会话执行逻辑
	Server       *server.SessionManager    // HTTP 会话管理器，处理 /api/sessions/* 请求
	DAGHandler   *server.DAGHandler        // DAG HTTP 处理器
	Runtime      *runtime.Runtime          // 运行时聚合器（人格、skill、watchdog 等）
	Postgres     *store.PostgresStore      // PostgreSQL 持久化存储
	Redis        *store.RedisStore         // Redis 缓存/消息存储
	ModelFactory *model.ModelFactory       // 模型工厂，管理各角色模型连接
	Embedder     embed.Embedder            // 向量嵌入器，用于记忆检索
	Config       *config.Config            // 应用运行时配置
	RoleConfig   *pkgconfig.RoleConfigFile // 角色配置对象
	DAGScheduler *dag.Scheduler            // DAG 调度器；DAG 关闭时为 nil
	Logger       *logger.Logger            // 结构化会话日志器

	// cleanup 保存 App 关闭时需要按逆序释放的资源。
	cleanup []func() error
}

// Close 按 Build 构造的逆序释放所有资源。
//
// 返回值：若任一清理函数报错，返回第一个错误；否则返回 nil。
func (a *App) Close() error {
	var firstErr error
	// 逆序遍历 cleanup 切片，确保后构造的资源先释放。
	for i := len(a.cleanup) - 1; i >= 0; i-- {
		// 执行第 i 个清理函数；只在首次遇到错误时记录，后续错误被忽略。
		if err := a.cleanup[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Build 按与原 main.go/testserver 一致的初始化顺序装配后端：
// env -> config -> stores -> models -> agent -> session manager。
//
// ReAct 引擎是当前的默认执行路径。返回的 App 拥有其构造出的全部资源，
// 调用方在关闭时必须调用 App.Close。
//
// 参数：
//   - ctx: 用于底层初始化（如数据库连接、模型预热）的上下文。
//   - paths: 启动所需的文件路径集合。
//
// 返回：装配完成的 *App，或初始化过程中的错误。
func Build(ctx context.Context, paths ConfigPaths) (*App, error) {
	// 第一步：校验必填路径是否存在且可访问。
	if err := validatePaths(paths); err != nil {
		return nil, err
	}

	// 第二步：加载 .env 文件，将环境变量注入进程。
	if err := config.LoadEnvFile(paths.EnvPath); err != nil {
		return nil, fmt.Errorf("load env file %s: %w", paths.EnvPath, err)
	}

	// 第三步：加载主配置文件。
	cfg, err := config.Load(paths.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", paths.ConfigPath, err)
	}

	// 第四步：加载角色配置文件。
	roleCfg, err := pkgconfig.LoadRoleConfig(paths.RolePath)
	if err != nil {
		return nil, fmt.Errorf("load roles %s: %w", paths.RolePath, err)
	}

	// 第五步：连接 PostgreSQL 并配置向量维度、记忆检索 token 上限。
	pgStore, err := store.NewPostgresStore(ctx, cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("connect postgres with DSN from %s: %w", paths.ConfigPath, err)
	}
	pgStore.SetEmbeddingDim(cfg.PgVector.Dimensions)
	pgStore.SetSearchBlockMemoryMaxTokens(cfg.Agent.SearchBlockMemoryMaxTokens)

	// 第六步：构造 embedder；失败时关闭已连接的 pgStore。
	embedder, err := embed.NewEmbedder(roleCfg.Embed, cfg.PgVector.Dimensions)
	if err != nil {
		pgStore.Close()
		return nil, fmt.Errorf("create embedder: %w", err)
	}
	pgStore.SetEmbedder(embedder)

	// 第七步：确保所需数据库 schema 已就绪。
	if err := ensureSchemas(ctx, pgStore, cfg.PgVector.Dimensions); err != nil {
		pgStore.Close()
		return nil, err
	}

	// 第八步：连接 Redis；失败时关闭 pgStore。
	redisStore, err := store.NewRedisStore(ctx, cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		pgStore.Close()
		return nil, fmt.Errorf("connect redis with addr from %s: %w", paths.ConfigPath, err)
	}

	// 第九步：创建结构化会话日志器。
	sessionLogger := logger.New(pgStore, paths.LogWriter)

	// 第十步：创建模型工厂并预热、校验连通性。
	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("warmup models: %w", err)
	}
	if err := modelFactory.VerifyConnectivity(ctx); err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("verify LLM connectivity: %w", err)
	}

	// 第十一步：加载技能池；未指定路径时使用内置技能池。
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

	// 第十二步：创建共享邮箱，供 Runtime（遗留 API 兼容）与 ReAct 子 Agent 调度器共同使用。
	sharedMailbox := mailbox.New()

	// 第十三步：构造运行时，注入共享邮箱与技能池。
	rt, err := runtime.New(paths.SoulPath, skillPool, runtime.WithMailbox(sharedMailbox))
	if err != nil {
		closeStores(pgStore, redisStore)
		return nil, fmt.Errorf("init runtime: %w", err)
	}
	rt.SetAgentConfig(&cfg.Agent)

	// 第十四步：装配 ReAct 引擎依赖。
	// 获取当前工作目录，用于工具注册表定位工作区；失败时回退到 "."。
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	roleRegistry := role.NewRegistry(roleCfg)                         // 角色注册表
	toolRegistry := tool.NewBuiltinRegistry(workDir, &cfg.Agent, nil) // 内置工具注册表
	memoryPipeline := memory.NewPipeline(memory.NewInMemoryStore())   // 记忆流水线

	// 第十五步：创建子 Agent 调度器，并注册工具调用能力。
	subAgentDispatcher := subagent.NewDispatcher(roleRegistry, &reactModelFactory{modelFactory}, toolRegistry, sharedMailbox, memoryPipeline)
	subAgentDispatcher.RegisterCallTool(toolRegistry)

	// 第十六步：创建 ReAct Agent 服务。
	agentSvc := agent.NewReactService(roleRegistry, modelFactory, toolRegistry, sharedMailbox, memoryPipeline, pgStore)

	// 第十七步：创建 HTTP SessionManager 并注入依赖。
	sessionMgr := server.NewSessionManager(agentSvc)
	sessionMgr.SetPostgresStore(pgStore)
	sessionMgr.SetModelFactory(modelFactory)

	// 第十八步：若 DAG 功能开启，创建并启动 DAG 调度器。
	var dagScheduler *dag.Scheduler
	if cfg.Agent.DAGEnabled {
		dagScheduler = dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
		dagScheduler.Start(ctx)
	}

	// 第十九步：创建 DAG HTTP 处理器。
	dagHandler := server.NewDAGHandler(pgStore, dagScheduler)

	// 第二十步：组装 App 实例。
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

	// 第二十一步：注册关闭时释放资源的回调，按依赖顺序排列（外层 Close 会逆序调用）。
	app.cleanup = []func() error{
		func() error {
			// 先停止 DAG 调度器，避免在数据库关闭后还在调度任务。
			if dagScheduler != nil {
				dagScheduler.Stop()
			}
			return nil
		},
		func() error { redisStore.Close(); return nil }, // 关闭 Redis 连接
		func() error { pgStore.Close(); return nil },    // 最后关闭 PostgreSQL 连接
	}

	return app, nil
}

// validatePaths 校验启动所需的文件路径。
//
// 必填项 ConfigPath、RolePath、EnvPath 必须非空且文件存在；
// SoulPath 与 SkillPath 可选：若提供则必须存在。
func validatePaths(paths ConfigPaths) error {
	// 遍历必填项，检查路径是否非空且文件可访问。
	for path, name := range map[string]string{
		paths.ConfigPath: "config file",
		paths.RolePath:   "roles file",
		paths.EnvPath:    "env file",
	} {
		// 路径为空时直接返回错误。
		if path == "" {
			return fmt.Errorf("%s path is required", name)
		}
		// 文件不存在或无法访问时返回错误。
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s not found: %s", name, path)
		}
	}
	// SoulPath 与 SkillPath 可选：空值时 runtime.New 使用空 Persona，skill 使用 BuiltinPool。
	if paths.SoulPath != "" {
		// 若提供了 soul 文件，则校验其存在性。
		if _, err := os.Stat(paths.SoulPath); err != nil {
			return fmt.Errorf("soul file not found: %s", paths.SoulPath)
		}
	}
	if paths.SkillPath != "" {
		// 若提供了 skills 文件，则校验其存在性。
		if _, err := os.Stat(paths.SkillPath); err != nil {
			return fmt.Errorf("skills file not found: %s", paths.SkillPath)
		}
	}
	return nil
}

// ensureSchemas 确保业务所需的数据库 schema 与向量维度正确。
//
// 参数：
//   - ctx: 数据库操作的上下文。
//   - pgStore: PostgreSQL 存储对象，提供底层 *sql.DB。
//   - expectedDim: 配置中声明的嵌入向量维度。
func ensureSchemas(ctx context.Context, pgStore *store.PostgresStore, expectedDim int) error {
	// 依次确保各业务表/扩展已创建。
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
	// 校验数据库中的 embedding 维度与配置一致。
	if err := store.ValidateEmbeddingDimension(ctx, pgStore.DB(), expectedDim); err != nil {
		return fmt.Errorf("validate embedding dimension: %w", err)
	}
	return nil
}

// closeStores 按 Redis -> PostgreSQL 的顺序关闭存储连接。
// 在 Build 中途出错时用于快速释放已分配资源。
func closeStores(pg *store.PostgresStore, redis *store.RedisStore) {
	redis.Close()
	pg.Close()
}

// reactModelFactory 将 model.ModelFactory 适配为子 Agent 调度器期望的
// 更窄的模型提供者工厂接口。
type reactModelFactory struct {
	inner *model.ModelFactory
}

// GetBladesProvider 委托给内部 ModelFactory 获取指定角色的模型提供者。
//
// 参数：
//   - ctx: 请求上下文。
//   - roleID: 角色标识。
//
// 返回：该角色对应的 agent.ModelProvider，或获取过程中的错误。
func (f *reactModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	return f.inner.GetBladesProvider(ctx, roleID)
}
