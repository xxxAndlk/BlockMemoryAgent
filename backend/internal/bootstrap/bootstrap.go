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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
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
	"github.com/blockmemory/agent/backend/internal/plugins"
	"github.com/blockmemory/agent/backend/internal/plugins/mcpbridge"
	"github.com/blockmemory/agent/backend/internal/retriever"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// ConfigPaths 汇总启动后端所需的文件系统路径。
//
// 其中 ConfigPath、RolePath、EnvPath 为必填项；SoulPath 与 SkillPath 为
// 可选项：SoulPath 为空时回退到空 Persona，SkillPath 为空时回退到内置
// 技能池。
type ConfigPaths struct {
	ConfigPath  string // 主配置文件路径（如 config/config.yaml）
	RolePath    string // 角色配置文件路径（如 config/roles.yaml）
	EnvPath     string // 环境变量文件路径（如 backend/.env）
	SoulPath    string // 人格文件路径（如 config/soul.md），可为空
	ProfilePath string // 用户画像文件路径（如 config/user_profile.md，TODO #28），可为空
	SkillPath   string // 技能文件路径（如 config/skills.yaml），可为空
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
	Plugins      *plugins.Manager         // 插件管理器（热插拔插件，设计文档《插件系统设计 v2》）

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
	// 启动即打印轻量模型解析结果（来源 direct / fallback-domain），排查配置加载漂移：
	// 2026-08-10 事故 roles.yaml 配 deepseek-v4-flash 但运行时 provider=glm-5.2（domain 回退），
	// 会话期生效配置与磁盘现值不一致（配置晚于会话加载 / CWD 路径漂移），启动日志当场暴露。
	if lmCfg, lmSource := modelFactory.LightweightResolution(); lmSource == "direct" {
		log.Printf("[bootstrap] lightweight model resolution: model=%q provider=%q base_url=%q source=direct (roles.yaml lightweight_model 段)",
			lmCfg.Model, lmCfg.Provider, lmCfg.BaseURL)
	} else {
		log.Printf("[bootstrap] lightweight model resolution: source=fallback-domain model=%q provider=%q（roles.yaml 未配 lightweight_model，轻量调用回退 domain 模型——若预期为独立轻量模型请配置）",
			lmCfg.Model, lmCfg.Provider)
	}
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
	// workDir 本身即沙箱：Agent 直接在用户项目目录内读写，不再创建 workspace/ 子区。
	// guards 仅挡 VCS/IDE/构建产物目录；sandbox.go 拦截路径逃逸 workDir。
	roleRegistry := role.NewRegistry(roleCfg)
	toolRegistry := tool.NewBuiltinRegistry(workDir, &cfg.Agent, nil) // 内置工具注册表
	// 把 yaml 中的 tool_sandbox_* 配置真正注入 Executor；否则 SafetyConfig 是死配置，
	// Executor 永远跑 DefaultSandboxConfig（默认禁写工作目录外、保留命令黑名单）。
	toolRegistry.SetSandboxConfig(&cfg.Agent.SafetyConfig)
	// 注入角色级写路径解析器（Layer 4）：roleID -> 该角色 Sandbox.AllowedWritePaths。
	// 角色未配 sandbox.allowed_write_paths 时返 nil，enforceRoleWritePath 跳过，现有写行为不变。
	toolRegistry.SetRoleWritePathResolver(func(roleID string) []string {
		rd := roleRegistry.Get(roleID)
		if rd == nil || rd.Sandbox == nil {
			return nil
		}
		return rd.Sandbox.AllowedWritePaths
	})
	// 注入 LLM 领域分区器：RefreshProjectDoc 工具（MetaAgent 侧）与 EnsureProjectDoc（首 session）
	// 均调轻量模型读文件样本按职责/实体分区（如"游戏运行时""炮塔实体"）；失败/超限回退启发式依赖图兜底，永不留空标注。
	cls := &llmDomainClassifier{factory: modelFactory}
	toolRegistry.SetDomainClassifier(cls)
	memoryPipeline := memory.NewPipeline(memory.NewPostgresEventStore(pgStore.DB())).
		WithSummarizer(newEventSummarizer(modelFactory)).
		WithSummarizeTimeout(time.Duration(cfg.Agent.SummarizeTimeoutSec)*time.Second). // 思考型模型摘要需 60-180s，旧 5s 硬编码致摘要全挂
		WithCompression(cfg.Agent.SummarizeEvery, cfg.Agent.SummarizeKeepRecent). // 记忆流水线
		WithContextBudget(cfg.Agent.ContextTokenBudget, cfg.Agent.TokenBudgetPerRole). // 上下文 token 阈值触发压缩（默认 150K，保留近 10 旧压成摘要块）
		WithTokenEstimator(agent.EstimateMessagesTokens) // 注入消息 token 估算器，避免 domain/memory 反向依赖 model

	// 第十五步：创建子 Agent 调度器，并注册工具调用能力。
	// 超时与循环参数从 cfg.Agent 派生（负数表示不限制，由 loopConfig 归一为 0）。
	reactCfg := agent.ReactRuntimeConfig{
		MaxIterations:             cfg.Agent.ToolCallMaxRounds,
		LLMTimeoutSec:             cfg.Agent.ReactLLMTimeoutSec,
		RetryCount:                cfg.Agent.RetryCount,
		RetryBackoffMs:            cfg.Agent.RetryBackoffMs,
		HistoryMaxMessages:        cfg.Agent.HistoryMaxMessages,
		ToolOutputHistoryMaxRunes: cfg.Agent.ToolOutputHistoryMaxRunes,
		TokenBudgetPerGoal:        cfg.Agent.TokenBudgetPerGoal,
		TokenBudgetPerRole:        cfg.Agent.TokenBudgetPerRole,
		ContextTokenBudget:        cfg.Agent.ContextTokenBudget,
		SessionMaxWallClockMin:    cfg.Agent.SessionMaxWallClockMin,
	}
	subAgentTimeout := time.Duration(cfg.Agent.SubAgentTimeoutMin) * time.Minute
	if subAgentTimeout < 0 {
		subAgentTimeout = 0 // 负数表示不限制
	}
	subAgentDispatcher := subagent.NewDispatcher(roleRegistry, &reactModelFactory{modelFactory}, toolRegistry, sharedMailbox, memoryPipeline)
	// 心跳检活：叶子 Agent 超过该时长无 generateOnce/工具派发判定假死，巡检主动 cancel+notify 父，
	// 比等满 sub_agent_timeout（60min）早暴露第二次 session 卡死（LLM 流式挂起等）。<=0 关闭。
	subAgentHeartbeat := time.Duration(cfg.Agent.SubAgentHeartbeatTimeoutMin) * time.Minute
	subAgentDispatcher.WithTimeout(subAgentTimeout).WithHeartbeatTimeout(subAgentHeartbeat).WithLoopConfigByRole(reactCfg.LoopConfigByRole).WithBlockMemorySearcher(pgStore)
	// DomainAgent 心跳（TODO #25-3 防误杀版）：默认 2× 叶子；等子/等回信靠后代活动冒泡保活。
	subAgentDispatcher.WithDomainHeartbeatTimeout(time.Duration(cfg.Agent.DomainHeartbeatTimeoutMin) * time.Minute)
	// Paused domain 续跑次数上限：续跑重置 fresh budget，无上限则研磨环路永不绑定；
	// 触顶后强制收口部分返回父 Agent（默认 1，cfg.Agent.PausedDomainMaxResumes 可调）。
	subAgentDispatcher.WithMaxPausedResumes(cfg.Agent.PausedDomainMaxResumes)
	// 派发 task 文本双档上限（TODO #35 放开预算）：soft 软着陆警告 / hard 硬拒。
	subAgentDispatcher.WithTaskRuneLimits(cfg.Agent.TaskMaxRunes, cfg.Agent.TaskMaxRunesHard)
	// 叶子助手 kind=error 失败自动重派（TODO #23）：默认 1 次，同任务同前缀重跑。
	subAgentDispatcher.WithDispatchRetryCount(cfg.Agent.DispatchRetryCount)
	// 派发执行模式引擎参数（TODO #29 三引擎）：reflection 自检轮数 / plan_execute 最大步数。
	subAgentDispatcher.WithEngineConfig(cfg.Agent.ReflectionMaxRounds, cfg.Agent.PlanExecuteMaxSteps)
	// 引擎辅助 LLM（自检 judge/规划）单次调用超时：该路径无 CallLLM 包装，
	// 无独立超时时 provider 重试 × judge 重试叠加可烧 ~70 分钟（2026-08-19 引擎 Agent 事故）。
	if cfg.Agent.EngineLLMTimeoutSec > 0 {
		subAgentDispatcher.WithEngineLLMTimeout(time.Duration(cfg.Agent.EngineLLMTimeoutSec) * time.Second)
	}
	// 校验 judge 角色（TODO #43 交叉模型）：reflection/rubric 校验取该角色 provider 作评审
	// （与被审角色不同模型）；取不到 dispatcher 回退同角色（测试场景不破）。
	subAgentDispatcher.WithJudgeRole(cfg.Agent.JudgeRole)
	// 块记忆写入闭环：默认开启（applyFeatureTogglesDefaults 兜底为 true）；
	// 显式 block_memory_write_enabled: false 时 Dispatcher 内部跳过沉淀。
	subAgentDispatcher.WithBlockMemorySaver(&blockMemorySaver{pg: pgStore}, cfg.Agent.BlockMemoryWriteEnabled == nil || *cfg.Agent.BlockMemoryWriteEnabled)
	// 事实提取：子 Agent 完成后用轻量模型提取 1-5 条关键事实，每条单独落 KnowledgeRecord，
	// 替代原始 result.Text 整段落库。提取失败自动回退原始保存（saveBlockMemory 内部处理）。
	subAgentDispatcher.WithFactExtractor(&llmFactExtractor{factory: modelFactory})
	// 失败打捞（TODO #20 第二层）：子 Agent 失败（超时/被杀/循环守卫终止）时用轻量模型
	// 提取"已读文件清单+已得结论+卡点"摘要，写共享槽位供同域重派带前序摘要 + 追加进父 mailbox。
	// 超时配置化（TODO #33）：思考型模型首 token 数十秒，旧 5s 硬编码致打捞全超时降级。
	subAgentDispatcher.WithSalvageExtractor(&llmSalvageExtractor{factory: modelFactory}).
		WithSalvageTimeout(time.Duration(cfg.Agent.SalvageLLMTimeoutSec) * time.Second)
	// 全局派发总数上限：单 session 所有角色派发合计超限拒绝；用户新消息重置。
	subAgentDispatcher.WithMaxTotalDispatches(cfg.Agent.MaxTotalDispatches)
	// Spec 强制：开启时 call_sub_agent 前必须先 WriteSpec(goal, acceptance, ...)，
	// dispatcher 校验 parentID:spec 存在且新鲜，缺失则拒绝派发。config 层默认 true，
	// config.yaml 可显式关闭；applyDefaults 保证非 nil，else 分支为防御。
	subAgentDispatcher.WithSpecEnforcement(cfg.Agent.SpecEnforcementEnabled == nil || *cfg.Agent.SpecEnforcementEnabled)
	// 共享记忆/spec：文件后端落盘到 <workDir>/.bma/shared/<hex(agentID)>__<slot>.md。
	// 主线程 Agent（meta/domain）持可写实例写关键上下文与 spec，
	// 子 Agent 派发时经 Dispatcher 的只读视图读取并注入任务前。
	// 文件 MD 格式：frontmatter 含 agent/slot/files mtime（+ spec 的 goal/acceptance/constraints），
	// body 为人读文本。进程重启后文件保留（当前会话不主动恢复，避免旧 spec 复用）。
	sharedKV := tool.NewFileSharedMemoryStore(workDir)
	subAgentDispatcher.WithSharedMemory(sharedKV)
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
	// 注册 cancel_agent 工具（TODO #25 控制面）：MetaAgent/DomainAgent 主动取消跑偏子 Agent。
	subAgentDispatcher.RegisterControlTool(toolRegistry)
	// 注册 create_role / list_roles 工具：让 MetaAgent 运行时注册动态角色。
	// 仅 MetaAgent 的 Tools 白名单含这两个工具（registry.go meta 角色 Tools 字段），
	// 其他角色看不到它们。Registry 持 closure 引用，Execute 直接读写 dynamic 层。
	roleRegistry.RegisterTools(toolRegistry)

	// 第十六步：创建 ReAct Agent 服务。
	agentSvc := agent.NewReactService(roleRegistry, modelFactory, toolRegistry, sharedMailbox, memoryPipeline, pgStore)
	// 串联权威 workDir：bootstrap 持有的 os.Getwd() 结果注入 session store，
	// 消除 newReactSessionStore 内不再自取 cwd 的双源漂移。
	agentSvc.SetWorkDir(workDir)
	// 注入 LLM 领域分区器：首 session 缺失 PROJECT.md 时 EnsureProjectDoc 调 cls.Partition
	// 读文件按职责分区生成；后续 session 幂等跳过（文件已存在）。
	agentSvc.SetDomainClassifier(cls)
	agentSvc.SetLogger(sessionLogger)
	agentSvc.SetRuntimeConfig(reactCfg)
	// 注入 Agent 树持久化层：Register/Finish/Cancel 后 best-effort 写入 PG,
	// 服务重启后 TreeFor lazy init 调 LoadFromStore 恢复历史节点(元数据恢复)。
	agentSvc.SetTreeStore(store.NewPostgresTreeStore(pgStore.DB()))
	// 注入任务看板（TODO #22 执行计划）：write_plan 写板、派发依赖门、TUI 面板真相源。
	// 看板按 sessionID 惰性创建（write_plan 首次调用 GetOrCreate）。
	agentSvc.SetBoard(rt.Boards.Get)
	// 用户输入自动提示词补全（TODO #36 Phase 0 规则版 + #39 四层管线）：命中续跑/控制/
	// 诊断意图时 sendMessage 附加【系统补全】段（意图标签 + 看板/树失败任务绑定），
	// 只增不改原文。L2 仲裁（#39）仅灰区触发：句首弱词/句中弱信号的短输入调轻量模型四分类，
	// 超时/失败/低置信一律普通任务直通，绝不阻塞用户输入。
	agentSvc.SetPromptEnhance(cfg.Agent.PromptEnhance == nil || *cfg.Agent.PromptEnhance)
	agentSvc.SetPromptEnhanceLLM(
		cfg.Agent.PromptEnhanceLLM == nil || *cfg.Agent.PromptEnhanceLLM,
		newIntentArbiter(modelFactory),
		time.Duration(cfg.Agent.PromptEnhanceLLMTimeoutSec)*time.Second,
		cfg.Agent.PromptEnhanceMaxInputRunes,
	)
	subAgentDispatcher.WithBoard(rt.Boards.Get, func(sid, goal string) *board.TaskBoard {
		return rt.Boards.GetOrCreate(sid, goal)
	})
	subAgentDispatcher.RegisterPlanTool(toolRegistry)
	// 注入共享记忆 KV：话题切换时把旧 Agent 树摘要写入 `topic:{id}:summary`,
	// 供新话题 MetaAgent 召回(召回注入侧步骤 4 part C 未做,摘要已落 KV)。
	agentSvc.SetSharedMemoryStore(sharedKV)
	// 注入破坏性操作审批钩子（TODO #17 P1）：命中生产边界/危险命令模式时
	// 工具调用暂停会话推「需确认」事件，用户答复经 sendMessage/answerClarify 路由回放行。
	// approvalHook 非 nil 仅影响命中边界的调用，常规编码流零阻塞。
	// 等待用户答复期间的心跳保活：审批/提问阻塞时周期性刷新子 Agent 活动时间，
	// 防"等用户操作"被巡检误判假死（等多久都不杀，直到用户答复或会话取消）。
	agentSvc.SetActivityPinger(subAgentDispatcher.PingActivity)
	toolRegistry.SetApprovalHook(agentSvc.ApprovalHook())
	// 外部知识库检索（TODO #27 热路径 a）：search_knowledge 工具 → retriever 混合检索。
	// 混合检索后端 = KnowledgeStore（直接满足 HybridSearchBackend：SearchByType + SearchKeywords）。
	// 嵌入用全局 embedder（roles.yaml embed 段；当前 pseudo，真实 embed 激活后自动升级）。
	kbRetriever := retriever.NewGlobalKnowledgeRetriever(nil, nil, embedder)
	kbRetriever.SetHybridBackend(pgStore.Knowledge)
	toolRegistry.SetKnowledgeSearchHook(func(ctx context.Context, query string, topK int) (string, error) {
		hits, err := kbRetriever.SearchHybrid(ctx, query, enums.KnowledgeTypeExternalKB, topK)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "外部知识库未命中。", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "外部知识库命中 %d 条：", len(hits))
		for i, rec := range hits {
			src, _ := rec.Meta["source"].(string)
			doc, _ := rec.Meta["doc"].(string)
			b.WriteString("\n--- ")
			fmt.Fprintf(&b, "[%d] %s", i+1, src)
			if doc != "" {
				b.WriteString(" (文档: ")
				b.WriteString(doc)
				b.WriteString(")")
			}
			b.WriteString(" ---\n")
			b.WriteString(truncateForPrompt(rec.Content, 600))
		}
		return b.String(), nil
	})
	// 注入 ask_user 工具钩子（TODO #24 人在回路）：meta/domain Agent 主动提问时
	// 置 PendingClarify + 暂停会话 + 阻塞等答复，答复作为工具结果带回 ReAct 循环。
	// 默认超时从 config.AskUserTimeoutSec 注入（0=不限），单次调用 timeout_sec 可覆盖。
	toolRegistry.SetAskUserHook(agentSvc.AskUserHook())
	toolRegistry.SetAskUserTimeoutDefault(cfg.Agent.AskUserTimeoutSec)
	// 注入子 Agent 实时事件转发器：子 Agent token 用量/流式增量按 sessionID 路由回会话 service，
	// 使 TUI/Web 看到所有 Agent（含子 Agent）的累计 token。
	subAgentDispatcher.WithLiveEvents(agentSvc.ForwardLiveEvent)
	// 人格加载器（soul.Loader）仅注入 MetaAgent：编排者人格（"多 Agent 编排助手"）
	// 首行即身份定义，子 Agent 若共享会与角色提示词（叶子=执行者/领域=领域负责人）
	// 冲突，thinking 模型据此在"等待兄弟回传"叙事里空转（实证 2026-08-13）。
	// 子 Agent 身份由 roles.yaml 角色提示词定义，不再下发人格。
	agentSvc.SetPersonaInjector(rt.Soul)
	// 用户画像（TODO #28 第四层记忆）：单文件 user_profile.md，人可编辑 + 程序结构化追加。
	// 注入仅 MetaAgent（metaPersona 组合人格+画像），子 Agent 不下发（防上下文膨胀/偏好泄露）。
	// 双路写入：remember_preference 工具（用户显式陈述，立即生效）+ 会话完成轻量模型提取。
	if paths.ProfilePath != "" {
		profileStore := userprofile.NewStore(paths.ProfilePath)
		if err := profileStore.Load(); err != nil {
			closeStores(pgStore, redisStore)
			return nil, fmt.Errorf("load user profile %s: %w", paths.ProfilePath, err)
		}
		agentSvc.SetUserProfileStore(profileStore)
		agentSvc.SetProfileExtractor(func(ctx context.Context, text string) ([]string, error) {
			return extractProfilePreferences(ctx, modelFactory, text)
		})
		toolRegistry.SetUserProfileHook(func(ctx context.Context, text string) error {
			return profileStore.Append("偏好", text)
		})
	}
	// 注入权威 Agent 树访问器：Dispatcher 派发时 Register/Finish/SetCancel，
	// HTTP API 的 /tree 与 /agents/{aid}/cancel 端点通过 ReactService.TreeFor 读取。
	subAgentDispatcher.WithTree(agentSvc.TreeFor)
	// 注入 Paused DomainAgent 消息历史持久化层：domain 触达 token 上限时
	// SaveMessages 落库 agent_messages，用户"继续"时 ResumePaused 从该表
	// LoadMessages 重建 domain Agent 续跑。缺失会导致暂停后无法恢复
	// （ResumePaused 前置校验 msgStore == nil 直接失败）。
	subAgentDispatcher.WithMessagesStore(agent.NewPostgresMessagesStore(pgStore.DB()))
	// 注入未决子 Agent 检查器，开启父会话终结保护：
	// 父 Agent 给出终答前若有未决子 Agent，阻塞等待其完成，防止迟到 mailbox 消息丢失。
	agentSvc.SetPendingChildrenChecker(subAgentDispatcher)
	// 注入 Paused 子 Agent 检查器：MetaAgent 无限 budget 不会因自身 token 暂停，
	// 靠此检查在 wait loop 检测 Paused 子 DomainAgent（触达 token 上限）后主动暂停会话，
	// 置 PausedOnChild 态等用户"继续"恢复该 domain。
	agentSvc.SetPausedChildChecker(subAgentDispatcher)
	// 注入 Paused DomainAgent 恢复器：sendMessage 在 PausedOnChild 态优先恢复 earliest paused domain，
	// 从 agent_messages 加载历史用 fresh budget 续跑（各 Agent 独立上下文）。
	agentSvc.SetPausedDomainResumer(subAgentDispatcher)
	// 软停止（TODO #37）：会话 Stop 先标记再触发子 Agent cancel，dispatcher 收尾分支
	// 把 domain 落 Paused（存 history 可续跑）、叶子部分回灌；倒计时到期硬销毁。
	agentSvc.SetSoftStopMarker(subAgentDispatcher)
	agentSvc.SetStopCountdown(time.Duration(cfg.Agent.StopDestroyCountdownSec) * time.Second)
	// 默认恢复历史会话：从 session_history 恢复最近 50 个会话到内存，
	// 保证重启后长任务上下文可见；显式 restore_sessions: false 关闭。
	// 恢复失败仅记录日志，不阻断启动。
	if cfg.Agent.RestoreSessions == nil || *cfg.Agent.RestoreSessions {
		agentSvc.RestoreSessions(ctx, 50)
	}

	// 第十六步半：插件系统（设计文档《插件系统设计 v2》）。
	// 热插拔插件：MCP 桥（web_search / computer_use 等）+ bundle 外部插件包。
	// plugins.yaml 与 config.yaml 同目录；plugins.d/ 亦与配置同目录（外部插件包目录）。
	// 装配：manager → 注入 mcp 工厂 → Load（自动 enable enabled:true）→ 注入角色可见性回调。
	// Load/Reload 失败不阻断启动（插件系统为可选能力，配置损坏仅记录）。
	pluginManager := plugins.NewManager(toolRegistry,
		plugins.WithLogger(slog.Default()),
		plugins.WithConfigDir(filepath.Dir(paths.ConfigPath)),
		plugins.WithBundlesDir(filepath.Join(filepath.Dir(paths.ConfigPath), "plugins.d")),
		plugins.WithSkillPool(skillPool),
		plugins.WithWorkDir(workDir),
		plugins.WithMCPFactory(func(id string, settings map[string]any, deps plugins.Deps) (plugins.Plugin, error) {
			return mcpbridge.NewFromSettings(id, settings, deps)
		}),
	)
	if err := pluginManager.Load(ctx); err != nil {
		log.Printf("[bootstrap] plugins load failed (non-fatal): %v", err)
	}
	// 角色可见性：meta/domain/动态角色按 Manifest.Roles 判定插件工具是否可见（§4.3）。
	agentSvc.SetPluginVisibility(pluginManager.ToolVisibility)
	subAgentDispatcher.WithPluginVisibility(pluginManager.ToolVisibility)
	// 挂载天花板（TODO #52）：tool.Registry 同一回调校验 tool_mount/派发 tools_hint
	// 是否越界（工具包不反向依赖 plugins，经同一函数值注入）。
	toolRegistry.SetPluginVisibility(pluginManager.ToolVisibility)
	// 插件管理工具组（TODO #51 Agent 自安装闭环）：plugin_search/install/enable/disable/list
	// 经适配器注入 tool.Registry（tool 包不反向依赖 plugins）。
	toolRegistry.SetPluginManager(plugins.NewToolManagerAdapter(pluginManager))

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
		Plugins:      pluginManager,
	}

	// 第二十一步：注册关闭时释放资源的回调，按依赖顺序排列（外层 Close 会逆序调用）。
	app.cleanup = []func() error{
		func() error {
			// 先停全部插件（MCP 子进程/HTTP 连接），再关 DAG 调度器。
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = pluginManager.StopAll(stopCtx)
			return nil
		},
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
		"agent_events":    store.EnsureAgentEventsSchema,
		"agent_messages":  store.EnsureAgentMessagesSchema,
		"session_logs":    store.EnsureSessionLogsSchema,
		"dag":             store.EnsureDAGSchema,
		"memory":          store.EnsureInitialMemorySchema,
		"agent_tree":      store.EnsureAgentTreeSchema,
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

// newIntentArbiter 构造 TODO #39 L2 意图仲裁器：轻量模型对灰区输入四分类
// （none/resume/control/diagnose + confidence）。
// 超时/调用失败/解析失败/低置信一律返回错误 → 上层按 IntentNone 直通（宁漏判不误判：
// 漏判退回无补全的旧行为，误判是 2026-08-11 事故）。
func newIntentArbiter(f *model.ModelFactory) agent.IntentArbiter {
	if f == nil {
		return nil
	}
	return func(ctx context.Context, text string) (agent.IntentKind, error) {
		prompt := "你是用户输入意图分类器。判断输入属于哪一类：\n" +
			"- none: 普通任务/功能描述/一般疑问，不涉及会话控制\n" +
			"- resume: 继续/续跑被中断或失败的任务\n" +
			"- control: 停止/取消/暂停当前任务\n" +
			"- diagnose: 询问失败原因/要求诊断分析\n" +
			"只输出 JSON：{\"intent\": \"none|resume|control|diagnose\", \"confidence\": 0.0-1.0}\n" +
			"输入: " + text
		out, err := f.CallLightweightWithRetry(ctx, prompt)
		if err != nil {
			return agent.IntentNone, err
		}
		var r struct {
			Intent     string  `json:"intent"`
			Confidence float64 `json:"confidence"`
		}
		if err := parseStrictJSON(out, &r); err != nil {
			return agent.IntentNone, fmt.Errorf("parse arbiter json: %w", err)
		}
		// 低置信不入流：0.6 阈值以下按无意图处理（防弱信号噪声带偏）。
		if r.Confidence < 0.6 {
			return agent.IntentNone, fmt.Errorf("low confidence %.2f", r.Confidence)
		}
		switch r.Intent {
		case "resume":
			return agent.IntentResume, nil
		case "control":
			return agent.IntentControl, nil
		case "diagnose":
			return agent.IntentDiagnose, nil
		default:
			return agent.IntentNone, nil
		}
	}
}

// parseStrictJSON 剥 markdown 围栏后严格解析 JSON（模型可能包 ```json 代码块）。
func parseStrictJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "json")
	return json.Unmarshal([]byte(strings.TrimSpace(s)), v)
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
