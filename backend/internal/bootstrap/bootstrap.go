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
	"path/filepath"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/domain/memory"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/domain/verifyloop"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
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
	sessionLogger := logger.NewWithConfig(cfg.Logging, pgStore, paths.LogWriter)
	// 同步注入存储层，让反序列化失败等错误日志以 [ERRO] 级别输出。
	pgStore.SetLogger(sessionLogger)

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
	// 向指令队列注入日志器，丢弃指令等错误以 [ERRO] 输出。
	rt.CmdQueue.SetLogger(sessionLogger)

	// 第十四步：装配 ReAct 引擎依赖。
	// 获取当前工作目录，用于工具注册表定位工作区；失败时回退到 "."。
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	// 确保 workspace/ 存在：Agent 的用户产物落点（guards 仅允许写 workspace/ 子树）。
	// 缺失会导致 ListDir workspace 失败、子 Agent 误写到 web/ 等受保护目录。
	// 幂等：已存在时 MkdirAll 不报错。
	if mkErr := os.MkdirAll(filepath.Join(workDir, "workspace"), 0o755); mkErr != nil {
		return nil, fmt.Errorf("create workspace dir: %w", mkErr)
	}
	roleRegistry := role.NewRegistry(roleCfg)                         // 角色注册表
	toolRegistry := tool.NewBuiltinRegistry(workDir, &cfg.Agent, nil) // 内置工具注册表
	// 把 yaml 中的 tool_sandbox_* 配置真正注入 Executor；否则 SafetyConfig 是死配置，
	// Executor 永远跑 DefaultSandboxConfig（默认禁写工作目录外、保留命令黑名单）。
	toolRegistry.SetSandboxConfig(&cfg.Agent.SafetyConfig)
	memoryPipeline := memory.NewPipeline(memory.NewInMemoryStore()).WithSummarizer(newEventSummarizer(modelFactory))   // 记忆流水线

	// 第十五步：创建子 Agent 调度器，并注册工具调用能力。
	// 超时与循环参数从 cfg.Agent 派生（负数表示不限制，由 loopConfig 归一为 0）。
	reactCfg := agent.ReactRuntimeConfig{
		MaxIterations:             cfg.Agent.ToolCallMaxRounds,
		LLMTimeoutSec:             cfg.Agent.ReactLLMTimeoutSec,
		RetryCount:                cfg.Agent.RetryCount,
		RetryBackoffMs:            cfg.Agent.RetryBackoffMs,
		HistoryMaxMessages:        cfg.Agent.HistoryMaxMessages,
		ToolOutputHistoryMaxRunes: cfg.Agent.ToolOutputHistoryMaxRunes,
		SummarizeEvery:            cfg.Agent.SummarizeEvery,
		SummarizeKeepRecent:       cfg.Agent.SummarizeKeepRecent,
	}
	subAgentTimeout := time.Duration(cfg.Agent.SubAgentTimeoutMin) * time.Minute
	if subAgentTimeout < 0 {
		subAgentTimeout = 0 // 负数表示不限制
	}
	subAgentDispatcher := subagent.NewDispatcher(roleRegistry, &reactModelFactory{modelFactory}, toolRegistry, sharedMailbox, memoryPipeline)
	subAgentDispatcher.WithTimeout(subAgentTimeout).WithLoopConfig(reactCfg.LoopConfig()).WithBlockMemorySearcher(pgStore)
	// 块记忆写入闭环：默认开启（applyFeatureTogglesDefaults 兜底为 true）；
	// 显式 block_memory_write_enabled: false 时 Dispatcher 内部跳过沉淀。
	subAgentDispatcher.WithBlockMemorySaver(&blockMemorySaver{pg: pgStore}, cfg.Agent.BlockMemoryWriteEnabled == nil || *cfg.Agent.BlockMemoryWriteEnabled)
	// 全局派发总数上限：单 session 所有角色派发合计超限拒绝；用户新消息重置。
	subAgentDispatcher.WithMaxTotalDispatches(cfg.Agent.MaxTotalDispatches)
	// Spec 强制：开启时 call_sub_agent 前必须先 WriteSpec(goal, acceptance, ...)，
	// dispatcher 校验 parentID:spec 存在且新鲜，缺失则拒绝派发。默认 true。
	if cfg.Agent.SpecEnforcementEnabled != nil {
		subAgentDispatcher.WithSpecEnforcement(*cfg.Agent.SpecEnforcementEnabled)
	} else {
		subAgentDispatcher.WithSpecEnforcement(true)
	}
	// 共享记忆/spec：文件后端落盘到 <workDir>/.bma/shared/<hex(agentID)>__<slot>.md。
	// 主线程 Agent（meta/domain）持可写实例写关键上下文与 spec，
	// 子 Agent 派发时经 Dispatcher 的只读视图读取并注入任务前。
	// 文件 MD 格式：frontmatter 含 agent/slot/files mtime（+ spec 的 goal/acceptance/constraints），
	// body 为人读文本。进程重启后文件保留（当前会话不主动恢复，避免旧 spec 复用）。
	sharedKV := tool.NewFileSharedMemoryStore(workDir)
	subAgentDispatcher.WithKVMemory(sharedKV)
	// 注入会话级日志器：使子 Agent LLM I/O（完整 prompt/response）写入 session_logs，
	// 与 MetaAgent 共用同一 sessionLogger 基础实例，子 Agent 运行时按 ctx 派生 session-scoped 视图。
	subAgentDispatcher.WithLogger(sessionLogger)
	// 把同一 sharedKV 注入工具注册表，使 WriteSharedMemory/WriteSpec 工具能写入；
	// MetaAgent 在派发复杂任务前调用 WriteSharedMemory 写入关键上下文，
	// 子 Agent 经 dispatcher.injectKVMemory 自动读取。
	toolRegistry.SetSharedMemory(sharedKV)
	subAgentDispatcher.RegisterCallTool(toolRegistry)
	// 注册 send_message 工具：支持任意 Agent 向另一个 Agent 实例邮箱投递消息，
	// 是多 Agent 协作验证闭环（代码 Agent <-> 测试 Agent 互问互答）的基础原语。
	subAgentDispatcher.RegisterMessagingTool(toolRegistry)

	// verifyloop 编排器：原生驱动"代码->自测->修正->上级统一测试"状态机。
	// AssistantSelfTestEnabled 开启时，code_assistant 异步完成后自动触发编排器，
	// 不再依赖主 Agent 提示词自觉。编排器通过 ExecuteChild 同步驱动子 Agent，
	// 不触发 onSubAgentDone 钩子，避免修正轮递归自测。
	// verifyloop 编排器：原生驱动"自测->修正->上级统一测试"状态机。
	// AssistantSelfTestEnabled 开启 code_assistant 等产出角色的自测；
	// DomainSelfTestEnabled 开启 domain 角色的模块级统一测试。
	// 两者共享 OnSubAgentDone 钩子，按 config.VerificationRolePairs 匹配角色对触发。
	// 角色对默认 [{code_assistant, test_assistant}]；DomainSelfTestEnabled 开启时追加 {domain, test_assistant}。
	// 需 ComputerUse/CLI/MCP 验证器时改用 NewWith 自定义装配。
	selfTestEnabled := cfg.Agent.AssistantSelfTestEnabled || cfg.Agent.DomainSelfTestEnabled
	if selfTestEnabled {
		// 合并配置角色对与 domain 运行时角色对。
		pairs := make([]config.VerificationRolePair, 0, len(cfg.Agent.VerificationRolePairs)+1)
		pairs = append(pairs, cfg.Agent.VerificationRolePairs...)
		if cfg.Agent.DomainSelfTestEnabled {
			// domain 产出的模块级验证：用 test_assistant 做上级统一测试。
			// 避免重复追加用户已显式配置的 domain 对。
			dup := false
			for _, p := range pairs {
				if p.CodeRole == "domain" {
					dup = true
					break
				}
			}
			if !dup {
				pairs = append(pairs, config.VerificationRolePair{CodeRole: "domain", TestRole: "test_assistant"})
			}
		}
		// 仅保留开关启用的角色对：AssistantSelfTestEnabled 控制 code_assistant 等，
		// DomainSelfTestEnabled 控制 domain。
		activePairs := pairs[:0]
		for _, p := range pairs {
			if p.CodeRole == "domain" {
				if cfg.Agent.DomainSelfTestEnabled {
					activePairs = append(activePairs, p)
				}
				continue
			}
			if cfg.Agent.AssistantSelfTestEnabled {
				activePairs = append(activePairs, p)
			}
		}
		// 按 codeRole 索引角色对，供钩子快速查找。
		rolePairByCode := make(map[string]config.VerificationRolePair, len(activePairs))
		for _, p := range activePairs {
			rolePairByCode[p.CodeRole] = p
		}
		// 每个角色对独立编排器实例（AgentVerifier/Fixer 绑定各自角色）。
		// ReviewEnabled 开启时装配 code_reviewer 作为 Reviewer，插入 PlanConfirm 与 SelfTest 之间。
		// PlanSkipEnabled 默认 true（PlanConfirm 整体移除中）；显式 false 仍走 PlanConfirm。
		reviewRole := ""
		if cfg.Agent.ReviewEnabled == nil || *cfg.Agent.ReviewEnabled {
			reviewRole = "code_reviewer"
		}
		planSkip := true
		if cfg.Agent.PlanSkipEnabled != nil {
			planSkip = *cfg.Agent.PlanSkipEnabled
		}
		orchestrators := make(map[string]*verifyloop.Orchestrator, len(activePairs))
		for _, p := range activePairs {
			orchestrators[p.CodeRole] = verifyloop.NewWithReviewer(subAgentDispatcher, sharedMailbox, cfg.Agent.VerificationMaxRounds, p.CodeRole, p.TestRole, reviewRole, planSkip)
		}
		subAgentDispatcher.SetOnSubAgentDone(func(parentID, subAgentID, roleID, task, resultText string) {
			pair, ok := rolePairByCode[roleID]
			if !ok {
				return // 该角色无验证角色对配置或开关未开，不触发。
			}
			o, ok := orchestrators[pair.CodeRole]
			if !ok {
				return
			}
			// 异步运行验证闭环，避免阻塞 call_sub_agent 的 notify 路径。
			// Run 内部已调用 Reporter.Report 投递结果到父邮箱。
			go o.Run(ctx, verifyloop.Request{
				ParentID:    parentID,
				ProducerID:  subAgentID,
				InitialTask: task,
				Produced:    resultText,
			})
		})
	}

	// 第十六步：创建 ReAct Agent 服务。
	agentSvc := agent.NewReactService(roleRegistry, modelFactory, toolRegistry, sharedMailbox, memoryPipeline, pgStore)
	agentSvc.SetLogger(sessionLogger)
	agentSvc.SetRuntimeConfig(reactCfg)
	// 注入子 Agent 实时事件转发器：子 Agent token 用量/流式增量按 sessionID 路由回会话 service，
	// 使 TUI/Web 看到所有 Agent（含子 Agent）的累计 token。
	subAgentDispatcher.WithLiveEvents(agentSvc.ForwardLiveEvent)
	// 注入未决子 Agent 检查器，开启父会话终结保护：
	// 父 Agent 给出终答前若有未决子 Agent，阻塞等待其完成，防止迟到 mailbox 消息丢失。
	agentSvc.SetPendingChildrenChecker(subAgentDispatcher)
	// 默认恢复历史会话：从 session_history 恢复最近 50 个会话到内存，
	// 保证重启后长任务上下文可见；显式 restore_sessions: false 关闭。
	// 恢复失败仅记录日志，不阻断启动。
	if cfg.Agent.RestoreSessions == nil || *cfg.Agent.RestoreSessions {
		agentSvc.RestoreSessions(ctx, 50)
	}

	// 第十七步：创建 HTTP SessionManager 并注入依赖。
	sessionMgr := server.NewSessionManager(agentSvc)
	sessionMgr.SetLogger(sessionLogger)
	sessionMgr.SetPostgresStore(pgStore)
	sessionMgr.SetModelFactory(modelFactory)

	// 第十八步：若 DAG 功能开启，创建并启动 DAG 调度器。
	var dagScheduler *dag.Scheduler
	if cfg.Agent.DAGEnabled {
		dagScheduler = dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
		dagScheduler.SetLogger(sessionLogger)
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

// newEventSummarizer 构造一个 memory.EventSummarizer，把近期事件列表交给轻量模型压成短摘要。
// 失败时返回错误，由 Pipeline 降级为直接 join 原始事件，主流程不受影响。
func newEventSummarizer(f *model.ModelFactory) memory.EventSummarizer {
	if f == nil {
		return nil
	}
	return func(ctx context.Context, events []string) (string, error) {
		// 拼装提示词：要求保留时间顺序与关键事实，丢弃冗余 I/O 细节。
		prompt := "将以下 ReAct Agent 近期事件按时间顺序压成 200 字以内的紧凑摘要，" +
			"保留：调用过哪些工具（工具名+关键参数）、关键产出文件路径、子 Agent 摘要要点。" +
			"丢弃：冗长输出、重复读文件、空响应 nudge。直接输出摘要，不要解释：\n" +
			strings.Join(events, "\n")
		return f.CallLightweightWithRetry(ctx, prompt)
	}
}

// blockMemorySaver 将 store.PostgresStore 适配为子 Agent 调度器期望的
// 块记忆写入接口（subagent.BlockMemorySaver）：
// 先用 PostgresStore.Embed 为记录内容生成向量，再委托 SaveKnowledge 落库，
// 保证写入的块记忆带有效 embedding（global_knowledge.embedding 为 VECTOR(N) 列，
// 空向量无法插入），可被后续 SearchBlockMemoryByGoal 语义召回命中。
type blockMemorySaver struct {
	pg *store.PostgresStore
}

// Save 实现 subagent.BlockMemorySaver 接口。
//
// 参数：
//   - ctx: 请求上下文。
//   - rec: 待写入的块记忆记录（Content/Meta/KnowledgeType 已由调用方填充）。
//
// 返回：向量化或落库过程中的错误。
func (s *blockMemorySaver) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	// 生成与检索侧同源的查询向量（Embed 内部走同一 embedder / PseudoEmbed 回退）。
	emb, err := s.pg.Embed(ctx, rec.Content)
	if err != nil {
		return fmt.Errorf("embed block memory: %w", err)
	}
	rec.Embedding = emb
	// 复用既有 SaveKnowledge（委托 KnowledgeStore.Save），不新增存储路径。
	return s.pg.SaveKnowledge(ctx, rec)
}
